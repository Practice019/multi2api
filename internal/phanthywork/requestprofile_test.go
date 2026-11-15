package phanthywork

// requestprofile_test.go `RequestProfile`（发往上游的请求形态）的行为判据。
//
// # 这一组判据在守护什么
//
// 2026-11-15 的方向性决策：**不再复刻官方 sidecar（设备注册 + 推理令牌 + 签名）**，
// 改为第三方 CLI `phanthy-code-launcher` 那条路 —— 只拿 Bearer 发推理。
//
// 依据：一个号只做了 register + summary + 两次 refresh、**零推理**，
// 一小时后照样被封（docs/reports/pw-new-account-observation-20261115.md）。
// ⇒ "复刻官方签名"没有解决问题。
//
// 所以本文件的判据要回答一个**很硬**的问题：
//
//	在 relay 形态下，Chat() 这一步**到底有没有**偷偷调 /api/oauth/*？
//
// 光"看代码里没调"不算证据 —— 那正是 13:44 那次改错的同一条缝
// （判据测的是函数本身，不是真实发出的请求）。
// ⇒ 所以下面的集成判据是**起一个 httptest 服务端，真的跑一遍 Chat，
//    再检查它收到了几个请求、分别是什么路径、带了什么头**。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ① 形态解析 ---------------------------------------------------------------

func TestParseRequestProfile(t *testing.T) {
	cases := []struct {
		in   string
		want RequestProfile
		why  string
	}{
		{"", ProfileKernel, "空 = 默认 kernel"},
		{"  RELAY  ", ProfileRelay, "大小写与空白无关"},
		{"relay", ProfileRelay, ""},
		{"kernel", ProfileKernel, ""},
		{"bare", ProfileBare, ""},
		{"pcl", ProfilePCL, ""},
		{"signature", ProfileSignature, ""},
		{"Signature", ProfileSignature, "大小写无关"},
		// ★ 未知值**必须**回落 relay，不能回落 signature。
		// 回落 signature 会意外打开设备注册与签名 —— 那正是我们想关掉的东西。
		{"garbage", ProfileKernel, "未知值回落 kernel（不是 signature）"},
		{"sig", ProfileKernel, "不做前缀匹配"},
	}
	for _, c := range cases {
		if got := ParseRequestProfile(c.in); got != c.want {
			t.Errorf("ParseRequestProfile(%q) = %q，want %q %s", c.in, got, c.want, c.why)
		}
	}
}

func TestRequestProfileDefaultsToKernel(t *testing.T) {
	p := NewProvider(Config{})
	if got := p.RequestProfile(); got != ProfileKernel {
		t.Fatalf("RequestProfile() 默认 = %q，want %q ——\n"+
			"默认必须是 kernel：它是**官方终端 CLI 的精确形态**（证据最强的那一档，"+
			"见 requestprofile.go 的真源）；signature 那套（设备注册+签名）已被证明解决不了封禁", got, ProfileKernel)
	}
	// 零值也要回落 relay，不能回落 signature。
	var zero RequestProfile
	if got := zero.String(); got != string(ProfileKernel) {
		t.Errorf("零值 String() = %q，want %q", got, ProfileKernel)
	}
}

func TestRequestProfileMatrix(t *testing.T) {
	cases := []struct {
		p        RequestProfile
		sig      bool
		device   bool
		pclIdent bool
		// kernel 的身份头由 SendsKernelIdentity 单独判，不在这张表里
	}{
		{ProfileSignature, true, true, false},
		{ProfileKernel, false, true, false},
		{ProfilePCL, false, true, true},
		{ProfileRelay, false, true, false},
		{ProfileBare, false, false, false},
	}
	for _, c := range cases {
		if got := c.p.NeedsSignature(); got != c.sig {
			t.Errorf("%s.NeedsSignature() = %v，want %v", c.p, got, c.sig)
		}
		if got := c.p.SendsDeviceHeader(); got != c.device {
			t.Errorf("%s.SendsDeviceHeader() = %v，want %v", c.p, got, c.device)
		}
		if got := c.p.SendsPCLIdentity(); got != c.pclIdent {
			t.Errorf("%s.SendsPCLIdentity() = %v，want %v", c.p, got, c.pclIdent)
		}
	}
}

// ② relay 形态真实发出的请求 ----------------------------------------------

// TestRelayProfileHeaderMatrix 逐形态检查**真实发出去的头**。
//
// 走的是 `ChatRelayStream` 本身（不是直接调某个 header helper）——
// 这是有意的：13:44 那次改错的教训是"判据测函数、不测真实请求"。
func TestRelayProfileHeaderMatrix(t *testing.T) {
	type rec struct {
		path   string
		header http.Header
	}
	var mu sync.Mutex
	var got []rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, rec{path: r.URL.Path, header: r.Header.Clone()})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := newTestAuth("u-relay", "rt-relay")

	for _, prof := range []RequestProfile{ProfileKernel, ProfileRelay, ProfileBare, ProfilePCL} {
		mu.Lock()
		got = nil
		mu.Unlock()
		if _, err := c.ChatRelayStream(context.Background(), a, pathChatCompletions, []byte(`{}`), prof); err != nil {
			t.Fatalf("%s: ChatRelayStream: %v", prof, err)
		}
		mu.Lock()
		if len(got) != 1 {
			mu.Unlock()
			t.Fatalf("%s: 应当恰好 1 个请求，实际 %d", prof, len(got))
		}
		h := got[0].header
		mu.Unlock()

		// ★★ 所有非签名形态共同的红线：一个证明头都不许有
		for _, forbid := range []string{
			"x-phanthy-proof-signature", "x-phanthy-proof-nonce",
			"x-phanthy-proof-timestamp", "x-phanthy-desktop-session",
			"x-desktop-signature", "x-desktop-nonce", "x-desktop-timestamp",
			"x-desktop-installation-id", "idempotency-key",
			// 官方身份头也不该出现（那是 signature 形态的事）
			"x-app", "x-phanthy-client-version",
		} {
			if v := h.Get(forbid); v != "" {
				t.Errorf("%s: 不该带 %s（实际 %q）——\n"+
					"非签名形态的全部意义就是「不发官方那套东西」", prof, forbid, v)
			}
		}

		// 三个必需的头
		if v := h.Get("Authorization"); !strings.HasPrefix(v, "Bearer ") {
			t.Errorf("%s: Authorization 缺失或形状不对: %q", prof, v)
		}
		if v := h.Get("Content-Type"); v != "application/json" {
			t.Errorf("%s: Content-Type = %q", prof, v)
		}
		if v := h.Get("Accept"); v != "text/event-stream" {
			t.Errorf("%s: Accept = %q", prof, v)
		}
		// UA 永远不能是 Go 的默认值（那是第三种不存在于上游视野的形态）
		if v := h.Get("User-Agent"); v == "" || strings.Contains(v, "Go-http-client") {
			t.Errorf("%s: User-Agent = %q —— Go 默认值是一个上游从未见过的形态", prof, v)
		}

		// 设备指纹：bare 不带，其余带
		dev := h.Get("x-phanthy-cli-device")
		if prof.SendsDeviceHeader() {
			if dev == "" {
				t.Errorf("%s: 缺 x-phanthy-cli-device（该形态声明要带它）", prof)
			}
		} else if dev != "" {
			t.Errorf("%s: 不该带 x-phanthy-cli-device（实际 %q）", prof, dev)
		}

		// 身份头：kernel 与 pcl 各带自己的一套，relay/bare 一个都不带
		src := h.Get("x-phanthy-client-source")
		prod := h.Get("x-phanthy-product")
		if prof.SendsKernelIdentity() {
			if src != "cli" || prod != "phanthy_code" {
				t.Errorf("%s: kernel 身份头不对: client-source=%q product=%q（真源：官方 CLI 的 `ok.clientSource`=cli、`ok.product`=phanthy_code）", prof, src, prod)
			}
			if h.Get("User-Agent") != KernelUA {
				t.Errorf("%s: UA = %q，want %q", prof, h.Get("User-Agent"), KernelUA)
			}
			if h.Get("x-phanthy-session") == "" || h.Get("x-phanthy-request") == "" {
				t.Errorf("%s: 缺 x-phanthy-session / x-phanthy-request（官方 CLI 每次都发）", prof)
			}
		} else if prof.SendsPCLIdentity() {
			if src != "phanthy-code-launcher" || prod != "phanthy_code" {
				t.Errorf("%s: PCL 身份头不对: client-source=%q product=%q", prof, src, prod)
			}
			if !strings.HasPrefix(h.Get("User-Agent"), "phanthy-code-launcher/") {
				t.Errorf("%s: UA 应当是 launcher 的, 实际 %q", prof, h.Get("User-Agent"))
			}
		} else {
			if src != "" || prod != "" {
				t.Errorf("%s: 不该带 PCL 身份头: client-source=%q product=%q", prof, src, prod)
			}
		}
	}
}

// TestRelayProfileSkipsQuotaSummary relay 形态下 RefreshQuota **不得**打 summary。
func TestRelayProfileSkipsQuotaSummary(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	p.SetRequestProfile("relay")
	a := newTestAuth("u-relay-quota", "rt")
	p.SetLocalCredential(a.UID(), a)

	before := atomicLoad(&fake.summaryCalls)
	if _, ok := p.RefreshQuota(a.UID()); ok {
		t.Error("relay 形态不该声称拿到了额度（它刻意不查 summary）")
	}
	if n := atomicLoad(&fake.summaryCalls) - before; n != 0 {
		t.Errorf("relay 形态打了 %d 次 summary ——\n"+
			"summary 是 180 秒推理令牌的唯一来源，也是「零推理仍被封」那次的嫌疑动作之一", n)
	}
	if n := fake.pathCount(pathRegister); n != 0 {
		t.Errorf("relay 形态不该发设备注册，实际 %d 次", n)
	}
}

// TestEnsureReadyRefusesRegistrationInRelay relay 形态下 ensureReady 必须**拒绝**，
// 而不是偷偷注册一次再继续。
func TestEnsureReadyRefusesRegistrationInRelay(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	p.SetRequestProfile("relay")
	a := newTestAuth("u-relay-ready", "rt")

	err := p.ensureReady(context.Background(), a)
	if err == nil {
		t.Fatal("relay 形态下 ensureReady 必须报错（它代表的正是「需要签名」的语义）")
	}
	if !strings.Contains(err.Error(), "不注册设备") {
		t.Errorf("错误信息应当明确说「不注册设备」，实际: %v", err)
	}
	if a.Reg.RegisteredAtMS != 0 {
		t.Error("ensureReady 失败了却把 RegisteredAtMS 写上了")
	}
	if n := fake.pathCount(pathRegister); n != 0 {
		t.Errorf("relay 形态发了 %d 次设备注册", n)
	}
}

// TestChatRelayMakesNoSignedRequests ★★ 本文件最重要的判据（集成级）。
//
// 真的跑一遍 `Chat()`，然后检查假上游收到了几条请求、都是什么路径。
//
// 期望：**只有一条** `POST /v1/chat/completions` —— 没有 register、
// 没有 activities/summary、没有 profile。
//
// 这条判据的价值在于它测的是**真实调用链**：如果将来有人在 relay 分支里
// 又加回 `ensureInferenceToken`（它会先 register 再 summary），这里会立刻红。
func TestChatRelayMakesNoSignedRequests(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		if hdr == nil {
			hdr = r.Header.Clone()
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewWithClient(NewWithBase(srv.URL), t.TempDir(), 0)
	p.SetRequestProfile("") // 空 = relay（默认）

	cred := newChatCredential("u-e2e-relay")
	body := []byte(`{"model":"phanthywork/Phanthy-fast","messages":[{"role":"user","content":"hi"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := p.Chat(ctx, cred, body)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if stream.Body != nil {
		_, _ = io.Copy(io.Discard, stream.Body)
		_ = stream.Body.Close()
	}
	if stream.Status != http.StatusOK {
		t.Fatalf("Status = %d，want 200", stream.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 {
		t.Fatalf("relay 形态下 Chat 应当只发 1 个请求，实际 %d 个: %v\n"+
			"多发的那几条大概率是 register / activities/summary —— 正是要摆脱的东西", len(paths), paths)
	}
	if want := "POST " + pathChatCompletions; paths[0] != want {
		t.Errorf("唯一的请求应当是 %s，实际 %s", want, paths[0])
	}
	// 顺手确认那唯一一条也没带签名头
	for _, forbid := range []string{"x-phanthy-proof-signature", "x-phanthy-desktop-session", "x-desktop-signature"} {
		if v := hdr.Get(forbid); v != "" {
			t.Errorf("relay 形态的推理请求不该带 %s（实际 %q）", forbid, v)
		}
	}
	// 但必须带设备指纹（relay 声明要带）
	if hdr.Get("x-phanthy-cli-device") == "" {
		t.Error("relay 形态的推理请求应当带 x-phanthy-cli-device")
	}
}

// TestChatRelayRejectsMessagesProtocol relay 形态 + messages 协议 ⇒ 明确报错。
//
// 理由：messages 的响应是 Anthropic SSE，核心不认识；那条翻译链路挂在
// 带签名的 chatMessages 上。这里报错，而不是给出一个"能通但下游解析不了"的流。
func TestChatRelayRejectsMessagesProtocol(t *testing.T) {
	p := NewWithClient(NewWithBase("http://127.0.0.1:1"), t.TempDir(), 0)
	p.SetRequestProfile("relay")
	p.SetProtocol("messages")
	_, err := p.Chat(context.Background(), newChatCredential("u-msg"), []byte(`{}`))
	if err == nil {
		t.Fatal("relay + messages 应当报错")
	}
	if !strings.Contains(err.Error(), "不支持 messages 协议") {
		t.Errorf("错误信息应当点明不支持 messages 协议，实际: %v", err)
	}
}

// TestEnsureReadyRegistersInSignatureProfile ★ 上一条的**正向对照**。
//
// # 为什么必须有它
//
// 写 `ensureReady` 的闸门时我漏了一个 `if`，把它写成了**无条件报错** ——
// 签名形态也被一起掐死，39 条既有判据集体变红。
// 而当时 `TestEnsureReadyRefusesRegistrationInRelay` **全绿**：
// 它只断言"relay 下必须报错"，而"无条件报错"正好满足它。
//
// ⇒ 一个只测单边的闸门判据，会在闸门把两边一起关死时保持沉默。
// 这条补上另一边：**signature 形态下必须真的去注册**。
func TestEnsureReadyRegistersInSignatureProfile(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv) // 夹具已显式声明 signature
	if got := p.RequestProfile(); got != ProfileSignature {
		t.Fatalf("夹具前提变了：RequestProfile() = %q，want signature", got)
	}
	a := newTestAuth("u-sig-ready", "rt")
	fake.trust(a)

	if err := p.ensureReady(context.Background(), a); err != nil {
		t.Fatalf("signature 形态下 ensureReady 应当成功，实际: %v", err)
	}
	if a.Reg.RegisteredAtMS == 0 {
		t.Error("signature 形态下注册成功后应当写上 RegisteredAtMS")
	}
	if n := fake.pathCount(pathRegister); n != 1 {
		t.Errorf("signature 形态下应当恰好发 1 次设备注册，实际 %d 次", n)
	}

	// 再来一次：已注册 ⇒ 零请求（不许重复注册，那是坑⑥的设备风控）。
	before := fake.pathCount(pathRegister)
	if err := p.ensureReady(context.Background(), a); err != nil {
		t.Fatalf("已注册后再调 ensureReady 应当成功: %v", err)
	}
	if after := fake.pathCount(pathRegister); after != before {
		t.Errorf("已注册的账号又被注册了一次（%d -> %d）", before, after)
	}
}
