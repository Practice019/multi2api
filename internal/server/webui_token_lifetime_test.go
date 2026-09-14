// webui_token_lifetime_test.go —— 「永久」与「本机拾取登录」两条前端分支的守卫。
//
// # 这两条守卫防的是同一类失败：**后端做对了，前端却永远走不到那条分支**
//
// 本轮后端新增了两件事：
//
//	AccountView.token_never_expires → 界面该显示「永久」而不是 `—`
//	LoginFlow 的 authURL 为空串     → 界面该显示"正在从本机读取登录态"
//
// 两者都是**分支**。分支写错不会报错，只会"看起来没生效" ——
// 而"没生效"与"后端没接线"在界面上长得一模一样，这正是本轮用户最初
// 报的那个形态（"探究一下怎回事"）。
//
// 所以这里断言的不只是"有没有这段字符串"，还有**顺序**与**对偶覆盖**：
//
//	未知分支必须排在「永久」之后（否则 `—` 会把永久吃掉）
//	两个分支都必须给出 id="loginState"（否则轮询循环会抛）
package server

import (
	"strings"
	"testing"
)

// jsFuncBody 截出某个 JS 函数的函数体。
//
// 与 webui_token_pill_test.go 的 tokenPillBody 同一手法、同一理由：
// 全文扫会把别处的同名字符串当成这里的证据（那种守卫会在别人改别处时
// 莫名其妙地红，然后被人删掉）。截体让断言的**作用域**与函数的**作用域**一致。
func jsFuncBody(t *testing.T, src, marker string) string {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("webui.html 里找不到 %q —— 守卫失效（fail-open）", marker)
	}
	rest := src[i:]
	// 函数体到第一个顶格的 `  }` 为止（本文件用两空格缩进的函数体风格）。
	j := strings.Index(rest, "\n  }")
	if j < 0 {
		t.Fatalf("%q 找不到函数体结尾", marker)
	}
	return rest[:j]
}

// TestWebUITokenExpiryRendersPermanentBeforeUnknown ★ 顺序守卫。
//
// # 为什么"顺序"才是这条守卫的核心
//
// `tokenExpiryCellHTML` 的结构是若干 `if` 依次 return。判"永久"的那一段
// 必须排在判"未知"的那一段**之前**。反过来的话：
//
//	loomy 的 token_never_expires=true 先命中"`token_expire_sec` 不存在"
//	→ 直接 return `—`
//	→ 「永久」那段代码成了**永远走不到的死代码**，而且不会有任何报错
//
// 用户看到的仍然是一个 `—`，与"这功能没做"完全一样 —— 正是本轮要修的东西。
func TestWebUITokenExpiryRendersPermanentBeforeUnknown(t *testing.T) {
	body := jsFuncBody(t, string(webuiHTML), "function tokenExpiryCellHTML(")

	neverAt := strings.Index(body, "token_never_expires")
	if neverAt < 0 {
		t.Fatal("★ tokenExpiryCellHTML 里没有 token_never_expires 分支 —— " +
			"后端下发了这个字段，界面却永远显示 `—`")
	}
	// 「永久」这三个字必须真的出现在函数体里（只读字段不渲染等于没接）。
	if !strings.Contains(body, "永久") {
		t.Error("tokenExpiryCellHTML 没有把「永久」渲染出来 —— 用户看不到这个结论")
	}

	unknownMark := "该上游没有提供凭证过期时间"
	unknownAt := strings.Index(body, unknownMark)
	if unknownAt < 0 {
		t.Fatalf("找不到「未知」分支的锚点 %q —— 守卫失效（fail-open）", unknownMark)
	}
	if neverAt > unknownAt {
		t.Error("★ 「永久」分支排在「未知」分支**之后** —— " +
			"未知会先 return，永久那段成了走不到的死代码：" +
			"loomy 那格永远是 `—`，与「没做」在界面上完全一样")
	}

	// 判据必须是 === true（不是"非空"/"truthy"）：
	// 老后端/`/status` 回落路径根本不带这个字段（undefined），
	// 而 undefined 是 falsy —— 用 truthy 判断今天也能工作，
	// 但一旦后端改成下发 false（"有到期时间"），truthy 的写法就会把
	// 一个 2 小时后失效的凭证说成「永久」。显式 === true 把这条钉死在类型上。
	if !strings.Contains(body, "token_never_expires === true") {
		t.Error("判据应当是 `=== true` —— 用 truthy 判断时后端的 false 也会被读成「永久」")
	}
}

// TestWebUIAddAccountHandlesEmptyAuthURL ★ 本机拾取（无授权页）的分支守卫。
func TestWebUIAddAccountHandlesEmptyAuthURL(t *testing.T) {
	body := jsFuncBody(t, string(webuiHTML), "function addAccount(")

	// 1) 必须有那个分支判据，且它读的是**数据**（auth_url）而不是上游名。
	if !strings.Contains(body, "hasAuthURL") {
		t.Fatal("★ addAccount 没有按 auth_url 是否为空分叉 —— " +
			"空 auth_url 会渲染成 `<a href=\"\">打开授权页面</a>`：" +
			"点了只是把当前页重新加载一遍，用户无法判断发生了什么")
	}
	if !strings.Contains(body, "r.auth_url") {
		t.Error("判据应当来自后端回执的 auth_url（不能靠上游名硬编码）")
	}

	// 1b) 判据必须**真的在判"非空"**。
	//
	// # 为什么只断言"出现了 hasAuthURL"不够（变异验证抓到的洞）
	//
	// 把那一行改成 `const hasAuthURL = true;` 之后，下面所有断言**仍然是绿的** ——
	// 因为分支还在、文案还在、`r.auth_url` 也还在（它在 href 里）。
	// 可那正是本轮要修的 bug 本身：无条件渲染授权链接。
	//
	// 所以判据要看**条件的实质**：它必须提到 auth_url，并且带一个
	// 排除空串/非字符串的判断。少了这一层，"分支存在"就只是个摆设。
	decl := ""
	if k := strings.Index(body, "const hasAuthURL"); k >= 0 {
		rest := body[k:]
		if e := strings.IndexAny(rest, "\n"); e >= 0 {
			decl = rest[:e]
		} else {
			decl = rest
		}
	}
	if decl == "" {
		t.Fatal("找不到 hasAuthURL 的声明行 —— 守卫失效（fail-open）")
	}
	if !strings.Contains(decl, "r.auth_url") {
		t.Errorf("hasAuthURL 的判据没有读 auth_url：%s\n"+
			"（写成常量 true 就等于回到了无条件渲染那个 bug）", decl)
	}
	if !strings.Contains(decl, "!== ''") && !strings.Contains(decl, `!== ""`) &&
		!strings.Contains(decl, "length") {
		t.Errorf("hasAuthURL 的判据没有排除空串：%s\n"+
			"（`!== undefined` 这类判据对空串也成立，等于没判）", decl)
	}
	// 2) 判据不许锚在某个上游名上（那会破坏"加新上游前端零改动"）。
	for _, bad := range []string{"'loomy'", `"loomy"`} {
		if strings.Contains(body, bad) {
			t.Errorf("★ addAccount 里出现了上游名 %s —— "+
				"判据必须锚在数据上（auth_url 空不空），否则加第四个上游又要改前端", bad)
		}
	}

	// 3) 两个分支都必须留 id="loginState"。
	//
	// # 这条不是形式主义
	//
	// 轮询循环里有 `$('loginState').innerHTML = ...`（超时、错误、成功三处）。
	// 若 else 分支忘记写这个 id，`$()` 返回 null，赋值会抛 TypeError ——
	// 而抛出的位置在 `setInterval` 的回调里，**页面不会白屏、也不会有提示**，
	// 只是"点了添加账号之后什么都没发生"。
	if n := strings.Count(body, `id="loginState"`); n < 2 {
		t.Errorf(`★ 两个分支都必须给出 id="loginState"（各一处），实际 %d 处 —— `+
			"缺了它，轮询回调里的 $('loginState') 是 null，赋值会静默抛错", n)
	}

	// 4) 无授权页那条分支不许出现空的授权链接。
	//
	// 截出 else 分支（从 "} else {" 到函数体末尾）单独检查。
	elseAt := strings.Index(body, "} else {")
	if elseAt < 0 {
		t.Fatal("找不到无授权页那条分支（} else {）")
	}
	elseBody := body[elseAt:]
	if strings.Contains(elseBody, "<a href=") {
		t.Error("★ 无授权页的分支里出现了 <a href= —— " +
			"那会是一个点了没用的按钮（本机拾取没有可打开的页面）")
	}
	if !strings.Contains(elseBody, "本机") {
		t.Error("无授权页的分支应当说明「登录态在本机」，让用户知道不用点任何东西")
	}
}
