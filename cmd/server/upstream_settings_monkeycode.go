// upstream_settings_monkeycode.go 把 monkeycode 的设置项接进设置页与宿主持久化。
//
// # 为什么需要这个适配器（与 workbuddy 那个同一条理由）
//
// 上游包只提供一份 Fields()/ApplySettings()（键 + 值 + 是否需重启），
// 而两侧要的形状不同：
//
//	admin.SettingsExt      —— 让设置页显示与写入（需要分类：是否需重启）
//	main.UpstreamSettings  —— 让宿主落盘（只需要 键 → 当前值）
//
// 一个适配器同时满足两者，比在上游包里实现两套更省。
//
// # 本次要解决的报障
//
// 用户：「这个代理的配置是写死的吧？设置里面都设置不了。」
// 事实：config.json 里**一直有** monkeycode.proxy，但设置页没有入口、且改了要重启。
// 现在它在设置页里可改、改完**立刻生效**（See monkeycode.ApplySettings：换 transport）。
package main

import (
	"encoding/json"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/monkeycode"
)

// monkeycodeSettingsAdapter 见文件头。
type monkeycodeSettingsAdapter struct {
	p   *monkeycode.Provider
	cfg *Config
}

func newMonkeycodeSettingsAdapter(p *monkeycode.Provider, cfg *Config) *monkeycodeSettingsAdapter {
	return &monkeycodeSettingsAdapter{p: p, cfg: cfg}
}

// ---- admin.SettingsExt ----

func (a *monkeycodeSettingsAdapter) Fields() []admin.SettingField {
	if a.p == nil {
		return nil
	}
	src := a.p.SettingsFields()
	out := make([]admin.SettingField, 0, len(src))
	for _, f := range src {
		out = append(out, admin.SettingField{Key: f.Key, Value: f.Value, RestartRequired: f.RestartRequired})
	}
	return out
}

func (a *monkeycodeSettingsAdapter) ApplySettings(patch json.RawMessage) ([]string, error) {
	if a.p == nil {
		return nil, nil
	}
	return a.p.ApplySettings(patch)
}

// ---- main.UpstreamSettings ----

// Values 本上游全部设置项的当前值（用于持久化与回显）。
//
// ⚠ 必须是**全量**：补丁是稀疏的，只写补丁里的键会把另一个键从 config.json 里抹掉
// （这条与 workbuddy 适配器同一条理由，是本仓踩过的 F1）。
func (a *monkeycodeSettingsAdapter) Values() map[string]any {
	out := map[string]any{}
	if a.p == nil {
		return out
	}
	for _, f := range a.p.SettingsFields() {
		out[f.Key] = f.Value
	}
	return out
}

// applyToMemory 把刚保存的上游设置同步进内存 cfg（供 Snapshot 回显 + 下次落盘）。
//
// ⚠ 这一步不能省：落盘读的是 config.json 的那份结构（s.cfg），
//
//	不同步的话会出现「设置页显示新值、config.json 里还是旧值」，重启后回退。
//	上游的 ApplySettings 只改了它自己的 client（这是对的：上游不碰配置文件格式）。
func (a *monkeycodeSettingsAdapter) applyToMemory(values map[string]any) {
	if a.cfg == nil {
		return
	}
	if v, ok := values[monkeycode.KeyProxy].(string); ok {
		// ⚠ 三处一起写：
		//   Admin.MonkeyCodeProxy  —— **managed 位置**（下次落盘写进 config.json 的 admin 段）
		//   MonkeyCodeProxy        —— 解析后的运行时值（日志/自检读它）
		//   MonkeyCode.Proxy       —— 嵌套镜像（兼容"只在 monkeycode 段手写"的老配置与文档）
		// 只写其中一处就是实测过的那种半生效：设置页显示对了，重启后读的是另一个位置。
		a.cfg.Admin.MonkeyCodeProxy = v
		a.cfg.MonkeyCodeProxy = v
		a.cfg.MonkeyCode.Proxy = v
	}
	if v, ok := values[monkeycode.KeyDirectProxy].(string); ok {
		a.cfg.Admin.MonkeyCodeDirectProxy = v
		a.cfg.MonkeyCodeDirectProxy = v
		a.cfg.MonkeyCode.DirectProxy = v
	}
}

var (
	_ admin.SettingsExt = (*monkeycodeSettingsAdapter)(nil)
	_ UpstreamSettings  = (*monkeycodeSettingsAdapter)(nil)
)
