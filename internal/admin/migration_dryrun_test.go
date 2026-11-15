// migration_dryrun_test.go U-02 管理端点的判据。
//
// # 三件必须成立的事
//
//	① 只有管理员能看（它列举的是全部令牌的权限面）
//	② 响应里**一个字节的凭据都没有** —— apikey.View.ID 就是完整密钥
//	③ 有阻塞时 blocked=true（前端据此禁用"开始迁移"按钮）
//
// 第 ② 条不是"小心一点"：报告要贴进工单、发给同事看。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// migrationFixture 带渠道 + 分组 + apikey 的管理端。
type migrationFixture struct {
	h      *Handler
	groups *groupstore.Store
	keys   *apikey.Store
	chans  *channelstore.Store
}

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	// channelstore.New 要求 store.DB 已 Open + Migrate（B1 之后渠存在 SQL 里）。
	// store.DB 是包级单例 ⇒ 每个用例自己开一个临时库并 Cleanup。
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	ks, err := apikey.New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true, Groups: gs, APIKeys: ks, Channels: cs})
	return &migrationFixture{h: h, groups: gs, keys: ks, chans: cs}
}

// dryRunCall 发一次 dry-run 请求（admin / 非 admin 两种）。
func (f *migrationFixture) dryRun(t *testing.T, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/migration/dry-run", nil)
	if admin {
		req = withTestAuth(req)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// ★ ① 只有管理员能看。
//
// ⚠ 断言要写成"不是 200"，而不是钉一个具体码：
//
//	401  没有凭据 —— 鉴权中间件在本 handler 之前就拒了（实测走这条）
//	403  有凭据但角色不对 —— 本 handler 里的 IsAdmin() 拒
//
// 两者都是"不许看"。钉死 403 会让这条判据依赖中间件的实现顺序，
// 而那个顺序以后完全可能变（比如把鉴权下放到 handler 里）。
func TestMigrationDryRunRequiresAdmin(t *testing.T) {
	f := newMigrationFixture(t)
	rec := f.dryRun(t, false)
	if rec.Code == http.StatusOK {
		t.Fatalf("★ 非管理员拿到了 200 —— 它列举的是全部令牌的权限面\nbody=%s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("应是 401 或 403，实际 %d", rec.Code)
	}
}

// ★ ② 响应里一个字节的凭据都没有。
//
// `apikey.View.ID` 与 `apikey.ApiKey.ID` **都是完整密钥**。本端点把它们当查找键用，
// 所以这条判据是"结构性"的：只要有人顺手往 DryRunItem 里加个 ID 字段，它就红。
func TestMigrationDryRunNeverLeaksCredential(t *testing.T) {
	f := newMigrationFixture(t)
	g, err := f.groups.Create("route", "", []string{"ch-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.chans.Upsert(&channelstore.Channel{
		ID: "ch-a", Name: "a", Kind: "standard", Status: channelstore.StatusEnabled,
		Models: []string{"real"}, ModelMap: map[string]string{"public": "real"},
		Keys: []channelstore.ChannelKey{{Key: "k", Status: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	k, err := f.keys.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{g.ID}); err != nil {
		t.Fatal(err)
	}

	rec := f.dryRun(t, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, k.ID) {
		t.Fatalf("★ 响应里出现了完整凭据 %q —— 这份报告是要贴进工单的\nbody=%s", k.ID, body)
	}
	// 也不该出现掩码以外的形态（掩码允许，明文不允许）。
	if strings.Contains(body, "sk-") && !strings.Contains(body, "*") {
		t.Fatalf("响应里出现了 sk- 前缀但没有掩码形态：%s", body)
	}
	if got := rec.Header().Get("Cache-Control"); got != credentialCacheControl {
		t.Fatalf("必须带 no-store，实际 %q", got)
	}
}

// ★ ③ 变宽的令牌必须让 blocked=true。
func TestMigrationDryRunBlocksOnWidening(t *testing.T) {
	f := newMigrationFixture(t)
	if _, err := f.groups.Create("route", "", []string{"ch-a"}); err != nil {
		t.Fatal(err)
	}
	if err := f.chans.Upsert(&channelstore.Channel{
		ID: "ch-a", Name: "a", Kind: "standard", Status: channelstore.StatusEnabled,
		Models: []string{"real"}, ModelMap: map[string]string{"public": "real"},
		Keys: []channelstore.ChannelKey{{Key: "k", Status: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	// 造一个新区令牌（policy_version=2）+ 空组作用域 + 未启用白名单。
	//
	// 旧解释：分组为空 + 未启用白名单 ⇒ **不限**；新解释：组候选 = public。
	// 这是**变窄**，不阻塞。为了造出变宽，要把旧解释弄成 restricted_empty：
	// 启用白名单 + 空列表 ⇒ 旧"一个都不给"，新"未启用"⇒ public ⇒ 变宽。
	k, err := f.keys.Create("app", 0, 0, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.UpdateWithGroups(k.ID, nil, nil, []string{}, []string{}); err != nil {
		t.Fatal(err)
	}

	rec := f.dryRun(t, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Report struct {
			Total     int  `json:"total"`
			Blocked   int  `json:"blocked"`
			Ready     bool `json:"ready"`
			CatalogOK bool `json:"catalog_known"`
			Items     []struct {
				Applicable bool   `json:"applicable"`
				Blocked    string `json:"blocked"`
			} `json:"items"`
		} `json:"report"`
		Blocked bool `json:"blocked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v\n%s", err, rec.Body.String())
	}
	// 至少要有渠道 → 目录非空 ⇒ CatalogKnown。
	if !out.Report.CatalogOK {
		t.Fatalf("有渠道时目录应可信：%s", rec.Body.String())
	}
	// blocked 与 report.ready 必须反向一致（前端只读一个字段也会对）。
	if out.Blocked == out.Report.Ready {
		t.Fatalf("blocked 与 ready 必须反向：blocked=%v ready=%v", out.Blocked, out.Report.Ready)
	}
}

// TestMigrationDryRunBlocksWithoutChannels 没有渠道 ⇒ 目录不可信 ⇒ 整份报告阻塞。
func TestMigrationDryRunBlocksWithoutChannels(t *testing.T) {
	f := newMigrationFixture(t)
	if _, err := f.keys.Create("app", 0, 0, nil); err != nil {
		t.Fatal(err)
	}

	rec := f.dryRun(t, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200（阻塞也是一种结论，不是错误），实际 %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["blocked"] != true {
		t.Fatalf("★ 没有渠道 ⇒ 目录为空 ⇒ 不能用空快照冒充全集，必须 blocked；实际 %v\n%s",
			out["blocked"], rec.Body.String())
	}
}
