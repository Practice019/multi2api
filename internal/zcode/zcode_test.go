// zcode_test.go 协议常量与判定的守卫。
//
// # 这些断言为什么值得写（它们守的都是"抄来的事实"）
//
// 本包的协议细节来自**外部证据**（官方源码 + Go 参照实现 + 我的实测）。
// 那类事实有个共同风险：**看起来很合理所以没人再核对**。
// 比如"业务码在 error.code 里" —— 但本上游有两种错误信封，
// OpenAI 形态把码放在 `code`、Anthropic 形态放在 `type`。
// 只认一种的实现不会崩，它只是**永远漏判**，而漏判表现为
// "风控被记成成功"，那是本仓已经吃过一次的亏（TRAE 签到假成功）。
package zcode

import (
	"encoding/json"
	"net/http"
	"testing"
)

// errCode 必须同时认两种错误信封。
//
// 两种形态都是实测抓到的真实响应：
//
//	OpenAI 端点（api.z.ai/api/paas/v4）
//	  {"error":{"code":"1001","message":"Authentication parameter not received..."}}
//	Anthropic 端点（api.z.ai/api/anthropic）
//	  {"error":{"message":"Authentication parameter not received...","type":"1001"}}
//
// ⚠ 注意 code 与 type 的类型**不同**：一个是字符串一个是数字。
// 这就是为什么 parseIntish 要两种都收。
func TestErrCodeAcceptsBothEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "OpenAI 形态：code 是字符串",
			body: `{"error":{"code":"1001","message":"Authentication parameter not received in Header, unable to authenticate"}}`,
			want: 1001,
		},
		{
			name: "Anthropic 形态：码在 type，是数字",
			body: `{"error":{"message":"Authentication parameter not received in Header, unable to authenticate","type":"1001"}}`,
			want: 1001,
		},
		{
			name: "Anthropic 形态：type 是数字而非字符串",
			body: `{"error":{"message":"token expired or incorrect","type":401}}`,
			want: 401,
		},
		{
			name: "风控 3012（这是最要紧的那个码）",
			body: `{"error":{"code":"3012","message":"unusual activity"}}`,
			want: 3012,
		},
		{
			name: "计量端点的 4011（SERVICE_AUTHENTICATION_INVALID）",
			body: `{"error":{"message":"SERVICE_AUTHENTICATION_INVALID","type":"4011"}}`,
			want: 4011,
		},
		{
			name: "code 优先于 type（两者都在时）",
			body: `{"error":{"code":"1005","type":"1113"}}`,
			want: 1005,
		},
		{
			name: "不是 JSON → 0（不 panic）",
			body: `<html>502 Bad Gateway</html>`,
			want: 0,
		},
		{
			name: "空体 → 0",
			body: ``,
			want: 0,
		},
		{
			name: "code 为 null → 0",
			body: `{"error":{"code":null,"type":null}}`,
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errCode([]byte(tc.body)); got != tc.want {
				t.Fatalf("errCode() = %d，期望 %d\nbody: %s", got, tc.want, tc.body)
			}
		})
	}
}

// errMessage 要能取到人类可读的说明（排障日志靠它）。
func TestErrMessageFromBothEnvelopes(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"error":{"code":"1001","message":"Authentication parameter not received"}}`, "Authentication parameter not received"},
		{`{"error":{"message":"token expired or incorrect","type":"401"}}`, "token expired or incorrect"},
		{`{"error":{"message":"Header中未收到Authorization参数，无法进行身份验证。","type":"1001"}}`, "Header中未收到Authorization参数，无法进行身份验证。"},
		{`not json`, ""},
	}
	for _, tc := range cases {
		if got := errMessage([]byte(tc.body)); got != tc.want {
			t.Fatalf("errMessage() = %q，期望 %q", got, tc.want)
		}
	}
}

// 业务码分类必须与官方 failure-provider-business-codes.ts 一致。
//
// ⚠ 这个分类**直接决定账号池的处置**：
//
//	终止类 → 换号（重试只是重复失败，还会白烧额度）
//	可重试 → 原地重试（过早摘号会浪费一个好号）
//
// 抄错任何一个都会导致"号池里明明有号却一直失败"或"好号被摘"。
func TestBusinessCodeClassification(t *testing.T) {
	terminal := []int{
		codeAuthRequired,        // 3007 验证码/鉴权
		codeQuotaExhausted,      // 1005 配额耗尽
		codeProviderMissing,     // 1006 provider 未配置
		codeInsufficientBalance, // 1113 余额不足
		codeModelNotFound,       // 3006 模型不存在
		codeContextTooLong,      // 1261 上下文超限
		codeRateLimitedHard,     // 1304 硬限流
	}
	for _, c := range terminal {
		if !isTerminalCode(c) {
			t.Errorf("业务码 %d 应为终止类（不该重试）", c)
		}
	}

	retryable := []int{
		codeNetworkRetry,       // 1234
		codeRateLimited,        // 1302
		codeProviderOverloaded, // 1312
		codeServerError,        // 2007
		codeConcurrency,        // 3008 并发上限
		codeRiskControl,        // 3012 风控（冷却后可重试）
		0,                      // 未知码不该被判成终止
	}
	for _, c := range retryable {
		if isTerminalCode(c) {
			t.Errorf("业务码 %d 不该是终止类 —— 过早摘号会浪费好号", c)
		}
	}
}

// 额度类业务码的判定。
//
// 这两种码的处置是"换号"，与"限流"（原地等）完全不同。
func TestQuotaCodeDetection(t *testing.T) {
	for _, c := range []int{codeQuotaExhausted, codeInsufficientBalance} {
		if !isQuotaCode(c) {
			t.Errorf("业务码 %d 应被认成额度耗尽", c)
		}
	}
	// 风控**不是**额度问题 —— 把它当额度会去换号，而真正该换的是出口。
	if isQuotaCode(codeRiskControl) {
		t.Error("3012 风控不该被判成额度耗尽 —— 该冷却/换出口，不是换号")
	}
	if isQuotaCode(codeRateLimited) {
		t.Error("限流不该被判成额度耗尽")
	}
}

// parseIntish 的边界。
func TestParseIntish(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`1001`, 1001},
		{`"1001"`, 1001},
		{`0`, 0},
		{`"0"`, 0},
		{``, 0},
		{`null`, 0},
		{`"null"`, 0},
		{`"abc"`, 0},
		{`"1001abc"`, 0},
		{`-5`, -5},
		{`3012`, 3012},
	}
	for _, tc := range cases {
		if got := parseIntish(json.RawMessage(tc.in)); got != tc.want {
			t.Errorf("parseIntish(%s) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

// 端点常量必须与实测一致。
//
// ⚠ 这些值**被实测否证过一次**（我原以为 Coding Plan 有 OpenAI 形态，实际 404），
// 所以把它们钉在测试里 —— 将来有人"顺手修正"成看似合理的值时会立刻变红。
func TestEndpointConstants(t *testing.T) {
	if pathPaaS != "/api/paas/v4" {
		t.Errorf("通用 OpenAI 端点路径变了：%s（实测为 /api/paas/v4）", pathPaaS)
	}
	if pathCodingPaaS != "/api/coding/paas/v4" {
		t.Errorf("Coding Plan 端点路径变了：%s（实测为 /api/coding/paas/v4）", pathCodingPaaS)
	}
	if planAnthropicPath != "/api/v1/zcode-plan/anthropic/v1/messages" {
		t.Errorf("JWT 通道端点变了：%s", planAnthropicPath)
	}
	// JWT 通道**只有 Anthropic 形态**（实测 paas/v4 与 v1/chat/completions 都 404）。
	// 这条断言守的是"别再把 OpenAI 形态加回来"。
	if contains(planAnthropicPath, "paas") || contains(planAnthropicPath, "chat/completions") {
		t.Error("JWT 通道端点是 Anthropic 协议 —— 实测 OpenAI 形态不存在（404）")
	}
	if originZAI != "https://api.z.ai" {
		t.Errorf("Z.ai origin 变了：%s", originZAI)
	}
	if originBigModel != "https://open.bigmodel.cn" {
		t.Errorf("BigModel origin 变了：%s", originBigModel)
	}
	if planOrigin != "https://zcode.z.ai" {
		t.Errorf("Coding Plan origin 变了：%s", planOrigin)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// JWT 通道的追踪头规则：**只发三元组**，不发 x-query-id / x-session-id。
//
// # 这条断言为什么最重要
//
// 参照实现里的实证警告（逐字）：
//
//	JWT (start-plan) channel rules from the reference:
//	  - x-api-key, x-query-id, x-session-id are NOT sent — the official
//	    start-plan client omits them; sending x-query-id/x-session-id here
//	    triggers upstream 3012 "unusual activity" (empirically).
//
// 也就是说：**多发两个"看起来该有"的头会触发风控**。
// 这是自己绝无可能推断出来的规则，只能靠抄 —— 所以必须钉住。
func TestJWTChannelOmitsRiskTriggeringHeaders(t *testing.T) {
	a := &Auth{Kind: CredKindJWT, JWT: "jwt-token", UID: "u1"}
	h := chatHeaders(a, nil)

	// 必须不发（发了触发 3012 风控）。
	for _, bad := range []string{hdrQueryID, hdrSessionID, "x-api-key"} {
		if v := h.Get(bad); v != "" {
			t.Errorf("JWT 通道不该发 %s（值为 %q）—— "+
				"参照实现实证：发 x-query-id/x-session-id 会触发上游 3012 风控", bad, v)
		}
	}

	// 必须发的追踪三元组。
	for _, need := range []string{hdrRequestID, hdrSessionType, hdrTraceID} {
		if h.Get(need) == "" {
			t.Errorf("JWT 通道缺少追踪头 %s", need)
		}
	}
	if got := h.Get(hdrSessionType); got != "main" {
		t.Errorf("x-zcode-session-type = %q，期望 main", got)
	}
	if got := h.Get("anthropic-version"); got != anthropicVersion {
		t.Errorf("JWT 通道的 anthropic-version = %q，期望 %q", got, anthropicVersion)
	}
	// 鉴权必须是 Bearer（实测 Anthropic 端点接受 Bearer）。
	if got := h.Get("Authorization"); got != "Bearer jwt-token" {
		t.Errorf("Authorization = %q，期望 Bearer jwt-token", got)
	}
}

// API Key 通道才发 query/session 头。
func TestAPIKeyChannelSendsQueryAndSession(t *testing.T) {
	a := &Auth{Kind: CredKindAPIKey, APIKey: "k1.k2", UID: "u1"}
	h := chatHeaders(a, nil)
	for _, need := range []string{hdrRequestID, hdrSessionType, hdrTraceID, hdrQueryID, hdrSessionID} {
		if h.Get(need) == "" {
			t.Errorf("API Key 通道缺少头 %s", need)
		}
	}
	if got := h.Get(hdrSessionID); len(got) < 6 || got[:5] != "sess_" {
		t.Errorf("x-session-id 应以 sess_ 开头，实际 %q", got)
	}
	// API Key 通道**不该**发 anthropic-version（它走 OpenAI 协议）。
	if got := h.Get("anthropic-version"); got != "" {
		t.Errorf("API Key 通道走 OpenAI 协议，不该发 anthropic-version（实际 %q）", got)
	}
}

// 每个请求的追踪 id 必须**不同**（复用会被上游看成同一请求的重放）。
func TestTraceIDsAreRegeneratedPerRequest(t *testing.T) {
	a := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u1"}
	h1 := chatHeaders(a, nil)
	h2 := chatHeaders(a, nil)
	if h1.Get(hdrRequestID) == h2.Get(hdrRequestID) {
		t.Error("两次请求的 x-request-id 相同 —— 追踪头必须每请求重新生成")
	}
	if h1.Get(hdrTraceID) == h2.Get(hdrTraceID) {
		t.Error("两次请求的 x-zcode-trace-id 相同 —— 追踪头必须每请求重新生成")
	}
	if !looksLikeUUID(h1.Get(hdrRequestID)) {
		t.Errorf("x-request-id 不是 uuid 形态：%q", h1.Get(hdrRequestID))
	}
}

// 设备指纹：**同账号稳定、不同账号不同**。
//
// # 为什么两条都要守
//
//	同账号不稳定 → 每次请求像一台新设备（比共用一个值更可疑）
//	不同账号相同 → 上游把多个账号看成同一台设备（风控信号）
//
// 参照实现的原话：X-Device-Mid 是 "the account's own persisted uuid4
// ('a fresh install on this machine' per account), not a shared/derived id."
func TestDeviceMidIsStablePerAccountAndDistinctAcrossAccounts(t *testing.T) {
	a1 := &Auth{Kind: CredKindAPIKey, APIKey: "k1", UID: "u1"}
	a2 := &Auth{Kind: CredKindAPIKey, APIKey: "k2", UID: "u2"}

	m1, m1b := deviceMidFor(a1), deviceMidFor(a1)
	if m1 != m1b {
		t.Error("同一账号两次派生的 device-mid 不同 —— 上游会把它看成新设备")
	}
	m2 := deviceMidFor(a2)
	if m1 == m2 {
		t.Error("不同账号的 device-mid 相同 —— 上游会把多个账号看成同一台设备")
	}
	if !looksLikeUUID(m1) {
		t.Errorf("device-mid 不是 uuid 形态：%q", m1)
	}
	// 显式设置的 device_mid 必须被尊重（用户可能从官方客户端拷过来）。
	a3 := &Auth{Kind: CredKindAPIKey, APIKey: "k3", UID: "u3", DeviceMid: "11111111-2222-4333-8444-555555555555"}
	if got := chatHeaders(a3, nil).Get(hdrDeviceMid); got != "3" && got != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("显式 device_mid 未被尊重，实际 %q", got)
	}
}

// looksLikeUUID 检查 8-4-4-4-12 形态。
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return false
			}
		}
	}
	return true
}

// 身份头必须自洽（平台/系统类别/UA 不能互相矛盾）。
//
// 为什么：上游按这些值判断"是不是官方客户端"。矛盾的组合比缺字段更可疑。
func TestIdentityHeadersAreSelfConsistent(t *testing.T) {
	a := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u1"}
	h := chatHeaders(a, nil)

	if got := h.Get("User-Agent"); got != "ZCode/"+defaultAppVersion {
		t.Errorf("User-Agent = %q，期望 %q", got, "ZCode/"+defaultAppVersion)
	}
	if got := h.Get(hdrAppVer); got != defaultAppVersion {
		t.Errorf("X-ZCode-App-Version = %q，期望 %q", got, defaultAppVersion)
	}
	// win32-x64 必须配 windows —— 这两个矛盾就是明显的伪造信号。
	plat, oscat := h.Get(hdrPlatform), h.Get(hdrOSCat)
	if plat != "win32-x64" || oscat != "windows" {
		t.Errorf("平台组合不自洽：X-Platform=%q X-Os-Category=%q（期望 win32-x64 / windows）", plat, oscat)
	}
	if got := h.Get(hdrReferer); got != "https://zcode.z.ai/" {
		t.Errorf("HTTP-Referer = %q，期望 https://zcode.z.ai/", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

// 透传是**白名单**，不是黑名单。
//
// # 为什么要守这条
//
// 出口层收到的头里混着调用方的**部署信息**（x-forwarded-for / via / cf-*）
// 和**我们自己的鉴权**。原样转发等于把内网拓扑与密钥泄露给上游。
func TestPassthroughIsAllowlistNotBlocklist(t *testing.T) {
	in := http.Header{}
	in.Set("anthropic-beta", "prompt-caching-2024-07-31")
	// 这些**绝不该**被转发出去。
	in.Set("X-Forwarded-For", "10.0.0.5")
	in.Set("Authorization", "Bearer caller-key")
	in.Set("Cookie", "session=secret")
	in.Set("Via", "1.1 internal-proxy")
	in.Set("CF-Connecting-IP", "203.0.113.9")
	in.Set("User-Agent", "some-client/1.0")

	a := &Auth{Kind: CredKindAPIKey, APIKey: "real-key", UID: "u1"}
	h := chatHeaders(a, in)

	if got := h.Get("anthropic-beta"); got != "prompt-caching-2024-07-31" {
		t.Errorf("白名单头 anthropic-beta 未被透传：%q", got)
	}
	for _, leaked := range []string{"X-Forwarded-For", "Via", "CF-Connecting-IP", "Cookie"} {
		if got := h.Get(leaked); got != "" {
			t.Errorf("部署信息 %s 被泄露给上游（值 %q）", leaked, got)
		}
	}
	// Authorization 必须是**我们的**，不是调用方的。
	if got := h.Get("Authorization"); got != "Bearer real-key" {
		t.Errorf("Authorization 被调用方的值覆盖了：%q（必须用我们自己的凭证）", got)
	}
	if got := h.Get("User-Agent"); got != "ZCode/"+defaultAppVersion {
		t.Errorf("User-Agent 被调用方的值覆盖了：%q", got)
	}
}
