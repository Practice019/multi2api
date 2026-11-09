package loginui

import (
	"strings"
	"testing"
)

// TestCaptchaPageBlocksEmptyAndOffersEntry 钉住 T-342 的两件事（都是用户实测出来的）：
//
//	① captcha 为空时**本地**拦下，不要让上游回一句"缺少 lot_number"
//	② 页面上必须存在**可点的验证入口**（此前 #cap 是个空 div：用户根本无从完成验证）
func TestCaptchaPageBlocksEmptyAndOffersEntry(t *testing.T) {
	html := Render(PageSpec{
		Title: "T", Subtitle: "T",
		SMSURL: "/sms", VerifyURL: "/verify",
		NeedCaptcha: true, CaptchaID: "cid",
		HeadScripts: []string{"https://static.alicaptcha.com/v4/ct4.js"},
	})

	// ① 本地拦截：发送前检查四个字段
	for _, want := range []string{"lot_number", "captcha_output", "pass_token", "gen_time"} {
		if !strings.Contains(html, want) {
			t.Errorf("页面里没有 captcha 字段 %s（无法本地校验）", want)
		}
	}
	if !strings.Contains(html, "请先完成") {
		t.Error("缺少本地拦截提示（captcha 为空时应直接提示，而不是打上游）")
	}
	// ② 入口：必须有一个可点的按钮触发验证组件
	if !strings.Contains(html, `id="captcha-trigger"`) {
		t.Error(`页面上没有人工验证的触发入口（不能是空 div：用户无从完成验证）`)
	}
	// ③ 不需要 captcha 的页面不该渲染这些（不能把 loomy 也带上）
	plain := Render(PageSpec{Title: "T", SMSURL: "/s", VerifyURL: "/v"})
	if strings.Contains(plain, `id="captcha-trigger"`) {
		t.Error(`NeedCaptcha=false 的页面不该渲染出验证按钮（loomy 是这类）；注意：脚本里出现该 id 是允许的（有 if (trig) 守卫）`)
	}
}
