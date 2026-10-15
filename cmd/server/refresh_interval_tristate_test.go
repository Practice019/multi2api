// refresh_interval_tristate_test.go 「续期间隔的三态」守卫。
//
// # 这里守的是一个真实缺陷（2026-10-15）
//
// 两层互相矛盾：
//
//	cmd/server/main.go:410-412  注释写着「可用 refresh_interval_seconds<=0 关闭」，
//	                            代码也是 `if cfg.CodeartsRefreshInterval > 0 { 注册 }`
//	cmd/server/config.go:1361   `if iv <= 0 { iv = 1800 }` ← 在装配层看到之前就改成 1800
//
// 于是**文档承诺的关闭开关根本到不了装配层**：用户写 0 想关掉，得到的是一个
// 每 30 分钟跑一次的周期任务。
//
// # 为什么这不是小事
//
// codearts / trae 的 refreshToken 是**消费型**的（换一次旧的作废）。关不掉
// 就意味着网关会一直在后台把用户的凭证刷失效 —— 实测用户账号被刷成
//
//	invalid refresh token: 'the refresh token has been used'
//
// 之后只能重新登录。而用户当时的意图恰恰就是"关掉它别再刷了"。
//
// # 根因
//
// 本仓一直在防的那类三态问题：**「没配」与「显式 0」必须不同**
// （`boolOr` 就是为它存在的，这里缺的是秒数版的 `secsOr`）。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ⚠ 三个值必须给出**三种**不同结果。
//
//	未配   → 默认 1800s（保持既有行为，不能变）
//	显式 0 → 0（= 关闭；装配层的 `> 0` 判断据此不注册任务）
//	显式 N → N
func TestCodeartsRefreshIntervalIsTriState(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want float64 // 秒
		why  string
	}{
		{
			"未配 → 默认 30m",
			`{"codearts":{"auth_dir":"./auths"}}`,
			1800,
			"没写这一行时必须与改造前逐字节一致（统一 30min 全量续）",
		},
		{
			"显式 0 → 关闭",
			`{"codearts":{"auth_dir":"./auths","refresh_interval_seconds":0}}`,
			0,
			"★ 这就是本次修的：0 必须原样传下去，装配层才可能不注册任务。\n" +
				"改造前它被改成 1800 ⇒ 用户按注释写 0 想关掉，却得到每 30 分钟跑一次，\n" +
				"而 codearts 的 refreshToken 是消费型的 —— 那就是用户的账号被刷失效的原因。",
		},
		{
			"显式 15 → 15s",
			`{"codearts":{"auth_dir":"./auths","refresh_interval_seconds":15}}`,
			15,
			"正向：显式值照常生效（这条改造前就有，别改坏）",
		},
		{
			"负数 → 按没配处理（落默认）",
			`{"codearts":{"auth_dir":"./auths","refresh_interval_seconds":-5}}`,
			1800,
			"负数是明显笔误：落默认 = 与「没写这一行」一致；\n" +
				"若把它当「关闭」，一个写错的配置会静默地少一个能力",
		},
	}
	for _, c := range cases {
		fp := filepath.Join(dir, c.name+".json")
		if err := os.WriteFile(fp, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(fp)
		if err != nil {
			t.Fatalf("%s: Load 失败: %v", c.name, err)
		}
		if got := cfg.CodeartsRefreshInterval.Seconds(); got != c.want {
			t.Errorf("★ %s：CodeartsRefreshInterval = %vs，期望 %vs\n\n%s",
				c.name, got, c.want, c.why)
		}
	}
}

// trae 同型：它的字段注释一直写「<=0 不注册续期任务」，
// 而 normalize 里原先照样把 <=0 改成 1800 —— 文档承诺的行为从来没生效过。
func TestTraeRefreshIntervalIsTriState(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want float64
	}{
		{"未配 → 默认 30m", `{"trae":{"auth_dir":"./auths/trae"}}`, 1800},
		{"显式 0 → 不注册", `{"trae":{"auth_dir":"./auths/trae","refresh_interval_seconds":0}}`, 0},
		{"显式 60 → 60s", `{"trae":{"auth_dir":"./auths/trae","refresh_interval_seconds":60}}`, 60},
	}
	for _, c := range cases {
		fp := filepath.Join(dir, c.name+".json")
		if err := os.WriteFile(fp, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(fp)
		if err != nil {
			t.Fatalf("%s: Load 失败: %v", c.name, err)
		}
		if got := cfg.TraeRefreshInterval.Seconds(); got != c.want {
			t.Errorf("★ %s：TraeRefreshInterval = %vs，期望 %vs\n\n"+
				"字段注释写的是「<=0 不注册续期任务」，而 normalize 原先无条件把\n"+
				"<=0 改成 1800 ⇒ 那个承诺到不了装配层。实测：配 0 之后启动日志\n"+
				"仍打印「续期间隔 30m0s」。", c.name, got, c.want)
		}
	}
}

// ⚠ 反向：**装配层必须真的按 0 跳过注册**。
//
// 只修配置层（让 0 传下去）而装配层仍无条件注册，等于没修 ——
// 那正是这次"两层各改一半"的形态，所以两层都要钉住。
//
// 判据是源码级：main.go 里注册 codearts 后台续期的那一段必须带条件。
func TestCodeartsRefreshRegistrationRespectsZero(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "if cfg.CodeartsRefreshInterval > 0 {") {
		t.Error("★ main.go 里注册 codearts 后台续期的那一段没有 `> 0` 条件 ——\n" +
			"那么配置层刚修好的「显式 0 = 关闭」到了装配层又会被无条件注册掉，\n" +
			"等于只修了一半（这正是本次缺陷的两层形态）。")
	}
	if !strings.Contains(src, "cb.SetRefreshInterval(cfg.CodeartsRefreshInterval)") {
		t.Error("main.go 里没有 SetRefreshInterval 调用 —— 判据要跟着改，否则这条守卫会静默失效")
	}
}
