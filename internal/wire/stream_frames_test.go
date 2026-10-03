package wire

import (
	"io"
	"strings"
	"testing"
)

// TestFrameScannerBasics 逐帧读取的基本行为。
func TestFrameScannerBasics(t *testing.T) {
	stream := "data: {\"a\":1}\n\n" +
		"\n" + // 多余空行（帧分隔）
		": keep-alive\n\n" + // 注释帧
		"data: {\"b\":2}\n\n" +
		"data: [DONE]\n\n"

	sc := NewFrameScanner(strings.NewReader(stream))
	var got []string
	var comments, done int
	for {
		f, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next 出错: %v", err)
		}
		switch {
		case f.IsDone:
			done++
		case f.NonData:
			comments++
		default:
			got = append(got, f.Raw)
		}
	}
	if done != 1 {
		t.Errorf("[DONE] 次数 = %d，want 1", done)
	}
	if comments != 1 {
		t.Errorf("注释帧数 = %d，want 1", comments)
	}
	if len(got) != 2 {
		t.Fatalf("数据帧 = %d，want 2（空行必须被吞掉，不能算帧）", len(got))
	}
	if got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Errorf("payload 不对: %v", got)
	}
}

// TestFrameScannerAcceptsBothDataShapes 必须同时认 `data:` 与 `data: `。
//
// # 这是本包**已经踩过**的坑（见 sseData 的注释）
//
// 早先只认带空格的形态，CodeArts（华为 InferHub）的每帧都匹配不上，
// 后果按流式与否劈成两半：`stream:false` 恒 502，`stream:true` 看着正常。
//
// 帧读取器复用的就是同一个 sseData，所以这条判据天然一致 ——
// 但正因为它重要，仍要有测试钉住"两条路给出同一个答案"。
func TestFrameScannerAcceptsBothDataShapes(t *testing.T) {
	// 无空格形态（CodeArts 实测形态）
	sc := NewFrameScanner(strings.NewReader("data:{\"x\":1}\n\ndata: {\"y\":2}\n\n"))
	var raws []string
	for {
		f, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		raws = append(raws, f.Raw)
	}
	if len(raws) != 2 {
		t.Fatalf("帧数 = %d，want 2（两种形态都该认出来）", len(raws))
	}
	if raws[0] != `{"x":1}` {
		t.Errorf("无空格形态解析错: %q", raws[0])
	}
}

// TestFrameScannerNormalizedStripsExtraFields 规范化必须剥掉上游额外字段。
//
// 转发给客户端的是 Normalized（不是 Raw）—— 上游的私有字段不该漏出去。
// 但 Raw 要**保留原文**：流内错误需要原文喂给上游自己的分类器。
func TestFrameScannerNormalizedStripsExtraFields(t *testing.T) {
	sc := NewFrameScanner(strings.NewReader(
		"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"upstream_secret\":\"s\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	f, err := sc.Next()
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if !strings.Contains(f.Raw, "upstream_secret") {
		t.Error("Raw 应当保留原文（流内错误分类器需要它）")
	}
	if _, leaked := f.Normalized["upstream_secret"]; leaked {
		t.Error("Normalized 不该带上游的额外字段")
	}
	if f.Normalized["id"] != "x" {
		t.Errorf("Normalized 丢了标准字段: %v", f.Normalized)
	}
}

// TestFrameScannerNonJSONDegrades 非 JSON 的 data 帧不能丢。
//
// 上游偶尔发非 JSON 的 data 行。丢帧会让内容静默消失；
// 应当 Obj=nil 但仍可透传 Raw（与 Stream 的降级行为一致）。
func TestFrameScannerNonJSONDegrades(t *testing.T) {
	sc := NewFrameScanner(strings.NewReader("data: not json at all\n\n"))
	f, err := sc.Next()
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if f.Obj != nil {
		t.Error("非 JSON 时 Obj 应为 nil")
	}
	if f.Raw != "not json at all" {
		t.Errorf("Raw = %q，应当原样保留以便透传", f.Raw)
	}
	if f.Normalized != nil {
		t.Error("Normalized 应当为 nil（调用方据此走原样透传）")
	}
}

// TestInBandErrorOfExported 导出的流内错误判据与内部一致。
//
// 工具循环要逐帧问"这帧是错误吗"。让它自己写一套判据等于把
// inBandErrorOf 的教训（第一版只认顶层 error，漏了华为系
// error_code/error_msg）复制成两份。
func TestInBandErrorOfExported(t *testing.T) {
	t.Run("华为系错误信封", func(t *testing.T) {
		obj := map[string]any{"error_code": "InferHub.002002009.404", "error_msg": "The model is not registered"}
		ib, ok := InBandErrorOf(obj, `{"error_code":"InferHub.002002009.404","error_msg":"…"}`)
		if !ok || ib == nil {
			t.Fatal("没认出华为系的错误信封")
		}
		if ib.Message == "" {
			t.Error("Message 为空")
		}
	})
	t.Run("正常数据帧（带额外诊断字段）不算错误", func(t *testing.T) {
		obj := map[string]any{
			"choices": []any{map[string]any{"index": 0}},
			"usage":   map[string]any{"total_tokens": 1},
		}
		if _, ok := InBandErrorOf(obj, `{"choices":[…]}`); ok {
			t.Error("有 choices 的帧不该被判成错误信封 —— 那是正常数据帧多带了字段")
		}
	})
}
