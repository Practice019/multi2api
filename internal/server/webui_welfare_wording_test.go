package server

import (
	"regexp"
	"strings"
	"testing"
)

// 本文件把「福利」列的**语义红线**搬进 `go test`。
//
// # 为什么需要（评审指出的 CI 盲区）
//
// 这条红线原本只有 `tests/frontend/accounts_e2e.js` 在守 —— 而那个文件
// **不在 CI 里**（`.github/workflows/ci.yml` 只跑 `go test ./...`，跑浏览器
// e2e 需要真实实例 + Chrome）。于是一条"用户明确要求、且最容易被好心改坏"
// 的语义约束，在 CI 上等于没有覆盖。
//
// # 红线是什么
//
// 「福利」列只允许说**我们自己知道的**事实：
//
//	今天领过（本地领取记录）  → `已领取`
//	领了但没领到 / 资格不符    → `未领到`
//	领取动作失败              → `失败`
//	说不准（没记录 / 查不到）  → `—`
//
// **绝不**渲染「未领取」「已领完」「无可领」这类**断言上游状态**的词：
// 上游的 claimable 字段分不清"已领完"和"资格不符"，写出来就是编的。
// 用户看到的会是一个假状态，而假状态比空白更有害 —— 空白让人去查，
// 假状态让人放心。
//
// # 为什么判据落在"函数体剥注释后不含禁词"
//
// 注释里**必须**能写这些词（"绝不渲染未领取"本身就是注释）。
// 所以先剥注释再查；同时又必须只查这个渲染函数，不能全文件查 ——
// 成长计划面板有一句正向事实「奖励已领完」，那是**另一列**的正当文案。

// jsFunctionBody 截出以 `head` 开头的函数的函数体（到行首两空格的 `}` 为止）。
//
// 与 webui_token_pill_test.go 里那个 tokenPillBody 同一思路：**限定范围**再判，
// 否则会误伤别的函数里的同名文案。
func jsFunctionBody(t *testing.T, src, head string) string {
	t.Helper()
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("webui.html 里找不到 %s —— 守卫失效（fail-open）", head)
	}
	rest := src[i:]
	j := strings.Index(rest, "\n  }")
	if j < 0 {
		t.Fatalf("%s 找不到函数体结尾", head)
	}
	return rest[:j]
}

// TestWebUIWelfareCellOnlyRendersAllowedValues 「福利」列只许渲染四个白名单值。
//
// 判据落在**文本节点**（`>...</>` 之间）而不是整个函数体 —— 这一点是被实测纠正的：
//
// 第一版我写的是"函数体剥注释后不许出现禁词"，于是它红在
// `title="…界面无法区分「今天已领完」与「资格不符」"` 上。但那句话是**对的** ——
// 它正是在解释为什么不能这么说。注释与 title 里**必须**能提到这些词，
// 否则就没法把"不要这么说"讲清楚。
//
// 红线管的是**渲染给用户看的那个值**，不是说明文字。所以判据只能落在
// 文本节点上：抽出来，要求每一个都在白名单里。
func TestWebUIWelfareCellOnlyRendersAllowedValues(t *testing.T) {
	src := string(webuiHTML)
	if strings.TrimSpace(src) == "" {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}
	body := jsFunctionBody(t, src, "function welfareCellHTML(")
	// 复用同一个剥注释器（它已经踩过"把 https:// 的 // 当注释"那个坑）。
	code := stripJSCommentsForNames(body)
	if strings.TrimSpace(code) == "" {
		t.Fatal("welfareCellHTML 剥注释后为空 —— 剥注释器坏了，守卫会 fail-open")
	}

	// 只匹配"文本紧跟闭合标签"（`>文字</`）。
	//
	// ⚠ 写成 `>([^<>]*)<` 会**跨界配错**：`</span>` 的 `>` 会和下一个
	// `'<span` 的 `<` 配成一对，把中间的 JS 代码（`'; if (s === 'fail') return '`）
	// 当成"渲染文字"。实测踩到过。加上那个 `/` 之后，只有真正的文本节点会命中。
	allowed := map[string]bool{"已领取": true, "未领到": true, "失败": true, "—": true}
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`>([^<>]*)</`).FindAllStringSubmatch(code, -1) {
		txt := strings.TrimSpace(m[1])
		if txt == "" {
			continue
		}
		seen[txt] = true
		if !allowed[txt] {
			t.Errorf("「福利」列渲染出了白名单外的文字 %q。\n"+
				"允许的只有：已领取 / 未领到 / 失败 / —\n"+
				"上游对 skip 只回一个 claimable 布尔，分不清「已领完」与「资格不符」——\n"+
				"把原因写出来就是编的（见 tasks/plan.md 的语义红线）。", txt)
		}
	}
	if len(seen) == 0 {
		t.Fatal("一个文本节点都没抽到 —— 判据失效（fail-open），它现在恒真")
	}
	// 四个取值都必须真的有出口：少一个意味着某个状态被吞掉
	//（例如 skip 落到 `—`，用户就再也看不到"点过但没领到"）。
	for w := range allowed {
		if !seen[w] {
			t.Errorf("「福利」列的渲染路径里没有 %q —— 该状态没有出口", w)
		}
	}
}

// TestWebUIWelfareFallbackIsTheDash 单独钉住"未知 → `—`"这条回落。
//
// 它是整个红线的最后一道闸：三态都匹配不上时必须落到 `—`，
// **不许**退化成某个默认的肯定结论（例如 `已领取`）。
func TestWebUIWelfareFallbackIsTheDash(t *testing.T) {
	src := string(webuiHTML)
	body := jsFunctionBody(t, src, "function welfareCellHTML(")
	code := stripJSCommentsForNames(body)

	// 至少有一处 return '—'（或带 span 的 `—`）。
	if !strings.Contains(code, "'—'") && !strings.Contains(code, ">—<") {
		t.Error("welfareCellHTML 里找不到 `—` 这个回落值 —— 未知态会渲染成别的东西")
	}
	// 回落的优先级：`—` 的 return 必须存在；不强制它排最后（实现可以是白名单），
	// 但**必须**出现，且不能被某个肯定结论提前短路。
	if strings.Contains(code, "return '已领取'") && !strings.Contains(code, "'—'") {
		t.Error("回落缺失且存在肯定的默认值 —— 这会把「不知道」渲染成「已领取」")
	}
}
