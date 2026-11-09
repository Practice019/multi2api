package codar

import (
	"strings"
	"testing"
)

// TestInvitePageShowsCodeAndCaptcha 邀请页必须能：看到自己的码 + 有 captcha 入口 + 有绑定表单。
func TestInvitePageShowsCodeAndCaptcha(t *testing.T) {
	html := RenderInvitePage(InvitePageSpec{
		UID: "user_19275734986_8275", InviteCode: "PB23JQ",
		InviteLink: "https://kuaipaper.com/?i=PB23JQ", Invited: false, CaptchaID: "CID", CaptchaJSURL: "https://x/ct4.js",
	})
	for _, want := range []string{
		"PB23JQ",                          // 自己的码
		"https://kuaipaper.com/?i=PB23JQ", // 链接
		`id="captcha-trigger"`,            // ★ captcha 入口（T-342 的教训：空 div 等于没入口）
		"lot_number",                      // 四个字段名
		`id="invite-code"`,                // 邀请码输入框
		`id="bind"`,                       // 绑定按钮
	} {
		if !strings.Contains(html, want) {
			t.Errorf("邀请页缺少 %s", want)
		}
	}
}

// TestInvitePageHidesFormWhenInvited 已被邀请 ⇒ 不渲染绑定表单（再绑无效，别画假按钮）。
func TestInvitePageHidesFormWhenInvited(t *testing.T) {
	html := RenderInvitePage(InvitePageSpec{
		UID: "user_15083583411_7985", InviteCode: "BB5VYS", Invited: true,
		InvitedMessage: "用户已被邀请", CaptchaID: "CID",
	})
	if strings.Contains(html, `id="bind"`) {
		t.Error("已被邀请时不该出现绑定按钮（每个号只能被邀请一次，画了必然失败）")
	}
	if !strings.Contains(html, "已被邀请") {
		t.Error("已被邀请时应显示状态说明")
	}
}

// TestInvitePageBlocksMissingCaptchaLocally 未完成验证时本地拦截（不发请求）—— 与登录页同一判据。
func TestInvitePageBlocksMissingCaptchaLocally(t *testing.T) {
	html := RenderInvitePage(InvitePageSpec{UID: "u", InviteCode: "C", CaptchaID: "CID"})
	for _, want := range []string{"请先完成上方的人工验证", "captcha_output", "pass_token", "gen_time"} {
		if !strings.Contains(html, want) {
			t.Errorf("缺本地 captcha 拦截：%s", want)
		}
	}
}
