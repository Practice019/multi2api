package raccoon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// creditsFake 一个假的积分上游（按路径分发）。
type creditsFake struct {
	// balanceBody /bills /grant 的响应（空 = 走 status）
	balanceBody string
	billsBody   string
	grantBody   string
	// 记录收到的请求
	gotPaths []string
}

func newCreditsServer(t *testing.T, f *creditsFake) *Client {
	t.Helper()
	mux := http.NewServeMux()
	reply := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.gotPaths = append(f.gotPaths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
	}
	mux.HandleFunc(pointsPrefix+"/balance", reply(f.balanceBody))
	mux.HandleFunc(pointsPrefix+"/bills", reply(f.billsBody))
	mux.HandleFunc(desktopPrefix+"/login/points/grant", reply(f.grantBody))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewWithBase(srv.URL)
}

func testAuth() *Auth { return &Auth{AccessToken: "at-test"} }

// ── 余额 ────────────────────────────────────────────────────────────────

// TestBalanceReadsAllPools 各积分池都要读到。
//
// ⚠ 各池**分开作 package**，让用户看出「奖励 / 每日 / 会员 / 充值」
// 是独立来源 —— 它们的有效期与回补规则都不同
//（每日积分每日刷新、充值积分长期有效）。
//
// 变异可检：把任何一个池的读取删掉 → 本用例红。
func TestBalanceReadsAllPools(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{balanceBody: `{
		"code":0,"message":"ok","data":{
			"available_points":3400,
			"reward_points":3000,
			"daily_points":300,
			"monthly_points":100,
			"topup_points":0
		}}`})

	b, err := c.FetchBalance(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if b.Total != 3400 {
		t.Errorf("total = %v，want 3400", b.Total)
	}
	if b.Reward != 3000 || b.Daily != 300 || b.Monthly != 100 {
		t.Errorf("各池未全部读到：reward=%v daily=%v monthly=%v",
			b.Reward, b.Daily, b.Monthly)
	}
}

// TestBalanceMissingAvailablePointsIsError 缺 available_points 判失败，**不编造 0**。
//
// 0 是"已用光"的语义；把它当成余额会让用户以为积分被清空了。
func TestBalanceMissingAvailablePointsIsError(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{balanceBody: `{"code":0,"message":"ok","data":{"reward_points":100}}`})
	if _, err := c.FetchBalance(context.Background(), testAuth()); err == nil {
		t.Error("缺 available_points 时应报错，而不是编造一个 0")
	}
}

// TestBalanceNonZeroCodeIsError 业务信封 code != 0 判失败（HTTP 可能仍是 200）。
func TestBalanceNonZeroCodeIsError(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{balanceBody: `{"code":403,"message":"forbidden"}`})
	if _, err := c.FetchBalance(context.Background(), testAuth()); err == nil {
		t.Error("code != 0 应报错")
	}
}

// ── 登录奖励（幂等一次性，**不是**每日签到） ──

// TestLoginRewardGrantedTrueIsClaimed granted:true ⇒ 领取成功。
func TestLoginRewardGrantedTrueIsClaimed(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{grantBody: `{
		"code":0,"message":"ok","data":{"granted":true,"popup":{"source":"desktop","points":3000}}}`})

	res, err := c.ClaimLoginGrant(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if !res.Claimed {
		t.Error("granted:true 应判 claimed")
	}
	if res.Points != 3000 {
		t.Errorf("points = %v，want 3000", res.Points)
	}
}

// TestLoginRewardGrantedFalseIsNotClaimed 幂等判据是 `granted`，不是 HTTP 状态码。
//
// ⚠ 重复领取同样返回 **HTTP 200** + `granted:false`。判成 claimed 会让用户
// 以为每次都真的加了额度。
//
// 变异可检：把 `!data.Granted` 分支删掉（直接当成功）→ 本用例红。
func TestLoginRewardGrantedFalseIsNotClaimed(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{grantBody: `{"code":0,"message":"ok","data":{"granted":false}}`})

	res, err := c.ClaimLoginGrant(context.Background(), testAuth())
	if err != nil {
		t.Fatalf("已领过不该是错误: %v", err)
	}
	if res.Claimed {
		t.Error("granted:false 应判**未领到**（已领过）—— " +
			"判成 claimed 会让用户以为每次都真的加了额度")
	}
	if res.Points != 0 {
		t.Errorf("已领过时 points = %v，want 0", res.Points)
	}
}

// TestLoginRewardPopupPointsFallbackTo3000 popup 缺 points 时用 3000 兜底。
func TestLoginRewardPopupPointsFallbackTo3000(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{grantBody: `{"code":0,"message":"ok","data":{"granted":true}}`})

	res, err := c.ClaimLoginGrant(context.Background(), testAuth())
	if err != nil {
		t.Fatal(err)
	}
	if res.Points != 3000 {
		t.Errorf("points = %v，want 3000（popup 缺失时的兜底）", res.Points)
	}
}

// ── onboarding 状态（账单明细判据） ──

// TestOnboardingRequiresBothBizTypeAndEventName **两个条件都要匹配**。
//
// ⚠ 只按 `biz_type === 'reward_grant'` 判定会让**新用户一开始就显示「已领取」**
// —— 因为「新人注册礼包」也是 reward_grant。
//
// 变异可检：把 event_name 那个条件删掉 → 第一条子用例红。
func TestOnboardingRequiresBothBizTypeAndEventName(t *testing.T) {
	t.Run("只有注册礼包（reward_grant 但 event_name 不同）→ 未领", func(t *testing.T) {
		c := newCreditsServer(t, &creditsFake{billsBody: `{
			"code":0,"message":"ok","data":{"items":[
				{"biz_type":"reward_grant","event_name":"新人注册礼包","points":3000}
			]}}`})
		st := c.FetchOnboardingStatus(context.Background(), testAuth())
		if st.Claimed {
			t.Error("「新人注册礼包」不是登录奖励 —— " +
				"只按 biz_type 判定会让新用户一开始就显示「已领取」")
		}
	})

	t.Run("有登录奖励记录 → 已领", func(t *testing.T) {
		c := newCreditsServer(t, &creditsFake{billsBody: `{
			"code":0,"message":"ok","data":{"items":[
				{"biz_type":"reward_grant","event_name":"新人注册礼包","points":3000},
				{"biz_type":"reward_grant","event_name":"桌面端登录奖励","points":3000}
			]}}`})
		st := c.FetchOnboardingStatus(context.Background(), testAuth())
		if !st.Claimed {
			t.Error("有「桌面端登录奖励」记录应判已领")
		}
		if st.Points != 3000 {
			t.Errorf("points = %v，want 3000（取账单里的实际值）", st.Points)
		}
	})
}

// TestOnboardingQueryFailureIsConservative 查询失败时保守返回未领。
//
// 宁可让用户多点一次（服务端幂等，无害），也不要误报「已领」
// 而让他真的错过。
func TestOnboardingQueryFailureIsConservative(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{billsBody: `{"code":500,"message":"boom"}`})
	st := c.FetchOnboardingStatus(context.Background(), testAuth())
	if st.Claimed {
		t.Error("查询失败时必须保守返回「未领」—— "+
			"误报「已领」会让用户真的错过奖励")
	}
	if st.Points != 3000 {
		t.Errorf("失败时 points 应为默认额度 3000，得到 %v", st.Points)
	}
}

// TestOnboardingDoesNotUseBalance 判据必须来自账单，**不是**余额。
//
// 余额是多个来源的合计，无法区分某一项是否已领。
// 本用例给一个"余额很大但账单里没有登录奖励"的账号 ——
// 若实现靠余额推断，会误判成已领。
func TestOnboardingDoesNotUseBalance(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		// 余额很大（暗示"可能领过"），但账单里没有登录奖励记录
		balanceBody: `{"code":0,"message":"ok","data":{"available_points":99999,"reward_points":99999}}`,
		billsBody:   `{"code":0,"message":"ok","data":{"items":[]}}`,
	})
	st := c.FetchOnboardingStatus(context.Background(), testAuth())
	if st.Claimed {
		t.Error("判据必须来自账单明细，不能靠余额推断 —— " +
			"余额是多个来源的合计，无法区分某一项是否已领")
	}
}

// ── 管理端点 ──

// TestAdminRoutesExposeBalanceAndReward 余额与登录奖励都必须有端点可达。
//
// # 为什么这条重要
//
// `FetchBalance` 与 `ClaimLoginGrant` 此前都已实现、也有测试，
// 但本包**没有** AdminRoutes —— 那两件事没有任何入口能调到
//（只有测试能碰）。cline 的余额端点踩过同一个坑。
func TestAdminRoutesExposeBalanceAndReward(t *testing.T) {
	p := NewWithConfig(Config{})
	routes := p.AdminRoutes()
	byPath := map[string]gateway.AdminRoute{}
	for _, r := range routes {
		byPath[r.Path] = r
	}
	for _, want := range []string{adminBalancePath, adminLoginRewardPath, adminOnboardingPath} {
		if _, ok := byPath[want]; !ok {
			t.Errorf("缺少端点 %s（实际 %v）", want, routes)
		}
	}
	// 领取是**写**操作，必须是 POST —— 只读优先原则：
	// 打开面板这类高频路径绝不碰写端点。
	if r, ok := byPath[adminLoginRewardPath]; ok && r.Method != http.MethodPost {
		t.Errorf("领取端点方法 = %s，want POST（写操作，不能被只读路径误触发）", r.Method)
	}
	if r, ok := byPath[adminBalancePath]; ok && r.Method != http.MethodGet {
		t.Errorf("余额端点方法 = %s，want GET", r.Method)
	}
}

// TestCapsDoesNotDeclareCheckin 登录奖励**不是**每日签到，不得声明 CapCheckin。
//
// ⚠ 每日 300 是**服务端按日自动发放，无端点** —— 实测该账号 13:30 注册、
// 13:31 就收到 daily_grant 账单。把它实现成签到按钮必然失败。
//
// 而登录奖励是**幂等一次性**的（每号一次），语义与「新手任务」同构。
// 声明 CapCheckin 会让前端画一个"每天可领"的按钮，用户点第二次就失望。
func TestCapsDoesNotDeclareCheckin(t *testing.T) {
	p := NewWithConfig(Config{})
	if p.Caps().Has(gateway.CapCheckin) {
		t.Error("Raccoon 的登录奖励是一次性的（每号一次），不是每日签到 —— " +
			"声明 CapCheckin 会让前端画一个「每天可领」的按钮")
	}
}

// TestBalanceHandlerReportsFailureNotZero 端点查不到时如实报原因，不显示 0。
func TestBalanceHandlerReportsFailureNotZero(t *testing.T) {
	p := NewWithConfig(Config{})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{
			Provider: ProviderID,
			UID:      uid,
			Secret:   &Auth{AccessToken: "t"},
		}, true
	})
	// 默认基址是真实上游 —— 用假基址避免真的发请求
	p.client = NewWithBase("http://127.0.0.1:1")

	rec := httptest.NewRecorder()
	p.handleBalance(rec, httptest.NewRequest(http.MethodGet, adminBalancePath+"?uid=u1", nil))

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["ok"] != false {
		t.Errorf("查不到时 ok 应为 false，得到 %v", out["ok"])
	}
	if _, has := out["total"]; has {
		t.Error("查不到时**不得**下发 total（0 是「已用光」的语义）")
	}
}

// TestRewardHandlerIdempotentReportsAlreadyClaimed 已领过时报 already-claimed。
func TestRewardHandlerIdempotentReportsAlreadyClaimed(t *testing.T) {
	p := NewWithConfig(Config{})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{Provider: ProviderID, UID: uid, Secret: &Auth{AccessToken: "t"}}, true
	})
	// 假上游：granted:false（已领过）
	mux := http.NewServeMux()
	mux.HandleFunc(desktopPrefix+"/login/points/grant", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"granted":false}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p.client = NewWithBase(srv.URL)

	rec := httptest.NewRecorder()
	p.handleLoginReward(rec, httptest.NewRequest(http.MethodPost, adminLoginRewardPath+"?uid=u1", nil))

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["kind"] != "already-claimed" {
		t.Errorf("kind = %v，want already-claimed —— "+
			"报 claimed 会让用户以为每次都真的加了额度", out["kind"])
	}
	if !strings.Contains(out["message"].(string), "每号一次") {
		t.Errorf("文案应说明这是每号一次的奖励，得到 %v", out["message"])
	}
}
