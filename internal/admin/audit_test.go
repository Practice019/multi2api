// audit_test.go 审计挂钩的判据（Wave1-T07）。
//
// 五条要钉住的：
//
//	① 一次**写操作**（改渠道 base_url）恰好 +1 行 audit_log —— 二元行为判据
//	② 落下来的字段是**对的**（actor / action / route / status / success）；
//	   行数判据对「字段错位」完全没有判别力（行数对了、内容全错，照样绿）
//	③ 失败的写操作也留痕且 success=false —— 只记成功的审计是最坏的一种：
//	   它让人以为失败的操作没发生过
//	④ 读操作不留痕；审计表里**绝不出现请求体**（含渠道 key 那种明文秘密）
//	⑤ 审计写失败不许影响请求结果（store 未打开时写端点仍要正常返回）
package admin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/store"
)

// auditHandler 把 Handler 接到一个**真库**上（临时文件），审计走 store.AppendAudit。
//
// # 为什么用真库而不是一个假回调
//
// 本任务的验收判据是「audit_log 行数 +1」—— 计数只能来自真表。用内存计数器
// 代替它，会把「字段到底写进去了没有」这条一起绕过（那正是②要钉的东西）。
func auditHandler(t *testing.T, s *channelstore.Store) *Handler {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	cfg := Config{AdminToken: testAdminToken, ListensLoopback: true, Audit: store.AppendAudit}
	cfg.Channels = s
	return New(cfg)
}

func auditRows(t *testing.T) int64 {
	t.Helper()
	n, err := store.CountAudit()
	if err != nil {
		t.Fatalf("store.CountAudit: %v", err)
	}
	return n
}

func lastAuditRow(t *testing.T) store.AuditLog {
	t.Helper()
	var row store.AuditLog
	if err := store.DB.Order("id desc").First(&row).Error; err != nil {
		t.Fatalf("读回最后一条 audit_log 失败: %v", err)
	}
	return row
}

// auditRowText 把一行里所有**文本**字段拼起来，用于「整行有没有泄漏秘密」的扫描。
//
// 逐字段断言查的是「该有的有没有」，这条查的是「不该有的有没有」—— 两者不是
// 一回事：秘密泄漏通常发生在一个**没人想到要断言**的字段上。
func auditRowText(row store.AuditLog) string {
	return strings.Join([]string{
		row.EventID, row.Actor, row.AuthMethod, row.Category, row.Action,
		row.Params, row.IP, row.UserAgent, row.Method, row.Route, row.Reason,
	}, " ")
}

const auditChanSecret = "sk-audit-must-not-be-stored"

func createAuditChannel(t *testing.T, h *Handler) string {
	t.Helper()
	body := `{"name":"审计渠道","type":0,"base_url":"https://api.example.com/v1",` +
		`"keys":["` + auditChanSecret + `"],"models":["m1"]}`
	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", body)
	if code != http.StatusOK {
		t.Fatalf("建渠道应 200，实际 %d: %s", code, resp)
	}
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil || out.Channel.ID == "" {
		t.Fatalf("建渠道响应应带 id，实际 %s（err=%v）", resp, err)
	}
	return out.Channel.ID
}

// TestAuditChannelBaseURLChangeAddsOneRow T07 的**二元行为判据**。
//
// 打印前后行数（t.Logf）而不是只断言差值：验收要求「贴前后行数」，
// 而只断言差值时，输出里看不到到底从几变到几。
func TestAuditChannelBaseURLChangeAddsOneRow(t *testing.T) {
	h := auditHandler(t, chanStore(t))
	id := createAuditChannel(t, h)

	before := auditRows(t)
	t.Logf("改 base_url 前 audit_log 行数 = %d", before)

	update := `{"id":"` + id + `","name":"审计渠道","type":0,` +
		`"base_url":"https://api2.example.com/v1","models":["m1"]}`
	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", update)
	if code != http.StatusOK {
		t.Fatalf("改 base_url 应 200，实际 %d: %s", code, resp)
	}

	after := auditRows(t)
	t.Logf("改 base_url 后 audit_log 行数 = %d（差 %d）", after, after-before)
	if after-before != 1 {
		t.Fatalf("改一次渠道 base_url 应恰好 +1 行，实际 before=%d after=%d", before, after)
	}

	// ② 字段逐个核对（行数判据看不见这一类错误）
	row := lastAuditRow(t)
	if row.Method != "POST" || row.Route != "/admin/channels" {
		t.Errorf("method/route 应为 POST /admin/channels，得到 %q %q", row.Method, row.Route)
	}
	if row.Action != "channel.save" {
		t.Errorf("action 应为 channel.save，得到 %q", row.Action)
	}
	if row.Actor != "admin_token" || row.AuthMethod != "token" {
		t.Errorf("actor/auth_method 应为 admin_token/token（脚本走 Bearer 令牌），得到 %q/%q", row.Actor, row.AuthMethod)
	}
	if row.Category != store.AuditCategoryOperation {
		t.Errorf("category 应为 %q，得到 %q", store.AuditCategoryOperation, row.Category)
	}
	if row.Status != http.StatusOK || !row.Success {
		t.Errorf("status/success 应为 200/true，得到 %d/%v", row.Status, row.Success)
	}
	if row.At <= 0 {
		t.Errorf("at 应被填上时间戳，得到 %d", row.At)
	}
	if row.IP == "" {
		t.Errorf("ip 不该为空（httptest 的 RemoteAddr 是 192.0.2.1:1234）")
	}
	// ④ 审计不存请求体：params 必须留空，且整行不得出现明文 key。
	if row.Params != "" {
		t.Errorf("params 必须留空（审计不存请求体 / 参数），得到 %q", row.Params)
	}
	if leaked := auditRowText(row); strings.Contains(leaked, auditChanSecret) {
		t.Errorf("★ 审计行里出现了渠道明文 key（审计表要长期保留，绝不该有它）: %s", leaked)
	}
}

// TestAuditRecordsFailedWrite 失败的写操作也必须留痕，且 success=false。
func TestAuditRecordsFailedWrite(t *testing.T) {
	h := auditHandler(t, chanStore(t))
	before := auditRows(t)

	// 体是合法 JSON，但 name 为空 ⇒ 拦在业务校验上（400）
	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels", `{"name":"","base_url":"https://a.example.com/v1"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("缺 name 应 400，实际 %d: %s", code, resp)
	}
	after := auditRows(t)
	t.Logf("失败的写操作：before=%d after=%d", before, after)
	if after-before != 1 {
		t.Fatalf("失败的写操作也应恰好 +1 行，实际 before=%d after=%d", before, after)
	}
	row := lastAuditRow(t)
	if row.Success || row.Status != http.StatusBadRequest {
		t.Errorf("失败请求应记 success=false/status=400，得到 success=%v status=%d", row.Success, row.Status)
	}
}

// TestAuditSkipsReadRequests 读操作不留痕。
//
// 若读也记，审计表会长成一张访问日志，「谁改了什么」就再也筛不出来了 ——
// 而 hyxapi 的判据同样只对 POST/PUT/PATCH/DELETE 生效。
func TestAuditSkipsReadRequests(t *testing.T) {
	h := auditHandler(t, chanStore(t))
	createAuditChannel(t, h)
	before := auditRows(t)
	if code, resp := doJSON(t, h, http.MethodGet, "/admin/channels", ""); code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d: %s", code, resp)
	}
	if after := auditRows(t); after != before {
		t.Errorf("GET 不该产生审计行，实际 before=%d after=%d", before, after)
	}
}

// TestAuditDisabledWithoutCallback cfg.Audit 为 nil ⇒ 行为与改造前完全一致。
//
// 这条守的是「注入是**可选**的」：未接线的部署（以及全部既有测试）不该因为
// 这次改造多出一个必填依赖。
func TestAuditDisabledWithoutCallback(t *testing.T) {
	h := chanHandler(t, chanStore(t)) // 仓储既有助手：不带 Audit
	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels",
		`{"name":"无审计","base_url":"https://a.example.com/v1"}`)
	if code != http.StatusOK {
		t.Fatalf("未接审计时写操作仍应 200，实际 %d: %s", code, resp)
	}
}

// TestAuditWriteFailureDoesNotBreakRequest 审计写失败不许影响请求结果。
//
// 这里故意不 Open（AppendAudit 会回 ErrNotOpen）：写端点必须照样 200。
// 审计是**附加**的事实，不是业务的前置条件 —— 反过来会让「库没起来」
// 变成「改不了配置」，那比没有审计更糟。
func TestAuditWriteFailureDoesNotBreakRequest(t *testing.T) {
	_ = store.Close() // 制造 ErrNotOpen
	cfg := Config{AdminToken: testAdminToken, ListensLoopback: true, Audit: store.AppendAudit}
	cfg.Channels = chanStore(t)
	h := New(cfg)
	code, resp := doJSON(t, h, http.MethodPost, "/admin/channels",
		`{"name":"无库","base_url":"https://a.example.com/v1"}`)
	if code != http.StatusOK {
		t.Fatalf("审计写失败不该影响请求结果（应 200），实际 %d: %s", code, resp)
	}
}

// TestAuditSucceededDecision 「成功」的判据本身（照 hyxapi 的行为，字段名换成本仓的 ok）。
func TestAuditSucceededDecision(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"200 且显式 ok:true", 200, `{"ok":true,"channel":{}}`, true},
		{"200 但显式 ok:false", 200, `{"ok":false,"error":"业务拒绝"}`, false},
		{"400 即使体里 ok:true 也判失败", 400, `{"ok":true}`, false},
		{"非 JSON 体按状态码", 200, "plain text", true},
		{"空体按状态码（204）", 204, "", true},
		{"截断的 JSON 回落到状态码", 200, `{"ok":tru`, true},
	}
	for _, c := range cases {
		if got := auditSucceeded(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: auditSucceeded(%d, %q) = %v，期望 %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

// TestAuditRouteForNormalizesParams 路径归一：带参数的写路由落**模板**，
// 未登记的路由回落 generic（**留痕**，而不是漏记）。
func TestAuditRouteForNormalizesParams(t *testing.T) {
	cases := []struct {
		method, path, wantRoute, wantAction string
	}{
		{"POST", "/admin/channels", "/admin/channels", "channel.save"},
		{"POST", "/admin/accounts/acc-9f3/enable", "/admin/accounts/{uid}/enable", "account.enable"},
		{"DELETE", "/admin/accounts/acc-9f3", "/admin/accounts/{uid}", "account.delete"},
		{"POST", "/admin/upstream-specific/thing", "/admin/upstream-specific/thing", "generic"},
		{"GET", "/admin/channels", "/admin/channels", "generic"}, // 读方法不登记
	}
	for _, c := range cases {
		route, action := auditRouteFor(c.method, c.path)
		if route != c.wantRoute || action != c.wantAction {
			t.Errorf("auditRouteFor(%q, %q) = %q/%q，期望 %q/%q", c.method, c.path, route, action, c.wantRoute, c.wantAction)
		}
	}
}
