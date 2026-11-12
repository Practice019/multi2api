package logx

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// capture 把输出接到内存，并在结束时还原 —— 同时复位级别与采样表，
// 否则测试之间会互相污染（采样表是包级状态）。
func capture(t *testing.T, lvl Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	// ⚠ 保存"上一个 sink"再还原，而不是硬编码 stdout ——
	//   否则嵌套调用（capture 里再 capture）会把外层丢掉。
	outMu.Lock()
	prevSink := sink
	outMu.Unlock()
	SetSink(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }))
	old := SetLevel(lvl)
	t.Cleanup(func() {
		outMu.Lock()
		sink = prevSink
		outMu.Unlock()
		SetLevel(old)
		sampleMu.Lock()
		sample = map[string]*sampleState{}
		sampleMu.Unlock()
		now = time.Now
	})
	return &buf
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestLineFormat 钉住行格式：HH:MM:SS LEVEL scope msg（列对齐，可 grep）。
func TestLineFormat(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 12, 12, 41, 5, 0, time.UTC) }
	defer func() { now = time.Now }()
	got := Line(LevelWarn, "loomy", "额度查询失败 uid=2609…7497")
	want := "12:41:05 WARN  loomy      额度查询失败 uid=2609…7497"
	if got != want {
		t.Fatalf("Line() = %q\n      want %q", got, want)
	}
}

// TestLevelFilter 级别过滤：默认 INFO 时 DEBUG 不出；quiet(=ERROR) 时 WARN 也不出。
func TestLevelFilter(t *testing.T) {
	buf := capture(t, LevelInfo)
	Debugf("loomy", "这条不该出现")
	Infof("loomy", "这条该出现")
	if s := buf.String(); strings.Contains(s, "不该出现") {
		t.Fatalf("INFO 级下 DEBUG 行被放行了：%q", s)
	} else if !strings.Contains(s, "该出现") {
		t.Fatalf("INFO 行没打出来：%q", s)
	}
	buf.Reset()
	SetLevel(LevelError)
	Warnf("qoder", "WARN 在 quiet 下不该出现")
	Errorf("qoder", "ERROR 必须出现")
	s := buf.String()
	if strings.Contains(s, "不该出现") || !strings.Contains(s, "必须出现") {
		t.Fatalf("quiet 语义错误（应为只留 ERROR）：%q", s)
	}
}

// TestSampleZapSemantics 采样：首次、每 10 次、每 10 分钟 —— zap NewSampler 的语义。
func TestSampleZapSemantics(t *testing.T) {
	base := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	cur := base
	now = func() time.Time { return cur }
	defer func() { now = time.Now }()
	const key = "k"
	var printed []int
	for i := 1; i <= 21; i++ {
		cur = base.Add(time.Duration(i) * time.Second) // 21 秒内 → 不会触发 tick 心跳
		if n, ok := Sample(key, "网络超时"); ok {
			printed = append(printed, n)
		}
	}
	want := []int{1, 10, 20}
	if len(printed) != len(want) {
		t.Fatalf("放行次数 = %v，want %v", printed, want)
	}
	for i := range want {
		if printed[i] != want[i] {
			t.Fatalf("第 %d 次放行的计数 = %d，want %d", i+1, printed[i], want[i])
		}
	}
	// tick 心跳：距上次放行超过 sampleTick 必须再放行一条。
	cur = base.Add(21*time.Second + sampleTick)
	if n, ok := Sample(key, "网络超时"); !ok || n != 22 {
		t.Fatalf("超过 tick 应放行一条（计数 22），实际 n=%d ok=%v", n, ok)
	}
}

// TestOncefCounts 放行时附「连续 N 次」，且 key 不同互不干扰（不互相吃计数）。
func TestOncefCounts(t *testing.T) {
	base := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	cur := base
	now = func() time.Time { return cur }
	defer func() { now = time.Now }()
	buf := capture(t, LevelInfo)
	for i := 1; i <= 10; i++ {
		cur = base.Add(time.Duration(i) * time.Second)
		Oncef(LevelWarn, "qoder", "uid:A:refresh", "refresh_token 已失效 uid=%s", "A")
	}
	s := buf.String()
	if strings.Count(s, "refresh_token 已失效") != 2 {
		t.Fatalf("10 次调用应只放行 2 条（首次 + 第 10 次），实际:\n%s", s)
	}
	if !strings.Contains(s, "连续 10 次") {
		t.Fatalf("第 10 次应附「连续 10 次」，实际:\n%s", s)
	}
	// ⚠ 原因变了必须**立刻**放行（防"藏故障"）：同一 uid 从"网络超时"变成"登录已失效"，
	//   如果被采样折叠掉，用户就永远看不到真正的新原因。
	buf.Reset()
	Oncef(LevelWarn, "qoder", "uid:A:refresh", "refresh_token 已失效 uid=%s（原因变了）", "A")
	if !strings.Contains(buf.String(), "原因变了") {
		t.Fatalf("消息内容变了必须立刻放行，实际:\n%s", buf.String())
	}
	// 另一个 uid 必须独立计数（首次立刻放行）——否则就是"藏故障"。
	buf.Reset()
	Oncef(LevelWarn, "qoder", "uid:B:refresh", "refresh_token 已失效 uid=%s", "B")
	if !strings.Contains(buf.String(), "uid=B") {
		t.Fatalf("不同 uid 的首次必须放行，实际:\n%s", buf.String())
	}
}

// TestPipeScopeAndLevel 拦截 stdlib log：抽 scope、按级别过滤、去掉自带时间戳后不重复。
func TestPipeScopeAndLevel(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 12, 12, 41, 5, 0, time.UTC) }
	defer func() { now = time.Now }()
	buf := capture(t, LevelInfo)
	w := Pipe(LevelInfo)
	_, _ = w.Write([]byte("loomy: 面板预热一轮 —— 待办 0\n"))
	_, _ = w.Write([]byte("没有前缀的消息\n"))
	s := buf.String()
	if !strings.Contains(s, "INFO  loomy") || !strings.Contains(s, "面板预热一轮") {
		t.Fatalf("scope 抽取失败:\n%s", s)
	}
	if !strings.Contains(s, "INFO  app") || !strings.Contains(s, "没有前缀的消息") {
		t.Fatalf("无前缀消息应归 app:\n%s", s)
	}
	buf.Reset()
	SetLevel(LevelError)
	_, _ = w.Write([]byte("loomy: 这条在 quiet 下不该出\n"))
	if buf.Len() != 0 {
		t.Fatalf("级别过滤没生效:\n%s", buf.String())
	}
}

// Example 把"控制台长什么样"钉成可断言的契约（go test 会逐字节比对 Output）。
// ⚠ 这条同时是给人看的：它演示了级别列、scope 列，以及 DEBUG 默认不出。
func Example() {
	now = func() time.Time { return time.Date(2026, 11, 12, 12, 41, 3, 0, time.UTC) }
	defer func() { now = time.Now }()
	var buf bytes.Buffer
	outMu.Lock()
	prev := sink
	outMu.Unlock()
	SetSink(&buf)
	defer func() { outMu.Lock(); sink = prev; outMu.Unlock() }()
	Logf(LevelInfo, "raccoon", "续期 2 · 跳过 0 · 失败 0")
	Logf(LevelWarn, "loomy", "额度查询失败 uid=2609…7497 · 100003 账号已被停用 · 连续 12 次")
	Logf(LevelError, "qoder", "refresh_token 失效 uid=…a1f40e · 需重新登录/导入")
	Debugf("travel", "skip (daily limit reached) —— 默认不显示")
	fmt.Print(buf.String())
	// Output:
	// 12:41:03 INFO  raccoon    续期 2 · 跳过 0 · 失败 0
	// 12:41:03 WARN  loomy      额度查询失败 uid=2609…7497 · 100003 账号已被停用 · 连续 12 次
	// 12:41:03 ERROR qoder      refresh_token 失效 uid=…a1f40e · 需重新登录/导入
}

// TestBootWindowDemotesInfoButKeepsErrors 启动窗口：普通 INFO 细节降级，
// **含错误关键词的行必须照常可见**（否则"启动失败"会被降噪一起吞掉）。
func TestBootWindowDemotesInfoButKeepsErrors(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 11, 12, 13, 33, 11, 0, time.UTC) }
	defer func() { now = time.Now }()
	buf := capture(t, LevelInfo)
	w := Pipe(LevelInfo)

	BeginBoot(time.Minute)
	_, _ = w.Write([]byte("codearts: 已启用（凭证目录 auths\\codearts）\n"))
	if strings.Contains(buf.String(), "已启用") {
		t.Errorf("启动窗口内的普通 INFO 细节应降级（否则启动就是一屏配置倾倒）：%q", buf.String())
	}
	buf.Reset()
	_, _ = w.Write([]byte("codearts: 凭证目录读不到账号，本次启用失败\n"))
	if !strings.Contains(buf.String(), "失败") {
		t.Errorf("含错误关键词的行必须照常可见（降噪不能把启动故障一起藏掉）：%q", buf.String())
	}
	buf.Reset()
	EndBoot()
	_, _ = w.Write([]byte("codearts: 已启用（凭证目录 auths\\codearts）\n"))
	if !strings.Contains(buf.String(), "已启用") {
		t.Errorf("启动结束后 INFO 应恢复可见：%q", buf.String())
	}
}
