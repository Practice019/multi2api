// Package timeouts 集中定义出站超时：relay（整请求）/ 流式（SSE 流中静默）/
// 连接池空闲 / 响应头。
//
// # 为什么要有这个包（而不是继续散在 internal/upstream 与 cmd/server）
//
// 改造前这四类超时分三处、全是硬编码，且其中两个值的语义彼此无关却常被当成一个：
//
//	internal/upstream/client.go  New()        Client.Timeout 120s / IdleConnTimeout 90s / ResponseHeaderTimeout 120s
//	cmd/server/config.go         normalize()  Upstream.IdleTimeoutSeconds 缺省 300（SSE 流中静默）
//
// 后果是运维无法按上游调：001 慢、003 会 78s+，一个写死的 120s 要么误掐，
// 要么（调大之后）让卡死的 goroutine 一直占着。new-api 把这四类做成环境变量
// （RELAY_TIMEOUT / STREAMING_TIMEOUT / RELAY_IDLE_CONN_TIMEOUT /
// RELAY_RESPONSE_HEADER_TIMEOUT，见 common/init.go:114-118），我们做成
// config.json 的一段 "timeouts"。
//
// # config.json 的形状
//
//	"timeouts": {
//	  "relay_timeout": 0,            // 秒；0 = 不限（缺省）
//	  "streaming_timeout": 300,      // 秒；缺省 300
//	  "idle_conn_timeout": 90,       // 秒；缺省 90
//	  "response_header_timeout": 0   // 秒；0 = 不限（缺省）
//	}
//
// 键名刻意不带 _seconds 后缀，单位在这里钉死为秒 —— 单位写在名字里会和
// 本仓既有的 timeout_seconds / header_timeout_seconds 混成两套命名。
package timeouts

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"time"
)

// 默认值刻意与 new-api 对齐（common/init.go:114-118 的缺省值）。
//
//	relay=0        不限，由上层定 —— 见 Limits.Relay（整请求上限会误伤长流）
//	streaming=300  与 cmd/server 里 Upstream.IdleTimeoutSeconds 的既有缺省一致，
//	               也就是说**这个值不改任何既有部署的行为**
//	idle_conn=90   与本仓 internal/upstream/client.go 的既有硬编码一致
//	response_header=0 不设上限（既有行为）。new-api 缺省是 1800，我们保守取 0：
//	               本仓短 RPC 本身有 120s 总时长兜底，把 1800 叠上去等于没设。
const (
	DefaultRelaySeconds          = 0
	DefaultStreamingSeconds      = 300
	DefaultIdleConnSeconds       = 90
	DefaultResponseHeaderSeconds = 0
)

// defaultMaxIdleConns / defaultMaxIdleConnsPerHost 连接池上限。
//
// # 为什么不放进 config
//
// 它们与"超时"不是一类问题：改它只会变吞吐，不会变"请求多久失败"。
// 取值与 internal/upstream/client.go 现有硬编码（100 / 20）逐字一致，
// 所以接上这个包之后**连接池行为不变**。
//
// # 为什么要显式设，不能全交给 http.DefaultTransport
//
// DefaultTransport 的 MaxIdleConnsPerHost 是 2 —— 同一个上游只能留 2 条
// 复用连接，多账号并发打同一个 host 时会退化成不停新建连接 + TLS 握手。
// 克隆 DefaultTransport 再覆盖，比从零拼一个 Transport 更安全（Proxy 环境变量、
// 30s 拨号超时、keepalive、10s TLS 握手超时、ExpectContinue 都被保留下来）。
const (
	defaultMaxIdleConns        = 100
	defaultMaxIdleConnsPerHost = 20
)

// maxSeconds 能安全换算成 time.Duration 的最大秒数（≈292 年）。
//
// # 这是为了挡一类真实事故（new-api service/http_client.go 的注释里记录过）
//
// time.Duration 是 int64 纳秒，秒数一旦超过约 292 年，
// "time.Duration(n) * time.Second" 会**回绕**成一个极小的正值（甚至负数）。
// 回绕后的正小值会让每一个请求瞬间超时 —— 那比"配错成不限"严重得多：
// 整个上游一条都通不过，而现象是"全部超时"，运维会去查网络，
// 不会想到是配置里的一个数字。所以这里是**钳制**而不是报错：
// 一个荒谬的大值，语义上就是"永远别超时"。
const maxSeconds int64 = math.MaxInt64 / int64(time.Second)

// Config 是 config.json 里 "timeouts" 那一段的外部输入形态。
//
// # 为什么字段是 *int 而不是 int
//
// 因为必须区分"没写"和"写了 0"：
//
//	没写   → 用默认值（streaming 一个没配的部署应该拿到 300s）
//	写了 0 → **不限**（streaming_timeout: 0 就是"我不要流式空闲上限"）
//
// 用 int 的话两者都是 0，只能二选一：要么"0 = 默认"（那么永远表达不出
// "不限"，也关不掉服务器端已在执行的 300s 上限），要么"0 = 不限"
// （那么缺省值 300 永远不生效，老配置升级后行为静默改变）。
// 指针是唯一能同时兼容两种意图的形态，encoding/json 也天然支持
// （键缺席或显式 null → nil）。
//
// ⚠ 语义一句话：**缺省 = 用默认值，写了 0 = 不限，负数 = 配置错误**。
// 这条规则有测试钉住（TestZeroMeansUnlimited）。
type Config struct {
	// RelaySeconds 整个请求（含读完响应体）的时长上限；0 = 不限。
	RelaySeconds *int `json:"relay_timeout"`
	// StreamingSeconds 流式响应**两次数据之间**允许的最长静默；0 = 不限。
	StreamingSeconds *int `json:"streaming_timeout"`
	// IdleConnSeconds 连接池里空闲连接的最长保留时间；0 = 永不回收。
	IdleConnSeconds *int `json:"idle_conn_timeout"`
	// ResponseHeaderSeconds 请求体写完到收到响应头之间的上限；0 = 不限。
	ResponseHeaderSeconds *int `json:"response_header_timeout"`
}

// Limits 是解析后的生效值，可直接套到 *http.Client / *http.Transport 上。
//
// 四个值互相独立，**不要合并**（这是最容易踩的坑，见每个字段的注释）。
//
//	Relay          → http.Client.Timeout
//	ResponseHeader → http.Transport.ResponseHeaderTimeout
//	IdleConn       → http.Transport.IdleConnTimeout
//	Streaming      → 不进任何 http 结构体，由 relay 层（SSE 读循环）自己监控
type Limits struct {
	// Relay 整请求上限，对应 http.Client.Timeout。
	//
	// # 为什么默认 0（不限）
	//
	// http.Client.Timeout 覆盖的是"从发起请求到读完响应体"的**全程**。
	// 一条合法的 SSE 长流可以跑几分钟到几十分钟，任何固定总时长都会在
	// 某个正常的慢回答上把连接掐断，而客户端看到的是"上游断流"——
	// 归因会错到上游头上。new-api 的 RelayTimeout 缺省同样是 0
	// （common/init.go:114），流式的边界交给 Streaming 与 ResponseHeader 各管一段。
	//
	// ⚠ 集成注意：本仓 internal/upstream/client.go 的 New() 里 Client.Timeout
	// 是硬编码 120s，它实际承担的是**短 RPC**（refresh/checkin/balance）的上限。
	// 直接用本包替换那 120s 会让短 RPC 失去上限（它们不流式，卡住就永远卡住）。
	// 两种接法都对，但必须选一个并写清楚：
	//   a) 配 relay_timeout: 120，保持既有行为；
	//   b) 短 RPC 的上限单独保留，relay 只作用于对话请求。
	Relay time.Duration
	// Streaming 流式响应两次数据之间允许的最长静默，0 = 不限。
	//
	// # 为什么不塞进 http.Client.Timeout
	//
	// 总时长管不了"卡住"：一个每分钟吐一个字节的流在总时长上限内永远合法，
	// 却会把 goroutine 和缓冲区一直占着。真正要限的是**静默间隔**，
	// 而那只能在读循环里量（本仓 internal/upstream/idle.go 的 monitorBody 就是干这个的）。
	//
	// # 与 IdleConn 的区别（名字像，语义完全无关）
	//
	//	IdleConn  = 连接池里**没人用**的连接多久回收（连接管理）
	//	Streaming = 一条正在读的流**多久没数据**就放弃（业务活性）
	Streaming time.Duration
	// IdleConn 连接池空闲连接回收时间，对应 http.Transport.IdleConnTimeout；0 = 不回收。
	IdleConn time.Duration
	// ResponseHeader 等响应头的上限，对应 http.Transport.ResponseHeaderTimeout；0 = 不限。
	//
	// # 为什么它必须与 Relay 分开（new-api common/constants.go:176-184 的原话）
	//
	// 上游有时会**收下连接但永不应答**，也不发 FIN/RST。没有这个上限时，
	// 那条 goroutine 和它持有的全部缓冲区（io.ReadAll 的原始 body、解码后的
	// 消息、重组后的上游 body）会活到进程结束 —— 泄漏是累积的、不可见的。
	//
	// 它只覆盖"等到响应头"这一段：头一到，后面的流式传输不受它影响。
	// 所以这个值可以很小也不误伤长流，这与 Relay 完全相反。
	ResponseHeader time.Duration
}

// DefaultLimits 返回缺省生效值（等价于 Config{} 的解析结果）。
func DefaultLimits() Limits {
	return Limits{
		Relay:          time.Duration(DefaultRelaySeconds) * time.Second,
		Streaming:      time.Duration(DefaultStreamingSeconds) * time.Second,
		IdleConn:       time.Duration(DefaultIdleConnSeconds) * time.Second,
		ResponseHeader: time.Duration(DefaultResponseHeaderSeconds) * time.Second,
	}
}

// Resolve 把外部配置（秒 + 指针）解析成生效的 Duration。
//
// 规则（缺省 / 显式 0 / 负数 的三分法）见 Config 的注释。
func (c Config) Resolve() (Limits, error) {
	var (
		l   Limits
		err error
	)
	if l.Relay, err = resolveSeconds(c.RelaySeconds, DefaultRelaySeconds, "relay_timeout"); err != nil {
		return Limits{}, err
	}
	if l.Streaming, err = resolveSeconds(c.StreamingSeconds, DefaultStreamingSeconds, "streaming_timeout"); err != nil {
		return Limits{}, err
	}
	if l.IdleConn, err = resolveSeconds(c.IdleConnSeconds, DefaultIdleConnSeconds, "idle_conn_timeout"); err != nil {
		return Limits{}, err
	}
	if l.ResponseHeader, err = resolveSeconds(c.ResponseHeaderSeconds, DefaultResponseHeaderSeconds, "response_header_timeout"); err != nil {
		return Limits{}, err
	}
	return l, nil
}

func resolveSeconds(p *int, def int, key string) (time.Duration, error) {
	if p == nil {
		return time.Duration(def) * time.Second, nil
	}
	secs := int64(*p)
	if secs < 0 {
		// 负数不收敛成 0（"不限"）也不收敛成默认值：两种都会把一个笔误
		// 变成一个**看起来正常**的配置。直接报错，让人在启动时就看见。
		return 0, fmt.Errorf("timeouts.%s: 不能为负数（%d）；要表示不限请写 0", key, secs)
	}
	if secs > maxSeconds {
		secs = maxSeconds
	}
	return time.Duration(secs) * time.Second, nil
}

// Validate 校验一个**手工构造**的 Limits（绕过 Config.Resolve 时用）。
//
// 负 Duration 在 net/http 里的行为不可依赖（不同版本可能当成 0 或立即超时），
// 所以宁可在装配阶段就拒绝，而不是让它在生产里表现成随机的超时。
func (l Limits) Validate() error {
	for _, f := range []struct {
		key string
		d   time.Duration
	}{
		{"relay_timeout", l.Relay},
		{"streaming_timeout", l.Streaming},
		{"idle_conn_timeout", l.IdleConn},
		{"response_header_timeout", l.ResponseHeader},
	} {
		if f.d < 0 {
			return fmt.Errorf("timeouts.%s: 不能为负数（%s）", f.key, f.d)
		}
	}
	return nil
}

// String 给启动日志用：把生效值打出来，省得运维去猜"到底哪个值生效了"。
// 0 一律显示为"不限"，避免把"没设上限"误读成"没配置"。
func (l Limits) String() string {
	return fmt.Sprintf("relay=%s streaming=%s idle_conn=%s response_header=%s",
		durText(l.Relay), durText(l.Streaming), durText(l.IdleConn), durText(l.ResponseHeader))
}

func durText(d time.Duration) string {
	if d == 0 {
		return "不限"
	}
	return d.String()
}

// NewTransport 构造一个与本仓既有行为一致的 Transport（保留 Proxy / 拨号 /
// TLS / HTTP2 缺省），再把两个与连接相关的超时应用上去。
func (l Limits) NewTransport() *http.Transport {
	tr := newBaseTransport()
	l.ApplyTransport(tr)
	return tr
}

// newBaseTransport 克隆 http.DefaultTransport：从零拼 Transport 会丢掉
// ProxyFromEnvironment、拨号超时、TLS 握手超时（理由同 defaultMaxIdleConns）。
// DefaultTransport 被替换成其他实现时，退回一份等价的手工配置。
func newBaseTransport() *http.Transport {
	if d, ok := http.DefaultTransport.(*http.Transport); ok && d != nil {
		return d.Clone()
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// ApplyTransport 把连接池相关的两个超时写到**既有** Transport 上。
//
// 存在的意义：调用方可能已经有一个 Transport（本仓 internal/upstream 就是
// "一个 Transport 给两个 Client 共用"），只想覆盖超时、不想换实例 ——
// 换实例等于丢掉池里已经在用的连接。
func (l Limits) ApplyTransport(tr *http.Transport) {
	if tr == nil {
		return
	}
	tr.MaxIdleConns = defaultMaxIdleConns
	tr.MaxIdleConnsPerHost = defaultMaxIdleConnsPerHost
	// 0 在 net/http 里本来就是"不限"，与本包的 0 语义天然一致，不需要翻译层。
	tr.IdleConnTimeout = l.IdleConn
	tr.ResponseHeaderTimeout = l.ResponseHeader
	// 与 new-api 一致：对公网直连走 HTTP/2，省一次握手。
	tr.ForceAttemptHTTP2 = true
}

// ApplyClient 把整请求上限写到既有 Client 上；0 = 不限。
func (l Limits) ApplyClient(c *http.Client) {
	if c == nil {
		return
	}
	c.Timeout = l.Relay
}

// NewClient 普通（非流式）出站 client：总时长 = Relay（0 = 不限）。
func (l Limits) NewClient() *http.Client {
	return &http.Client{Transport: l.NewTransport(), Timeout: l.Relay}
}

// NewStreamingClient 流式专用 client：**总时长恒为 0**，Streaming 不进这里。
//
// # 为什么不是"用 Streaming 当 Timeout"（这是最容易写错的一行）
//
// http.Client.Timeout 是**总时长**，而 Streaming 是**静默间隔**。拿它当总时长
// 会让一个持续吐数据、只是比较慢的正常长回答在 300s 整被掐断 ——
// 这正是 new-api 在 constants.go 里专门写一段注释强调的区别，
// 也是我们非要把"流式超时"与"relay 超时"分成两个字段的原因。
//
// 真正执行 Streaming 的地方是流读循环（本仓 internal/upstream/idle.go）。
//
// ⚠ 它自带一份 Transport，等于一套独立连接池。需要与普通 client 共用池时
// 别调这个方法：自己 NewTransport() 一次，然后
// "&http.Client{Transport: tr, Timeout: 0}"。
func (l Limits) NewStreamingClient() *http.Client {
	return &http.Client{Transport: l.NewTransport(), Timeout: 0}
}
