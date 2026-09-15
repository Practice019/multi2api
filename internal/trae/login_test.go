// login_test.go TRAE 页内登录的单元测试（假上游覆盖 ExchangeToken/GetUserInfo）。
package trae

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// freePort 找一个可用的本机端口（登录测试各用自己的回调端口，避免互抢 18080）。
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return strconv.Itoa(port)
}

// TestBuildLoginURL 登录 URL 的参数必须齐全且机器/设备 id 前后一致。
func TestBuildLoginURL(t *testing.T) {
	u := BuildLoginURL("m1", "d1", "t1")
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "www.trae.cn" || parsed.Path != "/authorization" {
		t.Errorf("登录 URL 指向错误: %s", u)
	}
	q := parsed.Query()
	for k, want := range map[string]string{
		"client_id":         ClientID,
		"auth_from":         "solo",
		"auth_callback_url": "http://127.0.0.1:18080/authorize",
		"machine_id":        "m1",
		"device_id":         "d1",
		"x_machine_id":      "m1",
		"x_device_id":       "d1",
		"login_trace_id":    "t1",
		"auth_type":         "local",
	} {
		if q.Get(k) != want {
			t.Errorf("登录 URL 参数 %s = %q，want %q", k, q.Get(k), want)
		}
	}
}

// TestParseCallback 完整回调（URL 编码 JSON 的 userInfo/userJwt）。
func TestParseCallback(t *testing.T) {
	cb := "http://127.0.0.1:18080/authorize?refreshToken=rt1&" +
		"userInfo=" + url.QueryEscape(`{"UserID":"uid1","ScreenName":"nick1","TenantID":"ent1"}`) +
		"&userJwt=" + url.QueryEscape(`{"Token":"at1","RefreshToken":"rt1","TokenExpireAt":1786847930141}`)
	info, err := ParseCallback(cb)
	if err != nil {
		t.Fatal(err)
	}
	if info.RefreshToken != "rt1" || info.Token != "at1" {
		t.Errorf("回调解析错: %+v", info)
	}
	if info.UserID != "uid1" || info.ScreenName != "nick1" || info.TenantID != "ent1" {
		t.Errorf("userInfo 未解析: %+v", info)
	}
}

// TestParseCallbackDoubleEncoded userInfo 被二次编码时也要能解（login.sh 实测形态）。
func TestParseCallbackDoubleEncoded(t *testing.T) {
	inner := url.QueryEscape(`{"UserID":"u2"}`)
	cb := "http://127.0.0.1:18080/authorize?refreshToken=rt2&userInfo=" + url.QueryEscape(inner)
	info, err := ParseCallback(cb)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "u2" {
		t.Errorf("双重编码 userInfo 未解析: %+v", info)
	}
}

// TestParseCallbackMissing 两者皆缺 → 报错。
func TestParseCallbackMissing(t *testing.T) {
	if _, err := ParseCallback("http://127.0.0.1:18080/authorize?foo=1"); err == nil {
		t.Fatal("缺 refreshToken 与 userJwt 应报错")
	}
}

// fakeOAuthServer 假 OAuth 端点（ExchangeToken / GetUserInfo）。
func fakeOAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(EpExchange, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Result":{"Token":"at-new","TokenExpireAt":1786847930141,"RefreshToken":"rt-new"}}`)
	})
	mux.HandleFunc(EpUserInfo, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Result":{"UserID":"uid-final","ScreenName":"nick-final","EnterpriseID":"ent-final"}}`)
	})
	return httptest.NewServer(mux)
}

// TestLoginFlowEndToEnd Start → 生成链接 → finish（回调换 token）→ Poll。
func TestLoginFlowEndToEnd(t *testing.T) {
	oauth := fakeOAuthServer(t)
	p := NewWithConfig(Config{Client: NewWithBase(oauth.URL), CallbackPort: freePort(t)})
	flow, ok := p.LoginFlow()
	if !ok {
		t.Fatal("LoginFlow 必须可用")
	}
	if !flow.Configured() {
		t.Error("Configured 应为 true（任何部署都能加账号）")
	}

	state, authURL, err := flow.Start()
	if err != nil {
		t.Fatal(err)
	}
	if state == "" {
		t.Fatalf("Start 回执异常: state 为空 authURL=%q", authURL)
	}
	// 对齐其它上游：authURL 是**真实 TRAE 登录页**（不是网关表单页）。
	if !strings.Contains(authURL, "www.trae.cn/authorization") {
		t.Errorf("authURL 应是 TRAE 官方登录页: %s", authURL)
	}
	if !strings.Contains(authURL, "machine_id=") || !strings.Contains(authURL, "device_id=") {
		t.Errorf("authURL 应带 machine/device id: %s", authURL)
	}

	// 未完成前 Poll 应 pending。
	if _, err := flow.Poll(state); err != gateway.ErrLoginPending {
		t.Fatalf("未完成时应 pending，得到 %v", err)
	}

	// 用回调链接完成登录（refreshToken 路径 → ExchangeToken → GetUserInfo）。
	cb := "http://127.0.0.1:18080/authorize?refreshToken=rt-cb&" +
		"userInfo=" + url.QueryEscape(`{"UserID":"uid-cb","ScreenName":"cb-nick"}`)
	if _, err := flow.(*loginFlow).finish(state, cb); err != nil {
		t.Fatal(err)
	}

	// Poll 应返回就绪凭证。
	cred, err := flow.Poll(state)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Provider != providerID || cred.UID != "uid-final" || cred.Nickname != "nick-final" {
		t.Errorf("凭证身份不对: %+v", cred)
	}
	af, ok := cred.Secret.(*authFile)
	if !ok || af.a == nil {
		t.Fatalf("Secret 应是 *authFile: %T", cred.Secret)
	}
	if af.a.AccessToken != "at-new" || af.a.RefreshToken != "rt-new" {
		t.Errorf("换来的 token 不对: %+v", af.a)
	}
	if af.a.UID != "uid-final" {
		t.Errorf("uid 应为 GetUserInfo 的 uid-final: %+v", af.a)
	}

	// 落盘形态可回读。
	name, raw, err := af.MarshalAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	if name != "trae-uid-final.json" {
		t.Errorf("文件名 = %q", name)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("落盘内容不是 JSON: %v", err)
	}
	if _, ok := probe["auth"]; !ok {
		t.Error("落盘应为嵌套形（含 auth 键）")
	}
}

// TestLoginFlowNoRefreshToken 回调只有 userJwt.Token → 兜底路径（不调 ExchangeToken）。
func TestLoginFlowNoRefreshToken(t *testing.T) {
	oauth := fakeOAuthServer(t)
	// 假上游只回 GetUserInfo 正常；如果走了 ExchangeToken，Token 会被换掉，
	// 这里断言最后 AccessToken 仍是回调里的 at-only —— 证明没走 ExchangeToken。
	p := NewWithConfig(Config{Client: NewWithBase(oauth.URL), CallbackPort: freePort(t)})
	f, _ := p.LoginFlow()
	state, _, err := f.Start()
	if err != nil {
		t.Fatal(err)
	}
	cb := "http://127.0.0.1:18080/authorize?userJwt=" +
		url.QueryEscape(`{"Token":"at-only","TokenExpireAt":1786847930}`)
	if _, err := f.(*loginFlow).finish(state, cb); err != nil {
		t.Fatal(err)
	}
	cred, err := f.Poll(state)
	if err != nil {
		t.Fatal(err)
	}
	af := cred.Secret.(*authFile)
	if af.a.AccessToken != "at-only" {
		t.Errorf("兜底路径应直接用 userJwt.Token，得到 %q", af.a.AccessToken)
	}
}

// TestLoginFlowFinishTwice 重复 finish 要报错。
func TestLoginFlowFinishTwice(t *testing.T) {
	oauth := fakeOAuthServer(t)
	p := NewWithConfig(Config{Client: NewWithBase(oauth.URL), CallbackPort: freePort(t)})
	f, _ := p.LoginFlow()
	state, _, err := f.Start()
	if err != nil {
		t.Fatal(err)
	}
	cb := "http://127.0.0.1:18080/authorize?refreshToken=rt1"
	if _, err := f.(*loginFlow).finish(state, cb); err != nil {
		t.Fatal(err)
	}
	if _, err := f.(*loginFlow).finish(state, cb); err == nil {
		t.Fatal("重复 finish 应报错")
	}
}
