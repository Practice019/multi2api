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
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

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
		// 它只声明自己需要的六个方法。
		Pool: poolAdapter{p: p},
		Log:  checkinLog,

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
	log.Printf("已注册上游: %v", registry.IDs())

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
		// 额度恢复策略由上游回答：workbuddy 给"次日 04:00"（等签到恢复），
		// 核心不再内置任何具体时点。缺失时 handler 回落到 now+1h。
		NextResetAt: wb.NextResetAt,
		// /v1/models 的 owned_by 由装配层注入（核心不再硬编码上游名）。
		// 多上游落地后这里要改成按 provider 合并各自的目录。
		OwnedBy: wb.ID(),
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
			// 注册表：admin 遍历它，把每个上游通过 AdminExt 声明的管理端点挂上来。
			// Task 3c 之后 22 个 workbuddy 端点就是这样挂的 ——
			// 加新上游时 admin 包零改动（判据 1）。
			Registry: registry,
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
