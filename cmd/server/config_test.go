package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/loomy"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 60 {
		t.Errorf("soft=%v", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

// TestRetiredTravelIntervalKeyIgnored 退役的 travel_interval_minutes 键按未知字段忽略，不报错。
func TestRetiredTravelIntervalKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("retired key should not fail load: %v", err)
	}
	if len(c.Schedule.CheckinHours) != 1 || c.Schedule.CheckinHours[0] != 9 {
		t.Errorf("checkin_hours=%v want [9]（同段其余键照常生效）", c.Schedule.CheckinHours)
	}
}

// TestScheduleEnabledByDefault 两个任务的 enabled 开关默认均为 true：
// 老 config 不写这两个键，行为必须与从前完全一致（照常 9/21 签到、22 保活）。
func TestScheduleEnabledByDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("enabled defaults want true/true, got %v/%v",
			c.Schedule.CheckinEnabled, c.Schedule.KeepaliveEnabled)
	}
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写小时数组）加载后仍是启用态。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("legacy config must stay enabled: %+v", c.Schedule)
	}
	if len(c.Schedule.CheckinHours) != 2 {
		t.Errorf("checkin_hours=%v", c.Schedule.CheckinHours)
	}
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled || c.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", c.Schedule)
	}
	// 小时数组仍回落默认值（禁用与默认值互不干扰：重新启用无需补配小时）。
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
		t.Errorf("checkin_hours=%v want default [9 21] even when disabled", c.Schedule.CheckinHours)
	}
	if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
		t.Errorf("keepalive_hours=%v want default [22] even when disabled", c.Schedule.KeepaliveHours)
	}
}

// TestScheduleDisableKeepsExplicitHours 禁用不擦除用户配置的小时（便于原样恢复）。
func TestScheduleDisableKeepsExplicitHours(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_hours":[10,14]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}
	if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 10 || c.Schedule.CheckinHours[1] != 14 {
		t.Errorf("explicit hours must be preserved: %v", c.Schedule.CheckinHours)
	}
}

// TestScheduleEmptyHoursFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyHoursFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_hours":null,"keepalive_hours":null}}`,
		"emptyarr": `{"schedule":{"checkin_hours":[],"keepalive_hours":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Schedule.CheckinHours) != 2 || c.Schedule.CheckinHours[0] != 9 || c.Schedule.CheckinHours[1] != 21 {
				t.Errorf("checkin_hours=%v want default [9 21]", c.Schedule.CheckinHours)
			}
			if len(c.Schedule.KeepaliveHours) != 1 || c.Schedule.KeepaliveHours[0] != 22 {
				t.Errorf("keepalive_hours=%v want default [22]", c.Schedule.KeepaliveHours)
			}
			if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
				t.Errorf("empty hours must not imply disabled: %+v", c.Schedule)
			}
		})
	}
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

// TestGrowthAutoClaimKeyAlias 领奖开关接受两个键名。
//
// 背景：accept 那个开关叫 growth_auto_accept_tasks（带 _tasks 后缀），
// 而领奖最初只认 growth_auto_claim。于是按一致性手写 growth_auto_claim_tasks
// 的人会被静默忽略——配置改了等于没改，而且因为默认值是 true，
// 表面上看不出任何异常（这正是线上发现的问题）。
// 两个键都必须生效，且显式 false 要能真的关掉它。
func TestGrowthAutoClaimKeyAlias(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"主键 true", `{"admin":{"growth_auto_claim":true}}`, true},
		{"主键 false", `{"admin":{"growth_auto_claim":false}}`, false},
		{"别名 true", `{"admin":{"growth_auto_claim_tasks":true}}`, true},
		{"别名 false", `{"admin":{"growth_auto_claim_tasks":false}}`, false},
		{"都没配时默认开", `{}`, true},
		// 主键优先：两个都写且冲突时，以规范键为准，不被别名悄悄翻掉。
		{"主键优先", `{"admin":{"growth_auto_claim":false,"growth_auto_claim_tasks":true}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(tc.body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.GrowthAutoClaim != tc.want {
				t.Errorf("GrowthAutoClaim=%v want %v (body=%s)", c.GrowthAutoClaim, tc.want, tc.body)
			}
		})
	}
}

// TestGrowthAutoClaimAliasIsNotSilentlyLost 反证：别名键必须被结构体真的解析到，
// 不能只是「不报错」。若哪天有人把别名字段删掉，这个测试会立刻红。
func TestGrowthAutoClaimAliasIsNotSilentlyLost(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"admin":{"growth_auto_claim_tasks":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.GrowthAutoClaimAlias == nil {
		t.Fatal("别名字段未被解析（growth_auto_claim_tasks 又被静默忽略了）")
	}
	if *c.Admin.GrowthAutoClaimAlias != false {
		t.Errorf("别名值解析错误: %v", *c.Admin.GrowthAutoClaimAlias)
	}
}

// TestGrowthAutoDefaults 钉死成长中心各自动动作的默认值。
//
// 为什么要单独守一条：默认值是"配置里不写"时的行为，最容易在改动中被悄悄翻掉，
// 而用户不会立刻察觉（不写配置的人占多数）。
//
// 接单的默认值尤其值得守：它从 false 改成 true 是因为实测发现
// not_accepted 只在「还没领到第一只 Buddy」的窄窗口存在，官方前端连接单按钮都不给；
// 默认关的实际后果是新账号那批任务连进度都不显示。
//
// 三个会消耗用户资产的动作（连登天数 / 能量 / 抽奖次数）必须保持默认关 ——
// 那是用户的东西，不该由默认值替他决定。
func TestGrowthAutoDefaults(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	// 空配置：全部走默认
	os.WriteFile(fp, []byte(`{}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}

	wantOn := map[string]bool{
		"领奖 GrowthAutoClaim":  c.GrowthAutoClaim,
		"接单 GrowthAutoAccept": c.GrowthAutoAccept,
		"补签 GrowthAutoMakeup": c.GrowthAutoMakeup,
	}
	for name, got := range wantOn {
		if !got {
			t.Errorf("%s 应为默认开，得到 false", name)
		}
	}
	wantOff := map[string]bool{
		"兑换 GrowthAutoRedeem": c.GrowthAutoRedeem,
		"开盲盒 GrowthAutoOpen":  c.GrowthAutoOpen,
		"抽奖 GrowthAutoDraw":   c.GrowthAutoDraw,
	}
	for name, got := range wantOff {
		if got {
			t.Errorf("%s 会消耗用户资产，应保持默认关，得到 true", name)
		}
	}

	// 显式关掉接单必须生效（默认开了，用户仍要能关）。
	fp2 := filepath.Join(dir, "c2.json")
	os.WriteFile(fp2, []byte(`{"admin":{"growth_auto_accept_tasks":false}}`), 0o600)
	c2, err := Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.GrowthAutoAccept {
		t.Error("显式 growth_auto_accept_tasks=false 未生效")
	}
}

// ---------------------------------------------------------------- codearts 段

// TestCodeartsAbsentSectionIsDisabled 守住**向后兼容**：老 config 里没有
// codearts 段时，行为必须与改造前逐字节一致 —— 不启用上游。
//
// 这是本次接入最容易踩的坑：如果"段缺席"被当成"用默认值启用"，
// 所有现有部署会在升级后突然多出一个上游（并可能因为扫不到凭证而报错）。
func TestCodeartsAbsentSectionIsDisabled(t *testing.T) {
	dir := t.TempDir()

	// 老 config 的真实形态：完全没有 codearts 键
	fp := filepath.Join(dir, "legacy.json")
	os.WriteFile(fp, []byte(`{"listen":":7863","auth_dir":"./auths"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CodeartsEnabled {
		t.Error("老配置（无 codearts 段）不应启用 CodeArts 上游")
	}

	// 空配置同理
	fp2 := filepath.Join(dir, "empty.json")
	os.WriteFile(fp2, []byte(`{}`), 0o600)
	c2, err := Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.CodeartsEnabled {
		t.Error("空配置不应启用 CodeArts 上游")
	}
	// Default() 也不得默认开启
	if d := Default(); d.Codearts.Enabled {
		t.Error("Default() 不应默认启用 CodeArts 上游")
	}
}

// TestAuthDirsArePerUpstreamSubdirs 每个上游的凭证目录是同名子目录。
//
// # 判据变了（原因值得记）
//
// 这条测试原来叫 `TestCodeartsAuthDirFallsBackToTopLevel`，断言
// "codearts 缺省复用顶层 auth_dir"。那个设计的理由是：
//
//	codearts.LoadDir 按 `codearts*.json` 通配，workbuddy 的是
//	`workbuddy-*.json`，前缀不同可以安全共存
//
// **理由本身没错，但实测暴露了两个问题**：
//
//  1. codearts 授权成功后凭证被写进 workbuddy 的目录（核心用的是
//     默认上游的 AuthDir）—— 用户实测报过
//     `凭证落盘失败: rename auths.tmp auths: Access is denied.`
//  2. "靠文件名前缀互相过滤"意味着前缀一旦不匹配（改名、换客户端），
//     对方的凭证会被自己的扫描器当成"无法解析"而**静默跳过**
//
// 改成按上游分子目录后，"哪个文件属于谁"由**位置**表达，
// 不再依赖文件名约定。
func TestAuthDirsArePerUpstreamSubdirs(t *testing.T) {
	dir := t.TempDir()

	// 只写顶层 auth_dir → 两个上游各自落到同名子目录
	fp := filepath.Join(dir, "a.json")
	os.WriteFile(fp, []byte(`{"auth_dir":"./myauths","codearts":{"enabled":true}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.CodeartsEnabled {
		t.Fatal("显式 enabled=true 未生效")
	}
	if want := filepath.Join("./myauths", "codearts"); c.CodeartsAuthDir != want {
		t.Errorf("codearts 的目录应是 %q，得到 %q", want, c.CodeartsAuthDir)
	}
	if want := filepath.Join("./myauths", "workbuddy"); c.AuthDir != want {
		t.Errorf("workbuddy 的目录应是 %q，得到 %q", want, c.AuthDir)
	}
	// AuthsBase 必须保留**父目录** —— 迁移期兼容扫描靠它同时看两处
	if c.AuthsBase != "./myauths" {
		t.Errorf("AuthsBase 应是父目录 ./myauths，得到 %q", c.AuthsBase)
	}

	// 显式配了就**原样用**（不再加子目录后缀 ——
	// 显式配置意味着"我知道凭证在哪"，加后缀会指到一个空目录）
	fp2 := filepath.Join(dir, "b.json")
	os.WriteFile(fp2, []byte(`{"auth_dir":"./myauths","codearts":{"enabled":true,"auth_dir":"./ca"}}`), 0o600)
	c2, err := Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.CodeartsAuthDir != "./ca" {
		t.Errorf("显式 codearts.auth_dir 未生效，得到 %q", c2.CodeartsAuthDir)
	}
}

// TestCodeartsRefreshIntervalDefaults 后台续期间隔的"未配置"与"显式关闭"必须分开。
//
// 混为一谈的后果：把"没配这一项"读成 0，再读成"关闭"，
// 于是用户明明没动过这一项，后台续期却悄悄不跑 —— 而 CodeArts 的 STS
// 只有约 2 小时寿命，表现是"空闲后第一个请求莫名慢几秒"。
func TestCodeartsRefreshIntervalDefaults(t *testing.T) {
	dir := t.TempDir()

	// 启用但未配间隔 → 默认 60s
	fp := filepath.Join(dir, "a.json")
	os.WriteFile(fp, []byte(`{"codearts":{"enabled":true}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.CodeartsRefreshInterval.Seconds() != 60 {
		t.Errorf("未配间隔应默认 60s，得到 %v", c.CodeartsRefreshInterval)
	}

	// 显式配 → 用配置值
	fp2 := filepath.Join(dir, "b.json")
	os.WriteFile(fp2, []byte(`{"codearts":{"enabled":true,"refresh_interval_seconds":15}}`), 0o600)
	c2, err := Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.CodeartsRefreshInterval.Seconds() != 15 {
		t.Errorf("显式间隔未生效，得到 %v", c2.CodeartsRefreshInterval)
	}

	// 未启用 → 即使配了间隔也不注册（不启动无用的后台任务）
	fp3 := filepath.Join(dir, "c.json")
	os.WriteFile(fp3, []byte(`{"codearts":{"enabled":false,"refresh_interval_seconds":15}}`), 0o600)
	c3, err := Load(fp3)
	if err != nil {
		t.Fatal(err)
	}
	if c3.CodeartsRefreshInterval != 0 {
		t.Errorf("未启用时不应解析出间隔，得到 %v", c3.CodeartsRefreshInterval)
	}
}

// TestLoomyAbsentSectionIsDisabled 守住**向后兼容**：老 config 里没有 loomy 段时
// 不得启用该上游。
//
// 与 codearts 那条同一条判据、同一个理由：现有部署的 config.json 里没有这一段，
// 若"段缺席"被当成"用默认值启用"，它们升级后会突然多出一个上游
// （而 loomy 的凭证目录通常是空的 —— 表现为"多了一个永远没号的空上游"）。
func TestLoomyAbsentSectionIsDisabled(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct{ name, body string }{
		{"老配置（完全没有 loomy 键）", `{"listen":":7863","auth_dir":"./auths"}`},
		{"空配置", `{}`},
		{"只配了 codearts（升级路径上最常见的形态）", `{"codearts":{"enabled":true}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(tc.body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if c.LoomyEnabled {
				t.Error("不应启用 loomy 上游（必须是显式 loomy.enabled=true）")
			}
			// 未启用时并池开关必须恒 false —— 不注册的上游不该在池里留痕迹
			if c.LoomyPoolAccounts {
				t.Error("未启用时 LoomyPoolAccounts 必须为 false")
			}
		})
	}
	if d := Default(); d.Loomy.Enabled {
		t.Error("Default() 不应默认启用 loomy 上游")
	}
}

// TestLoomyDefaults 未配的项要落到正确的默认值上。
//
// # 这里最容易错的一处（所以单独钉住）
//
// normalize 在解析 codearts 时会把 `c.AuthDir` **改写成 workbuddy 子目录**，
// 而保存原值的是 `c.AuthsBase`。loomy 的默认目录必须基于 **AuthsBase** 拼：
//
//	对：filepath.Join(c.AuthsBase, "loomy") → ./myauths/loomy
//	错：filepath.Join(c.AuthDir,  "loomy") → ./myauths/workbuddy/loomy
//
// 而错了**不会报错**：那个路径下确实能读能写（只要用户把凭证放那儿），
// 只是语义错乱成"一个上游的目录长在另一个上游里面"。
// 这种"能工作但错"的形态正是最该被测试钉住的一类。
func TestLoomyDefaults(t *testing.T) {
	dir := t.TempDir()

	fp := filepath.Join(dir, "a.json")
	os.WriteFile(fp, []byte(`{"auth_dir":"./myauths","loomy":{"enabled":true}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.LoomyEnabled {
		t.Fatal("显式 enabled=true 未生效")
	}
	if want := filepath.Join("./myauths", "loomy"); c.LoomyAuthDir != want {
		t.Errorf("loomy 的目录应是 %q，得到 %q\n"+
			"（若得到 ./myauths/workbuddy/loomy，说明用了 c.AuthDir 而不是 c.AuthsBase —— "+
			"功能上能跑，但语义错乱成「一个上游的目录长在另一个上游里面」）",
			want, c.LoomyAuthDir)
	}
	// 基址必须与包里的常量同源（不写第二份字面量）
	if c.LoomyBaseURL != loomy.DefaultBaseURL {
		t.Errorf("缺省基址应是 loomy.DefaultBaseURL=%q，得到 %q",
			loomy.DefaultBaseURL, c.LoomyBaseURL)
	}
	// 启用且未显式关 → 默认并入账号池
	if !c.LoomyPoolAccounts {
		t.Error("启用后默认应当并入账号池")
	}

	// 显式配了就原样用（不再拼子目录）：显式配置意味着"我知道凭证在哪"
	fp2 := filepath.Join(dir, "b.json")
	os.WriteFile(fp2, []byte(`{"auth_dir":"./myauths","loomy":{"enabled":true,"auth_dir":"./lm","base_url":"http://127.0.0.1:9/api/v1"}}`), 0o600)
	c2, err := Load(fp2)
	if err != nil {
		t.Fatal(err)
	}
	if c2.LoomyAuthDir != "./lm" {
		t.Errorf("显式 loomy.auth_dir 未生效，得到 %q", c2.LoomyAuthDir)
	}
	if c2.LoomyBaseURL != "http://127.0.0.1:9/api/v1" {
		t.Errorf("显式 loomy.base_url 未生效，得到 %q", c2.LoomyBaseURL)
	}

	// 显式 pool_accounts=false → 不并池（"只想用管理端点"的退出口）
	fp3 := filepath.Join(dir, "c.json")
	os.WriteFile(fp3, []byte(`{"loomy":{"enabled":true,"pool_accounts":false}}`), 0o600)
	c3, err := Load(fp3)
	if err != nil {
		t.Fatal(err)
	}
	if c3.LoomyPoolAccounts {
		t.Error("显式 pool_accounts=false 未生效")
	}
}

// TestLoomyClientDataDir 客户端数据目录：**留空就是留空**（= 自动探测）。
//
// # 为什么这里断言的是"空串"而不是"某个路径"
//
// 与上面三个 loomy 字段（auth_dir / base_url / pool_accounts）不同，
// 这一项**不做缺省填充**：填一个探测结果就等于把它变成"启动时刻的快照"，
// 于是"先起网关、之后才装并登录客户端"这种顺序必须重启网关才能用上
// 「添加账号」。留空让 loomy 包在**每次调用**时探测（一次 os.Stat）。
//
// 所以这条测试守的是一条**反直觉**的性质：别的字段都填缺省，它偏偏不填。
// 若有人"顺手补齐默认值"，这条会红 —— 而那个改动在功能上看起来无害。
func TestLoomyClientDataDir(t *testing.T) {
	dir := t.TempDir()

	t.Run("留空 → 原样空串（由 loomy 包自动探测）", func(t *testing.T) {
		fp := filepath.Join(dir, "empty.json")
		os.WriteFile(fp, []byte(`{"loomy":{"enabled":true}}`), 0o600)
		c, err := Load(fp)
		if err != nil {
			t.Fatal(err)
		}
		if c.LoomyClientDataDir != "" {
			t.Errorf("留空时必须**不做缺省填充**，得到 %q —— "+
				"填一个快照路径会让「先起网关、后装客户端」必须重启才能用上「添加账号」",
				c.LoomyClientDataDir)
		}
	})

	t.Run("显式配置 → 原样生效", func(t *testing.T) {
		fp := filepath.Join(dir, "explicit.json")
		os.WriteFile(fp, []byte(
			`{"loomy":{"enabled":true,"client_data_dir":"D:/loomy/Local Storage/leveldb"}}`),
			0o600)
		c, err := Load(fp)
		if err != nil {
			t.Fatal(err)
		}
		if c.LoomyClientDataDir != "D:/loomy/Local Storage/leveldb" {
			t.Errorf("显式 client_data_dir 未生效，得到 %q", c.LoomyClientDataDir)
		}
	})

	t.Run("只有空白 → 视同留空", func(t *testing.T) {
		fp := filepath.Join(dir, "blank.json")
		os.WriteFile(fp, []byte(`{"loomy":{"enabled":true,"client_data_dir":"   "}}`), 0o600)
		c, err := Load(fp)
		if err != nil {
			t.Fatal(err)
		}
		if c.LoomyClientDataDir != "" {
			t.Errorf("纯空白应当被裁成空串（否则会去 stat 一个叫「   」的目录），得到 %q",
				c.LoomyClientDataDir)
		}
	})

	t.Run("未启用时也不崩", func(t *testing.T) {
		fp := filepath.Join(dir, "disabled.json")
		os.WriteFile(fp, []byte(`{"loomy":{"client_data_dir":"/x"}}`), 0o600)
		c, err := Load(fp)
		if err != nil {
			t.Fatal(err)
		}
		if c.LoomyEnabled {
			t.Error("未写 enabled 时不该启用")
		}
	})
}
