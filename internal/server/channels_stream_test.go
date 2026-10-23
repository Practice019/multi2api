// channels_stream_test.go 转发字节一致性（2026-10-20，T3 的护栏）。
//
// # 这条测试为什么最重要
//
// 把 SSE 解析器接进转发循环，风险不在"解析错"，而在"影响转发"：
// 少写一个字节、吞掉一个空行、把 flush 往后挪，客户端就会看到
// 内容缺失、分帧错乱、或者整块蹦出来 —— 而这些都不会报错。
//
// 所以这里的判据不是"能不能解析出 usage"，而是**写出的字节与输入逐字节相同**，
// 在这个前提下才允许解析生效。
package server

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// captureWriter 记录写出的字节与 flush 次数。
type captureWriter struct {
	hdr     http.Header
	status  int
	body    bytes.Buffer
	flushes int
}

func newCaptureWriter() *captureWriter { return &captureWriter{hdr: http.Header{}} }

func (c *captureWriter) Header() http.Header { return c.hdr }
func (c *captureWriter) Write(p []byte) (int, error) {
	return c.body.Write(p)
}
func (c *captureWriter) WriteHeader(s int) { c.status = s }
func (c *captureWriter) Flush()            { c.flushes++ }

// chunkReader 按预设分片返回数据，用来精确模拟 TCP 分片。
type chunkReader struct {
	chunks [][]byte
	i      int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	if n < len(r.chunks[r.i-1]) {
		// 剩余部分留到下一次（模拟 read 短读）
		r.chunks[r.i-1] = r.chunks[r.i-1][n:]
		r.i--
	}
	return n, nil
}

// mkForward 构造一个最小可用的 channelForward。
func mkForward(body io.Reader) channelForward {
	return channelForward{
		Stream:        gateway.ChatStream{Status: http.StatusOK, Body: io.NopCloser(body)},
		ChannelID:     "ch-test",
		ChannelName:   "csg",
		UpstreamModel: "m-up",
	}
}

// TestChannelStreamByteIdentity 写出的字节必须与输入逐字节相同。
//
// 覆盖三种最容易出问题的输入形态：
//
//	① 整块（含 usage，走末帧分支）
//	② 逐字节分片（最坏分片：UTF-8 多字节字符与 `\n\n` 都会被切断）
//	③ 无 usage（走估算分支，验证采集器不因解析失败而影响写出）
func TestChannelStreamByteIdentity(t *testing.T) {
	withUsage := sseJoin(
		`{"choices":[{"delta":{"content":"你好世界"}}]}`,
		`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`,
	)
	noUsage := sseJoin(
		`{"choices":[{"delta":{"content":"abc def"}}]}`,
		`[DONE]`,
	)

	cases := []struct {
		name       string
		input      string
		byteWise   bool // true ⇒ 逐字节分片
		wantTok    bool
		wantSource usageSource
	}{
		{"整块含 usage", withUsage, false, true, usageFromLastFrame},
		{"逐字节分片含 usage", withUsage, true, true, usageFromLastFrame},
		{"无 usage 走估算", noUsage, false, true, usageEstimated},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var body io.Reader
			if c.byteWise {
				chunks := make([][]byte, 0, len(c.input))
				for i := 0; i < len(c.input); i++ {
					chunks = append(chunks, []byte{c.input[i]})
				}
				body = &chunkReader{chunks: chunks}
			} else {
				body = bytes.NewReader([]byte(c.input))
			}

			w := newCaptureWriter()
			cap := newStreamUsage(time.Now().Add(-3 * time.Millisecond))
			h := &Handler{}
			h.writeChannelStream(w, &http.Request{}, mkForward(body), cap, false)

			// ★ 核心断言：字节完全一致
			if got := w.body.String(); got != c.input {
				t.Fatalf("转发字节与输入不一致\n  want %q\n  got  %q", c.input, got)
			}
			// 状态码透传
			if w.status != http.StatusOK {
				t.Fatalf("状态码应为 200，实际 %d", w.status)
			}
			// flush 必须发生过（否则客户端会整块收到）
			if w.flushes == 0 {
				t.Fatalf("必须调用过 Flush，实际 %d 次", w.flushes)
			}
			// 采集结果
			tok, _, src := cap.Result()
			if src != c.wantSource {
				t.Fatalf("usage 来源应为 %q，实际 %q", c.wantSource, src)
			}
			if c.wantTok && tok <= 0 {
				t.Fatalf("应采到 token > 0，实际 %d", tok)
			}
		})
	}
}

// TestChannelStreamNilCapture 采集器为 nil 时转发照常工作（防御性）。
//
// 为什么要这条：调用点将来可能有人传 nil（比如某个不关心指标的新路径），
// 那时**转发必须还正常** —— 指标是附加品，不能成为转发的前提。
func TestChannelStreamNilCapture(t *testing.T) {
	input := sseJoin(`{"choices":[{"delta":{"content":"x"}}]}`)
	w := newCaptureWriter()
	h := &Handler{}
	h.writeChannelStream(w, &http.Request{}, mkForward(bytes.NewReader([]byte(input))), nil, false)
	if got := w.body.String(); got != input {
		t.Fatalf("nil 采集器下转发字节应完全一致，got %q", got)
	}
}
