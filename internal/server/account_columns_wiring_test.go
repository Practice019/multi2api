// account_columns_wiring_test.go 源码级守卫：**规范列 id 与前端注册表必须成对**。
//
// # 这条守卫守的是什么（用户刚踩过的那件事）
//
// 「加一列」在本仓要同时改三处：
//
//	① gateway/account_columns.go 的 id 常量 + IsKnownAccountColumn 白名单
//	② 该上游的 AccountColumns()（决定这个上游显不显示这一列）
//	③ webui.html 的列注册表（标题 + 单元格渲染器）
//
// 漏掉 ③ 的后果是**完全静默**的：前端对未知列 id 只打一行 `console.warn`
// 然后跳过那一列 —— 后端照发、接口看着正常、页面不报任何错，**只是少了一列**。
//
// 用户这次的现场正是它：「我就让你添加一列信息，为什么这么困难呢？」
// 一半的困难来自这条链要过四层，另一半来自"漏一处没人告诉你"。
//
// # 为什么从源码扫 id，而不是在这里硬编码一份清单
//
// 硬编码清单会随列增减而腐烂，而腐烂的方向恰好是**新列不被检查** ——
// 那正是本守卫要防的东西。扫 gateway 的源码则自动跟上：
// 那边新增一个 `AccountCol\w+ = "..."` 常量，这里立刻要求前端也补上。
//
// 双向都查（缺一列 / 多一个不认识的名字），因为两个方向都是真实的错法：
// 前者是漏改前端，后者是前端写了个后端不认的 id（拼错/改名）。
package server

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// canonicalColumnIDs 从 gateway 的源码里扫出全部规范列 id。
//
// 扫的是**声明处**（`AccountColXxx = "id"`），不是白名单 switch ——
// 白名单可能被漏更新，而常量声明是"加一列"的必经之处。
func canonicalColumnIDs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../internal/gateway/account_columns.go")
	if err != nil {
		t.Fatalf("读 gateway/account_columns.go 失败: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*AccountCol\w+\s*=\s*"([a-z_]+)"`)
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) < 10 {
		t.Fatalf("只扫到 %d 个列 id —— 正则多半失效了，先修它（否则本守卫形同虚设）", len(out))
	}
	sort.Strings(out)
	return out
}

// braceBlock 从 s 的第一个 `{` 起做花括号配平，返回含外层括号的整段。
//
// 前提：这一段里不会出现"落在字符串字面量里的花括号"。本文件用它取的是
// 列定义块（title/tip 都是普通中文与标点），所以成立；换用途时要重新确认。
func braceBlock(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:] // 没配平（文件被截断）→ 返回余下全部，交由调用方判断
}

// webuiBlock 取 webui.html 里某一段（从 startMarker 到 endMarker）。
func webuiBlock(t *testing.T, src, startMarker, endMarker string) string {
	t.Helper()
	i := strings.Index(src, startMarker)
	if i < 0 {
		t.Fatalf("webui.html 里找不到 %q —— 这段结构被改名/删掉了，"+
			"本守卫需要跟着更新（它守的正是这块）", startMarker)
	}
	rest := src[i:]
	j := strings.Index(rest, endMarker)
	if j < 0 {
		t.Fatalf("webui.html 里 %q 之后找不到 %q", startMarker, endMarker)
	}
	return rest[:j]
}

// 每个规范列 id 都必须在前端注册表里**同时**有标题与渲染器。
//
// 判据（两条都要）：
//
//	列标题   ACCT_COLUMN_DEFS 里出现 `id:` （后面跟 `{`）
//	渲染器   cells 里出现 `id: `<td…`  （那一格的 HTML）
//
// ⚠ 只查一条不够：只加标题不加渲染器，表格里那一格是空的（th 有、td 没有），
// 整行会**错位**；反过来则是一个没人认领的渲染器。
func TestEveryCanonicalColumnIsWiredInWebUI(t *testing.T) {
	raw, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	src := string(raw)

	defs := webuiBlock(t, src, "const ACCT_COLUMN_DEFS = {", "\n  };")
	// cells 是 accountRow 里那个对象字面量；用它的第一个键定位、到收尾 `};` 为止。
	cells := webuiBlock(t, src, "const cells = {", "\n    };")

	ids := canonicalColumnIDs(t)
	for _, id := range ids {
		// 标题：先定位该 id 的定义行，再在它**后面的一个窗口内**找 title。
		//
		// ⚠ 不能只在首行找：定义有单行（`provider: { title: '上游' },`）与
		// 多行（`free_quota: {\n  title: '免费额度', …}`）两种写法，
		// 只查首行会把多行的那几个全判成"没 title"——
		// 我自己第一版就是这么错的，而它红得像是真缺陷（三个列"缺标题"）。
		loc := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(id) + `:\s*\{`).FindStringIndex(defs)
		if loc == nil {
			t.Errorf("列 %q 在 ACCT_COLUMN_DEFS 里没有标题 ——\n"+
				"前端遇到未知列 id 只打一行 console.warn 然后**跳过**，"+
				"所以页面上只是少一列、不报任何错。\n"+
				"修法：在 webui.html 的 ACCT_COLUMN_DEFS 里补 `%s: { title: '…' }`", id, id)
		} else {
			// 用**花括号配平**精确取出这一列的定义块。
			//
			// ⚠ 我先后试过两种偷懒写法，都被自己的用例打回来了：
			//
			//	只查首行        → 多行定义（free_quota / token_expiry / welfare）
			//	                  全被判成"没 title"，红得像真缺陷
			//	截到下一个 "\n    " → 4 空格会匹配到 6 空格缩进的**前缀**，
			//	                  把块切空，同样假红
			//
			// 配平是唯一不依赖缩进约定的取法（这些 title 里不会出现花括号，
			// 所以不需要处理字符串内的括号）。
			blk := braceBlock(defs[loc[1]-1:])
			if !strings.Contains(blk, "title:") {
				// 有空对象也算通过会漏掉"忘了写 title"—— 那一列的表头是**空白**，
				// 比少一列更难发现（列还在，只是没名字）。
				t.Errorf("列 %q 的定义里没有 title —— 表头会渲染成空白（列在、名字没了）", id)
			}
		}
		// 渲染器：`id: `<td…``
		if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(id) + `:\s*` + "`" + `<td`).MatchString(cells) {
			t.Errorf("列 %q 在 accountRow 的 cells 里没有渲染器 ——\n"+
				"只加标题不加渲染器会让**表头有、单元格没有**，整行错位。\n"+
				"修法：在 cells 里补 `%s: `<td>…</td>`,`", id, id)
		}
	}
}

// 反向：前端注册表里不能有后端不认的列名。
//
// 这条抓的是拼错/改名 —— 那种列在界面上同样只是"不出现"，
// 但它连 console.warn 都不会有（前端以为自己认识它）。
func TestWebUIColumnRegistryHasNoUnknownIDs(t *testing.T) {
	raw, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	defs := webuiBlock(t, string(raw), "const ACCT_COLUMN_DEFS = {", "\n  };")

	known := map[string]bool{}
	for _, id := range canonicalColumnIDs(t) {
		known[id] = true
	}
	re := regexp.MustCompile(`(?m)^\s*([a-z_]+):\s*\{`)
	for _, m := range re.FindAllStringSubmatch(defs, -1) {
		if !known[m[1]] {
			t.Errorf("webui.html 的列注册表里有 %q，但 gateway 的规范词汇表里没有它。\n"+
				"两种可能：后端那一列被改名/删了（前端漏跟），"+
				"或者这里拼错了（那一列永远不显示，且**连 warn 都没有**）。", m[1])
		}
	}
}

// 「免费额度」这一列的端到端接线：id → 标题 → 渲染器三者都在。
//
// 上面两条是通用守卫；这条把**用户点名要的那一列**单独钉住，
// 免得将来有人"顺手"把它从某个上游的列集里删掉（通用守卫只查
// gateway↔前端，不查"哪个上游报了它"）。
func TestFreeQuotaColumnFullyWired(t *testing.T) {
	raw, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	src := string(raw)

	for _, needle := range []string{
		"free_quota: {",       // 标题定义
		"free_quota: `<td",    // 渲染器
		"freeQuotaCellHTML",   // 三态渲染函数
		"data-free-quota-uid", // 无感轮询按它定位单元格
		"免费额度",                // 用户看到的字
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("webui.html 里缺少 %q —— 「免费额度」这一列会不显示、"+
				"或显示但不随轮询更新。", needle)
		}
	}
	// 渲染函数的三态必须在：拿不到分项时回 `—`，**不回 0**。
	fn := webuiBlock(t, src, "function freeQuotaCellHTML(", "\n  }")
	if !strings.Contains(fn, "typeof v !== 'number'") {
		t.Error("freeQuotaCellHTML 的判据必须是「字段类型是不是 number」——\n" +
			"改成 `if (!v)` 之类的真值判断会把「没读到」与「0」混为一谈，" +
			"界面把未知显示成「免费额度已用完」，用户据此以为账号在烧永久积分。")
	}
}
