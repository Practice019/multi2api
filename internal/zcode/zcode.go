// zcode.go ZCode（Z.ai / 智谱 GLM Coding Plan）的协议常量与纯函数。
//
// # 事实来源（能抄则抄：逐字来自官方源码 + Go 参照实现）
//
// 本包的协议事实**全部来自一手证据**，不是逆向猜测：
//
//	官方源码   zai-org/ZCode（Apache-2.0，7433★，TypeScript monorepo）
//	           config/provider/zcode-builtin.json   —— 端点与协议类型
//	           packages/shared/src/zcodeEndpoint.ts —— URL 构造
//	           packages/shared/src/zcode-source-headers.ts —— 来源头全集
//	Go 参照    D3-vin/Zcode2Api/internal/upstream/chatheaders.go
//	           field-by-field 移植的头集合（含实证的风控警告）
//	           genevatrkassulkusc82-collab/zcode-proxy
//	           三协议双向转换 + 验证码求解（go-rod）
//	我的实测   以下每个端点都用真实 HTTP 请求验过存在性与鉴权形态
//
// # 为什么走 API Key 通道而不是 Anthropic 转换层
//
// 参照项目 zcode-proxy 写了 **27KB 的 Anthropic→OpenAI 转换层**
// （zp_convert.go，44 处 delta 处理）。我们**不需要**它 ——
// 因为 Z.ai 同时提供 **OpenAI 兼容端点**（实测确认存在）：
//
//	https://api.z.ai/api/paas/v4/chat/completions         通用
//	https://api.z.ai/api/coding/paas/v4/chat/completions  Coding Plan
//
// 本网关的 internal/wire 层已假定上游是 OpenAI 格式，所以走这条路
// **省掉整个转换层**，且与其它 10 个上游保持同一形状。
//
// 官方 builtin config 里 `api.type` 一律是 `anthropic-messages`，
// 但那描述的是**官方客户端自己怎么调**，不是上游只支持什么 ——
// 实测两条协议都在，且都接受 `Bearer` 与 `x-api-key`。
package zcode

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// 端点 origin（全部实测存在）。
const (
	// originZAI Z.ai 开放平台（国际版）。
	originZAI = "https://api.z.ai"
	// originBigModel 智谱开放平台（国内版）。
	originBigModel = "https://open.bigmodel.cn"
)

// OpenAI 兼容端点路径（本包走这条）。
const (
	// pathPaaS 通用 OpenAI 兼容端点。实测：POST 无鉴权 → 401 code 1001。
	pathPaaS = "/api/paas/v4"
	// pathCodingPaaS Coding Plan 专用 **API Key** 通道的 OpenAI 兼容端点。
	//
	// ⚠ 官方文档明示它与 pathPaaS **不可互换、不互相消耗额度**：
	// Coding Plan 订阅的额度只走这条，按量付费只走 pathPaaS。
	//
	// ⚠ 别把它与 JWT 通道混了：JWT 通道（planAnthropicPath）是另一回事，
	// 只有 Anthropic 协议。见下面 planAnthropicPath 的实测对照。
	pathCodingPaaS = "/api/coding/paas/v4"
)

// 业务错误码（抄自官方 failure-provider-business-codes.ts）。
//
// 官方源码里它们被分成"终止不重试"与"可重试"两类 —— 这个区分是**必要的**：
// 把终止类当可重试会白烧额度（比如验证码失败重试 3 次只是重复失败），
// 把可重试类当终止会过早摘号（比如限流）。
const (
	// codeAuthRequired 需要验证码/鉴权刷新 —— **终止，不重试**。
	//
	// 官方注释直说：`3007 验证码/鉴权 → 需 AuthRefresh（终止不重试）`。
	codeAuthRequired = 3007
	// codeConcurrency 并发上限（3008/3009/3010）—— 限流，不自动重试。
	codeConcurrency = 3008
	// codeRiskControl 风控（3012 "unusual activity"）。
	//
	// ⚠ 这是本上游**最容易踩的坑**：多发一个不该发的头就会触发它。
	// 见 chatHeaders 里关于 x-query-id / x-session-id 的警告。
	codeRiskControl = 3012
	// codeQuotaExhausted 配额耗尽（1005）—— 终止，应换号。
	codeQuotaExhausted = 1005
	// codeProviderMissing provider 未配置（1006）。
	codeProviderMissing = 1006
	// codeInsufficientBalance 余额/资源包不足（1113）—— 终止，换号。
	codeInsufficientBalance = 1113
	// codeModelNotFound 模型不存在（3006）—— 终止。
	codeModelNotFound = 3006
	// codeContextTooLong 上下文超限（1261）—— 终止。
	codeContextTooLong = 1261
	// codeNetworkRetry 网络错误（1234）—— 可重试。
	codeNetworkRetry = 1234
	// codeRateLimited 限流（1302/1303/1305）—— 可重试。
	codeRateLimited = 1302
	// codeRateLimitedHard 限流（1304/1308/1310/1313）—— 终止不重试。
	codeRateLimitedHard = 1304
	// codeProviderOverloaded provider 过载（1312）—— 可重试。
	codeProviderOverloaded = 1312
	// codeServerError 服务端错误（2007）—— 可重试。
	codeServerError = 2007
	// codeInvalidFlow 登录会话无效（3004）。
	//
	// 实测：`GET /api/v1/oauth/cli/poll/<不存在的 flow>` → HTTP 400
	// `{"code":3004,"msg":"invalid_flow"}`。
	//
	// 它是**终态**而不是可重试错误 —— 会话已过期或服务重启过，
	// 重试一万次都一样。归错类会让用户对着一个永远转不完的圈等下去。
	codeInvalidFlow = 3004
)

// 凭证形态。ZCode 有两条**互斥**的凭证路径，本包都支持。
const (
	// CredKindAPIKey API Key 通道（`{apiKey}.{secretKey}` 或裸 apiKey）。
	//
	// 端点走 pathCodingPaaS / pathPaaS，鉴权用 `Bearer` 或 `x-api-key`。
	// **不需要验证码** —— 这是本包 v1 的主路径。
	CredKindAPIKey = "api-key"
	// CredKindJWT Coding Plan JWT 通道（OAuth 登录得到）。
	//
	// 端点走 zcode.z.ai 的 plan 网关，鉴权用 `Bearer <jwt>`，
	// 且**每个请求都要带一个新的阿里云验证码参数**（见 captcha）。
	CredKindJWT = "jwt"
)

// planOrigin Coding Plan 的 JWT 网关 origin（实测存在）。
const planOrigin = "https://zcode.z.ai"

// planAnthropicPath JWT 通道的对话端点路径。
//
// ⚠ **本包实测否证了一个想当然的假设**：Z.ai 的公开 API 有 OpenAI 兼容端点，
// 但 Coding Plan 的 JWT 网关**只有 Anthropic 协议**。实测对照：
//
//	/api/v1/zcode-plan/paas/v4/chat/completions   → 404  ← 不存在，别猜
//	/api/v1/zcode-plan/v1/chat/completions        → 404  ← 不存在
//	/api/v1/zcode-plan/anthropic/v1/messages      → 401  ← 这才是它
//	/api/v1/off-peak/anthropic/v1/messages        → 401
//	/api/v1/ultra-zai/anthropic/v1/messages       → 401  ← 服务端改写目标
//	/api/v1/ultra/anthropic/v1/messages           → 401
//
// 所以 JWT 通道**必须走 Anthropic 协议**（需要协议转换），
// API Key 通道才是 OpenAI 原生（不需要转换）。两条路的成本不同，见 provider.go。
const planAnthropicPath = "/api/v1/zcode-plan/anthropic/v1/messages"

// 计量与配额端点。
//
// ⚠ 实测修正：它们是 **GET**，不是 POST（POST 时返回 404，会误判成"端点不存在"）。
const (
	planBillingCurrentPath = "/api/v1/zcode-plan/billing/current"
	planBillingBalancePath = "/api/v1/zcode-plan/billing/balance"
)

// anthropicVersion Anthropic 协议的版本头（走 Anthropic 端点时才需要）。
const anthropicVersion = "2023-06-01"

// defaultAppVersion 伪装用的客户端版本。
//
// 抄自参照实现（zcode2api constants.py 的 CLIENT_APP_VERSION）。
// 上游按 User-Agent 形态做风控，所以这个值要与官方客户端一致。
const defaultAppVersion = "3.14.4"

// errEnvelope 上游的错误信封。
//
// ⚠ 本上游的失败**有两种形态**，两种都要按业务码判：
//
//	{"error":{"code":"1001","message":"..."}}   OpenAI 形态（code 是**字符串**）
//	{"error":{"message":"...","type":"1001"}}   Anthropic 形态（code 在 type，**数字**）
//
// 只认一种会漏判 —— 这是本包 distinct 于其它上游的地方。
type errEnvelope struct {
	Error struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Type    json.RawMessage `json:"type"`
	} `json:"error"`
}

// errCode 从错误信封里取出业务码（两种形态都认，取不到返回 0）。
//
// code 与 type 都可能是 JSON 字符串或数字，所以统一走 RawMessage 再剥引号。
func errCode(body []byte) int {
	var env errEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return 0
	}
	if c := parseIntish(env.Error.Code); c != 0 {
		return c
	}
	return parseIntish(env.Error.Type)
}

// errMessage 从错误信封里取出人类可读的说明（取不到返回空串）。
func errMessage(body []byte) string {
	var env errEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return strings.TrimSpace(env.Error.Message)
}

// parseIntish 把 JSON 里的数字或字符串数字解析成 int。
//
// 为什么必须两种都收：同一套上游在两个协议下的错误体里，
// 业务码一个是 `"1001"`（带引号）一个是 `1001`（不带）。
func parseIntish(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0
	}
	n := 0
	neg := false
	for i, r := range s {
		if i == 0 && r == '-' {
			neg = true
			continue
		}
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	if neg {
		return -n
	}
	return n
}

// base64URLDecode 解 JWT 的 payload 段（无填充的 base64url）。
//
// 为什么单独写而不用 base64.RawURLEncoding 直接调：JWT 的段**可能**带
// 填充（有些实现会加 "="），RawURLEncoding 遇到填充会报错。
// 这里统一去掉填充 —— 只为一处调用点引入两种解码尝试不值得。
func base64URLDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// isTerminalCode 该业务码是否属于"终止，不该重试"（抄官方分类）。
//
// 这个分类直接决定账号池的处置：终止类应**换号**，可重试类应**原地重试**。
func isTerminalCode(code int) bool {
	switch code {
	case codeAuthRequired, codeQuotaExhausted, codeProviderMissing,
		codeInsufficientBalance, codeModelNotFound, codeContextTooLong,
		codeRateLimitedHard:
		return true
	}
	return false
}

// isQuotaCode 该业务码是否表示"这个号的额度没了"（应换号）。
func isQuotaCode(code int) bool {
	switch code {
	case codeQuotaExhausted, codeInsufficientBalance:
		return true
	}
	return false
}

// isRiskControlCode 该业务码是否是风控（3012）。
//
// 风控通常是**出口 IP 或请求头形态**的问题，不是账号本身坏了 ——
// 所以处置是"冷却并换出口"，而不是"禁用账号"。
func isRiskControlCode(code int) bool {
	return code == codeRiskControl
}
