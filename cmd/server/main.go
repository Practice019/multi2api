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
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/oauth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
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
		log.Printf("请求日志落盘: %s（保留 %d 天）", cfg.RequestLogPath, cfg.RequestLogKeepDays)
	}

	sch := scheduler.New(scheduler.Config{
		Pool:                    p,
		Upstream:                up,
		CheckinHours:            cfg.Schedule.CheckinHours,
		KeepaliveHours:          cfg.Schedule.KeepaliveHours,
		CheckinDisabled:         !cfg.Schedule.CheckinEnabled,
		KeepaliveDisabled:       !cfg.Schedule.KeepaliveEnabled,
		Log:                     checkinLog,
		TravelAutoClaimDisabled: !cfg.TravelAutoClaim,
		TravelWatchInterval:     cfg.TravelWatchInterval,

		GrowthWatchInterval: cfg.GrowthWatchInterval,
		// 传指针：nil 表示「未设置」，由 scheduler.New 决定默认（领奖开、补签开、其余关）。
		GrowthAutoClaim:  &cfg.GrowthAutoClaim,
		GrowthAutoAccept: &cfg.GrowthAutoAccept,
		GrowthAutoMakeup: &cfg.GrowthAutoMakeup,
		GrowthAutoRedeem: &cfg.GrowthAutoRedeem,
		GrowthAutoOpen:   &cfg.GrowthAutoOpen,
		GrowthAutoDraw:   &cfg.GrowthAutoDraw,
	})
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
	var clientLogin *clientlogin.Manager
	if cfg.ClientEnabled && cfg.ClientAuthDir != "" {
		clientLogin = clientlogin.New(cfg.ClientAuthDir, cfg.AuthDir, cfg.ClientArchiveDir)
		log.Printf("本地登录面板已启用：客户端凭证 %s，存档 %s", cfg.ClientAuthDir, cfg.ClientArchiveDir)
	} else if !cfg.ClientEnabled {
		log.Printf("本地登录面板已关闭（admin.client_login_enabled=false）")
	} else {
		log.Printf("本地登录面板不可用：未探测到客户端凭证目录（可用 admin.client_auth_dir 指定）")
	}

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Admin: admin.New(admin.Config{
			Pool:             p,
			Upstream:         up,
			Scheduler:        sch,
			OAuth:            oauth.New(cfg.OAuthBaseURL),
			Log:              checkinLog,
			Ring:             logRing,
			AuthDir:          cfg.AuthDir,
			ClientLogin:      clientLogin,
			ResetModelsCache: server.ResetModelsCache,
			Settings: newSettingsStore(*cfgPath, cfg, sch, checkinLog, func(days int) {
				if s := logRing.Sink(); s != nil {
					s.SetKeepDays(days)
				}
			}),
			StartedAt: time.Now(),
		}),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	// 猫猫旅行自动领奖守卫：按 arrive_at 错峰检查，到站即领（可用 admin.travel_auto_claim 关掉）。
	go sch.RunTravelWatcher(ctx, cfg.TravelWatchInterval)
	// 成长中心守卫：领任务奖励 / 补签 / 连登兑换 / 开盲盒 / 抽奖（后三个默认关，见 config）。
	go sch.RunGrowthWatcher(ctx, cfg.GrowthWatchInterval)
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
