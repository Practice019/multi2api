// store_test.go 守 groupstore 的四条硬语义：
//
//  1. 名字唯一 —— 重名时**原有数据不能被破坏**（失败必须是"什么都没发生"）
//  2. 落盘可读回 —— 重启（重新 New）之后配置一模一样
//  3. 文件损坏显式报错 —— 绝不静默重建（那等于清空用户配置）
//  4. 模型名归一化 —— 空格/空串/重复/顺序不能漏到内存或盘上
//
// 第 3 条单独列出来的理由：它是唯一一条"错了也没有报错线索"的路径 ——
// 静默重建之后程序照常跑，只是用户的分组全没了。
package groupstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore 造一个临时目录里的空库。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestCRUDRoundTrip 建/读/改/删 的完整流程。
func TestCRUDRoundTrip(t *testing.T) {
	s := newTestStore(t)

	g, err := s.Create("vip", "付费用户", []string{"m-b", "m-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(g.ID, "g-") {
		t.Errorf("ID 应以 g- 开头，实际 %q", g.ID)
	}
	if g.Name != "vip" || g.Description != "付费用户" {
		t.Errorf("字段没存对: %+v", g)
	}
	if _, err := time.Parse(time.RFC3339, g.CreatedAt); err != nil {
		t.Errorf("CreatedAt 不是 RFC3339: %q (%v)", g.CreatedAt, err)
	}

	got, ok := s.Get(g.ID)
	if !ok || got.Name != "vip" {
		t.Fatalf("Get 拿不到刚建的分组: %+v ok=%v", got, ok)
	}
	if byName, ok := s.GetByName("vip"); !ok || byName.ID != g.ID {
		t.Errorf("GetByName 没拿到同一条: %+v ok=%v", byName, ok)
	}
	if _, ok := s.GetByName("  vip  "); !ok {
		t.Error("GetByName 应容忍首尾空白（表单里很容易带进来）")
	}
	if _, ok := s.GetByName("nope"); ok {
		t.Error("不存在的名字不该命中")
	}

	newName, newDesc := "vip2", "改过"
	upd, err := s.Update(g.ID, &newName, &newDesc, []string{"m-c"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Name != "vip2" || upd.Description != "改过" {
		t.Errorf("Update 没生效: %+v", upd)
	}
	if len(upd.Models) != 1 || upd.Models[0] != "m-c" {
		t.Errorf("Update 应整表替换模型清单，实际 %v", upd.Models)
	}
	if upd.CreatedAt != g.CreatedAt {
		t.Error("CreatedAt 是创建时间，Update 不得改动它")
	}

	cleared, err := s.Update(g.ID, nil, nil, []string{})
	if err != nil {
		t.Fatalf("Update(clear): %v", err)
	}
	if len(cleared.Models) != 0 {
		t.Errorf("空切片应清空模型清单，实际 %v", cleared.Models)
	}
	if cleared.Name != "vip2" {
		t.Error("nil 参数不得改动其它字段")
	}

	if err := s.Delete(g.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(g.ID); ok {
		t.Error("删掉之后还能 Get 到")
	}
	if n := len(s.List()); n != 0 {
		t.Errorf("删完应为空，实际 %d 条", n)
	}
}

// TestNameUnique 重名必须报错，且**原有数据不被破坏**。
//
// "不被破坏"是这条测试的重点：如果实现先覆盖再校验，用户会丢掉原来那个分组。
func TestNameUnique(t *testing.T) {
	s := newTestStore(t)
	a, err := s.Create("dup", "原来的", []string{"m-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := s.Create("other", "", []string{"m-b"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []struct {
		name    string
		act     func() error
		wantErr error
	}{
		{"Create 用已存在的名字", func() error { _, e := s.Create("dup", "新的", []string{"m-x"}); return e }, ErrNameExists},
		{"Create 名字带空白也算重名", func() error { _, e := s.Create("  dup  ", "", nil); return e }, ErrNameExists},
		{"Update 改成别人的名字", func() error {
			n := "dup"
			_, e := s.Update(b.ID, &n, nil, nil)
			return e
		}, ErrNameExists},
		{"Create 空名字", func() error { _, e := s.Create("   ", "", nil); return e }, ErrNameEmpty},
		{"Update 名字改成空", func() error {
			n := " "
			_, e := s.Update(b.ID, &n, nil, nil)
			return e
		}, ErrNameEmpty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.act(); !errors.Is(err, c.wantErr) {
				t.Fatalf("期望 %v，实际 %v", c.wantErr, err)
			}
		})
	}

	if n := len(s.List()); n != 2 {
		t.Fatalf("失败的写入不该改变分组数，期望 2 实际 %d", n)
	}
	got, ok := s.Get(a.ID)
	if !ok || got.Description != "原来的" || len(got.Models) != 1 {
		t.Errorf("原有分组被写坏了: %+v ok=%v", got, ok)
	}
	if got2, _ := s.Get(b.ID); got2.Name != "other" {
		t.Errorf("失败的重命名把名字改掉了: %+v", got2)
	}

	if _, err := s.Update(b.ID, &b.Name, nil, nil); err != nil {
		t.Errorf("改成自己原有的名字不该报重名: %v", err)
	}
	if _, err := s.Create("DUP", "", nil); err != nil {
		t.Errorf("名字比较是大小写敏感的，DUP 应可用: %v", err)
	}
}

// TestPersistence 重新 New 同一路径要读回一模一样的配置。
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")
	s1, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g, err := s1.Create("持久化", "desc", []string{"m-b", "m-a", "m-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s1.Create("第二条", "", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}

	s2, err := New(path)
	if err != nil {
		t.Fatalf("重新 New: %v", err)
	}
	list := s2.List()
	if len(list) != 2 {
		t.Fatalf("期望读回 2 条，实际 %d", len(list))
	}
	back, ok := s2.Get(g.ID)
	if !ok {
		t.Fatalf("读不回 id=%s", g.ID)
	}
	if back.Name != "持久化" || back.Description != "desc" || back.CreatedAt != g.CreatedAt {
		t.Errorf("读回的字段不一致: %+v vs %+v", back, g)
	}
	if len(back.Models) != 2 || back.Models[0] != "m-a" || back.Models[1] != "m-b" {
		t.Errorf("模型清单读回不对（应已归一化）: %v", back.Models)
	}
	// 空模型清单要落成 []，不能是 null（前端少一条 null 分支）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if strings.Contains(string(raw), `"models": null`) {
		t.Error("空模型清单被写成 null，应为 []")
	}
}

// TestCorruptFileIsNotRebuilt 文件损坏必须报错，且**不得**覆盖成空库。
func TestCorruptFileIsNotRebuilt(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"不是 JSON", `{ this is not json`},
		{"截断的 JSON", `{"groups": [{"id": "g-1"`},
		{"group 缺少 id", `{"groups": [{"name": "a"}]}`},
		{"group 缺少 name", `{"groups": [{"id": "g-1"}]}`},
		{"名字重复", `{"groups": [{"id": "g-1", "name": "a"}, {"id": "g-2", "name": "a"}]}`},
		{"id 重复", `{"groups": [{"id": "g-1", "name": "a"}, {"id": "g-1", "name": "b"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "groups.json")
			body := c.body
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("准备坏文件: %v", err)
			}
			s, err := New(path)
			if err == nil {
				t.Fatalf("损坏的文件必须报错，实际成功（还读到了 %d 条）", len(s.List()))
			}
			// 关键：文件内容原封不动（没被"修好"成空库）
			after, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("读回文件: %v", rerr)
			}
			if string(after) != body {
				t.Error("报错之后不该动磁盘上的文件 —— 那是用户唯一的副本")
			}
		})
	}
}

// TestMissingAndEmptyFileAreEmptyStore 文件不存在 / 空文件都按空库处理（首次运行）。
func TestMissingAndEmptyFileAreEmptyStore(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "nope.json"))
	if err != nil || s == nil {
		t.Fatalf("文件不存在应按空库处理，实际 %v", err)
	}
	if n := len(s.List()); n != 0 {
		t.Errorf("期望空库，实际 %d 条", n)
	}

	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err != nil {
		t.Errorf("空文件不是损坏，应按空库处理: %v", err)
	}
}

// TestModelNormalization 模型的归一化：空格/空串/重复/顺序。
func TestModelNormalization(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"去首尾空白", []string{"  m-a  "}, []string{"m-a"}},
		{"丢空串", []string{"m-a", "", "   "}, []string{"m-a"}},
		{"去重", []string{"m-a", " m-a ", "m-a"}, []string{"m-a"}},
		{"排序", []string{"m-c", "m-a", "m-b"}, []string{"m-a", "m-b", "m-c"}},
		{"混合", []string{" m-b", "", "m-a", "m-b ", "  "}, []string{"m-a", "m-b"}},
		{"nil 得到空切片", nil, []string{}},
		{"全空得到空切片", []string{"", " "}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestStore(t)
			g, err := s.Create("g", "", c.in)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := g.Models; !equalStrings(got, c.want) {
				t.Fatalf("期望 %v，实际 %v", c.want, got)
			}
			if g.Models == nil {
				t.Error("Models 不该是 nil（JSON 里要写成 []，不是 null）")
			}
			s2, err := New(s.path)
			if err != nil {
				t.Fatalf("重新 New: %v", err)
			}
			if got, _ := s2.Get(g.ID); !equalStrings(got.Models, c.want) {
				t.Errorf("盘上的模型清单没归一化: %v", got.Models)
			}
		})
	}
}

// TestUpsert 按 ID 覆盖、ID 为空则生成、名字仍要唯一。
func TestUpsert(t *testing.T) {
	s := newTestStore(t)
	if err := s.Upsert(&Group{ID: "g-fixed", Name: "固定ID", Models: []string{" m-a ", "m-a"}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	g, ok := s.Get("g-fixed")
	if !ok {
		t.Fatal("Upsert 没写进去")
	}
	if g.CreatedAt == "" {
		t.Error("Upsert 应补 CreatedAt")
	}
	if !equalStrings(g.Models, []string{"m-a"}) {
		t.Errorf("Upsert 也要归一化模型，实际 %v", g.Models)
	}

	// 同一 ID 再写一次 = 覆盖（幂等迁移依赖这条）
	if err := s.Upsert(&Group{ID: "g-fixed", Name: "固定ID", Description: "第二版"}); err != nil {
		t.Fatalf("重复 Upsert: %v", err)
	}
	if n := len(s.List()); n != 1 {
		t.Errorf("同 ID 应为覆盖，分组数仍是 1，实际 %d", n)
	}
	if g2, _ := s.Get("g-fixed"); g2.Description != "第二版" {
		t.Errorf("覆盖没生效: %+v", g2)
	}

	// 空 ID ⇒ 生成一个
	if err := s.Upsert(&Group{Name: "自动ID"}); err != nil {
		t.Fatalf("Upsert(空ID): %v", err)
	}
	if n := len(s.List()); n != 2 {
		t.Errorf("空 ID 应新增一条，实际 %d", n)
	}

	// 拿 A 的 ID 去写 B 的名字 ⇒ 重名
	if err := s.Upsert(&Group{ID: "g-fixed", Name: "自动ID"}); !errors.Is(err, ErrNameExists) {
		t.Errorf("Upsert 也要校验名字唯一，实际 %v", err)
	}
	if err := s.Upsert(nil); err == nil {
		t.Error("nil 应报错")
	}
}

// TestDeleteMissingAndFailure 删不存在的 ID：报 ErrNotFound 且不留半改状态。
func TestDeleteMissingAndFailure(t *testing.T) {
	s := newTestStore(t)
	g, err := s.Create("keep", "", []string{"m-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete("g-does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在的 ID 应报 ErrNotFound，实际 %v", err)
	}
	if err := s.Delete(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删空 ID 应报 ErrNotFound，实际 %v", err)
	}
	if n := len(s.List()); n != 1 {
		t.Errorf("失败的删除不该动别的分组，实际 %d 条", n)
	}
	if _, ok := s.Get(g.ID); !ok {
		t.Error("失败的删除把别的分组删掉了")
	}
	if _, err := s.Update("g-does-not-exist", nil, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("改不存在的 ID 应报 ErrNotFound，实际 %v", err)
	}
}

// TestSaveFailureLeavesNoHalfState 落盘失败时内存必须回到操作前的状态。
//
// 手法：把 path 指到一个**已存在的目录**上 —— rename 到目录必然失败。
// 这是唯一能稳定制造"写盘失败"而不引入故障注入接口的办法。
func TestSaveFailureLeavesNoHalfState(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "as-dir")
	if err := os.MkdirAll(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Store{path: badPath, groups: map[string]*Group{}, now: time.Now}

	if _, err := s.Create("x", "", []string{"m-a"}); err == nil {
		t.Fatal("写到目录上应报错")
	}
	if n := len(s.List()); n != 0 {
		t.Errorf("写盘失败后内存里不该留下那条分组，实际 %d 条", n)
	}

	// Delete 失败同理：不能把内存里那条删掉（否则重启后它又回来了）
	s2 := &Store{path: badPath, groups: map[string]*Group{}, now: time.Now}
	s2.groups["g-pre"] = &Group{ID: "g-pre", Name: "已有", Models: []string{}}
	if err := s2.Delete("g-pre"); err == nil {
		t.Fatal("写到目录上应报错")
	}
	if _, ok := s2.Get("g-pre"); !ok {
		t.Error("写盘失败后不该真的删掉那条（否则重启后它又回来了）")
	}
}

// TestConcurrentCreate 并发建：结果条数正确，且没有丢写。
func TestConcurrentCreate(t *testing.T) {
	s := newTestStore(t)
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "g-" + string(rune('a'+i%26)) + "-" + itoa(i)
			if _, err := s.Create(name, "", []string{"m-a"}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发 Create 失败: %v", err)
	}
	if got := len(s.List()); got != n {
		t.Fatalf("期望 %d 条，实际 %d（丢写）", n, got)
	}
	// 盘上也要是 n 条：并发下最容易出现的 bug 是"内存有、盘上没有"
	s2, err := New(s.path)
	if err != nil {
		t.Fatalf("重新 New: %v", err)
	}
	if got := len(s2.List()); got != n {
		t.Errorf("盘上期望 %d 条，实际 %d", n, got)
	}
}

// TestConcurrentCreateSameNameOnlyOneWins 并发抢同一个名字：只能成功一个。
//
// 名字唯一性若靠"先查再写"而不在同一把锁里，并发下会同时通过检查。
func TestConcurrentCreateSameNameOnlyOneWins(t *testing.T) {
	s := newTestStore(t)
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Create("same", "", nil); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("同名并发建应只有 1 个成功，实际 %d", ok)
	}
	if got := len(s.List()); got != 1 {
		t.Errorf("应只剩 1 条，实际 %d", got)
	}
}

// TestListIsDeepCopy 调用方改 List 的返回值不能污染存储。
func TestListIsDeepCopy(t *testing.T) {
	s := newTestStore(t)
	g, err := s.Create("a", "", []string{"m-a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	list := s.List()
	list[0].Name = "被改了"
	list[0].Models[0] = "被改了"
	back, _ := s.Get(g.ID)
	if back.Name != "a" || back.Models[0] != "m-a" {
		t.Errorf("List 返回的不是深拷贝: %+v", back)
	}
}

// TestSnapshotShape 落盘形状（版本号在、字段名对）—— 配置是给人看和手改的。
func TestSnapshotShape(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Create("a", "d", []string{"m-a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	var probe struct {
		Version int `json:"version"`
		Groups  []struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Models      []string `json:"models"`
			CreatedAt   string   `json:"created_at"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("落盘内容不是合法 JSON: %v", err)
	}
	if probe.Version != 1 {
		t.Errorf("version 应为 1，实际 %d", probe.Version)
	}
	if len(probe.Groups) != 1 || probe.Groups[0].Name != "a" || probe.Groups[0].Description != "d" {
		t.Errorf("字段名/内容不对: %+v", probe.Groups)
	}
}

// equalStrings 比两个字符串切片（nil 与空切片视为相等 —— 语义上都是"没有"）。
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// itoa 把非负整数转成十进制字符串。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
