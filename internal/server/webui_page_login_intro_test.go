package server

import (
	"strings"
	"testing"
)

// TestWebUIPageLoginRewritesIntro 钉住 T-342 修掉的界面缺陷（用户实测）。
//
// 现象：点 codar 的「添加账号」，弹窗里同时出现两句说明 ——
// OAuth 型（点下面的链接在浏览器完成登录…）+ page 型（点开此页：填手机号…）。
// 根因：addAccount 进来先无条件 renderLoginIntro('browser') 放占位文案，
// 而 page 型分支只改 #loginBody、从没改写 #loginIntro。
func TestWebUIPageLoginRewritesIntro(t *testing.T) {
	src := frontendSourceString()

	if !strings.Contains(src, "mode === 'page'") {
		t.Error("renderLoginIntro 没有 'page' 分支")
	}
	branch := strings.Index(src, "if (pageLogin) {")
	if branch < 0 {
		t.Fatal("找不到 page 型分支（if (pageLogin)）")
	}
	rest := src[branch:]
	intro := strings.Index(rest, "renderLoginIntro('page')")
	iframe := strings.Index(rest, `id="manualPage"`)
	if intro < 0 {
		t.Fatal("page 型分支里没有 renderLoginIntro('page')（会留着 OAuth 那句）")
	}
	if iframe > 0 && intro > iframe {
		t.Error("renderLoginIntro('page') 必须在渲染 iframe 之前")
	}
}
