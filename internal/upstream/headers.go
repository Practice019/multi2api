// headers.go 构造上游请求头（common / chat / billing / refresh）。
//
// # 为什么它们是 *Client 的方法，而不是自由函数（本次改造）
//
// 改造前它们是 `func ChatHeaders(req, a)` 这样的自由函数，只能拿到账号，
// 拿不到**客户端级**的出站身份配置（UA 版本段、client_name、
// device_token、是否透传客户端 IP）。这些配置全都挂在 Client 上，
// 于是自由函数形态天然无法表达它们 —— 只能把值写死成常量。
//
// 改成方法之后，出站身份由**配置**决定（全部有"保持现状"的缺省值），
// 用户可以按部署环境对齐官方客户端形态，而不必改代码。
//
// # 缺省行为与改造前**逐字节一致**
//
// 所有新增能力都是 opt-in：
//
//	UserAgent 空 + ClientVersion 空  → UA = clientUA（改造前的常量，一字不变）
//	ClientName 空                    → 只设 X-Product: SaaS（改造前进的也是这一条）
//	DeviceToken/DeviceTokenFile 空   → 不注入 X-Device-Token
//	PassthroughIP false              → 不注入任何 IP 头
//
// 因此既有部署升级后出站请求**一个字节都不会变**。
package upstream

import (
	"net/http"

	"workbuddy2api/internal/auth"
)

const (
	// clientUA 缺省的出站 UA —— 官方 CLI 的形态。
	//
	// ⚠ 这是**保持现状**的值，不是"最新的"值。改动它属于风险控制敏感的变更
	// （上游可能按 UA 分桶），因此本包提供 `upstream.user_agent` 与
	// `upstream.client_version` 让使用者自己切换，而不是由本改造默默替所有人换掉。
	clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	// defaultClientVersion / defaultCliVersion 用于组装**桌面端三段式** UA：
	//
	//	WorkBuddy/<clientVersion> WorkBuddy/<clientVersion> CLI/<cliVersion>
	//
	// 对齐官方 WorkBuddy Desktop 分发包版本（5.5.4）与内置 CLI（2.137.1）。
	// 只在**显式配置了 client_version** 时才会走这条路 —— 见 Client.userAgent。
	defaultClientVersion = "5.5.4"
	defaultCliVersion    = "2.137.1"

	originRefererCN   = "https://www.codebuddy.cn"
	originRefererIntl = "https://www.workbuddy.ai"
)

// originRefererFor 按账号渠道选 Origin/Referer：
// 国际版（channel=intl）用 www.workbuddy.ai，国内版用 www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.Channel == auth.ChannelIntl {
		return originRefererIntl
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本段。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本段。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUA 组装桌面端三段式 UA。
//
// 两段相同的 `WorkBuddy/<ver>` 是**官方形态的逐字复刻**，不是笔误：
// 官方桌面端 RestOperations 层就是拼成 `WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1`。
// "看起来重复所以要合并"是一个很容易犯的错，会让 UA 与官方不一致。
func (c *Client) defaultWorkBuddyUA() string {
	return "WorkBuddy/" + c.clientVersion() + " WorkBuddy/" + c.clientVersion() +
		" CLI/" + c.cliVersion()
}

// userAgent 返回本次出站使用的 UA。
//
// # 三级优先
//
//	① Client.UserAgent 非空        → 逐字使用（配置完全接管）
//	② Client.ClientVersion 非空    → 桌面端三段式（见 defaultWorkBuddyUA）
//	③ 两者都空                     → clientUA（官方 CLI 形态，改造前的行为）
//
// ⚠ 第 ③ 条的措辞是"改造前的行为"，这是本条设计的核心约束：
// 未配置的部署**必须**发出与升级前完全相同的 UA。
func (c *Client) userAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	if c != nil && c.ClientVersion != "" {
		return c.defaultWorkBuddyUA()
	}
	return clientUA
}

// billingUA 白名单类（billing / checkin / banner）出站的单段 UA。
//
// 官方在 banner 请求上显式覆写成不带 CLI 段的形态。
// 只在**配置了 client_name** 时生效（即用户明确在对齐桌面端身份），
// 否则返回空串表示"不覆写" —— 保持改造前"billing 用默认 UA"的行为。
func (c *Client) billingUA() string {
	if c == nil || c.ClientName == "" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
//
// 优先级：每号 auth.DeviceToken > Client.DeviceToken（配置全局）> 文件兜底。
// 三者皆空/读失败 → 空串（调用方不注入该头，优雅降级）。
//
// # 为什么每号优先于全局
//
// 设备令牌是**账号级**的风控凭据（官方客户端一个账号一份）。
// 全局配置只是"所有号共用一个"的省事写法；一旦某个号有自己的值，
// 它显然比全局默认更准确。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token（仅当取到非空值）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent())
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
//
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）：
// 上游据此区分"这个字段确实为空"与"这个头根本没发"。
//
// clientIP 是本次请求的客户端 IP，**按参数传递**而不是读 Client 上的字段 ——
// 后者在并发下会串扰（A 请求写、B 请求读，B 的 IP 被 A 覆盖），
// 表现为上游看到一批莫名其妙的来源 IP。这也是 B 修过的同一个坑。
// PassthroughIP=false 或 clientIP 为空时不注入任何 IP 头。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string) {
	c.CommonHeaders(req, a)
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	// 用量归属头：ClientName 非空则四头跟随（对齐官方桌面端），
	// 空则保持 X-Product="SaaS"（改造前的行为）。
	c.injectAttribution(req)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
//
// # 它解决什么问题
//
// 官方控制台的「使用端」列靠这组头区分请求来自哪个客户端。
// 不注入时该列显示为空或 "SaaS"，无法确认流量确实走的是自己配置的桌面端身份。
//
// ClientName 非空时全量跟随该值，空则只保留 X-Product="SaaS"（旧行为，向后兼容）。
func (c *Client) injectAttribution(req *http.Request) {
	if c == nil || c.ClientName == "" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", c.ClientName)
	req.Header.Set("X-IDE-Type", c.ClientName)
	req.Header.Set("X-IDE-Version", c.clientVersion())
	req.Header.Set("X-Product", c.ClientName)
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 透传给上游（三等价头）。
//
// 三个头一起发是因为不同上游中间件读的名字不同（Nginx 读 X-Real-IP、
// 网关读 X-Forwarded-For、部分风控读 X-Client-IP），只发一个会有一半路径看不到。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 已迁移到 internal/gateway/clientip.go。
//
// # 为什么要迁走
//
// 它需要同时被 core（入站侧提取）与上游（出站侧注入）使用，
// 而 core **不得** import 具体上游（架构判据 3）。
// gateway 是两者唯一都能依赖的包，且本概念（通用 HTTP 头解析）
// 不含任何上游专有知识 —— 迁进 gateway 不违反判据 1。
//
// 用 `gateway.ExtractClientIP(r)` 取。

// BillingHeaders billing 接口请求头。
//
// UA 语义：默认**不设置**（保持 Go 客户端自带默认 UA，即改造前的行为）；
// 仅当显式配置 UserAgent 非空、或 client_name 配置了（走单段 billingUA）才覆写 ——
// 避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
	// 设备风控头：billing 域（report / travel / balance / checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}
