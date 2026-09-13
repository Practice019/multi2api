// account_columns_test.go —— 账号池列集与"上游自报过期时刻"的契约测试。
//
// # 这组测试守的两件事
//
//  1. **列集是上游自报的**：实现了 AccountColumnsExt 的上游在 manifest 里带
//     `accounts_columns`；**没实现的不带**（回落默认 11 列 —— 用户要求
//     workbuddy「直接复用现在的标题」，所以它的后端必须零改动）。
//  2. **过期时刻的第二个来源**：核心投影里没有 `ExpiresAt` 的上游
//     （codearts 的形态），能通过 CredentialExpiryExt 问到。
//
// # 为什么每条都要有"反例桩"
//
// 只测"实现了的样本"是不够的：若把判据写成 `info.AccountColumns = 默认值`
//（永远不给），"实现了的样本"那条断言照样绿 —— 那样的守卫是装饰品。
// 所以每组都配一个**没有实现**该扩展点的桩，断言它的行为**不同**。
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// ---------------------------------------------------------------------------
// 桩：两个扩展点各自一个"实现了"与一个"没实现"
// ---------------------------------------------------------------------------

// columnsStub 实现了 AccountColumnsExt。
type columnsStub struct {
	stubProvider
	cols []string
}

func (s *columnsStub) AccountColumns() []string { return s.cols }

// expiryStub 实现了 CredentialExpiryExt。
//
// 行为刻意做成"从 secret 里读" —— 与真实上游（codearts 读 *Auth.ExpiresAt）同构：
// 它证明**核心不需要认识 secret 的类型**，只是把它原样交回给上游。
type expiryStub struct {
	stubProvider
	// at 是被问到时返回的秒数（0 表示"我没有这个信息"）。
	at int64
}

func (s *expiryStub) TokenExpiry(cred gateway.Credential) (int64, bool) {
	if s.at <= 0 {
		return 0, false
	}
	return s.at, true
}

// plainStub 两个扩展点**都不实现**（= workbuddy 的形态）。
type plainStub struct{ stubProvider }

// ---------------------------------------------------------------------------
// 列集：manifest 下发
// ---------------------------------------------------------------------------

// TestManifestCarriesAccountColumns 上游自报的列集必须出现在 manifest 里。
func TestManifestCarriesAccountColumns(t *testing.T) {
	reg := gateway.NewRegistry()
	want := []string{
		gateway.AccountColNickname,
		gateway.AccountColUID,
		gateway.AccountColTokenExpiry,
		gateway.AccountColWelfare,
		gateway.AccountColOps,
	}
	if err := reg.Register(&columnsStub{
		stubProvider: stubProvider{id: "selfreport", caps: gateway.CapChat},
		cols:         want,
	}); err != nil {
		t.Fatal(err)
	}

	m := manifestFor(t, New(Config{Registry: reg}))
	info := findProvider(t, m, "selfreport")
	if len(info.AccountColumns) != len(want) {
		t.Fatalf("accounts_columns 长度 = %d，want %d（实际 %v）",
			len(info.AccountColumns), len(want), info.AccountColumns)
	}
	for i := range want {
		if info.AccountColumns[i] != want[i] {
			t.Errorf("第 %d 列 = %q，want %q（顺序即表头顺序）",
				i, info.AccountColumns[i], want[i])
		}
	}
}

// TestManifestOmitsAccountColumnsWhenNotReported 没自报列的上游**不下发该字段**。
//
// 这是用户的要求：workbuddy「直接复用现在的标题」→ 它的后端零改动 →
// 它不实现扩展点 → 字段不出现 → 前端回落默认 11 列。
//
// ⚠ 用 `omitempty` 的后果：断言要看**原始 JSON**，不能只看解码后的切片 ——
// 解码时"字段不存在"与"空数组"都会得到 nil/空，分不清。
func TestManifestOmitsAccountColumnsWhenNotReported(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&plainStub{stubProvider{id: "wblike", caps: gateway.CapChat}}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/ui/manifest", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	New(Config{Registry: reg}).ServeHTTP(rec, req)

	var raw struct {
		Providers []map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Providers) != 1 {
		t.Fatalf("providers 数量 = %d，want 1", len(raw.Providers))
	}
	if _, present := raw.Providers[0]["accounts_columns"]; present {
		t.Error("未自报列的上游**不该**下发 accounts_columns —— " +
			"前端会把它读成「这个上游明确要求空列集」，而不是「回落默认列」")
	}
}

// TestManifestCarriesUnknownColumnIDToo 未知列 id **照常下发**（不阻断）。
//
// 列名拼错是上游的错，不该让整张账号表打不出来。前端会 console.warn + 跳过。
// 若在这里把它过滤掉，"拼错"与"本来就没这一列"就更分不清了。
func TestManifestCarriesUnknownColumnIDToo(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&columnsStub{
		stubProvider: stubProvider{id: "typo", caps: gateway.CapChat},
		cols:         []string{gateway.AccountColUID, "uid_typo_column"},
	}); err != nil {
		t.Fatal(err)
	}
	info := findProvider(t, manifestFor(t, New(Config{Registry: reg})), "typo")
	if len(info.AccountColumns) != 2 || info.AccountColumns[1] != "uid_typo_column" {
		t.Fatalf("未知列 id 应当照常下发（由前端跳过并 warn），实际 %v", info.AccountColumns)
	}
}

// findProvider 在 manifest 里按 id 找上游。
func findProvider(t *testing.T, m uiManifest, id string) providerInfo {
	t.Helper()
	for _, p := range m.Providers {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("manifest 里找不到上游 %q（实际 %v）", id, m.Providers)
	return providerInfo{}
}

// ---------------------------------------------------------------------------
// 过期时刻：第二个来源
// ---------------------------------------------------------------------------

// accountsFor 打一次 /admin/accounts 并解码。
func accountsFor(t *testing.T, h *Handler) []AccountView {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200（body=%s）", rec.Code, rec.Body.String())
	}
	var out struct {
		Accounts []AccountView `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	return out.Accounts
}

// TestAccountViewsFillsTokenExpiryFromProvider 是本次修复的核心断言。
//
// 场景 = codearts 的真实形态：池子里账号的通用投影（`*auth.Auth`）
// **没有** ExpiresAt（所以 `has_token=false`、上面那段填不出 sec），
// 真正的过期时刻在**不透明 secret** 里，只有上游自己解释得了。
//
// 期望：走 CredentialExpiryExt 把 `token_expire_sec` 填上。
//
// ⚠ 这条在改造前必红 —— 那时没有任何代码去问上游，codearts 那一列永远是「—」。
func TestAccountViewsFillsTokenExpiryFromProvider(t *testing.T) {
	at := time.Now().Add(2 * time.Hour).Unix()
	reg := gateway.NewRegistry()
	if err := reg.Register(&expiryStub{
		stubProvider: stubProvider{id: "carts", caps: gateway.CapChat},
		at:           at,
	}); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	// 投影里**没有** ExpiresAt —— 这正是 codearts 的形态。
	p.AddFor("carts", &auth.Auth{UID: "ca-1"}, "不透明的 secret")

	h := New(Config{Pool: p, Registry: reg, DefaultProvider: "carts"})
	accts := accountsFor(t, h)
	if len(accts) != 1 {
		t.Fatalf("账号数 = %d，want 1", len(accts))
	}
	v := accts[0]
	if v.HasToken {
		t.Fatal("前置条件不成立：这个桩的通用投影不该有 token")
	}
	if v.TokenExpireSec == nil {
		t.Fatal("★ codearts 形态的账号没有 token_expire_sec —— " +
			"界面上那一列永远是「—」，而它其实有 STS 有效期")
	}
	// 允许几秒误差（中间隔了一次 time.Now()）
	got := *v.TokenExpireSec
	if got < 7100 || got > 7200 {
		t.Errorf("token_expire_sec = %d，want 约 7200（2 小时）", got)
	}
	if v.TokenExpireAt == nil || *v.TokenExpireAt != at {
		t.Errorf("token_expire_at = %v，want %d", v.TokenExpireAt, at)
	}
}

// TestAccountViewsOmitsTokenExpiryWhenProviderSaysUnknown 上游说"不知道"时字段不出现。
//
// 这是防"确定性假象"：`—` 与"还剩 0 秒"必须长得不一样。
// 若这里把 0 填进去，前端会渲染成"已过期"，比不显示更糟。
func TestAccountViewsOmitsTokenExpiryWhenProviderSaysUnknown(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&expiryStub{
		stubProvider: stubProvider{id: "carts", caps: gateway.CapChat},
		at:           0, // 上游说：我没有可读的过期时间
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("carts", &auth.Auth{UID: "ca-1"}, "不透明的 secret")

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "carts"}))[0]
	if v.TokenExpireSec != nil || v.TokenExpireAt != nil {
		t.Errorf("上游说未知时不该填过期字段（got sec=%v at=%v）—— "+
			"前端会把 0 渲染成「已过期」，比显示「—」更糟", v.TokenExpireSec, v.TokenExpireAt)
	}
}

// TestAccountViewsWorkbuddyPathUnchanged workbuddy 的路径**逐字段不变**。
//
// 它的过期时刻一直在通用投影里（`*auth.Auth.ExpiresAt`）。新增的第二来源
// **只在上面没拿到时才问** —— 若顺序写反（先问上游），workbuddy 的
// `token_expire_sec` 会被一个"不知道"的空值覆盖，那是可见回归。
func TestAccountViewsWorkbuddyPathUnchanged(t *testing.T) {
	at := time.Now().Add(60 * 24 * time.Hour).Unix() // 实测 workbuddy ≈ 60 天

	reg := gateway.NewRegistry()
	// 注意：这个桩**不实现** CredentialExpiryExt（= workbuddy 的形态），
	// 所以它只能走通用投影那条路。
	if err := reg.Register(&plainStub{stubProvider{id: "wb", caps: gateway.CapChat}}); err != nil {
		t.Fatal(err)
	}

	p := pool.New("")
	p.AddFor("wb", &auth.Auth{UID: "wb-1", AccessToken: "tok", ExpiresAt: at}, nil)

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "wb"}))[0]
	if !v.HasToken {
		t.Error("workbuddy 的 has_token 应当为 true（AccessToken 非空）")
	}
	if v.TokenExpireSec == nil || *v.TokenExpireSec < 60*24*3600-60 {
		t.Fatalf("workbuddy 的 token_expire_sec 被改坏了：%v（want 约 60 天）", v.TokenExpireSec)
	}
}

// TestCredentialExpiryOfRefusesUnknownProvider 未注册的上游 → 未知，不 panic。
//
// 三种"拿不到"都必须安全返回：账号不存在、上游未注册、上游没实现扩展点。
// 任何一条 panic 都会让**整张账号表**打不出来（handler 崩在渲染路径上）。
func TestCredentialExpiryOfRefusesUnknownProvider(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&expiryStub{
		stubProvider: stubProvider{id: "carts", caps: gateway.CapChat},
		at:           time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("carts", &auth.Auth{UID: "ca-1"}, "s")
	h := New(Config{Pool: p, Registry: reg, DefaultProvider: "carts"})

	cases := []struct {
		name     string
		uid      string
		provider string
	}{
		{"账号不存在", "没有这个 uid", "carts"},
		{"上游未注册", "ca-1", "ghostnet"},
		{"provider 为空", "ca-1", ""},
	}
	for _, c := range cases {
		if _, ok := h.credentialExpiryOf(c.uid, c.provider); ok {
			t.Errorf("%s：应当返回未知（false）", c.name)
		}
	}
}

// 保留对 context 的引用（stubProvider.Chat 的签名用到它），
// 并防止未来有人删掉这个 import 时困惑。
var _ = context.Background

// ---------------------------------------------------------------------------
// 今日福利：本地领取记录 → today_welfare
// ---------------------------------------------------------------------------

// newWelfareLog 造一份带一条 welfare 记录的历史。
func newWelfareLog(t *testing.T, uid, status string) *checkinlog.Log {
	t.Helper()
	l := checkinlog.New(t.TempDir()+"/log.json", 30)
	l.Append(checkinlog.Record{
		At:     time.Now(),
		UID:    uid,
		Kind:   checkinlog.KindWelfare,
		Status: status,
	})
	return l
}

// TestAccountViewsReportsTodayWelfare 领过 → 回 today_welfare。
//
// 这是用户要的「福利是否领取」的数据落点：
// 上游只回 `claimable`，分不清"今日已领"与"资格不符"；
// 而**我们自己**的领取动作是确定的，所以答案来自本地历史。
func TestAccountViewsReportsTodayWelfare(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&plainStub{stubProvider{id: "carts", caps: gateway.CapWelfare}}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("carts", &auth.Auth{UID: "ca-1"}, "s")

	h := New(Config{
		Pool:            p,
		Registry:        reg,
		DefaultProvider: "carts",
		Log:             newWelfareLog(t, "ca-1", checkinlog.StatusOK),
	})

	v := accountsFor(t, h)[0]
	if v.TodayWelfare != checkinlog.StatusOK {
		t.Fatalf("today_welfare = %q，want %q", v.TodayWelfare, checkinlog.StatusOK)
	}
	if v.TodayWelfareAt == 0 {
		t.Error("today_welfare_at 应当带上时间戳（界面要显示这是什么时候领的）")
	}
}

// TestAccountViewsOmitsTodayWelfareWhenNoRecord **没记录 ≠ 未领取**。
//
// ⚠ 这是本次最容易犯的错：把"我们不知道"渲染成"没领"。
// 字段缺失时前端显示 `—`；若这里填一个 "no"/"未领取"，
// 界面就在**断言**一件我们并不知道的事（上游那侧的状态我们看不到）。
func TestAccountViewsOmitsTodayWelfareWhenNoRecord(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&plainStub{stubProvider{id: "carts", caps: gateway.CapWelfare}}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("carts", &auth.Auth{UID: "ca-1"}, "s")

	// 历史是空的（今天没点过）
	h := New(Config{
		Pool:            p,
		Registry:        reg,
		DefaultProvider: "carts",
		Log:             checkinlog.New(t.TempDir()+"/log.json", 30),
	})

	if v := accountsFor(t, h)[0]; v.TodayWelfare != "" {
		t.Errorf("今天没领过时不该填 today_welfare（got %q）—— "+
			"前端会把它渲染成一个确定的结论，而事实是「不知道」", v.TodayWelfare)
	}
}

// TestTodayWelfareDoesNotBleedIntoTodayCheckin 两个 kind 不能互相串。
//
// codearts **没有**签到端点。若把 welfare 记录也算进 today_checkin，
// 账号池会对 codearts 渲染出「已签到」—— 一个它根本没有的动作。
func TestTodayWelfareDoesNotBleedIntoTodayCheckin(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&plainStub{stubProvider{id: "carts", caps: gateway.CapWelfare}}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("carts", &auth.Auth{UID: "ca-1"}, "s")

	h := New(Config{
		Pool:            p,
		Registry:        reg,
		DefaultProvider: "carts",
		Log:             newWelfareLog(t, "ca-1", checkinlog.StatusOK),
	})

	v := accountsFor(t, h)[0]
	if v.TodayCheckin != "" {
		t.Errorf("today_checkin = %q —— welfare 记录串进了签到列，"+
			"codearts 会被渲染成「已签到」而它没有签到端点", v.TodayCheckin)
	}
	if v.TodayWelfare != checkinlog.StatusOK {
		t.Errorf("today_welfare = %q，want %q", v.TodayWelfare, checkinlog.StatusOK)
	}
}
