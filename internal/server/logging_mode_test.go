// logging_mode_test.go 钉住「模式」列的语义（2026-10-20）。
//
// 这条测试的价值在于**它曾经会红**：改造前渠道分支把 mode 写成字面量 "sync"，
// 无论客户端请求体 stream 是 true 还是 false，日志里都是 sync。
// 用户看到自己发的是流式却显示非流式，只能凭截图来问「应该不是非流式吧」——
// 这正是"没有测试的字段迟早会漂"的实例。
package server

import "testing"

func TestChannelLogModeFollowsClientIntent(t *testing.T) {
	cases := []struct {
		name   string
		stream bool
		want   string
	}{
		{"客户端要流式", true, "stream"},
		{"客户端要非流式", false, "sync"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := channelLogMode(c.stream); got != c.want {
				t.Fatalf("stream=%v 应记 %q，实际 %q", c.stream, c.want, got)
			}
		})
	}
}

// TestChannelLogModeMatchesPoolPath 两条路径的取值必须同一套词汇。
//
// 为什么单独钉这条：模式值会被同一个聚合函数读（按 mode 分组的流式占比）。
// 如果渠道路径写 "streaming" 而账号池写 "stream"，分母会莫名少一半，
// 而这种错在页面上只表现为"流式占比偏低"，没人能一眼看出是词汇分叉。
func TestChannelLogModeMatchesPoolPath(t *testing.T) {
	// 账号池路径的两种取值（logging.go 里的 mode 变量）
	poolValues := map[string]bool{"sync": true, "stream": true}
	for _, stream := range []bool{true, false} {
		got := channelLogMode(stream)
		if !poolValues[got] {
			t.Fatalf("渠道路径产生 %q，不在账号池的词汇表 {sync, stream} 内 —— 两条路径写入同一 JSONL，词汇必须一致", got)
		}
	}
}
