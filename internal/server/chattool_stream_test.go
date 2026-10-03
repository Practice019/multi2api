package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/wire"
)

// frameOf 造一帧用于测试（模拟上游 chunk）。
func frameOf(t *testing.T, payload string) *wire.Frame {
	t.Helper()
	sc := wire.NewFrameScanner(strings.NewReader("data: " + payload + "\n\n"))
	f, err := sc.Next()
	if err != nil {
		t.Fatalf("造帧失败: %v", err)
	}
	return f
}

// TestToolCallNamesInEmptyIsNotNoTools 空名字 ≠ 没有工具。
//
// # 这条是流式判断的核心前提
//
// 流式里工具名**只在第一个 delta** 出现，后续 delta 只带 arguments 片段：
//
//	{"delta":{"tool_calls":[{"index":0,"function":{"name":"generate_image"}}]}}
//	{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"prompt\""}}]}}
//
// 所以 toolCallNamesIn 返回空切片是**常态**，调用方必须理解成
// "本帧没提供名字"，而不是"本帧没有工具调用"。
//
// 若把它当成"没有工具"，续帧就会被当普通帧透传出去 ——
// 客户端会看到半截 tool_calls（协议错乱）。
func TestToolCallNamesInEmptyIsNotNoTools(t *testing.T) {
	t.Run("首帧带名字", func(t *testing.T) {
		f := frameOf(t, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"generate_image"}}]}}]}`)
		names := toolCallNamesIn(f.Obj)
		if len(names) != 1 || names[0] != "generate_image" {
			t.Fatalf("names = %v，want [generate_image]", names)
		}
	})
	t.Run("续帧只有 arguments（名字为空）", func(t *testing.T) {
		f := frameOf(t, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"prompt\":\"cat\"}"}}]}}]}`)
		if names := toolCallNamesIn(f.Obj); len(names) != 0 {
			t.Fatalf("续帧应当没有名字，得到 %v", names)
		}
	})
	t.Run("普通文字帧", func(t *testing.T) {
		f := frameOf(t, `{"choices":[{"delta":{"content":"你好"}}]}`)
		if names := toolCallNamesIn(f.Obj); len(names) != 0 {
			t.Fatalf("文字帧不该有工具名，得到 %v", names)
		}
	})
	t.Run("多个工具（同一帧）", func(t *testing.T) {
		f := frameOf(t, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"bash"}},{"index":1,"function":{"name":"generate_image"}}]}}]}`)
		if names := toolCallNamesIn(f.Obj); len(names) != 2 {
			t.Fatalf("names = %v，want 2 个", names)
		}
	})
}

// TestAnyNameNotMine 归属判定。
//
// mine 为空（收尾轮"工具已去掉"）时，**任何**工具名都该算外部的 ——
// 否则收尾轮会把自己的 tool_calls 又拦下，陷入死循环。
func TestAnyNameNotMine(t *testing.T) {
	mine := map[string]bool{"generate_image": true}
	for _, tc := range []struct {
		name  string
		names []string
		mine  map[string]bool
		want  bool
	}{
		{"全是我们的", []string{"generate_image"}, mine, false},
		{"有外部的", []string{"generate_image", "bash"}, mine, true},
		{"只有外部的", []string{"bash"}, mine, true},
		{"空名字列表", []string{}, mine, false},
		{"mine 为空时任何名字都算外部", []string{"generate_image"}, nil, true},
		{"mine 为空且空名字", []string{}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := anyNameNotMine(tc.names, tc.mine); got != tc.want {
				t.Errorf("anyNameNotMine(%v) = %v，want %v", tc.names, got, tc.want)
			}
		})
	}
}

// TestStripTools 收尾轮必须真的去掉工具。
//
// 不去掉的话模型可能又调一次，永远收不了尾（到顶轮的意义就没了）。
func TestStripTools(t *testing.T) {
	in := []byte(`{"model":"m","tools":[{"type":"function"}],"tool_choice":"auto","messages":[]}`)
	out, err := stripTools(in)
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if _, has := obj["tools"]; has {
		t.Error("tools 没被去掉 —— 模型会又调一次，收不了尾")
	}
	if _, has := obj["tool_choice"]; has {
		t.Error("tool_choice 没被去掉")
	}
	if _, has := obj["messages"]; !has {
		t.Error("messages 被误删了")
	}
}

// TestSSEWriterFrameNormalizes 转发时用**规范化后**的对象。
//
// 上游的额外字段不该直接漏给客户端（与 wire.Stream 一致）。
func TestSSEWriterFrameNormalizes(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec)
	f := frameOf(t, `{"id":"x","object":"chat.completion.chunk","upstream_secret":"不该漏","choices":[{"index":0,"delta":{"content":"hi"}}]}`)
	if err := sw.frame(f); err != nil {
		t.Fatalf("写出失败: %v", err)
	}
	got := rec.Body.String()
	if strings.Contains(got, "upstream_secret") {
		t.Errorf("上游的额外字段漏给客户端了：%s", got)
	}
	if !strings.Contains(got, `"content":"hi"`) {
		t.Errorf("正文丢了：%s", got)
	}
	if !strings.HasPrefix(got, "data: ") {
		t.Errorf("不是 SSE data 帧：%s", got)
	}
}

// TestSSEWriterCommentOnlyComments 保活帧必须是注释（客户端会忽略）。
//
// # 为什么需要保活
//
// 工具执行期间（生图实测 13-35 秒）网关一个字节都不发。
// 中间的代理/客户端常在 30-60 秒空闲后掐掉连接 —— 那样用户不但看不到图，
// 连已经生成好的结果都拿不到。
func TestSSEWriterCommentOnlyComments(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec)
	sw.comment("正在执行工具 generate_image…")
	got := rec.Body.String()
	if !strings.HasPrefix(got, ": ") {
		t.Fatalf("保活帧必须是注释（`: ` 开头），实际：%q", got)
	}
	if strings.Contains(got, "data:") {
		t.Errorf("保活帧不该带 data（那会被当成内容）：%q", got)
	}
}

// TestEmitArtifactsAsTextCarriesMarkdown 兜底发射必须把 markdown 送出去。
//
// # 为什么这条兜底不能少（我第一版漏了，实测才发现）
//
// 流式路径里，模型收尾那句话是**逐帧**流出去的，而图片 markdown
// 没有地方发 —— 如果 emitArtifactsAsText 不在正确的时机被调用，
// 现象是"图生成了、模型说话了、用户看不到图"，而积分已经扣掉。
func TestEmitArtifactsAsTextCarriesMarkdown(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec)
	arts := []gateway.Artifact{{
		"type":     "image",
		"url":      "https://example.invalid/a.png",
		"markdown": "![猫](https://example.invalid/a.png)",
	}}
	emitArtifactsAsText(sw, arts)
	got := rec.Body.String()
	if !strings.Contains(got, "![猫](") {
		t.Fatalf("markdown 没发出去（发了就等于白花积分）：%s", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("没收尾帧，客户端会挂到超时：%s", got)
	}
}

// TestEmitArtifactsAsTextNoopWithoutMarkdown 没有 markdown 时不该发多余帧。
//
// 普通聊天（没有产物）走不到这里，但纯文本产物（如"note"）可能没有
// markdown —— 那时多发一帧空内容会让客户端多渲染一个空行。
func TestEmitArtifactsAsTextNoopWithoutMarkdown(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec)
	emitArtifactsAsText(sw, []gateway.Artifact{{"type": "note"}})
	if rec.Body.Len() != 0 {
		t.Errorf("没有 markdown 时不该写任何帧，实际：%q", rec.Body.String())
	}
}
