package oauth

// oauth_test.go —— 守住 auth/state 的 platform 参数。
//
// # 为什么要专门守它（用户实测报的 bug）
//
// 海外版（workbuddy-intl）原来用 `platform=CLI`，拿到的是 **CLI 插件版**登录页，
// 它**没有「设置地区」这一步** —— 于是登录出来的号后端没激活试用：
//
//	对话：429 code=14017「The trial version is not yet activated」
//	计费：/v2/billing/meter/get-user-resource 恒 500（额度显示 —）
//
// 而官方桌面客户端（platform=workbuddy-ai）登录的号完全正常。
// 参照 register-machine 已跑通的客户端登录流程改过来。
//
// # 这条测试在改回 platform=CLI 时必红 —— 它就是防"哪天又手滑改回去"的。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startAuthState 起一个假上游，跑一次 Start()，返回它收到的请求 URL（原始，含 query）。
func startAuthState(t *testing.T, baseURL string) string {
	t.Helper()
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RequestURI() // 含 path + query
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"state": "st-1", "authUrl": "https://example.com/authz"},
		})
	}))
	defer srv.Close()

	// 用假上游地址，但 Platform 按 baseURL 推导（与生产 New(base) 同一条判据）
	c := New(baseURL)
	c.BaseURL = srv.URL
	if _, _, err := c.Start(); err != nil {
		t.Fatalf("Start() 失败: %v", err)
	}
	return got
}

func TestStartUsesClientPlatformForIntl(t *testing.T) {
	uri := startAuthState(t, "https://www.workbuddy.ai")
	// 海外版必须是 workbuddy-ai（客户端流程，含设置地区），绝不能是 CLI
	if strings.Contains(uri, "platform=CLI") {
		t.Fatalf("★ 海外版 auth/state 用了 platform=CLI —— 登录页没有「设置地区」，"+
			"账号不完整开通（对话 14017 / 计费 500）。实际=%s", uri)
	}
	if !strings.Contains(uri, "platform=workbuddy-ai") {
		t.Fatalf("海外版 auth/state 应带 platform=workbuddy-ai，实际=%s", uri)
	}
	if !strings.Contains(uri, "version=5.5.2") {
		t.Fatalf("海外版 auth/state 应带 version=5.5.2（客户端登录页需要），实际=%s", uri)
	}
}

func TestStartKeepsCliPlatformForCN(t *testing.T) {
	uri := startAuthState(t, "https://copilot.tencent.com")
	// CN 版保持原样：本来就正常，且无实测依据改它
	if !strings.Contains(uri, "platform=CLI") {
		t.Fatalf("CN 版 auth/state 应保持 platform=CLI（行为逐字节不变），实际=%s", uri)
	}
	if strings.Contains(uri, "platform=workbuddy-ai") {
		t.Fatalf("CN 版不该用海外版的 platform，实际=%s", uri)
	}
}

func TestNewDerivesPlatformByChannel(t *testing.T) {
	if got := New("https://www.workbuddy.ai").Platform; got != platformIntl {
		t.Fatalf("intl Platform = %q, want %q", got, platformIntl)
	}
	if got := New("https://copilot.tencent.com").Platform; got != platformCN {
		t.Fatalf("cn Platform = %q, want %q", got, platformCN)
	}
	if got := New("").Platform; got != platformCN {
		t.Fatalf("默认 Platform = %q, want %q", got, platformCN)
	}
}
