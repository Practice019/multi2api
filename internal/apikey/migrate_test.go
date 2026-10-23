// migrate_test.go 守 P3.5 迁移的三条承诺。
//
// # 为什么这三条必须被测试钉住
//
// 迁移是**启动时自动跑、且改的是用户数据**的一次性动作。它出错的三种形态
// 都不会在界面上报错，只会表现为"某些 key 突然不能用了"：
//
//	① 不幂等 ⇒ 每重启一次就多建一批分组，分组列表越来越长
//	② 把"不限制"的 key 迁成"空分组" ⇒ 那把 key 一个模型都不能用
//	③ 迁移后能用的模型变了 ⇒ 升级本该无感，结果调用方开始 403
//
// 第 ② 条最隐蔽：空白名单的语义是"不限制"，而空分组是"受限且为空"。
// 两者在数据上只差一个"有没有 Groups"，后果却是全部可用 vs 全部不可用。
package apikey

import (
	"errors"
	"path/filepath"
	"testing"
)

// fakeGroupStore 一个最小的分组表：负责建分组、撞名判定，并记录建过什么。
type fakeGroupStore struct {
	byName map[string][]string // 名字 → 模型
	ids    []string            // 建分组的顺序（按名字）
	nextID int
	// failOn 非空时，建分组一律返回这个错误（测"失败不拖住别人"）。
	failOn error
}

func newFakeGroupStore() *fakeGroupStore {
	return &fakeGroupStore{byName: map[string][]string{}}
}

func (f *fakeGroupStore) create(name string, models []string) (string, error) {
	if f.failOn != nil {
		return "", f.failOn
	}
	if _, exists := f.byName[name]; exists {
		return "", ErrGroupNameExists
	}
	f.nextID++
	id := "g-" + itoa(f.nextID)
	f.byName[name] = append([]string(nil), models...)
	f.ids = append(f.ids, name)
	return id, nil
}

func (f *fakeGroupStore) count() int { return len(f.byName) }

// modelsOf 按**分组 ID** 取模型清单（"g-1" → f.byName 里的第一项，按创建顺序）。
//
// 为什么需要它：断言要与"谁先被迁移"解耦 —— 见 TestMigrateRetriesOnNameCollision 的注释。
func (f *fakeGroupStore) modelsOf(id string) []string {
	idx := 0
	for _, name := range f.ids {
		idx++
		if "g-"+itoa(idx) == id {
			return f.byName[name]
		}
	}
	return nil
}

// itoa 避免为了一条测试引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append(b, byte('0'+n%10))
		n /= 10
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// 基本迁移：有白名单的 key 变成"指向一个分组"，白名单被清空。
func TestMigrateBasic(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create("app", 0, 0, []string{"glm-5.2", "deepseek-v4-flash"})
	if err != nil {
		t.Fatal(err)
	}
	gs := newFakeGroupStore()

	res, err := MigrateModelsToGroups(s, gs.create)
	if err != nil {
		t.Fatalf("迁移报错: %v", err)
	}
	if res.KeysMigrated != 1 || res.GroupsCreated != 1 || res.Failed != 0 {
		t.Errorf("结果不对: %+v", res)
	}

	groups, _ := s.GroupsOf(k.ID)
	if len(groups) != 1 || groups[0] != "g-1" {
		t.Fatalf("key 应指向新分组 g-1，实际 %v", groups)
	}
	models, _ := s.ModelsOf(k.ID)
	if len(models) != 0 {
		t.Errorf("白名单应被清空（收敛到分组），实际 %v", models)
	}
	// 分组的模型 = 原白名单
	if got := gs.byName["app 的模型"]; !equalStrings(got, []string{"glm-5.2", "deepseek-v4-flash"}) {
		t.Errorf("分组模型应为原白名单，实际 %v", got)
	}
	// 打上标记
	if !mustKey(t, s, k.ID).MigratedToGroups {
		t.Error("迁移后应打上 MigratedToGroups 标记")
	}
}

// ★ 幂等：连跑两次，分组数不变、key 的状态不变。
func TestMigrateIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create("app", 0, 0, []string{"m-a"}); err != nil {
		t.Fatal(err)
	}
	gs := newFakeGroupStore()

	res1, err := MigrateModelsToGroups(s, gs.create)
	if err != nil {
		t.Fatal(err)
	}
	if res1.KeysMigrated != 1 || gs.count() != 1 {
		t.Fatalf("第一次迁移应建 1 个分组，实际 %+v / %d 个", res1, gs.count())
	}

	res2, err := MigrateModelsToGroups(s, gs.create)
	if err != nil {
		t.Fatal(err)
	}
	if res2.KeysMigrated != 0 || res2.GroupsCreated != 0 {
		t.Errorf("★ 第二次迁移不该再建分组（否则每重启一次分组列表就长一截），实际 %+v", res2)
	}
	if gs.count() != 1 {
		t.Errorf("★ 连跑两次后分组数必须还是 1，实际 %d", gs.count())
	}
	if res2.Skipped != 1 {
		t.Errorf("第二次应记为 skipped=1，实际 %+v", res2)
	}

	// 第三次也一样（防止"只幂等两次"的实现）
	if _, err := MigrateModelsToGroups(s, gs.create); err != nil {
		t.Fatal(err)
	}
	if gs.count() != 1 {
		t.Errorf("第三次迁移后分组数仍应是 1，实际 %d", gs.count())
	}
}

// ★ 空白名单（= 不限制）**不迁**：否则那把 key 会变成"受限且为空"⇒ 全部不可用。
func TestMigrateSkipsUnrestrictedKeys(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("free", 0, 0, nil)
	gs := newFakeGroupStore()

	res, err := MigrateModelsToGroups(s, gs.create)
	if err != nil {
		t.Fatal(err)
	}
	if res.KeysMigrated != 0 || gs.count() != 0 {
		t.Fatalf("空白名单的 key 不该被迁移，实际 %+v / %d 个分组", res, gs.count())
	}
	groups, _ := s.GroupsOf(k.ID)
	if len(groups) != 0 {
		t.Errorf("★ 给「不限制」的 key 建空分组 = 把它变成「一个模型都不许」，"+
			"一次迁移会把所有没想过限制的 key 打死。实际分组 %v", groups)
	}
}

// 已有分组的 key 不迁（用户手工配过，迁移不得覆盖）。
func TestMigrateSkipsKeysWithGroups(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, []string{"legacy"})
	if err := s.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-manual"}); err != nil {
		t.Fatal(err)
	}
	gs := newFakeGroupStore()
	res, err := MigrateModelsToGroups(s, gs.create)
	if err != nil {
		t.Fatal(err)
	}
	if res.KeysMigrated != 0 || gs.count() != 0 {
		t.Errorf("已有分组的 key 不该被迁移（会覆盖用户配置），实际 %+v", res)
	}
	if g, _ := s.GroupsOf(k.ID); len(g) != 1 || g[0] != "g-manual" {
		t.Errorf("用户配的分组不该被改掉，实际 %v", g)
	}
}

// 撞名：同名 key 迁移时加后缀重试，**不整段失败**。
func TestMigrateRetriesOnNameCollision(t *testing.T) {
	s := newTestStore(t)
	k1, _ := s.Create("app", 0, 0, []string{"m-1"})
	k2, _ := s.Create("app", 0, 0, []string{"m-2"})
	gs := newFakeGroupStore()

	res, err := MigrateModelsToGroups(s, gs.create)
	if err != nil {
		t.Fatalf("撞名不该让整段迁移失败: %v", err)
	}
	if res.KeysMigrated != 2 {
		t.Errorf("两把同名 key 都要迁成功，实际 %+v", res)
	}
	if gs.count() != 2 {
		t.Errorf("应建出 2 个分组（第二个带后缀），实际 %d: %v", gs.count(), gs.ids)
	}
	if gs.ids[0] != "app 的模型" || gs.ids[1] != "app 的模型-2" {
		t.Errorf("后缀应形如 -2，实际 %v", gs.ids)
	}
	// 两把 key 指向**不同**分组
	g1, _ := s.GroupsOf(k1.ID)
	g2, _ := s.GroupsOf(k2.ID)
	if len(g1) != 1 || len(g2) != 1 || g1[0] == g2[0] {
		t.Fatalf("两把 key 应指向不同分组，实际 %v / %v", g1, g2)
	}
	// ⚠ 断言必须**与遍历顺序无关**：两把 key 都叫 "app"，谁先被迁移是
	// map 遍历顺序决定的（每次跑都可能不同），所以不能假定 k1 拿到无后缀的那个名字。
	//
	// 真正要守的是**内容对应**：每把 key 指向的那个分组，里面的模型必须是
	// 它自己原来的白名单。写成"名字-2 一定属于 k2"会间歇性变红，
	// 而红的时候实现其实是对的 —— 那种测试比没有更糟。
	if got := gs.modelsOf(g1[0]); !equalStrings(got, []string{"m-1"}) {
		t.Errorf("key %s 的分组 %s 应含它自己的模型 [m-1]，实际 %v", k1.ID, g1[0], got)
	}
	if got := gs.modelsOf(g2[0]); !equalStrings(got, []string{"m-2"}) {
		t.Errorf("key %s 的分组 %s 应含它自己的模型 [m-2]，实际 %v", k2.ID, g2[0], got)
	}
	// 名字本身：一个无后缀、一个带 -2（顺序不定，但两种形态都必须出现）
	names := map[string]bool{}
	for n := range gs.byName {
		names[n] = true
	}
	if !names["app 的模型"] || !names["app 的模型-2"] {
		t.Errorf("撞名应加后缀 -2，实际建了 %v", gs.ids)
	}
}

// 非撞名错误 ⇒ 立刻失败该 key，但**不拖住**其它 key。
func TestMigrateFailureDoesNotBlockOthers(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create("a", 0, 0, []string{"m-a"}); err != nil {
		t.Fatal(err)
	}
	gs := newFakeGroupStore()
	gs.failOn = errors.New("磁盘满了")

	res, err := MigrateModelsToGroups(s, gs.create)
	if err != nil {
		t.Fatalf("单把失败不该让整段返回 error: %v", err)
	}
	if res.Failed != 1 || res.KeysMigrated != 0 {
		t.Errorf("应记 1 个失败，实际 %+v", res)
	}
	if len(res.Details) != 1 {
		t.Errorf("失败原因要带出来（启动日志直接用），实际 %v", res.Details)
	}
}

// 无名字的 key：分组名退化成"迁移自 <ID 前 8 位>"。
func TestMigrateNameFallback(t *testing.T) {
	s := newTestStore(t)
	// Create 会拒绝空名，所以直接塞内部记录（模拟手改过的 apikeys.json）
	s.mu.Lock()
	s.keys["sk-abcdefgh12345678"] = &Key{
		ID: "sk-abcdefgh12345678", Enabled: true, Models: []string{"m-a"},
	}
	s.mu.Unlock()
	gs := newFakeGroupStore()

	res, err := MigrateModelsToGroups(s, gs.create)
	if err != nil {
		t.Fatal(err)
	}
	if res.KeysMigrated != 1 {
		t.Fatalf("应迁移 1 把，实际 %+v", res)
	}
	if _, ok := gs.byName["迁移自 sk-abcde"]; !ok {
		t.Errorf("无名字时应退化成「迁移自 <ID 前8位>」，实际建了 %v", gs.ids)
	}
}

// ModelMap 必须在迁移后**保留**（它做的是改名，不是限定范围）。
func TestMigrateKeepsModelMap(t *testing.T) {
	s := newTestStore(t)
	mm := map[string]string{"alias-x": "m-a"}
	k, err := s.CreateOwned("", "app", 0, 0, []string{"m-a"}, mm)
	if err != nil {
		t.Fatal(err)
	}
	gs := newFakeGroupStore()
	if _, err := MigrateModelsToGroups(s, gs.create); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ModelMapOf(k.ID)
	if got["alias-x"] != "m-a" {
		t.Errorf("★ ModelMap 必须保留（清掉等于把所有对外名一次性作废），实际 %v", got)
	}
}

// 迁移后判定链仍放行原来的模型（升级无感）。
func TestMigrateKeepsEffectivePermissions(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("app", 0, 0, []string{"m-a", "m-b"})
	gs := newFakeGroupStore()
	if _, err := MigrateModelsToGroups(s, gs.create); err != nil {
		t.Fatal(err)
	}
	// 用注入的分组解析函数模拟装配层接线
	s.SetGroupModels(func(id string) []string {
		for name, models := range gs.byName {
			_ = name
			if id == "g-1" {
				return models
			}
		}
		return nil
	})
	_, models, ok := s.GroupModelsOf(k.ID)
	if !ok {
		t.Fatal("key 应仍存在")
	}
	if !equalStrings(models, []string{"m-a", "m-b"}) {
		t.Errorf("★ 迁移后能用的模型必须一模一样（升级无感），实际 %v", models)
	}
}

// nil 参数：不 panic、不做事。
func TestMigrateNilArgs(t *testing.T) {
	if res, err := MigrateModelsToGroups(nil, nil); err != nil || res.KeysMigrated != 0 {
		t.Errorf("nil 参数应无操作，实际 %+v %v", res, err)
	}
	s := newTestStore(t)
	if res, err := MigrateModelsToGroups(s, nil); err != nil || res.KeysMigrated != 0 {
		t.Errorf("nil createGroup 应无操作，实际 %+v %v", res, err)
	}
}

// 迁移结果要落盘（重启后不会又变成"待迁移"）。
func TestMigratePersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apikeys.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := s.Create("app", 0, 0, []string{"m-a"})
	gs := newFakeGroupStore()
	if _, err := MigrateModelsToGroups(s, gs.create); err != nil {
		t.Fatal(err)
	}

	// 重启：标记与分组都要读回来，再跑一次不会重复建组
	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := s2.GroupsOf(k.ID); len(g) != 1 {
		t.Fatalf("重启后分组应还在，实际 %v", g)
	}
	res, err := MigrateModelsToGroups(s2, gs.create)
	if err != nil {
		t.Fatal(err)
	}
	if res.KeysMigrated != 0 || gs.count() != 1 {
		t.Errorf("★ 重启后再跑不该重复建组，实际 %+v / %d 个", res, gs.count())
	}
}
