// example_config_test.go 发布产物里的示例配置**必须能被程序自己解析**。
//
// # 这条守卫是一次真实失误逼出来的
//
// 我在给 `config.example.json` 加中文注释时，把一段说明写在了
// **「最后一个键之后、`}` 之前」** —— 那个位置在 JSON 里无法表达：
//
//	{
//	  "idle_timeout_seconds": 300     ← 不能加逗号（后面没有键了）
//	  // 一段说明                      ← 于是注释只能吊在这里
//	}
//
// 标准 JSON 不允许尾随逗号，所以"最后一个键 + 说明注释"这个组合写不出来。
// 剥离注释后剩下的字节序列不再构成合法 JSON，报错是
// `invalid character '}' looking for beginning of object key string`。
//
// ⚠ 而我发现它的方式是**手工起了一次服务**。这个文件会打进 Release 产物、
// 是新用户第一个打开的东西 —— 一个跑不起来的示例比没有示例更糟
// （他会以为是自己配错了）。所以必须有自动化守卫。
//
// # 判据用「程序自己的解析器」，不用 encoding/json
//
// 示例里现在**允许写注释**（JSONC 子集）。用标准库判会把合法的带注释示例
// 误判为坏 —— 那等于逼着示例不能写注释，与本次改进的目的冲突。
// 所以判据必须与服务器**同一条解析路径**。
package main

import (
	"os"
	"strings"
	"testing"
)

// ⚠ config.example.json 必须能被程序自己的解析路径读成一份合法配置。
func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatalf("读 config.example.json 失败: %v\n"+
			"（它是 Release 产物的一部分 —— 见 .github/workflows/release.yml，"+
			"新用户第一个打开的就是它）", err)
	}

	// 与 Load 同一条路径：Default() → unmarshalJSONC → normalize。
	// 不走 Load() 是因为它还要碰文件系统（auth_dir 等），这里只验**可解析**。
	c := Default()
	if err := unmarshalJSONC(raw, c); err != nil {
		t.Fatalf("config.example.json 无法解析: %v\n"+
			"\n"+
			"这个文件会被打进 Release 产物、被新用户第一个打开。"+
			"它解析不了的话，用户会以为是自己配错了。\n"+
			"（最常见的写法错误：把说明注释吊在**最后一个键之后** —— "+
			"JSON 不允许尾随逗号，所以那个位置无法同时容纳'逗号'与'注释'。"+
			"说明应当写在**某个键之前**，或并入段的标题注释。）", err)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("config.example.json 能解析但过不了 normalize: %v\n"+
			"（说明里面的值不合规 —— 例如某处 duration 写了空串）", err)
	}
}

// 示例里的注释必须**真的在** —— 否则有人为让它解析而把注释删光，
// 上一条会照常绿，而这份示例的教育价值就没了。
func TestExampleConfigKeepsItsComments(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if n := strings.Count(src, "//"); n < 10 {
		t.Errorf("config.example.json 里只有 %d 处 `//` —— \n"+
			"这份示例的价值一半在于**说明每一项是干什么的**（用户原话："+
			"「我发现这个 Config 配置有点太丑了……太乱了我只能说」）。\n"+
			"为了让解析通过而把注释删光，等于放弃了这次改进的目的。", n)
	}
}

// 反向：示例的**上游段**里不该再有 `enabled` 字段（那个开关已移除）。
//
// # ⚠ 判据要精确到"上游段的花括号范围"，不能按行猜
//
// 我第一版按行跟踪"当前段"：只在行**以 `{` 结尾**时切换段名。
// 变异验证时我把 `"minimax": { "enabled": false, ... },` 单行塞进示例，
// 守卫**没红** —— 因为那一行不以 `{` 结尾，段名根本没被识别，
// 于是那行的 `"enabled"` 落在了"不在任何上游段内"的判定里。
//
// 而"用户单行写一个段"是完全可能的写法。所以改成**花括号配对**：
// 找到 `"<段名>":` 后的 `{`，扫到与它配对的 `}`，只在这个区间内找 `"enabled"`。
//
// 同时也避免了对 `session_sticky.enabled`（合法的，json 名恰好也叫 enabled）
// 的误报 —— 那个段名不在上游清单里，整段直接跳过。
//
// 教训：按"行形态"做的判据会在格式变化时静默失效，而**静默失效的守卫
// 比没有更糟**（它给人已经防住的错觉）。所以这条才改成结构判据。
func TestExampleConfigHasNoUpstreamEnabled(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	// 注释先剥离：段名可能出现在注释里（例如说明"以前可以写 minimax 段"）。
	src := string(stripJSONComments(raw))

	// 上游段名（一个段 = 一个上游）。session_sticky / schedule / login 等
	// 不是上游段，它们的 enabled 是另一回事。
	upstreamSections := []string{
		"codearts", "workbuddy_intl", "loomy", "trae", "cline",
		"raccoon", "lobsterai", "qoder", "zcode", "minimax",
	}

	for _, name := range upstreamSections {
		start, end, ok := sectionSpan(src, name)
		if !ok {
			continue // 示例里没写这个段（可省略，正常）
		}
		body := src[start:end]
		if idx := strings.Index(body, `"enabled"`); idx >= 0 {
			line := 1 + strings.Count(src[:start+idx], "\n")
			t.Errorf("config.example.json 第 %d 行：上游段 `%s` 里还有 \"enabled\" ——\n"+
				"内容片段：%q\n"+
				"\n"+
				"上游的 enabled 开关已被移除（一律启用）。示例里留着它会让新用户"+
				"以为还能靠它关掉某个上游，而那个字段现在会被静默忽略。",
				line, name, snippet(body, idx))
		}
	}
}

// sectionSpan 返回顶层段 `"name": { ... }` 里**花括号内部**的字节区间。
//
// 找不到该段、或括号不配平时返回 ok=false。
func sectionSpan(src, name string) (start, end int, ok bool) {
	needle := `"` + name + `"`
	i := strings.Index(src, needle)
	if i < 0 {
		return 0, 0, false
	}
	// 跳过 `"name"` 与冒号之间的空白，确认后面确实是冒号。
	j := i + len(needle)
	for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\r' || src[j] == '\n') {
		j++
	}
	if j >= len(src) || src[j] != ':' {
		return 0, 0, false
	}
	j++
	// 找到值对象的开括号。
	for j < len(src) && src[j] != '{' {
		if src[j] == ',' || src[j] == '}' || src[j] == '"' {
			// 值不是对象（例如 `"listen": "..."`）—— 不是段。
			return 0, 0, false
		}
		j++
	}
	if j >= len(src) {
		return 0, 0, false
	}
	open := j
	depth := 0
	for k := open; k < len(src); k++ {
		switch src[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return open + 1, k, true
			}
		}
	}
	return 0, 0, false // 括号不配平（示例本身坏了，由 TestExampleConfigParses 报）
}

// snippet 取命中处附近的一小段，便于在报错里看到上下文。
func snippet(s string, at int) string {
	lo := at - 40
	if lo < 0 {
		lo = 0
	}
	hi := at + 60
	if hi > len(s) {
		hi = len(s)
	}
	return strings.TrimSpace(s[lo:hi])
}
