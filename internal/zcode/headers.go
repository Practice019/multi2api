// headers.go ZCode 的请求头构造。
//
// # 能抄则抄：本文件是参照实现的 field-by-field 移植
//
// 来源：D3-vin/Zcode2Api/internal/upstream/chatheaders.go（Go，MIT-less 但要署名）
//
//	以及官方 zai-org/ZCode/packages/shared/src/zcode-source-headers.ts
//
// 为什么值得逐字抄：**上游按请求头形态做风控**，而且**多发一个头就会触发**。
// 参照实现里那条实证警告是我们自己绝无可能推断出来的：
//
//	JWT (start-plan) channel rules from the reference:
//	  - x-api-key, x-query-id, x-session-id are NOT sent — the official
//	    start-plan client omits them; sending x-query-id/x-session-id here
//	    triggers upstream 3012 "unusual activity" (empirically).
//
// 换句话说：抄对了头 = 能用；自作聪明补齐"看起来该有的"头 = 3012 风控。
package zcode

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
)

// 官方来源头（抄自 zcode-source-headers.ts 的 ZCODE_SOURCE_HEADERS）。
const (
	hdrReferer   = "HTTP-Referer"
	hdrTitle     = "X-Title"
	hdrAppVer    = "X-ZCode-App-Version"
	hdrAgent     = "X-ZCode-Agent"
	hdrPlatform  = "X-Platform"
	hdrRelease   = "X-Release-Channel"
	hdrLang      = "X-Client-Language"
	hdrTimezone  = "X-Client-Timezone"
	hdrOSCat     = "X-Os-Category"
	hdrOSVersion = "X-Os-Version"
	hdrDeviceMid = "X-Device-Mid"
)

// 追踪头（抄自 runner-attribution.ts）。
const (
	hdrRequestID   = "x-request-id"
	hdrSessionType = "x-zcode-session-type"
	hdrTraceID     = "x-zcode-trace-id"
	// ⚠ 这两个**只在 API Key 通道发**，JWT 通道发了会触发 3012 风控。
	hdrQueryID   = "x-query-id"
	hdrSessionID = "x-session-id"
)

// 验证码头（只在 JWT 通道必需）。
const (
	hdrCaptchaParam  = "X-Aliyun-Captcha-Verify-Param"
	hdrCaptchaRegion = "X-Aliyun-Captcha-Verify-Region"
)

// 伪装身份常量（抄参照实现，逐字）。
//
// 上游按这些值判断"是不是官方客户端"。值本身没有秘密，
// 但它们必须**自洽**（比如 X-Os-Category 与 X-Platform 不能矛盾），
// 否则会显得像伪造 —— 参照实现用的是 Windows 桌面客户端的组合。
const (
	fakePlatform  = "win32-x64"
	fakeOSCat     = "windows"
	fakeOSVersion = "10.0.26200"
	fakeRelease   = "production"
	fakeLang      = "en-001"
	fakeTimezone  = "Europe/Bucharest"
	fakeAgent     = "glm"
	fakeTitle     = "Z Code@cli"
)

// chatHeaders 构造对话请求的头集合。
//
// extra 是调用方透传的头（本仓出口层可能带 anthropic-beta）；
// 只有明确在白名单里的才转发 —— 见 copyPassthrough。
//
// ⚠ 两条通道的头**不同**，不是"多一点少一点"的问题：
// JWT 通道多发 x-query-id / x-session-id 会直接触发 3012 风控。
// 所以这里按 kind 分支，而不是"统一发全集"。
func chatHeaders(a *Auth, extra http.Header) http.Header {
	h := http.Header{}

	// 透传白名单（参照实现的 drop-list 反向：只有它明确保留的才转发）。
	copyPassthrough(h, extra, "anthropic-beta")

	h.Set("Content-Type", "application/json")
	// 实测：两条协议端点都接受 Bearer（Anthropic 端点也接受 x-api-key）。
	h.Set("Authorization", "Bearer "+a.Token())
	h.Set("Accept", "*/*")

	// 身份集（顺序对齐参照实现的 identity.py）。
	h.Set(hdrReferer, planOrigin+"/")
	h.Set("User-Agent", "ZCode/"+defaultAppVersion)
	h.Set(hdrAppVer, defaultAppVersion)
	h.Set(hdrTitle, fakeTitle)
	h.Set(hdrAgent, fakeAgent)
	h.Set(hdrPlatform, fakePlatform)
	h.Set(hdrRelease, fakeRelease)
	h.Set(hdrLang, fakeLang)
	h.Set(hdrTimezone, fakeTimezone)
	h.Set(hdrOSCat, fakeOSCat)
	h.Set(hdrOSVersion, fakeOSVersion)

	// 设备指纹：**每个账号一个自己的**，不是全局共用派生值。
	// 共用会让上游把多个账号看成同一台设备（风控上很可疑）。
	mid := strings.TrimSpace(a.DeviceMid)
	if mid == "" {
		mid = deviceMidFor(a)
	}
	h.Set(hdrDeviceMid, mid)

	// 追踪三元组：**每个请求重新生成**（复用会让上游看成同一请求的重放）。
	h.Set(hdrRequestID, randomUUID())
	h.Set(hdrSessionType, "main")
	h.Set(hdrTraceID, randomUUID())

	if a.UsesJWT() {
		// ⚠ JWT 通道**只发**上面那三个追踪头。
		// 这里刻意不写 x-query-id / x-session-id —— 见文件头的实证警告。
		// 也刻意不发 x-api-key（JWT 通道用 Authorization 即可）。
		h.Set("anthropic-version", anthropicVersion)
	} else {
		// API Key 通道才是发 query/session 的那个（官方 coding-plan 客户端行为）。
		h.Set(hdrQueryID, randomUUID())
		h.Set(hdrSessionID, "sess_"+randomUUID())
	}
	return h
}

// copyPassthrough 只把 allow 里的头从 src 抄到 dst。
//
// 为什么是白名单而不是黑名单：出口层收到的头里混着**调用方的部署信息**
// （x-forwarded-for / via / cf-*）和**我们自己的鉴权**（authorization）。
// 原样转发等于把内网拓扑和密钥泄露给上游 —— 参照实现为此专门维护了
// _DROP_HEADER_PREFIXES，我们反过来做白名单，更不容易漏。
func copyPassthrough(dst, src http.Header, allow ...string) {
	if src == nil {
		return
	}
	for _, name := range allow {
		if v := src.Get(name); v != "" {
			dst.Set(name, v)
		}
	}
}

// deviceMidFor 为一个账号派生稳定的设备指纹。
//
// 判据：**同一账号每次都得是同一个值**（上游按它认设备），
// 但**不同账号必须不同**（否则多账号看起来是一台设备）。
// 用 UID 做种子满足这两条，且不需要额外持久化字段。
//
// 注意这**不是**随机 UUID：随机值每次请求都不同，等于每请求换一台新设备，
// 那比共用一个值更可疑。
func deviceMidFor(a *Auth) string {
	seed := strings.TrimSpace(a.UID)
	if seed == "" {
		seed = a.Token()
	}
	// 用哈希拼成 uuid v4 形态（8-4-4-4-12），保持与真实 uuid 同形。
	h := sha256.Sum256([]byte("zcode-device-mid:" + seed))
	b := h[:16]
	// 按 RFC 4122 打上版本/变体位，免得被识破不是 uuid。
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomUUID 生成一个 v4 UUID（追踪头用）。
//
// 用 crypto/rand 而不是 math/rand：追踪头会进上游日志，
// 可预测的值在风控上是个信号（也便于上游把我们的请求串起来）。
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退回一个固定串也比 panic 好 ——
		// 追踪头不是正确性所需，缺了不该让对话失败。
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// billingHeaders 计量端点的头（比对话少，但仍需身份集）。
func billingHeaders(a *Auth) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.Token())
	h.Set("Accept", "*/*")
	h.Set(hdrReferer, planOrigin+"/")
	h.Set("User-Agent", "ZCode/"+defaultAppVersion)
	h.Set(hdrAppVer, defaultAppVersion)
	h.Set(hdrTitle, fakeTitle)
	mid := strings.TrimSpace(a.DeviceMid)
	if mid == "" {
		mid = deviceMidFor(a)
	}
	h.Set(hdrDeviceMid, mid)
	return h
}
