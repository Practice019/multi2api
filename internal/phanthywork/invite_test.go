// invite_test.go 邀请码面板的六条断言（＋一条能力位反例）。
//
// 取证与护栏写实现在 invite.go 文件头；这里只钉**行为**：
//
//	① 路由形状 / 能力位一致（未声明 CapInvite ⇒ 面板不出现）
//	② **一行一个账号**
//	③ 码解析自假上游（离线 httptest）
//	④ 同账号同日第二次打开 ⇒ **不再打 POST**（护栏②）
//	⑤ eligible_to_invite=false ⇒ **零 POST** + 可读原因（护栏④）
//	⑥ POST 失败 ⇒ 该行 error 可读、**其它账号不受影响**（护栏③的隔离）
package phanthywork

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// inviteFixture 造 n 个"已注册 + 已枚举"的账号 + 假上游。
func inviteFixture(t *testing.T, n int, tune func(*fakeUpstream)) (*Provider, []*Auth, *fakeUpstream) {
	t.Helper()
	inviteCodeSpacing = 0 // 用例不等 1 秒（生产值是 1s，见 invite.go）
	t.Cleanup(func() { inviteCodeSpacing = time.Second })
	srv, fake := startFakeUpstream(t)
	fake.eligibleToInvite = true
	if tune != nil {
		tune(fake)
	}
	p := newFakeProvider(t, srv)
	accts := make([]*Auth, 0, n)
	uids := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		a := newTestAuth(fmt.Sprintf("u-%d", i), "rt")
		a.Reg.RegisteredAtMS = nowMS() // 已注册：跳过写操作
		fake.trust(a)
		p.SetLocalCredential(a.UID(), a)
		accts = append(accts, a)
		uids = append(uids, a.UID())
	}
	p.SetUIDEnumerator(func() []string { return uids })
	return p, accts, fake
}

// inviteFetch 打一次面板端点，解出信封。
func inviteFetch(t *testing.T, p *Provider, query string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, invitePath+query, nil)
	rec := httptest.NewRecorder()
	p.handleInviteList(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("面板响应不是 JSON: %v；原文 %.300s", err, rec.Body.String())
	}
	return rec.Code, body
}

// inviteRows 取出 accounts 数组（并断言信封字段齐全 —— loomy 同形的判据）。
func inviteRows(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	for _, k := range []string{"accounts", "page", "pages", "total", "size", "updated_at", "summary"} {
		if _, ok := body[k]; !ok {
			t.Errorf("信封缺字段 %q（必须与 loomy 同形）: %#v", k, body)
		}
	}
	raw, _ := body["accounts"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		m, _ := e.(map[string]any)
		out = append(out, m)
	}
	return out
}

// ① 路由形状 / 能力位一致。
func TestInviteRouteShapeAndCapability(t *testing.T) {
	p, _, _ := inviteFixture(t, 1, nil)
	if !p.Caps().Has(gateway.CapInvite) {
		t.Fatal("声明了面板却没声明 CapInvite ⇒ 前端不会显示这个面板（入口由能力位驱动）")
	}
	var found *gateway.AdminRoute
	for i, r := range p.AdminRoutes() {
		if r.Path == invitePath {
			found = &p.AdminRoutes()[i]
		}
	}
	if found == nil {
		t.Fatalf("AdminRoutes 里没有 %s", invitePath)
	}
	if found.Method != http.MethodGet || found.Handler == nil {
		t.Errorf("路由形状不对: method=%q handler=%v", found.Method, found.Handler != nil)
	}
	if found.Capability != gateway.CapInvite {
		t.Errorf("路由 Capability = %v，want %v（前端按它决定是否显示入口）", found.Capability, gateway.CapInvite)
	}
	if found.Title != "邀请码" {
		t.Errorf("Title = %q（面板名必须叫「邀请码」—— 用户原话）", found.Title)
	}
}

// ① 的反例：**未声明** CapInvite ⇒ 按前端的 routesFor 规则，这个面板不会出现。
//
// ⚠ 规则照 internal/server/ui/app.js:1100-1105：`Capability == 0 || caps 里有它` 才返回。
func TestInvitePanelHiddenWhenCapabilityAbsent(t *testing.T) {
	p, _, _ := inviteFixture(t, 1, nil)
	route := p.inviteRoute()
	// 模拟"caps 里没有 invite"的前端：
	visible := route.Capability == 0 || (gateway.Capability(0) | gateway.CapChat).Has(route.Capability)
	if visible {
		t.Error("能力位没声明时该面板**不该**出现（它就是靠这条隐藏的）")
	}
	// 反过来：声明了就出现。
	visible = route.Capability == 0 || p.Caps().Has(route.Capability)
	if !visible {
		t.Error("声明了 CapInvite 面板却没出现")
	}
}

// ② 一行一个账号。
func TestInvitePanelOneRowPerAccount(t *testing.T) {
	p, accts, _ := inviteFixture(t, 3, nil)
	code, body := inviteFetch(t, p, "")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	rows := inviteRows(t, body)
	if len(rows) != len(accts) {
		t.Fatalf("行数 = %d，want %d（**一行一个账号**）", len(rows), len(accts))
	}
	seen := map[string]bool{}
	for i, row := range rows {
		uid, _ := row["uid"].(string)
		if uid != accts[i].UID() {
			t.Errorf("第 %d 行的 uid = %q，want %q（顺序应与账号顺序一致）", i, uid, accts[i].UID())
		}
		if seen[uid] {
			t.Errorf("uid %q 出现了两行", uid)
		}
		seen[uid] = true
	}
}

// ③ 码解析自假上游（离线）。
func TestInviteCodesParsedFromFakeUpstream(t *testing.T) {
	p, _, fake := inviteFixture(t, 1, func(f *fakeUpstream) { f.referralSlotCount = 5 })
	_, body := inviteFetch(t, p, "")
	rows := inviteRows(t, body)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d", len(rows))
	}
	row := rows[0]
	if ok, _ := row["ok"].(bool); !ok {
		t.Fatalf("这一行应当 ok=true，实际 %#v", row)
	}
	slots, _ := row["slots"].([]any)
	if len(slots) != 5 {
		t.Fatalf("槽位数 = %d，want 5", len(slots))
	}
	first, _ := slots[0].(map[string]any)
	if ic, _ := first["invite_code"].(string); !strings.HasPrefix(ic, "46KG-8S4M-2CE") {
		t.Errorf("invite_code 没解析对: %q", ic)
	}
	if c, _ := first["code"].(string); !strings.HasPrefix(c, "2_GeoAgB0BlMMz") {
		t.Errorf("code 没解析对: %q", c)
	}
	if u, _ := first["share_url"].(string); !strings.HasPrefix(u, "https://code.phanthy.com/r/") {
		t.Errorf("share_url 没解析对: %q", u)
	}
	// 幂等键形态：referral-code:<installationId>:<YYYY-MM-DD>（护栏②的键）。
	fake.mu.Lock()
	key, rawBody := fake.lastReferralKey, fake.lastReferralBody
	fake.mu.Unlock()
	if !strings.HasPrefix(key, "referral-code:") || strings.Count(key, ":") != 2 {
		t.Errorf("幂等键 = %q，want referral-code:<installationId>:<YYYY-MM-DD>", key)
	}
	if rawBody != "{}" {
		t.Errorf("POST body = %q，want 字面量空对象 {}", rawBody)
	}
	if got := atomicLoad(&fake.referralCodeCalls); got != 1 {
		t.Errorf("取码端点打了 %d 次，want 1", got)
	}
}

// ④ 同账号同日第二次打开 ⇒ 不再打 POST（护栏②）。
func TestInviteSecondOpenSameDayDoesNotPostAgain(t *testing.T) {
	p, _, fake := inviteFixture(t, 1, nil)
	_, first := inviteFetch(t, p, "")
	if got := atomicLoad(&fake.referralCodeCalls); got != 1 {
		t.Fatalf("第一次打开应当打 1 次取码，实际 %d", got)
	}
	_, second := inviteFetch(t, p, "")
	if got := atomicLoad(&fake.referralCodeCalls); got != 1 {
		t.Errorf("同日第二次打开又打了取码（累计 %d 次）—— 护栏②要求每账号每自然日最多 1 次", got)
	}
	rows := inviteRows(t, second)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d", len(rows))
	}
	if fc, _ := rows[0]["from_cache"].(bool); !fc {
		t.Errorf("第二次应当标 from_cache=true（它是缓存命中的证据），实际 %#v", rows[0]["from_cache"])
	}
	firstRows := inviteRows(t, first)
	if len(firstRows) == 1 && len(firstRows[0]["slots"].([]any)) == 0 {
		t.Error("第一次就该拿到码")
	}
}

// ⑤ eligible_to_invite=false ⇒ 零 POST + 可读原因（护栏④）。
func TestInviteIneligibleSkipsCodePost(t *testing.T) {
	p, _, fake := inviteFixture(t, 1, func(f *fakeUpstream) {
		f.eligibleToInvite = false
		f.referralProgramStatus = "closed"
	})
	_, body := inviteFetch(t, p, "")
	if got := atomicLoad(&fake.referralCodeCalls); got != 0 {
		t.Errorf("不可邀请的账号**一次都不该调**取码（它是写接口），实际 %d 次", got)
	}
	rows := inviteRows(t, body)
	if len(rows) != 1 {
		t.Fatalf("行数 = %d", len(rows))
	}
	e, _ := rows[0]["error"].(string)
	if e == "" {
		t.Errorf("这一行必须给**可读原因**（不是空、也不是空数组），实际 %#v", rows[0])
	}
	if !strings.Contains(e, "不可邀请") {
		t.Errorf("原因文案应当说清「不可邀请」，实际 %q", e)
	}
	if _, has := rows[0]["slots"]; has {
		t.Error("没取到码时**不该**出现 slots 字段（空数组会被界面读成「这人没有码」）")
	}
}

// ⑥ 一个账号取码失败 ⇒ 只有那一行报错，其它账号照常拿到码（扇出隔离）。
func TestInviteCodeFailureIsolatedToThatRow(t *testing.T) {
	p, accts, fake := inviteFixture(t, 2, nil)
	badInst, err := accts[1].installationID()
	if err != nil {
		t.Fatalf("取 installationID: %v", err)
	}
	fake.failReferralCodeInstID = badInst
	_, body := inviteFetch(t, p, "")
	rows := inviteRows(t, body)
	if len(rows) != 2 {
		t.Fatalf("行数 = %d，want 2", len(rows))
	}
	if ok, _ := rows[0]["ok"].(bool); !ok {
		t.Errorf("第 1 个账号应当不受影响（拿到码），实际 %#v", rows[0])
	}
	if ok, _ := rows[1]["ok"].(bool); ok {
		t.Errorf("第 2 个账号取码失败了，不该 ok=true: %#v", rows[1])
	}
	e, _ := rows[1]["error"].(string)
	if e == "" {
		t.Errorf("失败的那一行必须有可读 error，实际 %#v", rows[1])
	}
	// 两个账号都试过（护栏③：失败不回滚、不重试 ⇒ 各打一次）。
	if got := atomicLoad(&fake.referralCodeCalls); got != 2 {
		t.Errorf("取码端点打了 %d 次，want 2（失败不回滚、不重试）", got)
	}
}
