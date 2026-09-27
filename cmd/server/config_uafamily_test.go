package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUAModelFamiliesParsedFromConfig 配置里的 ua_model_families 能被解析。
//
// # 为什么这条测试必须存在（它守的是一整条"配置 → 生效"的链）
//
// 我此前踩过同型的坑：`Qoder` 配置段连 struct 都没有，json.Unmarshal
// 把整个对象当未知字段**静默丢弃** —— 配置写了 enabled:true，
// 程序照旧打印「未启用」，且无任何报错。
//
// 所以每个新增的配置键都必须有一条"从 JSON 到解析后字段"的断言。
// 这里的判据是**表序也要保住**（参照实现是"先命中先返回"，
// 排序会改变语义）。
func TestUAModelFamiliesParsedFromConfig(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{
		"upstream": {
			"ua_model_families": [
				{"match": "gpt-",   "ua": "INTL-UA"},
				{"match": "glm-",   "ua": "CN-UA"},
				{"match": "hy",     "ua": "CN-UA"}
			]
		}
	}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := c.Upstream.UAModelFamilies
	if len(got) != 3 {
		t.Fatalf("解析到 %d 条规则，want 3 —— "+
			"若为 0，说明 json tag 或字段名不对，配置被静默丢弃（本项目踩过同型的坑）", len(got))
	}
	// 表序即优先级：必须与 JSON 里的顺序一致。
	wantOrder := []string{"gpt-", "glm-", "hy"}
	for i, w := range wantOrder {
		if got[i].Match != w {
			t.Errorf("第 %d 条的 match = %q，want %q —— 表序被改变了；"+
				"参照实现是「先命中先返回」，排序会改变语义", i, got[i].Match, w)
		}
	}
	if got[0].UA != "INTL-UA" || got[1].UA != "CN-UA" {
		t.Errorf("UA 未正确解析：%+v", got)
	}
}

// TestUAModelFamiliesAbsentIsEmpty 段缺席时为空（= 不分档，与改造前一致）。
//
// 向后兼容的硬要求：既有部署的 config.json 里没有这个键，
// 解析后必须是**空表**（于是 UA 逐字节不变），而不是某个默认表。
func TestUAModelFamiliesAbsentIsEmpty(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"listen":":7863"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Upstream.UAModelFamilies) != 0 {
		t.Errorf("段缺席时应为空表（不分档），得到 %+v —— "+
			"给一个非空默认表会让既有部署的出站 UA 突然变化", c.Upstream.UAModelFamilies)
	}
}

// TestUAModelFamiliesReferenceIntlTable 参照 intl 表能被**原样**表达。
//
// 这条把参照 product.ts 的协议事实固化成可执行断言：
//
//	gpt- / gemini- / claude-     → 国际版形态（品牌段含 `AI`）
//	glm- / hy / kimi- / minimax- → 国内形态
//
// ⚠ `hy` **没有连字符** —— 它同时匹配 hy3 / hy3-x / hy4-preview。
// 写成 `hy-` 会让那些模型全部落到默认档。
func TestUAModelFamiliesReferenceIntlTable(t *testing.T) {
	const intlUA = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	const cnUA = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"

	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{"upstream":{"ua_model_families":[
		{"match":"gpt-","ua":"` + intlUA + `"},
		{"match":"gemini-","ua":"` + intlUA + `"},
		{"match":"claude-","ua":"` + intlUA + `"},
		{"match":"glm-","ua":"` + cnUA + `"},
		{"match":"hy","ua":"` + cnUA + `"},
		{"match":"kimi-","ua":"` + cnUA + `"},
		{"match":"minimax-","ua":"` + cnUA + `"}
	]}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	rules := c.Upstream.UAModelFamilies
	if len(rules) != 7 {
		t.Fatalf("规则数 = %d，want 7", len(rules))
	}
	// `hy` 必须是**无连字符**的那个形态。
	var hyRule string
	for _, r := range rules {
		if r.Match == "hy" {
			hyRule = r.UA
		}
		if r.Match == "hy-" {
			t.Error("match 写成了 `hy-` —— 它匹配不到 hy3 / hy4-preview（那些名字里没有 `hy-`）")
		}
	}
	if hyRule == "" {
		t.Error("缺少 match=`hy` 的规则")
	}
}
