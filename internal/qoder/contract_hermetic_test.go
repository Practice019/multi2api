// contract_hermetic_test.go 契约测试与假上游 —— **判据 2 的 enforcement**。
//
// 全程 hermetic：设备码轮询不需要真实账号。
//
// ⚠ 加密推理需要 WASM 签名器，测试用一个**桩签名器**（stubSigner）替代 ——
// 这样网络层与协议层的判据可以独立验证，不必等 WASM 移植完成。
package qoder

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

func fixtureJWT(expUnix int64) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expUnix)))
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

// stubSigner 桩签名器：把原 body 原样当 payload，附一个假的签名头。
//
// ⚠ 它刻意**不**做加密 —— 本文件验证的是网络层与协议层行为；
// 加密本身由 wasm 层的测试覆盖。
type stubSigner struct {
	head string
	path string
}

func (s *stubSigner) BuildInferRequest(id SignIdentity, model string, body []byte) (string, []byte, map[string]string, error) {
	if model == "" {
		return "", nil, nil, fmt.Errorf("stub: 缺 model")
	}
	if id.UID == "" {
		// 真实实现靠 uid 算鉴权字段；桩也把这条判据带上，
		// 好让"上游忘了传身份"这类缺陷在桩测试里也暴露。
		return "", nil, nil, fmt.Errorf("stub: 缺账号身份（uid）")
	}
	head := s.head
	if head == "" {
		head = "sig-stub"
	}
	return s.path, body, map[string]string{
		"X-Signature": head,
		"X-Model":     model,
		"X-UID":       id.UID,
	}, nil
}

// fakeUpstream 假 Qoder 上游。
func fakeUpstream(t *testing.T, pollFirst404 int) (*httptest.Server, *fakeCalls) {
	t.Helper()
	calls := &fakeCalls{}
	exp := time.Now().Add(3 * time.Hour).Unix()
	token := fixtureJWT(exp)
	polls := int32(0)

	mux := http.NewServeMux()

	// 设备码轮询：**GET**，参数在 query（challenge/verifier/nonce）。
	//
	// ⚠ 本轮之前这里断言的是 POST + JSON body 里的 client_id ——
	// 那是**我们自己的错误形状**，于是假上游把缺陷当正确行为"验证"通过。
	// 现在按参照实现（qoder-oauth.ts，已实测跑通）断言：
	//
	//	method=GET、verifier 与 challenge 配对、nonce 存在
	//
	// 任何一项不对就回 400 —— 让"形状写错"在单测里立刻暴露。
	mux.HandleFunc(devicePollPath, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&polls, 1)
		atomic.AddInt32(&calls.poll, 1)
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"poll must be GET"}`))
			return
		}
		q := r.URL.Query()
		verifier := q.Get("verifier")
		nonce := q.Get("nonce")
		if verifier == "" || nonce == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"verifier and nonce required"}`))
			return
		}
		if q.Get("challenge_method") != "S256" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"challenge_method must be S256"}`))
			return
		}
		if int(n) <= pollFirst404 {
			// ⚠ 404 = 尚未授权（**不是错误**）
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"session not ready"}`))
			return
		}
		// ⚠ 用 `token`（登录响应的字段名），不是 access_token ——
		// 参照的 parseQoderTokenPayload 读 token/device_token/access_token，
		// 我们此前只认 access_token，导致登录响应永远解析不出 token。
		_, _ = w.Write([]byte(`{"code":0,"message":"","token":"` + token + `","refresh_token":"rt-1","user_id":"u-1","user_name":"Qoder用户"}`))
	})

	// 续期：**必须带 machine_id**
	mux.HandleFunc(refreshPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.refresh, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if rt, _ := body["refresh_token"].(string); rt == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"refresh_token required"}`))
			return
		}
		if rt, _ := body["refresh_token"].(string); rt == "rt-expired" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"expired"}`))
			return
		}
		// ⚠ machine_id 必须存在（Qoder 特有要求）
		if mid, _ := body["machine_id"].(string); mid == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"machine_id required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"` + fixtureJWT(time.Now().Add(6*time.Hour).Unix()) + `","refresh_token":"rt-2","expires_in":10800}}`))
	})

	// 用户信息
	mux.HandleFunc(userInfoPath, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"u-1","nickname":"Qoder用户"}}`))
	})

	// 加密推理端点
	mux.HandleFunc(encryptedInferPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.chat, 1)
		// ⚠ 签名头必须原样透传（不能被 Bearer 覆盖）
		if r.Header.Get("X-Signature") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"missing signature header"}`))
			return
		}
		if r.Header.Get("X-Model") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"missing model"}`))
			return
		}
		// ⚠ 账号身份必须传到签名器（真实实现靠它算鉴权字段与 Cosy-User）。
		// 这条钉住的是**接缝本身**：请求走到网络层时，用的必须是
		// "这次选中的那个账号"的身份，而不是空值或别的账号 ——
		// 传错身份会把请求发到别人的账号上（最严重的一类缺陷）。
		if r.Header.Get("X-UID") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"message":"missing uid"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"成功\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, calls
}

type fakeCalls struct {
	poll    int32
	refresh int32
	chat    int32
}

func newContractProvider(srv *httptest.Server) gateway.Provider {
	c := NewWithBase(srv.URL)
	c.Signer = &stubSigner{}
	return NewWithConfig(Config{Client: c})
}

func newContractCredential(uid string) gateway.Credential {
	a := &Auth{
		AccessToken:  fixtureJWT(time.Now().Add(3 * time.Hour).Unix()),
		RefreshToken: "rt-1",
		UID:          uid,
		Nickname:     "Qoder用户",
		MachineID:    "machine-1",
		ProductID:    providerID,
	}
	return gateway.Credential{Provider: providerID, UID: uid, Nickname: a.Nickname, Secret: a}
}

// TestContract 契约测试 —— **判据 2 的正式入口**。
func TestContract(t *testing.T) {
	srv, _ := fakeUpstream(t, 0)
	gateway.RunProviderContract(t, func() gateway.Provider {
		return newContractProvider(srv)
	}, gateway.WithCredential(newContractCredential("u-1")))
}

// TestContractIsNotConditionallySkipped 契约必须无凭证也跑。
func TestContractIsNotConditionallySkipped(t *testing.T) {
	t.Setenv("QODER_ACCESS_TOKEN", "")
	t.Setenv("QODER_TEST_CRED", "")

	srv, _ := fakeUpstream(t, 0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gateway.RunProviderContract(t, func() gateway.Provider {
			return newContractProvider(srv)
		}, gateway.WithCredential(newContractCredential("u-1")))
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("契约在无凭证环境下未完成 —— CI 里会变成永远挂住")
	}
}

// ── 逐条钉住本 provider 特有的硬判据 ────────────────────────────────────

// TestPoll404MeansPending 404 表示尚未授权，必须继续轮询。
//
// ⚠ 这是本 provider 最容易做错的一处：按状态码判失败会把
// "用户还没点授权"误报成登录失败。
func TestPoll404MeansPending(t *testing.T) {
	srv, calls := fakeUpstream(t, 1) // 第一次 404
	c := NewWithBase(srv.URL)

	// ⚠ 本轮起轮询收的是**设备会话**（PKCE + nonce + machine_id），
	// 不再是裸 state —— 因为服务端要靠 verifier 校验 challenge。
	sess, err := newDeviceSession()
	if err != nil {
		t.Fatalf("生成设备会话失败: %v", err)
	}
	res, perr := c.PollDeviceToken(context.Background(), &sess)
	if perr != nil {
		t.Fatalf("404 不该是错误: %v", perr)
	}
	if !res.Pending {
		t.Error("404 必须解成 Pending=true")
	}
	if atomic.LoadInt32(&calls.poll) != 1 {
		t.Error("应发 1 次轮询")
	}
}

// TestLoginFlowEndToEnd 登录端到端（含 404 轮询与 machine_id 生成）。
func TestLoginFlowEndToEnd(t *testing.T) {
	srv, calls := fakeUpstream(t, 1)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	lf, ok := p.LoginFlow()
	if !ok {
		t.Fatal("必须实现 LoginFlow")
	}
	state, url, err := lf.Start()
	if err != nil {
		t.Fatalf("Start 报错: %v", err)
	}
	if !strings.Contains(url, deviceSelectPath) {
		t.Errorf("授权 URL 应含 %s，实际 %s", deviceSelectPath, url)
	}
	if !strings.Contains(url, "client_id="+Qoder.ClientID) {
		t.Errorf("授权 URL 应含 prod client_id: %s", url)
	}

	var cred gateway.Credential
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		c, perr := lf.Poll(state)
		if perr == nil {
			cred = c
			break
		}
		if perr != gateway.ErrLoginPending {
			t.Fatalf("轮询报错: %v", perr)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if cred.UID == "" {
		t.Fatalf("25 秒内未拿到凭证（poll 次数=%d）", atomic.LoadInt32(&calls.poll))
	}
	// ⚠ Secret 是 authFile 包装（核心落盘要求它实现 MarshalAuthFile）。
	// 要断言的字段在对内的 *Auth 上，所以这里解一层。
	// 包装存在的理由见 login.go 的 authFile 注释。
	af, ok := cred.Secret.(*authFile)
	if !ok || af == nil || af.a == nil {
		t.Fatalf("Secret 类型: %T（want *authFile，且内含非空 *Auth）", cred.Secret)
	}
	a := af.a
	// ⚠ machine_id 必须生成并进凭据（续期要用）
	if a.MachineID == "" {
		t.Error("登录后必须生成 machine_id（续期请求体要用它）")
	}
	if a.RefreshToken != "rt-1" {
		t.Errorf("refresh_token = %q, want rt-1", a.RefreshToken)
	}
	if a.ProductID != providerID {
		t.Errorf("product_id = %q, want %q", a.ProductID, providerID)
	}
}

// TestRefreshRequiresMachineID 续期必须带 machine_id（Qoder 特有）。
//
// ⚠ 假上游会拒绝不带 machine_id 的续期，所以这条测得出。
func TestRefreshRequiresMachineID(t *testing.T) {
	srv, calls := fakeUpstream(t, 0)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	cred := newContractCredential("u-1")
	a := cred.Secret.(*Auth)

	if err := p.RefreshCredential(cred); err != nil {
		t.Fatalf("续期失败（machine_id 必须带上）: %v", err)
	}
	if atomic.LoadInt32(&calls.refresh) != 1 {
		t.Fatal("应发 1 次续期")
	}
	if a.RefreshToken != "rt-2" {
		t.Errorf("refresh_token 应被更新为 rt-2，实际 %q", a.RefreshToken)
	}
	// machine_id 必须保留
	if a.MachineID != "machine-1" {
		t.Errorf("machine_id 必须保留，实际 %q", a.MachineID)
	}
}

// TestRefreshExpiredIsSentinel HTTP 401 是续期终态。
func TestRefreshExpiredIsSentinel(t *testing.T) {
	srv, _ := fakeUpstream(t, 0)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	cred := gateway.Credential{Provider: providerID, UID: "u",
		Secret: &Auth{AccessToken: "a", RefreshToken: "rt-expired", MachineID: "m"}}
	err := p.RefreshCredential(cred)
	if err == nil || !strings.Contains(err.Error(), ErrRefreshExpired.Error()) {
		t.Errorf("HTTP 401 应返回终态错误，实际 %v", err)
	}
	cred2 := gateway.Credential{Provider: providerID, UID: "u", Secret: &Auth{AccessToken: "a"}}
	if err := p.RefreshCredential(cred2); err == nil || !strings.Contains(err.Error(), ErrRefreshExpired.Error()) {
		t.Errorf("无 refresh_token 应返回终态错误，实际 %v", err)
	}
}

// TestNoSignerFailsLoudly 无 WASM 签名器必须**明确报错**，不回落公开端点。
//
// ⚠ 回落公开端点会把"加密不可用"伪装成"模型不存在"
// （公开端点不认目录 key，一律 `Unsupported model`），排查方向完全跑偏。
func TestNoSignerFailsLoudly(t *testing.T) {
	srv, calls := fakeUpstream(t, 0)
	c := NewWithBase(srv.URL)
	c.Signer = nil // 刻意不接签名器
	p := NewWithConfig(Config{Client: c})

	_, err := p.Chat(context.Background(), newContractCredential("u-1"), []byte(`{"model":"qfmodel"}`))
	if err == nil {
		t.Fatal("无签名器必须报错")
	}
	if !strings.Contains(err.Error(), "WASM") {
		t.Errorf("错误信息应说明是 WASM 缺失，实际 %q", err.Error())
	}
	if atomic.LoadInt32(&calls.chat) != 0 {
		t.Error("不该发出任何推理请求（更不能回落公开端点）")
	}
}

// TestSignatureHeadersPassedThrough 签名头必须原样透传。
//
// ⚠ 参照项目的警告：用普通 Bearer 覆盖签名头会被判签名无效。
func TestSignatureHeadersPassedThrough(t *testing.T) {
	srv, calls := fakeUpstream(t, 0)
	c := NewWithBase(srv.URL)
	c.Signer = &stubSigner{head: "sig-xyz"}
	p := NewWithConfig(Config{Client: c})

	cs, err := p.Chat(context.Background(), newContractCredential("u-1"),
		[]byte(`{"model":"qfmodel"}`))
	if err != nil {
		t.Fatalf("Chat 报错: %v", err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != 200 {
		t.Fatalf("status=%d（假上游会拒绝缺签名头的请求）", cs.Status)
	}
	if atomic.LoadInt32(&calls.chat) != 1 {
		t.Error("应发 1 次推理请求")
	}
}

// TestDeviceSelectURLShape 授权 URL 形态（PKCE + prod client_id）。
//
// # ⚠ 本条本轮被**改写**：旧版钉的是**缺陷**形状
//
// 旧断言要求 URL 含 `state=` / `client_type=5` / `business_product=cli` /
// `scene=assistant` —— 那是我们**自己发明的**参数（不是参照实现的），
// 而且**完全没有 PKCE**。于是它把"缺 PKCE"这个真实缺陷**保护**成了正确行为：
// 任何人想修都得先让这条测试变红，而红的原因看起来像是"你改坏了 URL"。
//
// 这是本仓最危险的一类测试形态（AGENTS.md 明确警告过"测试主动保护 bug"）。
//
// 现在的判据照抄参照实现（dsh-codearts-auth 的 buildQoderAuthUrl，
// 已实测跑通）：
//
//	challenge + challenge_method=S256 + nonce + machine_id + client_id
func TestDeviceSelectURLShape(t *testing.T) {
	c := New()
	sess, err := newDeviceSession()
	if err != nil {
		t.Fatalf("生成设备会话失败: %v", err)
	}
	u := c.DeviceSelectURL(&sess)
	if !strings.HasPrefix(u, Qoder.AuthBase+deviceSelectPath) {
		t.Errorf("URL 前缀不对: %s", u)
	}
	// PKCE 三件套 + machine_id + prod client_id —— 缺一不可。
	for _, want := range []string{
		"challenge=" + sess.Pkce.Challenge,
		"challenge_method=S256",
		"nonce=" + sess.Nonce,
		"machine_id=" + sess.MachineID,
		"client_id=" + Qoder.ClientID,
	} {
		if !strings.Contains(u, want) {
			t.Errorf("URL 缺 %q: %s", want, u)
		}
	}
	// 反向：旧实现那两个"自己发明的"参数**不该**再出现
	//（state 已被 nonce 取代；client_type 那几个元数据参照里没有）。
	for _, bad := range []string{"state=", "client_type=", "business_product=", "scene="} {
		if strings.Contains(u, bad) {
			t.Errorf("URL 里还有旧实现的 %q（参照实现没有这些参数）: %s", bad, u)
		}
	}
	// challenge 必须**无 padding** —— 带 '=' 会让服务端校验失败。
	if strings.Contains(sess.Pkce.Challenge, "=") {
		t.Errorf("challenge 带了 padding（%q）—— 服务端比对的是无 padding 形态", sess.Pkce.Challenge)
	}
}

// TestDevicePollURLShape 轮询 URL 形态（GET + query 里的 verifier）。
//
// ⚠ 这是本轮缺陷的第二半：我们此前发 **POST + JSON body**，
// 而参照是 **GET + query**。服务端既拿不到 verifier（PKCE 校验失败），
// 方法也不对 —— 表现同样是"授权成功但一直 pending"。
func TestDevicePollURLShape(t *testing.T) {
	c := New()
	sess, err := newDeviceSession()
	if err != nil {
		t.Fatalf("生成设备会话失败: %v", err)
	}
	u := c.DevicePollURL(&sess)
	// ⚠ 挂 openAPIBase（不是 authBase）：qoder.com 的同名路径返回 401。
	if !strings.HasPrefix(u, Qoder.OpenAPIBase+devicePollPath) {
		t.Errorf("轮询 URL 必须挂 openAPIBase: %s", u)
	}
	for _, want := range []string{
		"nonce=" + sess.Nonce,
		"verifier=" + sess.Pkce.Verifier,
		"challenge_method=S256",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("轮询 URL 缺 %q: %s", want, u)
		}
	}
	// verifier 与 challenge 必须是**配对**的（服务端靠它校验）。
	if pkceChallenge(sess.Pkce.Verifier) != sess.Pkce.Challenge {
		t.Error("challenge ≠ sha256(verifier) —— 服务端校验必然失败")
	}
}

// TestPollAcceptsTokenFieldNames 登录响应用 `token`，不是 `access_token`。
//
// # 为什么单独一条
//
// 参照的 parseQoderTokenPayload 接受四种字段名：
//
//	登录响应  token / device_token
//	续期响应  access_token
//
// 我们此前只认 access_token，于是**登录响应永远解析不出 token** ——
// 表现为"授权成功但一直 pending 到超时"，与缺 PKCE 的现象一模一样。
// 两条独立缺陷指向同一个症状，所以必须各自有断言。
func TestPollAcceptsTokenFieldNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"token", `{"token":"tok-1","refresh_token":"ref-1"}`},
		{"device_token", `{"device_token":"tok-2","refresh_token":"ref-2"}`},
		{"access_token", `{"access_token":"tok-3","refresh_token":"ref-3"}`},
		{"data.token", `{"data":{"token":"tok-4","refresh_token":"ref-4"}}`},
		{"data.access_token", `{"data":{"access_token":"tok-5","refresh_token":"ref-5"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewWithBase(srv.URL)
			sess, err := newDeviceSession()
			if err != nil {
				t.Fatalf("生成设备会话失败: %v", err)
			}
			res, err := c.PollDeviceToken(context.Background(), &sess)
			if err != nil {
				t.Fatalf("轮询报错: %v", err)
			}
			if res.Pending || res.AccessToken == "" {
				t.Errorf("字段 %s 没被认出来（Pending=%v token=%q）—— "+
					"登录响应用的是 token/device_token，只认 access_token 会让登录永远 pending",
					tc.name, res.Pending, res.AccessToken)
			}
		})
	}
}

// TestPollReadsUserID 设备码响应里的 user_id / user_name 必须读出来。
//
// ⚠ 加密推理需要 uid（WASM 用它派生 encrypt_user_info）。漏读会让
// 账号能登录、能列模型，但一对话就挂 —— 是最难查的那类形态。
func TestPollReadsUserID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"tok","refresh_token":"ref","user_id":"u-42","user_name":"阿猫"}`))
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	sess, err := newDeviceSession()
	if err != nil {
		t.Fatalf("生成设备会话失败: %v", err)
	}
	res, err := c.PollDeviceToken(context.Background(), &sess)
	if err != nil {
		t.Fatalf("轮询报错: %v", err)
	}
	if res.UID != "u-42" {
		t.Errorf("UID=%q want u-42 —— 漏读它会让加密推理缺 uid", res.UID)
	}
	if res.Nickname != "阿猫" {
		t.Errorf("Nickname=%q want 阿猫", res.Nickname)
	}
}

// TestModelTablesDifferByProduct 两个产品的模型表**必须不同**。
//
// ⚠ 这是实测差异，不是可选优化：沿用国际版会让 CN 菜单出现
// 5 个 CN 端点根本不认的模型（ultimate/performance/efficient/smodel/cmodel），
// 用户点了就报错；且 CN 独有的 q37fmodel/gm51model 会缺失。
func TestModelTablesDifferByProduct(t *testing.T) {
	intl := ModelsFor(providerID)
	cn := ModelsFor(ProviderIDCN)

	idsOf := func(ms []Model) map[string]Model {
		m := map[string]Model{}
		for _, x := range ms {
			m[x.ID] = x
		}
		return m
	}
	i, c := idsOf(intl), idsOf(cn)

	// CN 不该有这 5 个（国际版独有）
	for _, id := range []string{"ultimate", "performance", "efficient", "smodel", "cmodel"} {
		if _, has := c[id]; has {
			t.Errorf("CN 表不该含国际版独有模型 %q（CN 端点不认它，点了就报错）", id)
		}
		if _, has := i[id]; !has {
			t.Errorf("国际版表应含 %q", id)
		}
	}
	// CN 独有
	for _, id := range []string{"q37fmodel", "gm51model"} {
		if _, has := c[id]; !has {
			t.Errorf("CN 表应含 CN 独有模型 %q", id)
		}
		if _, has := i[id]; has {
			t.Errorf("国际版表不该含 CN 独有模型 %q", id)
		}
	}
	// mmodel 展示名不同（CN 是 M2.7，国际版是 M3）
	if c["mmodel"].Name != "MiniMax-M2.7" {
		t.Errorf("CN 的 mmodel 应是 MiniMax-M2.7，实际 %q", c["mmodel"].Name)
	}
	if i["mmodel"].Name != "MiniMax-M3" {
		t.Errorf("国际版的 mmodel 应是 MiniMax-M3，实际 %q", i["mmodel"].Name)
	}
	// 条数不同（17 vs 14）
	if len(intl) != 17 {
		t.Errorf("国际版表应 17 条，实际 %d", len(intl))
	}
	if len(cn) != 14 {
		t.Errorf("CN 表应 14 条，实际 %d", len(cn))
	}
}

// TestFreeModelPriceFactorZeroIsValid priceFactor=0 是**免费**，不是缺失。
func TestFreeModelPriceFactorZeroIsValid(t *testing.T) {
	m, ok := FindModel(providerID, "qfmodel")
	if !ok {
		t.Fatal("国际版表应含 qfmodel")
	}
	if !m.IsFree {
		t.Error("qfmodel 应标记 IsFree")
	}
	// ⚠ 0 是合法值，不能被 `> 0` 之类的过滤丢掉
	if m.PriceFactor != 0 {
		t.Errorf("qfmodel 的 PriceFactor 应为 0（免费），实际 %v", m.PriceFactor)
	}
}

// TestInBandErrorDetection 错误帧必须能被识别（真实缺陷的回归网）。
//
// ⚠ Qoder 用独立的 `event: error` + 顶层 {code,message,type}，
// **不是** OpenAI 的 {error:{message}}。早期解析器只认后者 →
// 错误被静默当成"正常结束、无内容"，UI 表现为「干净地停止、无任何报错」。
func TestInBandErrorDetection(t *testing.T) {
	cases := []struct {
		frame   string
		isError bool
	}{
		{"event: error\ndata: {\"code\":500,\"message\":\"boom\",\"type\":\"internal\"}", true},
		{"{\"code\":400,\"message\":\"bad request\",\"type\":\"invalid_request\"}", true},
		{"data: {\"choices\":[{\"delta\":{\"content\":\"正常\"}}]}", false},
		{"data: [DONE]", false},
		// OpenAI 风格不该被误判为 Qoder 错误帧（它由另一条路径处理）
		{"{\"error\":{\"message\":\"x\"}}", false},
	}
	for _, c := range cases {
		got, _ := IsInBandError(c.frame)
		if got != c.isError {
			t.Errorf("IsInBandError(%q) = %v, want %v", c.frame, got, c.isError)
		}
	}
	// 消息抽取
	if _, msg := IsInBandError(`event: error
data: {"code":500,"message":"配额不足","type":"x"}`); !strings.Contains(msg, "配额不足") {
		t.Errorf("应能抽出 message，实际 %q", msg)
	}
}

// TestErrorClassification 错误分类。
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{401, ``, gateway.ErrKindAuth},
		{403, ``, gateway.ErrKindAuth},
		{429, ``, gateway.ErrKindSoftRate},
		{400, ``, gateway.ErrKindClient},
		{500, ``, gateway.ErrKindServer},
		{200, `{"code":500,"message":"insufficient quota"}`, gateway.ErrKindHardCredit},
		{200, `{"code":0,"message":""}`, gateway.ErrKindNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestCredentialRoundTripKeepsMachineID 落盘必须保留 machine_id。
func TestCredentialRoundTripKeepsMachineID(t *testing.T) {
	a := &Auth{
		AccessToken: fixtureJWT(1790412721), RefreshToken: "rt",
		UID: "u-1", Nickname: "Qoder用户",
		MachineID: "machine-xyz", DeviceID: "pc_abc", ProductID: providerID,
	}
	raw, err := MarshalAuthFile(a)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if back.MachineID != a.MachineID {
		t.Errorf("machine_id 必须随凭据持久化（续期要用），want %q got %q",
			a.MachineID, back.MachineID)
	}
	if back.ProductID != providerID {
		t.Errorf("product_id 必须持久化，实际 %q", back.ProductID)
	}
	if back.UIDValue() != "u-1" {
		t.Errorf("uid = %q, want u-1", back.UIDValue())
	}
}

// TestLoadDirFiltersByProduct 两个产品共用目录，靠 product_id 过滤。
func TestLoadDirFiltersByProduct(t *testing.T) {
	dir := t.TempDir()
	writeCred := func(name string, product string) {
		a := &Auth{
			AccessToken:  fixtureJWT(time.Now().Add(time.Hour).Unix()),
			RefreshToken: "rt", UID: name, ProductID: product,
		}
		raw, _ := MarshalAuthFile(a)
		if err := writeFile(dir, "qoder-"+name+".json", raw); err != nil {
			t.Fatal(err)
		}
	}
	writeCred("intl1", providerID)
	writeCred("cn1", ProviderIDCN)

	intl, err := LoadDirFor(dir, providerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(intl) != 1 || intl[0].UIDValue() != "intl1" {
		t.Errorf("国际版应只看到 1 个自己的凭证，实际 %d 个", len(intl))
	}
	cn, err := LoadDirFor(dir, ProviderIDCN)
	if err != nil {
		t.Fatal(err)
	}
	if len(cn) != 1 || cn[0].UIDValue() != "cn1" {
		t.Errorf("CN 应只看到 1 个自己的凭证，实际 %d 个", len(cn))
	}
	all, _ := LoadDir(dir)
	if len(all) != 2 {
		t.Errorf("不过滤时应看到 2 个，实际 %d", len(all))
	}
}

// writeFile 把一个凭证文件写进测试目录。
func writeFile(dir, name string, content []byte) error {
	return os.WriteFile(filepath.Join(dir, name), content, 0o600)
}

// TestProductByID 产品查询。
func TestProductByID(t *testing.T) {
	if p, ok := ProductByID(providerID); !ok || p.ID != providerID {
		t.Error("应能查到国际版")
	}
	if p, ok := ProductByID(ProviderIDCN); !ok || p.ID != ProviderIDCN {
		t.Error("应能查到中国版")
	}
	if _, ok := ProductByID("nope"); ok {
		t.Error("未知 id 应返回 false")
	}
}

// TestProviderDoesNotPanicOnBadCredential 错型凭证返回错误而非 panic。
func TestProviderDoesNotPanicOnBadCredential(t *testing.T) {
	p := NewWithConfig(Config{})
	bad := gateway.Credential{Provider: providerID, UID: "u", Secret: "not-an-auth"}
	if _, err := p.Chat(context.Background(), bad, []byte(`{}`)); err == nil {
		t.Error("错型 Secret 应返回错误")
	}
	// Models 是静态表，不该因凭证问题而失败
	if _, err := p.Models(context.Background(), bad); err != nil {
		t.Errorf("Models 是静态表，不该失败: %v", err)
	}
}
