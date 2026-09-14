// chat_multiprovider_test.go —— **chat 出站路径**上的多上游回归。
//
// # 为什么必须补这个文件（这是本次修复的验收闸门）
//
// 改造前这个仓里**一条 chat 路径的多上游测试都没有**：
//
//	models_multiprovider_test.go  —— 只覆盖 /v1/models 的目录合并
//	pool/multiprovider_test.go    —— 只覆盖"选号按 provider 过滤"
//
// 于是「选号按上游分域了，但**出站仍然恒用 cfg.Upstream**」这个 bug
// 在测试里完全不可见 —— 选号那条断言是绿的（P0 的一半确实做对了），
// 而出站那一半没人看。
//
// 实测后果（真机 18080）：`codearts/glm-5.3-flash` 请求选中 codearts 账号后，
// 被 workbuddy 的客户端拿去 RefreshToken → "no refreshToken"
// → 账号被标记失败并冷却 → 池子耗尽 → 503 no_healthy_account。
// 证据：一次请求让 codearts 账号 err_total 从 3 涨到 6，
// 而 workbuddy 三个号的 last_err 始终是 0001-01-01。
//
// # 本文件钉住的是什么
//
// 一个**可观察的判据**（而不是"实现里调了哪个函数"）：
//
//	codearts 前缀的请求 → 只有 codearts 的 Provider 被调用；
//	                      workbuddy 的 Provider 与 workbuddy 的 upstream.Client
//	                      **一次都不能被碰**
//
// 这条判据在"改回 h.cfg.Upstream.ChatStream"时立刻变红 —— 见文件末尾的
// TestChatCodeartsUsesCodeartsProviderNotDefault 的注释（变异验证）。
package server

import (
	"context"
	"encoding/json"
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

// ---------------------------------------------------------------------------
// 记录型 Provider：既是 gateway.Provider，也是 gateway.CredentialRefresher
// ---------------------------------------------------------------------------

// recordingProvider 是一个**只记账不做事**的上游实现。
//
// 它刻意记录三件事，因为它们正是"出站分派对不对"的全部可观察面：
//
//	chatCalls      谁被调了 Chat（以及拿到的是哪份凭证）
//	refreshCalls   谁被调了续期
//	rawBody        出站请求体（验证 provider 前缀被剥掉）
//
// 不复用 internal/codearts / internal/workbuddy 的真实实现：
// 那两条路径都要真凭证与真上游，而本文件要验的是**分派**，
// 不是"codearts 的签名对不对"（后者由 codearts 包自己的测试覆盖）。
type recordingProvider struct {
	id string

	// status/body 是 Chat 的返回值（非 2xx 也走 Body，见 gateway 契约）。
	status int
	body   string

	// refreshErr 非 nil 时续期失败（用来验失败路径）。
	refreshErr error

	chatCalls    atomic.Int32
	refreshCalls atomic.Int32
	// lastSecret 最近一次 Chat 拿到的凭证 Secret（验"传的是这个上游的凭证"）。
	lastSecret atomic.Value // any
	// lastBody 最近一次 Chat 收到的请求体。
	lastBody atomic.Value // string
}

var _ gateway.Provider = (*recordingProvider)(nil)
var _ gateway.CredentialRefresher = (*recordingProvider)(nil)

func (p *recordingProvider) ID() string { return p.id }

func (p *recordingProvider) Caps() gateway.Capability { return gateway.CapChat }

func (p *recordingProvider) Chat(_ context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	p.chatCalls.Add(1)
	p.lastSecret.Store(cred.Secret)
	p.lastBody.Store(string(body))
	st, bd := p.status, p.body
	if st == 0 {
		st, bd = 200, sseOK
	}
	return gateway.ChatStream{Status: st, Body: io.NopCloser(strings.NewReader(bd))}, nil
}

func (p *recordingProvider) Models(_ context.Context, _ gateway.Credential) ([]gateway.ModelInfo, error) {
	return []gateway.ModelInfo{{ID: p.id + "-m"}}, nil
}

func (p *recordingProvider) RefreshCredential(_ gateway.Credential) error {
	p.refreshCalls.Add(1)
	return p.refreshErr
}

// ---------------------------------------------------------------------------
// 多上游桩：把两个 recordingProvider 接成 ProviderRouter
// ---------------------------------------------------------------------------

// multiRouter 是本包内实现 ProviderRouter 的多上游桩。
//
// ⚠ 与 models_multiprovider_test.go 里的 testRouter 分开：那个是目录专用的
// （它没有 Chat/Credential/RefreshCredential），本文件要的正是**出站**那一半。
// 两条桩都实现同一个接口，是因为接口本身就是"出口层需要的多上游能力"，
// 而不同测试只用到其中一部分。
type multiRouter struct {
	def string
	reg map[string]*recordingProvider
	p   *pool.Pool
	// creds 覆盖某 uid 的 secret（nil = 从池子取）。
	creds map[string]any
}

var _ ProviderRouter = (*multiRouter)(nil)

func (r *multiRouter) Has(id string) bool { _, ok := r.reg[id]; return ok }

func (r *multiRouter) Default() string { return r.def }

func (r *multiRouter) Models(_ context.Context, id string) ([]gateway.ModelInfo, bool) {
	pv, ok := r.reg[id]
	if !ok {
		return nil, false
	}
	return []gateway.ModelInfo{{ID: pv.id}}, true
}

func (r *multiRouter) Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	pv, ok := r.reg[id]
	if !ok {
		return gateway.ChatStream{}, false, nil
	}
	cs, err := pv.Chat(ctx, cred, body)
	return cs, true, err
}

func (r *multiRouter) Credential(id, uid string) (gateway.Credential, bool) {
	if _, ok := r.reg[id]; !ok {
		return gateway.Credential{}, false
	}
	// 覆盖表优先（测试要精确指定"这个号拿到的是 codearts 的凭证"）。
	if s, ok := r.creds[uid]; ok {
		return gateway.Credential{Provider: id, UID: uid, Secret: s}, true
	}
	if r.p == nil {
		return gateway.Credential{}, false
	}
	secret, ok := r.p.SecretOf(uid)
	if !ok || secret == nil {
		return gateway.Credential{}, false
	}
	return gateway.Credential{Provider: id, UID: uid, Secret: secret}, true
}

func (r *multiRouter) RefreshCredential(_ context.Context, id string, cred gateway.Credential) (bool, error) {
	pv, ok := r.reg[id]
	if !ok {
		return false, nil
	}
	if err := pv.RefreshCredential(cred); err != nil {
		return true, err
	}
	return true, nil
}

// RefreshSkew 把问题转给**该 ID 的上游**（与 RefreshCredential 同一形状）。
//
// 找不到该上游时返回 `ok=false`（"我没有意见"），出口层回落核心兜底窗口。
//
// ⚠ 这里刻意**不**回落成 `r.def` 的 skew —— 那正是本 bug 的形态：
// 拿默认上游的事实去回答另一个上游的问题。
func (r *multiRouter) RefreshSkew(id string, cred gateway.Credential) (time.Duration, bool) {
	pv, ok := r.reg[id]
	if !ok {
		return 0, false
	}
	ext, ok := gateway.ExtOf[gateway.RefreshSkewExt](pv)
	if !ok {
		return 0, false
	}
	return ext.RefreshSkew(cred)
}

// ResetAt 把"额度耗尽的号什么时候能再用"转给**该 ID 的上游**
// （与 RefreshSkew / RefreshCredential 同一形状）。
//
// ⚠ **必须按 id 查，绝不能回落成 `r.def` 的排程**：那正是 P2 这个 bug 的形态
// —— 无参回调只能返回同一个上游的答案，于是 codearts 的号被冷到
// workbuddy 的次日 04:00。找不到该上游时如实答 ok=false。
// SoftRateReset 测试桩：默认不给出模型级限时（保持账号级软冷却路径）。
func (r *multiRouter) SoftRateReset(_ string, _ int, _ string) (time.Time, bool) {
	return time.Time{}, false
}

func (r *multiRouter) ResetAt(id string, cred gateway.Credential) (time.Time, bool) {
	pv, ok := r.reg[id]
	if !ok {
		return time.Time{}, false
	}
	ext, ok := gateway.ExtOf[gateway.ResetPolicyExt](pv)
	if !ok {
		return time.Time{}, false
	}
	return ext.ResetAt(cred)
}

// Classify 把错误分类转给**该 ID 的上游**（gateway.ErrorClassifier）。
//
// ⚠ 与 ResetAt / RefreshSkew 同一条纪律：**不回落成默认上游的分类器**。
// 回落会让"某个上游没实现分类器"变成"用别的上游的判据解释它的错误体"，
// 那正是 ErrorClassifier 这个扩展点要修的事（见 gateway/errorclassifier.go）。
// 出口层拿到 ok=false 后回落 `upstream.Classify` —— 那是**核心**的明确取舍，
// 不是装配层偷偷替它做的。
func (r *multiRouter) Classify(id string, status int, body string) (gateway.ErrorKind, bool) {
	pv, ok := r.reg[id]
	if !ok {
		return gateway.ErrKindNone, false
	}
	ext, ok := gateway.ExtOf[gateway.ErrorClassifier](pv)
	if !ok {
		return gateway.ErrKindNone, false
	}
	return ext.Classify(status, body), true
}

// ---------------------------------------------------------------------------
// 构造：一个混池 + 两个 recording provider
// ---------------------------------------------------------------------------

// codeartsSecret / workbuddySecret 是两份**结构完全不同**的凭证。
//
// 这正是 bug 的根源：核心把 codearts 的那份交给 workbuddy 的客户端，
// 后者按自己的格式读必然读空（真实实现读的是 *auth.Auth 的字段）。
// 本文件用一个**哨兵字符串**复刻同样的可观察性：
// 哪份 secret 出现在哪个 Provider 的 Chat 里，一眼可辨。
type fakeCodeartsSecret struct{ AK, SK, DPoP string }

type fakeWorkbuddySecret struct{ AccessToken, RefreshToken string }

// newMultiUpstreamHandler 造一个 "workbuddy(默认) + codearts" 的 handler。
//
// 池子里有两个号：
//
//	ca-1  provider=codearts   secret = fakeCodeartsSecret
//	wb-1  provider=workbuddy  secret = fakeWorkbuddySecret
//
// 两边都设成永远不用刷新（ExpiresAt 远future）+ 额度充足，
// 让选号确定性地落到本用例要测的那个号上。
func newMultiUpstreamHandler(t *testing.T, caStatus int, caBody string) (*Handler, *recordingProvider, *recordingProvider, *multiRouter) {
	t.Helper()

	p := pool.New("")
	// 确定性随机源：候选集里积分最高者胜出（见 handler_test.go 的 testPoolWith）。
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetDefaultProvider("workbuddy")

	caSecret := &fakeCodeartsSecret{AK: "CA_AK", SK: "CA_SK", DPoP: "CA_DPOP"}
	wbSecret := &fakeWorkbuddySecret{AccessToken: "WB_AT", RefreshToken: "WB_RT"}

	// ⚠ SyncToDirWithSecrets 是"按 provider 分域入池"的唯一入口。
	// 用 p.Add 的话 provider 标签是默认上游，codearts 的号会落进 workbuddy 域，
	// 本用例就退化成"单上游"而**假绿**。
	caAuth := &auth.Auth{UID: "ca-1", Nickname: "codearts-号"}
	wbAuth := &auth.Auth{UID: "wb-1", Nickname: "workbuddy-号"}
	p.SyncToDirWithSecrets("codearts", []*auth.Auth{caAuth}, map[string]any{"ca-1": caSecret})
	p.SyncToDirWithSecrets("workbuddy", []*auth.Auth{wbAuth}, map[string]any{"wb-1": wbSecret})

	// 额度：codearts 的号给足，保证 PickFor("codearts", ...) 能选中它。
	p.SetCredits("ca-1", 100000)
	p.SetCredits("wb-1", 100000)
	// 在途上限：SetMaxInFlight 是**全局**的（不是 per-uid）——
	// 给一个宽松值，避免测试进程里残留的租约把候选集清空而假绿。
	p.SetMaxInFlight(10)

	ca := &recordingProvider{id: "codearts", status: caStatus, body: caBody}
	wb := &recordingProvider{id: "workbuddy"}

	r := &multiRouter{
		def: "workbuddy",
		reg: map[string]*recordingProvider{"codearts": ca, "workbuddy": wb},
		p:   p, // 注意：Credential 优先从池子取 secret
		creds: map[string]any{
			"ca-1": caSecret,
			"wb-1": wbSecret,
		},
	}

	// 默认上游的 upstream.Client 用一个**会打日志的假上游** ——
	// 它一旦被调用就说明出站分派漏了（见 TestChatCodeartsNeverTouchesDefaultClient）。
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 500, `{"error":"默认上游的 client 不该被 codearts 请求碰到"}`, false
	})

	h := NewHandler(Config{
		Pool:            p,
		Upstream:        up,
		Provider:        r,
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
	})
	return h, ca, wb, r
}

// chatOnce 发一次请求并返回 (http 状态码, 响应体)。
func chatOnce(t *testing.T, h *Handler, model string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ---------------------------------------------------------------------------
// 核心回归：codearts 前缀的请求必须走 codearts 的实现
// ---------------------------------------------------------------------------

// TestChatCodeartsUsesCodeartsProviderNotDefault 是本次修复的**主闸门**。
//
// # 变异验证（必须红）
//
// 把 handler.go 里的 `cr, status, terr := h.chatVia(ctx, reqProvider, acct, outBody)`
// 改回 `rc, status, _, terr := h.cfg.Upstream.ChatStream(acct, outBody)`：
//
//	→ codearts 的 recordingProvider.Chat 调用数为 **0**（断言 1 失败）
//	→ workbuddy 的假 upstream 被调用，返回 500
//	→ 本用例红
//
// 这就是"不补这个测试，bug 会静默回来"的具体含义。
func TestChatCodeartsUsesCodeartsProviderNotDefault(t *testing.T) {
	h, ca, wb, _ := newMultiUpstreamHandler(t, 200, sseOK)

	code, body := chatOnce(t, h, "codearts/glm-5.3-flash")

	// 1) 必须是 codearts 的实现被调用，且恰好一次。
	if got := ca.chatCalls.Load(); got != 1 {
		t.Fatalf("codearts 的 Provider 被调用 %d 次，want 1。\n"+
			"★ 出站分派漏了 —— 请求很可能被发给了默认上游（workbuddy）的客户端。\n"+
			"  这正是 861dee5 引入、预先存在的那条 bug：选号按 provider 分域了，出站没有。", got)
	}
	// 2) 默认上游的实现一次都不能被碰。
	if got := wb.chatCalls.Load(); got != 0 {
		t.Errorf("workbuddy 的 Provider 被调用 %d 次，want 0（codearts 的请求不该碰它）", got)
	}
	// 3) 请求必须成功（改造前这里是 503 no_healthy_account: no refreshToken）。
	if code != 200 {
		t.Fatalf("code=%d want 200 body=%s", code, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v want chat.completion", resp["object"])
	}

	// 4) 传下去的凭证必须是**这个上游的**那一份。
	//    用哨兵类型断言：拿到 *fakeWorkbuddySecret 就说明装配层装配错了上游。
	sec, _ := ca.lastSecret.Load().(*fakeCodeartsSecret)
	if sec == nil || sec.AK != "CA_AK" {
		t.Fatalf("codearts 的 Chat 拿到的 Secret 是 %#v，want *fakeCodeartsSecret{AK:CA_AK}", ca.lastSecret.Load())
	}
}

// TestChatCodeartsStripsProviderPrefix 出站体里必须是**剥掉前缀**的裸模型名。
//
// 上游不认识 "codearts/glm-5.3-flash"（实测：带着前缀发过去会被按未知模型拒绝）。
// 这条与"按前缀选号"是一体两面，改造前由 handler 里的 rewriteModel 保证 ——
// 本次改动把它挪进了分派路径，必须确认没丢。
func TestChatCodeartsStripsProviderPrefix(t *testing.T) {
	h, ca, _, _ := newMultiUpstreamHandler(t, 200, sseOK)

	if code, body := chatOnce(t, h, "codearts/glm-5.3-flash"); code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	raw, _ := ca.lastBody.Load().(string)
	if strings.Contains(raw, "codearts/") {
		t.Errorf("出站体里仍有 provider 前缀（上游不认识它）: %s", raw)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("出站体不是 JSON: %v %s", err, raw)
	}
	if obj["model"] != "glm-5.3-flash" {
		t.Errorf("出站 model=%v want glm-5.3-flash", obj["model"])
	}
}

// TestChatWorkbuddyStillUsesWorkbuddyProvider 反向回归：
// workbuddy 的请求（裸模型名 = 默认上游）走 workbuddy 的实现，不碰 codearts。
//
// 「修好新的，不能弄坏旧的」—— 这条守的是既有客户端的路径。
func TestChatWorkbuddyStillUsesWorkbuddyProvider(t *testing.T) {
	h, ca, wb, _ := newMultiUpstreamHandler(t, 200, sseOK)

	code, body := chatOnce(t, h, "glm-5.3-flash")
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if got := wb.chatCalls.Load(); got != 1 {
		t.Errorf("workbuddy 的 Provider 被调用 %d 次，want 1", got)
	}
	if got := ca.chatCalls.Load(); got != 0 {
		t.Errorf("codearts 的 Provider 被调用 %d 次，want 0（裸模型名走默认上游）", got)
	}
}

// TestChatWorkbuddyPrefixedAlsoUsesWorkbuddyProvider 显式 workbuddy/ 前缀同理。
func TestChatWorkbuddyPrefixedAlsoUsesWorkbuddyProvider(t *testing.T) {
	h, ca, wb, _ := newMultiUpstreamHandler(t, 200, sseOK)

	if code, body := chatOnce(t, h, "workbuddy/glm-5.3-flash"); code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if got := wb.chatCalls.Load(); got != 1 {
		t.Errorf("workbuddy 的 Provider 被调用 %d 次，want 1", got)
	}
	if got := ca.chatCalls.Load(); got != 0 {
		t.Errorf("codearts 的 Provider 被调用 %d 次，want 0", got)
	}
}

// ---------------------------------------------------------------------------
// 非 2xx：必须回到 upstream.Classify（而不是按 status 二次判断）
// ---------------------------------------------------------------------------

// TestChatCodeartsNon2xxGoesThroughClassify 上游返回非 2xx 时，
// 错误体必须从**流里**读出来喂 Classify。
//
// 这是 gateway.ChatStream 与旧 upstream.Client.ChatStream 的形状差异所在：
//
//	旧的：非 2xx 时 rc=nil、错误体在第三个返回值（[]byte）里
//	新的：非 2xx 时错误体**也在 Body 流里**
//
// 不做 io.ReadAll 的话，Classify 会收到空串 → 分类退化成按状态码猜 →
// 429 不再触发软冷却、402 不再触发硬冷却。那是一条**静默的行为退化**。
func TestChatCodeartsNon2xxGoesThroughClassify(t *testing.T) {
	// 402 + 额度耗尽关键词 → upstream.Classify 判 ErrHardCredit。
	h, ca, _, _ := newMultiUpstreamHandler(t, 402, `{"error":"insufficient credit, quota exhausted"}`)

	code, body := chatOnce(t, h, "codearts/glm-5.3-flash")

	if got := ca.chatCalls.Load(); got == 0 {
		t.Fatalf("codearts 的 Provider 一次都没被调用")
	}
	// 单号池 + MaxRotate 用尽 → 503，且消息里必须带上**上游的错误体**。
	if code != 503 {
		t.Fatalf("code=%d want 503 body=%s", code, body)
	}
	if !strings.Contains(body, "quota exhausted") {
		t.Errorf("响应里没有上游错误体原文 —— 说明 Classify 只拿到了空串。\n"+
			"  非 2xx 时 gateway.ChatStream 把错误体放在 Body（流）里，"+
			"不 io.ReadAll 就丢了。body=%s", body)
	}
}

// TestChatCodeartsNon2xxPunishesUpstreamNotUnrelated 只有**出错的号**被处理。
//
// 改造后的失败归因必须对：codearts 的请求失败 → codearts 的号进入冷却；
// workbuddy 的号**完全不受影响**（last_err 保持零值）。
//
// 这正是真机上的判据："codearts 的 err_total 不再上涨，而 workbuddy 的
// 账号 last_err 仍是 0001-01-01"。
//
// ⚠ 断言写成「ca-1 进了硬冷却 且 wb-1 完全干净」，而不是「ca-1 的 err_total>0」：
// 402 走的是 applyErrorPolicy 的 ErrHardCredit 分支 —— 那是
// `CooldownUntilNextReset`，**不喂** err_total（err_total 只由 ErrServer
// 的 NoteError 累加）。写成 err_total 会断言错一个级别的东西。
func TestChatCodeartsNon2xxPunishesUpstreamNotUnrelated(t *testing.T) {
	h, _, _, _ := newMultiUpstreamHandler(t, 402, `{"error":"insufficient credit"}`)

	if code, _ := chatOnce(t, h, "codearts/glm-5.3-flash"); code != 503 {
		t.Fatalf("code=%d want 503", code)
	}

	caSt := statusOf(t, h, "ca-1")
	wbSt := statusOf(t, h, "wb-1")

	// 出错的那个号必须被处理（冷却/禁用/计数，三者有其一）。
	if !caSt.Cooling && !caSt.Disabled && caSt.ErrTotal == 0 {
		t.Errorf("codearts 的号完全没有被处理（它才是出错的那个）: cooling=%v disabled=%v err_total=%d",
			caSt.Cooling, caSt.Disabled, caSt.ErrTotal)
	}
	// 无辜的号一点都不能被碰。
	if wbSt.Cooling || wbSt.Disabled || wbSt.ErrTotal != 0 || !wbSt.LastErrTime.IsZero() {
		t.Errorf("★ workbuddy 的号被无辜惩罚了: cooling=%v disabled=%v err_total=%d last_err=%s",
			wbSt.Cooling, wbSt.Disabled, wbSt.ErrTotal, wbSt.LastErrTime)
	}
}

// ---------------------------------------------------------------------------
// 凭证装配失败：换号，但**不惩罚**这个账号
// ---------------------------------------------------------------------------

// TestChatMissingCredentialDoesNotPunishAccount 装配层取不到凭证时，
// 出口层按"这个号现在发不出去"处理（换号），但**不能**惩罚这个账号。
//
// 理由：取不到凭证是**接线问题**（账号没进池 / secret 没装载），
// 不是这个账号坏了。惩罚它会让一个配置错误演变成"池子里少了一个好号" ——
// 与本次修的 bug 同源：**账号被与自己无关的失败惩罚**。
//
// # 为什么要显式把凭证做成"新鲜"的
//
// 不设 ExpiresAt（=0）时 `NeedsRefresh` 恒为 true（见 auth.Auth.NeedsRefresh
// 的第一行），于是请求会**先走续期分支**。那条分支对"取不到凭证"的取舍是
// 另一件事（续期拿不到凭证确实可以记一次错误：那说明装配层坏了），
// 不是本用例要测的那条。
//
// 本用例要测的是**已经走到出站那一步**时的行为，所以先把凭证设成远未过期。
func TestChatMissingCredentialDoesNotPunishAccount(t *testing.T) {
	h, ca, _, r := newMultiUpstreamHandler(t, 200, sseOK)
	// 把凭证通道掐断：Credential 恒返回 ok=false。
	r.creds = map[string]any{}
	r.p = nil
	// 凭证"新鲜"（远未过期）→ 跳过续期分支，直接进 chatVia。
	setExpiry(t, h, "ca-1", time.Now().Add(24*time.Hour).Unix())

	code, _ := chatOnce(t, h, "codearts/glm-5.3-flash")
	if code != 503 {
		t.Fatalf("code=%d want 503（发不出去就是发不出去）", code)
	}
	if got := ca.chatCalls.Load(); got != 0 {
		t.Errorf("没有凭证时不该调用上游 Chat，实际 %d 次", got)
	}
	st := statusOf(t, h, "ca-1")
	if st.ErrTotal != 0 || !st.LastErrTime.IsZero() || st.Cooling || st.Disabled {
		t.Errorf("★ 接线问题惩罚了账号: err_total=%d last_err=%s cooling=%v disabled=%v\n"+
			"  取不到凭证不代表这个号坏了。", st.ErrTotal, st.LastErrTime, st.Cooling, st.Disabled)
	}
}

// TestChatRefreshMissingCredentialDoesNotCooldown 续期分支上的同理断言。
//
// 与上一条的区别：这里凭证**已经是过期的**，请求会先走续期分支。
// 那条分支在拿不到凭证时返回 errNoProviderCredential，调用方据此
// **记一次 NoteError**（err_total +1）并换号。
//
// 这个取舍与出站分支不同，是刻意的：
//
//	出站拿不到凭证 → 这个号这一次发不出去（换号即可，不该累计）
//	续期拿不到凭证 → 装配层坏了，且**每次**请求都会走到这里，
//	                 不记错会让一个坏掉的接线表现为"无限重试同一个号"
//
// 但**冷却与禁用**在两条分支上都必须为假 —— 那才是"惩罚"的实质。
func TestChatRefreshMissingCredentialDoesNotCooldown(t *testing.T) {
	h, ca, _, r := newMultiUpstreamHandler(t, 200, sseOK)
	r.creds = map[string]any{}
	r.p = nil
	setExpiry(t, h, "ca-1", time.Now().Add(-time.Hour).Unix()) // 已过期 → 走续期分支

	if code, _ := chatOnce(t, h, "codearts/glm-5.3-flash"); code != 503 {
		t.Fatalf("code=%d want 503", code)
	}
	if got := ca.chatCalls.Load(); got != 0 {
		t.Errorf("续期拿不到凭证时不该调用上游 Chat，实际 %d 次", got)
	}
	st := statusOf(t, h, "ca-1")
	if st.Cooling || st.Disabled {
		t.Errorf("★ 接线问题冷却/禁用了账号: cooling=%v disabled=%v", st.Cooling, st.Disabled)
	}
	// 无辜的另一家一点都不能被碰。
	wbSt := statusOf(t, h, "wb-1")
	if wbSt.ErrTotal != 0 || !wbSt.LastErrTime.IsZero() {
		t.Errorf("★ workbuddy 的号被无辜惩罚: err_total=%d last_err=%s", wbSt.ErrTotal, wbSt.LastErrTime)
	}
}

// ---------------------------------------------------------------------------
// 续期分派（A′）：workbuddy 不能去刷 codearts 的号
// ---------------------------------------------------------------------------

// TestRefreshCredentialDispatchesByProvider 是 A′ 的闸门。
//
// workbuddy 的 upstream.Client 对着 codearts 的账号调 RefreshToken 会返回
// "no refreshToken"（真实实现读的是它自己的字段），上层随即把这个**无辜的
// codearts 账号**标记失败 —— 真机上 codearts 的 err_total 就是这么涨的。
//
// 改造后：近过期时按 provider 分派，codearts 的号由 codearts 的实现续期。
func TestRefreshCredentialDispatchesByProvider(t *testing.T) {
	h, ca, wb, _ := newMultiUpstreamHandler(t, 200, sseOK)

	// 把 codearts 的号做成「临近过期」：RefreshSkew 默认 10m。
	// 注意这里用的是**池子里**那份 auth.Auth（出口层读的是它）。
	setExpiry(t, h, "ca-1", time.Now().Add(2*time.Minute).Unix())

	if code, body := chatOnce(t, h, "codearts/glm-5.3-flash"); code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if got := ca.refreshCalls.Load(); got != 1 {
		t.Fatalf("codearts 的续期实现被调用 %d 次，want 1\n"+
			"★ 续期没有按 provider 分派（仍在用默认上游的客户端刷）。", got)
	}
	if got := wb.refreshCalls.Load(); got != 0 {
		t.Errorf("workbuddy 的续期实现被调用 %d 次，want 0", got)
	}
}

// TestRefreshCredentialSkippedWhenProviderHasNoRefresher
// 上游**没有**实现 CredentialRefresher 时，语义是"它的凭证不需要刷新" ——
// 直接跳过，继续用现有凭证发请求。
//
// 把它当成失败会让这类上游（纯 API Key 那种）每次请求都白换一次号，
// 最终耗尽轮转额度回 503。这是"合法实现被通用假设误伤"的典型。
func TestRefreshCredentialSkippedWhenProviderHasNoRefresher(t *testing.T) {
	p := pool.New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetDefaultProvider("keyonly")
	a := &auth.Auth{UID: "k-1", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	p.SyncToDirWithSecrets("keyonly", []*auth.Auth{a}, map[string]any{"k-1": &fakeWorkbuddySecret{AccessToken: "K"}})
	p.SetCredits("k-1", 1000)

	// 只实现 Provider，**不实现** CredentialRefresher。
	noRefresh := &noRefresherProvider{id: "keyonly"}
	r := NewHandler(Config{
		Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 500, `{}`, false }),
		Provider:        routerWithNoRefresher{def: "keyonly", pv: noRefresh},
		DefaultProvider: "keyonly",
	})

	code, body := chatOnce(t, r, "keyonly/anything")
	if code != 200 {
		t.Fatalf("code=%d want 200（没有续期实现 = 不需要续期，不该失败）body=%s", code, body)
	}
	if got := noRefresh.chatCalls.Load(); got != 1 {
		t.Errorf("Chat 被调用 %d 次 want 1", got)
	}
}

// noRefresherProvider 只实现 gateway.Provider。
type noRefresherProvider struct {
	id        string
	chatCalls atomic.Int32
}

var _ gateway.Provider = (*noRefresherProvider)(nil)

func (p *noRefresherProvider) ID() string               { return p.id }
func (p *noRefresherProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *noRefresherProvider) Models(_ context.Context, _ gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (p *noRefresherProvider) Chat(_ context.Context, _ gateway.Credential, _ []byte) (gateway.ChatStream, error) {
	p.chatCalls.Add(1)
	return gateway.ChatStream{Status: 200, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
}

// routerWithNoRefresher 把 noRefresherProvider 接成 ProviderRouter。
//
// ⚠ 刻意**不**实现 gateway.CredentialRefresher —— 这正是被测的那条路径。
type routerWithNoRefresher struct {
	def string
	pv  *noRefresherProvider
}

var _ ProviderRouter = routerWithNoRefresher{}

func (r routerWithNoRefresher) Has(id string) bool { return id == r.def }
func (r routerWithNoRefresher) Default() string    { return r.def }
func (r routerWithNoRefresher) Models(_ context.Context, _ string) ([]gateway.ModelInfo, bool) {
	return nil, false
}
func (r routerWithNoRefresher) Chat(_ context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error) {
	if id != r.def {
		return gateway.ChatStream{}, false, nil
	}
	cs, err := r.pv.Chat(context.Background(), cred, body)
	return cs, true, err
}
func (r routerWithNoRefresher) Credential(_, uid string) (gateway.Credential, bool) {
	return gateway.Credential{Provider: r.def, UID: uid, Secret: &fakeWorkbuddySecret{AccessToken: "K"}}, true
}
func (r routerWithNoRefresher) RefreshCredential(_ context.Context, _ string, _ gateway.Credential) (bool, error) {
	// 出口层问到这个上游能不能续期 —— 上游没实现，如实回答 ok=false。
	return false, nil
}

// RefreshSkew 同 RefreshCredential：本桩的上游没有实现该扩展点，如实答 ok=false。
func (r routerWithNoRefresher) RefreshSkew(_ string, _ gateway.Credential) (time.Duration, bool) {
	return 0, false
}

// ResetAt 同 RefreshSkew：本桩的上游没有上报额度恢复排程，如实答 ok=false。
// 出口层据此回落核心的通用保守值 now+1h。
func (r routerWithNoRefresher) ResetAt(_ string, _ gateway.Credential) (time.Time, bool) {
	return time.Time{}, false
}

// SoftRateReset 测试桩：默认不给出模型级限时（保持账号级软冷却路径）。
//
// ⚠ 接收者必须是**值**接收者：本桩以 `routerWithNoRefresher{}`（非指针）
// 被赋给 ProviderRouter，指针接收者会让它不满足接口。其余路由桩用指针
// 是因为它们本来就以指针传入 —— 这里跟随各自原有的接收者形态。
func (r routerWithNoRefresher) SoftRateReset(_ string, _ int, _ string) (time.Time, bool) {
	return time.Time{}, false
}

// Classify 同 RefreshSkew：本桩的上游没有实现分类器，如实答 ok=false。
// 出口层回落 upstream.Classify（默认上游的判据，也正是改造前的行为）。
func (r routerWithNoRefresher) Classify(_ string, _ int, _ string) (gateway.ErrorKind, bool) {
	return gateway.ErrKindNone, false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// statusOf 从池子里取一个账号的完整状态快照（断言用）。
func statusOf(t *testing.T, h *Handler, uid string) pool.Status {
	t.Helper()
	for _, s := range h.cfg.Pool.List() {
		if s.UID == uid {
			return s
		}
	}
	t.Fatalf("池子里找不到 uid=%s", uid)
	return pool.Status{}
}

// setExpiry 把池子里某个账号的过期时刻改成 at（Unix 秒）。
//
// 出口层的 `acct.NeedsRefresh(RefreshSkew)` 读的是**池子里那份** auth.Auth，
// 所以必须改那一份（改测试自己持有的副本不会生效）。
func setExpiry(t *testing.T, h *Handler, uid string, at int64) {
	t.Helper()
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		t.Fatalf("池子里找不到 uid=%s", uid)
	}
	a.Lock()
	a.ExpiresAt = at
	a.Unlock()
}
