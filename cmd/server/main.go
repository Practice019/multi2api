// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
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
	"workbuddy2api/internal/oauth"
	"workbuddy2api/internal/pool"
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

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

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
	if cfg.CodeartsEnabled {
		cb = codearts.NewWithConfig(codearts.Config{
			AuthDir: cfg.CodeartsAuthDir,
		})
		// 凭证访问器：核心把"现在有哪些账号"喂给上游（上游不得依赖 internal/pool）。
		//
		// 这里用**惰性**闭包而不是启动时快照：用户跑完 cmd/login 后点一下
		// 管理台的"刷新账号"，新的 codearts*.json 应当立即生效。
		cb.SetAccounts(func() []*codearts.Auth {
			list, err := codearts.LoadDir(cfg.CodeartsAuthDir)
			if err != nil {
				log.Printf("codearts: 读取凭证目录失败: %v", err)
				return nil
			}
			return list
		})
		// 管理端点的核心依赖：只暴露 uid 列表与按 uid 解析，核心不需要理解 CodeArts 凭证结构。
		cb.SetAdminEnv(codearts.AdminEnv{
			Accounts: func() []string {
				list, _ := codearts.LoadDir(cfg.CodeartsAuthDir)
				out := make([]string, 0, len(list))
				for _, a := range list {
					out = append(out, a.UID)
				}
				return out
			},
			Resolve: func(uid string) (*codearts.Auth, error) {
				list, err := codearts.LoadDir(cfg.CodeartsAuthDir)
				if err != nil {
					return nil, err
				}
				for _, a := range list {
					if a.UID == uid || a.AccessKey == uid {
						return a, nil
					}
				}
				return nil, fmt.Errorf("codearts: 账号不存在: %s", uid)
			},
		})
		if err := registry.Register(cb); err != nil {
			log.Fatalf("注册 CodeArts 上游失败: %v", err)
		}
		// 残留的续期备份 = 上次续期没善终（refresh_token 是消费型的，
		// 请求可能已消费但它没落盘）。这里只提示，不阻断启动。
		if stale, err := codearts.FindStaleBackups(cfg.CodeartsAuthDir); err == nil && len(stale) > 0 {
			log.Printf("codearts: 发现 %d 个残留的续期备份（上次续期可能未完成）: %v", len(stale), stale)
		}
		// 后台主动续期：STS 只有约 30 分钟寿命，预热能消掉"空闲后首个请求"
		// 多付的那次续期往返。可用 refresh_interval_seconds<=0 关闭。
		if cfg.CodeartsRefreshInterval > 0 {
			cb.SetRefreshInterval(cfg.CodeartsRefreshInterval)
			log.Printf("codearts: 后台续期已开启（每 %s 扫描一次）", cfg.CodeartsRefreshInterval)
		}
		log.Printf("codearts: 已启用（凭证目录 %s）", cfg.CodeartsAuthDir)
	} else {
		log.Printf("codearts: 未启用（config 里 codearts.enabled 缺省为 false）")
	}

	log.Printf("已注册上游: %v", registry.IDs())

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
		if n := syncCodeartsAccounts(p, cfg.CodeartsAuthDir); n > 0 {
			log.Printf("codearts: 已并入账号池 %d 个账号", n)
		} else {
			log.Printf("codearts: 账号池中暂无账号（凭证目录 %s 里没有可用的 codearts*.json）", cfg.CodeartsAuthDir)
		}
	} else if cb != nil {
		log.Printf("codearts: 未并入账号池（codearts.pool_accounts=false），只能通过其管理端点使用")
	}

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
		Pool:              p,
		Upstream:          up,
		APIKey:            cfg.APIKey,
		Session:           sessRouter,
		StickyCount:       sessCount,
		RedisMode:         redisMode,
		SoftCooldown:      cfg.SoftRateDur,
		ModelCatalog:      server.ModelCatalog,
		ModelCatalogState: server.ModelCatalogState,
		// 实例身份：配置里给了就用配置的，否则用编译期默认值。
		// 控制台顶栏与 /healthz 的 service 字段共用它 —— 前端不再硬编码服务名。
		ServiceName: cfg.ServiceName,
		// 额度恢复策略由上游回答：workbuddy 给"次日 04:00"（等签到恢复），
		// 核心不再内置任何具体时点。缺失时 handler 回落到 now+1h。
		NextResetAt: wb.NextResetAt,
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
