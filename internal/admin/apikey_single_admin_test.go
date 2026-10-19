// apikey_single_admin_test.go 单账号口径下的 API key 端点守卫。
//
// # ⚠ 这个文件是 apikey_role_split_test.go 的**替代**，说明为什么换掉
//
// 原文件（T8 验收）测的是"API key 的**归属过滤**"：成员列表 = 只有自己名下的、
// 成员动别人的 key ⇒ 403、成员之间互相看不见、成员连调都调不到轮换端点。
//
// 2026-10-19 用户新口径：「只要一个管理员就行了，只要一个账号。用户其实可以
// 不用。」⇒ **成员这个身份不存在了**，那六条判据里有一半测的面已经不存在：
//
//	① 成员列表只看见自己的          ⇒ 没有成员，测不出来（原意由下面的 ① 接住）
//	② 成员建的 key 归属自己          ⇒ 同上（成员建不出 key 了）
//	③ 成员动别人的 key ⇒ 403         ⇒ 同上（**没有"别人的 key"这种身份视角**）
//	⑥ 成员调不到轮换端点             ⇒ 同上（成员连门都进不了，是 401 不是 403）
//	④⑤ owner 判据边界（存储层）      ⇒ **保留**：这是防御纵深，不是成员功能
//
// "删掉一条测试"必须说清它测的面为什么消失 —— 上面就是。四条被删的判据都由
// **更强**的东西接住：成员身份在 Identify/Lookup 那一层就结束了（见
// internal/account 与 gate_test.go 的 TestGate_SingleAdminSession），
// 根本走不到 apikeys 端点的归属过滤逻辑。
//
// # 为什么 owner 相关的判据（④⑤）反而要留
//
// `apikey` 包里的 owner 字段与 ListFor/CanManage 是**按"最小权限"写的**：
// 空 owner + 非管理员 ⇒ 空列表；身份缺失 ⇒ 管不了任何 key。它们是"装配层
// 忘了接线"时的最后一道闸门（fail-closed）。这道闸门与"成员是否存在"无关 ——
// 今天的单账号部署里它表现为"管理员看到全部"，将来若再加第二种身份，
// 它仍然是那第二种身份的边界。删掉它等于把这条防线一起删掉。
//
// 同时这里新增了一条因本次改造而来的判据：
//
//	⑦ 回执里**不再有** `member` 字段 —— 它原先只服务「我的 API Key」标题，
//	  恒等于 `!IsAdmin()`；单账号口径下它恒为 false，留着就是一个
//	  "这里好像还分角色"的假线索（前端读 undefined 会走管理员渲染路径，
//	  所以删字段是安全的，见 apikeys.go 的注释）。
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

// adminKeyFixture 一个"单管理员 + 一套 apikey 库"的管理面夹具。
type adminKeyFixture struct {
	h         *Handler
	keys      *apikey.Store
	admin     *account.User
	adminKey  string // 管理员建的（owner 为空 = 存量语义）
	legacyKey string // 老数据：owner 为空
	otherKey  string // 归属一个**历史成员 ID** 的 key（改造前成员建的）
	sids      map[string]string
}

// legacyMemberID 一个只存在于 apikey 库归属字段里的历史成员 ID。
//
// 为什么不用真账号：单账号口径下账号库里不可能有成员了（那正是目标态 1），
// 但 **key 库里的存量归属字段**是改造前写下的、不会自己消失。这类 key 现在
// 是"只有管理员看得见/管得了"的东西 —— 下面 ④⑤ 钉的正是这一点。
const legacyMemberID = "u-legacy-staff"

func newAdminKeyFixture(t *testing.T) *adminKeyFixture {
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
	f := &adminKeyFixture{}
	// 唯一那个管理员：走唯一建号路径（空库 ⇒ CreateBootstrap）
	if f.admin, err = accounts.CreateBootstrap("boss", "boss-password-001"); err != nil {
		t.Fatal(err)
	}

	if f.keys, err = apikey.New(filepath.Join(dir, "apikeys.json")); err != nil {
		t.Fatalf("key 库: %v", err)
	}
	mk := func(owner, name string) string {
		k, err := f.keys.CreateOwned(owner, name, 0, 0, nil)
		if err != nil {
			t.Fatalf("建 key %s: %v", name, err)
		}
		return k.ID
	}
	f.adminKey = mk("", "admin-key")                    // 管理员：owner 为空（存量语义）
	f.legacyKey = mk("", "legacy-key")                  // 老数据：owner 为空
	f.otherKey = mk(legacyMemberID, "legacy-staff-key") // 改造前成员建的

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

// call 以管理员的身份打一次管理端点（走真实门禁）。
func (f *adminKeyFixture) call(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: account.CookieName, Value: f.sid(t)})
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// sid 取（或首次签发）管理员的会话 ID。
//
// 为什么缓存：会话签发有上限（`account.SessionStore` 的"1 小时内签发次数过多"
// ⇒ 429）—— 那是 T10 的防线之一，不是测试的障碍。
func (f *adminKeyFixture) sid(t *testing.T) string {
	t.Helper()
	if f.sids == nil {
		f.sids = map[string]string{}
	}
	if sid, ok := f.sids[f.admin.ID]; ok {
		return sid
	}
	sess, err := f.h.cfg.Sessions.Issue(f.admin, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("签发会话: %v", err)
	}
	f.sids[f.admin.ID] = sess.SID
	return sess.SID
}

// listKeys 调 GET /admin/apikeys，返回 key 列表与原始回执。
func (f *adminKeyFixture) listKeys(t *testing.T) ([]apikey.View, map[string]any) {
	t.Helper()
	rec := f.call(t, http.MethodGet, "/admin/apikeys", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/apikeys = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Keys []apikey.View `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("回执不是 JSON: %v / %s", err, rec.Body.String())
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
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

// ① 管理员看到**全部** key（含 owner 为空的老数据、含归属历史成员的）。
//
// 这条接住了原 ① 那一半仍然成立的意思：原判据是"列表按归属过滤"
// （管理员 = 全部 / 成员 = 只有自己的）。现在只剩"全部"这一侧，
// 所以改判"一把都不许漏"——它是**唯一**的身份，漏一把就等于那把 key
// 在这个部署里彻底失踪（既管理不了也看不见）。
func TestAPIKeysListShowsEverythingToTheOnlyAdmin(t *testing.T) {
	f := newAdminKeyFixture(t)

	all, raw := f.listKeys(t)
	if len(all) != 3 {
		t.Errorf("唯一管理员应当看到全部 3 把 key，实际 %d 把：%v", len(all), idsOf(all))
	}
	for _, want := range []string{f.adminKey, f.legacyKey, f.otherKey} {
		if !idsOf(all)[want] {
			t.Errorf("★ 漏了 key %s —— 单账号部署里它是唯一的管理者，漏掉它等于这把 key 没人管得了", want)
		}
	}
	// 管理员的回执里才有两把管理钥匙的掩码
	if raw["admin_key"] == "" || raw["admin_token"] == "" {
		t.Error("管理员的回执里应当有 admin_key / admin_token 两个掩码（前端要显示在用哪把）")
	}
}

// ⑦ 回执里不再有 `member` 字段（它恒等于 false）。
func TestAPIKeysPayloadHasNoMemberFlag(t *testing.T) {
	f := newAdminKeyFixture(t)
	_, raw := f.listKeys(t)

	if _, exists := raw["member"]; exists {
		t.Errorf("★ 回执里仍有 member 字段（= %v）——\n"+
			"它原先只服务「我的 API Key」标题，恒等于 !IsAdmin()；单账号口径下\n"+
			"恒为 false，留着就是一条「这里还分角色」的假线索。前端读不到这个键\n"+
			"会走管理员渲染路径（与改造前管理员的行为逐字节一致）。", raw["member"])
	}
	// 回执的可用性不受影响：前端真正需要的三个键都还在
	for _, want := range []string{"keys", "admin_key", "admin_token"} {
		if _, ok := raw[want]; !ok {
			t.Errorf("★ 删 member 字段不该动 %q —— 它是前端渲染的必需项", want)
		}
	}
}

// ② 新建的 key 归属当前身份。
//
// 口径（改造前后一致，只有"谁是当前身份"变了）：
//
//	会话身份（浏览器）  ⇒ owner = 那个账号的 ID
//	admin_token（脚本） ⇒ owner 为空（没有账号身份 = 管理员所有）
//
// ⚠ 改造前这条测的是"**成员**建的 key 归属那个成员"。成员没了 ⇒ 现在只有
// 管理员这一种会话身份，所以断言的是"归属**这个**管理员的 ID"。
// 单账号部署下它仍然重要：owner 是空还是这个 ID，决定了
// `ListFor`/`CanManage` 走哪条分支（见下面 ④⑤）。
func TestAPIKeysCreateStampsOwnerForAdmin(t *testing.T) {
	f := newAdminKeyFixture(t)

	rec := f.call(t, http.MethodPost, "/admin/apikeys", `{"name":"admin-new","limit":0,"rpm":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员建 key = %d: %s", rec.Code, rec.Body.String())
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
	if owner != f.admin.ID {
		t.Errorf("会话身份建的 key 归属 = %q，want %q（那个管理员账号的 ID）", owner, f.admin.ID)
	}

	// 用 admin_token 的脚本（无账号身份）建的 ⇒ owner 为空
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

	// ① 无论归属是空还是那个 ID，**同一个管理员都看得见**（5 把）
	all, _ := f.listKeys(t)
	if len(all) != 5 {
		t.Errorf("建完两把后应当看到 5 把，实际 %d（漏了就是「自己建完看不见」）", len(all))
	}
}

// ③ 管理员对**任何** key 的四个操作都可用（不再有"别人的 key"这一档）。
func TestAPIKeysMutationsWorkForAdminOnEveryKey(t *testing.T) {
	f := newAdminKeyFixture(t)

	cases := []struct{ name, path, body string }{
		{"toggle", "/admin/apikeys/toggle", `{"id":"%s","enabled":false}`},
		{"update", "/admin/apikeys/update", `{"id":"%s","limit":999}`},
		{"reset", "/admin/apikeys/reset", `{"id":"%s"}`},
		{"delete", "/admin/apikeys/delete", `{"id":"%s"}`},
	}
	// ⚠ 每个操作各用一把**新** key：delete 会把它删掉，删掉之后 reset/update
	// 拿到的是"key 不存在" ⇒ CanManage 判 false ⇒ 403。那是**正确**行为
	// （见 requireKeyAccess：不存在与不是你的都回 403，不给"这个 id 存在吗"
	// 的探测口），但会让"复用同一把"的写法看起来像权限不足。
	for _, c := range cases {
		own, err := f.keys.CreateOwned("", "own-"+c.name, 0, 0, nil)
		if err != nil {
			t.Fatalf("建测试 key: %v", err)
		}
		body := strings.Replace(c.body, "%s", own.ID, 1)
		if rec := f.call(t, http.MethodPost, c.path, body); rec.Code != http.StatusOK {
			t.Errorf("管理员 %s 自己的 key = %d，want 200: %s", c.name, rec.Code, rec.Body.String())
		}
	}
	// 归属**历史成员**的 key 同样管得了（改造前它属于那个成员，现在没有成员了）
	for _, c := range []struct{ name, path, body string }{
		{"toggle", "/admin/apikeys/toggle", `{"id":"` + f.otherKey + `","enabled":true}`},
		{"update", "/admin/apikeys/update", `{"id":"` + f.otherKey + `","limit":123}`},
	} {
		if rec := f.call(t, http.MethodPost, c.path, c.body); rec.Code != http.StatusOK {
			t.Errorf("管理员 %s 历史成员名下的 key = %d，want 200: %s", c.name, rec.Code, rec.Body.String())
		}
	}
}

// ④⑤ 归属判据本身的两条边界（存储层，不经过 HTTP）。
//
// 为什么单独测：`ListFor` / `CanManage` 是**判据的唯一实现**，HTTP 层只是调用它。
// 这两条边界（空 owner 与管理员同义、身份缺失走最严档）在这里钉住之后，
// 将来谁把它们改宽都会立刻红 —— 它们是"装配层忘了接线"时的最后一道闸门
// （fail-closed），与"成员是否存在"无关。
func TestOwnerPredicateBoundaries(t *testing.T) {
	f := newAdminKeyFixture(t)

	// 空 owner + 非管理员 ⇒ 空列表（**不是**"列出所有 owner 为空的"）
	if got := f.keys.ListFor("", false); len(got) != 0 {
		t.Errorf("★ 身份缺失（空 owner + 非管理员）应当得到空列表，实际 %d 条 ——\n"+
			"这里一旦放宽，未登录/会话失效/身份字段没接上都会把管理员的 key 端出去。", len(got))
	}
	// 空 owner + 管理员 ⇒ 全部
	if got := f.keys.ListFor("", true); len(got) != 3 {
		t.Errorf("管理员应当看到全部 3 条，实际 %d", len(got))
	}
	// 非管理员不得认领 owner 为空的老 key
	if f.keys.CanManage(legacyMemberID, false, f.legacyKey) {
		t.Error("★ 非管理员不得认领 owner 为空的老 key（空 = 管理员所有的标记）")
	}
	// ⚠ 这条分支**今天不可达**，但判据本身仍是"最小权限"的一部分，所以钉住它：
	//
	//	CanManage(owner, false, key) 且 OwnerID == owner ⇒ true
	//
	// 为什么不可达：唯一能产出 Principal 的地方是 account.Identify，而它只认
	// 管理员（非管理员的会话与账号都在 account 包那层就被拒了）。所以"非管理员
	// 身份"进不到这里。留着这条断言是为了**说明它是有意为之**：owner 是不透明
	// 字符串，本包不认识"成员"这个概念 —— 第二身份若将来出现，这道闸门直接生效。
	if !f.keys.CanManage(legacyMemberID, false, f.otherKey) {
		t.Error("CanManage(owner,false,自己名下的 key) 应当为真 ——\n" +
			"它是「第二身份存在时按归属隔离」的实现；今天因为没有第二身份而不可达，\n" +
			"但把它改窄会让将来加身份时静默失去隔离。")
	}
	if f.keys.CanManage("", false, f.adminKey) {
		t.Error("★ 身份缺失时不得管任何 key")
	}
	if !f.keys.CanManage("", true, f.otherKey) {
		t.Error("管理员应当能管历史成员名下的 key")
	}
}

// ⑥ 轮换管理凭据：只有管理员调得到（第二层硬判仍在）。
//
// ⚠ 语义变更：原判据是"成员调不到（403）"。成员身份不存在了 ⇒ 改成两条更强的：
//
//	① 无身份（本用例的默认门禁）连门都进不去 —— 401
//	② 管理员进得去，落到**业务校验**分支（说明第二层没把它挡成 403/404）
//
// 第二层硬判本身仍在 apikeys.go 里（requireAdmin），它现在的职责是
// "万一门禁被绕过，这里还有一次判定"。
func TestRotateEndpointsReachableOnlyAsAdmin(t *testing.T) {
	f := newAdminKeyFixture(t)

	for _, path := range []string{
		"/admin/apikeys/rotate-admin",
		"/admin/apikeys/rotate-api-key",
	} {
		// ① 无身份
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("无身份调 %s = %d，want 401", path, rec.Code)
		}
		// ⚠ 判据是"不得回显**凭据值**"（不是"不得出现 admin_token 这个词"）：
		// 401 的提示本来就要告诉调用方"怎么带令牌"（Bearer <admin_token>），
		// 那是可执行的引导；不能出现的是那把令牌本身与任何掩码。
		if body := rec.Body.String(); strings.Contains(body, testAdminToken) ||
			strings.Contains(body, "admin-api-key-shape") {
			t.Errorf("%s 的拒绝回执里不得出现任何凭据**值**，实际 %s", path, body)
		}

		// ② 管理员：进得去（夹具没配可轮换的凭据 ⇒ 业务层回 4xx，但不是 401/403）
		rec2 := f.call(t, http.MethodPost, path, `{}`)
		if rec2.Code == http.StatusUnauthorized || rec2.Code == http.StatusForbidden {
			t.Errorf("管理员调 %s 不该被门禁/角色挡住，实际 %d: %s", path, rec2.Code, rec2.Body.String())
		}
		if strings.Contains(rec2.Body.String(), "forbidden_role") {
			t.Errorf("%s 不该再出现 forbidden_role（成员口径已取消），实际 %s", path, rec2.Body.String())
		}
	}
}
