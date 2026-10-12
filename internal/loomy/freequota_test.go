// freequota_test.go 「免费额度」列的数据源守卫。
//
// # 用户的要求
//
//	「账号管理里的 Loomy 上游，能不能再添加一列数据，用于显示这个账号的
//	  免费额度还剩多少？」
//
// 「免费额度」= 每日赠送（dailyBalance）—— 会按自然日重置、不用就浪费的那部分。
// 它与「额度」列是**总数与子集**的关系：availableBalance = 永久 + 每日。
//
// # 本文件最要紧的一条：三态
//
//	拿到分项且 > 0  → 显示数字
//	拿到分项且 = 0  → 显示 0（真的用完了）
//	没拿到分项      → 必须**不填**（界面 `—`）
//
// 第三条不是"保守"，而是唯一正确的答案：把"没读到"渲染成 0 会让用户读成
// 「免费额度已用完、账号正在烧永久积分」—— 一个**看起来完全合理但错误**的
// 结论，他会据此换号或删号。
//
// 这条纪律在本仓有前科（pool.restoreQuota 那个"凭空造出一个 0"的真实缺陷，
// 见 pool/quota.go 的注释），所以这里用两条反向用例把它钉住。
package loomy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/gateway"
)

// freeQuotaFixture 造一个只回积分明细的假上游 + 一份对应的凭证。
//
// balanceBody 是 `data` 里那一层的 JSON（原样回给调用方）。
func freeQuotaFixture(t *testing.T, uid, session, dataJSON string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/points/records" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("token") != session {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":"000000","desc":"ok","data":%s}`, dataJSON)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	writeFile(t, dir, "loomy-x.json", `{"session":"`+session+`","userid":"`+uid+`"}`)
	return NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir})
}

const (
	fqUID  = "260101010101010101"
	fqSess = "cccccccccccccccccccccccccccccccc"
)

// 有每日赠送 ⇒ 「免费额度」拿到具体数字，且**额度列仍是总数**。
func TestRefreshQuotaReportsFreeQuota(t *testing.T) {
	// 实测形态：balance 25000 + dailyBalance 1434 = availableBalance 26434
	p := freeQuotaFixture(t, fqUID, fqSess,
		`{"balance":25000,"dailyBalance":1434,"availableBalance":26434}`)

	qv, ok := p.RefreshQuota(fqUID)
	if !ok || !qv.HasData {
		t.Fatal("假上游可用时应当拿到额度")
	}
	if qv.Remaining != 26434 {
		t.Errorf("额度列应是**总数** 26434（永久+每日），实际 %d —— "+
			"两列不是重复：额度说「还能不能跑」，免费额度说「该不该现在用它」", qv.Remaining)
	}
	if !qv.FreeKnown {
		t.Fatal("回执里有 dailyBalance ⇒ FreeKnown 必须为 true")
	}
	if qv.Free != 1434 {
		t.Errorf("免费额度 = %d，want 1434（dailyBalance）", qv.Free)
	}
}

// 每日赠送确实是 0 ⇒ 如实报 0（这是"已用完"，与"没读到"不同）。
func TestRefreshQuotaFreeQuotaZeroIsKnown(t *testing.T) {
	p := freeQuotaFixture(t, fqUID, fqSess,
		`{"balance":19836,"dailyBalance":0,"availableBalance":19836}`)

	qv, _ := p.RefreshQuota(fqUID)
	if !qv.FreeKnown {
		t.Error("回执里**明确**有 dailyBalance:0 ⇒ FreeKnown 必须为 true")
	}
	if qv.Free != 0 {
		t.Errorf("免费额度 = %d，want 0", qv.Free)
	}
}

// ⚠ 反向用例：回执里**没有** dailyBalance（老接口形态）⇒ 必须不填分项。
//
// 若这里填 0，界面会显示「免费额度 0」，用户读成"已用完、正在烧永久积分"。
func TestRefreshQuotaFreeQuotaAbsentStaysUnknown(t *testing.T) {
	p := freeQuotaFixture(t, fqUID, fqSess, `{"balance":8000}`)

	qv, ok := p.RefreshQuota(fqUID)
	if !ok || !qv.HasData {
		t.Fatal("有 balance 就应当拿到额度（只是分项没有）")
	}
	if qv.Remaining != 8000 {
		t.Errorf("总额度 = %d，want 8000", qv.Remaining)
	}
	if qv.FreeKnown {
		t.Errorf("回执里没有 dailyBalance ⇒ 分项必须是**未知**（FreeKnown=false），"+
			"实际却给出了 Free=%d —— 界面会把它渲染成「免费额度 %d」＝「已用完」，"+
			"那是把未知说成事实", qv.Free, qv.Free)
	}
}

// 分项为负（理论透支）⇒ clamp 到 0，且仍算"已知"。
func TestRefreshQuotaFreeQuotaNegativeClamped(t *testing.T) {
	p := freeQuotaFixture(t, fqUID, fqSess,
		`{"balance":100,"dailyBalance":-5,"availableBalance":100}`)

	qv, _ := p.RefreshQuota(fqUID)
	if !qv.FreeKnown {
		t.Error("负数也是「读到了」，应当是已知")
	}
	if qv.Free != 0 {
		t.Errorf("负的剩余额度应 clamp 到 0，实际 %d", qv.Free)
	}
}

// 上游视图 → 核心视图的语义必须一致（WithFree 的不变量）。
func TestWithFreeMarksKnownAndClamps(t *testing.T) {
	q := gateway.CreditsQuota(100).WithFree(42)
	if !q.FreeKnown || q.Free != 42 {
		t.Errorf("WithFree(42) 应得 {Free:42, FreeKnown:true}，实际 %+v", q)
	}
	// 不调用 WithFree 就是"不区分" —— 这正是其余 10 个上游的形态。
	plain := gateway.CreditsQuota(100)
	if plain.FreeKnown {
		t.Error("没调 WithFree 的额度视图不该自称有分项信息")
	}
	// 负数 clamp。
	if q2 := gateway.CreditsQuota(1).WithFree(-9); q2.Free != 0 || !q2.FreeKnown {
		t.Errorf("负数应 clamp 成 0 且仍是已知，实际 %+v", q2)
	}
}

// ── 本机客户端缓存那条**回落路径** ────────────────────────────────────
//
// # 为什么必须单独测它（变异验证逼出来的）
//
// 上面几条用例都指向一个**能用的**假上游 ⇒ 走的是「积分网关」那条路。
// 而 RefreshQuota 还有第二条路：积分网关查不到时回落到**本机客户端的
// 积分摘要缓存**（只有"本机登录的那个号"有值）。
//
// 我把 clientstore 里记 DailyKnown 的那一行删掉做变异时，**全部用例仍然绿** ——
// 说明第二条路径上的分项接线**一条断言都没覆盖**。
// 它的后果不是崩溃：本机那个号的「免费额度」列会永远显示 `—`，
// 而别的号正常显示数字 —— 一个很容易被当成"那个号刚好查不到"的假象。

// 本机缓存里有 dailyBalance ⇒ 分项要带上。
func TestRefreshQuotaLocalCacheReportsFree(t *testing.T) {
	dir := pointsStoreDir(t, "260911173523492332", fixtureSession,
		`{"balance":8000,"dailyBalance":5000,"updatedAt":"2026-09-14T08:15:28.627Z"}`)
	// 不配 AuthDir ⇒ 找不到凭证 ⇒ 直接走本机缓存那条回落路径
	//（与既有的 TestRefreshQuotaReportsBalance 同一条路径）。
	p := NewWithConfig(Config{ClientDataDir: dir})

	qv, ok := p.RefreshQuota("260911173523492332")
	if !ok || !qv.HasData {
		t.Fatal("本机缓存里有本机登录那个号的摘要 ⇒ 应当拿到额度")
	}
	if !qv.FreeKnown {
		t.Fatalf("缓存里有 dailyBalance ⇒ 分项必须已知，实际 %+v —— "+
			"漏掉它会让本机那个号的「免费额度」列永远是 —", qv)
	}
	if qv.Free != 5000 {
		t.Errorf("免费额度 = %d，want 5000（缓存里的 dailyBalance）", qv.Free)
	}
}

// ⚠ 缓存里**只有 balance** ⇒ 分项必须未知。
//
// 实测客户端会写**部分更新**的记录（例如只有 balance 的那条），
// 所以"没写过 dailyBalance"是真实形态。把它当 0 会让界面显示
// 「免费额度已用完」—— 而那个号其实还有额度。
func TestRefreshQuotaLocalCacheWithoutDailyStaysUnknown(t *testing.T) {
	dir := pointsStoreDir(t, "260911173523492332", fixtureSession,
		`{"balance":8000,"updatedAt":"2026-09-14T03:04:34.627Z"}`)
	p := NewWithConfig(Config{ClientDataDir: dir})

	qv, ok := p.RefreshQuota("260911173523492332")
	if !ok || !qv.HasData {
		t.Fatal("有 balance 就应当拿到额度（只是分项没有）")
	}
	if qv.FreeKnown {
		t.Errorf("缓存里没有 dailyBalance ⇒ 分项必须**未知**（界面显示 —），"+
			"实际给出了 Free=%d ⇒ 会被渲染成「免费额度已用完」", qv.Free)
	}
}
