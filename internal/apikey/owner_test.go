// owner_test.go —— key「归属账号」在**存储层**的守卫（登录系统多用户改造的第一步）。
//
// # 用户的需求
//
//	「成员只能看到自己的 key」
//
// # 本文件盯的是一条**不会自我暴露**的故障
//
// 过滤写错时：代码照编译、接口照回 200、界面照常渲染，唯一的症状是"某个成员
// 偶尔看见别人（或管理员）的 key"。所以这里把边角逐个钉死：空 owner + 非管理员
// 必须为空、老数据仍可用、视图也不把归属漏掉。
//
// # 命名
//
// 顶层函数名沿用本仓既有的 `TestXxx` 形态（全仓测试无一例外），用例名
// （t.Run 的标签）用中文。
package apikey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newOwnerStore 起一个全新的空库，并把它落盘的路径一并给出（重启类断言要用）。
func newOwnerStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apikeys.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

// 带归属建的 key 能被 OwnerOf 读回，且归属之外的字段不受影响。
func TestCreateOwnedAndOwnerOf(t *testing.T) {
	s, path := newOwnerStore(t)
	k, err := s.CreateOwned("u-alice", "alice-app", 100, 5, []string{"glm-5.2"})
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := s.OwnerOf(k.ID)
	if !ok || owner != "u-alice" {
		t.Fatalf("OwnerOf = (%q, %v)，期望 (u-alice, true)", owner, ok)
	}
	if k.Limit != 100 || k.RPM != 5 || len(k.Models) != 1 {
		t.Errorf("归属之外的字段不该受影响，实际 limit=%d rpm=%d models=%#v",
			k.Limit, k.RPM, k.Models)
	}

	// 归属必须随落盘持久化：否则重启一次，成员就又看得见全部 key 了。
	again, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if owner, ok := again.OwnerOf(k.ID); !ok || owner != "u-alice" {
		t.Errorf("重启后归属应保留，实际 (%q, %v)", owner, ok)
	}

	if _, ok := s.OwnerOf("sk-nonexistent"); ok {
		t.Error("不存在的 key 应报 ok=false（不能只看字符串，空串是合法归属）")
	}
}

// Create 仍是"管理员所有"：它委托给 CreateOwned("", ...)，既有调用方
// （internal/admin/apikeys.go）与既有测试都按这个行为断言。
func TestCreateIsAdminOwned(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create("老形态", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if k.OwnerID != "" {
		t.Errorf("Create 建的 key 归属必须为空（= 管理员所有），实际 %q", k.OwnerID)
	}
	if owner, ok := s.OwnerOf(k.ID); !ok || owner != "" {
		t.Errorf("OwnerOf = (%q, %v)，期望 (\"\", true)", owner, ok)
	}
}

// 成员之间互相看不见：各自只拿到自己名下的那一把，且视图带出归属
// （前端要显示「这把 key 是谁建的」）。
func TestListForIsolatesMembers(t *testing.T) {
	s := newTestStore(t)
	a, err := s.CreateOwned("u-alice", "alice-app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateOwned("u-bob", "bob-app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("admin-app", 0, 0, nil); err != nil {
		t.Fatal(err)
	}

	alice := s.ListFor("u-alice", false)
	if len(alice) != 1 || alice[0].ID != a.ID {
		t.Fatalf("alice 只该看到自己的 1 把，实际 %+v", alice)
	}
	if alice[0].OwnerID != "u-alice" {
		t.Errorf("视图应带出归属，实际 %q", alice[0].OwnerID)
	}

	bob := s.ListFor("u-bob", false)
	if len(bob) != 1 || bob[0].ID != b.ID {
		t.Fatalf("bob 只该看到自己的 1 把，实际 %+v", bob)
	}
	for _, v := range bob {
		if v.ID == a.ID {
			t.Error("bob 不该看见 alice 的 key")
		}
	}

	if got := s.ListFor("u-carol", false); len(got) != 0 {
		t.Errorf("没建过 key 的成员应看到空列表，实际 %+v", got)
	}
}

// 管理员（isAdmin=true）看全部，不分归属。
func TestListForAdminSeesAll(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create("admin-app", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwned("u-alice", "alice-app", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwned("u-bob", "bob-app", 0, 0, nil); err != nil {
		t.Fatal(err)
	}

	all := s.ListFor("", true)
	if len(all) != 3 {
		t.Fatalf("管理员应看到 3 把，实际 %d 把：%+v", len(all), all)
	}
	// 管理员带着自己的 owner 访问时，口径仍是"全部"（不是"只看自己的"）：
	// 他是管理员，不是"恰好有归属的成员"。
	if got := s.ListFor("u-alice", true); len(got) != 3 {
		t.Errorf("管理员带 owner 访问也应看到全部 3 把，实际 %d 把", len(got))
	}
}

// ★ 空 owner + 非管理员 ⇒ **空列表**（本次改造最危险的坑）。
//
// 反向写法（`k.OwnerID != owner → skip`，两边都空就相等）会把"空"当成
// "同样是空的那一类人"，而那正是**管理员所有**的那批 key —— 于是未登录、
// 会话失效、身份字段没接上，任何一次 owner 落到空串，都把管理员的 key 列表
// 端给成员。这条测试就是钉住它。
func TestListForBlankOwnerIsEmpty(t *testing.T) {
	s := newTestStore(t)
	// 两种"空归属"的 key：老形态（Create）与显式空 owner 建出来的。
	if _, err := s.Create("admin-app", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwned("", "also-admin", 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwned("u-alice", "alice-app", 0, 0, nil); err != nil {
		t.Fatal(err)
	}

	got := s.ListFor("", false)
	if len(got) != 0 {
		t.Fatalf("空归属 + 非管理员必须返回空列表（空 = 管理员所有，不是无主），"+
			"实际 %d 把：%+v", len(got), got)
	}
	// 还要是**非 nil 空切片**：nil 序列化成 null，前端得再判一次类型。
	if got == nil {
		t.Error("应返回空切片而不是 nil（JSON 里 null 与 [] 是两种东西）")
	}
	// 前后空白也不该被当成"有一个空 owner 的成员"。
	if got := s.ListFor("   ", false); len(got) != 0 {
		t.Errorf("全是空白的 owner 同样必须是空列表，实际 %d 把", len(got))
	}
}

// CanManage：成员只能管自己的；无身份（空 owner）谁都不能认领。
func TestCanManage(t *testing.T) {
	s := newTestStore(t)
	alice, err := s.CreateOwned("u-alice", "alice-app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.Create("admin-app", 0, 0, nil) // 老形态：归属为空
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		owner   string
		isAdmin bool
		id      string
		want    bool
	}{
		{"成员管自己的 key → 可以", "u-alice", false, alice.ID, true},
		{"成员管别人的 key → 不可以", "u-bob", false, alice.ID, false},
		{"成员认领无归属的老 key → 不可以", "u-alice", false, legacy.ID, false},
		{"空身份管自己的（无处可依）→ 不可以", "", false, legacy.ID, false},
		{"空身份管成员的 key → 不可以", "", false, alice.ID, false},
		{"管理员管成员的 key → 可以", "", true, alice.ID, true},
		{"管理员管无归属的老 key → 可以", "", true, legacy.ID, true},
		{"不存在的 key → 谁都不可以", "", true, "sk-nonexistent", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.CanManage(c.owner, c.isAdmin, c.id); got != c.want {
				t.Errorf("CanManage(%q, %v, %q) = %v，期望 %v",
					c.owner, c.isAdmin, c.id, got, c.want)
			}
		})
	}
}

// 老数据兼容：手写一份**不含 owner_id** 的 apikeys.json（改造前落盘的样子），
// 让 New() 加载。三条判据缺一不可：
//
//	① 这些 key 仍然**可用**（Validate 通过）—— 升级不该打死正在跑的调用方；
//	② 它们的 OwnerID == ""（= 管理员所有）⇒ 成员列表里看不见、也管不到；
//	③ 写回一次之后文件里**不多出** owner_id（omitempty），"只是重启了一下"
//	   不该产生整文件 diff。
func TestLegacyFileWithoutOwnerLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apikeys.json")
	const id = "sk-0123456789abcdef"
	legacy := `[
  {
    "id": "sk-0123456789abcdef",
    "name": "老调用方",
    "enabled": true,
    "limit": 0,
    "used": 7,
    "requests": 3,
    "fails": 0,
    "rpm": 0,
    "created_at": "2026-01-02T03:04:05Z"
  }
]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatalf("老文件应能加载，实际 %v", err)
	}

	if _, err := s.Validate(id); err != nil {
		t.Fatalf("老 key 升级后必须仍然可用，实际 %v", err)
	}
	owner, ok := s.OwnerOf(id)
	if !ok {
		t.Fatal("老 key 应能被查到")
	}
	if owner != "" {
		t.Errorf("老 key 的归属应为空（= 管理员所有），实际 %q", owner)
	}
	if got := s.ListFor("u-alice", false); len(got) != 0 {
		t.Errorf("老 key 是管理员所有的，成员不该看见，实际 %+v", got)
	}
	if s.CanManage("u-alice", false, id) {
		t.Error("成员不得认领无归属的老 key")
	}
	admin := s.ListFor("", true)
	if len(admin) != 1 || admin[0].OwnerID != "" {
		t.Fatalf("管理员应看到这把老 key 且归属为空，实际 %+v", admin)
	}

	// 触发一次落盘，确认 omitempty 没把 owner_id="" 写进文件。
	if err := s.Reset(id); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "owner_id") {
		t.Errorf("老 key 写回后不该多出 owner_id 字段，实际：\n%s", raw)
	}
}
