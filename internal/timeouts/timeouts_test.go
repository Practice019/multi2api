package timeouts

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func intp(v int) *int { return &v }

// TestDefaults 把默认值钉死。默认值就是契约：它们必须与 new-api 的
// 缺省值（0 / 300 / 90 / 0）逐字一致，否则"默认对齐 new-api"这句话
// 会随着一次顺手改代码静默失效。
func TestDefaults(t *testing.T) {
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"relay", DefaultLimits().Relay, 0},
		{"streaming", DefaultLimits().Streaming, 300 * time.Second},
		{"idle_conn", DefaultLimits().IdleConn, 90 * time.Second},
		{"response_header", DefaultLimits().ResponseHeader, 0},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// 常量也一并钉住：Resolve 与 DefaultLimits 必须来自同一组源头，
	// 否则两条路径会漂移（例如只改了常量忘了改 DefaultLimits）。
	if DefaultLimits() != (Limits{
		Relay:          time.Duration(DefaultRelaySeconds) * time.Second,
		Streaming:      time.Duration(DefaultStreamingSeconds) * time.Second,
		IdleConn:       time.Duration(DefaultIdleConnSeconds) * time.Second,
		ResponseHeader: time.Duration(DefaultResponseHeaderSeconds) * time.Second,
	}) {
		t.Fatal("DefaultLimits 与 Default*Seconds 常量不一致")
	}
}

// TestResolveDefaults 空配置（键全缺席 → 全 nil）必须落到默认值。
func TestResolveDefaults(t *testing.T) {
	got, err := Config{}.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != DefaultLimits() {
		t.Fatalf("空配置的解析结果应等于 DefaultLimits()，got %+v", got)
	}
}

// TestZeroMeansUnlimited 是本包最核心的一条语义：
// **写了 0 = 不限，没写 = 用默认值**，两者必须可区分。
//
// 这条测试的存在理由：曾经想用 int 而不是 *int，那样 "0" 就只能有
// 一个含义 —— 要么老配置升级后丢掉 300s 的流式上限，要么永远表达不出
// "不限"。用这条测试把选择钉住。
func TestZeroMeansUnlimited(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		pick func(Limits) time.Duration
		want time.Duration
	}{
		{
			name: "streaming 显式 0 = 不限（不是回落 300）",
			cfg:  Config{StreamingSeconds: intp(0)},
			pick: func(l Limits) time.Duration { return l.Streaming },
			want: 0,
		},
		{
			name: "streaming 缺省 = 300",
			cfg:  Config{},
			pick: func(l Limits) time.Duration { return l.Streaming },
			want: 300 * time.Second,
		},
		{
			name: "idle_conn 显式 0 = 不限（不是回落 90）",
			cfg:  Config{IdleConnSeconds: intp(0)},
			pick: func(l Limits) time.Duration { return l.IdleConn },
			want: 0,
		},
		{
			name: "idle_conn 缺省 = 90",
			cfg:  Config{},
			pick: func(l Limits) time.Duration { return l.IdleConn },
			want: 90 * time.Second,
		},
		{
			name: "relay 显式 0 = 不限（与缺省同值，但意图不同）",
			cfg:  Config{RelaySeconds: intp(0)},
			pick: func(l Limits) time.Duration { return l.Relay },
			want: 0,
		},
		{
			name: "response_header 显式 0 = 不限",
			cfg:  Config{ResponseHeaderSeconds: intp(0)},
			pick: func(l Limits) time.Duration { return l.ResponseHeader },
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.Resolve()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d := tc.pick(got); d != tc.want {
				t.Fatalf("got %v, want %v", d, tc.want)
			}
		})
	}
}

// TestResolveExplicit 显式配置按秒换算，且四个字段互不串味。
func TestResolveExplicit(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want Limits
	}{
		{
			name: "全部显式",
			cfg: Config{
				RelaySeconds:          intp(120),
				StreamingSeconds:      intp(600),
				IdleConnSeconds:       intp(30),
				ResponseHeaderSeconds: intp(1800),
			},
			want: Limits{
				Relay:          120 * time.Second,
				Streaming:      600 * time.Second,
				IdleConn:       30 * time.Second,
				ResponseHeader: 1800 * time.Second,
			},
		},
		{
			name: "只配 relay，其余仍是默认",
			cfg:  Config{RelaySeconds: intp(45)},
			want: Limits{
				Relay:          45 * time.Second,
				Streaming:      300 * time.Second,
				IdleConn:       90 * time.Second,
				ResponseHeader: 0,
			},
		},
		{
			name: "大值不溢出（100 年）",
			cfg:  Config{RelaySeconds: intp(100 * 365 * 24 * 3600)},
			want: Limits{
				Relay:          100 * 365 * 24 * time.Hour,
				Streaming:      300 * time.Second,
				IdleConn:       90 * time.Second,
				ResponseHeader: 0,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.Resolve()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolveOverflowClamped 超过 ~292 年的秒数必须被钳成正的大值，
// 不能回绕成小超时。回绕的后果是"每个请求瞬间超时"，而现象会被误判成网络故障。
func TestResolveOverflowClamped(t *testing.T) {
	// 直接用 MaxInt 做输入：换算成 time.Duration 一定回绕。
	got, err := Config{RelaySeconds: intp(int(^uint(0) >> 1)), ResponseHeaderSeconds: intp(int(^uint(0) >> 1))}.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Relay <= 0 {
		t.Fatalf("relay 被回绕成 %v（应钳成正大值）", got.Relay)
	}
	if got.Relay < 100*365*24*time.Hour {
		t.Fatalf("relay 钳制后的值太小：%v", got.Relay)
	}
	if got.ResponseHeader <= 0 {
		t.Fatalf("response_header 被回绕成 %v（应钳成正大值）", got.ResponseHeader)
	}
}

// TestResolveNegativeRejected 负数必须在启动期报错，而不是被悄悄当成 0 或默认值。
func TestResolveNegativeRejected(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"relay", Config{RelaySeconds: intp(-1)}},
		{"streaming", Config{StreamingSeconds: intp(-1)}},
		{"idle_conn", Config{IdleConnSeconds: intp(-1)}},
		{"response_header", Config{ResponseHeaderSeconds: intp(-1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.cfg.Resolve(); err == nil {
				t.Fatal("负数应返回错误，实际为 nil")
			}
		})
	}
}

// TestJSONShape config.json 的键名是外部契约，改名等于让所有既有部署静默失效。
func TestJSONShape(t *testing.T) {
	const raw = `{
		"relay_timeout": 120,
		"streaming_timeout": 0,
		"idle_conn_timeout": 30,
		"response_header_timeout": 1800
	}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := c.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := Limits{
		Relay:          120 * time.Second,
		Streaming:      0, // 显式 0 必须保住了
		IdleConn:       30 * time.Second,
		ResponseHeader: 1800 * time.Second,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// 键缺席 → nil → 默认值；顺带验证 null 与缺席同义。
	var empty Config
	if err := json.Unmarshal([]byte(`{"relay_timeout": null}`), &empty); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if empty.RelaySeconds != nil {
		t.Fatalf("null 应解析为 nil，got %v", *empty.RelaySeconds)
	}
	got, err = empty.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != DefaultLimits() {
		t.Fatalf("null 配置应落到默认值，got %+v", got)
	}
}

// TestApplyTransport 两个连接相关超时必须落到 Transport 上，且连接池上限
// 与本仓既有硬编码（100 / 20）一致 —— 接上本包后连接池行为不能变。
func TestApplyTransport(t *testing.T) {
	tr := &http.Transport{}
	Limits{IdleConn: 45 * time.Second, ResponseHeader: 12 * time.Second}.ApplyTransport(tr)

	if tr.IdleConnTimeout != 45*time.Second {
		t.Errorf("IdleConnTimeout = %v, want 45s", tr.IdleConnTimeout)
	}
	if tr.ResponseHeaderTimeout != 12*time.Second {
		t.Errorf("ResponseHeaderTimeout = %v, want 12s", tr.ResponseHeaderTimeout)
	}
	if tr.MaxIdleConns != defaultMaxIdleConns {
		t.Errorf("MaxIdleConns = %d, want %d", tr.MaxIdleConns, defaultMaxIdleConns)
	}
	if tr.MaxIdleConnsPerHost != defaultMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", tr.MaxIdleConnsPerHost, defaultMaxIdleConnsPerHost)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 应为 true")
	}

	// nil 不应 panic：装配层漏传是可能的，让它静默无效比崩在启动路径上安全。
	var nilTr *http.Transport
	Limits{}.ApplyTransport(nilTr)
}

// TestApplyZeroDisables 0 传到 Transport 上还是 0 —— net/http 里 0 就是"不限"，
// 中间不能有任何把 0 翻译成默认值的环节（那会让"不限"永远配不出来）。
func TestApplyZeroDisables(t *testing.T) {
	tr := &http.Transport{}
	l, err := Config{
		IdleConnSeconds:       intp(0),
		ResponseHeaderSeconds: intp(0),
	}.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	l.ApplyTransport(tr)
	if tr.IdleConnTimeout != 0 {
		t.Errorf("IdleConnTimeout = %v, want 0（不限）", tr.IdleConnTimeout)
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %v, want 0（不限）", tr.ResponseHeaderTimeout)
	}
}

// TestApplyClient relay=0 时 Client.Timeout 必须是 0。
//
// 这是"relay 默认不限"的落点：一旦这里变成非 0，所有长流都会被掐，
// 而报错形态是客户端侧"连接被重置"，会让人去查上游。
func TestApplyClient(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want time.Duration
	}{
		{"缺省 = 0（不限）", Config{}, 0},
		{"显式 0 = 不限", Config{RelaySeconds: intp(0)}, 0},
		{"显式 90 = 90s", Config{RelaySeconds: intp(90)}, 90 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := tc.cfg.Resolve()
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			c := &http.Client{}
			l.ApplyClient(c)
			if c.Timeout != tc.want {
				t.Fatalf("Client.Timeout = %v, want %v", c.Timeout, tc.want)
			}
		})
	}
	var nilClient *http.Client
	Limits{Relay: time.Second}.ApplyClient(nilClient) // 不应 panic
}

// TestNewClient 构造函数：普通 client 有总时长，流式 client 一定没有。
//
// 流式那条是最容易写错的一行（用 Streaming 当 Client.Timeout）：
// 那样一个"持续吐数据但很慢"的正常长回答会在 300s 整被掐断。
func TestNewClient(t *testing.T) {
	l, err := Config{RelaySeconds: intp(120), StreamingSeconds: intp(300)}.Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	c := l.NewClient()
	if c.Timeout != 120*time.Second {
		t.Errorf("NewClient.Timeout = %v, want 120s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("Transport 类型 = %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != l.ResponseHeader {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, l.ResponseHeader)
	}
	if tr.IdleConnTimeout != l.IdleConn {
		t.Errorf("IdleConnTimeout = %v, want %v", tr.IdleConnTimeout, l.IdleConn)
	}

	s := l.NewStreamingClient()
	if s.Timeout != 0 {
		t.Errorf("NewStreamingClient.Timeout = %v, want 0（流式不设总时长）", s.Timeout)
	}
	str, ok := s.Transport.(*http.Transport)
	if !ok || str == nil {
		t.Fatalf("流式 Transport 类型 = %T, want *http.Transport", s.Transport)
	}
	// 流式 client 仍受响应头上限约束 —— 否则"上游收下连接永不应答"会永久泄漏。
	if str.ResponseHeaderTimeout != l.ResponseHeader {
		t.Errorf("流式 ResponseHeaderTimeout = %v, want %v", str.ResponseHeaderTimeout, l.ResponseHeader)
	}
}

// TestNewTransportKeepsDefaults 克隆 DefaultTransport 的意义：Proxy 环境变量、
// 拨号/TLS 握手超时这些缺省不能被"自己从零拼一个 Transport"抹掉 ——
// 抹掉的后果是某些部署（需要 HTTP_PROXY 出网）突然连不上上游。
func TestNewTransportKeepsDefaults(t *testing.T) {
	tr := DefaultLimits().NewTransport()
	if tr.Proxy == nil {
		t.Error("Proxy 丢失：应保留 http.ProxyFromEnvironment")
	}
	if tr.DialContext == nil {
		t.Error("DialContext 丢失：应保留默认拨号器（含 30s 超时与 keepalive）")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Errorf("TLSHandshakeTimeout = %v，应保留默认值", tr.TLSHandshakeTimeout)
	}
}

// TestValidate 手工构造的 Limits（绕过 Resolve）也要挡住负数。
func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		limits  Limits
		wantErr bool
	}{
		{"全零合法", Limits{}, false},
		{"正常值合法", Limits{Relay: time.Second, Streaming: time.Minute}, false},
		{"relay 负数", Limits{Relay: -time.Second}, true},
		{"streaming 负数", Limits{Streaming: -time.Second}, true},
		{"idle_conn 负数", Limits{IdleConn: -time.Second}, true},
		{"response_header 负数", Limits{ResponseHeader: -time.Second}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.limits.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestString 启动日志要把 0 显示成"不限"，而不是 "0s"。
// 否则运维会把"不限"读成"配错了、值是 0"。
func TestString(t *testing.T) {
	got := DefaultLimits().String()
	want := "relay=不限 streaming=5m0s idle_conn=1m30s response_header=不限"
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
