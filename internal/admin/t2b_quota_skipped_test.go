package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// ---------------------------------------------------------------------------
// 本文件守住"被跳过的上游必须可见"。
//
// 背景（本 bug 能藏这么久的直接原因）：
//
//	ext, ok := exts[provider]
//	if !ok {
//	    continue // ← 曾经是一句光秃秃的 continue
//	}
//
// 这一行**没有任何可观测性**：没有日志、没有计数、被跳过的上游
// 连名字都不出现在回执的 providers 里。于是"某个上游整片账号没被刷到"
// 在日志、回执、界面上全部不可见 —— 实测回执只有
// `updated=1 unknown=0 failed=0 providers=codearts`，
// "workbuddy" 三个字在任何地方都看不到。
//
// 下面三条断言分别守住：计数、回执字段、以及"不要把正常情况算成跳过"。
// ---------------------------------------------------------------------------

// TestRefreshQuotas_SkippedCountsUpstreamWithoutExt 守卫 1。
//
// 有"没实现 QuotaExt"的上游时，Skipped 必须 > 0。
//
// ⚠ 断言写成 `!= 1`（而不是 `<= 0`）是刻意的：它同时排除
// "Skipped 恒等于某个非零常数"这类假实现。
//
// 变异验证：把 refreshQuotas 里的 `out.Skipped++` 删掉，本测试必须红。
func TestRefreshQuotas_SkippedCountsUpstreamWithoutExt(t *testing.T) {
	// workbuddy 没实现 QuotaExt；codearts 实现了。
	silent := &quotaSilentStub{id: "workbuddy"}
	ca := &quotaStub{id: "codearts", view: gateway.CreditsQuota(7474), ok: true}
	h, _, _ := newQuotaTestHandler(t, silent, ca)

	res := h.refreshQuotas([]string{"wb-1", "ca-1"})

	if res.Skipped != 1 {
		t.Fatalf("workbuddy 的账号没被问过额度，应记 skipped=1，得到 skipped=%d "+
			"（updated=%d unknown=%d failed=%d providers=%v）\n"+
			"skipped=0 意味着「未被问过的上游」在回执里依然不可见 —— 这正是本 bug 藏住的直接原因",
			res.Skipped, res.Updated, res.Unknown, res.Failed, res.Providers)
	}
	// 交叉校验：跳过**不是**失败，也不是未知 —— 三者必须互不混淆。
	if res.Failed != 0 {
		t.Errorf("跳过不等于失败：上游没实现 QuotaExt 不是故障，得到 failed=%d", res.Failed)
	}
	if res.Unknown != 0 {
		t.Errorf("跳过不等于未知：Unknown 是「问了，上游说不知道」，得到 unknown=%d", res.Unknown)
	}
	// Providers 仍然只收录被真正问过的上游（行为不变）。
	if len(res.Providers) != 1 || res.Providers[0] != "codearts" {
		t.Errorf("providers 应只有真正被问过的 codearts，得到 %v", res.Providers)
	}
}

// TestRefreshQuotas_SkippedCountsEveryAccount 多账号时逐个计数。
//
// 守卫 1 的加强版：Skipped 是**账号数**，不是一个布尔标志。
// 一个上游有 N 个账号被跳过就是 N。
func TestRefreshQuotas_SkippedCountsEveryAccount(t *testing.T) {
	silent := &quotaSilentStub{id: "workbuddy"}
	ca := &quotaStub{id: "codearts", view: gateway.CreditsQuota(1), ok: true}
	h, p, _ := newQuotaTestHandler(t, silent, ca)

	// 再往 workbuddy 加两个账号（它的上游没有 QuotaExt）。
	p.AddFor("workbuddy", &auth.Auth{UID: "wb-2"}, nil)
	p.AddFor("workbuddy", &auth.Auth{UID: "wb-3"}, nil)

	res := h.refreshQuotas([]string{"wb-1", "wb-2", "wb-3", "ca-1"})

	if res.Skipped != 3 {
		t.Fatalf("workbuddy 的 3 个账号都没被问过，应记 skipped=3，得到 %d", res.Skipped)
	}
	if res.Updated != 1 {
		t.Errorf("只有 codearts 的账号被刷新，得到 updated=%d", res.Updated)
	}
}

// TestQuotaRefreshResponse_ContainsSkippedField 守卫 2。
//
// 回执 JSON **必须**含 `skipped` 字段。
//
// # 为什么要断言"字段存在"而不只是"计数对"
//
// 计数对了但没写进回执，前端就只剩两个分支（updated / failed），
// 会**静默走错分支**把它当成"全都很正常"。所以这里直接解 JSON 看字段。
func TestQuotaRefreshResponse_ContainsSkippedField(t *testing.T) {
	silent := &quotaSilentStub{id: "workbuddy"}
	ca := &quotaStub{id: "codearts", view: gateway.CreditsQuota(7474), ok: true}
	h, _, _ := newQuotaTestHandler(t, silent, ca)

	rec := httptest.NewRecorder()
	h.accountsQuotaRefresh(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/quota/refresh", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("回执状态码应为 200，得到 %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("回执不是合法 JSON: %v\n%s", err, rec.Body.String())
	}

	raw, ok := body["skipped"]
	if !ok {
		t.Fatalf("回执 JSON 缺少 `skipped` 字段 —— 前端无法区分「上游整片被跳过」"+
			"和「一切正常」，会静默走错分支。实际字段: %v\n回执: %s",
			keysOfReceipt(body), rec.Body.String())
	}
	// JSON 解出来是 float64。
	n, ok := raw.(float64)
	if !ok {
		t.Fatalf("`skipped` 应是数字，得到 %T (%v)", raw, raw)
	}
	if int(n) != 1 {
		t.Errorf("回执里 skipped 应为 1（workbuddy 被跳过），得到 %v\n回执: %s", n, rec.Body.String())
	}
}

// TestRefreshQuotas_NoSkipWhenAllUpstreamsHaveExt 守卫 3。
//
// 所有上游都实现了 QuotaExt 时，Skipped 必须是 **0**。
//
// # 为什么这条不能少
//
// 没有它，"Skipped 恒为非零常数"（比如误写成 providers 数、或者
// 混进了其它计数）也会让守卫 1 通过。正常情况被算成跳过，
// 就会把告警变成噪音，然后被无视 —— 等于又回到"不可见"。
func TestRefreshQuotas_NoSkipWhenAllUpstreamsHaveExt(t *testing.T) {
	wb := &quotaStub{id: "workbuddy", view: gateway.CreditsQuota(1136), ok: true}
	ca := &quotaStub{id: "codearts", view: gateway.CreditsQuota(7474), ok: true}
	h, _, _ := newQuotaTestHandler(t, wb, ca)

	res := h.refreshQuotas([]string{"wb-1", "ca-1"})

	if res.Skipped != 0 {
		t.Fatalf("两个上游都实现了 QuotaExt，不该有任何跳过，得到 skipped=%d "+
			"（updated=%d unknown=%d failed=%d）", res.Skipped, res.Updated, res.Unknown, res.Failed)
	}
	if res.Updated != 2 {
		t.Errorf("两个账号都该被刷新，得到 updated=%d", res.Updated)
	}
}

// TestRefreshQuotas_UnknownIsNotCountedAsSkipped 守卫 3 的边界。
//
// 上游实现了 QuotaExt 但如实回答"我不知道" → 记 Unknown，**不记 Skipped**。
//
// ⚠ 这两者混同会把一个**正常**状态报成"上游没接额度"，
// 从而把真正的 skipped 淹没掉。注释里对这两者的区分必须由测试守住。
func TestRefreshQuotas_UnknownIsNotCountedAsSkipped(t *testing.T) {
	wb := &quotaStub{id: "workbuddy", view: gateway.UnknownQuota(), ok: true}
	ca := &quotaStub{id: "codearts", view: gateway.CreditsQuota(2), ok: true}
	h, _, _ := newQuotaTestHandler(t, wb, ca)

	res := h.refreshQuotas([]string{"wb-1", "ca-1"})

	if res.Unknown != 1 {
		t.Errorf("应记 unknown=1，得到 %d", res.Unknown)
	}
	if res.Skipped != 0 {
		t.Errorf("「问了但不知道」不是跳过，skipped 应为 0，得到 %d", res.Skipped)
	}
	// 被问过的上游（哪怕它回答"不知道"）仍应出现在 providers 里 ——
	// 这正是它和 skipped 的可观测差异。
	if len(res.Providers) != 2 {
		t.Errorf("两个上游都被问过（含回答不知道的那个），providers 应为 2 个，得到 %v", res.Providers)
	}
}

// TestRefreshQuotas_EmptyProviderFallsBackAndStillCountsSkipped
// 老状态文件（provider 为空 → 落到默认上游）也必须能被计数。
//
// # 为什么值得单列
//
// 空 provider 走的是 `provider = h.cfg.DefaultProvider` 这条分支。
// 如果计数写在解析 provider **之前**，这条路径的账号会漏计 ——
// 而它恰好是"最老的那批账号"，最可能是被静默跳过的受害者。
func TestRefreshQuotas_EmptyProviderFallsBackAndStillCountsSkipped(t *testing.T) {
	// 默认上游 workbuddy 没实现 QuotaExt。
	silent := &quotaSilentStub{id: "workbuddy"}
	h, p, _ := newQuotaTestHandler(t, silent)

	// 造一个 provider 为空的账号（模拟旧状态文件）。
	p.AddFor("", &auth.Auth{UID: "legacy-1"}, nil)

	res := h.refreshQuotas([]string{"legacy-1"})

	if res.Skipped != 1 {
		t.Fatalf("provider 为空并回落到默认上游 workbuddy（无 QuotaExt）时也要计数，得到 skipped=%d", res.Skipped)
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func keysOfReceipt(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
