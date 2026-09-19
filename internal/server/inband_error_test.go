// inband_error_test.go 出口层对"上游 2xx + 流内错误信封"的处理。
//
// # 为什么需要这一组
//
// upstream 侧只负责**认出**错误信封（见 upstream.InBandError 的测试）；
// 出口层要负责把它接回**既有的**错误处理链路：
//
//	classifyErr(reqProvider, 200, 原文) → applyErrorPolicy(kind) → fail → 换号
//
// 这里钉住三件事，任何一条退化都会让"外部调用 api 只有 codearts 报错"复发：
//
//  1. 客户端**不再**收到 `200 + 空 content`，而是带上游原文的真实错误；
//  2. 模型名/通道类错误**不惩罚账号**（否则一个客户端的错误模型名
//     就能把整个账号池冷却掉 —— 那是本仓库反复强调的红线）；
//  3. 额度类错误仍然触发硬冷却（沿用上游自己的判据）。
package server

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// 实测形态（`go test -tags probe` 抓到的 CodeArts / 华为 InferHub 原文）：
// 模型未注册。注意是 error_code / error_msg，**不是** OpenAI 风格的 error。
const framesModelNotRegistered = "data:{\"text\":\"[DONE]\"," +
	"\"error_code\":\"InferHub.002002009.404\"," +
	"\"error_msg\":\"The model is not registered, please request other model\"}\n\n"

// 实测形态：benefit 通道不可用（带 details 数组）。
const framesBenefitNotFound = "data:{\"error_code\":\"InferHub.4004.200\"," +
	"\"error_msg\":\"benefit not found\"," +
	"\"details\":[{\"error_code\":\"InferHub.4004.200\",\"error_msg\":\"requestId: abc\"}]}\n\n"

// 实测形态：额度耗尽（塞在 200 流里的业务错误）。
const framesQuotaExhausted = "data:{\"error_code\":\"InferHub.4291.200\"," +
	"\"error_msg\":\"insufficient quota\"}\n\n"

// inbandProvider 模拟"以 200 + 流内错误信封拒绝请求"的上游。
type inbandProvider struct {
	id     string
	frames string

	chatCalls atomic.Int32
}

var _ gateway.Provider = (*inbandProvider)(nil)
var _ gateway.ErrorClassifier = (*inbandProvider)(nil)

func (p *inbandProvider) ID() string { return p.id }

func (p *inbandProvider) Caps() gateway.Capability { return gateway.CapChat }

func (p *inbandProvider) Chat(_ context.Context, _ gateway.Credential, _ []byte) (gateway.ChatStream, error) {
	p.chatCalls.Add(1)
	// 关键：状态码是 **200** —— 错误只在流里。
	return gateway.ChatStream{Status: 200, Body: io.NopCloser(strings.NewReader(p.frames))}, nil
}

func (p *inbandProvider) Models(_ context.Context, _ gateway.Credential) ([]gateway.ModelInfo, error) {
	return []gateway.ModelInfo{{ID: p.id + "/m"}}, nil
}

// Classify 复刻 codearts 分类器的关键分派：额度耗尽 → 硬冷却；其余 → 不罚。
//
// ⚠ status 传进来的是 **200**（错误藏在 2xx 里就是这个事实），
// 所以分类器只能看正文 —— 与 codearts.Classify 的第一步
// （DetectQuotaExhausted）是同一个形状。
func (p *inbandProvider) Classify(_ int, body string) gateway.ErrorKind {
	if strings.Contains(body, "insufficient quota") {
		return gateway.ErrKindHardCredit
	}
	return gateway.ErrKindNone
}

// inbandRouter 只接一个上游的 ProviderRouter 桩。
type inbandRouter struct {
	def string
	reg map[string]*inbandProvider
}

var _ ProviderRouter = (*inbandRouter)(nil)

func (r *inbandRouter) Has(id string) bool { _, ok := r.reg[id]; return ok }

func (r *inbandRouter) Default() string { return r.def }

func (r *inbandRouter) Models(_ context.Context, id string) ([]gateway.ModelInfo, bool) {
	pv, ok := r.reg[id]
	if !ok {
		return nil, false
	}
	ms, err := pv.Models(context.Background(), gateway.Credential{Provider: id})
	if err != nil || len(ms) == 0 {
		return nil, false
	}
	return ms, true
}

// ModelMultipliers 测试桩：未实现官方倍率扩展点。
func (r *inbandRouter) ModelMultipliers(_ context.Context, _ string) (map[string]float64, bool) {
	return nil, false
}

func (r *inbandRouter) Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	pv, ok := r.reg[id]
	if !ok {
		return gateway.ChatStream{}, false, nil
	}
	cs, err := pv.Chat(ctx, cred, body)
	return cs, true, err
}

func (r *inbandRouter) Credential(id, uid string) (gateway.Credential, bool) {
	if _, ok := r.reg[id]; !ok {
		return gateway.Credential{}, false
	}
	return gateway.Credential{Provider: id, UID: uid, Secret: "secret-" + uid}, true
}

// RefreshCredential 返回 (false, nil) —— "该上游没有续期实现，
// 它的凭证不需要刷新"，正是 gateway.CredentialRefresher 的 ok=false 语义。
func (r *inbandRouter) RefreshCredential(_ context.Context, _ string, _ gateway.Credential) (bool, error) {
	return false, nil
}

func (r *inbandRouter) RefreshSkew(_ string, _ gateway.Credential) (time.Duration, bool) {
	return 0, false
}

// SoftRateReset 测试桩：默认不给出模型级限时（保持账号级软冷却路径）。
func (r *inbandRouter) SoftRateReset(_ string, _ int, _ string) (time.Time, bool) {
	return time.Time{}, false
}

func (r *inbandRouter) ResetAt(_ string, _ gateway.Credential) (time.Time, bool) {
	return time.Time{}, false
}

func (r *inbandRouter) Classify(id string, status int, body string) (gateway.ErrorKind, bool) {
	pv, ok := r.reg[id]
	if !ok {
		return gateway.ErrKindNone, false
	}
	return pv.Classify(status, body), true
}

// newInbandHandler 造一个只有单个 codearts 上游的 handler。
//
// 账号的 ExpiresAt 设成远未来 + RefreshSkew 报告"没有窗口"→ 回落核心兜底，
// 两边都判 false，保证本用例只测"流内错误怎么处理"。
func newInbandHandler(t *testing.T, frames string) (*Handler, *inbandProvider) {
	t.Helper()

	p := pool.New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetDefaultProvider("codearts")

	caAuth := &auth.Auth{UID: "ca-1", Nickname: "codearts-号"}
	p.SyncToDirWithSecrets("codearts", []*auth.Auth{caAuth}, map[string]any{"ca-1": "secret"})
	p.SetCredits("ca-1", 100000)
	p.SetMaxInFlight(10)

	pv := &inbandProvider{id: "codearts", frames: frames}
	r := &inbandRouter{def: "codearts", reg: map[string]*inbandProvider{"codearts": pv}}

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 500, `{"error":"默认上游的 client 不该被 codearts 请求碰到"}`, false
	})

	h := NewHandler(Config{
		Pool:            p,
		Upstream:        up,
		Provider:        r,
		DefaultProvider: "codearts",
		OwnedBy:         "codearts",
	})
	// 远未来：NeedsRefresh 必须为 false，否则会先走刷新分支。
	setExpiry(t, h, "ca-1", time.Now().Add(24*time.Hour).Unix())
	return h, pv
}

// postChat 发一次请求，body 原样传入（用来分别构造 stream / 非 stream）。
func postChat(t *testing.T, h *Handler, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestChatInBandModelErrorIsNotAnEmpty200 是本次修复在出口层的主闸门（非流式）。
//
// 改造前：上游 200 + 错误信封 → Aggregate 合成 `content:""` 的 200 →
// 客户端"成功"拿到一个空回复，错误原文丢失。
func TestChatInBandModelErrorIsNotAnEmpty200(t *testing.T) {
	h, pv := newInbandHandler(t, framesModelNotRegistered)

	code, body := postChat(t, h, `{"model":"codearts/no-such-model",`+
		`"messages":[{"role":"user","content":"hi"}]}`)

	if code == 200 {
		t.Fatalf("状态码仍是 200 —— 流内错误又被洗成了空成功。body=%q\n"+
			"★ 客户端会显示成空回复/报错，且日志里只有 200 tok=0。", body)
	}
	if !strings.Contains(body, "not registered") {
		t.Errorf("错误原文未透出给客户端，body=%q", body)
	}
	if pv.chatCalls.Load() == 0 {
		t.Error("上游 Provider 根本没被调用 —— 测的不是这条路径")
	}
}

// TestChatInBandBenefitChannelErrorIsNotAnEmpty200 覆盖实测的第二类拒绝形态。
//
// benefit 通道不可用时上游回的是 `InferHub.4004.200 benefit not found` ——
// 与"模型未注册"是**不同的** error_code，但同样塞在 200 里。
// 这正是 `codearts/glm-5.3-flash` 等 3 个模型实测返回空的原因，
// 也是它们被取消 Verified 的依据。
func TestChatInBandBenefitChannelErrorIsNotAnEmpty200(t *testing.T) {
	h, _ := newInbandHandler(t, framesBenefitNotFound)

	code, body := postChat(t, h, `{"model":"codearts/glm-5.3-flash",`+
		`"messages":[{"role":"user","content":"hi"}]}`)

	if code == 200 {
		t.Fatalf("状态码仍是 200 —— benefit 通道的流内错误也被洗成了空成功。body=%q", body)
	}
	if !strings.Contains(body, "benefit not found") {
		t.Errorf("错误原文未透出给客户端，body=%q", body)
	}
}

// TestChatInBandModelErrorDoesNotPunishAccount 守住"不惩罚无辜账号"这条红线。
//
// 模型名/通道类错误是**客户端写错了**，不是账号的问题。
// 若把它算成账号失败，一个客户端打错模型名就能把账号池冷却掉。
func TestChatInBandModelErrorDoesNotPunishAccount(t *testing.T) {
	h, _ := newInbandHandler(t, framesModelNotRegistered)

	_, _ = postChat(t, h, `{"model":"codearts/no-such-model",`+
		`"messages":[{"role":"user","content":"hi"}]}`)

	st := statusOf(t, h, "ca-1")
	if st.ErrTotal != 0 {
		t.Errorf("err_total=%d，期望 0 —— 模型名错误不该记在账号头上", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("账号被冷却（reason=%q）—— 模型名错误不该冷却账号", st.Reason)
	}
	if st.Disabled {
		t.Error("账号被禁用 —— 模型名错误绝不该永久禁用账号")
	}
}

// TestChatInBandQuotaErrorCoolsAccount 确认额度类流内错误**仍然**按上游判据处理。
//
// 这一条同时修掉 codearts 的"危害①额度漏判"：
// 额度耗尽的原文是 "insufficient quota"，而 workbuddy 分类器的
// hardMarkers 里是 "insufficient credit" —— 两者都只在**正文**里，
// 改造前 200 的状态码让分类器根本没机会看到它。
func TestChatInBandQuotaErrorCoolsAccount(t *testing.T) {
	h, _ := newInbandHandler(t, framesQuotaExhausted)

	code, _ := postChat(t, h, `{"model":"codearts/GLM-5.2",`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if code == 200 {
		t.Fatal("额度耗尽的流内错误仍被当成成功")
	}

	st := statusOf(t, h, "ca-1")
	if !st.Cooling {
		t.Errorf("额度耗尽后账号应进入冷却，实际 Cooling=false（err_total=%d）", st.ErrTotal)
	}
}

// TestChatStreamInBandErrorIsNotAnEmpty200 是流式路径上的对应闸门。
//
// 改造前客户端在这条路径上收到的是：
//
//	HTTP 200
//	data: {"id":"chatcmpl-wb2api","object":"chat.completion.chunk","usage":null}
//	data: [DONE]
//
// —— 一个没有 choices、没有 error 的空壳。
func TestChatStreamInBandErrorIsNotAnEmpty200(t *testing.T) {
	h, _ := newInbandHandler(t, framesModelNotRegistered)

	code, body := postChat(t, h, `{"model":"codearts/no-such-model","stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)

	if code == 200 {
		t.Fatalf("流式路径状态码仍是 200，body=%q\n"+
			"★ 首帧即错误时 Stream 不该写出任何东西，状态码必须还能改。", body)
	}
	if strings.Contains(body, "chatcmpl-wb2api") {
		t.Errorf("仍是那个空壳帧（本 bug 的原始形态）: %q", body)
	}
	if strings.HasPrefix(body, "data:") {
		t.Errorf("返回体仍是 SSE 而不是 JSON 错误: %q", body)
	}
	if !strings.Contains(body, "not registered") {
		t.Errorf("错误原文未透出，body=%q", body)
	}
}

// TestChatStreamNormalControlStillWorks 是对照组：
// 正常流必须**逐字不变**，仍然是 200 + SSE，且不被新逻辑误伤。
func TestChatStreamNormalControlStillWorks(t *testing.T) {
	h, _ := newInbandHandler(t, sseOK)

	code, body := postChat(t, h, `{"model":"codearts/GLM-5.2","stream":true,`+
		`"messages":[{"role":"user","content":"hi"}]}`)

	if code != 200 {
		t.Fatalf("正常流的对照用例不应失败：code=%d body=%q", code, body)
	}
	if !strings.HasPrefix(body, "data:") {
		t.Errorf("正常流应回 SSE，实际 %q", body)
	}
	if !strings.Contains(body, "你好") {
		t.Errorf("正常流的内容被改动了：%q", body)
	}
	if st := statusOf(t, h, "ca-1"); st.ErrTotal != 0 || st.Cooling {
		t.Errorf("正常流不该惩罚账号：err_total=%d cooling=%v", st.ErrTotal, st.Cooling)
	}
}

