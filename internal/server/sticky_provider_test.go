// sticky_provider_test.go —— 会话粘性**必须按上游隔离**的回归。
//
// # 被钉住的 bug
//
// handler.go 的粘性分支原来调 `Pool.PickByUID(stickyUID)`，而 PickByUID
// **只校验 healthy + inFlightFull，不校验 provider**：
//
//	func (p *Pool) PickByUID(uid string) *auth.Auth {
//	        ...
//	        if !e.healthy(now) { return nil }
//	        if p.inFlightFull(e) { return nil }
//	        return e.a                    // ← 没有 provider 判断
//	}
//
// 粘性命中时不匹配 provider 就**不会**返回 nil，于是下面的
// `if acct == nil { PickFor(reqProvider, ...) }` 回落分支根本不执行 ——
// **别的上游的账号被直接塞进出站循环**。
//
// # 为什么这个 bug 现在才致命
//
// 出站刚改成"按账号自己的 provider 找对应上游"（chatVia）。改造前它还算
// "显式失败"（用错的客户端 → 一次完整往返后报错）；改造后它**不报错**了：
// 粘性号走**它自己上游**的 client，请求成功返回，只是
// 「请求 workbuddy 的模型却用了 codearts 的账号」。路由静默错掉。
//
// # 判据（可观察，不依赖实现细节）
//
//	预绑定 conv-X → codearts 的号
//	发一个 **workbuddy 前缀** 的同会话请求
//	→ codearts 的 Provider **一次都不能被调用**
//	→ workbuddy 的 Provider 被调用，并得到 workbuddy 的号
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// newStickyMultiHandler 造一个"workbuddy(默认) + codearts"的 handler，
// 并**带上会话粘性路由**（newMultiUpstreamHandler 不带 Session）。
//
// 池子里的可用号列表按两个上游分别给出，保证 session.Resolve 的
// 快路径命中校验不会把绑定号判死 —— 我们要测的是**出口层的 provider 校验**，
// 而不是 session 层的可用性判断（后者见 session_test.go）。
func newStickyMultiHandler(t *testing.T) (*Handler, *recordingProvider, *recordingProvider, *bindStore) {
	t.Helper()
	h, ca, wb, _ := newMultiUpstreamHandler(t, 200, sseOK)

	// 两个上游的号都"可用" —— 让 Resolve 的快路径原样返回既有绑定。
	// 这正是 bug 的前提：session 层认为这个绑定是好的，把决定权交给出口层。
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:   time.Minute,
		Store: st,
		Available: func() []string {
			return append(
				append([]string{}, h.cfg.Pool.AvailableUIDsFor("workbuddy")...),
				h.cfg.Pool.AvailableUIDsFor("codearts")...,
			)
		},
	})
	h.cfg.Session = sess
	return h, ca, wb, st
}

// ---------------------------------------------------------------------------
// 负面：跨上游粘性不得被复用（本修复的主闸门）
// ---------------------------------------------------------------------------

// TestStickyCrossProviderNotReused 是本修复的**主断言**。
//
// # 变异验证（把修复去掉必须红）
//
// 把 handler.go 的
//
//	acct = h.cfg.Pool.PickByUIDFor(reqProvider, stickyUID)
//
// 改回
//
//	acct = h.cfg.Pool.PickByUID(stickyUID)
//
//	→ 粘性命中 ca-1（PickByUID 只看 healthy + inFlight，不看 provider，返回非 nil）
//	→ `acct == nil` 的回落分支**不执行**，PickFor("workbuddy", ...) 被跳过
//	→ 出站走 chatVia("codearts", ca-1) → **codearts 的 Provider 被调用**
//	→ 断言 1 失败（ca.chatCalls = 1，want 0）
//	→ 本用例红，并打印出"请求 workbuddy 却用了 codearts 的号"的实际 uid
func TestStickyCrossProviderNotReused(t *testing.T) {
	h, ca, wb, _ := newStickyMultiHandler(t)

	// 预绑定：会话 conv-1 历史上被绑到 **codearts** 的号 ca-1。
	// 这模拟真实场景 —— 同一个会话先问过 codearts 的模型，之后改问 workbuddy 的。
	h.cfg.Session.Bind("conv-1", "ca-1")

	// 同一个会话 key，但模型带 **workbuddy** 前缀（= 本次请求的上游是 workbuddy）。
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],`+
			`"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 1) codearts 的实现**一次都不能被调用**。
	//
	// 这是本 bug 的唯一硬判据：修复前它是 1（codearts 的号被粘性塞进了
	// workbuddy 的请求），而且**没有报错** —— 请求仍然是 200。
	if got := ca.chatCalls.Load(); got != 0 {
		t.Fatalf("codearts 的 Provider 被调用 %d 次，want 0。\n"+
			"★ 跨上游粘性被复用了 —— 请求 workbuddy 的模型，却用了 codearts 的账号。\n"+
			"  这条路径**不会报错**（出站按账号自己的上游分派），所以它只会静默错路由。", got)
	}

	// 2) workbuddy 的实现被调用（说明回落轮换真的执行了，不是整个请求失败）。
	if got := wb.chatCalls.Load(); got != 1 {
		t.Errorf("workbuddy 的 Provider 被调用 %d 次，want 1（应回落到本次请求的上游）", got)
	}

	// 3) 拿到的凭证必须是 workbuddy 的那一份（哨兵类型断言）。
	if sec, _ := wb.lastSecret.Load().(*fakeWorkbuddySecret); sec == nil || sec.AccessToken != "WB_AT" {
		t.Errorf("workbuddy 的 Chat 拿到的 Secret 是 %#v，want *fakeWorkbuddySecret{AccessToken:WB_AT}", wb.lastSecret.Load())
	}

	// 4) 绑定应收敛到 workbuddy 的号（跨上游的旧绑定被解绑 + 跟随成功号重绑）。
	if uid, ok := h.cfg.Session.Resolve("conv-1"); !ok || uid != "wb-1" {
		t.Errorf("粘性绑定应收敛到 wb-1，得到 %q ok=%v —— 跨上游的旧绑定没被替换", uid, ok)
	}
}

// TestStickyCrossProviderReverseDirection 反方向：workbuddy 粘性 + codearts 请求。
//
// 与上一条**对称**但独立：只测一个方向会漏掉"过滤写反了但恰好过了一条"的实现。
func TestStickyCrossProviderReverseDirection(t *testing.T) {
	h, ca, wb, _ := newStickyMultiHandler(t)

	h.cfg.Session.Bind("conv-2", "wb-1")

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"codearts/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],`+
			`"metadata":{"conversation_id":"conv-2"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := wb.chatCalls.Load(); got != 0 {
		t.Fatalf("workbuddy 的 Provider 被调用 %d 次，want 0（请求的是 codearts）", got)
	}
	if got := ca.chatCalls.Load(); got != 1 {
		t.Errorf("codearts 的 Provider 被调用 %d 次，want 1", got)
	}
	if sec, _ := ca.lastSecret.Load().(*fakeCodeartsSecret); sec == nil || sec.AK != "CA_AK" {
		t.Errorf("codearts 的 Chat 拿到的 Secret 是 %#v，want *fakeCodeartsSecret{AK:CA_AK}", ca.lastSecret.Load())
	}
}

// ---------------------------------------------------------------------------
// 正面回归：同上游粘性**仍要正常工作**（不能为了修 bug 把粘性废掉）
// ---------------------------------------------------------------------------

// TestStickySameProviderStillReused 同上游粘性命中必须**直接复用**粘性号，
// 不得退化成普通轮换。
//
// # 为什么这条断言不是形式主义
//
// 最省事的"修法"是把粘性分支整个删掉（或恒传一个空的 stickyUID）——
// 那样跨上游测试全绿，而会话粘性这个**既有功能**被静默废除，
// 对外表现为"多轮对话不再稳定收敛到同一个号"（上游侧会话状态丢失、
// 额度分布被打散）。本用例守住它。
//
// 判据：池子里 codearts 有**两个**健康号，普通轮换会加权随机；
// 而粘性命中必须拿到**绑定的那一个**（cb-1），与加权无关。
func TestStickySameProviderStillReused(t *testing.T) {
	h, ca, _, _ := newStickyMultiHandler(t)
	p := h.cfg.Pool

	// 再加一个 codearts 的号，让"普通轮换"有别的选择 ——
	// 否则"选中 ca-1"可能只是因为池子里只有它，测试假绿。
	p.SyncToDirWithSecrets("codearts", []*auth.Auth{
		{UID: "ca-1", Nickname: "codearts-1"},
		{UID: "ca-2", Nickname: "codearts-2"},
	}, map[string]any{
		"ca-1": &fakeCodeartsSecret{AK: "CA_AK", SK: "CA_SK", DPoP: "CA_DPOP"},
		"ca-2": &fakeCodeartsSecret{AK: "CA2_AK", SK: "CA2_SK", DPoP: "CA2_DPOP"},
	})
	p.SetCredits("ca-2", 100000)

	// 粘性绑定到 ca-2（积分与 ca-1 持平，普通轮换未必选它）。
	h.cfg.Session.Bind("conv-3", "ca-2")

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"codearts/glm-5.3-flash","messages":[{"role":"user","content":"hi"}],`+
			`"metadata":{"conversation_id":"conv-3"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := ca.chatCalls.Load(); got != 1 {
		t.Fatalf("codearts 的 Provider 被调用 %d 次，want 1", got)
	}

	// 关键：拿到的必须是**绑定的那个号**（ca-2），不是随便一个 codearts 号。
	sec, _ := ca.lastSecret.Load().(*fakeCodeartsSecret)
	if sec == nil || sec.AK != "CA2_AK" {
		t.Fatalf("粘性命中拿到的 Secret 是 %#v，want *fakeCodeartsSecret{AK:CA2_AK}（绑定的 ca-2）。\n"+
			"★ 粘性被绕过了 —— 同上游的粘性命中退化成了普通轮换。", ca.lastSecret.Load())
	}

	// 绑定保持不变（成功号 == 粘性号，不该被改写）。
	if uid, ok := h.cfg.Session.Resolve("conv-3"); !ok || uid != "ca-2" {
		t.Errorf("绑定应保持 ca-2，得到 %q ok=%v", uid, ok)
	}
}

// TestStickySameProviderBareModelStillReused 裸模型名（无前缀）走默认上游时，
// 粘性同样必须生效 —— 这是既有客户端的路径（默认上游 = workbuddy）。
func TestStickySameProviderBareModelStillReused(t *testing.T) {
	h, ca, wb, _ := newStickyMultiHandler(t)

	h.cfg.Session.Bind("conv-4", "wb-1")

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],`+
			`"metadata":{"conversation_id":"conv-4"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := wb.chatCalls.Load(); got != 1 {
		t.Fatalf("workbuddy 的 Provider 被调用 %d 次，want 1", got)
	}
	if got := ca.chatCalls.Load(); got != 0 {
		t.Errorf("codearts 的 Provider 被调用 %d 次，want 0", got)
	}
	if sec, _ := wb.lastSecret.Load().(*fakeWorkbuddySecret); sec == nil || sec.AccessToken != "WB_AT" {
		t.Fatalf("粘性命中没有命中 wb-1（Secret=%#v）", wb.lastSecret.Load())
	}
}
