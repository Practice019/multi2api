// quota_ext_test.go —— 「额度」的守卫。
//
// # 这里最重要的一条是"别把别人的余额贴到这个号上"
//
// 客户端缓存是**本机登录的那一个账号**的，不是每账号一份。
// 若 RefreshQuota 不校验 uid，池里第二个 loomy 账号会显示第一个账号的余额 ——
// 那是一个**看起来完全合理的错数**：用户会照着它决定用哪个号、甚至据此删号。
// 比"显示 —"糟得多。
package loomy

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// pointsStoreDir 造一个"客户端已登录 + 有积分缓存"的假数据目录。
func pointsStoreDir(t *testing.T, uid, session, pointsJSON string) string {
	t.Helper()
	return writeStore(t,
		storeRecord(keyAuthSession,
			storeSessionJSON(session, uid, "150****3411", "2026-09-11T09:35:22.593Z")),
		storeRecord(keyPointsSummary, pointsJSON))
}

// TestRefreshQuotaReportsBalance 正常路径：读到总余额。
func TestRefreshQuotaReportsBalance(t *testing.T) {
	dir := pointsStoreDir(t, "260911173523492332", fixtureSession,
		`{"balance":8000,"dailyBalance":5000,"updatedAt":"2026-09-14T08:15:28.627Z"}`)
	p := NewWithConfig(Config{ClientDataDir: dir})

	qv, ok := p.RefreshQuota("260911173523492332")
	if !ok {
		t.Fatal("uid 存在时 ok 必须为 true")
	}
	if !qv.HasData {
		t.Fatal("有缓存时 HasData 必须为 true（否则界面显示 — 而数据其实在）")
	}
	if qv.Remaining != 8000 {
		t.Errorf("Remaining = %d，want 8000（balance 总余额）", qv.Remaining)
	}
	if qv.Kind != gateway.QuotaKindCredits {
		t.Errorf("Kind = %q，want %q", qv.Kind, gateway.QuotaKindCredits)
	}
}

// TestRefreshQuotaUnknownForOtherUID ★ 本机缓存只属于本机登录的那个账号。
//
// # 反向验证
//
// 把 uid 校验那一段去掉，这条会读到 8000 而变红 —— 那个数会被贴到一个
// 它并不属于的账号上。这正是断言要钉住的东西。
func TestRefreshQuotaUnknownForOtherUID(t *testing.T) {
	dir := pointsStoreDir(t, "260911173523492332", fixtureSession,
		`{"balance":8000,"dailyBalance":5000,"updatedAt":"2026-09-14T08:15:28.627Z"}`)
	p := NewWithConfig(Config{ClientDataDir: dir})

	qv, ok := p.RefreshQuota("另一个账号的-uid")
	if !ok {
		t.Fatal("uid 传进来了就应当是 ok=true + HasData=false（未知），而不是 ok=false")
	}
	if qv.HasData {
		t.Fatalf("★ 把本机登录账号的余额贴到了一个不属于它的 uid 上："+
			"Remaining=%d —— 这是「看起来完全合理的错数」，比显示 — 糟得多", qv.Remaining)
	}
}

// TestRefreshQuotaUnknownWithoutClientDir 本机没有客户端目录 → 未知。
func TestRefreshQuotaUnknownWithoutClientDir(t *testing.T) {
	// 配置一个不存在的目录，保证走"读不到"这条路（不依赖测试机上有没有装客户端）。
	p := NewWithConfig(Config{ClientDataDir: t.TempDir() + "/missing"})
	qv, ok := p.RefreshQuota("any-uid")
	if !ok {
		t.Fatal("ok 应当为 true（账号没问题，只是拿不到额度）")
	}
	if qv.HasData {
		t.Error("读不到客户端存储时必须 HasData=false（界面显示 —，而不是 0）")
	}
}

// TestRefreshQuotaUnknownWithoutPoints 登录态在、积分缓存不在 → 未知。
func TestRefreshQuotaUnknownWithoutPoints(t *testing.T) {
	dir := authStoreDir(t, "U1", fixtureSession, "150****3411",
		"2026-09-11T09:35:22.593Z")
	p := NewWithConfig(Config{ClientDataDir: dir})
	if qv, ok := p.RefreshQuota("U1"); !ok || qv.HasData {
		t.Errorf("没有积分缓存时必须未知，得到 %+v ok=%v", qv, ok)
	}
}

// TestRefreshQuotaClampsNegative 负余额 clamp 到 0。
//
// 负额度在展示与选号权重上都没有意义；负数权重会被当成
// "比没有记录还差"，从而让一个还能用的号被静默降权。
func TestRefreshQuotaClampsNegative(t *testing.T) {
	dir := pointsStoreDir(t, "U1", fixtureSession,
		`{"balance":-500,"updatedAt":"2026-09-14T08:15:28.627Z"}`)
	p := NewWithConfig(Config{ClientDataDir: dir})
	qv, ok := p.RefreshQuota("U1")
	if !ok || !qv.HasData {
		t.Fatalf("应当有数据，得到 %+v ok=%v", qv, ok)
	}
	if qv.Remaining != 0 {
		t.Errorf("Remaining = %d，want 0（负数 clamp）", qv.Remaining)
	}
}

// TestRefreshQuotaZeroBalanceIsNotUnknown 查到 0 与"读不到"必须不同。
//
// 这是 gateway.QuotaView.HasData 存在的全部理由：
//
//	HasData=false,        → 没查到       → 界面 `—`
//	HasData=true, Rem=0   → 查到了，是 0 → 界面 `0`（用户据此知道该换号）
func TestRefreshQuotaZeroBalanceIsNotUnknown(t *testing.T) {
	dir := pointsStoreDir(t, "U1", fixtureSession,
		`{"balance":0,"updatedAt":"2026-09-14T08:15:28.627Z"}`)
	p := NewWithConfig(Config{ClientDataDir: dir})
	qv, ok := p.RefreshQuota("U1")
	if !ok || !qv.HasData || qv.Remaining != 0 {
		t.Errorf("余额为 0 必须报 HasData=true 且 Remaining=0，得到 %+v", qv)
	}
}

// TestRefreshQuotaEmptyUIDIsNotOK 空 uid 是调用方传错了 → ok=false。
//
// 契约：ok=false 表示"这条 uid 我服务不了"（调用方据此跳过/404），
// 与"读了但不知道"（ok=true, HasData=false）是两件事。
func TestRefreshQuotaEmptyUIDIsNotOK(t *testing.T) {
	p := NewWithConfig(Config{ClientDataDir: t.TempDir()})
	if _, ok := p.RefreshQuota("  "); ok {
		t.Error("空 uid 应当返回 ok=false")
	}
}
