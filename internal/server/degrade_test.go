// degrade_test.go 内容策略拦截 → 提示词降级重试 的端到端判据。
//
// # 这一层最容易错的不是"降级有没有生效"，而是"账号有没有被误罚"
//
// 改造前的路径是：内容拦截（HTTP 400 + 审核文案）→ 分类兜底成"客户端错误"
// → 出站循环按 ErrClient 处理"只换号不罚"。看起来没罚，但有两个真问题：
//
//	① 换号对内容问题**毫无意义** —— 每个账号背后是同一套内容策略。
//	   一次内容拦截会白烧 MaxRotate 次往返，最后返回"所有账号不可用"，
//	   把内容问题误报成账号池故障。
//	② 它占用了本该给真正故障用的换号预算。
//
// 本文件钉住修复后的四条性质：
//
//	A. 首次拦截 → 触发降级并**重试一次**（且第二次真的用了 Degraded 提示词）
//	B. 降级后仍被拦 → 如实返回上游错误，**不再换号**
//	C. 全程**不罚账号**（不冷却、不计错、不喂熔断）
//	D. 单账号部署也能重试（tried 里必须撤掉该号，否则选不出号 → 503 假故障）
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/upstream"
)

// newBuf 返回一个记录响应的 ResponseRecorder。
func newBuf() *httptest.ResponseRecorder { return httptest.NewRecorder() }

// httptestNewChat 造一个 POST /v1/chat/completions 请求。
func httptestNewChat(body string) *http.Request {
	return httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
}

// chatRecorder 记录每次出站请求的 body，并按调用序返回预设响应。
type chatRecorder struct {
	mu     sync.Mutex
	bodies []string

	// responses[i] 是第 i 次调用的 (status, body, isStream)；
	// 超出长度时复用最后一个。
	responses []struct {
		status int
		body   string
		stream bool
	}
}

func (r *chatRecorder) record(b []byte) {
	r.mu.Lock()
	r.bodies = append(r.bodies, string(b))
	r.mu.Unlock()
}

func (r *chatRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *chatRecorder) bodyAt(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.bodies) {
		return ""
	}
	return r.bodies[i]
}

func (r *chatRecorder) next(i int) (int, string, bool) {
	if len(r.responses) == 0 {
		return 500, `{"code":1,"msg":"recorder 未配置响应"}`, false
	}
	idx := i
	if idx >= len(r.responses) {
		idx = len(r.responses) - 1
	}
	x := r.responses[idx]
	return x.status, x.body, x.stream
}

// newRecorderUpstream 造一个会记录请求 body 的假上游。
func newRecorderUpstream(t *testing.T, r *chatRecorder) *upstream.Client {
	t.Helper()
	c := newFakeUpstream(t, func(string) (int, string, bool) { return 500, "", false })
	c.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		n := r.calls()
		r.record(raw)
		status, body, stream := r.next(n)
		ct := "application/json"
		if stream {
			ct = "text/event-stream"
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	return c
}

// systemContentOf 取出 wire body 的 messages[0].content。
func systemContentOf(t *testing.T, body string) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("wire body 不是 JSON: %v; %.300s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("wire body 没有 messages: %.300s", body)
	}
	s, _ := msgs[0].(map[string]any)["content"].(string)
	return s
}

// blockedBody 是上游内容策略拦截的响应体（workbuddy 的标记之一）。
const blockedBody = `{"code":1,"msg":"blocked by security policy"}`

// chatReqBody 带 system 的请求体（以便断言降级是否真的改写了它）。
const chatReqBody = `{"model":"glm-5.2","messages":[` +
	`{"role":"system","content":"客户端原始人格"},` +
	`{"role":"user","content":"hi"}]}`

// assertNotPunished 断言账号既没进冷却也没被熔断/禁用。
//
// # 这是本文件的核心断言
//
// "内容问题不是账号问题"这句话必须落在池状态上才算数：
// 只要 applyErrorPolicy 被误调一次，账号就会进冷却，
// 而它在界面上表现为"这个号有问题" —— 一个完全错误、且没人会怀疑的归因。
func assertNotPunished(t *testing.T, h *Handler, uid string) {
	t.Helper()
	st, ok := h.cfg.Pool.Status(uid)
	if !ok {
		t.Fatalf("池里找不到账号 %s", uid)
	}
	if st.Cooling {
		t.Errorf("内容拦截不得让账号进冷却：uid=%s reason=%q until=%v", uid, st.Reason, st.Until)
	}
	if st.Disabled {
		t.Errorf("内容拦截不得禁用账号：uid=%s", uid)
	}
	if !st.BreakerUntil.IsZero() {
		t.Errorf("内容拦截不得触发熔断：uid=%s breakerUntil=%v", uid, st.BreakerUntil)
	}
	// 计错计数也必须保持 0：它是成功率权重的输入，
	// 被内容拦截污染会让这个号在选号时被莫名降权。
	if st.ErrTotal != 0 {
		t.Errorf("内容拦截不得计入错误数：uid=%s errTotal=%d", uid, st.ErrTotal)
	}
}

// TestContentBlockTriggersDegradeAndRetries 首次拦截 → 触发降级 + 重试成功。
//
// 这是主路径：客户端人格撞了内容策略，网关换中性提示词重试一次，
// 第二次上游放行 → 用户拿到正常回复，且**完全不知道中间发生过拦截**。
func TestContentBlockTriggersDegradeAndRetries(t *testing.T) {
	rec := &chatRecorder{}
	rec.responses = append(rec.responses,
		struct {
			status int
			body   string
			stream bool
		}{400, blockedBody, false},
		struct {
			status int
			body   string
			stream bool
		}{200, sseOK, true},
	)
	up := newRecorderUpstream(t, rec)
	gate := prompt.NewGate()
	up.PromptMode = prompt.ModeCustom
	up.PromptText = "网关自有提示词"
	up.PromptGate = gate

	uid := "u-block"
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: uid, AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		PromptGate: gate,
	})

	rec2 := newBuf()
	req := httptestNewChat(chatReqBody)
	h.ServeHTTP(rec2, req)

	if rec2.Code != http.StatusOK {
		t.Fatalf("降级重试后应成功，得到 %d：%.300s", rec2.Code, rec2.Body.String())
	}
	if rec.calls() != 2 {
		t.Fatalf("应恰好调用上游 2 次（首次被拦 + 降级重试），实际 %d", rec.calls())
	}
	// 第一次用的是网关自有提示词（不是客户端那份）。
	if got := systemContentOf(t, rec.bodyAt(0)); got != "网关自有提示词" {
		t.Errorf("第 1 次出站的 system=%q，期望网关自有提示词", got)
	}
	// 第二次必须是**中性降级提示词** —— 这是本测试真正要证明的那一步。
	if got := systemContentOf(t, rec.bodyAt(1)); got != prompt.Degraded {
		t.Errorf("第 2 次出站的 system=%q，期望 Degraded（降级没生效 == 白重试一次）", got)
	}
	if !gate.Active() {
		t.Error("首次内容拦截后应进入降级期（后续请求直达中性提示词，不再先撞 400）")
	}
	assertNotPunished(t, h, uid)
}

// TestContentBlockTwiceReturnsUpstreamErrorWithoutRotating 降级后仍被拦 → 如实返回，不再换号。
//
// # 为什么"不再换号"是正确行为
//
// 第二次被拦说明问题在**用户内容**里（中性提示词已经没有指纹了）。
// 继续换号只会在每个号上重复同一次失败，最后返回"所有账号不可用" ——
// 一个把内容问题误报成账号池故障的错误结论。
//
// 本用例断言：返回体里带 error.code = "content_blocked" 且含上游原文，
// 状态码沿用上游的 400（不是 503）。
func TestContentBlockTwiceReturnsUpstreamErrorWithoutRotating(t *testing.T) {
	rec := &chatRecorder{}
	rec.responses = append(rec.responses, struct {
		status int
		body   string
		stream bool
	}{400, blockedBody, false})

	up := newRecorderUpstream(t, rec)
	gate := prompt.NewGate()
	up.PromptMode = prompt.ModeCustom
	up.PromptText = "网关自有提示词"
	up.PromptGate = gate

	uid := "u-block2"
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: uid, AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		PromptGate: gate,
		MaxRotate:  5, // 给足预算：若实现仍在换号，这里会跑满 5 次
	})

	w := newBuf()
	h.ServeHTTP(w, httptestNewChat(chatReqBody))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("应如实返回上游的 400，得到 %d：%.300s", w.Code, w.Body.String())
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v; %.300s", err, w.Body.String())
	}
	if resp.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q，期望 content_blocked（便于客户端识别这是内容问题）", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "blocked by security policy") {
		t.Errorf("应带上游原文，便于用户定位：%q", resp.Error.Message)
	}
	// 关键：只调了 2 次（首次 + 一次降级重试），**没有**跑满 MaxRotate。
	if rec.calls() != 2 {
		t.Errorf("应恰好 2 次上游调用（不换号重试），实际 %d —— "+
			"跑满说明内容拦截仍走了换号路径", rec.calls())
	}
	assertNotPunished(t, h, uid)
}

// TestContentBlockSingleAccountStillRetries 单账号部署也必须能重试。
//
// # 这条钉住的是一个非常容易被忽略的实现细节
//
// 出站循环用 `tried` 集合防止重复选同一个号。若内容拦截分支**沿用**这个集合，
// PickFor 会在下一轮跳过唯一的账号 → 选不出号 → break →
// 返回 503 "all accounts unavailable"。
//
// 于是"降级重试"在最常见的部署形态（一个号）下**从未发生过**，
// 用户看到的是"内容拦截 = 服务不可用"。
//
// 修复方式是内容拦截分支把该号从 tried 里撤掉（见 handler.go 的注释）。
// 反向判别力：去掉那句 delete，本用例必然失败（拿到 503 而不是 200）。
func TestContentBlockSingleAccountStillRetries(t *testing.T) {
	rec := &chatRecorder{}
	rec.responses = append(rec.responses,
		struct {
			status int
			body   string
			stream bool
		}{400, blockedBody, false},
		struct {
			status int
			body   string
			stream bool
		}{200, sseOK, true},
	)
	up := newRecorderUpstream(t, rec)
	gate := prompt.NewGate()
	up.PromptMode = prompt.ModeCustom
	up.PromptText = "网关自有提示词"
	up.PromptGate = gate

	// 池里**只有一个**账号 —— 这正是绝大多数自托管部署的形态。
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "only", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		PromptGate: gate,
	})

	w := newBuf()
	h.ServeHTTP(w, httptestNewChat(chatReqBody))

	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("单账号部署下投降级重试不该返回 503 —— "+
			"说明内容拦截分支没有把该号从 tried 里撤掉，下一轮选不出号：%.300s", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("应重试成功，得到 %d：%.300s", w.Code, w.Body.String())
	}
	if rec.calls() != 2 {
		t.Errorf("应恰好 2 次上游调用，实际 %d", rec.calls())
	}
}

// TestContentBlockWithNilGateReturnsErrorNoRetry 未接提示词体系（nil gate）→ 直接如实返回。
//
// nil gate 表示本部署没有降级能力。此时继续换号是纯浪费
// （不换提示词，重试必然同样被拦），如实返回上游错误更有信息量。
func TestContentBlockWithNilGateReturnsErrorNoRetry(t *testing.T) {
	rec := &chatRecorder{}
	rec.responses = append(rec.responses, struct {
		status int
		body   string
		stream bool
	}{400, blockedBody, false})
	up := newRecorderUpstream(t, rec)
	// 刻意不接提示词体系：Gate 与 PromptMode 都留空/nil。

	uid := "u-nogate"
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: uid, AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		MaxRotate: 5,
	})

	w := newBuf()
	h.ServeHTTP(w, httptestNewChat(chatReqBody))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("无降级能力时应如实返回 400，得到 %d：%.300s", w.Code, w.Body.String())
	}
	if rec.calls() != 1 {
		t.Errorf("无降级能力时不该重试（重试必然同样被拦），实际调用 %d 次", rec.calls())
	}
	assertNotPunished(t, h, uid)
}

// TestContentBlockAlreadyDegradedDoesNotRetryAgain 进入请求时已在降级期 → 不再多试一轮。
//
// 语义：每个请求最多因内容拦截重试**一次**。若上一批请求已经把 Gate 触发过，
// 那么本次发出的已经是 Degraded 提示词 —— 再拦就说明是用户内容，没有第三次可试。
//
// 反向判别力：把 `degradedTried` 的初值从 Gate.Active() 改成恒 false，
// 本用例会看到 2 次调用（多了一轮必然失败的重试）。
func TestContentBlockAlreadyDegradedDoesNotRetryAgain(t *testing.T) {
	rec := &chatRecorder{}
	rec.responses = append(rec.responses, struct {
		status int
		body   string
		stream bool
	}{400, blockedBody, false})
	up := newRecorderUpstream(t, rec)
	gate := prompt.NewGate()
	gate.Trigger() // 已在降级期
	up.PromptMode = prompt.ModeCustom
	up.PromptText = "网关自有提示词"
	up.PromptGate = gate

	uid := "u-predegraded"
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: uid, AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		PromptGate: gate,
		MaxRotate:  5,
	})

	w := newBuf()
	h.ServeHTTP(w, httptestNewChat(chatReqBody))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("降级期内的拦截应直接返回 400，得到 %d", w.Code)
	}
	if rec.calls() != 1 {
		t.Errorf("已在降级期时不该再重试一次，实际调用 %d 次", rec.calls())
	}
	// 而且本次出站用的就是 Degraded（客户端读 Gate 得知已在降级期）。
	if got := systemContentOf(t, rec.bodyAt(0)); got != prompt.Degraded {
		t.Errorf("降级期内首次出站就该用 Degraded，得到 %q", got)
	}
}
