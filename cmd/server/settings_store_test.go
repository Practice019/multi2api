package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/store"
)

// 复现并守住阶段 0 评审的 F1：**部分 PUT 不得覆盖未提交的配置**。
//
// # 缺陷原貌
//
// parse 用 `var p admin.Settings; json.Unmarshal(patch, &p)` ——
// 补丁里没出现的键保持零值，随后整个 p 被写回 config.json。
// 于是：
//
//	PUT {"checkin_enabled":false}
//	  → upstream.timeout_seconds 120 → 0
//	  → pool.max_in_flight         8 → 0
//	  → cooldown.soft_rate     "60s" → ""
//	  → session_sticky.enabled  true → false
//
// 用户只改一个开关，其它配置静默丢失。改造前就存在（非本次引入），
// 但危害真实，故一并修掉。

// settingsStoreForTest 造一个最小的 settingsStore（不启动调度器/日志）。
func settingsStoreForTest(t *testing.T, cfg *Config) *settingsStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &settingsStore{path: path, cfg: cfg}
}

// TestParseKeepsUnmentionedKeys 补丁里没提到的键必须保持原值。
func TestParseKeepsUnmentionedKeys(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.TimeoutSeconds = 120
	cfg.Upstream.HeaderTimeoutSeconds = 30
	cfg.Pool.MaxInFlight = 8
	cfg.Pool.BreakerThreshold = 5
	cfg.Cooldown.SoftRate = "60s"
	cfg.SessionSticky.Enabled = true
	cfg.Schedule.CheckinHours = []int{7, 19}
	cfg.Schedule.KeepaliveHours = []int{23}

	s := settingsStoreForTest(t, cfg)

	p, err := s.parse(json.RawMessage(`{"checkin_enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}

	// 被提交的那个键要生效
	if p.CheckinEnabled {
		t.Error("补丁里的 checkin_enabled=false 应生效")
	}
	// 未提交的键一个都不许被清零
	if p.UpstreamTimeoutSec != 120 {
		t.Errorf("upstream.timeout_seconds 被冲掉了: %d，期望 120", p.UpstreamTimeoutSec)
	}
	if p.UpstreamHeaderTimeout != 30 {
		t.Errorf("upstream.header_timeout_seconds 被冲掉了: %d", p.UpstreamHeaderTimeout)
	}
	if p.PoolMaxInFlight != 8 {
		t.Errorf("pool.max_in_flight 被冲掉了: %d，期望 8", p.PoolMaxInFlight)
	}
	if p.BreakerThreshold != 5 {
		t.Errorf("pool.breaker_threshold 被冲掉了: %d", p.BreakerThreshold)
	}
	if p.SoftRate != "60s" {
		t.Errorf("cooldown.soft_rate 被冲掉了: %q", p.SoftRate)
	}
	if !p.SessionStickyEnabled {
		t.Error("session_sticky.enabled 被冲掉了（true → false）")
	}
	if len(p.CheckinHours) != 2 || p.CheckinHours[0] != 7 {
		t.Errorf("checkin_hours 被冲掉了: %v", p.CheckinHours)
	}
	if len(p.KeepaliveHours) != 1 || p.KeepaliveHours[0] != 23 {
		t.Errorf("keepalive_hours 被冲掉了: %v", p.KeepaliveHours)
	}
}

// TestParseAppliesPatchOnTopOfCurrent 补丁要**覆盖**基底（而不是被基底盖掉）。
func TestParseAppliesPatchOnTopOfCurrent(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.TimeoutSeconds = 120
	cfg.Pool.MaxInFlight = 8

	s := settingsStoreForTest(t, cfg)
	p, err := s.parse(json.RawMessage(`{"upstream_timeout_seconds":300,"pool_max_in_flight":99}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.UpstreamTimeoutSec != 300 {
		t.Errorf("补丁应覆盖基底: 得到 %d，期望 300", p.UpstreamTimeoutSec)
	}
	if p.PoolMaxInFlight != 99 {
		t.Errorf("补丁应覆盖基底: 得到 %d，期望 99", p.PoolMaxInFlight)
	}
}

// TestParseEmptyPatchKeepsEverything 空补丁 = 不改任何东西。
func TestParseEmptyPatchKeepsEverything(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.TimeoutSeconds = 77
	cfg.Pool.MaxInFlight = 3
	s := settingsStoreForTest(t, cfg)

	for _, patch := range []string{``, `{}`} {
		p, err := s.parse(json.RawMessage(patch))
		if err != nil {
			t.Fatalf("patch=%q: %v", patch, err)
		}
		if p.UpstreamTimeoutSec != 77 || p.PoolMaxInFlight != 3 {
			t.Errorf("patch=%q 不该改动任何值: timeout=%d maxInFlight=%d",
				patch, p.UpstreamTimeoutSec, p.PoolMaxInFlight)
		}
	}
}

// TestParseRejectsMalformed 坏 JSON 要报错，而不是静默返回零值基座。
func TestParseRejectsMalformed(t *testing.T) {
	s := settingsStoreForTest(t, &Config{})
	if _, err := s.parse(json.RawMessage(`{"checkin_enabled":`)); err == nil {
		t.Error("损坏的 JSON 应报错")
	}
}

// TestCurrentSettingsCoversAllWritableFields currentSettings 与 applyToMemory
// 必须**字段对称**：一个是读、一个是写。
//
// # 为什么需要这条
//
// 若 currentSettings 漏了某个 applyToMemory 会写的字段，
// 那个字段每次保存都会被"基底的零值"覆盖 —— 也就是 F1 的复发形态。
// 这条测试用"写进去再读出来"的往返来证明对称性，
// 将来加字段时若只改了一边，这里会红。
func TestCurrentSettingsCoversAllWritableFields(t *testing.T) {
	// 先把所有可写字段设成**非零且互不相同**的值
	src := admin.Settings{
		SoftRate:              "45s",
		UpstreamTimeoutSec:    101,
		UpstreamHeaderTimeout: 102,
		UpstreamIdleTimeout:   103,
		PoolMaxInFlight:       104,
		BreakerThreshold:      105,
		BreakerCooldown:       "106s",
		BreakerCooldownMax:    "107s",
		ReviveProbeHours:      108,
		SanitizeFingerprints:  true,
		UpstashURL:            "https://upstash.example",
		SessionStickyEnabled:  true,
		SessionStickyTTL:      "109s",
		CheckinEnabled:        true,
		KeepaliveEnabled:      true,
		CheckinHours:          []int{1, 2},
		KeepaliveHours:        []int{3},
	}

	cfg := &Config{}
	s := settingsStoreForTest(t, cfg)

	// 用 applyToMemory 写入内存（这是"保存后生效"的路径）
	s.applyToMemory(src)

	// 再用 currentSettings 读回来
	got := s.currentSettings()

	// 逐字段比对：漏掉的字段会读出零值 → 报错
	check := func(name string, want, have any) {
		t.Helper()
		wj, _ := json.Marshal(want)
		hj, _ := json.Marshal(have)
		if string(wj) != string(hj) {
			t.Errorf("%s 不对称: applyToMemory 写入 %s，currentSettings 读出 %s —— "+
				"该字段每次保存都会被零值冲掉（F1 复发形态）", name, wj, hj)
		}
	}
	check("SoftRate", src.SoftRate, got.SoftRate)
	check("UpstreamTimeoutSec", src.UpstreamTimeoutSec, got.UpstreamTimeoutSec)
	check("UpstreamHeaderTimeout", src.UpstreamHeaderTimeout, got.UpstreamHeaderTimeout)
	check("UpstreamIdleTimeout", src.UpstreamIdleTimeout, got.UpstreamIdleTimeout)
	check("PoolMaxInFlight", src.PoolMaxInFlight, got.PoolMaxInFlight)
	check("BreakerThreshold", src.BreakerThreshold, got.BreakerThreshold)
	check("BreakerCooldown", src.BreakerCooldown, got.BreakerCooldown)
	check("BreakerCooldownMax", src.BreakerCooldownMax, got.BreakerCooldownMax)
	check("ReviveProbeHours", src.ReviveProbeHours, got.ReviveProbeHours)
	check("SanitizeFingerprints", src.SanitizeFingerprints, got.SanitizeFingerprints)
	check("UpstashURL", src.UpstashURL, got.UpstashURL)
	check("SessionStickyEnabled", src.SessionStickyEnabled, got.SessionStickyEnabled)
	check("SessionStickyTTL", src.SessionStickyTTL, got.SessionStickyTTL)
	check("CheckinEnabled", src.CheckinEnabled, got.CheckinEnabled)
	check("KeepaliveEnabled", src.KeepaliveEnabled, got.KeepaliveEnabled)
	check("CheckinHours", src.CheckinHours, got.CheckinHours)
	check("KeepaliveHours", src.KeepaliveHours, got.KeepaliveHours)
}

// TestPatchSingleKeyKeepsOtherKeysInFile F1 的**文件级**复现：补丁只提一个键时，
// 其余键必须逐个不变（不只是内存里的 admin.Settings 不变，config.json 也不能变）。
//
// 与 TestParseKeepsUnmentionedKeys 的分工：那条守 parse（内存），这条守 parse+persist
// 合起来之后的**盘上结果** —— F1 的破坏力正是在盘上体现的。
//
// 用例把 PATCH 前后的 JSON 都打出来（t.Logf），便于人工复核「改了一个键、其它原样」。
func TestPatchSingleKeyKeepsOtherKeysInFile(t *testing.T) {
	// 夹具刻意写**全**：只有全了才能证明「其余键逐个不变」，而不是「其余键恰好是 0」。
	const orig = `{
  "listen": "127.0.0.1:7863",
  "api_key": "MUST-SURVIVE",
  "schedule": { "checkin_enabled": true, "keepalive_enabled": true, "checkin_hours": [7,19], "keepalive_hours": [23] },
  "cooldown": { "soft_rate": "60s" },
  "upstream": { "timeout_seconds": 120, "header_timeout_seconds": 30, "idle_timeout_seconds": 90 },
  "pool": { "max_in_flight": 8, "breaker_threshold": 5, "breaker_cooldown": "5m", "breaker_cooldown_max": "30m", "revive_probe_interval_seconds": 3600 },
  "features": { "sanitize_blacklist_fingerprints": true },
  "upstash": { "url": "https://upstash.example" },
  "session_sticky": { "enabled": true, "ttl": "24h" },
  "admin": { "checkin_log_keep_days": 0 },
  "request_log_keep_days": 14
}
`
	cfg := &Config{}
	cfg.Schedule.CheckinEnabled = true
	cfg.Schedule.KeepaliveEnabled = true
	cfg.Schedule.CheckinHours = []int{7, 19}
	cfg.Schedule.KeepaliveHours = []int{23}
	cfg.Cooldown.SoftRate = "60s"
	cfg.Upstream.TimeoutSeconds = 120
	cfg.Upstream.HeaderTimeoutSeconds = 30
	cfg.Upstream.IdleTimeoutSeconds = 90
	cfg.Pool.MaxInFlight = 8
	cfg.Pool.BreakerThreshold = 5
	cfg.Pool.BreakerCooldown = "5m"
	cfg.Pool.BreakerCooldownMax = "30m"
	cfg.Pool.ReviveProbeIntervalSeconds = 3600
	cfg.Features.SanitizeBlacklistFingerprints = true
	cfg.Upstash.URL = "https://upstash.example"
	cfg.SessionSticky.Enabled = true
	cfg.SessionSticky.TTL = "24h"
	cfg.RequestLogKeepDays = 14
	// 注意：checkin_log_keep_days 的基底来自 s.log（本用例不装配它 ⇒ 0），
	// 所以夹具里那一项也写 0 —— 见 currentSettings 的 nil 判空注释。

	s, path := persistFixture(t, orig)
	s.cfg = cfg

	p, err := s.parse(json.RawMessage(`{"checkin_enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.persist(p); err != nil {
		t.Fatalf("persist 不该失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	t.Logf("PATCH 前 config.json:\n%s", orig)
	t.Logf("PATCH 后 config.json:\n%s", out)

	// 用户改的那一项生效
	if !strings.Contains(out, `"checkin_enabled": false`) {
		t.Error("被 PATCH 的 checkin_enabled 没有变成 false")
	}
	// 其余键逐个不变（F1 事故形态：timeout 120 / max_in_flight 8 / soft_rate 60s / sticky true 会被清零）
	for _, must := range []string{
		`"keepalive_enabled": true`,
		`"checkin_hours": [7,19]`,
		`"keepalive_hours": [23]`,
		`"soft_rate": "60s"`,
		`"timeout_seconds": 120`,
		`"header_timeout_seconds": 30`,
		`"idle_timeout_seconds": 90`,
		`"max_in_flight": 8`,
		`"breaker_threshold": 5`,
		`"breaker_cooldown": "5m"`,
		`"breaker_cooldown_max": "30m"`,
		`"revive_probe_interval_seconds": 3600`,
		`"sanitize_blacklist_fingerprints": true`,
		`"url": "https://upstash.example"`,
		`"ttl": "24h"`,
		`"request_log_keep_days": 14`,
		`"api_key": "MUST-SURVIVE"`,
	} {
		if !strings.Contains(out, must) {
			t.Errorf("★ F1 复发：未提交的键被冲掉了（%s）\nPATCH 后文件:\n%s", must, out)
		}
	}
	if !strings.Contains(out, `"listen": "127.0.0.1:7863"`) {
		t.Error("陌生键 listen 被冲掉了")
	}
}

// TestSettingsMirrorIntoOptionsStore options 表接线的端到端证据：
// ① 首次把 config.json 的可导入键导入 options（幂等：二次返回 0 条）；
// ② 保存一次（parse + persist + mirror）后，options 里被改的键变了、其余键一个都没变。
//
// 用**临时库**（t.TempDir 下的 gw.db），不碰真实 data/。
func TestSettingsMirrorIntoOptionsStore(t *testing.T) {
	const orig = `{
  "listen": "127.0.0.1:7863",
  "schedule": { "checkin_enabled": true, "keepalive_enabled": true, "checkin_hours": [7,19], "keepalive_hours": [23] },
  "cooldown": { "soft_rate": "60s" },
  "upstream": { "timeout_seconds": 120, "header_timeout_seconds": 30, "idle_timeout_seconds": 90 },
  "pool": { "max_in_flight": 8, "breaker_threshold": 5, "breaker_cooldown": "5m", "breaker_cooldown_max": "30m", "revive_probe_interval_seconds": 3600 },
  "features": { "sanitize_blacklist_fingerprints": true },
  "upstash": { "url": "https://upstash.example" },
  "session_sticky": { "enabled": true, "ttl": "24h" },
  "admin": { "checkin_log_keep_days": 0 },
  "request_log_keep_days": 14
}
`
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	// store 的 DB 是**包级句柄**：本用例独占它，并在收尾时关掉。
	if err := store.Open(filepath.Join(dir, "gw.db")); err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	cfg := &Config{}
	cfg.Schedule.CheckinEnabled = true
	cfg.Schedule.KeepaliveEnabled = true
	cfg.Schedule.CheckinHours = []int{7, 19}
	cfg.Schedule.KeepaliveHours = []int{23}
	cfg.Cooldown.SoftRate = "60s"
	cfg.Upstream.TimeoutSeconds = 120
	cfg.Upstream.HeaderTimeoutSeconds = 30
	cfg.Upstream.IdleTimeoutSeconds = 90
	cfg.Pool.MaxInFlight = 8
	cfg.Pool.BreakerThreshold = 5
	cfg.Pool.BreakerCooldown = "5m"
	cfg.Pool.BreakerCooldownMax = "30m"
	cfg.Pool.ReviveProbeIntervalSeconds = 3600
	cfg.Features.SanitizeBlacklistFingerprints = true
	cfg.Upstash.URL = "https://upstash.example"
	cfg.SessionSticky.Enabled = true
	cfg.SessionSticky.TTL = "24h"
	cfg.RequestLogKeepDays = 14
	s := &settingsStore{path: cfgPath, cfg: cfg}

	// ① 首次导入 + 幂等性
	s.seedOptionsFromConfig()
	before, err := store.LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.ImportFromConfigJSON(cfgPath); err != nil || n != 0 {
		t.Fatalf("二次导入应新增 0 条，得到 n=%d err=%v", n, err)
	}

	// ② 只改一个键
	p, err := s.parse(json.RawMessage(`{"checkin_enabled":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.persist(p); err != nil {
		t.Fatal(err)
	}
	s.mirrorOptions(p)

	after, err := store.LoadOptions()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("options 首次导入后 = %v", before)
	t.Logf("options 保存一次后 = %v", after)

	// 被 PATCH 的那个键：导入时是 true，保存后变 false
	if before["schedule.checkin_enabled"] != "true" {
		t.Errorf("首次导入时 schedule.checkin_enabled 应为 true，得到 %q", before["schedule.checkin_enabled"])
	}
	if after["schedule.checkin_enabled"] != "false" {
		t.Errorf("保存后 schedule.checkin_enabled 应为 false，得到 %q", after["schedule.checkin_enabled"])
	}

	// 其余受管键：**导入值与保存后值必须逐键相同**（这就是「PATCH 一个键不冲掉其它键」）
	// 期望值同时钉住字符串形态：数组 = 紧凑 JSON、布尔 = true/false、整数 = 十进制。
	unchanged := map[string]string{
		"schedule.keepalive_enabled":               "true",
		"schedule.checkin_hours":                   "[7,19]",
		"schedule.keepalive_hours":                 "[23]",
		"cooldown.soft_rate":                       "60s",
		"upstream.timeout_seconds":                 "120",
		"upstream.header_timeout_seconds":          "30",
		"upstream.idle_timeout_seconds":            "90",
		"pool.max_in_flight":                       "8",
		"pool.breaker_threshold":                   "5",
		"pool.breaker_cooldown":                    "5m",
		"pool.breaker_cooldown_max":                "30m",
		"pool.revive_probe_interval_seconds":       "3600",
		"features.sanitize_blacklist_fingerprints": "true",
		"upstash.url":                              "https://upstash.example",
		"session_sticky.enabled":                   "true",
		"session_sticky.ttl":                       "24h",
		"admin.checkin_log_keep_days":              "0", // s.log 未装配 ⇒ 基底 0（见 currentSettings）
		"request_log_keep_days":                    "14",
	}
	for k, v := range unchanged {
		if before[k] != v {
			t.Errorf("导入后 options[%q] = %q，期望 %q", k, before[k], v)
		}
		if after[k] != v {
			t.Errorf("★ 保存后 options[%q] = %q，期望 %q（未提交的键被冲掉了）", k, after[k], v)
		}
	}
	// 非受管键（seed 导入进来的）不该被镜像动过
	if after["listen"] != "127.0.0.1:7863" {
		t.Errorf("options[listen] 被改动了: %q", after["listen"])
	}
}
