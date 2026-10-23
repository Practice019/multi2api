// model_meta_test.go 钉住 /v1/models 的元数据契约。
//
// # 用户报的四条里，两条与这里有关
//
//	第1条  001 的 context_length / max_output_tokens 为空
//	第3条  004 的 max_output_tokens = 0（可能是不限，也可能是漏填）
//
// 根因是同一个：**零值写成 0**，让"未知"变成了一个确切的假事实。
// 客户端读到 0 会以为"这个模型上下文长度为 0"，而不是"上游没报"。
package server

import (
	"testing"

	"workbuddy2api/internal/channelstore"
)

// TestEntryOfOmitsZeroMeta 0 值不该出现在 /v1/models 的条目里。
func TestEntryOfOmitsZeroMeta(t *testing.T) {
	h := &Handler{cfg: Config{}}

	// 上游只报了 id（001 的形态）。
	e := h.entryOf("001/deepseek-v4.1-flash-1017", 0, 0, "")
	if _, has := e["context_length"]; has {
		t.Error("★ context_length=0 不该写出来 —— 客户端会读成「上下文长度为 0」这个确切的假事实")
	}
	if _, has := e["max_output_tokens"]; has {
		t.Error("★ max_output_tokens=0 不该写出来（同样是把未知说成 0）")
	}
	if e["id"] != "001/deepseek-v4.1-flash-1017" {
		t.Errorf("id 丢了: %v", e["id"])
	}

	// 有值时照常写。
	e2 := h.entryOf("x/y", 1000000, 128000, "ch-a")
	if e2["context_length"] != int64(1000000) {
		t.Errorf("context_length: %v", e2["context_length"])
	}
	if e2["max_output_tokens"] != int64(128000) {
		t.Errorf("max_output_tokens: %v", e2["max_output_tokens"])
	}
}

// TestEntryOfOmitsEmptyOwnedBy scrub 时空的 owned_by 不该作为字段出现。
//
// # 为什么"空字符串"也不行
//
// 一个 "owned_by": "" 仍然在说"这里有个来源字段，只是我没填"。
// 客户端按字段存在与否分组时照样能分出"这两类是同一组"——
// 而用户的要求是「按别名处理，不能暴露别的」。
func TestEntryOfOmitsEmptyOwnedBy(t *testing.T) {
	h := &Handler{cfg: Config{}}
	e := h.entryOf("007/x", 200000, 32000, "")
	if _, has := e["owned_by"]; has {
		t.Error("★ owned_by 为空时不该输出该字段")
	}
	e2 := h.entryOf("007/x", 200000, 32000, "ch-a")
	if e2["owned_by"] != "ch-a" {
		t.Errorf("有值时应当输出: %v", e2["owned_by"])
	}
}

// TestChannelInfoHasMetaFields 渠道要能声明模型的 ctx / max_output。
//
// # 用户报的第 1 条在渠道侧的形态
//
// 001/deepseek-v4.1-flash-1017 走的是**渠道**，而渠道的模型以前只有
// 「公开名 → 上游名」的映射，不带任何元数据 ⇒ 同一个列表里
// 有的条目有 ctx、有的是空的。
//
// 这条钉住"渠道能声明它"，否则上游改了窗口大小我们永远跟不上。
func TestChannelInfoHasMetaFields(t *testing.T) {
	// 编译期即可验证：字段存在且是可写的 int64。
	var info channelstoreChannelInfoCheck
	info.ContextLength = 1000000
	info.MaxOutputTokens = 128000
	if info.ContextLength != 1000000 || info.MaxOutputTokens != 128000 {
		t.Error("渠道元数据字段不可用")
	}
}

// channelstoreChannelInfoCheck 指向 channelstore.ChannelInfo，避免在本文件引入无关 import。
type channelstoreChannelInfoCheck = channelstore.ChannelInfo
