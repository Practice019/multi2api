// settings.go 管理台「设置」页的数据契约。
//
// 为什么把 Settings 定义放在 admin 而不是复用 cmd/server 的 Config：
// admin 是 internal 子包，不能 import package main；把可编辑字段抽成一个
// 独立结构体，由宿主（cmd/server）负责「Config ↔ Settings」双向转换与落盘，
// admin 只认这个契约。这样配置文件格式的改动不会渗透进 HTTP 层。
package admin

import (
	"encoding/json"
	"log"
	"net/http"
)

// Settings 设置页可读写的全部字段。
//
// 分两类：
//   - 运行时生效（改完立即影响行为，无需重启）
//   - 需重启（已写入 config.json，但当前进程不重新读取）
//
// 哪一类由宿主的 Apply 决定并回传，UI 据此提示用户。
type Settings struct {
	// ---- 调度（运行时生效）----
	CheckinEnabled     bool  `json:"checkin_enabled"`
	KeepaliveEnabled   bool  `json:"keepalive_enabled"`
	CheckinHours       []int `json:"checkin_hours"`
	KeepaliveHours     []int `json:"keepalive_hours"`
	TravelAutoClaim    bool  `json:"travel_auto_claim"`
	TravelWatchSeconds int   `json:"travel_watch_interval_seconds"`

	// ---- 成长中心自动动作（运行时生效）----
	// GrowthAutoClaim 领奖：把条件已达成但未领的任务奖励领回来，是唯一让积分到账的动作。
	// 纯收益，默认开。
	GrowthAutoClaim bool `json:"growth_auto_claim"`
	// GrowthAutoAccept 接单：只是把任务接进列表开始计进度，**不发奖励**，默认关。
	GrowthAutoAccept bool `json:"growth_auto_accept_tasks"`
	// 补签是纯收益（只花补签卡）；下面三个会消耗能量/连登天数/抽奖次数，默认关。
	GrowthAutoMakeup bool `json:"growth_auto_makeup"`
	GrowthAutoRedeem bool `json:"growth_auto_redeem"`
	GrowthAutoOpen   bool `json:"growth_auto_open"`
	GrowthAutoDraw   bool `json:"growth_auto_draw"`

	// ---- 历史与日志（运行时生效）----
	CheckinLogKeepDays int `json:"checkin_log_keep_days"`
	RequestLogKeepDays int `json:"request_log_keep_days"`

	// ---- 以下需重启（保存进 config.json，当前进程不重读）----
	SoftRate              string `json:"cooldown_soft_rate"`
	UpstreamTimeoutSec    int    `json:"upstream_timeout_seconds"`
	UpstreamHeaderTimeout int    `json:"upstream_header_timeout_seconds"`
	UpstreamIdleTimeout   int    `json:"upstream_idle_timeout_seconds"`
	PoolMaxInFlight       int    `json:"pool_max_in_flight"`
	BreakerThreshold      int    `json:"pool_breaker_threshold"`
	BreakerCooldown       string `json:"pool_breaker_cooldown"`
	BreakerCooldownMax    string `json:"pool_breaker_cooldown_max"`
	SanitizeFingerprints  bool   `json:"sanitize_blacklist_fingerprints"`
	UpstashURL            string `json:"upstash_url"`
	SessionStickyEnabled  bool   `json:"session_sticky_enabled"`
	SessionStickyTTL      string `json:"session_sticky_ttl"`
	// 成长中心扫描间隔：ticker 在启动时创建，运行中改不了。
	GrowthWatchSeconds int `json:"growth_watch_interval_seconds"`

	// ---- 只读展示 ----
	Listen     string `json:"listen"`
	AuthDir    string `json:"auth_dir"`
	ConfigPath string `json:"config_path"`
	RedisMode  string `json:"redis_mode"`
	APIKeySet  bool   `json:"api_key_set"`
}

// SettingsStore 由宿主（cmd/server）实现：提供当前生效设置、校验并持久化补丁。
type SettingsStore interface {
	// Snapshot 返回当前进程生效的设置。
	Snapshot() Settings
	// Apply 校验并持久化；返回运行时已生效的字段名、需要重启才生效的字段名。
	// 校验失败必须返回 error，且不得写入磁盘。
	Apply(s Settings) (applied []string, needRestart []string, err error)
}

// 字段名清单（用于向 UI 说明哪些改动需要重启）。与 Settings 的 json tag 一致。
var restartRequiredFields = []string{
	"cooldown_soft_rate",
	"upstream_timeout_seconds",
	"upstream_header_timeout_seconds",
	"upstream_idle_timeout_seconds",
	"pool_max_in_flight",
	"pool_breaker_threshold",
	"pool_breaker_cooldown",
	"pool_breaker_cooldown_max",
	"sanitize_blacklist_fingerprints",
	"upstash_url",
	"session_sticky_enabled",
	"session_sticky_ttl",
	// 守卫轮的 ticker 在启动时创建，间隔无法在运行中改变 → 如实归入需重启。
	"travel_watch_interval_seconds",
	"growth_watch_interval_seconds",
}

var runtimeFields = []string{
	"checkin_enabled",
	"keepalive_enabled",
	"checkin_hours",
	"keepalive_hours",
	"travel_auto_claim",
	"growth_auto_accept_tasks",
	"growth_auto_makeup",
	"growth_auto_redeem",
	"growth_auto_open",
	"growth_auto_draw",
	"checkin_log_keep_days",
	"request_log_keep_days",
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeError(w, http.StatusNotImplemented, "设置存储未接线")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":       h.cfg.Settings.Snapshot(),
		"runtime_fields": runtimeFields,
		"restart_fields": restartRequiredFields,
		"request_log":    h.requestLogStats(),
	})
}

func (h *Handler) settingsUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeError(w, http.StatusNotImplemented, "设置存储未接线")
		return
	}
	var patch Settings
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "设置解析失败: "+err.Error())
		return
	}
	applied, needRestart, err := h.cfg.Settings.Apply(patch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("admin: 设置已保存，运行时生效=%v 需重启=%v", applied, needRestart)
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":     h.cfg.Settings.Snapshot(),
		"applied":      applied,
		"need_restart": needRestart,
		"request_log":  h.requestLogStats(),
	})
}

// requestLogStats 落盘日志概况（文件路径、条数、体积、保留天数）。
func (h *Handler) requestLogStats() map[string]any {
	if h.cfg.Ring == nil {
		return map[string]any{"enabled": false}
	}
	if s := h.cfg.Ring.Sink(); s != nil {
		return s.Stats()
	}
	return map[string]any{"enabled": false}
}
