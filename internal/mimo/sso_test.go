// sso_test.go — SSO 链（passToken→serviceLogin→sts）的假上游闭环测试。
// 复刻用户 2026-09-24 探测报告 §3.3 的实测链：跳数、cookie 要求、续签行为。
package mimo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/gateway"
)

type fakeSSO struct {
	mu       sync.Mutex
	issued   int    // sts 发过几张票（只有最新票能过对话鉴权 = 旧票失效）
	passNow  string // 当前有效 passToken（会被"续签"换新）
	cUserNow string
	nick     string
}

func newFakeSSO(t *testing.T) (*httptest.Server, *fakeSSO) {
	t.Helper()
	f := &fakeSSO{passNow: "V1:pass-initial", cUserNow: "cu-1", nick: "SSO昵称"}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/user/xiaomi/me", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		want := fmt.Sprintf("serviceToken=svc-%d", f.issued)
		f.mu.Unlock()
		if f.issued > 0 && strings.Contains(r.Header.Get("Cookie"), want) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"nickname":"` + f.nick + `"}}`))
			return
		}
		// 未登录 → 302 进 SSO 链（报告 §3.1 第一步）。
		http.Redirect(w, r, "/pass/serviceLogin?sid=mimopc&callback=%2Fapi%2Fsts%3Fsign%3Dabc", http.StatusFound)
	})

	mux.HandleFunc("/pass/serviceLogin", func(w http.ResponseWriter, r *http.Request) {
		ck := r.Header.Get("Cookie")
		f.mu.Lock()
		passOK := strings.Contains(ck, "passToken="+f.passNow)
		f.mu.Unlock()
		// §7 坑复刻：cookie 不齐 → 302 SPA 登录页（对脚本是 200 死路）。
		if !passOK || !strings.Contains(ck, "cUserId=") || !strings.Contains(ck, "pass_ua=pc") || !strings.Contains(ck, "userId=") {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!DOCTYPE html><title>login</title>"))
			return
		}
		// serviceLogin 顺手续签 passToken（30 天滚动 —— 报告 §3.3 关键点）。
		f.mu.Lock()
		f.passNow = f.passNow + "-rotated"
		rotated := f.passNow
		f.mu.Unlock()
		// account 域 Set-Cookie 在假服务器上同 host —— 仍按名收集。
		http.SetCookie(w, &http.Cookie{Name: "passToken", Value: rotated, Path: "/"})
		http.Redirect(w, r, "/api/sts?sign=abc&auth=ticket&_ssign=sig", http.StatusFound)
	})

	mux.HandleFunc("/api/sts", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.issued++
		n := f.issued
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "serviceToken", Value: fmt.Sprintf("svc-%d", n), Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "userId", Value: "777001", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "mimopc_slh", Value: fmt.Sprintf("s%d", n), Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "mimopc_ph", Value: fmt.Sprintf("p%d", n), Path: "/"})
		w.Header().Set("Location", "/api/user/xiaomi/me")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})

	mux.HandleFunc("/api/route/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		ck := r.Header.Get("Cookie")
		f.mu.Lock()
		fresh := f.issued > 0 && strings.Contains(ck, fmt.Sprintf("serviceToken=svc-%d", f.issued))
		f.mu.Unlock()
		if !fresh {
			// 真网关对过期会话票的行为：302 回 SSO（我们的 ingestRouteResp 折算 401）。
			http.Redirect(w, r, "/pass/serviceLogin?sid=mimopc", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"sso答\"}}]}\n\ndata: [DONE]\n\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

func TestSSOFreshChainRotateAndUID(t *testing.T) {
	srv, f := newFakeSSO(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	a := &Auth{Channel: ChannelRoute, Type: TypeAPI,
		PassToken: f.passNow, CUserID: "cu-1", DeviceD: "pc_test",
		UID: "777001"}
	if err := p.client.SSOFresh(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if a.ServiceToken != "svc-1" || a.Slh != "s1" || a.Ph != "p1" {
		t.Fatalf("换票结果不对: %+v", a)
	}
	if !strings.HasSuffix(a.PassToken, "-rotated") {
		t.Errorf("passToken 续签未滚动保存: %q", a.PassToken)
	}
	if a.ServiceAt == 0 {
		t.Error("ServiceAt 未记录")
	}
	if !a.Renewable() {
		t.Error("带 passToken 的 route 应可续")
	}
}

func TestSSOFreshMissingMaterial(t *testing.T) {
	srv, _ := newFakeSSO(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	// 旧式：只有 serviceToken → 不可续，SSOFresh 明确报错不瞎跑。
	a := &Auth{Channel: ChannelRoute, ServiceToken: "svc-old", UID: "u"}
	if err := p.client.SSOFresh(context.Background(), a); err == nil {
		t.Fatal("无 passToken 必须报错")
	}
	if a.Renewable() {
		t.Error("无 passToken 的旧式凭证不该可续")
	}
}

// TestRouteChatSelfHealViaSSO 对话吃 302（折算 401）→ SSO 现换 → 重试成功。
// 这是"以后再也不用管令牌"的端到端形态。
func TestRouteChatSelfHealViaSSO(t *testing.T) {
	srv, f := newFakeSSO(t)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	a := &Auth{Channel: ChannelRoute, Type: TypeAPI, ServiceToken: "svc-0-dead",
		PassToken: f.passNow, CUserID: "cu-1", UID: "777001"}
	cred := routeAuthCred(a)
	body := []byte(`{"model":"mimo-v2.6-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	cs, err := p.Chat(context.Background(), cred, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != 200 {
		t.Fatalf("SSO 自愈失败: %d", cs.Status)
	}
	if a.ServiceToken != "svc-1" {
		t.Errorf("活凭证未原地换新: %q", a.ServiceToken)
	}
}

func TestImportPassOnlyViaSync(t *testing.T) {
	srv, f := newFakeSSO(t)
	dir := t.TempDir()
	p := NewWithConfig(Config{AuthDir: dir, Client: NewWithBase(srv.URL)})
	body, _ := json.Marshal(map[string]string{"cookie": "passToken=" + f.passNow + "; cUserId=cu-1; userId=777001; deviceId=pc_t"})
	req := httptest.NewRequest(http.MethodPost, "/admin/mimo/sync", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.handleSync(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["ok"] != true || out["uid"] != "777001" {
		t.Fatalf("pass-only 同步应现换票进池: %+v", out)
	}
	// 缺 userId 的 passToken 套：按真实规则当场拒绝并指路（而不是进链撞 SPA）。
	body2, _ := json.Marshal(map[string]string{"cookie": "passToken=" + f.passNow + "; cUserId=cu-1"})
	req2 := httptest.NewRequest(http.MethodPost, "/admin/mimo/sync", strings.NewReader(string(body2)))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	p.handleSync(rec2, req2)
	var out2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &out2)
	if out2["ok"] == true || !strings.Contains(fmt.Sprint(out2["error"]), "userId") {
		t.Fatalf("缺 userId 应明确报错指路: %+v", out2)
	}
	list, _ := LoadDir(dir)
	if len(list) != 1 || list[0].PassToken == "" || list[0].ServiceToken == "" {
		t.Fatalf("落盘凭证不完整: %+v", list)
	}
}

func routeAuthCred(a *Auth) gateway.Credential {
	return gateway.Credential{Provider: "mimo", UID: a.UID, Secret: a}
}
