package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// creditsFake 一个假的 /sash/ 上游，按路径分发。
//
// 每个用例只关心"服务端返回什么"，所以这里收的是**响应体**而不是请求，
// 请求侧的行为由下面几条专门的用例断言（头、body 形态）。
type creditsFake struct {
	// usageBody /sash/api/v2/me/usage 的响应（空 = 404）
	usageBody string
	// campaignsBody /sash/api/v1/me/campaigns 的响应（空 = 404）
	campaignsBody string
	// claimBody 领取响应（空 = 200 空体）
	claimBody string
	// claimStatus 领取的 HTTP 状态码（0 → 200）
	claimStatus int
	// requests 记录收到的请求（用于断言头与 body）
	requests []*http.Request
	// bodies 记录请求体（断言 claim 的 body 为空）
	bodies []string
}

func newCreditsServer(t *testing.T, f *creditsFake) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(usagePath, func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r)
		if f.usageBody == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.usageBody))
	})
	mux.HandleFunc(campaignsPath, func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r)
		if f.campaignsBody == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.campaignsBody))
	})
	mux.HandleFunc(campaignsPath+"/", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r)
		b, _ := json.Marshal(r.URL.Path)
		f.bodies = append(f.bodies, string(b))
		if f.claimStatus != 0 {
			w.WriteHeader(f.claimStatus)
		}
		if f.claimBody != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.claimBody))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewWithBase(srv.URL)
}

func testAuth() *Auth {
	return &Auth{AccessToken: "at-test", UID: "u1", MachineID: "m1"}
}

// ── 余额 ────────────────────────────────────────────────────────────────

// TestBalanceReadsAddOnQuotaToo 余额**不只在 userQuota 里**。
//
// # 这条钉住的是一个真实缺陷形态
//
// 实测某账号 `userQuota.remaining = 0` 而 `addOnQuota.remaining = 100`
// （用户说的「资源包 100 积分」正是后者）。只读 userQuota 会显示 0 ——
// 与其它 provider 的「漏读某一层」是同一类缺陷。
//
// 变异可检：把 addOnQuota 那一段删掉 → 本用例红（total 变 0）。
func TestBalanceReadsAddOnQuotaToo(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{usageBody: `{
		"displayMode":"qoder",
		"qoderUsage":{
			"userType":"personal_standard",
			"userQuota":{"total":0,"used":0,"remaining":0,"unit":"credits"},
			"addOnQuota":{"total":100,"used":0,"remaining":100,"unit":"credits"},
			"expiresAt":253402214400000
		}}`})

	b, ok := c.FetchCreditBalance(context.Background(), testAuth())
	if !ok {
		t.Fatal("应当查到余额")
	}
	if b.Total != 100 {
		t.Errorf("total = %v，want 100 —— "+
			"只读 userQuota 会得到 0（漏读资源包），那正是本用例要防的缺陷", b.Total)
	}
	if len(b.Packages) != 2 {
		t.Fatalf("包数 = %d，want 2（套餐额度 + 资源包）", len(b.Packages))
	}
	if b.Packages[0].Name != "套餐额度" || b.Packages[1].Name != "资源包" {
		t.Errorf("包顺序/名字不对：%q / %q（顺序即展示顺序）",
			b.Packages[0].Name, b.Packages[1].Name)
	}
}

// TestBalanceEnterpriseReturnsNotFound 企业版没有额度数字，必须报"查不到"而不是 0。
//
// 报 0 会让用户以为额度被清空了，而真相是"这个账号看不到数字"。
func TestBalanceEnterpriseReturnsNotFound(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{usageBody: `{
		"displayMode":"enterprise",
		"enterpriseUsage":{"detailUrl":"https://x"}}`})

	if _, ok := c.FetchCreditBalance(context.Background(), testAuth()); ok {
		t.Error("企业版应返回 ok=false（无额度数字），报 0 会误导用户")
	}
}

// TestBalanceClampsNegative 负值必须 clamp 到 0。
//
// 服务端在超额扣费/计量回滚下可能下发负值，原样透出会让卡片显示「-12.5 积分」。
func TestBalanceClampsNegative(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{usageBody: `{
		"displayMode":"qoder",
		"qoderUsage":{"userQuota":{"total":10,"used":25,"remaining":-15,"unit":"credits"}}}`})

	b, ok := c.FetchCreditBalance(context.Background(), testAuth())
	if !ok {
		t.Fatal("应当查到余额")
	}
	if b.Total < 0 {
		t.Errorf("total = %v，负值必须 clamp 到 0", b.Total)
	}
}

// TestBalanceComputesRemainingWhenMissing remaining 缺失时按 total-used 算。
func TestBalanceComputesRemainingWhenMissing(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{usageBody: `{
		"displayMode":"qoder",
		"qoderUsage":{"userQuota":{"total":100,"used":30,"unit":"credits"}}}`})

	b, ok := c.FetchCreditBalance(context.Background(), testAuth())
	if !ok {
		t.Fatal("应当查到余额")
	}
	if b.Total != 70 {
		t.Errorf("total = %v，want 70（total-used）", b.Total)
	}
}

// TestBalanceEmptyShapeIsNotFound 一个包都解析不出来 → "查不到"，不是"余额 0"。
func TestBalanceEmptyShapeIsNotFound(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{usageBody: `{"displayMode":"qoder","qoderUsage":{}}`})
	if _, ok := c.FetchCreditBalance(context.Background(), testAuth()); ok {
		t.Error("形状与预期不符应返回 ok=false，而不是余额 0（后者会让用户以为额度被清空）")
	}
}

// TestBalanceNetworkFailureIsNotFound 网络失败/404 同样返回 ok=false。
func TestBalanceNetworkFailureIsNotFound(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{})
	if _, ok := c.FetchCreditBalance(context.Background(), testAuth()); ok {
		t.Error("404 应返回 ok=false")
	}
}

// ── 请求头（两个头都必需，缺一服务端不下发可领活动） ──

// TestCreditsHeadersUseSashClientType 必须用 SashClientType（10 = 桌面 app）。
//
// 消融实验：用 client_type=5（CLI）时 /campaigns 恒返回 campaigns:[]。
func TestCreditsHeadersUseSashClientType(t *testing.T) {
	f := &creditsFake{campaignsBody: `{"showCampaign":false,"claimable":false,"campaigns":[]}`}
	c := newCreditsServer(t, f)
	_, _ = c.FetchCheckinStatus(context.Background(), testAuth())

	if len(f.requests) == 0 {
		t.Fatal("没有发出请求")
	}
	got := f.requests[0].Header.Get("Cosy-ClientType")
	if got != "10" {
		t.Errorf("Cosy-ClientType = %q，want 10（桌面 app 身份）—— "+
			"用 5（CLI）时服务端恒返回 campaigns:[]，签到会被误判成「无活动」", got)
	}
}

// disableLiveMachineIdentity 强制走**磁盘缓存退路**（禁掉实时 spawn）。
//
// # 为什么每个碰 machine 身份的用例都必须调它
//
// `resolveMachineIdentity` 的主路径是实时 spawn `runtime-info.exe`
// （见 runtimeinfo.go）。若不隔离，在**装了 Qoder 的开发机**上：
//
//   - 用例会真的去 spawn（每次约 3.8 秒，整套测试白白变慢）
//   - **拿到的是开发机的真实身份**，而用例断言的是 fixture 里的值 → 假红
//   - 更糟：在没装 Qoder 的 CI 上走缓存、在开发机上走 spawn，
//     同一份用例行为不同 —— 结果随环境漂移
//
// 指向一个**不存在的路径**即等价于"本机没装 Qoder"，于是自然退回缓存。
// 这与参照实现的 `QODER_RUNTIME_INFO` 隔离手段同一个用法。
func disableLiveMachineIdentity(t *testing.T) {
	t.Helper()
	t.Setenv("QODER_RUNTIME_INFO", "/nonexistent/runtime-info")
	resetMachineIdentity()
	t.Cleanup(resetMachineIdentity)
}

// TestCreditsHeadersMachinePairIsAtomic machine 头**必须成对**，不能只发一个。
//
// 消融实验：去掉 MachineToken 或 MachineType 任一 → 服务端退回 1 条
// VIEW_DETAILS（没有 CLAIM_BENEFIT）→ 会被误判成「今天已领」。
func TestCreditsHeadersMachinePairIsAtomic(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/machine_token.json"
	writeText(t, fp, `{"token":"tok-1","type":"typ-1"}`)
	t.Setenv("QODER_MACHINE_TOKEN_PATH", fp)
	disableLiveMachineIdentity(t)

	f := &creditsFake{campaignsBody: `{"showCampaign":false,"claimable":false,"campaigns":[]}`}
	c := newCreditsServer(t, f)
	_, _ = c.FetchCheckinStatus(context.Background(), testAuth())

	h := f.requests[0].Header
	tok, typ := h.Get("Cosy-MachineToken"), h.Get("Cosy-MachineType")
	if (tok == "") != (typ == "") {
		t.Fatalf("machine 头必须成对：token=%q type=%q —— "+
			"只发一个等于没发（消融实验：缺一即失效）", tok, typ)
	}
	if tok != "tok-1" || typ != "typ-1" {
		t.Errorf("machine 头值不对：%q / %q", tok, typ)
	}
}

// TestCreditsHeadersOmitMachineWhenUnavailable 拿不到 machine 身份时**照常发请求**。
//
// 用户若未安装 Qoder 桌面端就没有该文件 —— 此时不能让整个积分功能报错
// （保守降级：少一次可领活动，而不是功能不可用）。
func TestCreditsHeadersOmitMachineWhenUnavailable(t *testing.T) {
	t.Setenv("QODER_MACHINE_TOKEN_PATH", "/nonexistent/path/machine_token.json")
	disableLiveMachineIdentity(t)

	f := &creditsFake{campaignsBody: `{"showCampaign":false,"claimable":false,"campaigns":[]}`}
	c := newCreditsServer(t, f)
	_, ok := c.FetchCheckinStatus(context.Background(), testAuth())

	if !ok {
		t.Fatal("拿不到 machine 身份时仍应发出请求并拿到响应（保守降级）")
	}
	if len(f.requests) == 0 {
		t.Fatal("没有发出请求 —— 拿不到 machine 身份不该阻止请求")
	}
	if f.requests[0].Header.Get("Cosy-MachineToken") != "" {
		t.Error("拿不到身份时不该带 machine 头")
	}
}

// TestMachineIdentityRequiresBothFields 两个字段都要有，缺一视为拿不到。
func TestMachineIdentityRequiresBothFields(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, body string }{
		{"只有 token", `{"token":"t"}`},
		{"只有 type", `{"type":"x"}`},
		{"token 为空", `{"token":"","type":"x"}`},
		{"非法 JSON", `{not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := dir + "/mt.json"
			writeText(t, fp, tc.body)
			t.Setenv("QODER_MACHINE_TOKEN_PATH", fp)
			disableLiveMachineIdentity(t)

			if got := resolveMachineIdentity(); got != nil {
				t.Errorf("应当视为拿不到（配对是必要条件），得到 %+v", got)
			}
		})
	}
}

// ── 签到状态（判据最微妙的一处） ──

// TestCheckinStatusClaimedMeansAlreadyClaimed 有 CLAIM_BENEFIT+CLAIMED ⇒ 今天已领。
//
// 2026-09-21 抓包给了同一账号的领取前后对照：
//
//	领取前  claimable=true   那条 CLAIM_BENEFIT 的 claimStatus=CLAIMABLE
//	领取后  claimable=false  那条 CLAIM_BENEFIT 的 claimStatus=CLAIMED
//
// 故「有 CLAIM_BENEFIT + CLAIMED」是「已领」的**充分且可靠**判据。
func TestCheckinStatusClaimedMeansAlreadyClaimed(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{campaignsBody: `{
		"showCampaign":true,"claimable":false,
		"campaigns":[
			{"campaignId":"c1","campaignKey":"act-1","actionType":"CLAIM_BENEFIT",
			 "claimStatus":"CLAIMED","benefit":{"amount":100}}
		]}`})

	st, ok := c.FetchCheckinStatus(context.Background(), testAuth())
	if !ok {
		t.Fatal("应当查到状态")
	}
	if !st.TodayCheckedIn {
		t.Error("有 CLAIM_BENEFIT+CLAIMED 应判「今天已领」")
	}
	if !st.Active {
		t.Error("拿到响应即 active=true（不能按列表非空判定 —— " +
			"服务端在「今天已领」时会把 campaigns 清空）")
	}
}

// TestCheckinStatusEmptyListIsNotAlreadyClaimed 空列表**不等于**今天已领。
//
// # 这条是一个真实缺陷的直接防线
//
// 用户报障：「没领过就显示已经领取，去 IDE 看还是可以领取的状态」。
// 「列表里没有可领项」不等于「今天领过了」—— 它还可能是
// ① 未到刷新时间（每日 10:00 UTC+8）、② 请求头不完整导致服务端未下发、
// ③ 该账号本就无此类活动。三者都不是「已领」。
//
// 方向取保守：误报「未领」最多让用户多点一次（服务端幂等，回 replayed:true，
// 无害）；误报「已领」会让其**真的错过当天积分**。
func TestCheckinStatusEmptyListIsNotAlreadyClaimed(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{campaignsBody: `{
		"showCampaign":false,"claimable":false,"campaigns":[]}`})

	st, ok := c.FetchCheckinStatus(context.Background(), testAuth())
	if !ok {
		t.Fatal("应当查到状态")
	}
	if st.TodayCheckedIn {
		t.Error("空列表**不得**判成「今天已领」—— " +
			"「没有可领项」还可能是未到刷新时间/请求头不完整/本就无活动，" +
			"误报已领会让用户真的错过当天积分")
	}
}

// TestCheckinStatusViewDetailsOnlyIsNotAlreadyClaimed 只有 VIEW_DETAILS 也不算已领。
//
// 「Pro 首月翻倍」这类活动是 VIEW_DETAILS（仅跳转详情），**不该尝试领取**，
// 也不代表"已领"。
func TestCheckinStatusViewDetailsOnlyIsNotAlreadyClaimed(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{campaignsBody: `{
		"showCampaign":true,"claimable":false,
		"campaigns":[
			{"campaignId":"c1","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}
		]}`})

	st, ok := c.FetchCheckinStatus(context.Background(), testAuth())
	if !ok {
		t.Fatal("应当查到状态")
	}
	if st.TodayCheckedIn {
		t.Error("只有 VIEW_DETAILS 不得判成「已领」")
	}
}

// TestCheckinStatusClaimableReportsDailyCredit 可领时报告额度。
func TestCheckinStatusClaimableReportsDailyCredit(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{campaignsBody: `{
		"showCampaign":true,"claimable":true,
		"campaigns":[
			{"campaignId":"c1","campaignKey":"act-20260921-308","actionType":"CLAIM_BENEFIT",
			 "claimStatus":"CLAIMABLE","benefit":{"amount":100}}
		]}`})

	st, ok := c.FetchCheckinStatus(context.Background(), testAuth())
	if !ok {
		t.Fatal("应当查到状态")
	}
	if st.TodayCheckedIn {
		t.Error("有 CLAIMABLE 项时不该判「已领」")
	}
	if st.DailyCredit != 100 {
		t.Errorf("dailyCredit = %v，want 100", st.DailyCredit)
	}
	if st.ActivityName != "act-20260921-308" {
		t.Errorf("activityName = %q", st.ActivityName)
	}
}

// ── 领取 ────────────────────────────────────────────────────────────────

// TestClaimReplayedMeansAlreadyClaimed 幂等判据是 `replayed`，**不是** HTTP 状态码。
//
// ⚠ 重复领取同样返回 **200**，但 `replayed:true`、**不含 benefit**、
// 且 claimedAt 是上一次领取的旧时间。只看状态码会把「今天已领」误报成
// 「领取成功 +100」。
//
// # ⚠ 判据为什么必须落在 claimOne 上（我在这里连踩两次）
//
// **第一次**：只断言 `ClaimDailyCheckin` 的 (Kind, Credit)。
// 变异（把 replayed 判断短路）后测试**依然全绿**。
//
// **第二次**：补上 Message 断言，**还是绿**。追下去才看清原因 ——
// 聚合层（ClaimDailyCheckin）有**两条**路径都产出 already-claimed：
//
//	claimOne 认出 replayed          → 循环里 total=0 → 落到末尾 already-claimed
//	claimOne 不认 replayed（变异）  → 返回 claimed/credit=0 → total 仍为 0
//	                                → **同样**落到末尾 already-claimed
//
// 也就是说这个变异在**聚合层的可观测输出上不可见** —— 两条路径殊途同归。
// 但它们在 `claimOne` 上完全不同（already-claimed vs claimed），
// 而那个差别是有意义的：`claimed` 会被累加进 total（若服务端回放时
// 恰好带了 benefit，就会把"今天已领"误报成"+100"）。
//
// 所以判据下沉到 claimOne —— 那里才是这个判断真正生效的地方。
func TestClaimReplayedMeansAlreadyClaimed(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		campaignsBody: `{"showCampaign":true,"claimable":true,"campaigns":[
			{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
			 "benefit":{"amount":100}}]}`,
		claimBody: `{"status":"CLAIMED","replayed":true,
			"campaignId":"c1","claimedAt":"2026-09-18T14:54:12Z"}`,
	})

	// 直接测 claimOne —— 两个分支在**这里**才可区分（见上方长注释）。
	out := c.claimOne(context.Background(), testAuth(), "c1")
	if out.Kind != "already-claimed" {
		t.Errorf("claimOne kind = %q，want already-claimed —— "+
			"replayed:true 表示此前已领（服务端回放上次结果）；"+
			"判成 claimed 会让它被累加进 total，把「今天已领」误报成加分",
			out.Kind)
	}
	if out.Message != "今天已领取" {
		t.Errorf("claimOne message = %q，want 「今天已领取」", out.Message)
	}

	// 聚合层也要给出一致的结论（用户看到的那一层）。
	agg := c.ClaimDailyCheckin(context.Background(), testAuth())
	if agg.Kind != "already-claimed" || agg.Credit != 0 {
		t.Errorf("聚合层 = (%q, %v)，want (already-claimed, 0)", agg.Kind, agg.Credit)
	}
}

// TestClaimReplayedWithBenefitIsNotCounted 变异守卫：replayed 响应**即使带 benefit**
// 也不得被计成"领取成功"。
//
// # 为什么单独造这个输入
//
// 上一条用例的 replayed 响应不含 benefit，于是"短路 replayed 判断"这个变异
// 算出的 credit 也是 0 —— 与正确路径殊途同归（见那里的长注释）。
//
// 这条**故意让 replayed 响应带上 benefit**：真实服务端不会这么回
// （回放的是上次结果，不含新 benefit），但它是**判别性输入** ——
// 短路判断时 credit 会变成 100 并被累加进 total，
// 于是聚合层会报 "claimed +100"，把「今天已领」误报成「加 100 分」。
//
// 这正是那个变异**唯一**能造成用户可见后果的形态，所以必须钉住它。
func TestClaimReplayedWithBenefitIsNotCounted(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		campaignsBody: `{"showCampaign":true,"claimable":true,"campaigns":[
			{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
			 "benefit":{"amount":100}}]}`,
		// ⚠ 判别性输入：replayed:true **且**带 benefit
		claimBody: `{"status":"CLAIMED","replayed":true,"benefit":{"amount":100}}`,
	})

	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.Kind == "claimed" || out.Credit != 0 {
		t.Errorf("聚合层 = (%q, %v)，want already-claimed/0 —— "+
			"replayed:true 必须优先于 benefit 判断；"+
			"短路它会把「今天已领」误报成「+%v 分」", out.Kind, out.Credit, out.Credit)
	}
}

// TestClaimSuccessReturnsAmount 正常领取返回金额。
func TestClaimSuccessReturnsAmount(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		campaignsBody: `{"showCampaign":true,"claimable":true,"campaigns":[
			{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
			 "benefit":{"amount":100}}]}`,
		claimBody: `{"grantId":"g1","status":"CLAIMED","replayed":false,
			"benefit":{"kind":"CREDITS","amount":100},
			"campaignId":"c1","campaignKey":"act-20260921-308"}`,
	})

	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.Kind != "claimed" {
		t.Fatalf("kind = %q，want claimed（%s）", out.Kind, out.Message)
	}
	if out.Credit != 100 {
		t.Errorf("credit = %v，want 100", out.Credit)
	}
}

// TestClaimNoTargetsIsInactiveNotAlreadyClaimed 无可领活动必须是 inactive。
//
// # 这是另一个真实缺陷的直接防线
//
// 旧实现在 targets 为空时直接返回「今天已领取」，于是只要服务端没下发
// 可领项（含请求头不完整、未到刷新时间、本就无活动三种情形），
// 界面就显示「今天已领取」，与 IDE 的「可领取」直接矛盾。
//
// 二者语义完全不同：inactive = 没东西可领；already-claimed = 领过了。
func TestClaimNoTargetsIsInactiveNotAlreadyClaimed(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		campaignsBody: `{"showCampaign":false,"claimable":false,"campaigns":[]}`,
		usageBody:     `{"displayMode":"qoder","qoderUsage":{"userQuota":{"total":1,"used":0,"remaining":1},"addOnQuota":{"total":100,"used":0,"remaining":100}}}`,
	})

	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.Kind == "already-claimed" {
		t.Fatal("无可领活动**不得**报 already-claimed —— " +
			"那会让用户以为今天领过了，而其实只是没东西可领（真实缺陷）")
	}
	if out.Kind != "inactive" {
		t.Errorf("kind = %q，want inactive", out.Kind)
	}
}

// TestClaimNotActivatedGivesActionableHint 未开通时给**可操作**提示。
//
// 判据（两条同时满足，避免误报）：
//
//  1. 活动列表里没有 CLAIM_BENEFIT（连已领的都没有）
//  2. 用量响应里 addOnQuota 字段**不存在**（缺失，不是 0 ——
//     已开通账号即使额度用尽也有该字段）
//
// ⚠ 第 2 条用「字段是否存在」而非「remaining 是否为 0」：后者对
// 「额度用光」与「从未开通」不可区分，会把用光额度的老账号误报成未开通。
func TestClaimNotActivatedGivesActionableHint(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		campaignsBody: `{"showCampaign":false,"claimable":false,"campaigns":[]}`,
		// ⚠ 没有 addOnQuota 字段
		usageBody: `{"displayMode":"qoder","qoderUsage":{"userQuota":{"total":0,"used":0,"remaining":0}}}`,
	})

	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.Kind != "inactive" {
		t.Fatalf("kind = %q，want inactive", out.Kind)
	}
	if !out.ActionRequired {
		t.Error("未开通时必须置 action_required —— " +
			"那是给 UI 的显式信号（需要用户去官方客户端登录一次），" +
			"而不是让它去猜文案")
	}
	if !strings.Contains(out.Message, "官方客户端") {
		t.Errorf("提示要**可操作**（应指引去官方客户端登录），得到 %q", out.Message)
	}
}

// TestClaimUsedUpQuotaIsNotReportedAsNotActivated 额度用光 ≠ 未开通。
//
// ⚠ 这条是上面那条的**反向**判据，防"用字段存在性之外的方式判断"。
// 已开通账号即使额度用尽也会有 addOnQuota 字段（{total:100, remaining:0}），
// 若改用「remaining 是否为 0」判断，这个账号会被误报成未开通。
func TestClaimUsedUpQuotaIsNotReportedAsNotActivated(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		campaignsBody: `{"showCampaign":false,"claimable":false,"campaigns":[]}`,
		// addOnQuota **存在但已用尽**
		usageBody: `{"displayMode":"qoder","qoderUsage":{"userQuota":{"total":0,"used":0,"remaining":0},"addOnQuota":{"total":100,"used":100,"remaining":0}}}`,
	})

	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.ActionRequired {
		t.Error("额度用尽的**已开通**账号不得被判成「未开通」—— " +
			"判据必须是「addOnQuota 字段是否存在」，不是「remaining 是否为 0」")
	}
}

// TestClaimEmptyBodyIsSent 领取请求体必须是**空串**。
//
// 抓包实测 `content-length: 0`。源码里领取走 POST 但无 payload；
// 发 `{}` 之类未经验证的 body 属额外风险。
func TestClaimEmptyBodyIsSent(t *testing.T) {
	f := &creditsFake{
		campaignsBody: `{"showCampaign":true,"claimable":true,"campaigns":[
			{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
			 "benefit":{"amount":100}}]}`,
		claimBody: `{"status":"CLAIMED","replayed":false,"benefit":{"amount":100}}`,
	}
	c := newCreditsServer(t, f)
	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.Kind != "claimed" {
		t.Fatalf("kind = %q（%s）", out.Kind, out.Message)
	}
	// 找到 claim 那次请求（路径里含 /claim）
	var found bool
	for i, r := range f.requests {
		if strings.Contains(r.URL.Path, "/claim") {
			found = true
			if cl := r.ContentLength; cl > 0 {
				t.Errorf("claim 请求体长度 = %d，want 0（抓包实测 content-length: 0）", cl)
			}
			_ = i
		}
	}
	if !found {
		t.Fatal("没有发出 claim 请求")
	}
}

// TestClaimMultipleTargetsClaimsAll 一个账号可能同时有多个可领活动，逐个领取。
func TestClaimMultipleTargetsClaimsAll(t *testing.T) {
	f := &creditsFake{
		campaignsBody: `{"showCampaign":true,"claimable":true,"campaigns":[
			{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}},
			{"campaignId":"c2","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":50}}]}`,
		claimBody: `{"status":"CLAIMED","replayed":false,"benefit":{"amount":100}}`,
	}
	c := newCreditsServer(t, f)
	out := c.ClaimDailyCheckin(context.Background(), testAuth())

	if out.Kind != "claimed" {
		t.Fatalf("kind = %q（%s）", out.Kind, out.Message)
	}
	if out.Credit != 200 {
		t.Errorf("credit = %v，want 200（两个活动各 100）—— "+
			"只领第一个会漏掉其它运营活动", out.Credit)
	}
	n := 0
	for _, r := range f.requests {
		if strings.Contains(r.URL.Path, "/claim") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("发出了 %d 次 claim 请求，want 2（逐个领取）", n)
	}
}

// TestClaimFailureIsReported 失败要如实报，不能吞掉。
func TestClaimFailureIsReported(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		campaignsBody: `{"showCampaign":true,"claimable":true,"campaigns":[
			{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
			 "benefit":{"amount":100}}]}`,
		claimStatus: 500,
		claimBody:   `boom`,
	})

	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.Kind != "failed" {
		t.Errorf("kind = %q，want failed（服务端 500）", out.Kind)
	}
	if out.Message == "" {
		t.Error("失败必须带原因")
	}
}

// TestClaimNon2xxCredentialGivesReadableReason 凭据失效时给可读原因。
func TestClaimNon2xxCredentialGivesReadableReason(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{
		campaignsBody: `{"showCampaign":true,"claimable":true,"campaigns":[
			{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE",
			 "benefit":{"amount":100}}]}`,
		claimStatus: 401,
	})

	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.Kind != "failed" {
		t.Fatalf("kind = %q，want failed", out.Kind)
	}
	if !strings.Contains(out.Message, "凭据") {
		t.Errorf("401 应给「凭据已失效，请重新登录」这类可读原因，得到 %q", out.Message)
	}
}

// TestCampaignsUnavailableIsFailed 活动列表查不到时明确失败（不是"无活动"）。
//
// 两者语义不同：查不到 = 无法判断；无活动 = 确认没有。
// 混为一谈会让网络故障伪装成"今天没有可领的活动"。
func TestCampaignsUnavailableIsFailed(t *testing.T) {
	c := newCreditsServer(t, &creditsFake{})
	out := c.ClaimDailyCheckin(context.Background(), testAuth())
	if out.Kind != "failed" {
		t.Errorf("kind = %q，want failed（活动列表查不到时不能报 inactive —— "+
			"那会把网络故障伪装成「今天没有可领的活动」）", out.Kind)
	}
}

// ── 契约：能力声明必须可达 ──

// TestAdminRoutesExposeCheckin 声明 CapCheckin 就必须有对应的管理端点。
//
// gateway 的契约测试强制这条（"声明了的能力必须真的可达"）。
// 这里直接断言路由存在且路径按**实例 ID** 生成 —— 两个产品是同类型的
// 两份实例，路径必须区分，否则路由表里两条同路径注册冲突。
func TestAdminRoutesExposeCheckin(t *testing.T) {
	p := NewWithConfig(Config{})
	routes := p.AdminRoutes()
	if len(routes) < 2 {
		t.Fatalf("管理端点只有 %d 条，want ≥2（签到 + 余额）", len(routes))
	}
	paths := map[string]gateway.Capability{}
	for _, r := range routes {
		paths[r.Path] = r.Capability
	}
	if cap, ok := paths["/admin/qoder/checkin"]; !ok {
		t.Errorf("缺少 /admin/qoder/checkin（实际 %v）", keysOf(paths))
	} else if cap != gateway.CapCheckin {
		t.Errorf("签到端点的 Capability = %v，want CapCheckin", cap)
	}
	if _, ok := paths["/admin/qoder/balance"]; !ok {
		t.Errorf("缺少 /admin/qoder/balance（实际 %v）", keysOf(paths))
	}

	// 中国版实例的路径必须不同（同类型的另一份实例）
	cn := NewWithConfig(Config{Product: QoderCN})
	for _, r := range cn.AdminRoutes() {
		if strings.Contains(r.Path, "/qoder/") {
			t.Errorf("qodercn 实例的端点路径 %q 与国际版冲突 —— "+
				"两个产品共用一套端点会注册冲突", r.Path)
		}
	}
}

// TestCapsDeclaresCheckin 能力声明必须包含 CapCheckin（与路由一致）。
func TestCapsDeclaresCheckin(t *testing.T) {
	p := NewWithConfig(Config{})
	if !p.Caps().Has(gateway.CapCheckin) {
		t.Error("实现了签到并有管理端点，就该声明 CapCheckin（名实相符）")
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// writeText 写一个测试文件（不与 contract_hermetic_test.go 的 writeFile 重名）。
func writeText(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
