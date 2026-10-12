// freequota_test.go 「免费额度」列在**核心侧**的契约守卫。
//
// # 为什么在这里再测一遍（Loomy 侧已经测过语义）
//
// 因为前端看到的不是 Loomy 的 `gateway.QuotaView`，而是**这一层拼出来的 JSON**。
// 中间隔着两跳（gateway → pool → admin），每跳都可能把"未知"漏成 0：
//
//	gateway.QuotaView.Free/FreeKnown
//	  → toPoolQuota（逐字段搬，漏一个就退化成 0）
//	  → pool.Status.Quota
//	  → AccountView.FreeQuota（指针，omitempty）
//
// 所以本文件断言的是**原始 JSON**：字段在不在、值是多少。
// 那是前端 `freeQuotaCellHTML` 真正读的东西，也是本列最容易骗人的地方
// （"没读到"渲染成 0 ⇒ 用户以为账号正在烧永久积分）。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// gatewayQuota 造一个 `gateway.QuotaView`（`toPoolQuota` 的入参）。
//
// freeKnown=false 时**不调用** WithFree —— 那正是"该上游不区分这个分项"
// 的合法形态（其余 10 个上游都是这样）。
func gatewayQuota(free int64, freeKnown bool) gateway.QuotaView {
	q := gateway.CreditsQuota(100)
	if freeKnown {
		q = q.WithFree(free)
	}
	return q
}

// freeQuotaJSON 打一次 /admin/accounts，取出唯一那个账号的原始 JSON。
func freeQuotaJSON(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200（body=%s）", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("账号数 = %d，want 1", len(out.Accounts))
	}
	return out.Accounts[0]
}

// newFreeQuotaHandler 造一个"池里有一个账号、额度视图由参数决定"的 handler。
func newFreeQuotaHandler(t *testing.T, q pool.QuotaView) *Handler {
	t.Helper()
	p := pool.New("")
	p.AddFor("loomy", &auth.Auth{UID: "ly-1"}, nil)
	p.SetQuota("ly-1", q)
	return New(Config{Pool: p, DefaultProvider: "loomy"})
}

// 有分项 ⇒ JSON 里出现 free_quota，值是那个数。
func TestAccountViewsReportsFreeQuota(t *testing.T) {
	h := newFreeQuotaHandler(t, pool.QuotaView{
		Kind: pool.QuotaKindCredits, Remaining: 26434, HasData: true,
		Free: 1434, FreeKnown: true,
	})
	obj := freeQuotaJSON(t, h)

	raw, ok := obj["free_quota"]
	if !ok {
		t.Fatal("FreeKnown=true 时 JSON 里必须有 free_quota 字段")
	}
	if n, _ := raw.(float64); int64(n) != 1434 {
		t.Errorf("free_quota = %v，want 1434", raw)
	}
	// 「额度」列仍应是**总数**（两列不是重复）。
	if n, _ := obj["credits"].(float64); int64(n) != 26434 {
		t.Errorf("credits = %v，want 26434（总数 = 永久 + 每日）", obj["credits"])
	}
}

// ⚠ 反向：上游不区分分项 ⇒ JSON 里**不能有**这个键。
//
// 这是本文件最重要的一条。前端 `freeQuotaCellHTML` 的判据是
// "字段类型是不是 number" —— 所以字段一旦出现，界面就会把它当成**已知**。
// 若这里漏出一个 0，"该上游不区分免费额度"就变成了
// 「免费额度已用完」，用户据此以为账号在烧永久积分。
func TestAccountViewsOmitsFreeQuotaWhenUnknown(t *testing.T) {
	// workbuddy 形态：只有一个总额度，没有分项（Free/FreeKnown 留零值）。
	h := newFreeQuotaHandler(t, pool.QuotaView{
		Kind: pool.QuotaKindCredits, Remaining: 5000, HasData: true,
	})
	obj := freeQuotaJSON(t, h)

	if _, ok := obj["free_quota"]; ok {
		t.Errorf("该上游不区分分项 ⇒ JSON 里**不该有** free_quota（前端据此显示「—」），"+
			"实际下发了 %v —— 那会被读成「免费额度已用完」。\n完整账号对象: %v",
			obj["free_quota"], obj)
	}
	// 前置条件：总额度本身要在（不能因为新字段把老路径弄坏）。
	if n, _ := obj["credits"].(float64); int64(n) != 5000 {
		t.Errorf("credits = %v，want 5000 —— 新增一列不该影响原有列", obj["credits"])
	}
}

// 0 与"缺失"必须分得开 —— 这是本列最容易骗人的地方。
func TestAccountViewsFreeQuotaZeroIsKnownNotUnknown(t *testing.T) {
	h := newFreeQuotaHandler(t, pool.QuotaView{
		Kind: pool.QuotaKindCredits, Remaining: 19836, HasData: true,
		Free: 0, FreeKnown: true, // 明确读到"每日赠送确实用完了"
	})
	obj := freeQuotaJSON(t, h)

	raw, ok := obj["free_quota"]
	if !ok {
		t.Fatal("**明确**读到 0 时字段必须在（前端要显示 0，与「—」是两件事）")
	}
	if n, isNum := raw.(float64); !isNum || n != 0 {
		t.Errorf("free_quota = %v，want 数字 0", raw)
	}
}

// 逐字段搬迁不能漏：toPoolQuota 只搬 Free 不搬 FreeKnown 会退化成"已知的 0"。
//
// 这条直接钉住那个最危险的半吊子实现 —— 它会让**所有**上游的分项都变成 0。
func TestToPoolQuotaCopiesBothFreeFields(t *testing.T) {
	// 上游明确报"不区分"（FreeKnown=false, Free=0）。
	// 若实现是"无条件搬 Free"，得到的就是 {Free:0, FreeKnown:false} —— 那是对的；
	// 但若实现是"看到 Free 非零才搬"，就会漏掉"FreeKnown=true, Free=0"这一格。
	got := toPoolQuota(gatewayQuota(0, true))
	if !got.FreeKnown || got.Free != 0 {
		t.Errorf("明确读到 0 时必须搬成 {Free:0, FreeKnown:true}，实际 %+v —— "+
			"漏掉它会让界面把「确实是 0」显示成「—」（少报一次真实状态）", got)
	}
	// 上游不区分。
	got2 := toPoolQuota(gatewayQuota(0, false))
	if got2.FreeKnown {
		t.Errorf("上游不区分时不该搬成已知，实际 %+v", got2)
	}
	// 有值。
	got3 := toPoolQuota(gatewayQuota(1434, true))
	if !got3.FreeKnown || got3.Free != 1434 {
		t.Errorf("有值时应搬成 {Free:1434, FreeKnown:true}，实际 %+v", got3)
	}
}

// 前端读的是 `a.free_quota`（下划线），后端契约不能改名。
//
// 这条防的是"后端改成 freeQuota 而前端还读 free_quota"这类静默失配 ——
// 界面上表现为那一列永远 `—`，而没有任何东西报错。
func TestFreeQuotaJSONKeyIsStable(t *testing.T) {
	h := newFreeQuotaHandler(t, pool.QuotaView{
		Kind: pool.QuotaKindCredits, Remaining: 10, HasData: true,
		Free: 7, FreeKnown: true,
	})
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"free_quota"`) {
		t.Errorf(`响应的键必须是 "free_quota"（前端 freeQuotaCellHTML 读 a.free_quota），实际: %s`, body)
	}
}
