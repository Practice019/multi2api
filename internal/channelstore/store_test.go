// store_test.go 守卫 channelstore 的三条行为：
//
//	① 落盘与重载（含能力索引自愈）
//	② 能力索引随渠道变化同步（改 models / 停用 / 删除 ⇒ 对外可见模型跟着变）
//	③ 返回的是深拷贝（调用方改不动缓存 —— 否则"改一个返回值"会静默污染存储）
package channelstore

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/store"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	// B1：SQL 是存储后端 ⇒ 先开临时库；path 仍传"历史文件"路径（不存在 = 空库，只打一行日志）。
	openTestDB(t)
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

// ① 写入 + 重载：渠道与能力都还在。
//
// ⚠ B1：断言从"文件落盘 + 0600 权限"换成"SQL 读回"。旧断言针对 channels.json，
// 而那个文件已经不是权威了（权威是 data/gw.db，它的权限由 store 侧负责）；
// 继续断言文件存在会变成"测一个已经废弃的产物"。
func TestUpsertPersistsAndReloads(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.Upsert(ch("c1", "m-a", "m-b")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// 直接问 SQL：这一行真的在库里（不是躺在某个内存缓存里）。
	row, ok, err := store.GetChannelByExternalID("c1")
	if err != nil {
		t.Fatalf("按外部 id 查 SQL: %v", err)
	}
	if !ok || row == nil {
		t.Fatalf("SQL 里应有 external_id=c1 的行（ok=%v）", ok)
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
