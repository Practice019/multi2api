// status.go 把 Snapshot 与外部事实组装成**状态页的响应体**（PLAN §0.5 / T12）。
//
// # 本文件只做「组装与判档」，不取数
//
// 取数分三路，各自有归属：
//
//	本机资源 / 运行时   ← 同包的 Sampler.Sample()（Linux/Windows 各一份实现）
//	请求口径（窗口/今日）← store.TodayAggregate / store.WindowAggregate（T06）
//	依赖事实（库/号池/上游/Redis）← 装配层（cmd/server 或 admin）注入
//
// 组装放在这里而不是放在 HTTP 处理器里，有两个具体理由：
//
//	① 阈值与分档是**口径**，不是渲染细节。口径只有一个来源，才能被单测钉住
//	   （阈值漂了 75→60，页面颜色会整体变形，而没有任何报错）。
//	② 处理器只剩「取数 → 组装 → 写 JSON」三行，接线错一眼可见。
//
// # 与 hyxapi 的关系（AGPL 动作分类：只借判断）
//
// 字段集与阈值口径只借判断，来源见 PLAN §0.5 的引文（hyxapi 的
// controller/system_status.go:96-110 阈值与成功率分档、:85/:134 的 window_seconds）。
// 代码是照**本仓**的形态重写的：本仓没有 HybridCache 亲和缓存（§0.5 已裁定），
// 也没有第二个日志库（§2.A A10），这两处一律**显式置空并标注不适用**，
// 绝不用 0 或「未启用」冒充一个不存在的读数。
package sysmon

import (
	"runtime"
	"strconv"
)

// 阈值三档（§0.5：hyxapi/controller/system_status.go:96-102）。
//
// ⚠ 判据是**闭区间下界**：>=90 严重、>=75 警告。写成 > 会在 90.0 这个点上
// 分叉（采样值常常正好落在整数上），而那种偏差没有任何测试会自然覆盖到。
const (
	ThresholdWarnPct = 75.0
	ThresholdCritPct = 90.0
)

// 成功率分档（§0.5：hyxapi 同文件 :103-110）：<95 警告 / <90 严重。
//
// ⚠ 与上面三档**方向相反**（这里是「越小越坏」），所以不能共用 PctVerdict。
const (
	SuccessWarnPct = 95.0
	SuccessCritPct = 90.0
)

// WindowSeconds 请求健康窗口（§0.5：hyxapi :85/:134 的 window_seconds:300）。
//
// 它是**契约的一部分**而不只是个渲染参数：窗口一改，同一串数字的含义就变了
// （RPM 的分母、成功率的样本集）。所以它随响应体一起下发，页面拿它显示文案。
const WindowSeconds = 300

// 档位取值。与页面上的三种配色一一对应（ok / warning / critical）。
const (
	VerdictOK       = "ok"
	VerdictWarning  = "warning"
	VerdictCritical = "critical"
)

// PctVerdict 百分比类指标（CPU / 内存 / 磁盘）的三档判定。
func PctVerdict(pct float64) string {
	switch {
	case pct >= ThresholdCritPct:
		return VerdictCritical
	case pct >= ThresholdWarnPct:
		return VerdictWarning
	default:
		return VerdictOK
	}
}

// SuccessVerdict 成功率（0..100）的分档：越大越好。
func SuccessVerdict(rate float64) string {
	switch {
	case rate < SuccessCritPct:
		return VerdictCritical
	case rate < SuccessWarnPct:
		return VerdictWarning
	default:
		return VerdictOK
	}
}

// worse 取两个档位里更坏的那个。
//
// 为什么要它：service.status 是**汇总**判据（任一资源进警告就整体警告）。
// 用最大值比较字符串是错的（"warning" > "critical" 按字典序），必须显式排名。
func worse(a, b string) string {
	if verdictRank(b) > verdictRank(a) {
		return b
	}
	return a
}

func verdictRank(v string) int {
	switch v {
	case VerdictCritical:
		return 2
	case VerdictWarning:
		return 1
	default:
		return 0
	}
}

// ── 响应体的分块结构 ───────────────────────────────────────────────────────
//
// ⚠ 字段名是**页面契约**（internal/server/webui-status.html 按这些键渲染），
// 改一个键名就会让对应的卡片静默变成「未上报」—— 页面不会报错，只是空着。
// 所以这里的分块刻意保持页面已有的嵌套形状（service/resource/health/deps/cache），
// 而不是摊平成 hyxapi 那套顶层字段：形状换来换去只会让两边的键名各漂一次。

// ServiceBlock 服务总体状态 + 进程身份。
type ServiceBlock struct {
	Status            string   `json:"status"`
	Warnings          []string `json:"warnings"`
	Version           string   `json:"version"`
	GoVersion         string   `json:"go_version"`
	Runtime           string   `json:"runtime"`
	UptimeSeconds     float64  `json:"uptime_seconds"`
	Goroutines        int      `json:"goroutines"`
	ActiveConnections int      `json:"active_connections"`
}

// ResourceBlock 宿主资源与进程内存画像（字段与 Snapshot 同名，页面直接用）。
type ResourceBlock struct {
	CPUOK          bool    `json:"cpu_ok"`
	CPUPct         float64 `json:"cpu_pct"`
	MemOK          bool    `json:"mem_ok"`
	MemPct         float64 `json:"mem_pct"`
	MemTotalBytes  uint64  `json:"mem_total_bytes"`
	MemUsedBytes   uint64  `json:"mem_used_bytes"`
	DiskOK         bool    `json:"disk_ok"`
	DiskPct        float64 `json:"disk_pct"`
	DiskTotalBytes uint64  `json:"disk_total_bytes"`
	DiskFreeBytes  uint64  `json:"disk_free_bytes"`
	DiskPath       string  `json:"disk_path,omitempty"`
	ActiveConns    int     `json:"active_conns"`
	UptimeSec      float64 `json:"uptime_sec"`
	HeapAllocBytes uint64  `json:"heap_alloc_bytes"`
	GCCount        uint32  `json:"gc_count"`
	GCPauseNS      uint64  `json:"gc_pause_ns"`
}

// HealthBlock 请求口径：窗口（§0.5 窗口 300s）+ 今日（T06 聚合）。
type HealthBlock struct {
	WindowSeconds    int     `json:"window_seconds"`
	RPM5m            float64 `json:"rpm_5m"`
	WindowRequests5m int64   `json:"window_requests_5m"`
	WindowErrors5m   int64   `json:"window_errors_5m"`
	SuccessRate      float64 `json:"success_rate"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	TodayRequests    int64   `json:"today_requests"`
	TodayTokens      int64   `json:"today_tokens"`
	TodayCost        int64   `json:"today_cost"`
	Goroutines       int     `json:"goroutines"`
}

// SQLiteBlock 主库（= 契约里的 database）。
type SQLiteBlock struct {
	OK        bool  `json:"ok"`
	SizeBytes int64 `json:"size_bytes"`
	Tables    int   `json:"tables"`
	LogRows   int64 `json:"log_rows"`
	// LogDatabase 是契约里的 log_database。本仓**单文件单连接池**（§2.A A10：
	// 没有第二个日志库），所以恒为 "same"，并由 LogDatabaseNote 说明理由 ——
	// 留空会让读页面的人以为「日志库还没接上」。
	LogDatabase     string `json:"log_database"`
	LogDatabaseNote string `json:"log_database_note"`
}

// RedisBlock Redis 事实。
//
// ⚠ §0.5 与任务书都要求：Redis 未启用**只加 warning 文案、不改 service.status**。
// 因为「没配 Redis」是**配置事实**不是故障（本仓有 Noop + 本地文件降级，见
// PLAN §1 硬结论 2）—— 把它算进 status 会让每次部署都顶着一个假红灯。
type RedisBlock struct {
	Mode    string `json:"mode"`
	Enabled bool   `json:"enabled"`
	Warning string `json:"warning"`
}

// PoolBlock 号池计数（字段名与页面渲染函数一一对应）。
type PoolBlock struct {
	Healthy      int `json:"healthy"`
	Cooling      int `json:"cooling"`
	Tripped      int `json:"tripped"`
	Autodisabled int `json:"autodisabled"`
	Inflight     int `json:"inflight"`
}

// FileBlock 一个 data/ 下的状态文件。
type FileBlock struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Bytes int64  `json:"bytes,omitempty"`
	Note  string `json:"note,omitempty"`
}

// UpstreamBlock 一个上游的熔断态。
type UpstreamBlock struct {
	Name    string `json:"name"`
	Tripped bool   `json:"tripped"`
	Note    string `json:"note,omitempty"`
}

// DepsBlock 依赖健康（页面第 ④ 组）。
type DepsBlock struct {
	SQLite    *SQLiteBlock    `json:"sqlite,omitempty"`
	Redis     *RedisBlock     `json:"redis,omitempty"`
	Pool      *PoolBlock      `json:"pool,omitempty"`
	JSONFiles []FileBlock     `json:"json_files"`
	Upstreams []UpstreamBlock `json:"upstreams"`
}

// DiskCacheBlock 磁盘缓存（契约里的 disk_cache）。
type DiskCacheBlock struct {
	Files  int64 `json:"files"`
	Bytes  int64 `json:"bytes"`
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`
}

// AffinityBlock 渠道亲和缓存（契约里的 channel_affinity）。
//
// ⚠⚠ 本仓**没有** HybridCache 亲和缓存（§0.5 明确不照搬），选号走无状态轮询。
// 所以这里只保留 Applicable=false + Note 一句，**不放 entries/algo 这类会被人
// 当成读数的字段** —— 编一个 entries:0 出来，比不显示更坏：它看起来像"启用着但空"。
type AffinityBlock struct {
	Applicable bool   `json:"applicable"`
	Enabled    bool   `json:"enabled"`
	Note       string `json:"note"`
}

// CacheBlock 缓存与容量（页面第 ⑤ 组）。
type CacheBlock struct {
	Disk     *DiskCacheBlock `json:"disk,omitempty"`
	Affinity *AffinityBlock  `json:"affinity,omitempty"`
}

// StatusPayload 就是 GET /admin/system-status 的响应体。
type StatusPayload struct {
	Service  ServiceBlock  `json:"service"`
	Resource ResourceBlock `json:"resource"`
	Health   HealthBlock   `json:"health"`
	Deps     DepsBlock     `json:"deps"`
	Cache    CacheBlock    `json:"cache"`
}

// Sources 是组装所需的**外部事实**（本包不认识号池、store、Redis，只认这些数）。
//
// 传入 nil 的指针表示「这一项本部署没有」⇒ 对应分块整块不渲染，而不是渲染成 0。
type Sources struct {
	Version     string
	ActiveConns int
	RedisMode   string // "" / "noop" / "redis"
	Pool        *PoolBlock
	SQLite      *SQLiteBlock
	JSONFiles   []FileBlock
	Upstreams   []UpstreamBlock
	// 窗口与今日口径（T06 聚合）。Window 的窗口长度由本包按 WindowSeconds 判，
	// 调用方只需给出窗口内的计数 —— 但**必须真的是 300s 窗**，否则 RPM 是错的。
	WindowRequests int64
	WindowErrors   int64
	AvgLatencyMs   float64
	TodayRequests  int64
	TodayTokens    int64
	TodayCost      int64
	DiskCache      *DiskCacheBlock
}

// BuildStatus 组装响应体。纯函数：同样的输入必然得到同样的输出（可单测）。
func BuildStatus(snap Snapshot, src Sources) StatusPayload {
	version := src.Version
	if version == "" {
		version = "dev"
	}
	warnings := make([]string, 0, 4)

	// ── 资源三档 + 采样失败（采样失败**必须**说出来：0 不是读数）
	res := ResourceBlock{
		CPUOK: snap.CPUOK, CPUPct: snap.CPUPct,
		MemOK: snap.MemOK, MemPct: snap.MemPct,
		MemTotalBytes: snap.MemTotalBytes, MemUsedBytes: snap.MemUsedBytes,
		DiskOK: snap.DiskOK, DiskPct: snap.DiskPct,
		DiskTotalBytes: snap.DiskTotalBytes, DiskFreeBytes: snap.DiskFreeBytes,
		DiskPath:       snap.DiskPath,
		ActiveConns:    src.ActiveConns,
		UptimeSec:      snap.UptimeSec,
		HeapAllocBytes: snap.HeapAllocBytes,
		GCCount:        snap.GCCount, GCPauseNS: snap.GCPauseNS,
	}
	verdict := VerdictOK
	for _, f := range snap.FailedSections() {
		switch f {
		case "cpu":
			warnings = append(warnings, "CPU 采样不可用（首次采样无基线，下一次会自动恢复）")
		case "mem":
			warnings = append(warnings, "内存采样不可用")
		case "disk":
			warnings = append(warnings, "磁盘采样不可用（数据目录所在卷探测失败）")
		}
		verdict = worse(verdict, VerdictWarning)
	}
	if snap.CPUOK {
		verdict = worse(verdict, PctVerdict(snap.CPUPct))
	}
	if snap.MemOK {
		verdict = worse(verdict, PctVerdict(snap.MemPct))
	}
	if snap.DiskOK {
		verdict = worse(verdict, PctVerdict(snap.DiskPct))
	}

	// ── 请求健康：RPM 按**契约窗口**算，不按调用方给的别的窗口
	rpm := float64(src.WindowRequests) / (float64(WindowSeconds) / 60.0)
	success := 100.0
	if src.WindowRequests > 0 {
		success = float64(src.WindowRequests-src.WindowErrors) / float64(src.WindowRequests) * 100
	}
	// ⚠ 无样本（窗口内 0 个请求）时**不判档**：成功率保持 100 且不进 verdict。
	// 否则一个刚启动的空窗会被判成 critical（0/0 算出来的分档没有任何样本意义）。
	if src.WindowRequests > 0 {
		verdict = worse(verdict, SuccessVerdict(success))
		if src.WindowErrors > 0 {
			warnings = append(warnings, "近 "+strconv.Itoa(WindowSeconds/60)+" 分钟有 "+strconv.FormatInt(src.WindowErrors, 10)+" 个错误请求")
		}
	}

	// ── Redis：**只加文案，不改 status**（§0.5 与任务书的硬口径）
	if src.RedisMode == "" || src.RedisMode == "noop" {
		warnings = append(warnings, "Redis 未启用（当前走内存 + 本地文件降级），不影响请求链路")
	}

	payload := StatusPayload{
		Service: ServiceBlock{
			Status:            verdict,
			Warnings:          warnings,
			Version:           version,
			GoVersion:         runtime.Version(),
			Runtime:           runtime.GOOS + "/" + runtime.GOARCH,
			UptimeSeconds:     snap.UptimeSec,
			Goroutines:        snap.Goroutines,
			ActiveConnections: src.ActiveConns,
		},
		Resource: res,
		Health: HealthBlock{
			WindowSeconds:    WindowSeconds,
			RPM5m:            rpm,
			WindowRequests5m: src.WindowRequests,
			WindowErrors5m:   src.WindowErrors,
			SuccessRate:      success,
			AvgLatencyMs:     src.AvgLatencyMs,
			TodayRequests:    src.TodayRequests,
			TodayTokens:      src.TodayTokens,
			TodayCost:        src.TodayCost,
			Goroutines:       snap.Goroutines,
		},
		Deps: DepsBlock{
			SQLite:    src.SQLite,
			Redis:     redisBlock(src.RedisMode),
			Pool:      src.Pool,
			JSONFiles: src.JSONFiles,
			Upstreams: src.Upstreams,
		},
		Cache: CacheBlock{
			Disk: src.DiskCache,
			// channel_affinity：**不适用**，显式置空 + 标注（不编造数据）。
			Affinity: &AffinityBlock{
				Applicable: false,
				Enabled:    false,
				Note:       "本仓无 HybridCache 亲和缓存，选号走无状态轮询 —— 该项不适用",
			},
		},
	}
	return payload
}

// redisBlock 把模式名翻成页面要的三元组。
func redisBlock(mode string) *RedisBlock {
	switch mode {
	case "redis":
		return &RedisBlock{Mode: "redis", Enabled: true}
	case "", "noop":
		return &RedisBlock{
			Mode: "noop", Enabled: false,
			Warning: "未启用：当前走内存 + 本地文件降级（配置事实，不是故障）",
		}
	default:
		return &RedisBlock{Mode: mode, Enabled: true}
	}
}
