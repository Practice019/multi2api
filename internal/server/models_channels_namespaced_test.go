// models_channels_namespaced_test.go 钉住「同名模型不得因跨渠道重名而消失」。
//
// # 用户实测报障
//
// 「渠道有这些模型，但是分组里面找不到」「不应该只有三个模型，应该还有其他的模型才对」
//
// 现场：一条渠道声明 8 个模型，对外目录里只见 3 个 —— 缺的 5 个恰好是与另一条渠道
// **重名**的那些。根因是两条按裸模型名收敛的链路：
//
//	EnabledModels         按 ab.Model 去重 ⇒ 丢掉"属于哪条渠道"
//	channelReverseMapping map[裸上游名]公开名 ⇒ 多渠道同名时先到先得
//
// 本用例把形态固定下来：两条渠道都声明同一个模型名时，**两条命名空间名都要出现**。
package server

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/store"
)

func TestSameModelNameFromTwoChannelsBothVisible(t *testing.T) {
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建 store 失败: %v", err)
	}
	// 两条渠道声明**同一个**上游模型名（这正是重名现场）。
	for _, c := range []struct{ id, name string }{
		{"ch-aaa", "chan-a"},
		{"ch-bbb", "chan-b"},
	} {
		if err := s.Upsert(&channelstore.Channel{
			ID: c.id, Name: c.name, BaseURL: "https://placeholder.invalid/v1",
			Status: channelstore.StatusEnabled,
			Models: []string{"shared-model"},
			Keys:   []channelstore.ChannelKey{{Key: "sk-" + c.id, Status: channelstore.StatusEnabled}},
		}); err != nil {
			t.Fatalf("建渠道 %s 失败: %v", c.name, err)
		}
	}
	h := NewHandler(Config{Channels: s})

	names, ok := h.channelPublicNames(nil)
	if !ok {
		t.Fatal("配了渠道存储，应当返回渠道口径")
	}
	hasA, hasB := false, false
	for _, n := range names {
		if n == "chan-a/shared-model" {
			hasA = true
		}
		if n == "chan-b/shared-model" {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Errorf("两条渠道的同名模型都必须出现在对外目录里（否则其中一条的模型对用户彻底不可见）：\n"+
			"  chan-a/shared-model = %v\n  chan-b/shared-model = %v\n  实际列表 = %v", hasA, hasB, names)
	}
}
