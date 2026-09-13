package codearts

import (
	"os"
	"strings"
	"testing"
)

// liveAuthForWelfare 取真实凭证用于联网测试（无凭证则跳过）。
//
// 默认路径是仓库根的 ./auths —— 测试的工作目录是包目录
// (internal/codearts)，所以要往上走两级。
//
// # ⚠ T2 修正：必须同时扫 `auths/codearts/` 子目录
//
// 目录布局改成"按上游分子目录"（`auths/<provider>/`）之后，
// codearts 的凭证落到了 `auths/codearts/` —— **而这里没跟着改**。
// 于是 LoadDir 在 `../../auths` 里只找到 workbuddy 的文件（它按
// `codearts*.json` 通配，一个都匹配不到），len(creds)==0，
// **本测试永远 Skip**。
//
// 后果不是"测试失败"，而是"测试存在但从不执行" —— 实测代价：
// "codearts 到底能不能取到额度"从来没有被真正验证过，
// 直到 T2 手工跑真实凭证才发现它其实能取到（7474.04）而界面上是 0。
//
// 所以把子目录加进候选：不修的话这条 live 覆盖永远是假的。
func liveAuthForWelfare(t *testing.T) *Auth {
	t.Helper()
	p := os.Getenv("CORARTS_TEST_CRED")
	candidates := []string{
		p,
		"../../auths/codearts", // 按上游分子目录后的真实位置
		"../../auths",          // 兼容旧布局
		"./auths",
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if creds, err := LoadDir(dir); err == nil && len(creds) > 0 {
			return creds[0]
		}
	}
	t.Skip("无可用凭证，跳过联网测试（先跑 go run ./cmd/login -out ./auths）")
	return nil
}

// TestFetchWelfareLive 端到端验证福利列表接口。
//
// 契约来自 IDE 运行日志（不是扩展源码里的拼法 —— 那个是 404）。
// 这个测试的价值是**锁住实测可用性**：若上游改了前缀或头，
// 这里立刻失败，而不是等用户点"签到"时才发现。
func TestFetchWelfareLive(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	a := liveAuthForWelfare(t)
	c := New()

	items, err := c.FetchWelfare(a)
	if err != nil {
		t.Fatalf("拉取福利列表失败: %v", err)
	}
	t.Logf("拿到 %d 个福利活动", len(items))
	for _, it := range items {
		t.Logf("  campaignId=%d  %s  额度=%d%s  可领=%v  有效期 %s→%s",
			it.CampaignID, it.Title, it.BenefitAmount, it.BenefitUnit,
			it.Claimable, it.Extra.StartTime, it.Extra.EndTime)
	}

	// 实测账号至少有"每日签到"这个 campaign
	if len(items) == 0 {
		// token 套餐账号确实会返回空，属正常，但要能明确区分
		sub, serr := c.FetchSubscription(a)
		if serr == nil && sub.IsTokenPack {
			t.Skip("该账号是 token 套餐，福利列表本就为空（符合预期）")
		}
		t.Fatal("非 token 套餐却拿不到任何福利活动")
	}

	found := false
	for _, it := range items {
		if it.Type == "USER_LOGIN" {
			found = true
			if it.BenefitAmount <= 0 {
				t.Errorf("每日签到额度异常: %d", it.BenefitAmount)
			}
			if it.BenefitUnit != "CREDIT" {
				t.Errorf("每日签到单位异常: %q", it.BenefitUnit)
			}
		}
	}
	if !found {
		t.Error("未找到 USER_LOGIN（每日签到）类型的活动")
	}
}

// TestFetchSubscriptionLive 验证套餐/额度接口。
//
// 这是管理台"剩余积分"的来源 —— 此前一直显示 0 就是因为没接它。
func TestFetchSubscriptionLive(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	a := liveAuthForWelfare(t)
	c := New()

	sub, err := c.FetchSubscription(a)
	if err != nil {
		t.Fatalf("拉取订阅失败: %v", err)
	}
	t.Logf("套餐: %s (%s)  信用包=%v  token包=%v  状态=%s",
		sub.PackageNameCN, sub.SpecCode, sub.IsCreditPack, sub.IsTokenPack, sub.Status)
	t.Logf("有效期: %s → %s", sub.StartDate, sub.EndDate)
	t.Logf("积分: 总 %.0f  剩余 %.2f  已用 %.2f", sub.CreditTotal, sub.CreditRemain, sub.CreditUsed)

	if sub.SpecCode == "" {
		t.Error("spec_code 为空 —— 响应结构可能变了")
	}
	if sub.IsCreditPack && sub.CreditTotal <= 0 {
		t.Error("积分套餐但总额为 0 —— 解析 metrics 可能有问题")
	}
}

// TestClaimAllWelfareLive 全量领取（幂等，已领过会返回"今日已领"）。
//
// 注意：这个测试**会真的发领取请求**。服务端按 idempotentKey + 活动周期去重，
// 重复调用不会重复发放，所以是安全的。
func TestClaimAllWelfareLive(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	if os.Getenv("CORARTS_TEST_CLAIM") != "1" {
		t.Skip("未设 CORARTS_TEST_CLAIM=1，跳过（避免误触真实领取）")
	}
	a := liveAuthForWelfare(t)
	c := New()

	results, err := c.ClaimAllWelfare(a)
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	for _, r := range results {
		status := "未领"
		if r.Claimed {
			status = "已领"
		}
		t.Logf("  campaignId=%d %-24s %s  %s", r.CampaignID, r.Title, status, r.Message)
	}

	// 关键断言：无论成功与否，都不能返回空 message 的静默失败
	for _, r := range results {
		if !r.Claimed && strings.TrimSpace(r.Message) == "" {
			t.Errorf("campaignId=%d 未领取但无原因说明", r.CampaignID)
		}
	}
}
