// system_status.go GET /admin/system-status —— 状态页的**唯一取数入口**（T12）。
//
// # 为什么它是一条 /admin/* 端点，而不是塞进 /status
//
// `/status` 是**网关自身**的池状态（账号、号池计数、上游熔断），给探针与首页用；
// 它挂在 handler.go:420，判据是控制台身份，返回体形状已被既有调用方依赖。
// 状态页要的是**另一组事实**（宿主资源、进程运行时长、依赖健康、请求口径），
// 把两者塞进同一个响应体会让 /status 的既有消费者被迫解析一堆与它无关的字段。
// 所以新开一条 /admin/system-status：与 /admin/* 同一道门禁，形状独立。
//
// # 取数分工（与 internal/sysmon/status.go 的注释是一对）
//
//	本机资源 / 运行时     ← sysmon.Sampler（**进程级单例**，见下面 statusSampler）
//	请求口径（窗口/今日） ← store.TodayAggregate / store.WindowAggregate（T06）
//	依赖事实             ← admin 自己知道的（号池 / 上游 / 库）+ Config 注入的 Redis 模式
//
// # 不得编造的两处（PLAN §0.5 已裁定）
//
//	channel_affinity  本仓无 HybridCache 亲和缓存 ⇒ 由 sysmon 显式置为不适用
//	disk_cache        本仓**也没有**磁盘缓存层（只有 logbuf 的 jsonl 落盘，那是日志
//	                  不是缓存）⇒ 传 nil，页面上那一组整块不渲染。
//	                  留一个 entries:0 出来会看起来像"启用着但是空的"，比不显示更坏。
package admin

import (
	"net/http"
	"time"

	"workbuddy2api/internal/store"
	"workbuddy2api/internal/sysmon"
)

// statusSampler 进程级采样器。
//
// ⚠ **必须**是包级单例：CPU 占用率是"两次采样之差"（sysmon 包注释），
// 每次请求新建一个 Sampler 就永远没有基线 ⇒ cpu_ok 恒为 false、CPU 永远显示"未上报"。
// 这正是 sysmon 当初做成类型而不是包级函数的原因（见其 Sampler 注释）。
var statusSampler = sysmon.New()

// systemStatus 组装并返回状态页的响应体。
func (h *Handler) systemStatus(w http.ResponseWriter, r *http.Request) {
	snap := statusSampler.Sample()

	src := sysmon.Sources{
		Version:   store.Version,
		RedisMode: h.cfg.RedisMode,
		SQLite:    sqliteStatusBlock(),
		Upstreams: upstreamBlocks(h),
		Pool:      poolBlock(h),
		// JSONFiles / DiskCache 留 nil ⇒ 对应卡片不渲染（理由见文件头）。
	}
	src.ActiveConns = activeConns(h)

	if store.DB != nil {
		// 窗口口径：**必须**用 sysmon.WindowSeconds 这个契约窗口，而不是随手写 300。
		// 两者一旦漂移，页面上的"近 N 分钟"与 RPM 的分母就会互不一致。
		now := time.Now().Unix()
		if reqs, succ, avg, err := store.WindowAggregate(now-sysmon.WindowSeconds, now); err == nil {
			src.WindowRequests = reqs
			src.WindowErrors = reqs - succ
			src.AvgLatencyMs = avg
		}
		if reqs, tokens, cost, err := store.TodayAggregate(startOfLocalDay(time.Now()).Unix()); err == nil {
			src.TodayRequests = reqs
			src.TodayTokens = tokens
			src.TodayCost = cost
		}
	}

	payload := sysmon.BuildStatus(snap, src)
	if store.DB == nil {
		// 库没打开 ⇒ 请求口径与库信息都是"没有"而不是"零"。必须说出来：
		// 一个 0 会被读成"今天一个请求都没有"，那是**假事实**。
		payload.Service.Warnings = append(payload.Service.Warnings,
			"数据库未启用（store 未 Open）：请求口径与库信息不可用，不是 0")
	}
	writeJSON(w, http.StatusOK, payload)
}

// startOfLocalDay 本地日 0 点（T06 的 TodayAggregate 要求调用方给，见其注释）。
func startOfLocalDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// poolBlock 号池计数。
//
// ⚠ 三个字段的**单位不同**，页面上必须各自标对（改标签的那次见 webui-status.html）：
//
//	healthy  账号数     健康号
//	cooling  账号数     冷却中（含常规冷却与熔断期）
//	tripped  **上游数** 当前处于熔断期的 upstream 个数 —— 不是账号数！
//	autodisabled 账号数 已被禁用
//	inflight 请求数     各账号 in_flight 之和
func poolBlock(h *Handler) *sysmon.PoolBlock {
	if h.cfg.Pool == nil {
		return nil
	}
	_, healthy, cooling, disabled, _ := h.cfg.Pool.CountsDetailed()
	tripped := 0
	for _, ps := range h.cfg.Pool.ProviderStates() {
		if ps.Tripped {
			tripped++
		}
	}
	return &sysmon.PoolBlock{
		Healthy:      healthy,
		Cooling:      cooling,
		Tripped:      tripped,
		Autodisabled: disabled,
		Inflight:     activeConns(h),
	}
}

// activeConns 当前在途请求数 = 各账号 in_flight 之和。
//
// ⚠ 这是**本仓口径**，不是 hyxapi 的：hyxapi 数的是挂在 relay-router 上的
// `/v1` 在途请求（middleware/stats.go），本仓没有那个计数器。PLAN §0.5 已裁定
// 不复刻其口径；这里给的是"账号侧在途"，页面副标题写的就是"当前在途请求数"，
// 两者一致。**没有**任何地方把它冒名成 hyxapi 的 active_connections。
func activeConns(h *Handler) int {
	if h.cfg.Pool == nil {
		return 0
	}
	n := 0
	for _, st := range h.cfg.Pool.List() {
		n += st.InFlight
	}
	return n
}

// upstreamBlocks 上游熔断态。
//
// 只有**有过失败记录**的上游会出现在这里（ProviderStates 的语义，见其注释）：
// 列表为空读作"没有上游在熔断"，而不是"未知"。
func upstreamBlocks(h *Handler) []sysmon.UpstreamBlock {
	if h.cfg.Pool == nil {
		return nil
	}
	states := h.cfg.Pool.ProviderStates()
	if len(states) == 0 {
		return nil
	}
	out := make([]sysmon.UpstreamBlock, 0, len(states))
	for _, ps := range states {
		out = append(out, sysmon.UpstreamBlock{Name: ps.Provider, Tripped: ps.Tripped})
	}
	return out
}

// sqliteStatusBlock 主库事实（契约里的 database；log_database 见 SQLiteBlock 注释）。
//
// 未打开时返回 OK=false 而不是 nil：**"没有数据库"是配置事实，要告知**
// （页面据此显示"数据库未启用，指标走内存聚合模式"），而 nil 会让那张卡整块消失，
// 读页面的人无从判断是没接还是没显示。
func sqliteStatusBlock() *sysmon.SQLiteBlock {
	blk := &sysmon.SQLiteBlock{
		LogDatabase:     "same",
		LogDatabaseNote: "本仓单文件单连接池：日志与主库同库（无第二个日志库，PLAN §2.A A10）",
	}
	if store.DB == nil {
		return blk
	}
	blk.OK = true
	blk.Tables = len(store.Tables())
	var rows int64
	if err := store.DB.Model(&store.Log{}).Count(&rows).Error; err == nil {
		blk.LogRows = rows
	}
	// 库文件大小：用 PRAGMA 而不是 os.Stat，因为 store 不暴露库文件路径
	// （路径的权威在 cmd/server 的 dbPath，本包不该再发明一个来源）。
	var pageCount, pageSize int64
	if err := store.DB.Raw("PRAGMA page_count").Scan(&pageCount).Error; err == nil {
		if err := store.DB.Raw("PRAGMA page_size").Scan(&pageSize).Error; err == nil {
			blk.SizeBytes = pageCount * pageSize
		}
	}
	return blk
}
