// gate_test.go 管理台门禁的守卫。
//
// # 这组测试是四件事的判据（用户报障「没授权却能改设置」）
//
//	① 远程无凭据          → 拒绝
//	② 远程拿 **api_key**  → 拒绝（这是改造前的**权限倒挂**：对话钥匙能开管理台）
//	③ 远程拿 admin_token  → 放行
//	④ 本机免令牌          → **只在"进程只绑回环"时**成立
//
// 第 ④ 条拆成三个子用例（绑回环 / 绑全网卡 / 经反代），因为改造前的漏洞
// 正是"只看 RemoteAddr 是回环"，而反代从 127.0.0.1 连过来时那条恒成立。
//
// # 为什么每条都要有
//
// 这类缺陷的形态是"**静默放行**"——它不报错、不影响正常使用，
// 只在有人恶意访问时才显形。少一条断言就少一层可被静默绕过的前提。
package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// gateFixture 门禁测试夹具：一个最小 Handler + 两把不同的钥匙。
type gateFixture struct {
	h          *Handler
	apiKey     string
	adminToken string
}

func newGateFixture(t *testing.T, listensLoopback bool) *gateFixture {
	t.Helper()
	const (
		apiKey     = "API-KEY-for-chat-only"
		adminToken = "ADMIN-TOKEN-for-console"
	)
	h := New(Config{
		ListensLoopback: listensLoopback,
		APIKey:          apiKey,
		AdminToken:      adminToken,
	})
	return &gateFixture{h: h, apiKey: apiKey, adminToken: adminToken}
}

// probe 打一个**只读**的管理端点（GET /admin/ui/manifest，不需要任何接线），
// 返回状态码。用它而不是真的去改设置：判据是"能不能进管理面"，
// 而只读端点已经足够表达那件事，且不会产生副作用。
func (f *gateFixture) probe(remoteAddr, bearer string, extraHeaders map[string]string) int {
	req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest", nil)
	req.RemoteAddr = remoteAddr
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec.Code
}

// ① 远程无凭据 ⇒ 拒绝。
func TestGate_RemoteWithoutCredentialDenied(t *testing.T) {
	f := newGateFixture(t, false) // 绑全网卡（暴露）
	if got := f.probe("203.0.113.9:5555", "", nil); got != http.StatusForbidden {
		t.Errorf("远程无凭据应当 403，实际 %d ——\n"+
			"这是本组最基本的判据：未授权不得访问任何管理面。", got)
	}
}

// ② 远程拿 **api_key** ⇒ 仍然拒绝。
//
// ⚠ 这一条是本次修复的**核心**。改造前门禁认的就是 api_key，
// 于是"给某个客户端配一把 key 调对话"等于"把管理台交出去"。
func TestGate_RemoteWithAPIKeyDenied(t *testing.T) {
	f := newGateFixture(t, false)
	if got := f.probe("203.0.113.9:5555", f.apiKey, nil); got != http.StatusForbidden {
		t.Errorf("远程拿 api_key 应当 403，实际 %d ——\n"+
			"api_key 是**对话接口**的凭据（可能发给第三方客户端/机器人），"+
			"它绝不能解锁管理台。改造前它们共用一把钥匙，那正是用户报的"+
			"「没授权却能改设置」的根因之一。", got)
	}
}

// ③ 远程拿 admin_token ⇒ 放行。
func TestGate_RemoteWithAdminTokenAllowed(t *testing.T) {
	f := newGateFixture(t, false)
	if got := f.probe("203.0.113.9:5555", f.adminToken, nil); got != http.StatusOK {
		t.Errorf("远程拿 admin_token 应当放行，实际 %d ——\n"+
			"这是管理台**唯一**的远程入口，它坏了等于没有远程管理面。", got)
	}
}

// ③b `?token=` 查询参数同样可用（浏览器地址栏无法设请求头）。
func TestGate_QueryTokenAccepted(t *testing.T) {
	f := newGateFixture(t, false)
	req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest?token="+f.adminToken, nil)
	req.RemoteAddr = "203.0.113.9:5555"
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("?token=<admin_token> 应当放行，实际 %d ——\n"+
			"浏览器地址栏没办法自定义请求头，查询参数是它唯一能带凭据的地方。", rec.Code)
	}
}

// ④a 绑回环 + 来自回环 ⇒ 免令牌放行。
func TestGate_LoopbackExemptWhenBoundToLoopback(t *testing.T) {
	f := newGateFixture(t, true)
	if got := f.probe("127.0.0.1:5555", "", nil); got != http.StatusOK {
		t.Errorf("只绑回环时的本机访问应当免令牌放行，实际 %d ——\n"+
			"这是用户明确要的便利（本机不该手填钥匙）。", got)
	}
}

// ④b 绑**全网卡** ⇒ 即使来自回环也**不**免令牌。
//
// ⚠ 这条是本修复的关键一环。绑 0.0.0.0 时"本机"这个前提不成立：
// 端口对所有人开放，而来源地址只说明"上一跳"。
func TestGate_LoopbackNotExemptWhenExposed(t *testing.T) {
	f := newGateFixture(t, false) // 绑 0.0.0.0:7863
	if got := f.probe("127.0.0.1:5555", "", nil); got != http.StatusForbidden {
		t.Errorf("绑全网卡时来自回环的请求应当**仍然要求令牌**，实际 %d ——\n"+
			"绑 0.0.0.0 意味着端口对外公开，此时「本机」这个豁免不成立。\n"+
			"（Docker 端口映射、本机反代都会让远程请求以 127.0.0.1 到达。）", got)
	}
}

// ④c 带反代转发头 ⇒ 不免令牌（即使绑回环 + 来源回环）。
//
// ⚠ 这条专治**本机反代**：nginx/caddy/frp 从 127.0.0.1 连过来时，
// RemoteAddr 恒为回环。改造前它因此被判成"本机"，管理台对全网敞开。
func TestGate_ForwardedRequestNotExempt(t *testing.T) {
	f := newGateFixture(t, true) // 绑回环（最容易出错的那种配置）
	for _, hdr := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded"} {
		if got := f.probe("127.0.0.1:5555", "", map[string]string{hdr: "203.0.113.9"}); got != http.StatusForbidden {
			t.Errorf("带 %s 的请求应当要求令牌，实际 %d ——\n"+
				"这个头说明链路上有反向代理，我们看到的来源是**代理**而不是真实客户端，"+
				"所以「本机直连」这个前提不成立。", hdr, got)
		}
	}
}

// 未配 admin_token ⇒ 无论来源一律拒绝（安全默认：没有远程管理面）。
func TestGate_NoAdminTokenConfiguredDeniesAll(t *testing.T) {
	h := New(Config{ListensLoopback: true, APIKey: "some-api-key"})
	for _, addr := range []string{"127.0.0.1:5555", "203.0.113.9:5555"} {
		// 本机仍然走"绑回环 + 来源回环"那条豁免 —— 那是**设计**（本地部署无需令牌）。
		// 远程则必须拒绝：它没有可用的令牌。
		want := http.StatusOK
		if addr != "127.0.0.1:5555" {
			want = http.StatusForbidden
		}
		req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("未配 admin_token 时 %s 应当 %d，实际 %d ——\n"+
				"没配令牌就**没有远程管理面**（安全默认），"+
				"而不是回落到用 api_key（那正是要修掉的权限倒挂）。", addr, want, rec.Code)
		}
	}
	// 反向：拿 api_key 冒充门禁令牌也要被拒（不能有隐式回落）。
	req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Authorization", "Bearer some-api-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("未配 admin_token 时拿 api_key 应当 403，实际 %d —— "+
			"任何「api_key 能当门禁用」的隐式回落都是权限倒挂的复活。", rec.Code)
	}
}

// ⚠ 轮换门禁令牌后**立即生效**（用户报障：轮换之后还要重启才有用）。
//
// 根因是内存里有两份副本：server.Handler.apiKey（有 SetAPIKey）与
// admin.Handler.cfg.APIKey（构造期快照）。门禁读后者 ⇒ 永不更新。
// 现在门禁读 SetAdminToken 维护的可变字段 —— 这条钉住那个行为。
func TestGate_RotationTakesEffectImmediately(t *testing.T) {
	f := newGateFixture(t, false)
	const newToken = "ROTATED-ADMIN-TOKEN"

	if got := f.probe("203.0.113.9:5555", newToken, nil); got != http.StatusForbidden {
		t.Fatalf("前置条件不成立：新令牌在轮换前不该能用（实际 %d）", got)
	}
	f.h.SetAdminToken(newToken)
	if got := f.probe("203.0.113.9:5555", newToken, nil); got != http.StatusOK {
		t.Errorf("轮换后新令牌应当**立即**可用，实际 %d ——\n"+
			"这正是用户报的「轮换之后还需要重启整个项目才有用」：\n"+
			"门禁若读构造期快照（cfg.AdminToken），SetAdminToken 就白调了。", got)
	}
	if got := f.probe("203.0.113.9:5555", f.adminToken, nil); got != http.StatusForbidden {
		t.Errorf("轮换后**旧**令牌应当立即失效，实际 %d ——\n"+
			"旧令牌还能进意味着「轮换」没有真正吊销凭据。", got)
	}
}

// 门禁拒绝时返回的提示必须**可执行**（指出该配哪个字段），而不是空话。
func TestGate_DenyHintIsActionable(t *testing.T) {
	f := newGateFixture(t, false)
	req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Header.Set("Authorization", "Bearer "+f.apiKey) // 拿错钥匙
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("应当 403，实际 %d", rec.Code)
	}
	for _, want := range []string{"admin_token", "api_key"} {
		if !containsStr(body, want) {
			t.Errorf("拒绝响应里应当说明「%s」这个字段（用户才知道去哪配），实际: %s", want, body)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
