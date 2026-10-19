// apikey_role_split_test.go T8 验收：API key 的**归属过滤**。
//
// # 这组测试要挡的失效形态
//
// "一个成员偶尔看见别人的 key" —— 它不会崩溃、不会报错、日志干净，只是**某一行
// 不该出现**。这类问题只能靠判据测试钉住，所以这里逐条覆盖：
//
//	① 成员列表 = 只有自己名下的（管理员 = 全部）
//	② 成员建的 key 归属自己；用 admin_token 的脚本建的 = 管理员所有
//	③ 成员改/删/重置别人的 key ⇒ 403（不是 404 —— 不给"这个 id 存在吗"的探测口）
//	④ 老数据（owner 为空）**视为管理员所有** ⇒ 成员看不见也管不了
//	⑤ 身份缺失（空 owner + 非管理员）⇒ 空列表 / 不能管（最严那一档）
//	⑥ 轮换管理凭据的端点：成员连调都调不到（第二层硬判）
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/account"
	"workbuddy2api/internal/apikey"
)

// roleFixture 一个"两账号 + 一套 apikey 库"的管理面夹具。
type roleFixture struct {
	h         *Handler
	keys      *apikey.Store
	admin     *account.User
	member    *account.User
	other     *account.User     // 第二个成员（验证"成员之间也互相看不见"）
	adminKey  string            // 管理员建的那把
	memberKey string            // 成员 A 建的
	otherKey  string            // 成员 B 建的
	legacyKey string            // 老数据：owner 为空
	sids      map[string]string // 账号 ID → 会话 ID（每账号只签一次，避开签发限流）
}

func newRoleFixture(t *testing.T) *roleFixture {
	t.Helper()
	dir := t.TempDir()

	accounts, err := account.New(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sessions, err := account.NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	f := &roleFixture{}
	if f.admin, err = accounts.Create("boss", "boss-password-001", account.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if f.member, err = accounts.Create("staff", "staff-password-002", account.RoleMember); err != nil {
		t.Fatal(err)
	}
	if f.other, err = accounts.Create("staff2", "staff2-password-003", account.RoleMember); err != nil {
		t.Fatal(err)
	}

	if f.keys, err = apikey.New(filepath.Join(dir, "apikeys.json")); err != nil {
		t.Fatalf("key 库: %v", err)
	}
	// 三把带归属 + 一把"老数据"（owner 为空）
	mk := func(owner, name string) string {
		k, err := f.keys.CreateOwned(owner, name, 0, 0, nil)
		if err != nil {
			t.Fatalf("建 key %s: %v", name, err)
		}
		return k.ID
	}
	f.adminKey = mk("", "admin-key") // 管理员：owner 为空（存量语义）
	f.memberKey = mk(f.member.ID, "staff-key")
	f.otherKey = mk(f.other.ID, "staff2-key")
	f.legacyKey = mk("", "legacy-key")

	f.h = New(Config{
		Accounts:   accounts,
		Sessions:   sessions,
		AdminToken: testAdminToken,
		APIKeys:    f.keys,
		APIKey:     "admin-api-key-shape-0001",
		Pool:       nil, // 本组测试只打 apikeys 端点
	})
	return f
}

// call 以某个账号的身份打一次管理端点（走真实门禁）。
//
// # 为什么缓存会话（每个账号只签一次）
//
// 会话签发有上限（`account.SessionStore` 的"1 小时内签发次数过多" ⇒ 429）——
// 那是 T10 的防线之一，不是测试的障碍。夹具一次签一条、反复复用，既避开限流，
// 也更接近真实浏览器的行为（同一个标签页本来就只有一条会话）。
func (f *roleFixture) call(t *testing.T, u *account.User, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: account.CookieName, Value: f.sidOf(t, u)})
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// sidOf 取（或首次签发）该账号的会话 ID。
func (f *roleFixture) sidOf(t *testing.T, u *account.User) string {
	t.Helper()
	if f.sids == nil {
		f.sids = map[string]string{}
	}
	if sid, ok := f.sids[u.ID]; ok {
		return sid
	}
	sess, err := f.h.cfg.Sessions.Issue(u, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("签发会话: %v", err)
	}
	f.sids[u.ID] = sess.SID
	return sess.SID
}

// listKeys 调 GET /admin/apikeys 并把回执里的 key 列表解出来。
func (f *roleFixture) listKeys(t *testing.T, u *account.User) ([]apikey.View, map[string]any) {
	t.Helper()
	rec := f.call(t, u, http.MethodGet, "/admin/apikeys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/apikeys 作为 %s = %d: %s", u.Username, rec.Code, rec.Body.String())
	}
	var out struct {
		Keys   []apikey.View `json:"keys"`
		Member bool          `json:"member"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("回执不是 JSON: %v / %s", err, rec.Body.String())
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	// member 标志要一致（前端据此改标题）
	if want := u.Role != account.RoleAdmin; out.Member != want {
		t.Errorf("%s 的 member 标志 = %v，want %v", u.Username, out.Member, want)
	}
	return out.Keys, raw
}

// idsOf 把列表里的 key id 收成集合（断言"包含/不包含"更好读）。
func idsOf(views []apikey.View) map[string]bool {
	out := map[string]bool{}
	for _, v := range views {
		out[v.ID] = true
	}
	return out
}

// ① ★ 列表按归属过滤：管理员看全部；成员只看自己那把。
func TestAPIKeysListIsOwnerFiltered(t *testing.T) {
	f := newRoleFixture(t)

	all, rawAdmin := f.listKeys(t, f.admin)
	if len(all) != 4 {
		t.Errorf("管理员应当看到全部 4 把 key，实际 %d 把", len(all))
	}
	// 管理员的回执里才有两把管理钥匙的掩码
	if rawAdmin["admin_key"] == "" || rawAdmin["admin_token"] == "" {
		t.Error("管理员的回执里应当有 admin_key / admin_token 两个掩码（前端要显示在用哪把）")
	}

	mine, rawMember := f.listKeys(t, f.member)
	if len(mine) != 1 || !idsOf(mine)[f.memberKey] {
		t.Errorf("★ 成员应当只看到自己名下那 1 把 key，实际 %d 把：%v", len(mine), idsOf(mine))
	}
	if idsOf(mine)[f.adminKey] || idsOf(mine)[f.legacyKey] {
		t.Error("★★ 成员看到了管理员的 key（含 owner 为空的老 key）—— 这是本次改造最危险的形态")
	}
	if rawMember["admin_key"] != "" || rawMember["admin_token"] != "" {
		t.Error("★ 成员连管理钥匙的**掩码**都不该拿到（它提示「这东西存在、长这样」）")
	}

	// 另一个成员也只看自己的（成员之间同样隔离）
	other, _ := f.listKeys(t, f.other)
	if len(other) != 1 || !idsOf(other)[f.otherKey] {
		t.Errorf("另一个成员应当只看到自己的 key，实际 %v", idsOf(other))
	}
}

// ② 新建的 key 归属当前身份。
func TestAPIKeysCreateStampsOwner(t *testing.T) {
	f := newRoleFixture(t)

	rec := f.call(t, f.member, http.MethodPost, "/admin/apikeys",
		`{"name":"member-new","limit":0,"rpm":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("成员建 key = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("回执不是 JSON: %v", err)
	}
	owner, ok := f.keys.OwnerOf(created.ID)
	if !ok {
		t.Fatal("新建的 key 在库里找不到")
	}
	if owner != f.member.ID {
		t.Errorf("★ 成员建的 key 归属 = %q，want %q（成员建的必须归他，否则他建完就看不见自己那把）",
			owner, f.member.ID)
	}

	// 成员建完之后，他自己的列表里有 2 把；管理员看得到全部
	mine, _ := f.listKeys(t, f.member)
	if len(mine) != 2 {
		t.Errorf("成员建完应当看到 2 把，实际 %d", len(mine))
	}
	// 用 admin_token 的脚本（无账号身份）建的 ⇒ owner 为空 = 管理员所有
	req := httptest.NewRequest(http.MethodPost, "/admin/apikeys", strings.NewReader(`{"name":"script-made"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAdminToken)
	rec2 := httptest.NewRecorder()
	f.h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("admin_token 建 key = %d: %s", rec2.Code, rec2.Body.String())
	}
	var c2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec2.Body.Bytes(), &c2)
	if owner, _ := f.keys.OwnerOf(c2.ID); owner != "" {
		t.Errorf("admin_token 建的 key 归属应为空（= 管理员所有），实际 %q", owner)
	}
}

// ③ ★ 成员动别人的 key ⇒ 403（且不是 404 —— 不提供 id 存在性的探测口）。
func TestAPIKeysMutationsRejectForeignKeys(t *testing.T) {
	f := newRoleFixture(t)

	cases := []struct{ name, path, body string }{
		{"toggle", "/admin/apikeys/toggle", `{"id":"%s","enabled":false}`},
		{"delete", "/admin/apikeys/delete", `{"id":"%s"}`},
		{"reset", "/admin/apikeys/reset", `{"id":"%s"}`},
		{"update", "/admin/apikeys/update", `{"id":"%s","limit":999}`},
	}
	// 目标：管理员的 key、老数据 key、**另一个成员**的 key
	for _, target := range []string{"adminKey", "legacyKey", "otherKey"} {
		var id string
		switch target {
		case "adminKey":
			id = f.adminKey
		case "legacyKey":
			id = f.legacyKey
		case "otherKey":
			id = f.otherKey
		}
		for _, c := range cases {
			body := strings.Replace(c.body, "%s", id, 1)
			rec := f.call(t, f.member, http.MethodPost, c.path, body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("★ 成员 %s 别人的 key（%s）= %d，want 403: %s",
					c.name, target, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "forbidden_role") {
				t.Errorf("%s 的 403 应当带 code=forbidden_role，实际 %s", c.name, rec.Body.String())
			}
		}
	}

	// 自己的 key：四个操作都应当成功（不然就是"过滤过头"，成员没法管自己的东西）。
	//
	// ⚠ 每个操作**各用一把新 key**：delete 会把 key 删掉，而删掉之后 reset/update
	// 拿到的是"key 不存在" ⇒ `CanManage` 判 false ⇒ 403。那是**正确**行为
	// （见 requireKeyAccess 的注释：不存在与不是你的都回 403，不给"这个 id 存在吗"
	// 的探测口），但会让"复用同一把"的写法看起来像过滤过头。
	for _, c := range cases {
		own, err := f.keys.CreateOwned(f.member.ID, "own-"+c.name, 0, 0, nil)
		if err != nil {
			t.Fatalf("建测试 key: %v", err)
		}
		body := strings.Replace(c.body, "%s", own.ID, 1)
		rec := f.call(t, f.member, http.MethodPost, c.path, body)
		if rec.Code != http.StatusOK {
			t.Errorf("成员 %s **自己**的 key = %d，want 200: %s", c.name, rec.Code, rec.Body.String())
		}
	}
	// 管理员动成员的 key 仍然可以（管理员管全部）
	rec := f.call(t, f.admin, http.MethodPost, "/admin/apikeys/delete",
		`{"id":"`+f.otherKey+`"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("管理员删成员的 key = %d，want 200: %s", rec.Code, rec.Body.String())
	}
}

// ④⑤ 归属判据本身的两条边界（存储层，不经过 HTTP）。
//
// 为什么单独测：`ListFor` / `CanManage` 是**判据的唯一实现**，HTTP 层只是调用它。
// 这两条边界（空 owner 与管理员同义、身份缺失走最严档）在这里钉住之后，
// 将来谁把它们改宽都会立刻红。
func TestOwnerPredicateBoundaries(t *testing.T) {
	f := newRoleFixture(t)

	// 空 owner + 非管理员 ⇒ 空列表（**不是**"列出所有 owner 为空的"）
	if got := f.keys.ListFor("", false); len(got) != 0 {
		t.Errorf("★ 身份缺失（空 owner + 非管理员）应当得到空列表，实际 %d 条 ——\n"+
			"这里一旦放宽，未登录/会话失效/身份字段没接上都会把管理员的 key 端出去。", len(got))
	}
	// 空 owner + 管理员 ⇒ 全部
	if got := f.keys.ListFor("", true); len(got) != 4 {
		t.Errorf("管理员应当看到全部 4 条，实际 %d", len(got))
	}
	// 成员不得认领 owner 为空的老 key
	if f.keys.CanManage(f.member.ID, false, f.legacyKey) {
		t.Error("★ 成员不得认领 owner 为空的老 key（空 = 管理员所有的标记）")
	}
	if f.keys.CanManage("", false, f.memberKey) {
		t.Error("★ 身份缺失时不得管任何 key")
	}
	if !f.keys.CanManage("", true, f.memberKey) {
		t.Error("管理员应当能管成员的 key")
	}
}

// ⑥ 轮换管理凭据：成员连调都调不到（门禁挡住），管理员能调到"写入前的业务校验"分支。
func TestRotateEndpointsAreAdminOnly(t *testing.T) {
	f := newRoleFixture(t)

	for _, path := range []string{
		"/admin/apikeys/rotate-admin",
		"/admin/apikeys/rotate-api-key",
	} {
		rec := f.call(t, f.member, http.MethodPost, path, `{}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("★★ 成员调 %s = %d，want 403 ——\n"+
				"它的回执里带**完整的新凭据**，成员调通一次就等于拿到管理员那条路。",
				path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "admin_token") || strings.Contains(rec.Body.String(), "api_key\"") {
			t.Errorf("%s 的拒绝回执里不得出现任何凭据相关字段名/值，实际 %s", path, rec.Body.String())
		}
	}
}
