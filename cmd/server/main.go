// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/clientlogin"
	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/loomy"
	"workbuddy2api/internal/oauth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/workbuddy"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// 兼容读：迁移期凭证可能还在 `auths/` 根（旧位置），
	// 也可能已搬到 `auths/workbuddy/`（新位置）。只读一处会看到
	// "账号池突然空了" —— 那是很难判断的故障（看起来像凭证损坏）。
	//
	// LoadDirCompat 子目录优先、按 uid 去重，两处都扫。
	auths, err := auth.LoadDirCompat(cfg.AuthsBase, workbuddy.ProviderID)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s（兼容扫描 %s）",
		len(auths), cfg.AuthDir, cfg.AuthsBase)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	// 默认上游必须在 SyncToDir 之前注入：对齐时要用它判断"未打标签的账号算谁"。
	// 这里先用 workbuddy（注册顺序上的第一个），注册表建好后再用 registry.First() 校正。
	p.SetDefaultProvider(workbuddy.ProviderID)
	p.SyncToDir(auths) // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	// 软冷却指数退避封顶（<=0 时保留 pool 的默认值 2h）。
	p.SetSoftRateMax(cfg.SoftRateMaxDur)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站身份（全部可省略；省略时出站请求头与改造前逐字节一致）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	{
		// 显式记一行：出站身份会决定"上游怎么看我们"，
		// 排查"为什么官网使用端显示不对"时这是唯一的入口。
		ua := "CLI/2.63.2 CodeBuddy/2.63.2（内置默认）"
		if cfg.Upstream.UserAgent != "" {
			ua = cfg.Upstream.UserAgent + "（user_agent 覆盖）"
		} else if cfg.Upstream.ClientVersion != "" {
			ua = "桌面端三段式（client_version=" + cfg.Upstream.ClientVersion + "）"
		}
		log.Printf("出站身份：UA=%s client_name=%q device_token=%v passthrough_ip=%v",
			ua, cfg.Upstream.ClientName,
			cfg.Upstream.DeviceToken != "" || cfg.Upstream.DeviceTokenFile != "",
			cfg.Upstream.PassthroughIP)
	}
	// 系统提示词体系（借鉴 workbuddy2api-panel）：模式 + 正文 + 降级状态机。
	//
	// 正文由 cfg.normalize() 在启动时就加载好（文件不可读会在那里直接报错）。
	// 状态机在这里创建，**同一个实例**必须同时交给两处：
	//
	//	up.PromptGate   出站客户端读它，决定用 PromptText 还是 Degraded
	//	h.cfg.PromptGate 出站循环写它（观察到内容拦截时 Trigger）
	//
	// 两处共用同一个 *prompt.Gate 是本机制成立的前提 ——
	// 各建一个的话，handler 触发的降级客户端永远看不到，退化成"每次重试都先撞 400"。
	up.PromptMode = cfg.PromptMode
	up.PromptText = cfg.PromptText
	promptGate := prompt.NewGate()
	up.PromptGate = promptGate
	{
		src := "内置默认"
		if cfg.Prompt.File != "" {
			src = cfg.Prompt.File
		}
		log.Printf("系统提示词：mode=%s 来源=%s（%d 字节）", cfg.PromptMode, src, len(cfg.PromptText))
	}

	checkinLog := checkinlog.New(cfg.CheckinLogPath, cfg.CheckinLogKeepDays)

	// 请求日志落盘：挂到 server 包的环形缓冲上，实时视图走内存、历史视图走文件。
	logRing := server.ChatLogRing()
	if sink, err := logbuf.OpenSink(cfg.RequestLogPath, cfg.RequestLogKeepDays, 0); err != nil {
		log.Printf("请求日志落盘不可用（仅内存模式）: %v", err)
	} else {
		logRing.SetSink(sink)
		defer sink.Close()
		// 用落盘日志里的最大 seq 播种两个序号计数器（Ring 的与 stdout 的），
		// 否则重启后序号从 1 重来，落盘文件里会出现重复 seq。
		// 读失败只影响序号连续性，不该拦住启动，因此仅记日志后继续。
		if maxSeq, err := sink.MaxSeq(); err != nil {
			log.Printf("读取历史日志序号失败（本次从 1 重新计数）: %v", err)
		} else if maxSeq > 0 {
			if err := logRing.SeedSeq(maxSeq); err != nil {
				log.Printf("播种请求序号失败: %v", err)
			}
			server.SeedChatSeq(maxSeq)
			log.Printf("请求日志续接历史序号：从 #%d 之后继续", maxSeq)
		}
		log.Printf("请求日志落盘: %s（保留 %d 天）", cfg.RequestLogPath, cfg.RequestLogKeepDays)
	}

	// ---- 上游注册 ----
	//
	// 加新上游时**只在这里加一行** registry.Register(...)：核心包
	// （gateway/pool/logbuf/admin/server/scheduler）一行都不用改。
	//
	// scheduler 不 import workbuddy —— 它通过 gateway.ExtOf[gateway.JobExt]
	// 发现任务，方向是"核心读接口"，不是"核心认上游"。
	registry := gateway.NewRegistry()
	wb := workbuddy.NewWithConfig(workbuddy.Config{
		// 账号池经适配器传入：上游包不得依赖 internal/pool（架构约束），
		// 它只声明自己需要的几个方法。
		Pool: poolAdapter{p: p},
		// 本上游在池里的归属标识：管理端点据此只列自己的账号
		// （见 workbuddy.accountList）。与 ID() 同源，避免两处漂移。
		Provider: workbuddy.ProviderID,
		Log:      checkinLog,
		// 凭证目录：按上游分子目录后，workbuddy 自己的是 `auths/workbuddy/`。
		// 供 gateway.LoginFlow.AuthDir 用（页内添加账号落盘时用）。
		AuthDir: cfg.AuthDir,
		// 登录流程：装配层把具体的 OAuth 客户端适配进来。
		//
		// 与 Pool 同一个思路 —— workbuddy 只声明"我要什么形状"，
		// 不认识 oauth 包。见 upstream_business.go 的 workbuddyLogin。
		//
		// ⚠ 这里与第 384 行的 `OAuth:` 各给一份，**不是重复**：
		// admin 那份服务过渡期的旧路径（请求不带 provider 时走它），
		// 这一份供"按 provider 分派"使用。旧路径退役后前者可删。
		Login: workbuddyLogin(cfg.OAuthBaseURL),

		TravelAutoClaimDisabled: !cfg.TravelAutoClaim,
		TravelWatchInterval:     cfg.TravelWatchInterval,

		GrowthWatchInterval: cfg.GrowthWatchInterval,
		// 传指针：nil 表示「未设置」，由 workbuddy 决定默认（领奖开、接单开、补签开、其余关）。
		GrowthAutoClaim:  &cfg.GrowthAutoClaim,
		GrowthAutoAccept: &cfg.GrowthAutoAccept,
		GrowthAutoMakeup: &cfg.GrowthAutoMakeup,
		GrowthAutoRedeem: &cfg.GrowthAutoRedeem,
		GrowthAutoOpen:   &cfg.GrowthAutoOpen,
		GrowthAutoDraw:   &cfg.GrowthAutoDraw,
	})
	// 上游的 HTTP 客户端跟着 config 的超时一起装配（与改造前 upstream.New() 同一份配置）。
	wb.SetClient(up)
	// 对话活跃上报排程（借鉴 workbuddy2api-panel）。
	//
	// ⚠ 默认**不启用**：schedule.activity_hours 为空即不跑
	// （见 config.go 的 ActivityHours 注释：新增能力必须 opt-in，
	// 否则老部署升级后会自动开始对每个账号发上游请求）。
	wb.SetActivitySchedule(cfg.Schedule.ActivityHours, cfg.Schedule.ActivityEnabled)
	if wb.ActivityEnabled() {
		log.Printf("对话活跃上报：已启用，时点 %v", wb.ActivityHours())
	}
	if err := registry.Register(wb); err != nil {
		log.Fatalf("注册上游失败: %v", err)
	}

	// ---- 第二个上游：CodeArts（判据 1 的实测对象）----
	//
	// 全部装配逻辑只有这一段，核心包一行都不用改 —— 这正是判据 1 要证明的。
	//
	// 注意 **注册顺序**：workbuddy 先注册，所以 registry.First() 仍是 workbuddy，
	// default_provider 缺省时"裸模型名走谁"的行为与改造前完全一致（向后兼容）。
	// codearts 只在显式配置或 "codearts/model" 前缀时才被用到。
	var cb *codearts.Provider
	// creds 是 codearts 凭证的**进程内唯一所有者**（见 codeartscreds.go）。
	//
	// ⚠ 它必须在三处是**同一个**：
	//
	//	SetAccounts（后台续期任务 + 请求路径的凭证来源）
	//	SetAdminEnv（管理端点）
	//	syncCodeartsAccounts（池 secret）
	//
	// 少共享一处就会回到 007 修的 bug：同一份凭证在进程里有多个对象，
	// 对象级 refreshMu 跨对象失效，后台续期写回打不到池子 → 503 no_healthy_account。
	var creds *codeartsCredStore
	if cfg.CodeartsEnabled {
		cb = codearts.NewWithConfig(codearts.Config{
			AuthDir: cfg.CodeartsAuthDir,
			// 页内添加账号（PKCE + DPoP，走本地回调服务器）。
			//
			// 这里是 R1 的最后一环：`codearts.Manager` 早就在包内实现了
			// 完整授权流程（从 codearts2api 移植），但**装配层一直没传进来** ——
			// 于是 `Configured()` 恒为 false，manifest 的 login 恒为 null，
			// 界面上 codearts 那一行永远没有「＋ 添加账号」。
			//
			// 传了之后：
			//   manifest.providers[codearts].login = {"kind":"device","label":"添加账号"}
			//   → 前端渲染按钮（它已经在按 manifest 渲染，前端零改动）
			//   → /admin/login/start?provider=codearts 走 codearts 自己的流程
			Login: codearts.NewManager(cfg.CodeartsOAuthPortal, cfg.CodeartsOAuthSTS, ""),
		})
		// 凭证访问器：核心把"现在有哪些账号"喂给上游（上游不得依赖 internal/pool）。
		//
		// 这里用**惰性**闭包而不是启动时快照：用户跑完 cmd/login 后点一下
		// 管理台的"刷新账号"，新的 codearts*.json 应当立即生效。
		//
		// ⚠ store.List 每次都重新枚举目录、但**按 uid 复用同一个对象**，
		// 而不是"每次 LoadDir 造一批新对象"—— 后者正是 503 的根因：
		// 后台续期任务拿到的那份对象与池 secret 那份不是同一个，
		// 一次性 refresh_token 换回来的新凭证永远写不回池子。
		//
		// 三条读路径（池 secret / 后台任务 / 管理端点）的接线统一收在
		// wireCodeartsCreds 里，好让测试能钉住**装配本身**（见 codeartscreds.go）。
		creds = wireCodeartsCreds(cb, cfg.CodeartsAuthDir, checkinLog)
		if err := registry.Register(cb); err != nil {
			log.Fatalf("注册 CodeArts 上游失败: %v", err)
		}
		// 残留的续期备份 = 上次续期没善终（refresh_token 是消费型的，
		// 请求可能已消费但它没落盘）。这里只提示，不阻断启动。
		if stale, err := codearts.FindStaleBackups(cfg.CodeartsAuthDir); err == nil && len(stale) > 0 {
			log.Printf("codearts: 发现 %d 个残留的续期备份（上次续期可能未完成）: %v", len(stale), stale)
		}
		// 后台主动续期：STS 只有约 2 小时寿命，预热能消掉"空闲后首个请求"
		// 多付的那次续期往返。可用 refresh_interval_seconds<=0 关闭。
		if cfg.CodeartsRefreshInterval > 0 {
			cb.SetRefreshInterval(cfg.CodeartsRefreshInterval)
			log.Printf("codearts: 后台续期已开启（每 %s 扫描一次）", cfg.CodeartsRefreshInterval)
		}
		log.Printf("codearts: 已启用（凭证目录 %s）", cfg.CodeartsAuthDir)
	} else {
		log.Printf("codearts: 未启用（config 里 codearts.enabled 缺省为 false）")
		// 凭证在、上游却没开 —— 这是最容易被读成"界面坏了"的一种状态：
		// 用户明明看得见 auths/codearts/ 里的凭证，账号池与任务面板里却没有它们
		// （实测事故：用户看到的是"2 个账号 · 按默认上游推断（未在 manifest 里注册）"
		// 加"2 个已注册任务"，而真相就是这一行没有被启用）。
		//
		// 所以这里把"为什么看不到"直接讲出来。数量取实、不猜；读不到目录时
		// 静默 —— 这一行只是提示，不该让启动失败。
		if list, err := codearts.LoadDir(cfg.CodeartsAuthDir); err == nil && len(list) > 0 {
			log.Printf("codearts: 注意 —— 凭证目录 %s 里有 %d 份凭证，但本次未启用该上游："+
				"它们不会并入账号池、也不会有后台续期任务；池中若残留旧账号，下一步对账会逐出",
				cfg.CodeartsAuthDir, len(list))
		}
	}

	// ---- 把 codearts 的账号并入核心账号池（Task 6）----
	//
	// # 为什么必须做这件事
	//
	// 改造前 codearts 的凭证只存在它自己的目录里，池子一无所知。
	// 后果是**请求永远不可能被路由到 codearts 账号** —— 无论客户端
	// 写 "codearts/xxx" 还是别的什么，出口层选号时池里只有 workbuddy 的号。
	//
	// # 为什么不是把两个目录合并后一次 SyncToDir
	//
	// 那样会**误删**：SyncToDir 的语义是"扫描结果的全集就是池中应有的全集"，
	// 而两个上游的 LoadDir 各自只扫自己前缀的文件。把某一次的扫描结果
	// 当成全集，会把另一个上游的账号整体剔除。
	//
	// 所以两条同步**各管各的域**（SyncToDir / SyncToDirWithSecrets 的
	// provider 参数），谁都不会动别家的账号。
	//
	// # 顺序
	//
	// 先 workbuddy（默认上游，它在 SyncToDir 里已随启动完成），
	// 再 codearts。注册顺序也保持 workbuddy 在前 —— registry.First()
	// 因此仍是 workbuddy，"裸模型名走谁"与改造前一致。
	if cb != nil && cfg.CodeartsPoolAccounts {
		if n := syncCodeartsAccounts(p, creds); n > 0 {
			log.Printf("codearts: 已并入账号池 %d 个账号", n)
		} else {
			log.Printf("codearts: 账号池中暂无账号（凭证目录 %s 里没有可用的 codearts*.json）", cfg.CodeartsAuthDir)
		}
	} else if cb != nil {
		log.Printf("codearts: 未并入账号池（codearts.pool_accounts=false），只能通过其管理端点使用")
	}

	// ---- 第三个上游：Loomy（判据 1 的第二次实测）----
	//
	// # 这一段的长度本身就是结论
	//
	// codearts 那一段（上面）要处理一次性 refresh_token、DPoP 私钥、
	// 单一所有者 store、后台续期、残留备份提示 —— 三十多行。
	// loomy 这一段只有注册 + 并池两件事，因为它**没有会变的凭证**：
	// 一个 session 字符串，无 TTL、无 refresh token。
	//
	// 如果加第三个上游需要改核心，说明 gateway 那条接缝漏了；
	// 这里再次确认：核心包一行未动。
	//
	// 注册顺序仍然是 workbuddy 在前 → registry.First() 是 workbuddy →
	// "裸模型名走谁"与之前完全一致。loomy 只在显式配置或
	// "loomy/模型名" 前缀时才被用到。
	var lm *loomy.Provider
	if cfg.LoomyEnabled {
		lm = loomy.NewWithConfig(loomy.Config{
			AuthDir: cfg.LoomyAuthDir,
			BaseURL: cfg.LoomyBaseURL,
		})
		if err := registry.Register(lm); err != nil {
			log.Fatalf("注册 Loomy 上游失败: %v", err)
		}
		log.Printf("loomy: 已启用（凭证目录 %s，基址 %s）", cfg.LoomyAuthDir, cfg.LoomyBaseURL)
	} else {
		log.Printf("loomy: 未启用（config 里 loomy.enabled 缺省为 false）")
		// 与 codearts 同一个提示（那段的长注释同样适用）：凭证在、上游却没开，
		// 是最容易被读成"界面坏了"的一种状态 —— 明明看得见文件，池里却没有号。
		if list, err := loomy.LoadDir(cfg.LoomyAuthDir); err == nil && len(list) > 0 {
			log.Printf("loomy: 注意 —— 凭证目录 %s 里有 %d 份凭证，但本次未启用该上游："+
				"它们不会并入账号池；池中若残留旧账号，下一步对账会逐出",
				cfg.LoomyAuthDir, len(list))
		}
	}

	if lm != nil && cfg.LoomyPoolAccounts {
		if n := syncLoomyAccounts(p, cfg.LoomyAuthDir); n > 0 {
			log.Printf("loomy: 已并入账号池 %d 个账号", n)
		} else {
			log.Printf("loomy: 账号池中暂无账号（凭证目录 %s 里没有可用的 loomy*.json）", cfg.LoomyAuthDir)
		}
	} else if lm != nil {
		log.Printf("loomy: 未并入账号池（loomy.pool_accounts=false），只能通过其管理端点使用")
	}

	// "已注册上游"必须打在**所有**上游注册完之后。
	//
	// # 为什么（实测踩到过）
	//
	// 它原先在 codearts 之后、loomy 之前，于是日志打出
	// `已注册上游: [workbuddy]` 而 loomy 其实已经启用并把账号并进了池子。
	// 一行"看起来权威"的日志与事实不符，比不打印更糟 ——
	// 排查的人会顺着它得出"loomy 没注册上"的错误结论，
	// 然后去查一个根本不存在的注册失败。
	log.Printf("已注册上游: %v", registry.IDs())

	// 注册表建好后校正默认上游：它必须与"裸模型名走谁"的唯一权威一致。
	// 正常情况下就是 workbuddy（先注册），这里取 First() 是为了让
	// "谁先注册谁当默认"这条规则在装配层只有一处定义。
	//
	// 记进 defProvider 而不是只在 if 里用：/admin/providers 也要标出默认上游，
	// 两处必须取**同一个值**，否则前端显示的默认与实际的默认会不一致。
	defProvider, _ := registry.First()
	if defProvider != "" {
		p.SetDefaultProvider(defProvider)
	}

	// ---- 对账：逐出「本次启动没有注册的上游」的幽灵账号（见 pool_reconcile.go）----
	//
	// # 这次实测事故的来龙去脉
	//
	// 账号池是**持久化**的（data/state.json，另有 Redis 快照）。用户先用一份
	// 启用了 codearts 的 config 跑过：auths/codearts/ 下 2 份凭证并入池子并落盘。
	// 随后改用一份**没有 codearts 段**的 config 启动（codearts.enabled 缺省 false
	// ⇒ 上游不注册），但启动恢复（Pool.RestoreFromSnapshot / Pool.load）只认
	// state.json，照样把那 2 个 codearts 账号装回池子。于是：
	//
	//	/admin/accounts 有 5 个账号（2 个 provider="codearts"），
	//	/admin/ui/manifest 的 providers 只有 workbuddy、jobs 只有 2 条 ——
	//	前端账号池里冒出一个 manifest 里没注册的上游分组；
	//	更实害：这 2 个号永远选得到却没有上游能服务、也没有续期任务
	//	（codearts 的 STS 只有约 2 小时寿命），请求只会失败。
	//
	// 修法是在"知道本次注册了哪些上游"的这一刻对账一次：池里出现过、
	// 但不在 registry 里的上游，从**池中**逐出。
	//
	// # 为什么必须在 SetDefaultProvider 之后
	//
	// Pool.Providers() 返回的是**生效**标识：没打 provider 标签的历史账号会
	// 回落成默认上游。默认上游还没设时它们会解析成**空串**，而空串不在
	// registry.IDs() 里 —— 对账就会把「没有标签的号」整批误删。
	// 所以顺序必须是 SetDefaultProvider → 对账；pruneUnregisteredProviders
	// 内部还额外跳过空串，两道一起守（双保险）。
	//
	// # 不变量：凭证文件一个字节都不动
	//
	// 这里只把账号从**池**里逐出，auths/codearts/ 下的凭证文件原样保留 ——
	// 重新启用该上游后，下次启动会照常把凭证重新并入池子。
	if pruned, evicted := pruneUnregisteredProviders(p, registry.IDs(), log.Printf); pruned > 0 {
		log.Printf("账号池：对账完成，本次启动未注册的上游 %d 个、账号 %d 个已逐出", pruned, evicted)
	}

	// 槽位定义在这里给出：核心只认识"有个叫 X 的槽位、配在 Y 点"，
	// 不认识 checkin/keepalive 是什么业务 —— 那是 workbuddy 的事。
	// 装配处（本文件）是唯一同时认识核心与上游的地方，翻译在这里发生。
	sch := scheduler.New(scheduler.Config{
		Slots: []scheduler.Slot{
			{
				Name:     workbuddy.SlotCheckin,
				Hours:    cfg.Schedule.CheckinHours,
				Disabled: !cfg.Schedule.CheckinEnabled,
			},
			{
				Name:     workbuddy.SlotKeepalive,
				Hours:    cfg.Schedule.KeepaliveHours,
				Disabled: !cfg.Schedule.KeepaliveEnabled,
			},
		},
		// 到点喊谁：上游实现 scheduler.SlotRunner（RunSlot/RunSlotFor），
		// 适配器只做一次返回类型的逐字段转换（两侧各自声明的类型不得共用）。
		Runner: slotRunnerAdapter{p: wb},
		Log:    checkinLog,
		// 注册表交给调度器：它自己发现各上游的 JobExt 任务（成长/旅行守卫轮）。
		Registry: registry,
	})
	// 历史落库经核心：格式跨上游统一，Nickname 由核心从账号池补。
	wb.SetCore(schedulerAdapter{sch: sch})
	// 管理端点的核心依赖（/admin/schedule 的时点 + 与签到/保活共用的任务槽）。
	// 后注入的理由就在这里：调度器在 Provider 之后构造。
	wb.SetAdminEnv(workbuddy.AdminEnv{
		Schedule: schedulerAdapter{sch: sch},
		TaskSlot: newTaskSlotAdapter(sch),
	})
	// 本机客户端登录态管理（见下）。
	// 槽位收尾的搭车任务（旅行状态机）由上游提供，核心只负责在正确的时机喊一声。
	sch.AddSlotHook(wb)
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）：猫猫旅行同时停摆（搭签到便车）")
	case len(cfg.Schedule.CheckinHours) == 0:
		log.Printf("猫猫旅行已合并到签到时点执行：签到 + 派猫 + 领取旅行奖励")
	default:
		log.Printf("猫猫旅行已合并到签到时点执行：签到 + 派猫 + 领取旅行奖励（%v 点）", cfg.Schedule.CheckinHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	}

	// 本机客户端登录态管理：能读就开面板，读不到就置 nil（该面板降级为 503），
	// 禁止因为客户端没装/路径变了就让整个网关起不来。
	//
	// Task 3c 起，这个面板的端点由 workbuddy 自注册，所以管理器要交给上游：
	// 核心的 admin 不再认识它。
	var clientLogin *clientlogin.Manager
	if cfg.ClientEnabled && cfg.ClientAuthDir != "" {
		clientLogin = clientlogin.New(cfg.ClientAuthDir, cfg.AuthDir, cfg.ClientArchiveDir)
		wb.SetClientLogin(clientLoginAdapter{m: clientLogin})
		log.Printf("本地登录面板已启用：客户端凭证 %s，存档 %s", cfg.ClientAuthDir, cfg.ClientArchiveDir)
	} else if !cfg.ClientEnabled {
		log.Printf("本地登录面板已关闭（admin.client_login_enabled=false）")
	} else {
		log.Printf("本地登录面板不可用：未探测到客户端凭证目录（可用 admin.client_auth_dir 指定）")
	}

	// 目录状态在 server 与 admin 里各有一个结构完全相同的类型（两个包互不 import，
	// 所以没法共用一个定义）。转换只在这一个地方做，两边各自保持零依赖。
	modelCatalogState := func() admin.ModelCatalogState {
		st := server.ModelCatalogState()
		return admin.ModelCatalogState{
			State:     st.State,
			Models:    st.Models,
			Stale:     st.Stale,
			Cooldown:  st.Cooldown,
			FetchedAt: st.FetchedAt.Format(time.RFC3339),
		}
	}

	// 上游设置的适配器：把 workbuddy 的设置项接进设置页与宿主持久化。
	//
	// Task 3c 之后它只剩**设置项**一条线：账号级业务（成长/旅行/额度）的
	// 22 个管理端点已经由 workbuddy 自己通过 AdminExt 注册，不再经核心转发。
	upSettings := newUpstreamSettingsAdapter(wb, cfg)

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		MaxBodyMB:    cfg.Server.MaxBodyMB,
		// ⚠ 必须是**同一个** promptGate 实例（与 up.PromptGate 一起给）：
		// handler 在这里写、出站客户端在那里读。两个实例 = 机制失效。
		PromptGate:        promptGate,
		ModelCatalog:      server.ModelCatalog,
		ModelCatalogState: server.ModelCatalogState,
		// 实例身份：配置里给了就用配置的，否则用编译期默认值。
		// 控制台顶栏与 /healthz 的 service 字段共用它 —— 前端不再硬编码服务名。
		ServiceName: cfg.ServiceName,
		// 额度恢复策略由上游回答：workbuddy 给"次日 04:00"（等签到恢复），
		// 核心不再内置任何具体时点。缺失时 handler 回落到 now+1h。
		//
		// ⚠ 参数 providerID 是**必须的**（P2 修复）：旧签名 `func() time.Time`
		// 没有参数，装配层只能把 wb.NextResetAt 塞进去，于是 codearts 的号
		// 也被冷到 workbuddy 的次日 04:00 —— 而 codearts 没有签到恢复机制，
		// 04:00 不是它的任何事实（可能白闲置近 24h）。
		//
		// 现在按 ID 分派：`registryRouter.ResetAt` 走 gateway.ResetPolicyExt
		// （与 RefreshSkew / Classify 同一模式，没实现的上游返回 ok=false
		// → 核心回落 now+1h）。workbuddy 自己实现该扩展点并给出"次日 04:00"，
		// 所以它的行为与改造前**逐字一致**（这是硬要求）。
		//
		// 凭证由 registryRouter 内部按 (id, uid) 组装；这里没有 uid
		// （恢复排程是**上游**的事实，不是某一份凭证的），传零值 Credential。
		NextResetAt: func(providerID string) (time.Time, bool) {
			return registryRouter{reg: registry, p: p}.ResetAt(providerID, gateway.Credential{Provider: providerID})
		},
		// /v1/models 的 owned_by 由装配层注入（核心不再硬编码上游名）。
		// 多上游之后它是**默认上游**的 owned_by，其余上游按各自 ID 填。
		OwnedBy: wb.ID(),
		// 多上游路由：出口层只按 ID 问，不认识任何具体上游。
		// 注册表 + 账号池在这一层合并成出口层要的那个小接口。
		Provider:        registryRouter{reg: registry, p: p},
		DefaultProvider: func() string { id, _ := registry.First(); return id }(),
		Admin: admin.New(admin.Config{
			Pool:     p,
			Upstream: up,
			OAuth:    oauth.New(cfg.OAuthBaseURL),
			Log:      checkinLog,
			Ring:     logRing,
			AuthDir:  cfg.AuthDir,
			// 兼容扫描的父目录：迁移期凭证可能还在 `auths/` 根。
			AuthsBase: cfg.AuthsBase,
			// 核心调度视图 + 共享任务槽，供 /admin/schedule 与 /admin/task。
			//
			// 这两条端点读的是核心自己排的班，不表达任何上游身份 ——
			// Task 3c 曾把它们放进 workbuddy，阶段 0 评审指出
			// 第二个上游若不声明 CapCheckin 就没人服务它们。已移回核心。
			//
			// 任务槽与 workbuddy 指向**同一个** sharedTaskSlot：
			// "同一时刻只允许一个全量任务"是进程级语义，不分上游。
			Scheduler: adminSchedulerAdapter{sch},
			TaskSlot:  adminTaskSlotAdapter{sharedTaskSlot},
			// 缺省上游：/admin/providers 标出它，前端据此把它的模型按**裸名**展示
			//（其余上游带前缀）。裸名向后兼容是硬要求，所以这个值必须与
			// 出口层实际用的缺省上游一致。
			DefaultProvider: defProvider,
			// `AuthDir` 里的凭证属于 workbuddy（`auth.LoadDir` 只 glob
			// `workbuddy*.json`）—— 必须**显式**声明，不能让 reload 靠
			// "谁是默认上游"去猜。评审 F3：那在本部署里碰巧正确
			//（workbuddy 恰好第一个注册），但默认上游一变就会误删别的上游账号。
			ReloadProvider: workbuddy.ProviderID,
			// 注册表：admin 遍历它，把每个上游通过 AdminExt 声明的管理端点挂上来。
			// Task 3c 之后 22 个 workbuddy 端点就是这样挂的 ——
			// 加新上游时 admin 包零改动（判据 1）。
			Registry: registry,
			// 控制台渲染契约里的服务名，与 /healthz 的 service 字段同源。
			//
			// admin 不得 import server（server 依赖 admin，反向会成环），
			// 所以这个值必须在这里显式传进去。
			// 用 cfg.ServiceName（可为空）→ server 内部再回落默认常量，
			// 保证"配置没写"与"配置写了空串"行为一致。
			ServiceName: firstNonEmpty(cfg.ServiceName, server.ServiceName),
			// 上游设置项必须以**适配器**形式显式注入，不能靠从 Registry 里
			// 断言 SettingsExt：上游的 SettingField 与 admin 的是两个类型
			// （各自声明，互不 import），方法集精确匹配会静默失败
			// —— 表现成设置页少几个键且没有任何报错。
			SettingsExts:      []admin.SettingsExt{upSettings},
			ResetModelsCache:  server.ResetModelsCache,
			ModelCatalog:      server.ModelCatalog,
			ModelCatalogState: modelCatalogState,
			Settings: newSettingsStore(*cfgPath, cfg, sch, upSettings, checkinLog, func(days int) {
				if s := logRing.Sink(); s != nil {
					s.SetKeepDays(days)
				}
			}),
			StartedAt: time.Now(),
		}),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// 核心调度循环：整点签到/保活 + 各上游通过 gateway.JobExt 注册的守卫任务
	// （workbuddy 的成长/旅行守卫轮就在其中）。核心**不认识**具体任务名。
	go sch.Run(ctx)
	if cfg.TravelAutoClaim {
		log.Printf("猫猫旅行自动领奖已开启（守卫轮 %s；按 arrive_at 错峰，到站即领）", cfg.TravelWatchInterval)
	} else {
		log.Printf("猫猫旅行自动领奖已关闭（admin.travel_auto_claim=false），仅手动领奖")
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
