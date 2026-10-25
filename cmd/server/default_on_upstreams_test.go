// default_on_upstreams_test.go 「上游一律启用」的守卫。
//
// # 用户的要求（两次，措辞在收紧）
//
//	第一次：「不要有启动或者不启动了，默认全部启动。」
//	第二次：「我不需要这个凭证全失效，安静下来停一个上游，不需要。
//	        就是默认全部停，配置里面也修改一下。」
//
// 第二次明确了要**删掉开关本身** —— 不留 `enabled` 字段。
//
// # ⚠ 这是一次兼容性反转，必须逐条钉住
//
// 改之前：`enabled` 缺席 = **关**；改之后：字段不存在，上游**恒开**。
//
// 于是"以前故意不写 enabled 来关掉某上游"的配置，升级后会**变成打开**
// （而且配置里残留的 `"enabled": false` 会被当成未知字段忽略）——
// 用户不会收到任何提示。这类"默认值反转"最容易悄悄改变部署行为，
// 所以**每个上游一条断言**，任何一个漏改都会红。
package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// providers 是全部上游：JSON 段名 + Config 上对应的派生字段名。
//
// ⚠ 加第 13 个上游时漏加到这里，会让"它是不是启用的"重新变成没人守的状态。
// 下面另有一条**反向**断言：配置结构体里不允许再出现任何 `enabled` 字段 ——
// 它保证"漏加了这条表"也能被发现（因为新上游会带来一个新段）。
var providers = []struct {
	name  string // 配置段名
	field string // 派生字段（Config 上的 bool）
}{
	{"codearts", "CodeartsEnabled"},
	{"workbuddy_intl", "WorkbuddyIntlEnabled"},
	{"loomy", "LoomyEnabled"},
	{"trae", "TraeEnabled"},
	{"cline", "ClineEnabled"},
	{"raccoon", "RaccoonEnabled"},
	{"lobsterai", "LobsteraiEnabled"},
	{"qoder", "QoderEnabled"},
	{"zcode", "ZCodeEnabled"},
	{"minimax", "MiniMaxEnabled"},
	{"verdent", "VerdentEnabled"},
}

// ⚠ 核心判据：**配置段整段缺席**时，每个上游都必须被启用。
func TestAllUpstreamsEnabledByDefault(t *testing.T) {
	c := Default()
	v := reflect.ValueOf(c).Elem()
	for _, p := range providers {
		f := v.FieldByName(p.field)
		if !f.IsValid() {
			t.Errorf("测试表里写了未知字段 %s —— 改字段名时忘了同步这里", p.field)
			continue
		}
		if !f.Bool() {
			t.Errorf("上游 %s 在配置里**整段缺席**时应当启用（%s=false）——\n"+
				"用户要求「默认全部启动」。这里绿不了的话，缺省关闭会让新装的用户"+
				"放好凭证却什么也跑不起来。", p.name, p.field)
		}
	}
}

// ⚠ 反向：连**配置结构体里**都不该再有 `enabled` 字段。
//
// # 为什么这条比上面那条更重要
//
// 用户第二次说的是"删掉开关"，而不只是"改个默认值"。留着字段的后果：
//
//	① 用户写了 `"enabled": false` 却毫无效果 —— 而配置里明明写着它，
//	   他会认为程序坏了（本仓反复踩过的"配了但不生效"形态）
//	② 读结构体的人以为那个字段有用
//
// 所以这条直接扫结构体定义：任何上游段里出现 `json:"enabled"` 就是回归。
// 它同时兜住"新增上游时忘了更新上面那张表"——新上游若带 enabled 字段，
// 这里会红。
func TestNoUpstreamHasEnabledField(t *testing.T) {
	raw, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("读 config.go 失败: %v", err)
	}
	src := string(raw)

	// 先确认前提：确实没有 `Enabled *bool` 的上游字段了。
	if strings.Contains(src, "Enabled *bool `json:\"enabled\"`") {
		t.Error("config.go 里仍有 `Enabled *bool `json:\"enabled\"`` ——\n" +
			"用户要求删掉这个开关。留着它会出现「配了 false 但上游照常跑」这种" +
			"最难查的状态：配置里写着，程序不认。")
	}
	// 放宽一点：任何上游结构体里出现 json:\"enabled\" 都算回归。
	// （SessionSticky.Enabled 用的是同一个 json 名，但它不是上游段 ——
	// 所以按"出现次数"断言：只允许它一处。）
	if n := strings.Count(src, "`json:\"enabled\"`"); n > 1 {
		t.Errorf("config.go 里 `json:\"enabled\"` 出现了 %d 次 ——\n"+
			"除 session_sticky 之外不该再有上游用它（上游的开关已移除）。", n)
	}
}

// 派生字段的求值必须是**常量**（不再读任何配置）。
//
// # 为什么还要有这条（前面的断言不够）
//
// 上面的断言只证明"结构体里没有 enabled 字段"。
// 但如果有人把求值写成 `c.XEnabled = false`（比如复制粘贴时改错），
// 结构体那条仍然会过，而"上游一律启用"就废了。
//
// 所以这里钉住源码形状：`c.XEnabled = <常量>`，
// 且**不允许**再出现 boolOr(...Enabled...) 的读配置形态。
func TestEnabledEvaluationsAreConstant(t *testing.T) {
	raw, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("读 config.go 失败: %v", err)
	}
	src := string(raw)

	for _, p := range providers {
		// 接受 `= true` 与 `= on`（enableDefaults 里那个局部常量）。
		okTrue := strings.Contains(src, "c."+p.field+" = true")
		okOn := strings.Contains(src, "c."+p.field+" = on")
		if !okTrue && !okOn {
			t.Errorf("config.go 里找不到 `c.%s = true`（或 `= on`）——\n"+
				"上游 %s 的启用求值应当是一个**常量**，不再读配置。",
				p.field, p.name)
		}
	}

	// 反向：不允许任何"读配置决定启用"的残留。
	if strings.Contains(src, "boolOr(c.Codearts.Enabled") ||
		strings.Contains(src, "boolOr(c.Loomy.Enabled") ||
		strings.Contains(src, "boolOr(c.Trae.Enabled") {
		t.Error("config.go 里仍有 `boolOr(c.X.Enabled, ...)` —— " +
			"那是「读配置决定启用」的旧形态；用户要求删掉开关（一律启用）")
	}
}
