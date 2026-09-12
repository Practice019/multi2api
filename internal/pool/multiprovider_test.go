package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 本文件是 Task 6（账号池多上游）的回归网。
//
// 它钉住四条最容易被后续改动破坏的性质：
//  1. 同步某一个上游**不得**剔除别的上游的账号（最容易踩的坑）
//  2. 选号按上游隔离：一个上游的请求绝拿不到另一个上游的账号
//  3. 未打标签的账号按默认上游解释 —— 向后兼容的根
//  4. 归属标签跨重启存活

// addProvider 往池里放一个带归属标签的账号。
func addProvider(p *Pool, provider, uid string) {
	p.SyncToDirFor(provider, []*auth.Auth{{UID: uid}})
}

// TestSyncToDirDoesNotPruneOtherProvider 是本任务最重要的回归测试。
//
// 场景：两个上游共用账号池，代码里对 workbuddy 调了一次"对齐目录"
// （那是改造前就有的调用，语义是"扫描结果的全集就是池中应有的全集"）。
// 若剔除逻辑不按上游限定，codearts 的账号会被整体删除，
// 而且删除会落盘 —— 重启也回不来。
func TestSyncToDirDoesNotPruneOtherProvider(t *testing.T) {
	p := New("")
	p.SetDefaultProvider("alpha")

	// 两个上游各有账号。
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}, {UID: "a2"}})
	p.SyncToDirFor("beta", []*auth.Auth{{UID: "b1"}})

	// 单独再同步一次 alpha，且这次它只剩一个账号。
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})

	if got := len(p.ListFor("alpha")); got != 1 {
		t.Errorf("alpha 应剩 1 个账号（a2 被剔除），得到 %d", got)
	}
	if got := len(p.ListFor("beta")); got != 1 {
		t.Errorf("同步 alpha **不得**影响 beta，beta 应仍有 1 个账号，得到 %d。"+
			"这正是 SyncToDir 剔除范围必须限定在本上游的理由", got)
	}
	if p.AuthByUID("b1") == nil {
		t.Error("b1 在 pool 中不存在：跨上游误删已发生")
	}
}

// TestSyncToDir_BackwardCompatibleSingleProvider 钉住 SyncToDir（无 provider 参数）
// 在单上游场景下仍然是"全量对齐" —— 既有 55 处调用点的语义不能变。
func TestSyncToDir_BackwardCompatibleSingleProvider(t *testing.T) {
	p := New("")
	p.SyncToDir([]*auth.Auth{{UID: "u1"}, {UID: "u2"}})
	if got := len(p.List()); got != 2 {
		t.Fatalf("首次同步后应有 2 个账号，得到 %d", got)
	}
	// 文件消失 → 剔除（改造前的行为）。
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if got := len(p.List()); got != 1 {
		t.Errorf("SyncToDir 仍须剔除消失的账号，应剩 1 个，得到 %d", got)
	}
	if p.AuthByUID("u1") != nil {
		t.Error("u1 应已被剔除")
	}
}

// TestUntaggedAccountsFollowDefaultProvider 钉住向后兼容的根：
// 改造前读进来的账号没有归属标签，它们必须落在默认上游下，
// 且默认上游的请求能选到它们。
func TestUntaggedAccountsFollowDefaultProvider(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	// 注意：**先**放账号再设默认上游，模拟"旧账号已存在，装配层随后注入默认上游"。
	p.SyncToDir([]*auth.Auth{{UID: "legacy1"}})
	p.SyncToDirFor("beta", []*auth.Auth{{UID: "b1"}})
	p.SetDefaultProvider("alpha")

	// 未打标签的账号按默认上游解释。
	if got := len(p.ListFor("alpha")); got != 1 {
		t.Errorf("未打标签账号应归入默认上游 alpha，得到 %d 个", got)
	}
	if st, ok := p.Status("legacy1"); !ok || st.Provider != "alpha" {
		t.Errorf("Status.Provider 应报告生效归属 alpha，得到 %q (ok=%v)", st.Provider, ok)
	}

	// 默认上游的选号能拿到它；beta 的选号拿不到它。
	for i := 0; i < 20; i++ {
		if a := p.PickFor("", "", nil); a == nil || a.UID != "legacy1" {
			t.Fatalf("默认上游选号应命中 legacy1，得到 %v", a)
		}
	}
	if a := p.PickFor("beta", "", nil); a == nil || a.UID != "b1" {
		t.Fatalf("beta 选号应只命中 b1，得到 %v", a)
	}
}

// TestPickForIsolatesProviders 选号按上游隔离：
// 即使另一个上游的账号额度更高、更闲置，也不得被选中。
func TestPickForIsolatesProviders(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetDefaultProvider("alpha")
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})
	p.SyncToDirFor("beta", []*auth.Auth{{UID: "b1"}, {UID: "b2"}})

	// 把 beta 的账号堆到极高额度：若选号不隔离，它们会把 alpha 的号压掉。
	p.SetQuota("b1", FromCredits(1_000_000))
	p.SetQuota("b2", FromCredits(1_000_000))
	p.SetQuota("a1", FromCredits(1))

	for i := 0; i < 50; i++ {
		a := p.PickFor("alpha", "", nil)
		if a == nil {
			t.Fatal("alpha 有可用账号时不应返回 nil")
		}
		if a.UID != "a1" {
			t.Fatalf("alpha 的请求拿到了别的上游的账号 %q —— 上游隔离失效", a.UID)
		}
	}
}

// TestPickForUnknownProviderReturnsNil 请求一个池里没有任何账号的上游时返回 nil，
// 而不是"回落到别的上游"（那会让上游收到不认识的凭证）。
func TestPickForUnknownProviderReturnsNil(t *testing.T) {
	p := New("")
	p.SetDefaultProvider("alpha")
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})

	if a := p.PickFor("nobody", "", nil); a != nil {
		t.Errorf("池中没有 nobody 的账号，应返回 nil，得到 %v", a)
	}
}

// TestProviderTagSurvivesReload 归属标签必须落盘 —— 否则重启后
// 别的上游的账号会被当成默认上游的账号，凭空多出一批必然失败的号。
func TestProviderTagSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p := New(fp)
	p.SetDefaultProvider("alpha")
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})
	p.SyncToDirFor("beta", []*auth.Auth{{UID: "b1"}})
	p.Flush()

	// 直接检查落盘内容带 provider 字段。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("解析 state.json: %v", err)
	}
	if sf.Accounts["b1"].Provider != "beta" {
		t.Errorf("b1 的 provider 应落盘为 beta，得到 %q", sf.Accounts["b1"].Provider)
	}

	// 重开池子：归属必须从状态文件恢复。
	p2 := New(fp)
	p2.SetDefaultProvider("alpha")
	if st, ok := p2.Status("b1"); !ok || st.Provider != "beta" {
		t.Errorf("重启后 b1 的归属应为 beta，得到 %q (ok=%v)", st.Provider, ok)
	}
}

// TestLegacyStateFileWithoutProvider 旧状态文件（无 provider 键）仍可读，
// 且归属按默认上游解释 —— 升级不需要迁移脚本。
func TestLegacyStateFileWithoutProvider(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 模拟改造前的状态文件：没有 provider 字段。
	legacy := `{"accounts":{"old1":{"credits":42}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.SetDefaultProvider("alpha")

	st, ok := p.Status("old1")
	if !ok {
		t.Fatal("旧状态文件的账号应被加载")
	}
	if st.Provider != "alpha" {
		t.Errorf("旧账号无 provider 字段时应归入默认上游 alpha，得到 %q", st.Provider)
	}
	if st.Credits != 42 {
		t.Errorf("旧账号额度应保留 42，得到 %d", st.Credits)
	}
}

// TestSecretOfRoundTrip 上游私有凭证经不透明通道存取，
// 且"账号在但没凭证"与"账号不存在"可区分。
func TestSecretOfRoundTrip(t *testing.T) {
	p := New("")
	p.SetDefaultProvider("alpha")
	// 用一个自定义类型模拟上游私有凭证（pool 不得认识它的结构）。
	type secret struct{ AK, SK string }
	p.AddFor("beta", &auth.Auth{UID: "b1"}, secret{AK: "ak1", SK: "sk1"})

	got, ok := p.SecretOf("b1")
	if !ok {
		t.Fatal("b1 应存在")
	}
	s, isSecret := got.(secret)
	if !isSecret {
		t.Fatalf("secret 应能断言回原类型，得到 %T", got)
	}
	if s.AK != "ak1" {
		t.Errorf("secret 内容丢失：%+v", s)
	}

	// 账号存在但没有私有凭证 → (nil, true)。
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})
	if v, ok := p.SecretOf("a1"); !ok || v != nil {
		t.Errorf("无凭证的账号应返回 (nil,true)，得到 (%v,%v)", v, ok)
	}
	// 账号不存在 → (nil, false)。
	if _, ok := p.SecretOf("nope"); ok {
		t.Error("不存在的账号应返回 ok=false")
	}
}

// TestProvidersEnumeration 管理台据此做分组展示。
func TestProvidersEnumeration(t *testing.T) {
	p := New("")
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})
	p.SyncToDirFor("beta", []*auth.Auth{{UID: "b1"}})
	got := p.Providers()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("Providers 应按字典序返回 [alpha beta]，得到 %v", got)
	}
}

// TestAvailableUIDsForScopedByProvider 会话粘性的可用号列表同样按上游隔离。
func TestAvailableUIDsForScopedByProvider(t *testing.T) {
	p := New("")
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})
	p.SyncToDirFor("beta", []*auth.Auth{{UID: "b1"}, {UID: "b2"}})

	if got := p.AvailableUIDsFor("alpha"); len(got) != 1 || got[0] != "a1" {
		t.Errorf("alpha 的可用号应为 [a1]，得到 %v", got)
	}
	if got := p.AvailableUIDsFor("beta"); len(got) != 2 {
		t.Errorf("beta 的可用号应有 2 个，得到 %v", got)
	}
	// 默认上游为空 → 归一成空默认，与 alpha/beta 都不相交。
	if got := p.AvailableUIDsFor(""); len(got) != 0 {
		t.Errorf("默认上游未设置时不应返回任何账号，得到 %v", got)
	}
}

// TestCooldownFallbackScopedByProvider 全冷却兜底同样不得跨上游：
// 拿别的上游的账号兜底除了必然失败没有任何意义。
func TestCooldownFallbackScopedByProvider(t *testing.T) {
	p := New("")
	p.SetDefaultProvider("alpha")
	p.SyncToDirFor("alpha", []*auth.Auth{{UID: "a1"}})
	p.SyncToDirFor("beta", []*auth.Auth{{UID: "b1"}})

	// alpha 的号软冷却（仍可兜底），beta 的号健康。
	p.Cooldown("a1", CoolSoft, time.Minute, "test")
	// beta 的号也软冷却，且到期更晚 —— 若兜底不隔离，会选到 b1。
	p.Cooldown("b1", CoolSoft, time.Hour, "test")

	a := p.PickFor("alpha", "", nil)
	if a == nil {
		t.Fatal("alpha 处于软冷却兜底范围内，应返回 a1")
	}
	if a.UID != "a1" {
		t.Errorf("alpha 的兜底拿到了 %q —— 兜底跨上游了", a.UID)
	}
}

// TestSyncToDirWithSecretsPrunesOnlyItsProvider 带 secret 的同步同样只剔本上游。
func TestSyncToDirWithSecretsPrunesOnlyItsProvider(t *testing.T) {
	p := New("")
	p.SetDefaultProvider("alpha")
	p.SyncToDir([]*auth.Auth{{UID: "a1"}})
	p.SyncToDirWithSecrets("beta", []*auth.Auth{{UID: "b1"}}, map[string]any{"b1": "SECRET"})

	// 再同步一次 beta，这次空了 → 只该剔掉 b1。
	p.SyncToDirWithSecrets("beta", nil, nil)

	if p.AuthByUID("b1") != nil {
		t.Error("b1 应被剔除（它是 beta 的号且不再出现）")
	}
	if p.AuthByUID("a1") == nil {
		t.Error("a1 是 alpha 的号，不该被 beta 的同步剔除")
	}
}
