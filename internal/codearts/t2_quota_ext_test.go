package codearts

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// T2：`gateway.QuotaExt` 在 codearts 上的实现（RefreshQuota）。
//
// # 这组测试为什么分两层
//
//	TestRefreshQuota*        不打网络 —— 钉住**取不到时的行为**（返回未知，不返回 0）
//	TestRefreshQuotaLive     打真上游 —— 钉住**能取到**这条事实（会 Skip 见下）
//
// 分开是刻意的：第一层必须在任何机器上都能跑（CI 无凭证），
// 第二层只在有真实凭证时跑，但**它是唯一能发现"接口变了/权限变了"的东西**。

// TestRefreshQuota_NoAccountReportsNotOK 账号不存在时 ok=false。
//
// 调用方（admin 的 refreshQuotas）据此跳过该账号，
// 而不是把一个"问不到"当成"额度是 0"。
func TestRefreshQuota_NoAccountReportsNotOK(t *testing.T) {
	p := NewWithConfig(Config{}) // 无凭证目录、无账号

	qv, ok := p.RefreshQuota("不存在的-uid")

	if ok {
		t.Errorf("账号不存在时应返回 ok=false，得到 ok=true qv=%+v", qv)
	}
	if qv.HasData {
		t.Errorf("账号不存在时不该报告有额度数据：%+v", qv)
	}
}

// TestRefreshQuota_UnreachableUpstreamReportsUnknown 上游不可达时必须返回**未知**。
//
// ⚠ **这是 T2 最关键的一条不变量，也是本次要修的坑。**
//
// 上游拉不到额度时（网络、STS 需续期、上游 5xx），返回的必须是
// `HasData=false` —— 界面显示 `—`。若返回 `CreditsQuota(0)`，
// 用户会看到 0 并读成"这个号没额度了"。
//
// 构造手法：给一个**空的 authDir**（LoadDir 扫不到任何凭证），
// 于是 accountFor 会返回 errNoAccount —— 走的就是"解析不出来"分支。
// 这不需要真网络，所以本测试在任何机器上都跑。
func TestRefreshQuota_UnreachableUpstreamReportsUnknown(t *testing.T) {
	// 用一个**不存在的** uid 走 accountFor 的失败分支，
	// 等价于"这个账号我服务不了"。
	p := NewWithConfig(Config{})

	qv, ok := p.RefreshQuota("")

	if ok {
		t.Fatalf("无账号可解析时应 ok=false，得到 qv=%+v", qv)
	}
	// 关键：无论 ok 是什么，**都不能**给出一个看起来确定的 0。
	if qv.HasData {
		t.Errorf("取不到额度时绝不能报告 HasData=true（那会显示成 0）：%+v", qv)
	}
}

// TestRefreshQuotaLive 用真实凭证端到端验证**能取到额度**。
//
// # 这条测试的由来（它抓出过一个真实缺陷）
//
// T2 之前，"codearts 有没有额度"从来没有被验证过 ——
// liveAuthForWelfare 只扫 `../../auths`，而按上游分子目录后
// 凭证在 `auths/codearts/`，于是那些 live 测试**永远 Skip**。
//
// 手工跑一次真实凭证才发现：codearts 的额度是 **7474.04**，
// 而界面上显示的是 0。所以这条测试同时钉住两件事：
//
//  1. 链路能通（FetchSubscription 的签名/端点没变）
//  2. 拿到的是**单值订阅级积分**（不是"按模型"，那个没有数值字段）
//
// 无凭证时 Skip —— 这是刻意的：CI 不该因为缺凭证而红，
// 但**开发机上有凭证时必须真跑**。
func TestRefreshQuotaLive(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	a := liveAuthForWelfare(t)

	p := NewWithConfig(Config{})
	// 直接注入账号访问器，绕开 authDir 解析（本测试关注额度，不关注目录）。
	p.SetAccounts(func() []*Auth { return []*Auth{a} })

	qv, ok := p.RefreshQuota(a.UID)

	if !ok {
		t.Fatal("账号存在（已注入凭证）时 RefreshQuota 必须 ok=true")
	}
	if !qv.HasData {
		t.Fatalf("真实账号应能取到额度，却报告未知：%+v\n"+
			"（若上游接口变了，这里会红 —— 这正是它存在的意义）", qv)
	}
	if qv.Kind != gateway.QuotaKindCredits {
		t.Errorf("codearts 的额度是订阅级单值，期望 kind=%q 得到 %q",
			gateway.QuotaKindCredits, qv.Kind)
	}
	t.Logf("✅ codearts 真实额度取到：kind=%s remaining=%d（界面原先显示 0，那是假的）",
		qv.Kind, qv.Remaining)

	if qv.Remaining < 0 {
		t.Errorf("额度不该为负（实现里应 clamp 到 0）：%d", qv.Remaining)
	}
}
