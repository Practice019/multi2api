package admin

// jobcover_test.go 任务覆盖检查的判据守卫。
//
// 判据分三层：① 有能力吗（编译期事实）② 注册了吗（运行时事实）
// ③ 配对正确吗（①真 && ②假 ⇒ 报缺）。最容易写错的是 ③：
//   · 写成「②假就报缺」→ 本来没能力的上游全被标红，真正的缺口被噪声埋掉；
//   · 写成「①真就报缺」→ 注册了的也被标红，用户会去删一个正常任务。
//
// # 为什么夹具要造真 Provider
//
// 续期能力（CanRefresh）是**接口实现**这个编译期事实，providerInfo 里没有 ——
// 必须让 jobCoverageOf 拿到 registry，对 Provider 本体做
// gateway.ExtOf[gateway.CredentialRefresher] 断言。所以用 fakeProv 造最小 Provider。

import (
	"context"
	"testing"

	"workbuddy2api/internal/gateway"
)

// fakeProv 最小 Provider：只实现接口要求的 4 个方法。
type fakeProv struct {
	id   string
	caps gateway.Capability
}

func (f fakeProv) ID() string               { return f.id }
func (f fakeProv) Caps() gateway.Capability { return f.caps }
func (f fakeProv) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (f fakeProv) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

// fakeRefresher 在 fakeProv 基础上**多实现** CredentialRefresher。
//
// ⚠ 必须用嵌入而不是给 fakeProv 加方法：加了之后所有 fakeProv 都成了
//
//	「能续期」，就测不出「无能力不报缺」那条了。
type fakeRefresher struct{ fakeProv }

// RefreshCredential 满足 gateway.CredentialRefresher。
//
// ⚠ 方法名是 RefreshCredential 不是 Refresh，签名是「原地更新 + 只返 error」，
//
//	不是「返回新凭证」。写错方法名不会报错 —— 只是类型断言永远失败，
//	于是所有上游都显示「不能续期」，覆盖检查形同虚设。
func (f fakeRefresher) RefreshCredential(c gateway.Credential) error {
	return nil
}

// covFixture 造一套夹具：registry + providerInfo 列表。
//
// 四个上游覆盖四种形态：
//
//	both         签到能力 + 续期能力（任务一个都没注册）
//	with-checkin 签到能力 + 已注册签到任务
//	with-refresh 续期能力 + 已注册续期任务
//	no-cap       两种能力都没有（不该被报缺）
func covFixture(t *testing.T) (*gateway.Registry, []providerInfo) {
	t.Helper()
	reg := gateway.NewRegistry()
	add := func(pr gateway.Provider) {
		t.Helper()
		if err := reg.Register(pr); err != nil {
			t.Fatalf("注册假上游 %s 失败: %v", pr.ID(), err)
		}
	}
	add(fakeRefresher{fakeProv{id: "both", caps: gateway.CapCheckin | gateway.CapChat}})
	add(fakeProv{id: "with-checkin", caps: gateway.CapCheckin | gateway.CapChat})
	add(fakeRefresher{fakeProv{id: "with-refresh", caps: gateway.CapChat}})
	add(fakeProv{id: "no-cap", caps: gateway.CapChat})

	providers := []providerInfo{
		{ID: "both", Capabilities: []string{gateway.String(gateway.CapCheckin), gateway.String(gateway.CapChat)}},
		{ID: "with-checkin", Capabilities: []string{gateway.String(gateway.CapCheckin), gateway.String(gateway.CapChat)}},
		{ID: "with-refresh", Capabilities: []string{gateway.String(gateway.CapChat)}},
		{ID: "no-cap", Capabilities: []string{gateway.String(gateway.CapChat)}},
	}
	return reg, providers
}

// TestJobCoverageMissingWhenCapableButUnregistered 有能力但没注册 ⇒ 报缺。
func TestJobCoverageMissingWhenCapableButUnregistered(t *testing.T) {
	reg, providers := covFixture(t)
	jobs := []uiJob{
		{Provider: "with-checkin", Name: "with-checkin-checkin"},
		{Provider: "with-refresh", Name: "with-refresh-refresh"},
	}
	byID := map[string]jobCoverage{}
	for _, c := range jobCoverageOf(providers, jobs, reg) {
		byID[c.Provider] = c
	}

	// 有能力 + 已注册 = 不报缺。
	if c := byID["with-checkin"]; !c.CanCheckin || !c.HasCheckin || c.MissingCheckin {
		t.Errorf("with-checkin 应「有能力且注册」不报缺，实得 %+v", c)
	}
	if c := byID["with-refresh"]; !c.CanRefresh || !c.HasRefresh || c.MissingRefresh {
		t.Errorf("with-refresh 应「有能力且注册」不报缺，实得 %+v", c)
	}

	// 有能力 + 未注册 = 报缺（both 两种能力都没对应任务）。
	if c := byID["both"]; !c.MissingCheckin || !c.MissingRefresh {
		t.Errorf("both 两种能力都有但没注册 —— 必须都报缺，实得 %+v", c)
	}

	// 无能力 = 不适用，**不许**报缺（否则页面全红）。
	if c := byID["no-cap"]; c.MissingCheckin || c.MissingRefresh {
		t.Errorf("no-cap 没有任何能力，不该报缺，实得 %+v", c)
	}
}

// TestJobCoverageIgnoresUnknownJobNames 不认识的任务名不该被当成签到/续期。
//
// ⚠ 守的是「宁可漏报也不要误报」：jobKindOf 只认 -checkin / -refresh 后缀。
func TestJobCoverageIgnoresUnknownJobNames(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(fakeProv{id: "wb", caps: gateway.CapCheckin}); err != nil {
		t.Fatal(err)
	}
	providers := []providerInfo{{ID: "wb", Capabilities: []string{gateway.String(gateway.CapCheckin)}}}
	jobs := []uiJob{
		{Provider: "wb", Name: "wb-growth-watch"},
		{Provider: "wb", Name: "wb-travel-watch"},
	}
	got := jobCoverageOf(providers, jobs, reg)
	if len(got) != 1 {
		t.Fatalf("应得 1 条，实得 %d", len(got))
	}
	if got[0].HasCheckin {
		t.Error("把 growth-watch / travel-watch 当成了签到任务 —— 用户会以为签到已注册")
	}
	if !got[0].MissingCheckin {
		t.Error("有签到能力但只有非签到任务，应报缺")
	}
}

// TestJobKindOf 后缀判据本身。
//
// ⚠ pool-health-check 是反例：以 -check 结尾但不是 -checkin，
//
//	写成 HasSuffix(n, "check") 就会把它当成签到。
func TestJobKindOf(t *testing.T) {
	cases := map[string]string{
		"monkeycode-checkin":     "checkin",
		"workbuddy-checkin":      "checkin",
		"qoder-refresh":          "refresh",
		"codearts-refresh":       "refresh",
		"workbuddy-growth-watch": "",
		"pool-health-check":      "",
		"":                       "",
	}
	for name, want := range cases {
		if got := jobKindOf(name); got != want {
			t.Errorf("jobKindOf(%q) = %q，应为 %q", name, got, want)
		}
	}
}

// TestJobCoverageEmptyProviders 空上游列表不崩。
func TestJobCoverageEmptyProviders(t *testing.T) {
	got := jobCoverageOf(nil, nil, nil)
	if len(got) != 0 {
		t.Errorf("空输入应得空结果，实得 %d 条", len(got))
	}
}
