// settings_test.go workbuddy 设置项的契约（Task 3c 搬迁）。
//
// # 这组测试为什么在这里
//
// 改造前这九个开关是 internal/admin/settings.go 里写死的字段，
// 由 cmd/server 的 settingsStore 逐行读写。搬进上游包之后有两件事必须钉住：
//
//  1. **键名不能变** —— 它们是前端读写的 JSON 键，也是 config.json 里的键。
//     改名会让已保存的配置静默失效（用户改了配置但重启后没生效）。
//  2. **语义不能变** —— 六个成长开关 + 旅行自动领奖必须是"运行时生效"、
//     两个守卫轮间隔必须是"需重启"。分类错了界面会给出错误的提示。
package workbuddy

import (
	"encoding/json"
	"testing"
)

// TestSettingsFieldKeysAreStable 键名是**对外契约**，逐字钉死。
//
// 为什么这值得一条测试：这些字符串同时出现在三个地方
// （前端 localStorage / HTTP 请求体 / config.json），任何一处改了而别处没改
// 都会表现成"界面上是开的、实际是关的"这种最难查的偏差。
func TestSettingsFieldKeysAreStable(t *testing.T) {
	p := NewWithConfig(Config{})
	fields := p.SettingsFields()

	want := []string{
		"travel_auto_claim",
		"growth_auto_claim",
		"growth_auto_accept_tasks",
		"growth_auto_makeup",
		"growth_auto_redeem",
		"growth_auto_open",
		"growth_auto_draw",
		"travel_watch_interval_seconds",
		"growth_watch_interval_seconds",
	}
	if len(fields) != len(want) {
		t.Fatalf("设置项=%d，期望 %d（与改造前 admin.Settings 的字段一一对应）",
			len(fields), len(want))
	}
	for i, w := range want {
		if fields[i].Key != w {
			t.Errorf("第 %d 项的键=%q，期望 %q（顺序也要稳定，前端按序渲染）",
				i, fields[i].Key, w)
		}
	}
}

// TestSettingsRestartClassification 分类错了界面会给出错误提示。
//
// 六个自动动作改完立即生效（Provider 内存里的 bool，下一轮守卫即按新值走）；
// 两个守卫轮间隔在调度器注册 Job 时就被读走了，运行中改不了 —— 必须如实归入需重启。
func TestSettingsRestartClassification(t *testing.T) {
	p := NewWithConfig(Config{})
	for _, f := range p.SettingsFields() {
		wantRestart := f.Key == "travel_watch_interval_seconds" ||
			f.Key == "growth_watch_interval_seconds"
		if f.RestartRequired != wantRestart {
			t.Errorf("%s: RestartRequired=%v，期望 %v —— "+
				"把需重启的说成运行时生效，用户会以为改了但实际没变",
				f.Key, f.RestartRequired, wantRestart)
		}
	}
}

// TestSettingsValuesReflectToggles Fields() 返回的值必须是**当前**状态。
//
// 这条防的是"回显写死"：如果实现返回常量，前端保存后刷新会看到旧值。
func TestSettingsValuesReflectToggles(t *testing.T) {
	p := NewWithConfig(Config{})
	p.SetGrowthToggles(true, false, true, false, true, false)
	p.SetTravelAutoClaim(true)

	got := map[string]any{}
	for _, f := range p.SettingsFields() {
		got[f.Key] = f.Value
	}

	want := map[string]bool{
		"growth_auto_accept_tasks": true,
		"growth_auto_makeup":       false,
		"growth_auto_redeem":       true,
		"growth_auto_open":         false,
		"growth_auto_draw":         true,
		"growth_auto_claim":        false,
		"travel_auto_claim":        true,
	}
	for k, w := range want {
		v, ok := got[k].(bool)
		if !ok {
			t.Errorf("%s 的值不是 bool（%T）—— 前端把它当开关渲染", k, got[k])
			continue
		}
		if v != w {
			t.Errorf("%s=%v，期望 %v（回显必须反映当前状态）", k, v, w)
		}
	}
}

// TestApplySettingsUpdatesRuntimeToggles 补丁里的成长开关必须**真的**改到内存。
func TestApplySettingsUpdatesRuntimeToggles(t *testing.T) {
	p := NewWithConfig(Config{})

	// 先确认默认值（接单开、领奖开、补签开，其余三个关）
	accept, makeup, redeem, open, draw, claim := p.GrowthToggles()
	if !accept || !makeup || !claim || redeem || open || draw {
		t.Fatalf("默认开关不对: accept=%v makeup=%v redeem=%v open=%v draw=%v claim=%v",
			accept, makeup, redeem, open, draw, claim)
	}

	applied, err := p.ApplySettings(json.RawMessage(`{
		"growth_auto_accept_tasks": false,
		"growth_auto_open": true,
		"travel_auto_claim": true
	}`))
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	accept, makeup, redeem, open, draw, claim = p.GrowthToggles()
	if accept {
		t.Error("growth_auto_accept_tasks=false 未生效")
	}
	if !open {
		t.Error("growth_auto_open=true 未生效")
	}
	// 未出现在补丁里的不能被清掉（SetGrowthToggles 是全量语义，实现必须先取当前值）
	if !makeup || !claim {
		t.Errorf("补丁里没提到的开关被清掉了: makeup=%v claim=%v "+
			"（必须先读当前值再覆盖，否则保存一次就把其它开关归零）", makeup, claim)
	}
	if redeem || draw {
		t.Errorf("补丁里没提到的开关被打开了: redeem=%v draw=%v", redeem, draw)
	}
	if !p.TravelAutoClaimEnabled() {
		t.Error("travel_auto_claim=true 未生效")
	}

	// applied 必须回报实际改到的键
	inApplied := map[string]bool{}
	for _, k := range applied {
		inApplied[k] = true
	}
	for _, k := range []string{"growth_auto_accept_tasks", "growth_auto_open", "travel_auto_claim"} {
		if !inApplied[k] {
			t.Errorf("applied 里缺少 %s（前端据此显示已生效）", k)
		}
	}
}

// TestApplySettingsIgnoresForeignKeys 补丁里属于核心的键必须被忽略而不是报错。
//
// ApplySettings 收到的是**整份补丁**（核心不负责切分），所以里头一定有
// checkin_hours 这类不属于本上游的键。忽略它们是正确的 ——
// 报错会让"改签到时间"这件事被上游挡住。
func TestApplySettingsIgnoresForeignKeys(t *testing.T) {
	p := NewWithConfig(Config{})
	applied, err := p.ApplySettings(json.RawMessage(`{
		"checkin_hours": [9, 21],
		"pool_max_in_flight": 3,
		"unknown_key": "whatever"
	}`))
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("不该回报任何键，实际 %v", applied)
	}
}

// TestApplySettingsReportsRestartKeys 需重启的两个间隔要出现在 applied 里。
//
// 它们**不由本包写入**（那是宿主对 config.json 的事），但必须回报 ——
// 否则前端会把"保存成功"读成"没有生效"，而后端其实已经照常落盘。
func TestApplySettingsReportsRestartKeys(t *testing.T) {
	p := NewWithConfig(Config{})
	applied, err := p.ApplySettings(json.RawMessage(`{
		"travel_watch_interval_seconds": 120,
		"growth_watch_interval_seconds": 600
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied=%v，期望两个需重启的键", applied)
	}
}

// TestApplySettingsEmptyPatch 空补丁是 no-op（不能 panic、不能改任何东西）。
func TestApplySettingsEmptyPatch(t *testing.T) {
	p := NewWithConfig(Config{})
	before := p.SettingsFields()
	if _, err := p.ApplySettings(nil); err != nil {
		t.Fatalf("空补丁不该报错: %v", err)
	}
	if _, err := p.ApplySettings(json.RawMessage(`{}`)); err != nil {
		t.Fatalf("空对象补丁不该报错: %v", err)
	}
	after := p.SettingsFields()
	if len(before) != len(after) {
		t.Fatal("空补丁改变了设置项数量")
	}
}

// TestSettingsNilProviderSafe 未构造的 Provider 不得 panic（测试与降级路径）。
func TestSettingsNilProviderSafe(t *testing.T) {
	var p *Provider
	if got := p.SettingsFields(); got != nil {
		t.Errorf("nil Provider 的 SettingsFields 应为 nil，得到 %v", got)
	}
	if _, err := p.ApplySettings(json.RawMessage(`{"growth_auto_open":true}`)); err != nil {
		t.Errorf("nil Provider 的 ApplySettings 不该报错: %v", err)
	}
}
