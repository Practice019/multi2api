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
//	prompt_armor_mode   off / append / block（T-902 起界面恰好三档）
//
// ⚠ replace 是**已下线的旧值**：界面不再提供它，但旧配置/旧提交仍可能带着它 ——
//
//	读得到、不报错，归一成 append（见 ApplySettings 与 config.go normalize 的注释）。
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
	"log"
	"strings"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/prompt"
)

// 设置页的键名（对外的 JSON 名）。
const keyArmorMode = "prompt_armor_mode"

// keyArmorRetry 命中拒绝后是否自动重试一次（默认关，与原项目一致）。
const keyArmorRetry = "prompt_armor_retry"

// ---- 三态的范围/名单/观察/模式库（2026-11-13 用户拍板）----
//
// 键名与 config.json 的 prompt 段一一对应（设置页写的是**首发位置**：admin 段）。
// ⚠ 新增任何一个 prompt_armor_* 键，都必须在 config 的 normalize 里有一条解析赋值 ——
//
//	否则就是"设置页显示对了、重启静默回退"那一族（本仓已中三次），
//	且有源码级守卫 TestEveryArmorSettingKeyIsResolvedInNormalize 钉着。
const (
	keyArmorInjectScope   = "prompt_armor_inject_scope"
	keyArmorInjectTargets = "prompt_armor_inject_targets"
	keyArmorBlockScope    = "prompt_armor_block_scope"
	keyArmorBlockTargets  = "prompt_armor_block_targets"
	keyArmorObserve       = "prompt_armor_observe"
	keyArmorPatterns      = "prompt_armor_patterns"
	keyArmorPatternsOnly  = "prompt_armor_patterns_only"
)

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
	cfg := prompt.ArmorConfig{}
	if a != nil && a.st != nil {
		cfg = a.st.Config()
	}
	// 名单/模式库以**字符串**下发（设置页那边是输入框）：逗号+空格连接，
	// 与 splitArmorList 的分隔符口径一致（改一边必须改另一边）。
	return []admin.SettingField{
		{Key: keyArmorMode, Value: mode, RestartRequired: false},
		{Key: keyArmorRetry, Value: retry, RestartRequired: false},
		{Key: keyArmorInjectScope, Value: scopeOrDefault(cfg.InjectScope), RestartRequired: false},
		{Key: keyArmorInjectTargets, Value: strings.Join(cfg.InjectTargets, ", "), RestartRequired: false},
		{Key: keyArmorBlockScope, Value: scopeOrDefault(cfg.BlockScope), RestartRequired: false},
		{Key: keyArmorBlockTargets, Value: strings.Join(cfg.BlockTargets, ", "), RestartRequired: false},
		{Key: keyArmorObserve, Value: cfg.Observe, RestartRequired: false},
		{Key: keyArmorPatterns, Value: strings.Join(cfg.Patterns, ", "), RestartRequired: false},
		{Key: keyArmorPatternsOnly, Value: cfg.PatternsOnly, RestartRequired: false},
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
	// ---- 三态的范围/名单/观察/模式库 ----
	//
	// 逐键判"补丁里有没有"：设置页只发改动过的键，这里必须按缺省=不动处理
	// （用零值覆盖会把其它键清空 —— 那是"保存一次设置，别的设置没了"的经典形态）。
	cur := a.st.Config()
	changed := false
	strKeys := map[string]*string{
		keyArmorInjectScope:   &cur.InjectScope,
		keyArmorInjectTargets: nil, // 名单要拆分，单独处理（见下）
		keyArmorBlockScope:    &cur.BlockScope,
		keyArmorPatterns:      nil,
	}
	for k, dst := range strKeys {
		if dst == nil {
			continue
		}
		raw, ok := m[k]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s 需要是字符串（%s / %s / %s）", k, prompt.ArmorScopeAll, prompt.ArmorScopeInclude, prompt.ArmorScopeExclude)
		}
		v = strings.ToLower(strings.TrimSpace(v))
		switch v {
		case "", prompt.ArmorScopeAll, prompt.ArmorScopeInclude, prompt.ArmorScopeExclude:
		default:
			// 非法值**报错**，不静默当默认：范围写错时悄悄按 all 处理，
			// 用户会以为名单生效了 —— 而实际拦的是所有人。
			return nil, fmt.Errorf("%s=%q 非法：只接受 %q / %q / %q",
				k, v, prompt.ArmorScopeAll, prompt.ArmorScopeInclude, prompt.ArmorScopeExclude)
		}
		*dst = v
		applied = append(applied, k)
		changed = true
	}
	for _, item := range []struct {
		key string
		dst *[]string
	}{
		{keyArmorInjectTargets, &cur.InjectTargets},
		{keyArmorBlockTargets, &cur.BlockTargets},
		{keyArmorPatterns, &cur.Patterns},
	} {
		raw, ok := m[item.key]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s 需要是字符串（多个条目用逗号或换行分隔）", item.key)
		}
		*item.dst = splitArmorList(v)
		applied = append(applied, item.key)
		changed = true
	}
	for _, item := range []struct {
		key string
		dst *bool
	}{
		{keyArmorObserve, &cur.Observe},
		{keyArmorPatternsOnly, &cur.PatternsOnly},
	} {
		raw, ok := m[item.key]
		if !ok {
			continue
		}
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("%s 需要是布尔值", item.key)
		}
		*item.dst = b
		applied = append(applied, item.key)
		changed = true
	}
	if changed {
		a.st.Set(cur) // 热生效：出站路径每次请求读它
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
	case prompt.ArmorOff, prompt.ArmorAppend, prompt.ArmorBlock:
	case prompt.ArmorReplace:
		// ⚠ 旧值兼容（T-902）：界面已不再提供「替换」这一档（用户要求收窄成三档），
		//   但旧页面 / 旧脚本 / 旧提交里仍可能带着它 ⇒ 保存不报错、归一成 append，
		//   并留一条可读提示。与 config.go 的 normalize **必须同一口径**（两处不一致 =
		//   "设置页保存对了、重启又变回去"那一族的老病）。
		log.Printf("破甲：设置里提交了已下线的模式 %q（替换），已归一成 %q（追加）", prompt.ArmorReplace, prompt.ArmorAppend)
		mode = prompt.ArmorAppend
	case "":
		mode = prompt.ArmorOff
	default:
		// 非法值**报错**而不是静默当 off：用户填错时"开关看起来打开了但没生效"
		// 是最难查的一种（与 prompt.mode 的那条纪律同源）。
		// ⚠ 放宽 replace 兼容**不得**变成"接受任何字符串"（有断言钉着）。
		return nil, fmt.Errorf("%s=%q 非法：只接受 %q / %q / %q（%q 是已下线的旧值，会被归一成 %q）",
			keyArmorMode, v, prompt.ArmorOff, prompt.ArmorAppend, prompt.ArmorBlock,
			prompt.ArmorReplace, prompt.ArmorAppend)
	}
	cur = a.st.Config() // 上面那批键可能已经改过 cur，这里取最新再改 mode
	cur.Mode = mode
	a.st.Set(cur) // 热生效：出站客户端下一次请求就读到新值
	return append(applied, keyArmorMode), nil
}

// scopeOrDefault 空范围按 all 下发（界面上的默认选项就是它）。
func scopeOrDefault(v string) string {
	if s := strings.TrimSpace(v); s != "" {
		return s
	}
	return prompt.ArmorScopeAll
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
		// 三处一起写（与 mode 那条同一纪律）：
		//   PromptArmorRetry       —— 解析后的运行时值（启动日志与装配读它）
		//   Prompt.ArmorRetry      —— 嵌套镜像（兼容手写 prompt.armor_retry 的配置与文档）
		//   Admin.PromptArmorRetry —— managed 位置（下次落盘写进 config.json 的 admin 段）
		//
		// 注意：admin 那份是 *bool，这里取的是**副本**的地址（不能指向接口/临时值）；
		//   用户关掉开关时落的是一个显式 &false —— 那正是它能压过手写 true 的原因。
		val := v
		a.cfg.PromptArmorRetry = val
		a.cfg.Prompt.ArmorRetry = val
		a.cfg.Admin.PromptArmorRetry = &val
	}
	// ---- 三态的范围/名单/观察/模式库：三处一起写（与 mode/retry 同一纪律）----
	//
	// 只写其中一处就是实测到的那族 bug：设置页显示对了、重启回退。
	//   Admin.PromptArmor*  —— managed 位置（下次落盘写进 config.json 的 admin 段）
	//   cfg.Armor*          —— 运行时值（启动日志与装配读它）
	//   cfg.Prompt.Armor*   —— 嵌套镜像（兼容手写 prompt 段的配置与文档）
	if v, ok := values[keyArmorInjectScope].(string); ok {
		a.cfg.Admin.PromptArmorInjectScope = v
		a.cfg.PromptArmorInjectScope = v
		a.cfg.Prompt.ArmorInjectScope = v
	}
	if v, ok := values[keyArmorBlockScope].(string); ok {
		a.cfg.Admin.PromptArmorBlockScope = v
		a.cfg.PromptArmorBlockScope = v
		a.cfg.Prompt.ArmorBlockScope = v
	}
	if v, ok := values[keyArmorInjectTargets].(string); ok {
		a.cfg.Admin.PromptArmorInjectTargets = v
		a.cfg.PromptArmorInjectTargets = splitArmorList(v)
		a.cfg.Prompt.ArmorInjectTargets = v
	}
	if v, ok := values[keyArmorBlockTargets].(string); ok {
		a.cfg.Admin.PromptArmorBlockTargets = v
		a.cfg.PromptArmorBlockTargets = splitArmorList(v)
		a.cfg.Prompt.ArmorBlockTargets = v
	}
	if v, ok := values[keyArmorPatterns].(string); ok {
		a.cfg.Admin.PromptArmorPatterns = v
		a.cfg.PromptArmorPatterns = splitArmorList(v)
		a.cfg.Prompt.ArmorPatterns = v
	}
	if v, ok := values[keyArmorObserve].(bool); ok {
		// 指针：显式 false 也要能压过手写位置的 true（同 retry 那条注释）。
		val := v
		a.cfg.Admin.PromptArmorObserve = &val
		a.cfg.PromptArmorObserve = val
		a.cfg.Prompt.ArmorObserve = &val
	}
	if v, ok := values[keyArmorPatternsOnly].(bool); ok {
		val := v
		a.cfg.Admin.PromptArmorPatternsOnly = &val
		a.cfg.PromptArmorPatternsOnly = val
		a.cfg.Prompt.ArmorPatternsOnly = val
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
