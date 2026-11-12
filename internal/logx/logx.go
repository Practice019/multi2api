// Package logx 是本进程的日志出口：级别 + 去重采样 + 统一行格式。
//
// # 设计对标（成熟项目的既有做法，不是自创）
//
//  1. **级别** DEBUG/INFO/WARN/ERROR —— 与 Go 标准库 log/slog、zap、logr 同一套语义，默认 INFO。
//  2. **重复消息采样** 照 zap 的 NewSampler(core, tick, first, thereafter)：每个 key 在 tick 内
//     先放行 first 条，之后每 thereafter 条放行一条。本包取 first=1 / thereafter=10 / tick=10min，
//     放行时附「连续 N 次」。⚠ 本仓实测 97.3% 的日志行是同一 (上游,uid,原因) 的重复，
//     这是那一类的正解，而不是散落的 if 判断。
//  3. **轮询/无变化的报告不配 INFO**，进 DEBUG（Caddy 的 log_level、Traefik 的 level 同一取舍）。
//  4. **访问日志（每请求一行）走独立流**，不经本包 —— nginx/Envoy/Caddy 都这么分：
//     表格行有列结构，混进级别前缀反而没法读。
//
// # 与 stdlib log 的关系：拦截，而不是推翻
//
// 本仓有 362 处 log.Printf（跨 28 个包）。全部改写不现实、且是一次高风险大重构。
// 做法：main 里 log.SetFlags(0) + log.SetOutput(logx.Pipe(...)) ⇒ 既有调用点立刻获得
// 统一前缀与级别过滤；再按音量把最吵的十几处逐步迁到显式级别/采样。
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level 数值越大越严重（与 slog 的排序一致）。
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelName = map[Level]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

// String 让级别能直接进日志文案（%s）。
func (l Level) String() string {
	if n := levelName[l]; n != "" {
		return strings.ToLower(n)
	}
	return "info"
}

var current atomic.Int32

func init() { current.Store(int32(LevelInfo)) }

// ParseLevel 认 debug/info/warn/error/quiet（大小写不敏感）。
// quiet = 只留 ERROR：桌面场景"别烦我"的正确语义不是全关，真错误仍然要出。
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "verbose", "trace":
		return LevelDebug, nil
	case "", "info", "normal":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error", "quiet":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("认不出的日志级别 %q（可写 debug/info/warn/error/quiet）", s)
}

// SetLevel 设置全局级别，返回旧值（测试与自证用）。
func SetLevel(l Level) Level { return Level(current.Swap(int32(l))) }

// LevelNow 当前级别。
func LevelNow() Level { return Level(current.Load()) }

// Enabled 该级别当前是否放行。
func Enabled(l Level) bool { return l >= LevelNow() }

// now 可被测试替换（假时钟）。
var now = time.Now

var outMu sync.Mutex

// Line 组装一行：HH:MM:SS LEVEL scope message
// ⚠ 只留时分秒：控制台是"看着现在发生什么"的流；跨天追溯用文件 / JSONL。
func Line(l Level, scope, msg string) string {
	name := levelName[l]
	if name == "" {
		name = "INFO"
	}
	if scope == "" {
		scope = "app"
	}
	if len(scope) > 10 {
		scope = scope[:10]
	}
	return fmt.Sprintf("%s %-5s %-10s %s", now().Format("15:04:05"), name, scope, msg)
}

// Logf 按级别输出一行（scope 通常是上游/子系统名，便于 grep）。
func Logf(l Level, scope, format string, args ...any) {
	if !Enabled(l) {
		return
	}
	write(Line(l, scope, fmt.Sprintf(format, args...)))
}

// Debugf / Infof / Warnf / Errorf 是 Logf 的便捷入口。
func Debugf(scope, format string, args ...any) { Logf(LevelDebug, scope, format, args...) }
func Infof(scope, format string, args ...any)  { Logf(LevelInfo, scope, format, args...) }
func Warnf(scope, format string, args ...any)  { Logf(LevelWarn, scope, format, args...) }
func Errorf(scope, format string, args ...any) { Logf(LevelError, scope, format, args...) }

// sink 显式输出目标（测试用 SetSink/CaptureTo 设置）。nil = 跟随 stdlib log 的 Writer。
var sink io.Writer

// piped：main 是否已经把 stdlib log 接到本包（Pipe）。接了就不能再跟随 log.Writer()，
// 否则 logx 写 → log 写 → logx 写，自环。
var piped atomic.Bool

func write(line string) {
	// ⚠ 默认**跟随 stdlib log 的 Writer**：迁移期间有 6 处老测试用
	//   `log.SetOutput(&buf)` 捕获日志 —— 跟随之后它们自动抓得到 logx 的行，
	//   不用逐个改（改漏一个就是假红或假绿）。
	w := sink
	if w == nil {
		if piped.Load() {
			w = os.Stdout
		} else {
			w = log.Writer()
		}
	}
	outMu.Lock()
	defer outMu.Unlock()
	_, _ = io.WriteString(w, line+"\n")
}

// SetSink 换输出目标（测试用；生产不调）。
func SetSink(w io.Writer) { outMu.Lock(); defer outMu.Unlock(); sink = w }

// ---------------------------------------------------------------------------
// 采样（去重）：zap NewSampler 的语义，first=1 / thereafter=10 / tick=10min
// ---------------------------------------------------------------------------

const (
	sampleFirst      = 1
	sampleThereafter = 10
	sampleTick       = 10 * time.Minute
)

type sampleState struct {
	n       int
	last    time.Time
	lastMsg string
}

var (
	sampleMu sync.Mutex
	sample   = map[string]*sampleState{}
)

// Sample 判定 key 这次要不要放行；放行时返回累计次数（供调用方写「连续 N 次」）。
//
// 四条规则，缺一条都会退化（要么继续刷屏、要么把故障藏起来）：
//
//	· 第一次一定放行 —— 否则首次故障看不见；
//	· ⚠⚠ **内容变了立刻放行** —— 这是防"藏故障"的那一条：同一个 uid 从
//	  「网络超时」变成「登录已失效」，消息文本就变了，必须立刻再报一次。
//	  没有它，采样会把"原因变了"的新故障一起折叠掉（比刷屏坏得多）。
//	· 每满 sampleThereafter 放行一条 —— 故障持续时仍看得见它在继续；
//	· 距上次放行超过 sampleTick 放行一条（心跳）—— 用来区分"还在报"与"已经好了"。
//
// msg 传"格式化后的正文"，用于内容比对（不含后面追加的「连续 N 次」）。
func Sample(key, msg string) (int, bool) {
	sampleMu.Lock()
	defer sampleMu.Unlock()
	s := sample[key]
	if s == nil {
		s = &sampleState{}
		sample[key] = s
	}
	s.n++
	t := now()
	switch {
	case s.n == sampleFirst:
	case msg != s.lastMsg:
	case s.n%sampleThereafter == 0:
	case t.Sub(s.last) >= sampleTick:
	default:
		return s.n, false
	}
	s.last, s.lastMsg = t, msg
	return s.n, true
}

// ResetSample 清掉某个 key 的采样状态（测试用；故障恢复后也用它让下次重新"首次放行"）。
func ResetSample(key string) {
	sampleMu.Lock()
	defer sampleMu.Unlock()
	delete(sample, key)
}

// Oncef 重复消息降噪：首次、每 N 次、每 tick 各放行一条，放行时附累计次数。
//
// 用法（key 要把"同一件事"的因素都编进去）：
//
//	logx.Oncef(logx.LevelWarn, "qoder", "uid:"+uid+":refresh",
//		"refresh_token 已失效 uid=%s · 需重新登录/导入", uid)
//
// ⚠ key 少了因素（例如只写原因、不写 uid）会让不同账号互相吃掉计数 ——
//
//	那是"藏故障"，比刷屏更坏。
func Oncef(l Level, scope, key, format string, args ...any) {
	if !Enabled(l) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	// ⚠ 先采样（比对的 msg 不含「连续 N 次」后缀），再决定是否加计数后缀并输出。
	n, print := Sample(key, msg)
	if !print {
		return
	}
	if n > 1 {
		msg = fmt.Sprintf("%s · 连续 %d 次", msg, n)
	}
	write(Line(l, scope, msg))
}

// ---------------------------------------------------------------------------
// 拦截 stdlib log：让 362 处既有 log.Printf 立刻获得统一前缀与级别过滤
// ---------------------------------------------------------------------------

// scopePrefix 从既有消息里抽 `xxx: ` 的 xxx 当 scope（抽不到归 app）。
// 本仓既有格式绝大多数是 log.Printf("loomy: …") / log.Printf("codearts: …")，
// 所以这一条解析就把 scope 补齐了，**不用动那 362 处**。
var scopePrefix = regexp.MustCompile("^([A-Za-z][A-Za-z0-9_.-]{1,15}):[ \t]+")

// pipeWriter 把 stdlib log 的一行（已 SetFlags(0)，不再自带时间前缀）转成本包格式。
type pipeWriter struct{ lvl Level }

func (p pipeWriter) Write(b []byte) (int, error) {
	if !Enabled(p.lvl) {
		return len(b), nil
	}
	s := strings.TrimRight(string(b), "\r\n")
	scope, msg := "app", s
	if m := scopePrefix.FindStringSubmatch(s); m != nil {
		scope, msg = m[1], s[len(m[0]):]
	}
	write(Line(p.lvl, scope, msg))
	return len(b), nil
}

// Pipe 给 log.SetOutput 用：把 stdlib log 的输出接进本包（按 lvl 归级）。
// ⚠ 调用方必须同时 log.SetFlags(0)，否则 stdlib 会自带 `2026/11/12 12:05:15 ` 前缀，
//
//	与本包的 HH:MM:SS 叠成两段时间。
func Pipe(lvl Level) io.Writer {
	piped.Store(true) // 见 write：接了 Pipe 就不能再跟随 log.Writer()（自环）
	return pipeWriter{lvl: lvl}
}

// PrintfWriter 给"只认 Printf 的第三方日志器"用 ——
// GORM 的 logger.Writer 就是这个形状（GORM 自己会 new 一个 log.Logger，绕开 SetOutput）。
type PrintfWriter struct{ lvl Level }

// Printf 实现 GORM 的 logger.Writer 接口。
func (p PrintfWriter) Printf(format string, args ...any) {
	Logf(p.lvl, "sql", fmt.Sprintf(format, args...))
}

// SQLWriter 返回给 gorm logger.New 用的 writer（见 internal/store/db.go 的 gormConfig）。
func SQLWriter(lvl Level) PrintfWriter { return PrintfWriter{lvl: lvl} }

// CaptureTo 测试专用：把 logx 的输出接到 w、级别临时放到 DEBUG（让测试看到全部），
// 返回还原函数（调用方 defer 它）。
//
// ⚠ 为什么需要它：迁移到 logx 之后，老的"只换 log.SetOutput"捕获器**抓不到**这些行了 ——
//
//	表现是测试莫名断言空串（假红），或者反过来抓不到就等于没测（假绿）。
//	本函数与 log.SetOutput 配对使用，两个 sink 一起捕获。
func CaptureTo(w io.Writer) func() {
	outMu.Lock()
	prevSink := sink
	outMu.Unlock()
	prevLvl := SetLevel(LevelDebug)
	SetSink(w)
	return func() {
		outMu.Lock()
		sink = prevSink
		outMu.Unlock()
		SetLevel(prevLvl)
	}
}

// LF 供调用方拼接（避免各处散落字面量）。
const LF = "\n"
