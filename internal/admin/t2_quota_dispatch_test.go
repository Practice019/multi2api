package admin

import (
	"context"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// ---------------------------------------------------------------------------
// T2 测试替身：一个"会报额度"的上游 + 一个"不会报额度"的上游。
//
// 为什么用两个而不是一个：这条路径的价值全在**分派**上 ——
// "按账号自己的 provider 去找上游"。只有一个上游时，
// 即使分派写错了（拿全局默认上游去问所有账号）测试也照样绿。
// ---------------------------------------------------------------------------

// quotaStub 实现 gateway.Provider + gateway.QuotaExt。
type quotaStub struct {
	id string
	// called 记录被问过额度的 uid（供断言"问对了号/没问错号"）。
	called []string
	// view 固定返回的额度；ok 控制第二返回值。
	view gateway.QuotaView
	ok   bool
	// panicOn 为真时 RefreshQuota 直接 panic（测 safeRefreshQuota 的兜底）。
	panicOn bool
	// noExt 为真时不实现 QuotaExt（用另一个类型），这里用字段模拟不方便，
	// 所以"不报额度的上游"由 quotaSilentStub 承担。
	_ struct{}
}

func (s *quotaStub) ID() string               { return s.id }
func (s *quotaStub) Caps() gateway.Capability { return gateway.CapChat }
func (s *quotaStub) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (s *quotaStub) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

func (s *quotaStub) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	if s.panicOn {
		panic("上游实现违约：RefreshQuota panic（测试注入）")
	}
	s.called = append(s.called, uid)
	return s.view, s.ok
}

// quotaSilentStub 是一个**没有** QuotaExt 的上游（只实现 Provider）。
type quotaSilentStub struct{ id string }

func (s *quotaSilentStub) ID() string               { return s.id }
func (s *quotaSilentStub) Caps() gateway.Capability { return gateway.CapChat }
func (s *quotaSilentStub) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (s *quotaSilentStub) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// 测试骨架
// ---------------------------------------------------------------------------

// newQuotaTestHandler 建一个带池 + 注册表的 handler。
//
// 池里预置：一个 workbuddy 账号、一个 codearts 账号。
func newQuotaTestHandler(t *testing.T, providers ...gateway.Provider) (*Handler, *pool.Pool, *gateway.Registry) {
	t.Helper()
	reg := gateway.NewRegistry()
	for _, p := range providers {
		if err := reg.Register(p); err != nil {
			t.Fatalf("注册上游 %s 失败: %v", p.ID(), err)
		}
	}

	p := pool.New("")
	p.AddFor("workbuddy", &auth.Auth{UID: "wb-1"}, nil)
	p.AddFor("codearts", &auth.Auth{UID: "ca-1"}, nil)

	h := New(Config{Pool: p, Registry: reg, DefaultProvider: "workbuddy"})
	return h, p, reg
}

// TestRefreshQuotas_DispatchesPerAccountProvider 分派必须按**账号自己**的上游。
//
// ⚠ **这是 T2 的核心断言。**
//
// 变异验证（见交付报告）：把 refreshQuotas 里的
//
//	provider := st.Provider; ... ext := exts[provider]
//
// 改成"只对 workbuddy 生效"（即固定用默认上游），本测试必须红 ——
// 因为 codearts 的账号会拿不到额度。
func TestRefreshQuotas_DispatchesPerAccountProvider(t *testing.T) {
	wb := &quotaStub{id: "workbuddy", view: gateway.CreditsQuota(1136), ok: true}
	ca := &quotaStub{id: "codearts", view: gateway.CreditsQuota(7474), ok: true}
	h, p, _ := newQuotaTestHandler(t, wb, ca)

	res := h.refreshQuotas([]string{"wb-1", "ca-1"})

	if res.Updated != 2 {
		t.Fatalf("两个账号都应刷新成功，得到 updated=%d（unknown=%d failed=%d）",
			res.Updated, res.Unknown, res.Failed)
	}

	// 每个上游各自只被问到自己的账号 —— 交叉问会写错数。
	if len(wb.called) != 1 || wb.called[0] != "wb-1" {
		t.Errorf("workbuddy 应只被问 wb-1，实际 %v", wb.called)
	}
	if len(ca.called) != 1 || ca.called[0] != "ca-1" {
		t.Errorf("codearts 应只被问 ca-1，实际 %v —— 分派可能没按账号的 provider 走", ca.called)
	}

	// 额度必须落到**正确的账号**上。
	stWB, _ := p.Status("wb-1")
	if stWB.Quota.Remaining != 1136 {
		t.Errorf("wb-1 额度应为 1136，得到 %+v", stWB.Quota)
	}
	stCA, _ := p.Status("ca-1")
	if stCA.Quota.Remaining != 7474 {
		t.Errorf("ca-1 额度应为 7474，得到 %+v —— codearts 的额度没有写回池", stCA.Quota)
	}
	if !stCA.Quota.HasData {
		t.Errorf("ca-1 的额度必须标为有数据：%+v", stCA.Quota)
	}
}

// TestRefreshQuotas_UpstreamWithoutExtStaysUnknown 不报额度的上游必须保持未知。
//
// 这是用户要的"有什么显示什么，没有就不显示"：
// 上游不报额度时**不写池**，界面显示 `—`，**不是 0**。
func TestRefreshQuotas_UpstreamWithoutExtStaysUnknown(t *testing.T) {
	silent := &quotaSilentStub{id: "workbuddy"}
	ca := &quotaStub{id: "codearts", view: gateway.CreditsQuota(7474), ok: true}
	h, p, _ := newQuotaTestHandler(t, silent, ca)

	res := h.refreshQuotas([]string{"wb-1", "ca-1"})

	if res.Updated != 1 {
		t.Fatalf("只有 codearts 应被刷新，得到 updated=%d", res.Updated)
	}

	stWB, _ := p.Status("wb-1")
	if stWB.Quota.HasData {
		t.Errorf("没有 QuotaExt 的上游，其账号不该被写成「有数据」：%+v\n"+
			"（尤其不能写 0 —— 0 会被读成「没额度了」）", stWB.Quota)
	}
	if stWB.Quota.Remaining != 0 {
		t.Errorf("不该凭空写额度：%+v", stWB.Quota)
	}
}

// TestRefreshQuotas_UnknownNotWrittenAsZero 上游如实回答"我不知道"时不得写 0。
//
// ⚠ 这条直接守住"不造假数据"：`HasData=false` 的返回**必须不写池**。
// 若实现里把 `!qv.HasData` 那条 continue 删掉，本测试会红 ——
// 因为 SetQuota 会把池里的额度覆盖成 `{HasData:false}`，
// 而池里原本可能有真数据。
func TestRefreshQuotas_UnknownNotWrittenAsZero(t *testing.T) {
	// 上游说"我不知道"。
	wb := &quotaStub{id: "workbuddy", view: gateway.UnknownQuota(), ok: true}
	h, p, _ := newQuotaTestHandler(t, wb)

	// 池里先放一份真数据（模拟上一次成功刷新的结果）。
	p.SetQuota("wb-1", pool.QuotaView{
		Kind: pool.QuotaKindCredits, Remaining: 1136, HasData: true,
	})

	res := h.refreshQuotas([]string{"wb-1"})

	if res.Unknown != 1 {
		t.Fatalf("应记为 unknown=1，得到 %d", res.Unknown)
	}
	if res.Updated != 0 {
		t.Fatalf("未知不该被算作更新，得到 updated=%d", res.Updated)
	}

	st, _ := p.Status("wb-1")
	if !st.Quota.HasData || st.Quota.Remaining != 1136 {
		t.Errorf("上游回答未知时**不该覆盖池里的真数据**，得到 %+v", st.Quota)
	}
}

// TestRefreshQuotas_PanicInOneUpstreamDoesNotKillOthers panic 只降级该账号。
//
// 遍历所有上游跑，一个上游的 bug 不该连累其它上游的额度也刷不出来。
func TestRefreshQuotas_PanicInOneUpstreamDoesNotKillOthers(t *testing.T) {
	bad := &quotaStub{id: "workbuddy", panicOn: true}
	good := &quotaStub{id: "codearts", view: gateway.CreditsQuota(7474), ok: true}
	h, p, _ := newQuotaTestHandler(t, bad, good)

	res := h.refreshQuotas([]string{"wb-1", "ca-1"})

	if res.Failed != 1 {
		t.Errorf("panic 的那个账号应记为 failed=1，得到 %d", res.Failed)
	}
	if res.Updated != 1 {
		t.Errorf("正常上游必须仍被刷新，得到 updated=%d", res.Updated)
	}
	st, _ := p.Status("ca-1")
	if st.Quota.Remaining != 7474 {
		t.Errorf("codearts 的额度被 panic 连累了：%+v", st.Quota)
	}
}

// TestRefreshQuotas_ReportsProviders 回执要报告真正被问过的上游。
//
// 为什么重要：前端据此判断"这次刷新到底覆盖了谁"，
// 也是排查"某个上游的额度一直是 —"的第一手信息。
func TestRefreshQuotas_ReportsProviders(t *testing.T) {
	wb := &quotaStub{id: "workbuddy", view: gateway.CreditsQuota(1), ok: true}
	ca := &quotaStub{id: "codearts", view: gateway.CreditsQuota(2), ok: true}
	h, _, _ := newQuotaTestHandler(t, wb, ca)

	res := h.refreshQuotas([]string{"wb-1", "ca-1"})

	if len(res.Providers) != 2 {
		t.Fatalf("应报告 2 个上游，得到 %v", res.Providers)
	}
	// 稳定排序（避免前端因为 map 遍历顺序抖动而重复渲染）。
	if res.Providers[0] != "codearts" || res.Providers[1] != "workbuddy" {
		t.Errorf("上游列表应排序输出，得到 %v", res.Providers)
	}
}
