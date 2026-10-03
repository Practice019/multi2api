// stream_frames.go 逐帧读取上游 SSE —— 供"边流边判断"的场景使用。
//
// # 为什么需要它（而不是让调用方自己 bufio 扫行）
//
// SSE 的帧形态判据在本包里已经有**唯一**的一份实现（sseData：同时认
// `data:` 与 `data: `，见它的注释 —— 那是实测抓到的真 bug）。
// 调用方自己再扫一遍行，就是把这个判据复制成两份；而"两处各写一遍"
// 正是那个 bug 当初的成因（Aggregate 与 Stream 对同一份帧形态
// 给过不同的答案）。
//
// 所以这里只做**读取与切帧**，判据仍复用 sseData / normalizeFrame，
// 让"什么是一帧合法数据"永远只有一个答案。
package wire

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Frame 上游流里的一帧。
type Frame struct {
	// Raw 原始 payload 文本（`data:` 之后、去掉前导空格）。
	//
	// ⚠ 保留原文而不只留解析结果：流内错误信封需要**原文**喂给上游自己的
	// 分类器（见 InBandError 的注释）。重建过的字段会丢判据。
	Raw string
	// Obj 解析后的对象。JSON 解析失败时为 nil（此时 Raw 仍可用）。
	Obj map[string]any
	// Normalized 规范化后的对象（白名单重建）。
	//
	// 转给客户端时用它，而不是原始 Obj —— 上游的额外字段不该直接漏出去。
	// Obj 为 nil（非 JSON）时本字段也是 nil。
	Normalized map[string]any
	// IsDone 这一帧是不是 `[DONE]` 终止标记。
	IsDone bool
	// NonData 非 data 行（注释、event: 等）。此时 Raw 是**整行**（含换行）。
	NonData bool
}

// FrameScanner 逐帧读取上游 SSE。
//
// 用法：
//
//	sc := wire.NewFrameScanner(body)
//	for {
//	    f, err := sc.Next()
//	    if err == io.EOF { break }
//	    …
//	}
//
// # 与 Stream 的分工
//
//	Stream        单纯透传（不需要看内容决定行为时用它）
//	FrameScanner  调用方要**逐帧做判断**时用它（例如"这一帧里有
//	              工具调用吗？有就得拦住，别透传"）
type FrameScanner struct {
	br *bufio.Reader
}

// NewFrameScanner 包一个上游流。
func NewFrameScanner(r io.Reader) *FrameScanner {
	return &FrameScanner{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next 读下一帧。
//
// 返回 (nil, io.EOF) 表示流正常结束。空行（帧分隔）被吞掉 ——
// 调用方自己产出 `\n\n`。
func (s *FrameScanner) Next() (*Frame, error) {
	for {
		line, err := s.br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")

		if payload, isData := sseData(trimmed); isData {
			if strings.TrimSpace(payload) == "[DONE]" {
				// ⚠ 不在这里返回 EOF：调用方需要知道"上游显式结束了"，
				// 以便区分"正常收尾"与"流被截断"（后者要日志）。
				return &Frame{Raw: payload, IsDone: true}, nil
			}
			f := &Frame{Raw: payload}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) == nil {
				f.Obj = obj
				f.Normalized = normalizeFrame(obj)
			}
			return f, nil
		}

		// 非 data 行：注释/其它。原样带回，调用方决定是否透传。
		if trimmed != "" {
			return &Frame{Raw: line, NonData: true}, nil
		}

		// 空行（帧分隔）与其他空内容：吞掉，继续读。
		if err != nil {
			return nil, err
		}
	}
}

// InBandErrorOf 判定一帧是否是**流内错误信封**（导出包装）。
//
// # 为什么导出而不让调用方自己判
//
// 判据（哪种字段形态算错误、什么情况算正常数据帧带诊断字段）是本包的
// 私有知识，而且它的第一版**曾经判错过**（只认顶层 error，漏了华为系的
// error_code/error_msg，见 inBandErrorOf 的注释）。
//
// 工具循环要逐帧看内容，就必须能问"这帧是错误吗" —— 让它自己写一套
// 判据等于把那个教训复制成两份，下次改一边就会漂移。
//
// raw 传 frame.Raw（**原文**）而不是重建对象：分类器需要看到上游原样字段。
func InBandErrorOf(obj map[string]any, raw string) (*InBandError, bool) {
	return inBandErrorOf(obj, raw)
}
