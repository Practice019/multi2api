package cline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// balanceFake 一个假的余额上游。
type balanceFake struct {
	status int
	body   string
	// gotPath / gotAuth 记录收到的请求（用于断言 account_id 与前缀）
	gotPath string
	gotAuth string
}

func newBalanceServer(t *testing.T, f *balanceFake) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/", func(w http.ResponseWriter, r *http.Request) {
		f.gotPath = r.URL.Path
		f.gotAuth = r.Header.Get("Authorization")
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewWithBase(srv.URL)
}

// TestBalanceUsesAccountIDNotSub 余额必须用 account_id（usr-…），不是 JWT 的 sub。
//
// # 这条钉住的是一个极易混用的差别
//
// 实测：传 sub（`user_01M3BCQ86DV4S9KKBT85X4GKTV`）返回
// `400 {"error":"Invalid request format"}`。两者形态完全不同
//（`usr-…` vs `user_…`），但都是"用户标识"，混用时不会编译失败、
// 只会得到一个与真实原因毫不相干的 400。
func TestBalanceUsesAccountIDNotSub(t *testing.T) {
	f := &balanceFake{body: `{"success":true,"data":{"userId":"usr-1","balance":500000}}`}
	c := newBalanceServer(t, f)

	res := c.FetchBalance(context.Background(), &Auth{
		AccessToken: "jwt-token",
		// 刻意给一个与 account_id 不同的 sub 形态值
		AccountID: "usr-01M3BCV4FYCGJKAWD3MJG3DBQM",
	})
	if res.Error != "" {
		t.Fatalf("查询失败: %s", res.Error)
	}
	if !strings.Contains(f.gotPath, "usr-01M3BCV4FYCGJKAWD3MJG3DBQM") {
		t.Errorf("请求路径 = %q，应包含 account_id（usr-…）—— "+
			"用 JWT 的 sub（user_…）会得到 400 Invalid request format", f.gotPath)
	}
}

// TestBalanceMissingAccountIDIsExplicit 缺 account_id 时明确报错（不是静默用 sub）。
func TestBalanceMissingAccountIDIsExplicit(t *testing.T) {
	f := &balanceFake{body: `{}`}
	c := newBalanceServer(t, f)

	res := c.FetchBalance(context.Background(), &Auth{AccessToken: "jwt"})
	if res.Error == "" {
		t.Fatal("缺 account_id 时应明确报错")
	}
	if !strings.Contains(res.Error, "account_id") {
		t.Errorf("报错文案应点名 account_id（便于定位），得到 %q", res.Error)
	}
	if f.gotPath != "" {
		t.Error("缺 account_id 时不该发出请求（必然 400）")
	}
}

// TestBalanceAuthKeepsWorkOSPrefix Authorization 必须原样保留 `workos:` 前缀。
//
// 剥掉即 401，且报错文案是 "make sure you're using the latest version of Cline"
// —— 与真实原因毫不相干，会让人误判成「版本过旧」。
func TestBalanceAuthKeepsWorkOSPrefix(t *testing.T) {
	f := &balanceFake{body: `{"success":true,"data":{"balance":1}}`}
	c := newBalanceServer(t, f)

	_ = c.FetchBalance(context.Background(), &Auth{
		AccessToken: clineBearerValue("jwt-abc"),
		AccountID:   "usr-1",
	})
	if !strings.HasPrefix(f.gotAuth, "Bearer workos:") {
		t.Errorf("Authorization = %q，必须保留 `workos:` 前缀 —— "+
			"剥掉即 401，且报错文案会误导成「版本过旧」", f.gotAuth)
	}
}

// TestBalanceParsesSuccess 正常响应解析出原始值。
func TestBalanceParsesSuccess(t *testing.T) {
	f := &balanceFake{body: `{"success":true,"data":{"userId":"usr-1","balance":500000}}`}
	c := newBalanceServer(t, f)

	res := c.FetchBalance(context.Background(), &Auth{AccessToken: "t", AccountID: "usr-1"})
	if res.Error != "" {
		t.Fatalf("查询失败: %s", res.Error)
	}
	if res.Raw != 500000 {
		t.Errorf("raw = %v，want 500000（原始值必须透出，单位换算是唯一不确定点）", res.Raw)
	}
}

// TestBalanceRecognizesBothFailureShapes 两种失败形态**都要认**（真实缺陷）。
//
//	{success:false, error:"…"}   业务层失败（HTTP 200）
//	{error:"Unauthorized: …"}    网关层失败（HTTP 401 —— **没有 success 字段**）
//
// 早期只判第一种，于是 401 会落到「响应缺少 data 字段」这个**误导性**文案，
// 而服务端真正给的原因被丢掉 —— 那正是排查鉴权问题唯一有用的线索。
//
// # ⚠ 这条测试的判据与"实现走哪一支"的关系（我在这里差点写错）
//
// 我先按"两条分支都必须存在"写了一个变异（把 `serverErr != ""` 那半段
// 从 success 判断里删掉），结果**测试依然全绿**。追下去才看清：
//
//	401 在实现里走的是**状态码分支**（`status < 200 || status >= 300`），
//	那里也会把 serverErr 拼进文案 —— 所以删掉 success 判断里的那半段
//	对 401 **没有可观测影响**。
//
// 也就是说：本包的正确性**不依赖**"success 判断要认无 success 的形态"，
// 而依赖"状态码分支要保留服务端文案"。两者是不同的机制，
// 而我把判据写成了前者。
//
// 所以这里如实按**行为**断言（401 的文案里必须有服务端原文、
// 且不能是"缺少 data 字段"），不假装它在测某个具体分支 ——
// 这样的判据在两种实现下都成立，也正是用户真正关心的那件事。
func TestBalanceRecognizesBothFailureShapes(t *testing.T) {
	t.Run("业务层失败（200 + success:false）", func(t *testing.T) {
		f := &balanceFake{body: `{"success":false,"error":"insufficient scope"}`}
		c := newBalanceServer(t, f)
		res := c.FetchBalance(context.Background(), &Auth{AccessToken: "t", AccountID: "usr-1"})
		if !strings.Contains(res.Error, "insufficient scope") {
			t.Errorf("应保留服务端文案，得到 %q", res.Error)
		}
	})

	t.Run("网关层失败（401，无 success 字段）", func(t *testing.T) {
		// ⚠ 关键：**没有** success 字段
		f := &balanceFake{status: 401, body: `{"error":"Unauthorized: Please make sure you're using the latest version of Cline and re-authenticate your Cline account."}`}
		c := newBalanceServer(t, f)
		res := c.FetchBalance(context.Background(), &Auth{AccessToken: "t", AccountID: "usr-1"})
		if res.Error == "" {
			t.Fatal("401 必须报错")
		}
		if !strings.Contains(res.Error, "re-authenticate") {
			t.Errorf("401 的报错必须保留服务端原文（那是排查鉴权唯一有用的线索），"+
				"而不是「响应缺少 data 字段」这类误导性文案。得到 %q", res.Error)
		}
		if strings.Contains(res.Error, "缺少 data") {
			t.Error("落到了「响应缺少 data 字段」—— 说明没有识别「无 success 字段」这一形态")
		}
	})
}

// TestBalanceNonJSONIsReported 非 JSON 响应要如实报（含 HTTP 码）。
func TestBalanceNonJSONIsReported(t *testing.T) {
	f := &balanceFake{status: 502, body: `<html>bad gateway</html>`}
	c := newBalanceServer(t, f)
	res := c.FetchBalance(context.Background(), &Auth{AccessToken: "t", AccountID: "usr-1"})
	if !strings.Contains(res.Error, "502") {
		t.Errorf("非 JSON 响应应带上 HTTP 码，得到 %q", res.Error)
	}
}

// TestNormalizedBalanceScale 换算系数按 balanceScale 生效。
//
// ⚠ 这个系数是**推断**而非实测（见 balanceScale 的注释），
// 所以本用例只钉"换算确实发生了、且用的是那个常数"，
// 不假装它一定对 —— 真实核对靠 /admin/cline/balance 透出的 raw 值。
func TestNormalizedBalanceScale(t *testing.T) {
	if got := normalizedBalance(500000); got != 5 {
		t.Errorf("normalizedBalance(500000) = %v，want 5（500000/%d）", got, balanceScale)
	}
	if got := normalizedBalance(0); got != 0 {
		t.Errorf("normalizedBalance(0) = %v，want 0", got)
	}
}

// ── 管理端点 ──

// TestAdminRoutesExposeBalance 余额必须有管理端点可达。
//
// # 这条修的是一句**不成立的注释**
//
// provider.go 的 Caps 注释此前写着「余额…由 AdminExt 暴露」，
// 而本包当时**没有** AdminRoutes —— 那个余额查询实现了却没有任何入口
// 能调到它（只有测试能碰）。代码里的承诺与代码实际做的事不符，
// 比没有承诺更糟：后来者读到会以为"已经接好了"，于是不去查。
func TestAdminRoutesExposeBalance(t *testing.T) {
	p := NewWithConfig(Config{})
	routes := p.AdminRoutes()
	if len(routes) == 0 {
		t.Fatal("没有管理端点 —— 余额查询无入口可达（注释承诺的 AdminExt 不存在）")
	}
	var found bool
	for _, r := range routes {
		if r.Path == balancePath {
			found = true
			if r.Method != http.MethodGet {
				t.Errorf("余额端点方法 = %s，want GET", r.Method)
			}
		}
	}
	if !found {
		t.Errorf("缺少 %s（实际 %v）", balancePath, routes)
	}
}

// TestCapsDoesNotDeclareCheckin Cline 没有签到，**不得**声明 CapCheckin。
//
// 对整个 sidecar 做字符串扫描，checkin / check-in / daily / campaign
// 均无 Cline 业务端点命中（campaign 的命中是 PostHog 的 UTM 参数与
// feature-flag 事件属性；daily 是 YAML cron 别名与 Blob 导出频率枚举）。
//
// 声明了等于给前端画一个点了必然失败的面板。
func TestCapsDoesNotDeclareCheckin(t *testing.T) {
	p := NewWithConfig(Config{})
	if p.Caps().Has(gateway.CapCheckin) {
		t.Error("Cline 没有签到接口，不得声明 CapCheckin")
	}
}

// TestBalanceHandlerReportsFailureNotZero 端点查不到时**如实报原因**，不显示 0。
//
// 0 是"已用光"的语义；显示 0 会让用户以为余额被清空了。
func TestBalanceHandlerReportsFailureNotZero(t *testing.T) {
	p := NewWithConfig(Config{})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{
			Provider: ProviderID,
			UID:      uid,
			Secret:   &Auth{AccessToken: "t"}, // 缺 AccountID → 必然失败
		}, true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, balancePath+"?uid=u1", nil)
	p.handleBalance(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["ok"] != false {
		t.Errorf("查不到时 ok 应为 false，得到 %v", out["ok"])
	}
	if _, hasTotal := out["total"]; hasTotal {
		t.Error("查不到时**不得**下发 total（0 是「已用光」的语义，会让用户以为余额被清空）")
	}
	if out["error"] == nil || out["error"] == "" {
		t.Error("必须给出失败原因")
	}
}

// TestBalanceHandlerUnknownUIDIs404 账号不在池里 → 404。
func TestBalanceHandlerUnknownUIDIs404(t *testing.T) {
	p := NewWithConfig(Config{})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{}, false
	})
	rec := httptest.NewRecorder()
	p.handleBalance(rec, httptest.NewRequest(http.MethodGet, balancePath+"?uid=nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d，want 404（账号不在池里）", rec.Code)
	}
}
