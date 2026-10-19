// store_test.go 守卫 channelstore 的三条行为：
//
//	① 落盘与重载（含能力索引自愈）
//	② 能力索引随渠道变化同步（改 models / 停用 / 删除 ⇒ 对外可见模型跟着变）
//	③ 返回的是深拷贝（调用方改不动缓存 —— 否则"改一个返回值"会静默污染存储）
package channelstore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "channels.json")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, path
}

func ch(id string, models ...string) *Channel {
	return &Channel{
		ID:      id,
		Name:    id,
		Type:    1,
		BaseURL: "https://example.invalid/v1",
		Keys:    []ChannelKey{{Key: "sk-" + id, Status: StatusEnabled}},
		Models:  models,
	}
}

// ① 落盘 + 重载：渠道与能力都还在。
func TestUpsertPersistsAndReloads(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.Upsert(ch("c1", "m-a", "m-b")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应已落盘: %v", err)
	}
	// 权限必须是 0600（里面有 key）。
	// ⚠ Windows 不认 POSIX 权限位（Chmod(0600) 之后 Mode().Perm() 报 -rw-rw-rw-），
	// 所以这条只在非 Windows 上断言 —— 否则是"判据错、不是代码错"（2026-10-20 踩到）。
	if fi, err := os.Stat(path); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("渠道文件权限应为 0600，实际 %v", fi.Mode().Perm())
	}

	s2, err := New(path)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if _, ok := s2.Get("c1"); !ok {
		t.Fatal("重载后渠道丢失")
	}
	got := s2.EnabledModels(DefaultGroup)
	if len(got) != 2 || got[0] != "m-a" || got[1] != "m-b" {
		t.Fatalf("重载后对外模型应为 [m-a m-b]，实际 %v", got)
	}
}

// ② 能力索引随渠道变化同步 —— 这是"选路/对外列表只读能力索引"的前提。
func TestAbilityIndexFollowsChannel(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Upsert(ch("c1", "m-a", "m-b")); err != nil {
		t.Fatal(err)
	}

	// 改 models：能力表要跟着换（旧的必须消失，不能残留）
	c1, _ := s.Get("c1")
	c1.Models = []string{"m-c"}
	if err := s.Upsert(c1); err != nil {
		t.Fatal(err)
	}
	got := s.EnabledModels(DefaultGroup)
	if len(got) != 1 || got[0] != "m-c" {
		t.Fatalf("改 models 后应只剩 m-c，实际 %v", got)
	}

	// 停用：能力整体消失（new-api：状态必须是 enabled 才可用）
	if err := s.SetStatus("c1", StatusManuallyDisabled); err != nil {
		t.Fatal(err)
	}
	if got := s.EnabledModels(DefaultGroup); len(got) != 0 {
		t.Fatalf("停用后不该有对外模型，实际 %v", got)
	}
	// 再启用：能力回来
	if err := s.SetStatus("c1", StatusEnabled); err != nil {
		t.Fatal(err)
	}
	if got := s.EnabledModels(DefaultGroup); len(got) != 1 {
		t.Fatalf("恢复启用后应有 1 个对外模型，实际 %v", got)
	}

	// 删除：渠道与能力一起走
	if err := s.Delete("c1"); err != nil {
		t.Fatal(err)
	}
	if got := s.EnabledModels(DefaultGroup); len(got) != 0 {
		t.Fatalf("删除后不该有对外模型，实际 %v", got)
	}
	if n := len(s.Abilities()); n != 0 {
		t.Fatalf("删除后不该有孤儿能力记录，实际 %d 条", n)
	}
}

// 两个渠道声明同一个模型 ⇒ 对外目录只出现一次（能力索引去重）。
func TestEnabledModelsDedupes(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Upsert(ch("c1", "m-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ch("c2", "m-a", "m-b")); err != nil {
		t.Fatal(err)
	}
	got := s.EnabledModels(DefaultGroup)
	if len(got) != 2 {
		t.Fatalf("两个渠道共享 m-a 时对外应只有 2 个模型，实际 %v", got)
	}
}

// ③ 深拷贝：改返回值不能污染存储。
func TestGetReturnsClone(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Upsert(ch("c1", "m-a")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("c1")
	got.Models = append(got.Models, "m-注入")
	got.ModelMap["pub"] = "real"
	got.Keys[0].Key = "sk-被改"

	again, _ := s.Get("c1")
	if len(again.Models) != 1 {
		t.Fatalf("存储里的 models 被返回值改动了: %v", again.Models)
	}
	if _, ok := again.ModelMap["pub"]; ok {
		t.Fatal("存储里的 ModelMap 被返回值改动了")
	}
	if again.Keys[0].Key != "sk-c1" {
		t.Fatalf("存储里的 key 被返回值改动了: %s", again.Keys[0].Key)
	}
}

// 不存在/空 ID 的错误面（别静默成功）。
func TestErrorsAreExplicit(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Upsert(&Channel{}); err == nil {
		t.Fatal("空 ID 的 Upsert 应报错")
	}
	if err := s.Delete("nope"); err == nil {
		t.Fatal("删不存在的渠道应报错")
	}
	if err := s.SetStatus("nope", StatusEnabled); err == nil {
		t.Fatal("改不存在渠道的状态应报错")
	}
	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get 不存在的渠道应返回 false")
	}
}
