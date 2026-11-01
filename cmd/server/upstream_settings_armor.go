// upstream_settings_armor.go 破甲注入的设置项：接进设置页与宿主持久化。
//
// # 为什么破甲要能被设置页开关（用户要求）
//
// 「当然这个也可以在前端设置一个开关，是否要破甲，是否要注入什么的」。
// 破甲会改变**每一条出站请求**的内容，所以它必须是显式可选、且改完立即生效的：
// 用户点一下就该看到差别，而不是"改了配置重启后不知道有没有生效"。
//
// # 键名
//
//	prompt_armor_mode   off / append / replace
//
// 与 config.json 的 `prompt.armor_mode` 同义（设置页写的是**首发位置**——
// cmd/server 的 settingsStore 会把上游段的值落在 config.json 的对应段/键上，
// 与 monkeycode 的出口那两个键同一条链路）。
//
// # 热生效
//
// ApplySettings 直接改 `*prompt.ArmorState` —— 出站客户端每次请求读它，
// 所以不需要重启（ArmorState 自带读写锁）。
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/prompt"
)

// 设置页的键名（对外的 JSON 名）。
const keyArmorMode = "prompt_armor_mode"

// keyArmorRetry 命中拒绝后是否自动重试一次（默认关，与原项目一致）。
const keyArmorRetry = "prompt_armor_retry"

// armorSettingsAdapter 见文件头。
type armorSettingsAdapter struct {
	st  *prompt.ArmorState
	cfg *Config
}

func newArmorSettingsAdapter(st *prompt.ArmorState, cfg *Config) *armorSettingsAdapter {
	return &armorSettingsAdapter{st: st, cfg: cfg}
}

// ---- admin.SettingsExt ----

// Fields 当前模式。
//
// RestartRequired=false：热生效（见文件头）。前端据此显示「立即生效」徽章 ——
// 若这里写成 true，界面会告诉用户"要重启"，而实际上不用。
func (a *armorSettingsAdapter) Fields() []admin.SettingField {
	mode := prompt.ArmorOff
	if a != nil && a.st != nil {
		if m := strings.TrimSpace(a.st.Mode()); m != "" {
			mode = m
		}
	}
	retry := false
	if a != nil && a.st != nil {
		retry = a.st.RetryEnabled()
	}
	return []admin.SettingField{
		{Key: keyArmorMode, Value: mode, RestartRequired: false},
		{Key: keyArmorRetry, Value: retry, RestartRequired: false},
	}
}

// ApplySettings 应用补丁里属于破甲的键（不认得的忽略 —— 整份补丁会发给每个上游）。
func (a *armorSettingsAdapter) ApplySettings(patch json.RawMessage) ([]string, error) {
	if a == nil || a.st == nil {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(patch, &m); err != nil {
		return nil, fmt.Errorf("设置解析失败: %w", err)
	}
	applied := make([]string, 0, 2)
	retryRaw, hasRetry := m[keyArmorRetry]
	if hasRetry {
		var b bool
		if err := json.Unmarshal(retryRaw, &b); err != nil {
			return nil, fmt.Errorf("%s 需要是布尔值", keyArmorRetry)
		}
		cur := a.st.Config()
		cur.RetryOnRefusal = b
		a.st.Set(cur)
		applied = append(applied, keyArmorRetry)
	}
	raw, ok := m[keyArmorMode]
	if !ok {
		return applied, nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("%s 需要是字符串（off / append / replace）", keyArmorMode)
	}
	mode := strings.ToLower(strings.TrimSpace(v))
	switch mode {
	case prompt.ArmorOff, prompt.ArmorAppend, prompt.ArmorReplace:
	case "":
		mode = prompt.ArmorOff
	default:
		// 非法值**报错**而不是静默当 off：用户填错时"开关看起来打开了但没生效"
		// 是最难查的一种（与 prompt.mode 的那条纪律同源）。
		return nil, fmt.Errorf("%s=%q 非法：只接受 %q / %q / %q",
			keyArmorMode, v, prompt.ArmorOff, prompt.ArmorAppend, prompt.ArmorReplace)
	}
	cur := a.st.Config()
	cur.Mode = mode
	a.st.Set(cur) // 热生效：出站客户端下一次请求就读到新值
	return append(applied, keyArmorMode), nil
}

// ---- main.UpstreamSettings ----

// Values 全量值（落盘用；补丁是稀疏的，只写补丁里的键会把别的键抹掉）。
func (a *armorSettingsAdapter) Values() map[string]any {
	out := map[string]any{}
	for _, f := range a.Fields() {
		out[f.Key] = f.Value
	}
	return out
}

// applyToMemory 把设置同步进内存 cfg（供 Snapshot 回显 + 下次落盘）。
func (a *armorSettingsAdapter) applyToMemory(values map[string]any) {
	if a == nil || a.cfg == nil {
		return
	}
	if v, ok := values[keyArmorRetry].(bool); ok {
		a.cfg.PromptArmorRetry = v
		a.cfg.Prompt.ArmorRetry = v
		a.cfg.Admin.PromptArmorRetry = v
	}
	if v, ok := values[keyArmorMode].(string); ok {
		// 三处一起写（与 monkeycode 出口适配器同一纪律）：
		//   Admin.PromptArmorMode —— managed 位置（下次落盘写进 config.json 的 admin 段）
		//   PromptArmorMode       —— 解析后的运行时值（启动日志与装配读它）
		//   Prompt.ArmorMode      —— 嵌套镜像（兼容手写 prompt.armor_mode 的配置与文档）
		//
		// ⚠ 只写后两处就是实测到的那个 bug：设置页显示 append、落盘也对，重启却回 off ——
		//   因为启动读的是 admin 段（设置页的 managed 位置），而它没被写。
		a.cfg.Admin.PromptArmorMode = v
		a.cfg.PromptArmorMode = v
		a.cfg.Prompt.ArmorMode = v
	}
}

var (
	_ admin.SettingsExt = (*armorSettingsAdapter)(nil)
	_ UpstreamSettings  = (*armorSettingsAdapter)(nil)
)
