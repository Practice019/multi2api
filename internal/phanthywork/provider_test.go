// provider_test.go 端到端行为断言：对着**假上游**跑真实的五步链路。
//
// 这些用例的价值在于"把不可诊断的坑变成可诊断的"：每个用例的失败信息
// 都直接点名报告里的那一条（坑②/③/⑥/⑨/⑪…），而不是"断言失败"。
package phanthywork

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// newChatCredential 造一份"能走完整链路"的凭证（exp 还很远，不触发续期）。
func newChatCredential(uid string) gateway.Credential {
	a := newTestAuth(uid, "refresh-old")
	a.FilePath = ""
	return gateway.Credential{
		Provider: providerID,
		UID:      a.UID(),
		Nickname: a.Nickname(),
		Secret:   a,
	}
}

// chatOnce 打一次 Chat 并把流读完，返回 (状态码, 响应体, error)。
func chatOnce(t *testing.T, p *Provider, cred gateway.Credential, body string) (int, string, error) {
	t.Helper()
	cs, err := p.Chat(context.Background(), cred, []byte(body))
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = cs.Body.Close() }()
	raw, _ := io.ReadAll(cs.Body)
	return cs.Status, string(raw), nil
}

// TestHappyPathFiveSteps 五步链路端到端（假上游逐个校验签名）。
//
// 期望的请求顺序（报告 §2 的五步）：
//
//	GET  /api/oauth/profile                        取 accountScope
//	POST /api/oauth/desktop-installations/register 设备注册（v1 + device 双签名）
//	POST /api/oauth/activities/summary             换推理令牌（v1 签名，**无幂等键**）
//	POST /v1/messages                              12 段 proof 签名
func TestHappyPathFiveSteps(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")

	chatBody := `{"model":"phanthywork/Phanthy-fast","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	status, body, err := chatOnce(t, p, cred, chatBody)
	if err != nil {
		t.Fatalf("Chat 报错（五步链路未走通）: %v；已打过的路径: %v", err, fake.pathsSnapshot())
	}
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200；响应体: %s", status, body)
	}
	// ⚠ 交出去的必须是 **OpenAI SSE**（核心只认 choices[].delta.*）：
	// 本包在 Chat 成功分支里把上游的 Anthropic SSE 转成 OpenAI 形态（见 provider.go 的注释）。
	// 这条断言 2026-11-12 改过一次：改前它断言的是"像 /v1/messages 的 SSE"——
	// 而**恰恰是那个形态**让客户端拿到空 content（真机 7 次实测）。
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("响应体不是 OpenAI SSE 分片（核心解析不到 ⇒ 客户端会拿到空 content）: %q", body)
	}
	if strings.Contains(body, "\"output_tokens\"") {
		t.Errorf("Anthropic 的 usage 键不该出现在交出去的流里（说明没转换）: %q", body)
	}
	for _, want := range []string{pathProfile, pathRegister, pathActivitiesSummary, pathMessages} {
		if fake.pathCount(want) == 0 {
			t.Errorf("没有请求过 %s（已打过: %v）", want, fake.pathsSnapshot())
		}
	}
}

// TestInferenceTokenIsReusedWithinTTL 180 秒内不重复换令牌（坑④的另一半）。
//
// ⚠ "每次调用前按 TTL+余量换新"的反面同样重要：**不该每请求都换**。
// 每请求都换会打爆 summary 端点，而症状只是"慢"（最难发现的那一类）。
func TestInferenceTokenIsReusedWithinTTL(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	for i := 0; i < 3; i++ {
		if _, _, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`); err != nil {
			t.Fatalf("第 %d 次 Chat 失败: %v", i+1, err)
		}
	}
	if n := fake.pathCount(pathActivitiesSummary); n != 1 {
		t.Errorf("3 次调用只应换 1 次推理令牌（TTL 180 秒 + 30 秒余量），实际换了 %d 次", n)
	}
	if n := fake.pathCount(pathRegister); n != 1 {
		t.Errorf("设备注册只应发生 1 次（一次性写操作），实际 %d 次", n)
	}
	if n := fake.pathCount(pathMessages); n != 3 {
		t.Errorf("推理应发生 3 次，实际 %d 次", n)
	}
}

// TestExpiredInferenceTokenIsRenewed 令牌过期后必须换新（坑④ 的正向）。
func TestExpiredInferenceTokenIsRenewed(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	a := cred.Secret.(*Auth)
	if _, _, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`); err != nil {
		t.Fatal(err)
	}
	// 把它手动改成"已过期"，下一次调用必须换新
	a.storeInferenceToken("stale-token", nowMS()-1000, nowMS())
	if _, _, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`); err != nil {
		t.Fatal(err)
	}
	if n := fake.pathCount(pathActivitiesSummary); n != 2 {
		t.Errorf("过期后应再换一次令牌（共 2 次），实际 %d 次", n)
	}
}

// TestUpstreamBusinessErrorReturnedAsStream 非 2xx 也要按流返回（契约）。
//
// 核心要拿 (status, body) 去分类，所以"上游拒绝"不能变成 error。
func TestUpstreamBusinessErrorReturnedAsStream(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	fake.summaryStatus = http.StatusForbidden
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	status, body, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`)
	if err != nil {
		t.Fatalf("换令牌失败是**业务错误**，应当按流返回而不是 error: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("状态码 = %d, want 403", status)
	}
	if !strings.Contains(body, "request_not_authorized") {
		t.Errorf("响应体应带上游原文（核心靠它分类）: %q", body)
	}
	if fake.pathCount(pathMessages) != 0 {
		t.Error("换令牌失败时不该继续打推理端点")
	}
}

// TestRegistrationConflictIsTerminal 409 是终态、不重试、且封存 24 小时。
func TestRegistrationConflictIsTerminal(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	fake.conflict = true
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	a := cred.Secret.(*Auth)

	_, _, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`)
	if err == nil {
		t.Fatal("409 应当失败（推理面报废）")
	}
	if !IsRegistrationConflict(err) {
		t.Errorf("409 应能被 IsRegistrationConflict 认出来，得到 %v", err)
	}
	if n := fake.pathCount(pathRegister); n != 1 {
		t.Errorf("409 之后不该重试，注册请求数应为 1，实际 %d", n)
	}
	if a.Reg.ConflictAtMS == 0 {
		t.Error("409 必须被记进状态（否则每次启动都重发一次 ⇒ 持续触发设备风控）")
	}
	// 第二次调用：熔断期内**零请求**
	before := fake.pathCount(pathRegister)
	if _, _, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`); err == nil {
		t.Error("熔断期内应当继续失败")
	}
	if after := fake.pathCount(pathRegister); after != before {
		t.Errorf("熔断期内不该再发注册请求（%d -> %d）", before, after)
	}
}

// TestUAIsSentAndRequired 坑②：UA 逐字，且缺了会被拒。
func TestUAIsSentAndRequired(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	fake.badUA = true
	p := newFakeProvider(t, srv)
	// ⚠ 必须**真的**把客户端的 UA 换掉：假上游的判据是"收到的 UA 不是官方值就拒"，
	// 客户端照旧发 Bun/1.3.14 的话这条用例会静默变成"永远绿"。
	p.client.UA = "node/20.11.0"
	cred := newChatCredential("u-1")
	_, body, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`)
	if err != nil {
		t.Fatalf("应把上游错误按流返回而不是 error: %v", err)
	}
	if !strings.Contains(body, "TM.00000001") {
		t.Errorf("UA 不对时上游应回 TM.00000001，实际 %q", body)
	}
}

// TestSummaryNeverSendsIdempotencyKey 坑⑪：这条请求**不发**幂等键。
//
// 假上游的判据是"收到的幂等键必须为空" ⇒ 实现若"顺手加一个"，这里就红。
func TestSummaryNeverSendsIdempotencyKey(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	status, body, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("summary 带了幂等键会被判签名失败：status=%d body=%s", status, body)
	}
	if fake.pathCount(pathActivitiesSummary) != 1 {
		t.Error("应当换过一次推理令牌")
	}
}

// TestRegisterSendsIdempotencyKey 反面：register 那条**要**带 register:<installationId>。
func TestRegisterSendsIdempotencyKey(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	if _, _, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`); err != nil {
		t.Fatal(err)
	}
	if fake.pathCount(pathRegister) != 1 {
		t.Fatal("应注册过一次")
	}
	a := cred.Secret.(*Auth)
	id, err := a.installationID()
	if err != nil {
		t.Fatal(err)
	}
	key := registerIdempotencyKey(id)
	if !strings.HasPrefix(key, "register:di_") {
		t.Errorf("注册幂等键格式应为 register:di_…，得到 %q", key)
	}
}

// TestConcurrentRefreshOnlyHitsTokenEndpointOnce 坑⑤：并发续期只打一次 token 端点。
//
// 这是"refreshToken 会轮换 ⇒ 并发刷新会互踢"的正面证据：闸把并发压成一次，
// 而且**键跨轮换稳定**（否则第二个请求会拿旧 token 再刷一次）。
func TestConcurrentRefreshOnlyHitsTokenEndpointOnce(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	// 造一份"已过期"的凭证，让 RefreshCredential 真的去打 token 端点
	a := newTestAuth("u-1", "refresh-old")
	a.OAuth.ExpiresAtMS = nowMS() - 10_000
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = p.RefreshCredential(cred)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个并发续期失败: %v", i, err)
		}
	}
	if got := fake.pathCount(pathOAuthToken); got != 1 {
		t.Errorf("%d 个并发续期只应打 1 次 token 端点（坑⑤：并发刷新会互踢），实际 %d 次", n, got)
	}
	if a.OAuth.RefreshToken != "refresh-rotated" {
		t.Errorf("续期后 refresh_token 应被更新（轮换），得到 %q", a.OAuth.RefreshToken)
	}
}

// TestRefreshExpiredIsTerminalSentinel 凭据废了要给可识别的终态。
func TestRefreshExpiredIsTerminalSentinel(t *testing.T) {
	p := newFakeProvider(t, httptest.NewServer(http.NotFoundHandler()))
	a := newTestAuth("u-1", "") // 没有 refreshToken ⇒ 不可续期
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}
	err := p.RefreshCredential(cred)
	if !IsRefreshExpired(err) {
		t.Errorf("无 refreshToken 应返回 ErrRefreshExpired（终态），得到 %v", err)
	}
}

// TestParseIntRawHandlesQuotedAndBareNumbers parseIntRaw 必须同时认
// **带引号**与**裸数字**两种形态，并且对 null / 空串返回 0。
//
// ⚠ 这三条是实测踩出来的：
//
//	strings.Trim(t, "") 的 cutset 是**空串**时 Trim **原样返回** —— 于是带引号的
//	数字（JSON 里 expires_in 有可能是字符串）解析失败 ⇒ 返回 0 ⇒ 静默用兜底 TTL。
//	它不报错、不 panic，只是"过一个季度才有人发现 TTL 用错"。
func TestParseIntRawHandlesQuotedAndBareNumbers(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{`"42"`, 42},
		{`42`, 42},
		{` 42 `, 42},
		{`42.0`, 42},
		{`"1800"`, 1800},
		{`null`, 0},
		{``, 0},
		{`"abc"`, 0},
	}
	for _, c := range cases {
		if got := parseIntRaw(json.RawMessage(c.in)); got != c.want {
			t.Errorf("parseIntRaw(%s) = %d, want %d", c.in, got, c.want)
		}
	}
	// ⚠ 空 cutset 的 Trim 是**静默**失效：这条断言专门守它
	if got := parseIntRaw(json.RawMessage(`"7"`)); got != 7 {
		t.Errorf("带引号的数字必须能解析（空 cutset 的 Trim 会原样返回 ⇒ 这里会红），得到 %d", got)
	}
}

// TestBriefHasNoNewlines brief 必须把响应体压成**一行**（日志/错误文案用）。
func TestBriefHasNoNewlines(t *testing.T) {
	got := brief([]byte("a\nb\nc"))
	if strings.Contains(got, "\n") {
		t.Errorf("brief 的产物不该含换行（否则日志一行变三行）: %q", got)
	}
	if got != "a b c" {
		t.Errorf("brief = %q, want %q", got, "a b c")
	}
	// 超长要截断（否则一条错误能把日志顶爆）
	long := strings.Repeat("x", 1000)
	if out := brief([]byte(long)); len(out) > 400 {
		t.Errorf("超长响应体应被截断，得到 %d 字符", len(out))
	}
	// 空输入不该 panic
	if out := brief(nil); out != "" {
		t.Errorf("空输入应得到空串，得到 %q", out)
	}
}

// TestSummaryTokenRequestOmitsIdempotencyKeyHeader 坑⑪ 的 canonical 断言。
//
// 假上游已经逐字校验过"收到的幂等键必须为空"，这里再钉一次
// **第 7 段空串 + 分隔符不能省** —— 两条合起来才完整。
func TestSummaryTokenRequestOmitsIdempotencyKeyHeader(t *testing.T) {
	body := []byte(`{"inference_public_key":"pk"}`)
	canonical := V1Canonical(http.MethodPost, pathActivitiesSummary, "1700000000000", "nonce", body, "")
	// ⚠ v1 的分隔符是**空格**（2026-11-14 修正），不是 LF —— 用常量而非字面量。
	segs := strings.Split(canonical, deviceV1Separator)
	if len(segs) != 7 {
		t.Fatalf("v1 canonical 段数 = %d, want 7", len(segs))
	}
	if segs[6] != "" {
		t.Errorf("activities/summary 的第 7 段必须是空串（坑⑪），得到 %q", segs[6])
	}
	if !strings.HasSuffix(canonical, deviceV1Separator) {
		t.Error("空段的分隔符不能省（坑⑪ / §2.6 规则 3）")
	}
	// 反面：注册那条**要**有幂等键，且第 7 段非空
	reg := V1Canonical(http.MethodPost, pathRegister, "1", "n", []byte("{}"), registerIdempotencyKey("di_x"))
	if !strings.HasSuffix(reg, "register:di_x") {
		t.Errorf("注册的幂等键必须是第 7 段，得到 %q", reg)
	}
}

// TestStreamSurvivesAfterChatReturns 慢速 SSE 流在 Chat 返回后必须**仍可读完**。
//
// # 这条守的是一个真 bug（自查发现）
//
// 第一版 Chat 里写的是 `defer cancel()` —— 那会在函数返回的瞬间取消请求上下文。
// 小响应体检不出来（Go 已经把它缓冲完了），但真机上的长回答会"半路断"，
// 而现象看起来像"上游的问题"。所以这里让假上游**分两段发**、
// 中间真等一会儿，再断言两段都读得到。
func TestStreamSurvivesAfterChatReturns(t *testing.T) {
	mux := http.NewServeMux()
	// 这条链路只需要 /v1/messages：用一个"已经注册好"的凭证，跳过注册与换令牌。
	mux.HandleFunc(pathMessages, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// ⚠ 夹具必须是**合法的 Anthropic SSE**（带 content_block_start / text_delta）。
		// 本包现在会把响应转成 OpenAI 形态再交出去，随手写两行 text 会被转换器丢掉 ——
		// 那样这条用例就**测不到"流有没有被掐断"**（它守的是 defer cancel() 那个真 bug）。
		_, _ = w.Write([]byte(`event: message_start` + "\n" +
			`data: {"type":"message_start","message":{"id":"m1","model":"Phanthy-fast","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
			`event: content_block_start` + "\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(300 * time.Millisecond) // 模拟"长回答的间隔"
		_, _ = w.Write([]byte(`event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"后半段"}}` + "\n\n" +
			`event: message_delta` + "\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newFakeProvider(t, srv)
	a := newTestAuth("u-1", "rt")
	a.Reg.RegisteredAtMS = nowMS() // 已注册：跳过写操作
	now := nowMS()
	a.storeInferenceToken("infer-token", now+120_000, now) // 令牌够新：跳过换令牌
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}

	cs, err := p.Chat(context.Background(), cred, []byte(`{"model":"phanthywork/Phanthy-fast","messages":[]}`))
	if err != nil {
		t.Fatalf("Chat 报错: %v", err)
	}
	// ⚠ 关键：**Chat 已经返回**，此时流还没读完（第二段要 300ms 后才来）。
	// 若实现里写了 defer cancel()，这里的读取会被 context 取消截断。
	raw, rerr := io.ReadAll(cs.Body)
	_ = cs.Body.Close()
	if rerr != nil {
		t.Fatalf("读流失败（defer cancel() 会掐断它）: %v；已读到 %q", rerr, raw)
	}
	// ⚠ 断言"第二段（Chat 返回之后 300ms 才发的）读到了" —— 且读到的是**转换后**的文本。
	// 改前这里断言的是 Anthropic 事件名，夹具被转换后自然对不上（2026-11-12 同步改口径）。
	if !strings.Contains(string(raw), "后半段") {
		t.Errorf("流被截断：Chat 返回之后发的那一段也该读到（且已被转成 OpenAI 形态），实际 %q", raw)
	}
}
