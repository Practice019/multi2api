package admin

import "testing"

// 本文件：额度自动刷新的守门用例。见 quota_autorefresh.go。
//
// ⚠ 为什么值得一测：这个函数会**无人值守地**每 3 分钟跑一次，而它的失败模式
// 全是"悄悄不干活"：池子没接线时 panic 一次就没人再刷、账号列表读不到时静默返回 ——
// 症状都是"额度又变回旧的"，且**不会报错**。

// TestRefreshAllQuotasNowWithoutPoolIsSafe 没接线池子时返回零值，而不是 panic。
func TestRefreshAllQuotasNowWithoutPoolIsSafe(t *testing.T) {
	h := &Handler{}
	got := h.RefreshAllQuotasNow()
	if got.Updated != 0 || got.Failed != 0 || got.Skipped != 0 {
		t.Errorf("没接线池子时应当返回零值统计，实际 %+v", got)
	}
}

// TestRefreshAllQuotasNowNilReceiverIsSafe nil 接收者也不能 panic。
func TestRefreshAllQuotasNowNilReceiverIsSafe(t *testing.T) {
	var h *Handler
	got := h.RefreshAllQuotasNow()
	if got.Updated != 0 {
		t.Errorf("nil 接收者应当返回零值统计，实际 %+v", got)
	}
}

// TestStartQuotaAutoRefreshIsIdempotentAndNonBlocking 反复调用不会阻塞、不会打死自己。
func TestStartQuotaAutoRefreshIsIdempotentAndNonBlocking(t *testing.T) {
	h := &Handler{}
	for i := 0; i < 5; i++ {
		h.StartQuotaAutoRefresh() // ⚠ 必须**立刻返回**（首轮有 20 秒延迟）
	}
}
