package codar

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// ─────────────────────────────────────────────────────────────────────────────
// T-364：微信绑定（codar 邀请奖励的第二段 —— 被邀请人绑微信后邀请人才入账）
//
// 上游实测（2026-11-08，主脑 curl 逐字）：
//
//	POST /v1/user/wechat-bound  {username} → 200 {"code":200,"message":"查询成功","success":true,"data":true|false}
//	POST /v1/wechat_agent/bind  {username} → 200 {"code":200,…,"data":{"scene":"e8ef…","qr_url":"https://mp.weixin.qq.com/…"}}
//	                                          已绑 → {"code":400,"message":"用户已绑定微信","success":false,"data":null}  ★ HTTP 仍 200
//	POST /v1/wechat/login/status {scene}   → 200 {"code":200,…,"data":{"status":"pending","token":null,…,"mode":"bind"}}
//	                                          场景失效 → {"code":404,"message":"场景已失效","success":false,"data":null}  ★ HTTP 仍 200
//
// ⚠ 上游**业务码在 body 里、HTTP 恒 200** ⇒ 判失败一律看 `code`，不看 HTTP 状态。
//   （T-357 在 inviteViewFor 里踩过同一个坑。）
// ─────────────────────────────────────────────────────────────────────────────

// wechatStubFn 微信三端点的桩：按路径回预设，可注入"已绑定"与"场景失效"。
func wechatStubFn(bound bool, scene string, status string, sceneDead bool) func(string, string, int) (int, string) {
	return func(path, body string, _ int) (int, string) {
		switch path {
		case "/v1/user/wechat-bound":
			if bound {
				return 0, `{"code":200,"message":"查询成功","success":true,"data":true}`
			}
			return 0, `{"code":200,"message":"查询成功","success":true,"data":false}`
		case "/v1/wechat_agent/bind":
			if bound {
				return 0, `{"code":400,"message":"用户已绑定微信","success":false,"data":null}`
			}
			return 0, `{"code":200,"message":"生成成功","success":true,"data":{"scene":"` + scene + `","qr_url":"https://mp.weixin.qq.com/cgi-bin/showqrcode?ticket=TICKET"}}`
		case "/v1/wechat/login/status":
			if sceneDead {
				return 0, `{"code":404,"message":"场景已失效","success":false,"data":null}`
			}
			return 0, `{"code":200,"message":"查询成功","success":true,"data":{"status":"` + status + `","token":null,"username":"user_x","phone":null,"message":null,"mode":"bind"}}`
		default:
			return 0, `{"code":200,"message":"ok","success":true,"data":{}}`
		}
	}
}

func wechatGet(t *testing.T, p *Provider, target string) (int, map[string]any, string) {
	t.Helper()
	w := httptest.NewRecorder()
	// 路由分派：三个 handler 的 URL 前缀不同，按 target 选（等价于 admin 的注册表）
	switch {
	case strings.HasPrefix(target, "/admin/codar/wechat/status"):
		p.handleWechatStatus(w, httptest.NewRequest(http.MethodGet, target, nil))
	case strings.HasPrefix(target, "/admin/codar/wechat-page"):
		p.handleWechatPage(w, httptest.NewRequest(http.MethodGet, target, nil))
	default:
		p.handleWechatQR(w, httptest.NewRequest(http.MethodGet, target, nil))
	}
	body := w.Body.String()
	var got map[string]any
	// ⚠ 不管状态码都尝试解析：502/404 的响应体同样带 ok/error/upstream_message
	//（只在 200 时解析会让"失败用例"读到 nil，把真实的契约问题掩盖掉）。
	if strings.TrimSpace(body) != "" {
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s 响应不是 JSON：%s", target, body)
		}
	}
	return w.Code, got, body
}

// ── seam ①：/admin/codar/wechat/qr 的响应形状（未绑 / 已绑 / 失败） ──

// TestWechatQRUnbound 未绑定 ⇒ 带 scene + qr_url + expires_in=300，bound=false。
func TestWechatQRUnbound(t *testing.T) {
	p := stubInviteUpstreamFn(t, wechatStubFn(false, "e8efae225c4d10d60effe8795faf482d", "pending", false))
	withTwoAccounts(t, p, t.TempDir(), "u1")

	code, got, raw := wechatGet(t, p, "/admin/codar/wechat/qr?uid=u1")
	if code != http.StatusOK {
		t.Fatalf("HTTP = %d，want 200（body=%s）", code, raw)
	}
	for k, want := range map[string]any{
		"ok": true, "uid": "u1", "bound": false,
		"scene":      "e8efae225c4d10d60effe8795faf482d",
		"qr_url":     "https://mp.weixin.qq.com/cgi-bin/showqrcode?ticket=TICKET",
		"expires_in": float64(300),
	} {
		if got[k] != want {
			t.Errorf("%s = %v，want %v", k, got[k], want)
		}
	}
}

// TestWechatQRAlreadyBound 已绑定 ⇒ 不生成二维码（无 scene），bound=true。
func TestWechatQRAlreadyBound(t *testing.T) {
	p := stubInviteUpstreamFn(t, wechatStubFn(true, "SHOULD_NOT_APPEAR", "pending", false))
	withTwoAccounts(t, p, t.TempDir(), "u1")

	code, got, raw := wechatGet(t, p, "/admin/codar/wechat/qr?uid=u1")
	if code != http.StatusOK {
		t.Fatalf("HTTP = %d，want 200（body=%s）", code, raw)
	}
	if got["ok"] != true || got["bound"] != true {
		t.Errorf("ok/bound = %v/%v，want true/true", got["ok"], got["bound"])
	}
	if _, has := got["scene"]; has {
		t.Errorf("已绑定不该带 scene：%v", got["scene"])
	}
	if strings.Contains(raw, "SHOULD_NOT_APPEAR") {
		t.Error("已绑定却仍然向上游要了二维码")
	}
}

// TestWechatQRFailureIs502 上游业务失败（code!=200）⇒ HTTP 502 + upstream_message。
//
// 上游已绑定时会回 {"code":400,"message":"用户已绑定微信"} 而 **HTTP 仍 200**
// ⇒ 必须靠 body 的 code 判断，否则会把"失败"当成"生成了二维码"。
func TestWechatQRFailureIs502(t *testing.T) {
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		if path == "/v1/user/wechat-bound" {
			return 0, `{"code":200,"message":"查询成功","success":true,"data":false}`
		}
		return 0, `{"code":400,"message":"用户已绑定微信","success":false,"data":null}`
	})
	withTwoAccounts(t, p, t.TempDir(), "u1")

	code, got, raw := wechatGet(t, p, "/admin/codar/wechat/qr?uid=u1")
	if code != http.StatusBadGateway {
		t.Fatalf("HTTP = %d，want 502（body=%s）", code, raw)
	}
	if got["ok"] != false {
		t.Errorf("ok = %v，want false", got["ok"])
	}
	if got["upstream_message"] != "用户已绑定微信" {
		t.Errorf("upstream_message = %v，want 用户已绑定微信", got["upstream_message"])
	}
	if _, has := got["error"]; !has {
		t.Error("缺 error 字段")
	}
}

// TestWechatQRUnknownUIDIs404 uid 不存在 ⇒ 404（不是 502，也不是空二维码）。
func TestWechatQRUnknownUIDIs404(t *testing.T) {
	// 白名单桩：ghost 不在名单 ⇒ pickAuth 失败（生产环境的 credSrc 就是这样）
	p := stubInviteUpstreamFnFor(t, []string{"u1"}, wechatStubFn(false, "S", "pending", false))
	withTwoAccounts(t, p, t.TempDir(), "u1")

	code, got, raw := wechatGet(t, p, "/admin/codar/wechat/qr?uid=ghost")
	if code != http.StatusNotFound {
		t.Fatalf("HTTP = %d，want 404（body=%s）", code, raw)
	}
	if got["ok"] != false {
		t.Errorf("ok = %v，want false", got["ok"])
	}
	if s, _ := got["error"].(string); !strings.Contains(s, "ghost") {
		t.Errorf("error 应提到 uid：%v", got["error"])
	}
}

// ── seam ②：/admin/codar/wechat/status 的状态归一化 ──

// TestWechatStatusNormalization 归一化映射（期望值是**手写字面量**，不是拿实现算的）。
func TestWechatStatusNormalization(t *testing.T) {
	cases := []struct {
		upstream string
		want     string
		expired  bool
		dead     bool
	}{
		{"pending", "pending", false, false}, // 小写原样 → pending
		{"SCANNED", "SCANNED", false, false}, // 已扫码
		{"scanned", "SCANNED", false, false}, // 大小写不敏感
		{"SUCCESS", "SUCCESS", false, false},
		{"completed", "SUCCESS", false, false}, // COMPLETED 也算成功
		{"FAILED", "FAILED", false, false},
		{"EXPIRED", "EXPIRED", true, false},
		{"whatever", "pending", false, false}, // 未知一律 pending
		{"", "pending", false, true},          // 场景失效（code=404）→ EXPIRED
	}
	for _, c := range cases {
		t.Run(c.upstream+"|dead="+boolStr(c.dead), func(t *testing.T) {
			p := stubInviteUpstreamFn(t, wechatStubFn(false, "S", c.upstream, c.dead))
			withTwoAccounts(t, p, t.TempDir(), "u1")

			code, got, raw := wechatGet(t, p, "/admin/codar/wechat/status?scene=SCENE1")
			if code != http.StatusOK {
				t.Fatalf("HTTP = %d，want 200（场景失效也应当是 200；body=%s）", code, raw)
			}
			wantStatus := c.want
			wantExpired := c.expired
			if c.dead {
				wantStatus, wantExpired = "EXPIRED", true
			}
			if got["status"] != wantStatus {
				t.Errorf("status = %v，want %v", got["status"], wantStatus)
			}
			if got["expired"] != wantExpired {
				t.Errorf("expired = %v，want %v", got["expired"], wantExpired)
			}
			if got["ok"] != true {
				t.Errorf("ok = %v，want true（场景失效不是错误）", got["ok"])
			}
			// 场景失效时上游 data=null ⇒ 没有 mode 可谈（只对正常档断言）
			if !c.dead && got["mode"] != "bind" {
				t.Errorf("mode = %v，want bind", got["mode"])
			}
		})
	}
}

// TestWechatStatusSuccessFillsBound 变 SUCCESS 时顺手查一次 wechat-bound，把 bound 填准。
func TestWechatStatusSuccessFillsBound(t *testing.T) {
	p := stubInviteUpstreamFn(t, wechatStubFn(true, "S", "SUCCESS", false))
	withTwoAccounts(t, p, t.TempDir(), "user_x") // 桩里 status 的 username 是 user_x

	_, got, raw := wechatGet(t, p, "/admin/codar/wechat/status?scene=SCENE1")
	if got["status"] != "SUCCESS" {
		t.Fatalf("status = %v，want SUCCESS（body=%s）", got["status"], raw)
	}
	if got["bound"] != true {
		t.Errorf("bound = %v，want true（SUCCESS 后应回查 wechat-bound）", got["bound"])
	}
}

// TestWechatStatusUpstreamTransportErrorIs502 传输失败 ⇒ 502（与"场景失效 200"区分开）。
func TestWechatStatusUpstreamTransportErrorIs502(t *testing.T) {
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		return http.StatusInternalServerError, `boom`
	})
	withTwoAccounts(t, p, t.TempDir(), "u1")

	code, got, raw := wechatGet(t, p, "/admin/codar/wechat/status?scene=SCENE1")
	if code != http.StatusBadGateway {
		t.Fatalf("HTTP = %d，want 502（body=%s）", code, raw)
	}
	if got["ok"] != false {
		t.Errorf("ok = %v，want false", got["ok"])
	}
}

// boolStr 小工具（测试可读性）：t.Run 的用例名里显示 bool。
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// 明确：本文件不 import gateway 之外的核心包；handler 走的是包内直调（与 T-357 同手法）。
var _ = gateway.Credential{}
