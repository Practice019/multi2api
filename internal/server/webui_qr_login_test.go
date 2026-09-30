package server

import (
	"strings"
	"testing"
)

// TestWebUIQRFlowHasNoBrowserWording ★ 扫码流程**不许**出现"浏览器登录"类文案。
//
// # 这条钉的是一个用户实测的错话（2026-09-30）
//
// 弹窗顶部原本有一句**写死**的引导语：
//
//	点下面的链接在浏览器完成登录（本机已是登录态的话通常一键确认）…
//
// 它对扫码类上游（raccoon：用微信扫）**是错的** —— 那个 auth_url 是
// "给手机扫的内容"，在电脑浏览器里打开只会看到一个"请在微信中打开"
// 之类的页面，不是登录路径。而它又不在 `#loginBody` 里（是独立元素），
// 于是弹窗会**同时**显示两段互相矛盾的话：
//
//	去浏览器登录 ← 写死的
//	用手机微信扫码 ← QR 分支渲染的
//
// 用户按第一句点了「复制链接」，然后不知道该怎么办。
//
// # 判据：引导语必须**按形态分叉**，而不是存在一句静态文案
//
// 只断言"存在 renderLoginIntro"是不够的（变异验证的教训，见
// webui_token_lifetime_test.go 里 `const hasAuthURL = true` 那一段）：
// 一个永远写 "browser" 的实现照样能通过"函数存在"的检查。
// 所以要求它**同时**有 qr / local / browser 三种输出，且 qr 那句
// 不出现"浏览器"。
func TestWebUIQRFlowHasNoBrowserWording(t *testing.T) {
	if !strings.Contains(string(webuiHTML), `id="loginIntro"`) {
		t.Fatal("★ 引导语必须带 id（原来是写死的静态文案）—— " +
			"否则 JS 无法按登录形态改写它，扫码类上游会同时看到" +
			"『去浏览器登录』与『用手机微信扫码』两句矛盾的话")
	}

	body := jsFuncBody(t, string(webuiHTML), "function renderLoginIntro(")

	// 三条分支都要存在：qr / local 是显式字面量，browser 是**兜底 else**。
	//
	// ⚠ 兜底不能写死成某个字面量分支 —— 那会让"没识别出的形态"落进
	// 一个空文案（界面上一片空白）。所以断言的是"兜底那一支说的是浏览器登录"。
	for _, mode := range []string{"'qr'", "'local'"} {
		if !strings.Contains(body, mode) {
			t.Errorf("renderLoginIntro 没有处理形态 %s —— "+
				"缺少分叉时那句静态文案会落到不该用它的流程上", mode)
		}
	}
	if !strings.Contains(body, "} else {") {
		t.Error("renderLoginIntro 缺少兜底分支 —— " +
			"未识别的形态会渲染出空引导语（界面一片空白）")
	}
	if !strings.Contains(body, "在浏览器完成登录") {
		t.Error("兜底分支应当保留原来的『在浏览器完成登录』文案（浏览器类流程仍用它）")
	}

	// 抠出 qr 那一支，断言它不提"浏览器"。
	qrAt := strings.Index(body, "'qr'")
	if qrAt < 0 {
		t.Fatal("找不到 qr 分支")
	}
	rest := body[qrAt:]
	end := strings.Index(rest, "} else if")
	if end < 0 {
		end = len(rest)
	}
	qrBranch := rest[:end]
	if strings.Contains(qrBranch, "浏览器") {
		t.Errorf("★ 扫码流程的引导语里出现了「浏览器」：\n%s\n"+
			"扫码类上游的链接是给手机扫的，在电脑浏览器打开不是登录路径 —— "+
			"这句会把人引到一条走不通的路上（用户实测报障）", qrBranch)
	}
	if !strings.Contains(qrBranch, "扫") {
		t.Errorf("扫码流程的引导语应当说清『用手机扫』，实际：\n%s", qrBranch)
	}
}

// TestWebUIQRBranchOffersNoBrowserEntry ★ 扫码分支**不许**给"在浏览器打开"的入口。
//
// # 用户原话：「这个不需要浏览器，只需要微信扫码就可以了。
// 浏览器登录是错的，把浏览器登录去掉。」
//
// 该分支此前（与其它分支共用模板时）会渲染「重新无痕打开」与
// `<a href=…>普通窗口打开</a>` —— 对扫码流程都是错的入口：
// 点它们只会打开一个"请在微信中打开"的页面。
//
// 所以判据是两条**否定**：
//
//	不许有 重新无痕打开（它调用 /admin/login/open，即开浏览器）
//	不许有 <a href=（即"在普通窗口打开授权页"）
//
// 保留「复制链接」是**有意的**：那是"把链接拿到另一台设备去生成二维码再扫"，
// 与"在浏览器打开"是两件事，文案也已改成不带"浏览器"字样。
func TestWebUIQRBranchOffersNoBrowserEntry(t *testing.T) {
	body := jsFuncBody(t, string(webuiHTML), "function addAccount(")

	// 抠出 QR 分支：从 `if (qrSVG) {` 到紧邻的 `} else if (hasAuthURL)`
	at := strings.Index(body, "if (qrSVG) {")
	if at < 0 {
		t.Fatal("★ 找不到 qrSVG 分支 —— 扫码流程的渲染没了")
	}
	rest := body[at:]
	end := strings.Index(rest, "} else if (hasAuthURL)")
	if end < 0 {
		t.Fatal("★ QR 分支没有以 `} else if (hasAuthURL)` 收尾 —— " +
			"切分点找不到会让本守卫静默失效（fail-open）")
	}
	qr := rest[:end]

	for _, bad := range []string{"重新无痕打开", "btnReopenAuth", "<a href="} {
		if strings.Contains(qr, bad) {
			t.Errorf("★ 扫码分支里出现了 %q —— "+
				"扫码流程不该有任何『在浏览器打开』的入口（用户明确要求去掉）", bad)
		}
	}
	// 正面临界：二维码本身必须在
	if !strings.Contains(qr, "${qrSVG}") {
		t.Error("扫码分支必须真的渲染二维码（${qrSVG}）")
	}
	// 「复制链接」保留（换个设备扫的用途）—— 断言它还在，防止被误删
	if !strings.Contains(qr, "复制链接") {
		t.Error("「复制链接」应当保留 —— 它的用途是“把链接拿到另一台设备生成二维码再扫”")
	}
}
