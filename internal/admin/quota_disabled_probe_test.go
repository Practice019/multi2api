package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// quota_disabled_probe_test.go —— 第四档作用域：只探测**已禁用**的号，探测可用则解禁。
//
// # 为什么这一档值得单独钉（用户 2026-11-12 要求）
//
// 用户原话：「刷新这个上游的全部额度……但是如果说我想把禁用的账号去探测一下，
// 那我这个其实会有很多已经正常的账号也探测，那就效率会变低，所以我需要再加一个按钮。」
//
// 两个语义必须同时成立，缺一个这个按钮就白做：
//
//	① **只**问禁用的号 —— 否则和「刷新本上游额度」没区别（效率就是这里的用户体验）；
//	② 探测到可用就**真的解禁并放回池子** —— 否则按钮点了「成功」但号回不来。

// TestQuotaRefreshOnlyDisabledScope 只问禁用号，且探测可用即解禁。
func TestQuotaRefreshOnlyDisabledScope(t *testing.T) {
	h, asked := newScopeFixture(t)
	p := h.cfg.Pool
	p.Disable("a1", "会话失效（自动或人工摘号）")
	// ⚠ 再压一层软冷却：被摘掉很久的号身上往往还压着冷却/熔断运行态，
	//   而 `Pool.Enable` **刻意不清**它 ⇒ 只 Enable 的后果是「界面已启用，选号仍跳过它」
	//   （healthy 要求 disabled/until/breakerUntil 三者全无）。这条断言把「真的放回池子」钉住。
	p.Cooldown("a1", pool.CoolSoft, 30*time.Minute, "429 历史冷却")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/quota/refresh",
		strings.NewReader(`{"provider":"alpha","only_disabled":true}`))
	h.accountsQuotaRefresh(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	// ① 只问了 a1（a2 / b1 都是正常号，不该被打上游）。
	if got := *asked; len(got) != 1 || got[0] != "a1" {
		t.Errorf("只该探测禁用的 a1，实际问了 %v —— 那就退化成「刷新本上游额度」了", got)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("回执不是 JSON: %v", err)
	}
	if out["count"] != float64(1) {
		t.Errorf("回执 count=%v want 1（探测的是禁用号数，不是该上游总数）", out["count"])
	}
	// ② 探测拿到数据 ⇒ 解禁（否则界面已启用、选号仍跳过它）。
	if st, ok := p.Status("a1"); !ok || st.Disabled {
		t.Error("探测证明可用，账号仍被禁用 —— 用户点这个按钮的目的就是把它放回来")
	}
	if st, _ := p.Status("a1"); st.Cooling {
		t.Error("解禁了却把历史冷却留在身上 —— 界面显示已启用，选号仍然跳过它（点了等于没点）")
	}
	if out["enabled"] != float64(1) {
		t.Errorf("回执 enabled=%v want 1（界面要靠它说「解除禁用 N」）", out["enabled"])
	}
}

// deadSessionQuotaStub 复刻 loomy 失效会话的**真实形状**：额度回答「我不知道」（HasData=false）
// **同时**通过 QuotaAuthFailureExt 自报「这次失败是账号层失效」。
//
// ⚠ 两个信号必须**同时**存在，这个桩才有意义：
//
//	只回 Unknown          ⇒ 走的还是「没有证据」这条（与本档要防的另一半相同）
//	Unknown + 自报失效     ⇒ 这才是真有判据的情形（loomy 实测就是这个形状）
//
// 而本档的误杀防线要求：**这两种都不能解禁**。
// （第一版我漏了自报失效这一半，于是"无证据也解禁"的变异**没被抓住** —— 见提交说明。）
type deadSessionQuotaStub struct{ id string }

func (s deadSessionQuotaStub) ID() string { return s.id }
func (s deadSessionQuotaStub) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapQuotaProbe
}
func (s deadSessionQuotaStub) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (s deadSessionQuotaStub) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (s deadSessionQuotaStub) RefreshQuota(string) (gateway.QuotaView, bool) {
	return gateway.UnknownQuota(), true
}

// QuotaAuthFailed 自报「这次失败是账号层失效」—— 与 loomy 的 100002 同形。
func (s deadSessionQuotaStub) QuotaAuthFailed(string) bool { return true }

// TestQuotaRefreshOnlyDisabledScopeKeepsUnprovenDisabled 探测**拿不到数据**时保持禁用。
//
// 这一档的动作是解禁，而解禁会把号放回候选集 ⇒ 只有**正面证据**才允许解禁。
// 拿 Unknown 去解禁 = 把坏号放回池子，而用户点这个按钮的目的正是「别再放回坏号」。
func TestQuotaRefreshOnlyDisabledScopeKeepsUnprovenDisabled(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(deadSessionQuotaStub{id: "alpha"}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("alpha", &auth.Auth{UID: "a1", Nickname: "a1"}, nil)
	p.Disable("a1", "会话失效")

	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true, Registry: reg, Pool: p, DefaultProvider: "alpha"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/quota/refresh",
		strings.NewReader(`{"provider":"alpha","only_disabled":true}`))
	h.accountsQuotaRefresh(rec, req)

	if st, _ := p.Status("a1"); !st.Disabled {
		t.Error("探测没拿到任何数据却解禁了 —— 没有任何证据就把号放回候选集")
	}
}
