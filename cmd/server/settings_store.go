// settings_store.go 管理台设置页的宿主持久化实现。
//
// 两条设计约束：
//  1. 落盘用「读原 JSON → 改嵌套键 → 写回」，而不是把 Config 结构体整体序列化。
//     否则用户手工在 config.json 里留的注释键、未知键、字段顺序都会在一次保存后消失。
//  2. 只有明确能在运行时生效的字段才立刻应用；其余照常写盘，但如实回报
//     「需重启」，绝不假装已生效。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/scheduler"
)

// settingsStore 实现 admin.SettingsStore。
type settingsStore struct {
	path string
	cfg  *Config
	sch  *scheduler.Scheduler
	log  *checkinlog.Log
	// setLogKeepDays 调整落盘请求日志的保留天数（由 main 注入，避免这里依赖 logbuf）。
	setLogKeepDays func(int)
}

func (s *settingsStore) Snapshot() admin.Settings {
	c := s.cfg
	checkinH, keepaliveH := s.sch.Hours()
	gAccept, gMakeup, gRedeem, gOpen, gDraw, gClaim := s.sch.GrowthToggles()
	return admin.Settings{
		CheckinEnabled:     s.sch.CheckinEnabled(),
		KeepaliveEnabled:   s.sch.KeepaliveEnabled(),
		CheckinHours:       checkinH,
		KeepaliveHours:     keepaliveH,
		TravelAutoClaim:    s.sch.TravelAutoClaimEnabled(),
		TravelWatchSeconds: int(s.sch.WatchInterval().Seconds()),

		GrowthAutoClaim:  gClaim,
		GrowthAutoAccept: gAccept,
		GrowthAutoMakeup: gMakeup,
		GrowthAutoRedeem: gRedeem,
		GrowthAutoOpen:   gOpen,
		GrowthAutoDraw:   gDraw,

		CheckinLogKeepDays: s.log.KeepDays(),
		RequestLogKeepDays: c.RequestLogKeepDays,

		SoftRate:              c.Cooldown.SoftRate,
		UpstreamTimeoutSec:    c.Upstream.TimeoutSeconds,
		UpstreamHeaderTimeout: c.Upstream.HeaderTimeoutSeconds,
		UpstreamIdleTimeout:   c.Upstream.IdleTimeoutSeconds,
		PoolMaxInFlight:       c.Pool.MaxInFlight,
		BreakerThreshold:      c.Pool.BreakerThreshold,
		BreakerCooldown:       c.Pool.BreakerCooldown,
		BreakerCooldownMax:    c.Pool.BreakerCooldownMax,
		SanitizeFingerprints:  c.Features.SanitizeBlacklistFingerprints,
		UpstashURL:            c.Upstash.URL,
		SessionStickyEnabled:  c.SessionSticky.Enabled,
		SessionStickyTTL:      c.SessionSticky.TTL,

		GrowthWatchSeconds: int(s.sch.GrowthWatchInterval().Seconds()),

		Listen:     c.Listen,
		AuthDir:    c.AuthDir,
		ConfigPath: s.path,
		APIKeySet:  c.APIKey != "",
	}
}

// Apply 校验 → 写盘 → 应用运行时字段。
// 校验失败在写盘之前返回，磁盘保持原样。
func (s *settingsStore) Apply(p admin.Settings) (applied, needRestart []string, err error) {
	if err := validateSettings(p); err != nil {
		return nil, nil, err
	}

	// 1) 写盘（合并进现有 JSON，保留未知键）
	if err := s.persist(p); err != nil {
		return nil, nil, fmt.Errorf("写入 %s 失败: %w", s.path, err)
	}

	// 2) 运行时生效
	s.sch.SetCheckinEnabled(p.CheckinEnabled)
	s.sch.SetKeepaliveEnabled(p.KeepaliveEnabled)
	if err := s.sch.SetHours(p.CheckinHours, p.KeepaliveHours); err != nil {
		return nil, nil, err
	}
	s.sch.SetTravelAutoClaim(p.TravelAutoClaim)
	s.sch.SetGrowthToggles(p.GrowthAutoAccept, p.GrowthAutoMakeup,
		p.GrowthAutoRedeem, p.GrowthAutoOpen, p.GrowthAutoDraw, p.GrowthAutoClaim)
	if p.CheckinLogKeepDays > 0 {
		s.log.SetKeepDays(p.CheckinLogKeepDays)
	}
	if p.RequestLogKeepDays > 0 && s.setLogKeepDays != nil {
		s.setLogKeepDays(p.RequestLogKeepDays)
	}
	applied = []string{
		"checkin_enabled", "keepalive_enabled", "checkin_hours", "keepalive_hours",
		"travel_auto_claim", "checkin_log_keep_days", "request_log_keep_days",
		"growth_auto_claim", "growth_auto_accept_tasks", "growth_auto_makeup",
		"growth_auto_redeem", "growth_auto_open", "growth_auto_draw",
	}

	// 3) 只写盘、需重启才生效（守卫轮 ticker 的创建时机、上游 client 的超时、
	//    账号池限流器、Redis 连接都是在启动时一次性装配的，运行中改不了）。
	needRestart = []string{
		"cooldown_soft_rate",
		"upstream_timeout_seconds", "upstream_header_timeout_seconds", "upstream_idle_timeout_seconds",
		"pool_max_in_flight", "pool_breaker_threshold", "pool_breaker_cooldown", "pool_breaker_cooldown_max",
		"sanitize_blacklist_fingerprints", "upstash_url",
		"session_sticky_enabled", "session_sticky_ttl",
		"travel_watch_interval_seconds",
		"growth_watch_interval_seconds",
	}
	// 把新值同步进内存 cfg，让 Snapshot 显示的就是刚保存的值。
	s.applyToMemory(p)
	return applied, needRestart, nil
}

func (s *settingsStore) applyToMemory(p admin.Settings) {
	c := s.cfg
	c.Cooldown.SoftRate = p.SoftRate
	c.Upstream.TimeoutSeconds = p.UpstreamTimeoutSec
	c.Upstream.HeaderTimeoutSeconds = p.UpstreamHeaderTimeout
	c.Upstream.IdleTimeoutSeconds = p.UpstreamIdleTimeout
	c.Pool.MaxInFlight = p.PoolMaxInFlight
	c.Pool.BreakerThreshold = p.BreakerThreshold
	c.Pool.BreakerCooldown = p.BreakerCooldown
	c.Pool.BreakerCooldownMax = p.BreakerCooldownMax
	c.Features.SanitizeBlacklistFingerprints = p.SanitizeFingerprints
	c.Upstash.URL = p.UpstashURL
	c.SessionSticky.Enabled = p.SessionStickyEnabled
	c.SessionSticky.TTL = p.SessionStickyTTL
	c.Schedule.CheckinEnabled = p.CheckinEnabled
	c.Schedule.KeepaliveEnabled = p.KeepaliveEnabled
	c.Schedule.CheckinHours = p.CheckinHours
	c.Schedule.KeepaliveHours = p.KeepaliveHours
	c.Admin.TravelWatchIntervalSeconds = p.TravelWatchSeconds
	travelAuto := p.TravelAutoClaim
	c.Admin.TravelAutoClaim = &travelAuto
	c.Admin.CheckinLogKeepDays = p.CheckinLogKeepDays
	c.RequestLogKeepDays = p.RequestLogKeepDays
	// 成长中心自动动作开关
	c.GrowthAutoClaim = p.GrowthAutoClaim
	c.GrowthAutoAccept = p.GrowthAutoAccept
	c.GrowthAutoMakeup = p.GrowthAutoMakeup
	c.GrowthAutoRedeem = p.GrowthAutoRedeem
	c.GrowthAutoOpen = p.GrowthAutoOpen
	c.GrowthAutoDraw = p.GrowthAutoDraw
	if p.GrowthWatchSeconds > 0 {
		c.Admin.GrowthWatchIntervalSeconds = p.GrowthWatchSeconds
	}
}

func validateSettings(p admin.Settings) error {
	checkHour := func(name string, hours []int) error {
		for _, h := range hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("%s: %d 不是合法小时（0-23）", name, h)
			}
		}
		return nil
	}
	if err := checkHour("签到时点", p.CheckinHours); err != nil {
		return err
	}
	if err := checkHour("保活时点", p.KeepaliveHours); err != nil {
		return err
	}
	if p.SoftRate != "" {
		if _, err := time.ParseDuration(p.SoftRate); err != nil {
			return fmt.Errorf("429 冷却时长: %w", err)
		}
	}
	for name, v := range map[string]string{
		"熔断基础时长":   p.BreakerCooldown,
		"熔断退避封顶":   p.BreakerCooldownMax,
		"会话粘性 TTL": p.SessionStickyTTL,
	} {
		if v == "" {
			continue
		}
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if p.UpstreamTimeoutSec < 0 || p.UpstreamHeaderTimeout < 0 || p.UpstreamIdleTimeout < 0 {
		return fmt.Errorf("上游超时不能为负")
	}
	if p.PoolMaxInFlight < 0 || p.BreakerThreshold < 0 {
		return fmt.Errorf("账号池参数不能为负")
	}
	if p.TravelWatchSeconds < 0 {
		return fmt.Errorf("旅行守卫间隔不能为负")
	}
	if p.CheckinLogKeepDays < 0 || p.RequestLogKeepDays < 0 {
		return fmt.Errorf("日志保留天数不能为负")
	}
	return nil
}

// persist 把设置合并进 config.json，保留文件里已有的其它键。
func (s *settingsStore) persist(p admin.Settings) error {
	doc := map[string]any{}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &doc) // 解析失败就用空文档重写
	}

	set := func(section string, kv map[string]any) {
		sub, _ := doc[section].(map[string]any)
		if sub == nil {
			sub = map[string]any{}
		}
		for k, v := range kv {
			sub[k] = v
		}
		doc[section] = sub
	}
	// unset 从某个段里删掉一个键。用于把历史遗留的别名键收敛掉：
	// 只写新键而不删旧键的话，文件里会同时存在两个意思相同、值可能相反的键，
	// 下次读哪个就说不清了。
	unset := func(section, key string) {
		if sub, ok := doc[section].(map[string]any); ok {
			delete(sub, key)
		}
	}

	set("schedule", map[string]any{
		"checkin_enabled":   p.CheckinEnabled,
		"keepalive_enabled": p.KeepaliveEnabled,
		"checkin_hours":     p.CheckinHours,
		"keepalive_hours":   p.KeepaliveHours,
	})
	set("cooldown", map[string]any{"soft_rate": p.SoftRate})
	set("upstream", map[string]any{
		"timeout_seconds":        p.UpstreamTimeoutSec,
		"header_timeout_seconds": p.UpstreamHeaderTimeout,
		"idle_timeout_seconds":   p.UpstreamIdleTimeout,
	})
	set("pool", map[string]any{
		"max_in_flight":        p.PoolMaxInFlight,
		"breaker_threshold":    p.BreakerThreshold,
		"breaker_cooldown":     p.BreakerCooldown,
		"breaker_cooldown_max": p.BreakerCooldownMax,
	})
	set("features", map[string]any{"sanitize_blacklist_fingerprints": p.SanitizeFingerprints})
	set("upstash", map[string]any{"url": p.UpstashURL})
	set("session_sticky", map[string]any{
		"enabled": p.SessionStickyEnabled,
		"ttl":     p.SessionStickyTTL,
	})
	set("admin", map[string]any{
		"travel_auto_claim":             p.TravelAutoClaim,
		"travel_watch_interval_seconds": p.TravelWatchSeconds,
		"checkin_log_keep_days":         p.CheckinLogKeepDays,

		"growth_auto_claim":             p.GrowthAutoClaim,
		"growth_auto_accept_tasks":      p.GrowthAutoAccept,
		"growth_auto_makeup":            p.GrowthAutoMakeup,
		"growth_auto_redeem":            p.GrowthAutoRedeem,
		"growth_auto_open":              p.GrowthAutoOpen,
		"growth_auto_draw":              p.GrowthAutoDraw,
		"growth_watch_interval_seconds": p.GrowthWatchSeconds,
	})
	// 别名键收敛：写入规范键 growth_auto_claim 后，删掉曾用过（且曾被静默忽略）的
	// growth_auto_claim_tasks，避免两个键并存给出互相矛盾的信号。
	unset("admin", "growth_auto_claim_tasks")
	doc["request_log_keep_days"] = p.RequestLogKeepDays

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return os.Rename(tmp, s.path)
}

// newSettingsStore 组装宿主实现。
func newSettingsStore(path string, cfg *Config, sch *scheduler.Scheduler,
	log *checkinlog.Log, setLogKeepDays func(int)) admin.SettingsStore {
	return &settingsStore{
		path:           path,
		cfg:            cfg,
		sch:            sch,
		log:            log,
		setLogKeepDays: setLogKeepDays,
	}
}
