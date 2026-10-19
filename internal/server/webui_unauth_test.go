// webui_unauth_test.go 「未授权 ⇒ 整块控制台都不显示」的守卫。
//
// # 用户报障（原话）
//
//	「我希望只显示红色，绿色全部不要」
//
// 红色 = 那句"本次访问未获授权"的指路提示（#keyHint）；
// 绿色 = 导航 + 所有面板（<main id="appMain">）。
//
// 改造前未授权时只**加**了一段提示，面板照常渲染 —— 用户看到的正是
// "提示说没授权，但设置页还在那里"，而每点一下都 403。
//
// # 为什么用源码级判据
//
// 页面是单文件内嵌 HTML（`//go:embed`），"未授权时隐藏了哪一块"是
// **运行时 DOM 行为**，Go 侧测不到（要起浏览器）。所以这里钉住那个
// 行为所依赖的**三个事实**：
//
//	① 主容器有 id（否则脚本无从收藏它）
//	② 未授权分支调 hideConsole()（而不是只加提示）
//	③ hideConsole() 真的把主容器收起来
//
// 这三条任一被删掉，那个"未授权也能看见面板"的形态就会悄悄回来。
package server

import (
	"strings"
	"testing"
)

func TestUnauthorizedHidesWholeConsole(t *testing.T) {
	raw, err := readWebUIHTML()
	if err != nil {
		t.Fatal(err)
	}
	// ⚠ 归一化行尾再比对：内嵌的 webui.html 在工作树里是 CRLF，而
	// 源码里的锚点按 LF 写 —— 不归一化的话下面每条 strings.Contains
	// 都会静默不命中（而那种失败看起来像"注释被改了"）。
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")

	// ① 主容器必须可寻址。
	if !strings.Contains(src, `<main id="appMain">`) {
		t.Error("主容器（<main>）没有 id —— 脚本无从在未授权时把它收起来")
	}

	// ② 「没有身份时收起控制台」这件事必须**仍然存在于某处** —— 但载体变了。
	//
	// ⚠ 2026-10-19（T7）：旧的载体是 boot() 里那个
	//
	//	} else {
	//	  // 服务端**没有**注入任何凭据 ⇒ 本次访问未获授权
	//	  hideConsole();
	//
	// 分支。T7 起这条路**走不到了**：服务端不再把未登录的人送到这个页面
	// （送去登录页），所以"未授权也能看见面板"这个原始故障形态从根上消失。
	//
	// 但"会话中途失效"（登出 / 改口令 / 被禁用 / cookie 到期）时面板还在屏幕上
	// —— 那一刻仍然必须收起控制台。所以判据改成：**会话失效的处置路径**里
	// 必须调 hideConsole()，而不是只看某个特定分支还在不在。
	if !strings.Contains(src, "function hideConsole()") {
		t.Fatal("没有 hideConsole() —— 会话失效时无法隐藏整块控制台")
	}
	// probeSession 里 401 ⇒ 去登录页（而不是留一屏点不动的面板）；
	// refresh 里 401/403 ⇒ 收起控制台 + 说清原因。
	for _, anchor := range []string{
		"async function probeSession()",
		"toLogin('expired')",
	} {
		if !strings.Contains(src, anchor) {
			t.Errorf("★ 找不到会话失效的处置路径（%q）——\n"+
				"旧模型那条「未授权分支」已被 T7 删除，新模型必须有一条等价的\n"+
				"「没有有效会话 ⇒ 别让用户面对一屏点不动的面板」路径。", anchor)
		}
	}
	// refresh 的失败分支同样要收起控制台（probeSession 之外的兜底）。
	//
	// ⚠ 判据要在锚点的**两侧**找 hideConsole()：那条分支里 `hideConsole()`
	// 在 `setDot(...)` **之前**（先收面板再报状态），只往后找必然找不到。
	fi := strings.Index(src, "登录状态已失效 · 请重新登录")
	if fi < 0 {
		t.Fatal("refresh 的 401 分支不见了（它的文案是「登录状态已失效 · 请重新登录」）")
	}
	winLo, winHi := fi-900, fi+220
	if winLo < 0 {
		winLo = 0
	}
	if winHi > len(src) {
		winHi = len(src)
	}
	if !strings.Contains(src[winLo:winHi], "hideConsole()") {
		t.Error("★ refresh 的认证失败分支没有收起控制台 ——\n" +
			"用户报的正是「token 没授权，设置页却还在那里」：\n" +
			"只加提示、不收起面板的话，用户会以为「能看见就能改」，\n" +
			"而每点一下都失败，看不出到底发生了什么。")
	}

	// ③ hideConsole() 本身必须真的隐藏主容器。
	//
	// ⚠ 判据分两半：`$('appMain')` 与 `display = 'none'`。
	// 写成一整句 `$('appMain').style.display = 'none'` 是错的 ——
	// 实际代码是 `const m = $('appMain'); if (m) m.style.display = 'none'`，
	// 中间隔着 `; if (m) `。（我第一版就是这么写错的，测试立刻红了。）
	hi := strings.Index(src, "function hideConsole()")
	if hi < 0 {
		t.Fatal("hideConsole 未定义")
	}
	body := src[hi:]
	if end := strings.Index(body, "\n  }\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "$('appMain')") || !strings.Contains(body, "display = 'none'") {
		t.Error("hideConsole() 没有把主容器设为 display:none —— 那个函数等于空转，" +
			"未授权时面板仍会显示")
	}

	// ④ 反向：取数成功时必须**展开**（否则一次未授权之后永久空白）。
	if !strings.Contains(src, "showConsole();") {
		t.Error("没有 showConsole() 调用 —— 取数成功时控制台仍是隐藏的，" +
			"用户会看到「登录了却什么都没有」。")
	}
}
