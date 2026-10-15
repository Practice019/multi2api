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

	// ② 未授权分支必须**收起**控制台，而不是只加一段提示。
	//
	// 判据：`hideConsole()` 调用必须存在；且未授权分支里要有它。
	if !strings.Contains(src, "function hideConsole()") {
		t.Fatal("没有 hideConsole() —— 未授权时无法隐藏整块控制台")
	}
	unauthIdx := strings.Index(src, "} else {\n      // 服务端**没有**注入任何凭据")
	if unauthIdx < 0 {
		t.Fatal("找不到未授权分支（注释被改了？）")
	}
	// 在未授权分支的前 60 行里必须出现 hideConsole()。
	tail := src[unauthIdx:]
	if end := strings.Index(tail, "\n    }\n"); end > 0 {
		tail = tail[:end]
	} else if len(tail) > 4000 {
		tail = tail[:4000]
	}
	if !strings.Contains(tail, "hideConsole()") {
		t.Error("★ 未授权分支里没有 hideConsole() ——\n" +
			"用户报的正是" + "「token 没授权，设置页却还在那里」" + "：\n" +
			"只加提示、不收起面板的话，用户会以为" + "「能看见就能改」" + "，\n" +
			"而每点一下都 403，看不出到底发生了什么。")
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
