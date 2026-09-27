package qoder

import (
	"io"
	"strings"
	"testing"
)

// TestUnwrapEnvelopeTakesInnerChoices 信封帧必须被解出内层 OpenAI 帧。
//
// 这是本文件存在的**唯一理由**：不解包时下游看到的每帧都没有 choices 字段，
// 表现为静默无输出。所以这条测试是"功能可用"的直接判据。
func TestUnwrapEnvelopeTakesInnerChoices(t *testing.T) {
	// 真实形态：内层是**被转义了一层**的 JSON 字符串
	line := `data:{"headers":{"Content-Type":"application/json"},` +
		`"body":"{\"choices\":[{\"delta\":{\"content\":\"Q\"}}]}",` +
		`"statusCodeValue":200,"statusCode":"OK"}`

	got, changed := unwrapEnvelopeLine(line)
	if !changed {
		t.Fatal("信封帧未被改写 —— 下游会看到一帧没有 choices 的内容（静默无输出）")
	}
	want := `data: {"choices":[{"delta":{"content":"Q"}}]}`
	if got != want {
		t.Errorf("解包结果 = %q，want %q", got, want)
	}
	// 反证：内层确实被取出来了（外层字段不该残留）
	if strings.Contains(got, "statusCodeValue") {
		t.Error("外层信封字段残留 —— 说明只剥了一半")
	}
}

// TestUnwrapEnvelopeKeepsCodeField 业务错误帧必须**保留 code 字段**，
// 且转换后的帧必须能被 IsInBandError 认出来。
//
// # 判据为什么是"能被 IsInBandError 认出"而不是逐字段比对
//
// 逐字段比对**测不住真正的缺陷**。我第一版就是这么写的（断言含
// `"code":10605` 与 `"message":"queue full"`），然后做变异实验：
// 把错误帧分支整个短路掉（`if false`，即**原样透传内层**）——
// 测试**依然全绿**。因为原样透传的内层本身就是
// `{"code":10605,"message":"queue full"}`，它当然含 code 与 message。
//
// 但两者的功能后果完全不同：IsInBandError 的判据是
// 「含 code **且含 type** 且不含 choices」，而原样透传的内层**没有 type 字段**
// → 认不出来 → 错误被静默当成"正常结束、无内容"，
// 正是 errors.go 注释里记的那条真实缺陷（UI 表现为「干净地停止、无任何报错」）。
//
// 所以判据必须落在**消费方能不能识别**上：那才是"错误会不会被吞掉"的直接答案。
// 这条也钉住了"转换后的帧必须带 type"这个**看似冗余**的要求。
func TestUnwrapEnvelopeKeepsCodeField(t *testing.T) {
	line := `data:{"body":"{\"code\":10605,\"message\":\"queue full\"}","statusCodeValue":200}`

	got, changed := unwrapEnvelopeLine(line)
	if !changed {
		t.Fatal("错误信封未被改写 —— 错误会被当成无内容吞掉（静默停止）")
	}
	body := strings.TrimPrefix(got, "data: ")

	// 核心判据：转换后的帧必须被本包的错误识别器认出来。
	// （短路错误分支、或漏掉 type 字段，都会让这里红。）
	ok, msg := IsInBandError(body)
	if !ok {
		t.Fatalf("转换后的错误帧 IsInBandError 认不出来 —— "+
			"错误会被静默吞掉，UI 表现为「干净地停止、无任何报错」。帧=%q", body)
	}
	if !strings.Contains(msg, "queue full") {
		t.Errorf("识别出的错误文案丢了原文：%q", msg)
	}

	// code 必须是**独立字段**且保持数字形态：下游按顶层 code 识别排队错误，
	// 把它拼进 message 会让识别永远不命中（参照项目的真实缺陷）。
	if !strings.Contains(body, `"code":10605`) {
		t.Errorf("code 字段丢失或变形 —— 下游按顶层 code 识别排队错误会永远不命中。得到 %q", body)
	}
	// message 不得被拼上后缀
	if strings.Contains(body, "(10605)") || strings.Contains(body, `10605"`) {
		t.Error("code 被拼进了 message 文案 —— 那会污染内层 JSON，下游无法二次解析")
	}
}

// TestUnwrapEnvelopeErrorFrameIsNotPassedThroughRaw 反向：错误帧不能原样透传。
//
// 这是上一条的**变异守卫**：短路错误分支后内层会被原样发出，
// 那种形态 IsInBandError 认不出来。这里直接断言"原样形态会被漏掉"，
// 让"短路"这个变异必然红 —— 而不是依赖上一条间接覆盖。
func TestUnwrapEnvelopeErrorFrameIsNotPassedThroughRaw(t *testing.T) {
	// 原样内层（**没有 type 字段**）—— 这正是短路分支后会发出的东西
	rawInner := `{"code":10605,"message":"queue full"}`
	if ok, _ := IsInBandError(rawInner); ok {
		t.Skip("IsInBandError 的判据变了（现在认没有 type 的形态）；" +
			"若确实放宽了判据，本测试的前提需重新评估")
	}

	line := `data:{"body":"{\"code\":10605,\"message\":\"queue full\"}","statusCodeValue":200}`
	got, _ := unwrapEnvelopeLine(line)
	body := strings.TrimPrefix(got, "data: ")

	if body == rawInner {
		t.Fatal("错误帧被原样透传了 —— IsInBandError 认不出这种形态，" +
			"错误会被静默吞掉。必须转成带 type 字段的错误帧")
	}
}

// TestUnwrapEnvelopePassesThroughNonEnvelope 非信封帧原样透传。
//
// 这是**容错**而非兜底：服务端某天直接回标准帧是合法的，
// 那时必须放行而不是把它当信封解出空内容。
func TestUnwrapEnvelopePassesThroughNonEnvelope(t *testing.T) {
	cases := []struct{ name, line string }{
		{"标准 OpenAI 帧", `data:{"choices":[{"delta":{"content":"hi"}}]}`},
		{"DONE 标记", `data: [DONE]`},
		{"event 行", `event: finish`},
		{"空 data", `data:`},
		{"非法 JSON", `data:not-json-at-all`},
		{"无 body 字段的 JSON", `data:{"statusCodeValue":200}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := unwrapEnvelopeLine(c.line)
			if changed {
				t.Errorf("不该改写，得到 %q", got)
			}
			if got != c.line {
				t.Errorf("必须逐字节原样透传：%q → %q", c.line, got)
			}
		})
	}
}

// TestUnwrapEnvelopeHandlesUnescapedBodyObject body 是对象而非字符串时也要能解。
//
// 服务端两种形态都可能下发，参照实现同样两路处理。
func TestUnwrapEnvelopeHandlesUnescapedBodyObject(t *testing.T) {
	line := `data:{"body":{"choices":[{"delta":{"content":"x"}}]},"statusCodeValue":200}`
	got, changed := unwrapEnvelopeLine(line)
	if !changed {
		t.Fatal("body 为对象时未解包")
	}
	if !strings.Contains(got, `"choices"`) {
		t.Errorf("内层 choices 未取出：%q", got)
	}
}

// TestUnwrapEnvelopeStreamSplitsAcrossChunks 流式解包必须处理跨 chunk 的半行。
//
// ⚠ 这是流式解析的**核心风险**：TCP 分片不尊重行边界，一次 Read 很可能拿到
// 半行。把半行当整行转换会得到一段无法解析的 JSON —— 表现为随机丢帧
//（且只在网络分片恰好在中间时复现，极难定位）。
func TestUnwrapEnvelopeStreamSplitsAcrossChunks(t *testing.T) {
	full := `data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"A\"}}]}"}` + "\n" +
		`data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"B\"}}]}"}` + "\n"

	// 逐字节喂入：每个 chunk 只有一个字节，必然切在行中间
	src := &bytewiseReader{data: []byte(full)}
	out, err := io.ReadAll(unwrapEnvelopeStream(src))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}

	got := string(out)
	if !strings.Contains(got, `"content":"A"`) || !strings.Contains(got, `"content":"B"`) {
		t.Errorf("逐字节喂入时丢帧 —— 跨 chunk 的半行处理有误。得到 %q", got)
	}
	// 两帧不能粘连成一帧
	if strings.Count(got, "data:") != 2 {
		t.Errorf("帧数不对（粘连或丢失）：%q", got)
	}
}

// TestUnwrapEnvelopeStreamPreservesBlankLineSeparators 空行（帧分隔符）必须保留。
//
// 丢了空行会让下游把相邻两帧拼成一帧，表现为"内容粘连"。
func TestUnwrapEnvelopeStreamPreservesBlankLineSeparators(t *testing.T) {
	full := "data:{\"body\":\"{\\\"choices\\\":[]}\"}\n\ndata: [DONE]\n"
	out, err := io.ReadAll(unwrapEnvelopeStream(strings.NewReader(full)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "\n\n") {
		t.Errorf("空行分隔符丢失（会导致帧粘连）：%q", string(out))
	}
}

// TestUnwrapEnvelopeStreamStripsCR SSE 允许 CRLF，\r 必须剥掉。
//
// 留着 \r 会让 JSON 解析失败（表现为整帧丢弃）。
func TestUnwrapEnvelopeStreamStripsCR(t *testing.T) {
	full := "data:{\"body\":\"{\\\"choices\\\":[1]}\"}\r\ndata: [DONE]\r\n"
	out, err := io.ReadAll(unwrapEnvelopeStream(strings.NewReader(full)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "\r") {
		t.Errorf("\\r 未剥掉（会让下游 JSON 解析失败）：%q", string(out))
	}
}

// TestUnwrapEnvelopeStreamForwardsClose Close 必须转发到源流。
//
// 不转发会让 HTTP 连接泄漏 —— 上游一多就表现成"跑一会儿全卡住"。
// 契约测试的 verifyChatStreamClosable 专门检查这一条。
func TestUnwrapEnvelopeStreamForwardsClose(t *testing.T) {
	src := &closeTrackingReader{Reader: strings.NewReader("data: x\n")}
	r := unwrapEnvelopeStream(src)
	if err := r.Close(); err != nil {
		t.Fatalf("Close 报错: %v", err)
	}
	if !src.closed {
		t.Error("Close 未转发到源流 —— 会导致 HTTP 连接泄漏")
	}
	// 二次 Close 不报错（有些实现会 panic）
	if err := r.Close(); err != nil {
		t.Errorf("二次 Close 应无错，得到 %v", err)
	}
}

// TestUnwrapEnvelopeStreamHandlesLastLineWithoutNewline
// SSE 最后一帧可能没有换行结尾 —— 那也要发出去，不能吞掉。
func TestUnwrapEnvelopeStreamHandlesLastLineWithoutNewline(t *testing.T) {
	out, err := io.ReadAll(unwrapEnvelopeStream(
		strings.NewReader(`data:{"body":"{\"choices\":[9]}"}`)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"choices":[9]`) {
		t.Errorf("末行（无换行结尾）被吞掉：%q", string(out))
	}
}

// ── 测试替身 ──────────────────────────────────────────────────────────────

// bytewiseReader 每次 Read 只给一个字节，强制切在行中间。
type bytewiseReader struct {
	data []byte
	i    int
}

func (r *bytewiseReader) Read(p []byte) (int, error) {
	if r.i >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.i]
	r.i++
	return 1, nil
}

// closeTrackingReader 记录 Close 是否被调用。
type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}
