package phanthywork

// chatcompletions_test.go 新版内核协议（POST /v1/chat/completions）的护栏。
//
// # 为什么必须有这个文件
//
// 2026-11-13 之前，本包只实现 Anthropic `/v1/messages` + 12 段 ed25519 签名。
// 而新版 Phanthy Codeo 内核（0.2.13）抓包实证**完全不碰** `/v1/messages`：
//
//	POST /api/event_logging/batch      （遥测）
//	POST /v1/chat/completions          （推理，唯一）
//
// # ★★ 2026-11-14 更正：原第 ②③ 条是错的（方向性错误）
//
// 本文件原先断言：
//
//	② **不签名**：没有 x-desktop-* / x-phanthy-proof-*
//	③ **不换推理令牌**：直接用 access_token 当 Bearer
//
// 那两条来自**只抓到链路的一跳**：
//
//	官方：内核（Phanthy Codeo）→ **本地桥（sidecar, 127.0.0.1）** → 上游
//	抓到：内核 → 本地桥             ← 这一跳确实不签名、不换令牌
//	漏掉：本地桥 → 上游             ← **桥补 12 段签名 + 4 个证明头**
//
// 桥的入口集合逐字包含 chat（`Sd3 = new Set(["/v1/messages","/v1/responses",
// "/v1/chat/completions"])`），且桥的 proof 构造器 `_O8` 与本包 `InferenceCanonical`
// 逐段一致 ⇒ 签名算法早就对了，缺的只是"用在 chat 上"。
//
// ⇒ 现在正确的三条是：
//
//	① 端点：chat，不是 messages
//	② **要签名**：必须带齐 4 个 `x-phanthy-desktop-session` / `x-phanthy-proof-*`
//	③ **要换推理令牌**：先 activities/summary 拿 desktop_inference_token 才能签
//
// 判据 `TestChatSendsNoSignatureHeaders` / `TestChatDoesNotFetchInferenceToken`
// 已分别**反转**为 `TestChatPathCarriesDesktopInferenceProof` /
// `TestChatFetchesInferenceToken` —— 原判据在守护错误行为，是"判据锁死 bug"的又一例。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// chatFakeUpstream 一个只认 /v1/chat/completions 的假上游。
//
// 它会记录收到的请求，供断言检查"我们到底发了什么"。
type chatFakeUpstream struct {
	gotPath    string
	gotHeaders http.Header
	gotBody    []byte
	calls      int
	// streamBody 非空时用它作为 200 的响应体（默认回一段最小 OpenAI SSE）。
	streamBody string
	// status 非 0 时直接回这个状态码（用于错误分类断言）。
	status  int
	errBody string

	// ★ 2026-11-14：chat 路径现在也要换推理令牌（12 段 canonical 第 8 段 + 第 4 个证明头），
	//   而换令牌前要 ensureInstallation ⇒ 这三个端点都要能被假上游应答。
	summaryCalls  int
	gotSummaryHdr http.Header
	gotSummaryKey string
	profileCalls  int
	registerCalls int
	// gotInferToken 记下发出去的推理令牌 —— 判据用它比对
	// `x-phanthy-desktop-session`（官方那里放的就是令牌原值，不是 sessionId）。
	gotInferToken string
}

func (f *chatFakeUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// profile：ensureInstallation 每次注册前都会问一次权威 accountScope。
		if r.URL.Path == pathProfile {
			f.profileCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"account":{"uuid":"scope-u-chat","display_name":"chat-account"}}`)
			return
		}
		// register：chat 路径现在要先 ensureInstallation（换令牌的前置）。
		// 成功标志由 provider 自己记（RegisteredAtMS = 本地时刻），所以回一个
		// **不含 reason_code / error** 的 JSON 即视为成功。
		if r.URL.Path == pathRegister {
			f.registerCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"installation":{"id":"inst-u-chat"}}`)
			return
		}

		// activities/summary：只发推理令牌，不记进 chat 的 calls/gotPath。
		//
		// 真源：官方桥 `_O8` 的 `if(!iy) return null` —— 没有推理令牌就**不发证明头**，
		// 而 chat 路径要带证明头 ⇒ 必须先换令牌。
		if r.URL.Path == pathActivitiesSummary {
			f.summaryCalls++
			f.gotSummaryHdr = r.Header.Clone()
			f.gotSummaryKey = r.Header.Get("idempotency-key")
			_, _ = io.ReadAll(r.Body)
			tok := fakeJWT("u-1", time.Now().Unix()+180)
			f.gotInferToken = tok
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"schema_version":1,"server_time":"`+
				time.Now().UTC().Format(time.RFC3339Nano)+
				`","desktop_inference_token":"`+tok+`"}`)
			return
		}

		f.calls++
		f.gotPath = r.URL.Path
		f.gotHeaders = r.Header.Clone()
		f.gotBody, _ = io.ReadAll(r.Body)

		if f.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status)
			_, _ = io.WriteString(w, f.errBody)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		body := f.streamBody
		if body == "" {
			body = "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好的\"}}]}\n\n" +
				"data: [DONE]\n\n"
		}
		_, _ = io.WriteString(w, body)
	}
}

// newChatCred 造一份"已注册、未过期"的凭证，指向给定 uid。
func newChatCred(t *testing.T, uid string) gateway.Credential {
	t.Helper()
	a := newTestAuth(uid, "refresh-"+uid)
	a.OAuth.AccessToken = "access-" + uid
	return gateway.Credential{UID: uid, Secret: a}
}

// TestChatUsesChatCompletionsPath 断言默认协议打的是 /v1/chat/completions。
//
// 这是本文件的主断言：端点选错，真机上一切都不成立。
func TestChatUsesChatCompletionsPath(t *testing.T) {
	up := &chatFakeUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	p := newFakeProviderChat(t, srv)
	cred := newChatCred(t, "u-chat")

	st, err := p.Chat(context.Background(), cred, mustJSON(t, map[string]any{
		"model":    "phanthywork/Phanthy-fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer st.Body.Close()
	_, _ = io.Copy(io.Discard, st.Body)

	if st.Status != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（body=%s）", st.Status, string(up.errBody))
	}
	if up.gotPath != pathChatCompletions {
		t.Fatalf("端点 = %q, want %q\n"+
			"修法：Chat 必须在 ProtocolChat 下走 pathChatCompletions",
			up.gotPath, pathChatCompletions)
	}
	if up.gotPath == pathMessages {
		t.Fatalf("端点退回了旧版 %q —— 新版内核抓包实证从不使用它", pathMessages)
	}
}

// TestChatPathCarriesDesktopInferenceProof 断言 chat 路径**必须带齐 4 个 proof 头**。
//
// # ★★ 这条判据是**反转**来的，原判据在守护错误行为
//
// 原名 `TestChatSendsNoSignatureHeaders`，断言"chat 路径不带任何签名头"，
// 理由是"抓包实证：客户端全程零签名"。那个实证**只覆盖了链路的一跳**：
//
//	官方：内核（Phanthy Codeo）→ **本地桥（sidecar, 127.0.0.1）** → 上游
//	抓到：内核 → 本地桥        ← 这一跳**确实**零签名（原判据抓到的是这里）
//	漏掉：本地桥 → 上游        ← **桥补 12 段签名 + 这 4 个头**
//
// 真源（官方 sidecar 二进制，逐字）：
//
//	Sd3 = new Set(["/v1/messages", "/v1/responses", "/v1/chat/completions"])
//	for (let x of ["x-phanthy-desktop-session","x-phanthy-proof-signature",
//	               "x-phanthy-proof-nonce","x-phanthy-proof-timestamp"]) G.delete(x);
//	let u = _O8(q.method, K.pathname, $, G); if (u) for (...) G.set(x, m);
//
// 旁证：上游对缺这个头的请求回的正是
//
//	403 {"code":"request_not_authorized","message":"missing desktop session"}
//
// ⇒ 原判据让"少 4 个头"这件事一路绿灯。现在反过来钉住它。
func TestChatPathCarriesDesktopInferenceProof(t *testing.T) {
	up := &chatFakeUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	p := newFakeProviderChat(t, srv)
	cred := newChatCred(t, "u-proof")

	st, err := p.Chat(context.Background(), cred, mustJSON(t, map[string]any{
		"model":    "Phanthy-fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer st.Body.Close()
	_, _ = io.Copy(io.Discard, st.Body)

	// ① 4 个证明头**必须**在
	for _, h := range []string{
		"x-phanthy-desktop-session",
		"x-phanthy-proof-timestamp",
		"x-phanthy-proof-nonce",
		"x-phanthy-proof-signature",
	} {
		if v := up.gotHeaders.Get(h); v == "" {
			t.Errorf("chat 路径缺 proof 头 %q —— 官方本地桥一定会补它，"+
				"缺了上游回 403 request_not_authorized「missing desktop session」", h)
		}
	}

	// ② desktop-session 放的是**推理令牌本身**（不是 sessionId）
	if got, want := up.gotHeaders.Get("x-phanthy-desktop-session"), up.gotInferToken; want != "" && got != want {
		t.Errorf("x-phanthy-desktop-session = %q，want 推理令牌 %q", got, want)
	}

	// ③ 推理路径**不该**带 v1 的 `x-desktop-*`（那 4 个只属于 /api/* 一族）
	for _, h := range []string{
		"x-desktop-installation-id", "x-desktop-timestamp",
		"x-desktop-nonce", "x-desktop-signature",
	} {
		if v := up.gotHeaders.Get(h); v != "" {
			t.Errorf("推理路径不该带 v1 签名头 %q（实际 %q）—— 官方 proof 用的是 "+
				"x-phanthy-proof-*，两套头不能混", h, v)
		}
	}

	// ④ 换过令牌（4 个 proof 头的前置）
	if up.summaryCalls == 0 {
		t.Error("chat 路径没调 activities/summary —— 没有推理令牌就签不出这个包")
	}
}

// TestChatSendsSessionHeaders 断言会话头齐全，且 session 形态正确。
//
// ★★ 2026-11-14 修正：此前这条判据**断言了两个官方不存在的头**
// （`x-phanthy-client: cli` / `x-phanthy-project: global`）——
// 判据把错的行为锁死了，这正是"改错了却一路绿灯"的原因。
//
// 官方 sidecar 2.51.0 的 `/v1/chat/completions` 头构造器里**没有**它们：
//
//	X = () => ({ Authorization, "Content-Type", Accept,
//	             "x-phanthy-client-source": "desktop",
//	             "x-phanthy-cli-device": <指纹>,
//	             "x-claude-code-session-id": sessionId })
//
// 判据改为断言「官方有的必须在、官方没有的必须不在」，见
// TestInferenceHeadersMatchOfficialClient。
func TestChatSendsSessionHeaders(t *testing.T) {
	up := &chatFakeUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	p := newFakeProviderChat(t, srv)
	cred := newChatCred(t, "u-hdr")

	st, err := p.Chat(context.Background(), cred, mustJSON(t, map[string]any{
		"model":    "Phanthy-fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer st.Body.Close()
	_, _ = io.Copy(io.Discard, st.Body)

	// 上游按这两个头认会话/去重；缺了服务端不认这次推理。
	if got := up.gotHeaders.Get("x-claude-code-session-id"); got == "" {
		t.Error("缺 x-claude-code-session-id（官方 sidecar 每次推理都发它，上游按它认会话）")
	}
	reqID := up.gotHeaders.Get("x-request-id")
	if len(reqID) != 36 || strings.Count(reqID, "-") != 4 {
		t.Errorf("x-request-id = %q，want uuid 形态（36 字符 / 4 个连字符）", reqID)
	}

	// session 是 ses_ + 24 位 cuid2（小写字母+数字，首字符字母）
	sess := up.gotHeaders.Get("x-phanthy-session")
	if !strings.HasPrefix(sess, "ses_") {
		t.Fatalf("x-phanthy-session = %q，want 以 ses_ 开头", sess)
	}
	body := strings.TrimPrefix(sess, "ses_")
	if len(body) != 24 {
		t.Fatalf("session 主体 %q 长度 = %d，want 24（cuid2）", body, len(body))
	}
	if c := body[0]; c < 'a' || c > 'z' {
		t.Fatalf("session 首字符 = %q，want 小写字母（cuid2 形态）", string(c))
	}
	for _, c := range body {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
			t.Fatalf("session 主体含非法字符 %q（只允许小写字母+数字；"+
				"用 base64url 会产出 '-'/'_'，形态与真实值不符）", string(c))
		}
	}
}

// TestInferenceHeadersMatchOfficialClient ★ 推理路径的头必须与官方客户端**逐项一致**。
//
// # 为什么这条判据存在（2026-11-14 真机封号）
//
// 改之前新版内核那条路发的是：
//
//	x-phanthy-client: cli            官方 sidecar 里根本不存在这个头
//	x-phanthy-project: global        不存在
//	x-phanthy-product: phanthy_code  不存在
//	x-phanthy-client-source: cli     **应为 desktop**（注册声称 edition=phanthy_work）
//	（且完全没有 x-app / x-phanthy-client-version / x-phanthy-cli-device）
//
// 后果与官方文档记的封号触发点逐条吻合（PROTOCOL-INFERENCE.md §二之二）：
// 「来源=桌面端、OS 为空、版本对不上」的异常组合 ⇒ 风控记账 ⇒ 封号。
// 真机结果就是 403 `disabled_account`。
//
// 真源：官方 sidecar 2.51.0 二进制里 /v1/chat/completions 的头构造器（逐字见
// client.go 的 ChatCompletionsStream 注释）。
//
// ⚠ 这条判据是**双向**的：官方有的少一个不行，官方没有的多一个也不行 ——
//
//	两边都是「异常客户端」特征。
func TestInferenceHeadersMatchOfficialClient(t *testing.T) {
	up := &chatFakeUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	p := newFakeProviderChat(t, srv)
	cred := newChatCred(t, "u-parity")

	st, err := p.Chat(context.Background(), cred, mustJSON(t, map[string]any{
		"model":    "Phanthy-fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer st.Body.Close()
	_, _ = io.Copy(io.Discard, st.Body)

	// ① 官方**必须有**的头（缺一个 ⇒ 异常客户端）
	must := map[string]string{
		// 身份三件套：来源 / 版本 / 设备指纹。
		// 服务端「用量」页的「来源 / OS」两列就由后两个推导。
		"x-phanthy-client-source":  "desktop",
		"x-app":                    "cli",
		"x-phanthy-client-version": OfficialClientVersion,
		// 会话与去重
		"x-claude-code-session-id": "",
		"x-request-id":             "",
	}
	for name, want := range must {
		got := up.gotHeaders.Get(name)
		if got == "" {
			t.Errorf("缺 %s —— 官方客户端每次都发它，缺了就是「异常客户端」特征\n"+
				"  （2026-11-14 真机封号复盘：PROTOCOL-INFERENCE.md §二之二）", name)
			continue
		}
		if want != "" && got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// 设备指纹必须是 base64url(JSON)，且含 platform（官方用它填「OS」列）
	dev := up.gotHeaders.Get("x-phanthy-cli-device")
	if dev == "" {
		t.Error("缺 x-phanthy-cli-device（官方设备指纹头；缺了服务端看到的是「来源=桌面端但 OS 为空」）")
	} else if dec, derr := b64urlDecode(dev); derr != nil {
		t.Errorf("x-phanthy-cli-device 不是合法 base64url: %v", derr)
	} else if s := string(dec); !strings.Contains(s, `"platform"`) || !strings.Contains(s, `"osRelease"`) {
		t.Errorf("设备指纹缺 platform/osRelease 字段: %s", s)
	}

	// ② 官方**没有**的头（多一个 ⇒ 反常特征）。这三个是实测出来的幽灵头。
	forbidden := []string{
		"x-phanthy-client",  // 只有 x-phanthy-client-source，没有这个
		"x-phanthy-project", // 官方 sidecar 字符串里 0 次
		"x-phanthy-product", // 同上
	}
	for _, h := range forbidden {
		if v := up.gotHeaders.Get(h); v != "" {
			t.Errorf("多发了官方不存在的头 %s = %q\n"+
				"  官方 sidecar 2.51.0 的字符串转储里这个头出现 0 次 ——\n"+
				"  多一个官方没有的头，就是风控眼里的「异常客户端」。", h, v)
		}
	}
}

// TestChatFetchesInferenceToken 断言 chat 路径**必须**调 activities/summary 换令牌。
//
// # ★★ 这条判据是**反转**来的，原判据在守护错误行为
//
// 原名 `TestChatDoesNotFetchInferenceToken`，断言"chat 不调 summary、直接用
// access_token 当 Bearer、少一个失败点"。那个结论来自只抓到链路的一跳
// （内核 → 本地桥），而**桥 → 上游**那一跳会补 12 段签名，
// 而 canonical 的第 8 段就是 `sha256(desktop_inference_token)`、
// 第 4 个证明头 `x-phanthy-desktop-session` 放的就是令牌原值 ——
// **没有令牌就签不出这个包**（官方桥里是 `if(!iy) return null`，干脆不发）。
//
// ⇒ 现在正确的行为是：必须先换令牌，再签，再发。
func TestChatFetchesInferenceToken(t *testing.T) {
	up := &chatFakeUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	p := newFakeProviderChat(t, srv)
	cred := newChatCred(t, "u-token")

	st, err := p.Chat(context.Background(), cred, mustJSON(t, map[string]any{
		"model":    "Phanthy-fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer st.Body.Close()
	_, _ = io.Copy(io.Discard, st.Body)

	if up.summaryCalls == 0 {
		t.Fatalf("chat 路径没调 %s —— 12 段 canonical 第 8 段是令牌的哈希，"+
			"没有令牌就签不出证明包（官方桥 `_O8` 里 `if(!iy) return null`）",
			pathActivitiesSummary)
	}
	// summary 这条**不发**幂等键（canonical 第 7 段是空串，坑⑪）
	if k := up.gotSummaryKey; k != "" {
		t.Errorf("summary 带上了幂等键 %q —— 官方这一步 canonical 第 7 段是空串，"+
			"带了会签出一个服务端重算不出来的串", k)
	}
	// 换到的令牌必须被用上（与 desktop-session 同值）
	if up.gotInferToken == "" {
		t.Fatal("假上游没发出推理令牌，用例前提不成立")
	}
	if got := up.gotHeaders.Get("x-phanthy-desktop-session"); got != up.gotInferToken {
		t.Errorf("x-phanthy-desktop-session = %q，want 刚换到的令牌 %q", got, up.gotInferToken)
	}
}

// TestChatPassesOpenAIBodyThrough 断言请求体**不做 OpenAI→Anthropic 转换**。
//
// 原始 body 里必须仍含 `messages[].role`（OpenAI 形状），
// 而不是被改成 Anthropic 的 `system` 顶层字段。
func TestChatPassesOpenAIBodyThrough(t *testing.T) {
	up := &chatFakeUpstream{}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	p := newFakeProviderChat(t, srv)
	cred := newChatCred(t, "u-body")

	st, err := p.Chat(context.Background(), cred, mustJSON(t, map[string]any{
		"model":       "phanthywork/Phanthy-fast",
		"temperature": 0.7,
		"max_tokens":  1234,
		"messages": []any{
			map[string]any{"role": "system", "content": "you are a bot"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer st.Body.Close()
	_, _ = io.Copy(io.Discard, st.Body)

	var sent map[string]any
	if err := json.Unmarshal(up.gotBody, &sent); err != nil {
		t.Fatalf("发出去的 body 不是 JSON: %v（原文 %s）", err, string(up.gotBody))
	}
	// ① model 前缀必须被剥掉
	if m, _ := sent["model"].(string); m != "Phanthy-fast" {
		t.Errorf("model = %q，want %q（出口层带 phanthywork/ 前缀，上游只认裸名）", m, "Phanthy-fast")
	}
	// ② stream 必须被强制 true（否则上游回一次性 JSON，出口按 SSE 解析 ⇒ 空流 502）
	if s, _ := sent["stream"].(bool); !s {
		t.Errorf("stream = %v，want true（出口层按 SSE 解析，false 会得到空流）", sent["stream"])
	}
	// ③ messages 必须**原样保留**（角色/顺序都不动），而不是被压成 Anthropic 的 messages+system
	msgs, ok := sent["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages 被改动了：%v（want 原样 2 条）", sent["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Errorf("messages[0].role = %v，want system —— chat 路径不做协议转换，"+
			"system 必须留在 messages 里（Anthropic 才把它提成顶层字段）", first["role"])
	}
	// ④ 其余字段逐字节保留
	if v, _ := sent["temperature"].(float64); v != 0.7 {
		t.Errorf("temperature = %v，want 0.7（RawMessage 整形不该动无关字段）", sent["temperature"])
	}
	if v, _ := sent["max_tokens"].(float64); v != 1234 {
		t.Errorf("max_tokens = %v，want 1234（新版内核把 max_tokens 原样透传）", sent["max_tokens"])
	}
}

// TestChatRawSSEPassThrough 断言 200 的响应体**原样透传**。
//
// 与 messages 路径对比：那条要把 Anthropic 帧翻成 OpenAI 帧（否则核心聚合到
// 空 content），这条上游本来就是 OpenAI 帧 ⇒ 一个字节都不该动。
func TestChatRawSSEPassThrough(t *testing.T) {
	const want = "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	up := &chatFakeUpstream{streamBody: want}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	p := newFakeProviderChat(t, srv)
	cred := newChatCred(t, "u-raw")

	st, err := p.Chat(context.Background(), cred, mustJSON(t, map[string]any{
		"model":    "Phanthy-fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer st.Body.Close()
	got, _ := io.ReadAll(st.Body)

	if string(got) != want {
		t.Fatalf("响应体被改动了（chat 路径应原样透传）\n got=%q\nwant=%q", string(got), want)
	}
}

// TestChatErrorStatusReachesClassifier 断言非 2xx 原样交给核心分类。
//
// 关键：`request_not_authorized` 这类错误必须在**核心**里被判，
// 本包只负责把状态码与响应体原样交出去。分类策略见 errors.go 的文件头。
func TestChatErrorStatusReachesClassifier(t *testing.T) {
	const errBody = `{"error":{"code":"request_not_authorized","message":"当前请求不可用，请更新或重新打开客户端后重试。"}}`
	up := &chatFakeUpstream{status: 403, errBody: errBody}
	srv := httptest.NewServer(up.handler())
	defer srv.Close()

	p := newFakeProviderChat(t, srv)
	cred := newChatCred(t, "u-err")

	st, err := p.Chat(context.Background(), cred, mustJSON(t, map[string]any{
		"model":    "Phanthy-fast",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}))
	if err != nil {
		t.Fatalf("403 不该以 error 返回，应作为 ChatStream 交给核心分类: %v", err)
	}
	defer st.Body.Close()
	raw, _ := io.ReadAll(st.Body)

	if st.Status != 403 {
		t.Fatalf("状态码 = %d, want 403", st.Status)
	}
	if string(raw) != errBody {
		t.Fatalf("响应体必须原样保留（核心靠 code 分类）\n got=%q\nwant=%q", string(raw), errBody)
	}
	// 分类口径：request_not_authorized → 换号但不罚号（errors.go 文件头）
	if k := Classify(st.Status, string(raw)); k == gateway.ErrKindSessionDead || k == gateway.ErrKindAuth {
		t.Fatalf("request_not_authorized 被归成 %v —— 那会永久禁用账号；"+
			"errors.go 的文件头明确它只应「换号不罚」（ErrKindClient）", k)
	}
}

// TestProtocolParse 协议名解析（含默认值与容错）。
func TestProtocolParse(t *testing.T) {
	cases := []struct {
		in   string
		want Protocol
	}{
		{"", ProtocolChat},     // 默认 chat
		{"chat", ProtocolChat}, // 显式
		{"CHAT", ProtocolChat}, // 大小写不敏感
		{"messages", ProtocolMessages},
		{" Messages ", ProtocolMessages}, // 空白容错
		{"anthropic", ProtocolMessages},  // 别名
		{"v1messages", ProtocolMessages}, // 别名
		{"garbage", ProtocolChat},        // 未知值 → 安全默认
	}
	for _, c := range cases {
		if got := ParseProtocol(c.in); got != c.want {
			t.Errorf("ParseProtocol(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestProtocolDefaultsToChat 断言**不配置**就走新版协议。
//
// 这是产品级默认值：新版内核已是主流，旧版 claude-sidecar 只是兼容。
func TestProtocolDefaultsToChat(t *testing.T) {
	p := NewProvider(Config{})
	if p.Protocol() != ProtocolChat {
		t.Fatalf("默认协议 = %q, want %q\n"+
			"新版内核（0.2.13）抓包实证只走 /v1/chat/completions；"+
			"默认必须是 chat，否则开箱即 500/403。", p.Protocol(), ProtocolChat)
	}
	if got := p.protocolPath(); got != pathChatCompletions {
		t.Fatalf("默认路径 = %q, want %q", got, pathChatCompletions)
	}
	// SetProtocol 能切回旧版（给旧内核部署留的口子）
	p.SetProtocol("messages")
	if got := p.protocolPath(); got != pathMessages {
		t.Fatalf("SetProtocol(messages) 之后路径 = %q, want %q", got, pathMessages)
	}
}

// mustJSON 把 map 序列化成测试用 body（失败即 t.Fatal）。
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化测试 body 失败: %v", err)
	}
	return b
}

// digestOf 给日志用的一行摘要（避免在断言失败信息里打印整段 body）。
func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])[:12]
}

var _ = fmt.Sprintf // 保持 fmt 被引用（部分子测试只在失败分支用到）
var _ = digestOf
