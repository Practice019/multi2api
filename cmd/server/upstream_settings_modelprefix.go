// upstream_settings_modelprefix.go 设置项 `strict_model_prefix`：模型名必须带上游前缀。
//
// # 它是什么（用户要求）
//
// 「调用的模型必须有上游的前缀」。打开后，**裸模型名一律 400**；
// 显式配置过的别名/映射仍放行（见 internal/server/model_check.go 的放行分支）。
//
// 默认 **关**：既有客户端可能就在用裸名（`auto` 这类），不能把用户的线上调用直接打死。
//
// # 键名与存储位置（照本仓"两处存储"纪律）
//
//	strict_model_prefix   设置页写的位置（落进 config.json 的 admin 段）
//	features.strict_model_prefix   手写配置的位置（admin 段优先，这里作回退）
//
// 只在其中一处读写就是本仓踩过两次的那个 bug：设置页显示已开启、落盘也对、
// 重启后回到 off。所以 applyToMemory 三处一起写（admin / 解析值 / features 镜像）。
//
// # 热生效
//
// ApplySettings 直接改 *server.ModelPrefixPolicy，而请求路径（model_check.go）
// 每次读它 ⇒ 不需要重启（RestartRequired=false，前端据此显示「立即生效」）。
package main

import (
	"encoding/json"
	"fmt"

	"workbuddy2api/internal/admin"
	"workbuddy2api/internal/server"
)

// keyStrictModelPrefix 设置页的键名（对外的 JSON 名）。
const keyStrictModelPrefix = "strict_model_prefix"

// modelPrefixSettingsAdapter 见文件头。
type modelPrefixSettingsAdapter struct {
	st  *server.ModelPrefixPolicy
	cfg *Config
}

func newModelPrefixSettingsAdapter(st *server.ModelPrefixPolicy, cfg *Config) *modelPrefixSettingsAdapter {
	return &modelPrefixSettingsAdapter{st: st, cfg: cfg}
}

// ---- admin.SettingsExt ----

// Fields 当前值。RestartRequired=false：热生效（见文件头）。
func (a *modelPrefixSettingsAdapter) Fields() []admin.SettingField {
	strict := false
	if a != nil && a.st != nil {
		strict = a.st.Strict()
	}
	return []admin.SettingField{{Key: keyStrictModelPrefix, Value: strict, RestartRequired: false}}
}

// ApplySettings 只认这一个键（整份补丁会发给每个适配器，不认得的忽略）。
func (a *modelPrefixSettingsAdapter) ApplySettings(patch json.RawMessage) ([]string, error) {
	if a == nil || a.st == nil {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(patch, &m); err != nil {
		return nil, fmt.Errorf("设置解析失败: %w", err)
	}
	raw, ok := m[keyStrictModelPrefix]
	if !ok {
		return nil, nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		// 报错而不是静默按 false："填错了但看起来生效"是最难查的一种。
		return nil, fmt.Errorf("%s 需要是布尔值", keyStrictModelPrefix)
	}
	a.st.SetStrict(v) // 热生效：下一次请求即生效
	return []string{keyStrictModelPrefix}, nil
}

// ---- main.UpstreamSettings ----

// Values 全量值（落盘用；补丁是稀疏的，只写补丁里的键会把别的键抹掉）。
func (a *modelPrefixSettingsAdapter) Values() map[string]any {
	out := map[string]any{}
	for _, f := range a.Fields() {
		out[f.Key] = f.Value
	}
	return out
}

// applyToMemory 三处一起写（见文件头的"两处存储"说明）。
func (a *modelPrefixSettingsAdapter) applyToMemory(values map[string]any) {
	if a == nil || a.cfg == nil {
		return
	}
	v, ok := values[keyStrictModelPrefix].(bool)
	if !ok {
		return
	}
	// 取地址时要一份**独立的**副本：下面三处共享同一个指针是故意的
	//（它们表示同一个值），但不能指向循环/参数里的临时变量以外的东西。
	b := v
	a.cfg.Admin.StrictModelPrefix = &b
	a.cfg.StrictModelPrefix = v
	a.cfg.Features.StrictModelPrefix = &b
}

var (
	_ admin.SettingsExt = (*modelPrefixSettingsAdapter)(nil)
	_ UpstreamSettings  = (*modelPrefixSettingsAdapter)(nil)
)
