// reset_dispatch_test.go —— P2 设计缺口：**无参回调 = 默认上游的隐式硬编码**。
//
// # 被钉住的缺陷
//
// `server.Config.NextResetAt` 原先是 `func() time.Time` —— 没有参数。
// 签名本身就排除了"按上游分派"的可能：无论注释怎么写，实现永远只能返回
// **同一个**上游的答案。装配层注入的是 `wb.NextResetAt`（workbuddy 的
// 次日 04:00），于是**任何**上游的账号在 ErrHardCredit 时都被冷到那一天。
//
// 对 codearts 这种**没有签到恢复机制**的上游，04:00 不是它的任何事实：
//
//	明明已恢复却还冷到次日凌晨 → 白白闲置近 24h
//	按 04:00 解冻而实际未恢复   → 又撞一次硬错误
//
// 讽刺之处：handler.go 的注释**早已写着正确的设计**
// （"出口层只问'这个号什么时候能再用'，具体策略由上游定义"），
// 但 `func() time.Time` 这个类型不允许它做那件事。
//
// # 本文件钉住什么（三个可观察判据，不是"实现里调了哪个函数"）
//
//  1. codearts 的号 ErrHardCredit 后 **不得**落在 workbuddy 的次日 04:00
//  2. workbuddy 的号 ErrHardCredit 后 **仍然**落在次日 04:00（回归，逐字不变）
//  3. 没有该信息的上游 → 回落 now+1h
//
// 判据 2 是硬要求：修好 codearts 不能把 workbuddy 改坏。
//
// # 为什么走完整的 HTTP 路径而不是直接调 applyErrorPolicy
//
// 因为缺陷的本质是**调用链上丢了 provider**：applyErrorPolicy 只拿得到 uid，
// 必须自己去池子反查归属（Pool.ProviderOf）。直接调它就等于把"反查"这一步
// 从被测范围里挖掉 —— 而那恰恰是最容易漏的一步，变异验证会因此假绿。
package server

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/workbuddy"
)

// wbNext4AM 是 workbuddy 的额度恢复策略（直接借用它的实现，
// 而不是在测试里重抄一份 04:00 —— 抄一份的话上游策略变了测试不会跟随）。
func wbNext4AM() time.Time { return workbuddy.NextCheckinReset(time.Now()) }

// resettingProvider 复刻 codearts 的形状：它是一个**真实的** gateway.Provider，
// 且**不实现** gateway.ResetPolicyExt —— 也就是"这个上游没有上报恢复排程"。
//
// 为什么用真 Provider 而不是再改一次 multiRouter：multiRouter 的 ResetAt 是
// 按 ExtOf 分派的**哨兵实现**，用它会掩盖"出口层到底问了谁"；这里要的是一个
// 与线上 registryRouter 逐字同形的装配层，所以把真实的扩展点发现机制接进来。
type resettingProvider struct {
	*recordingProvider
	// until 非 nil = 本上游上报恢复排程（workbuddy 的做法）
	// nil        = 本上游**没有**这个信息（codearts 的做法）
	until func() time.Time
}

var _ gateway.Provider = (*resettingProvider)(nil)

// SoftRateReset 测试桩：默认不给出模型级限时（保持账号级软冷却路径）。
func (p *resettingProvider) SoftRateReset(_ string, _ int, _ string) (time.Time, bool) {
	return time.Time{}, false
}

func (p *resettingProvider) ResetAt(_ gateway.Credential) (time.Time, bool) {
	if p.until == nil {
		return time.Time{}, false
	}
	return p.until(), true
}

// newResetDispatchHandler 造一个 "workbuddy(默认) + codearts" 的混池 handler，
// 装配层用**与 cmd/server 同形**的接线：
//
//	Config.NextResetAt = func(id) { 按 id 取该上游的 ResetPolicyExt }
//
// 两个 Recording Provider 轮流返回 402 额度耗尽，于是两条 chat 路径都会走到
// applyErrorPolicy 的 ErrKindHardCredit 分支。
func newResetDispatchHandler(t *testing.T) *Handler {
	t.Helper()

	p := pool.New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetDefaultProvider("workbuddy")

	caSecret := &fakeCodeartsSecret{AK: "CA_AK", SK: "CA_SK", DPoP: "CA_DPOP"}
	wbSecret := &fakeWorkbuddySecret{AccessToken: "WB_AT", RefreshToken: "WB_RT"}

	// ⚠ SyncToDirWithSecrets 是"按 provider 分域入池"的唯一入口。
	// 用 p.Add 的话 provider 标签是默认上游，codearts 的号会落进 workbuddy 域，
	// 本用例就退化成"单上游"而**假绿**。
	p.SyncToDirWithSecrets("codearts", []*auth.Auth{{UID: "ca-1", Nickname: "codearts-号"}},
		map[string]any{"ca-1": caSecret})
	p.SyncToDirWithSecrets("workbuddy", []*auth.Auth{{UID: "wb-1", Nickname: "workbuddy-号"}},
		map[string]any{"wb-1": wbSecret})

	p.SetCredits("ca-1", 100000)
	p.SetCredits("wb-1", 100000)
	p.SetMaxInFlight(10)

	// 402 + 额度耗尽关键词 → gateway.ErrKindHardCredit（两个上游都命中）。
	const hardCreditBody = `{"error":"insufficient credit, quota exhausted"}`

	ca := &resettingProvider{
		recordingProvider: &recordingProvider{id: "codearts", status: 402, body: hardCreditBody},
		// codearts：**没有**恢复排程（线上就是 ResetPolicyExt 的 ok=false）
		until: nil,
	}
	wb := &resettingProvider{
		recordingProvider: &recordingProvider{id: "workbuddy", status: 402, body: hardCreditBody},
		// workbuddy：次日 04:00（等 09:00/21:00 签到恢复）
		until: wbNext4AM,
	}

	router := &multiRouter{
		def:   "workbuddy",
		reg:   map[string]*recordingProvider{"codearts": ca.recordingProvider, "workbuddy": wb.recordingProvider},
		p:     p,
		creds: map[string]any{"ca-1": caSecret, "wb-1": wbSecret},
	}
	// ⚠ 把**带扩展点的**实例放进另一张表，供装配层做 ExtOf 发现。
	// multiRouter 自己只认 recordingProvider（那是既有用例的契约），
	// 这里不动它，改用一层薄适配绕过 —— 见 extRouter 的注释。
	ext := &extRouter{multiRouter: router, ext: map[string]gateway.Provider{
		"codearts": ca, "workbuddy": wb,
	}}

	h := NewHandler(Config{
		Pool:            p,
		Upstream:        newFakeUpstream(t, func(string) (int, string, bool) { return 500, `{}`, false }),
		Provider:        ext,
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
		// === 装配层接线（与 cmd/server/main.go 逐字同形）===
		//
		// 按 id 问该上游自己的排程；没人上报就 ok=false，
		// 核心据此回落 now+1h。**刻意不塞固定时刻** —— 塞固定值会让
		// 本用例在修复前的代码下也变绿，那样的测试没有变异验证能力。
		NextResetAt: func(providerID string) (time.Time, bool) {
			pv, ok := ext.ext[providerID]
			if !ok {
				return time.Time{}, false
			}
			ext2, ok := gateway.ExtOf[gateway.ResetPolicyExt](pv)
			if !ok {
				return time.Time{}, false
			}
			return ext2.ResetAt(gateway.Credential{Provider: providerID})
		},
	})
	return h
}

// extRouter 在 multiRouter 之上只替换"扩展点发现"这一步。
//
// 为什么必须这样：`multiRouter.ResetAt` 用的是 ExtOf（正确的分派模式），
// 但它查的是 `r.reg` 里的 *recordingProvider —— 那个类型**没有**实现
// gateway.ResetPolicyExt，所以它永远返回 ok=false。
// 那样本用例只会测到"回落"一条路径，workbuddy 的回归判据 2 就无法成立。
//
// 于是另起一张 ext 表，把**真正带扩展点**的实例交给装配层。
// 其余方法原样转发，出口层看到的行为与线上 registryRouter 一致。
type extRouter struct {
	*multiRouter
	ext map[string]gateway.Provider
}

var _ ProviderRouter = (*extRouter)(nil)

// SoftRateReset 测试桩：默认不给出模型级限时（保持账号级软冷却路径）。
func (r *extRouter) SoftRateReset(_ string, _ int, _ string) (time.Time, bool) {
	return time.Time{}, false
}

func (r *extRouter) ResetAt(id string, cred gateway.Credential) (time.Time, bool) {
	pv, ok := r.ext[id]
	if !ok {
		return time.Time{}, false
	}
	ext2, ok := gateway.ExtOf[gateway.ResetPolicyExt](pv)
	if !ok {
		return time.Time{}, false
	}
	return ext2.ResetAt(cred)
}

// TestHardCreditCooldownDispatchesPerProvider 是本修复的**主闸门**。
//
// 池子里一个 codearts 号 + 一个 workbuddy 号，两条请求各自触发 402 额度耗尽，
// 然后断言两个号落到的冷却截止时刻**不同**：
//
//	codearts  → 上游没上报排程 → now+1h（通用保守值）
//	workbuddy → 上游上报了     → 次日 04:00（回归，逐字不变）
//
// # 变异验证（必须红）
//
// 把 handler.go 的 nextResetAt 退回"无参回调"的形状：
//
//	func (h *Handler) nextResetAt(uid string) time.Time {
//	    if h.cfg.NextResetAt != nil {
//	        if u, ok := h.cfg.NextResetAt(""); ok { return u }   // ← 忽略 uid
//	    }
//	    return time.Now().Add(time.Hour)
//	}
//
// 并让配置里的回调忽略 id（等价于改造前的 `func() time.Time`）：
//
//	NextResetAt: func(string) (time.Time, bool) { return wbNext4AM(), true }
//
// → codearts 的号也落在次日 04:00 → **判据 1 红**（距离 50~70 分钟那条），
//
//	而 workbuddy 的两条断言仍绿 —— 这正说明本用例能区分两条上游，
//	不是"随便断言一个就过"。
func TestHardCreditCooldownDispatchesPerProvider(t *testing.T) {
	h := newResetDispatchHandler(t)

	// ---- 判据 1：codearts 必须**不**落在 workbuddy 的次日 04:00 ----
	if code, body := chatOnce(t, h, "codearts/glm-5.3-flash"); code != 503 {
		t.Fatalf("codearts 请求 code=%d want 503 body=%s", code, body)
	}
	caSt := statusOf(t, h, "ca-1")
	if !caSt.Cooling {
		t.Fatalf("codearts 的号应进入冷却: %+v", caSt)
	}
	if caSt.CoolKind != pool.CoolHard.String() {
		t.Errorf("codearts 的号应为 hard 冷却，得到 %q", caSt.CoolKind)
	}
	d := time.Until(caSt.Until)
	if d < 50*time.Minute || d > 70*time.Minute {
		t.Errorf("★ codearts 的硬冷却时长 %v 不是通用保守值（约 1h）\n"+
			"  截止时刻 = %s\n"+
			"  这说明它被套上了**别的上游**的恢复排程（P2：无参回调的隐式硬编码）。\n"+
			"  codearts 没有签到恢复机制，次日 04:00 不是它的任何事实。",
			d, caSt.Until.Format(time.RFC3339))
	}
	// 更直白的一条：绝不能正好落在 workbuddy 的那个 04:00 上。
	if wb := wbNext4AM(); caSt.Until.Sub(wb).Abs() < time.Minute {
		t.Errorf("★ codearts 的冷却截止 %s 就是 workbuddy 的次日 04:00 —— "+
			"上游的策略被硬编码到了另一个上游身上", caSt.Until.Format(time.RFC3339))
	}

	// ---- 判据 2：workbuddy 必须**仍然**是次日 04:00（回归，逐字不变）----
	if code, body := chatOnce(t, h, "workbuddy/auto"); code != 503 {
		t.Fatalf("workbuddy 请求 code=%d want 503 body=%s", code, body)
	}
	wbSt := statusOf(t, h, "wb-1")
	if !wbSt.Cooling {
		t.Fatalf("workbuddy 的号应进入冷却: %+v", wbSt)
	}
	want := wbNext4AM()
	if got := wbSt.Until; got.Sub(want).Abs() > 2*time.Second {
		t.Errorf("★ workbuddy 的硬冷却截止 %s，want 次日 04:00 ≈ %s（差 %v）\n"+
			"  这是回归：修好 codearts 不得改变 workbuddy 的既有行为。",
			got.Format(time.RFC3339), want.Format(time.RFC3339), got.Sub(want))
	}
	if hh := wbSt.Until.Hour(); hh != 4 || wbSt.Until.Minute() != 0 || wbSt.Until.Second() != 0 {
		t.Errorf("workbuddy 的冷却截止应为 04:00:00 整点，得到 %s",
			wbSt.Until.Format(time.RFC3339))
	}
}

// TestHardCreditCooldownFallsBackWhenProviderSilent 判据 3：
// 上游**没有**上报恢复排程时，回落 now+1h（而不是某个别的上游的排程）。
//
// 本用例把 codearts 换成"**完全不实现** gateway.ResetPolicyExt 的上游"
// （即 ExtOf 落空的那一类），覆盖"没有该信息"的另一种写法。
// 线上 codearts 走的是 ok=false 那条（见 internal/codearts/resetpolicy.go），
// 两条路径在装配层都归到同一个 ok=false，出口层的取舍必须一致。
func TestHardCreditCooldownFallsBackWhenProviderSilent(t *testing.T) {
	h := newResetDispatchHandler(t)
	// 让 codearts 的实例**不带**扩展点：ExtOf 落空。
	ext := h.cfg.Provider.(*extRouter)
	ext.ext["codearts"] = ext.ext["codearts"].(*resettingProvider).recordingProvider

	if code, body := chatOnce(t, h, "codearts/glm-5.3-flash"); code != 503 {
		t.Fatalf("code=%d want 503 body=%s", code, body)
	}
	// ⚠ 这里**不能**断言"核心问了 h.cfg.NextResetAt"。
	//
	// 我上一版就是这么写的（`asked` 计数器），它红了 —— 而**红得对**：
	// 多上游模式下 `nextResetAt` 的结构是
	//
	//	if h.cfg.Provider != nil {
	//	    ... 问该上游的 ResetAt ...
	//	    return time.Now().Add(time.Hour)     // ← 直接回落，**从不**碰注入的回调
	//	}
	//	// 下面才是单上游回退，用 h.cfg.NextResetAt
	//
	// 也就是：**`h.cfg.NextResetAt` 是单上游回退专用的**，
	// 多上游路径有它自己的回落（now+1h）。断言它被调用 = 把两条回退路径搞混，
	// 是**测试探错了层次**，不是产品缺陷。
	//
	// 真正该钉的是下面那条：**冷却截止落在 now+1h 附近**。
	st := statusOf(t, h, "ca-1")
	if !st.Cooling || st.CoolKind != pool.CoolHard.String() {
		t.Fatalf("应进入 hard 冷却: %+v", st)
	}
	if d := time.Until(st.Until); d < 50*time.Minute || d > 70*time.Minute {
		t.Errorf("★ 上游没上报排程时应回落 now+1h，实际距现在 %v（截止 %s）",
			d, st.Until.Format(time.RFC3339))
	}
}

// TestHardCreditCooldownUnknownUIDFallsBack 号不在池里时**不猜**成默认上游。
//
// 判据：ProviderOf 失败 → 回落通用值，而不是拿默认上游的次日 04:00 顶上。
// "拿 A 的事实回答 B 的问题"正是本 bug 的形态，这里把它堵在第二处。
func TestHardCreditCooldownUnknownUIDFallsBack(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "ghost", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	// 直接从池子里摘掉这个号，模拟"冷却时号已被删除"的竞态。
	if !h.cfg.Pool.Remove("ghost") {
		t.Fatal("未能从池子里移除 ghost")
	}
	got := h.nextResetAt("ghost")
	if d := time.Until(got); d < 50*time.Minute || d > 70*time.Minute {
		t.Errorf("★ 未知 uid 应回落 now+1h，实际 %v（%s）—— "+
			"不存在的号没有上游归属，不能猜成默认上游", d, got.Format(time.RFC3339))
	}
}

