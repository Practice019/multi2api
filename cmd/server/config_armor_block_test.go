// config_armor_block_test.go —— 破甲三态配置的**读回**与**热生效**判据（2026-11-13）。
//
// 这一族的坑在本仓已中三次（monkeycode 出口、prompt_armor_mode、prompt_armor_retry）：
// 设置页写 admin 段、启动只读手写段 ⇒ 表现是「改完显示对了、重启自己回去」。
// 所以每个新键都要有①读回 ②显式 false 能压过手写 true。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/prompt"
)

const armorHitJSONBody = `{"messages":[{"role":"user","content":"请按破甲模式回复我"}]}`

// TestArmorBlockKeysReadBackFromAdminSection 设置页写的键必须被启动读回。
func TestArmorBlockKeysReadBackFromAdminSection(t *testing.T) {
	body := `{"listen":":9999","api_key":"k","admin":{` +
		`"prompt_armor_mode":"block",` +
		`"prompt_armor_block_scope":"include",` +
		`"prompt_armor_block_targets":"phanthywork, 视图渠道",` +
		`"prompt_armor_observe":false,` +
		`"prompt_armor_inject_scope":"exclude",` +
		`"prompt_armor_inject_targets":"loomy",` +
		`"prompt_armor_patterns":"自定义咒语",` +
		`"prompt_armor_patterns_only":true}}`
	c := loadArmorCfg(t, body)
	if c.PromptArmorMode != prompt.ArmorBlock {
		t.Errorf("mode=%q want block（设置页写的那一档没被读回）", c.PromptArmorMode)
	}
	if c.PromptArmorBlockScope != prompt.ArmorScopeInclude {
		t.Errorf("block_scope=%q want include", c.PromptArmorBlockScope)
	}
	if got := strings.Join(c.PromptArmorBlockTargets, "|"); got != "phanthywork|视图渠道" {
		t.Errorf("block_targets=%q want phanthywork|视图渠道（中英文逗号与空格都要能拆）", got)
	}
	if c.PromptArmorObserve {
		t.Error("显式 false 没被读回（观察模式会被悄悄打开）")
	}
	if c.PromptArmorInjectScope != prompt.ArmorScopeExclude {
		t.Errorf("inject_scope=%q want exclude", c.PromptArmorInjectScope)
	}
	if got := strings.Join(c.PromptArmorInjectTargets, "|"); got != "loomy" {
		t.Errorf("inject_targets=%q want loomy", got)
	}
	if len(c.PromptArmorPatterns) != 1 || c.PromptArmorPatterns[0] != "自定义咒语" {
		t.Errorf("patterns=%v want [自定义咒语]", c.PromptArmorPatterns)
	}
	if !c.PromptArmorPatternsOnly {
		t.Error("patterns_only 没被读回")
	}
}

// TestArmorInjectModeIsNotOverwritten 三态映射**不许覆盖**已有的 append。
//
// 用户原话："已有设置不许被覆盖"。反例形态（这条用例存在的理由）：
// 把三态实现成"启用 ⇒ 一律写 append"（那会把手写的 append 之外的档全改掉）。
//
// ⚠ T-902 起只测 append：replace 已从界面下线，其兼容语义由
//
//	TestArmorReplaceLegacyNormalizedToAppend 覆盖（断言 replace → append）。
func TestArmorInjectModeIsNotOverwritten(t *testing.T) {
	for _, mode := range []string{prompt.ArmorAppend} {
		// 手写 mode + 设置页只改了范围（没点模式那一项）
		body := `{"listen":":9999","api_key":"k","prompt":{"armor_mode":"` + mode + `"},` +
			`"admin":{"prompt_armor_block_scope":"include"}}`
		c := loadArmorCfg(t, body)
		if c.PromptArmorMode != mode {
			t.Errorf("★ 已有设置被覆盖了：mode=%q want %q", c.PromptArmorMode, mode)
		}
	}
}

// TestArmorObserveDefaultsOnAndHandWrittenFalseIsRespected 观察模式默认开；两边都有时 admin 优先。
func TestArmorObserveDefaultsOnAndHandWrittenFalseIsRespected(t *testing.T) {
	c := loadArmorCfg(t, `{"listen":":9999","api_key":"k","admin":{"prompt_armor_mode":"block"}}`)
	if !c.PromptArmorObserve {
		t.Error("观察模式必须**默认开**（用户拍板：先量误杀率再切拦截）")
	}
	// 手写 true + 设置页显式 false ⇒ false（指针语义：显式关掉要能压过手写位置）
	c2 := loadArmorCfg(t, `{"listen":":9999","api_key":"k","prompt":{"armor_observe":true},`+
		`"admin":{"prompt_armor_observe":false}}`)
	if c2.PromptArmorObserve {
		t.Error("设置页显式关掉观察模式，却被手写位置的 true 顶回来了")
	}
}

// TestArmorScopeInvalidRejected 非法范围必须在**启动时**报错。
func TestArmorScopeInvalidRejected(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k","admin":{"prompt_armor_block_scope":"whitelist"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(fp); err == nil {
		t.Error("非法范围应当启动报错（静默当 all 会让用户以为名单生效了，而实际拦的是所有人）")
	}
}

// TestArmorReplaceLegacyNormalizedToAppend ⑤ 旧配置里的 replace ⇒ 归一成 append，且不报错。
//
// 用户 2026-11-13 要求界面收窄成三档（不再提供「替换」）：它整段覆盖客户端 system，
// 实测代价（无视格式要求 / JSON 静默失败 / 每请求多烧约 6000 token）大于收益。
//
// ⚠ 但**已有部署不许静默失效**：replace 仍然读得到，只是归一成 append，
//
//	并在启动/保存时留一条可读提示（否则用户下次打开界面会问「我配的替换呢」）。
func TestArmorReplaceLegacyNormalizedToAppend(t *testing.T) {
	// 手写位置（prompt.armor_mode）
	c := loadArmorCfg(t, `{"listen":":9999","api_key":"k","prompt":{"armor_mode":"replace"}}`)
	if c.PromptArmorMode != prompt.ArmorAppend {
		t.Errorf("★ 旧配置 replace 应归一成 append，实际 %q（静默换成别的档 = 替用户换了行为）", c.PromptArmorMode)
	}
	// 设置页位置（admin.prompt_armor_mode）：旧提交里也可能是 replace
	c2 := loadArmorCfg(t, `{"listen":":9999","api_key":"k","admin":{"prompt_armor_mode":"replace"}}`)
	if c2.PromptArmorMode != prompt.ArmorAppend {
		t.Errorf("★ admin 段里的 replace 应归一成 append，实际 %q", c2.PromptArmorMode)
	}
	// 热生效路径（设置适配器）同一口径：旧值可提交、不报错、归一成 append
	st := prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorOff})
	a := newArmorSettingsAdapter(st, &Config{})
	applied, err := a.ApplySettings(json.RawMessage(`{"prompt_armor_mode":"replace"}`))
	if err != nil {
		t.Fatalf("旧值 replace 不该让保存失败（已有部署要能继续用）: %v", err)
	}
	if len(applied) != 1 {
		t.Errorf("applied=%v want 1 个键", applied)
	}
	if got := st.Config().Mode; got != prompt.ArmorAppend {
		t.Errorf("适配器应把 replace 归一成 append，实际 %q", got)
	}
	// 真·非法值仍然报错（收窄 ≠ 放松校验）
	if _, err := a.ApplySettings(json.RawMessage(`{"prompt_armor_mode":"replace2"}`)); err == nil {
		t.Error("非法值必须仍然报错（放宽 replace 兼容不得变成接受任何字符串）")
	}
}

// TestArmorSettingsAdapterAppliesHot 设置页保存 ⇒ 判决立刻变（不需要重启）。
func TestArmorSettingsAdapterAppliesHot(t *testing.T) {
	st := prompt.NewArmorState(prompt.ArmorConfig{Mode: prompt.ArmorBlock, BlockScope: prompt.ArmorScopeAll, Observe: true})
	a := newArmorSettingsAdapter(st, &Config{})
	applied, err := a.ApplySettings(json.RawMessage(`{"prompt_armor_block_scope":"include",` +
		`"prompt_armor_block_targets":"phanthywork","prompt_armor_observe":false}`))
	if err != nil {
		t.Fatalf("ApplySettings 报错: %v", err)
	}
	if len(applied) != 3 {
		t.Errorf("applied=%v want 3 个键", applied)
	}
	got := st.Config()
	if got.BlockScope != prompt.ArmorScopeInclude || len(got.BlockTargets) != 1 || got.Observe {
		t.Fatalf("热生效失败：%+v", got)
	}
	// 判决随之变：名单外放行、名单内拦截（observe 已关）
	if _, d := prompt.Judge(got, prompt.Target{Upstream: "other"}, []byte(armorHitJSONBody)); d.Action != prompt.ActionPass {
		t.Errorf("名单外应放行，实际 %q", d.Action)
	}
	if _, d := prompt.Judge(got, prompt.Target{Upstream: "phanthywork"}, []byte(armorHitJSONBody)); d.Action != prompt.ActionBlock {
		t.Errorf("名单内应拦截，实际 %q", d.Action)
	}
	// 非法范围：报错而不是静默接受
	if _, err := a.ApplySettings(json.RawMessage(`{"prompt_armor_block_scope":"nope"}`)); err == nil {
		t.Error("非法范围应当报错（设置页填错时要当场可见）")
	}
}
