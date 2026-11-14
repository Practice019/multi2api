// client.go PhanthyWork 的 HTTP 出口层：OAuth 续期 / 设备注册 /
// activities/summary 换推理令牌 / v1/messages 调模型 / 模型目录。
//
// # 分层（报告 §7.2 推崇的那个分层）
//
//	crypto.go  纯函数：三条 canonical、sha256、ed25519、时钟规整
//	client.go  这一层：**只管把请求发出去**。它不认识账户、不认识账号池，
//	           唯一的输入是一份 *Auth 与它自己报的偏斜
//	provider.go 接缝：把上面的东西拼成 gateway.Provider
//
// 于是"哪个请求要带哪几个头"这件事只有一处真相（本文件的 applyV1 /
// proofFor），而"签名算得对不对"能在 crypto_test.go 里离线逐字节断言。
//
// # 端点清单（全部来自报告，逐字）
//
//	GET  /api/oauth/profile                          取 account.uuid（签名要用的 accountScope）
//	POST /oauth/token                                OAuth 换 token 与续期（**两处编码不同**，见下）
//	POST /api/oauth/desktop-installations/register    设备注册（**写操作**，一次性）
//	POST /api/oauth/activities/summary                换推理令牌（TTL 180s）+ 返回 server_time
//	GET  /api/phanthy_code/models                    模型目录（只带 Bearer）
//	POST /v1/messages                                 调模型（12 段 proof 签名）
//
// # 两处最容易搞错的编码差异（照抄会错）
//
//	登录换 token（authorization_code）：**form-urlencoded**，20 秒超时，不重试
//	续期（refresh_token）：**JSON body**，8 秒超时，3 次重试（仅 429/5xx）
//
// 另外两个 expires_in 缺省值**故意不同**（1800 / 3600），照抄会错。
package phanthywork

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/gateway"
)

// Client PhanthyWork 的 HTTP 客户端。
//
// 三个可注入字段（测试用 httptest 指向假上游）：HTTP / APIBase / UA。
type Client struct {
	HTTP *http.Client
	// APIBase 上游基址（默认 DefaultAPIBase）。
	//
	// ⚠ 出站 URL 只能拼在它下面 —— 报告 §1.2 引开源实现的 origin.mjs：
	// 「一次拼装错误就把账号送给了别的主机，而这类错**不会报错**：
	//   请求成功、日志干净、用户只是从此收不到额度。」
	APIBase string
	// UA User-Agent。**必须是 Bun/1.3.14**（坑②），否则 403 TM.00000001。
	UA string
	// skewMS 本机与「服务端」的偏移（毫秒，本机 - 服务端）。
	//
	// ⚠ 只在**登录流程**里用（那时还没有账号对象）；拿到账号之后一律走
	// Auth.serverNowMS（**按账号隔离**）。两条路径共用同一个口径。
	skewMS atomic.Int64
}

// New 生产默认值。
func New() *Client {
	return &Client{
		// 不设总超时：SSE 流不能被"总时长"掐断 —— 逐请求设超时。
		HTTP:    &http.Client{Timeout: 0},
		APIBase: DefaultAPIBase,
		UA:      DefaultUA,
	}
}

// NewWithBase 测试构造：把基址指向假上游（UA 保持官方值 ——
// 坑② 的断言需要它）。
func NewWithBase(base string) *Client {
	c := New()
	c.APIBase = base
	return c
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// apiBase 生效的上游基址（去尾斜杠）。
func (c *Client) apiBase() string {
	if c != nil && strings.TrimSpace(c.APIBase) != "" {
		return strings.TrimRight(strings.TrimSpace(c.APIBase), "/")
	}
	return DefaultAPIBase
}

// userAgent 生效的 UA。
//
// ⚠ 空串也要回落到 DefaultUA：UA 缺失同样命中"第三方客户端"判据
// （Node 默认 UA 实测 403），所以"没配"不能等于"不发"。
func (c *Client) userAgent() string {
	if c != nil && strings.TrimSpace(c.UA) != "" {
		return strings.TrimSpace(c.UA)
	}
	return DefaultUA
}

// skew 当前客户端口径的偏移（登录流程用）。
func (c *Client) skew() int64 {
	if c == nil {
		return 0
	}
	return NormalizeClockSkewMS(c.skewMS.Load())
}

// setSkew 设定客户端偏移（由 server_time 校准）。
func (c *Client) setSkew(v int64) {
	if c == nil {
		return
	}
	c.skewMS.Store(NormalizeClockSkewMS(v))
}

// serverNowMS 客户端口径的服务端毫秒时间（登录流程用）。
func (c *Client) serverNowMS() int64 { return serverNowMS(nowMS(), c.skew()) }

// baselineSkewMS 给新建的凭证用的时钟基线。
func (c *Client) baselineSkewMS() int64 { return c.skew() }

// 两个 expires_in 兜底值**故意不同**（报告 §4.2 的对照表）：
//
//	登录换 token（performExchange）：服务端没给时用 1800
//	续期（refresh_token）：服务端没给时用 3600
//
// 来自官方 sidecar 的逐字反编译 —— 照抄任一侧的兜底值都会错：
// 续期那条用的是 3600，而登录那条是 1800（刷出来的 token 通常更长）。
const (
	// exchangeDefaultExpiresIn 登录换 token 的兜底有效期（秒）。
	exchangeDefaultExpiresIn = 1800
	// refreshDefaultExpiresIn 续期的兜底有效期（秒）。
	refreshDefaultExpiresIn = 3600
)

// ── 通用请求 ─────────────────────────────────────────────────────────────

// doJSON 发一次请求并把响应读到内存，返回 (状态码, 响应体)。
//
// 非 2xx **不当作传输错误**：调用方要拿 (status, body) 去做分类与错误文案
// （与 gateway.Provider.Chat 的语义一致）。
func (c *Client) doJSON(req *http.Request, timeout time.Duration) (int, []byte, error) {
	ctx := req.Context()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	resp, err := c.httpClient().Do(req.WithContext(ctx))
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// 16 MiB 上限（与上游的 body 上限同量级，够任何 JSON 响应）。
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if rerr != nil {
		return resp.StatusCode, nil, rerr
	}
	return resp.StatusCode, body, nil
}

// apiCallUA 官方 `/api/*` 调用的 User-Agent。
//
// # 真源：官方 sidecar 的通用 API 调用器（二进制字符串转储，逐字）
//
//	function wtK(q, K) {
//	  return {
//	    Accept: "application/json",
//	    "Content-Type": "application/json",
//	    "User-Agent": `phanthy-code-cli/${GK9}`,   // GK9 = process.env.APP_VERSION || "2.1.0"
//	    "x-phanthy-client-source": "desktop",
//	    ...q.headers,                              // 调用方传入的 x-desktop-* 签名头
//	    Authorization: `Bearer ${K}`,
//	  }
//	}
//
// # ★★ 2026-11-14 系统对照所得：UA 是**按路径分档**的，不是全局一个值
//
//	/api/oauth/*            → phanthy-code-cli/<APP_VERSION>   ← 本常量（wtK）
//	/api/phanthy_code/models → 官方**不设** UA，走 fetch 默认
//	/v1/chat/completions     → 官方**不设** UA，走 fetch 默认（Bun 下即 Bun/1.3.14）
//	/v1/messages             → claude-cli/<ver> (<USER_TYPE>, cli)   ← OR()
//
// 此前我们对**所有**路径统一发 `Bun/1.3.14`：
//
//	· 推理路径（/v1/chat/completions）**正好是对的** —— 官方也不设，Bun fetch 默认就是它，
//	  这也解释了"坑② 多次真机验证没报错"；
//	· 但 `/api/*` 那一族是错的，官方发的是 `phanthy-code-cli/<APP_VERSION>`。
//
// ⚠ 版本段取官方默认 `2.1.0`（`process.env.APP_VERSION || "2.1.0"`）。
// 我们无法得知真实部署的 APP_VERSION，取官方文档化的默认值是**唯一有据可依**的选择；
// 自造一个（如 2.51.0）等于发明数据。
const apiCallUA = "phanthy-code-cli/2.1.0"

// applyCommonHeaders 注入 `/api/*` 一族请求的公共头。
//
// 这三项与官方 `wtK` **逐项一致**（见 apiCallUA 的真源）：
//
//	user-agent               phanthy-code-cli/2.1.0
//	x-phanthy-client-source  desktop    （官方取值函数：PHANTHY_CODE_CLIENT_SOURCE==="desktop" ? "desktop" : "cli"）
//	accept                   application/json
//
// ⚠ 签名头（x-desktop-*）与 idempotency-key 由各调用点自己用 applyV1 挂上，
//
//	顺序与官方 `...q.headers` 的位置语义一致（调用方传入的覆盖默认）。
func (c *Client) applyCommonHeaders(req *http.Request) {
	req.Header.Set("User-Agent", apiCallUA)
	req.Header.Set("x-phanthy-client-source", clientSource)
	req.Header.Set("Accept", "application/json")
}

// ── /v1/chat/completions（新版内核，OpenAI 兼容，**不签名**）────────────────

// chatClientSource 新版内核推理请求的 x-phanthy-client-source 取值。
//
// ★★ 2026-11-14 更正：此前写的是 "cli"，**是错的**，而且正是封号的成因之一。
//
// 真源：官方 PROTOCOL-INFERENCE.md §二之二（按桌面端 2.51.0 二进制复核）逐字写着：
//
//	"x-phanthy-client-source": "desktop",   // Yg3(): 桌面端 = "desktop"，否则 "cli"
//
// 这个值的语义是「**我是桌面端还是 CLI**」—— 桌面客户端发 desktop。
// 而我们注册时声称的正是桌面端（edition = "phanthy_work"），推理却自称 cli，
// **身份自相矛盾**；再叠加缺失的版本头与设备指纹头，就构成官方文档记的异常组合：
//
//	「来源=桌面端、OS 为空、版本还是旧版」的异常组合 ⇒ 风控记账 ⇒ 封号
//	（PROTOCOL-INFERENCE.md §二之二："2026-11-13 账号被封的最可能触发点"）
//
// 判据：TestInferenceHeadersMatchOfficialClient。
const chatClientSource = "desktop"

// cliDeviceHeader 官方桌面端的设备指纹头 `x-phanthy-cli-device`。
//
// 二进制取证（magpie-PhanthyCode 的 PROTOCOL-INFERENCE.md §二之二，
// 来源 `claude-sidecar.exe` 静态反编译）：
//
//	base64url(JSON.stringify({
//	  version: 1,
//	  platform,          // "win32"
//	  arch,              // "x64"
//	  osRelease,         // "10.0.26300"
//	  cliVersion,        // 官方桌面端版本 = OfficialClientVersion
//	  ipCandidates: [IPv4…],   // 本机网卡候选
//	}))
//
// 服务端「用量」页的 **来源 / OS** 两列由它与 `x-phanthy-client-source` 推出。
// 缺了它，服务端看到「来源=桌面端、OS 为空」的异常组合，风控会记账。
//
// ⚠ 只含平台/架构/系统版本/本机 IPv4，**不含任何对话内容**；
//
//	与官方客户端采集的字段一致。
//
// 进程内只算一次（网卡枚举不便宜，且运行期不变）。
var (
	cliDeviceOnce   sync.Once
	cliDeviceCached string
)

// CliDeviceHeader 返回设备指纹头（首次调用后缓存）。
func CliDeviceHeader() string {
	cliDeviceOnce.Do(func() {
		cliDeviceCached = buildCliDeviceHeader()
	})
	return cliDeviceCached
}

// buildCliDeviceHeader 构造设备指纹（可独立测试）。
func buildCliDeviceHeader() string {
	ips := make([]string, 0, 8)
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			// 只要 IPv4（官方客户端也只收集 IPv4）
			if ip == nil || ip.To4() == nil {
				continue
			}
			ips = append(ips, ip.String())
		}
	}
	payload := map[string]any{
		"version":      1,
		"platform":     nodePlatformName(),
		"arch":         nodeArchName(),
		"osRelease":    osReleaseString(),
		"cliVersion":   OfficialClientVersion,
		"ipCandidates": ips,
	}
	// 键序不参与任何签名（这个头不进 canonical），所以用 map + json.Marshal 即可。
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return b64url(b)
}

// osReleaseString 尽力取系统版本；取不到就退回空串（不影响请求）。
func osReleaseString() string {
	// Windows: os/exec 调 ver 太慢；用 runtime 拿不到版本号。
	// Linux/macOS 可读 uname。这里做最小实现，失败返回空串 ——
	// 官方客户端在取不到时同样是空值，服务端不因此拒绝。
	if out, err := exec.Command("cmd", "/c", "ver").Output(); err == nil {
		s := strings.TrimSpace(string(out))
		// 形如 "Microsoft Windows [Version 10.0.22631.4317]" → 抽版本号
		if i := strings.Index(s, "Version "); i >= 0 {
			if j := strings.Index(s[i:], "]"); j > 0 {
				return strings.TrimSpace(s[i+len("Version ") : i+j])
			}
		}
	}
	return ""
}

// ★★ 2026-11-14：这里原本有一个 `chatProduct = "phanthy_code"` 常量，
// 用于给推理请求设 `x-phanthy-product`。**已删除**，理由：

//	· 官方 sidecar 2.51.0 的字符串转储里 `x-phanthy-product` 出现 **0 次**
//	  （连 `x-phanthy-project` 也是 0 次）—— 这两个头是当初照"想当然的形态"
//	  编出来的，不是抓包得来的；
//	· 多发它们等于给风控送"异常客户端"特征（2026-11-14 真机封号复盘）；
//	· 判据 TestInferenceHeadersMatchOfficialClient 现在会拦住任何"多发的头"。

// （原注释保留在案，说明当时的取值依据，供将来核对遥测侧是否真的用 `phanthy_codeo`）：
// 推理 `phanthy_code`（无 o）/ 遥测 `phanthy_codeo`（带 o）。

// newSessionID 造一个 `ses_<cuid2>` 形态的会话 id。
//
// 抓包原文：`ses_e2aa5d923ffe02djYUjBpWTS5I` —— `ses_` + 24 位 cuid2
// （小写字母 + 数字，首字符为字母）。**不要**用 UUID 或 base64url：
// 后者会产出 `-`/`_`/大写，形态与真实值不符。
func newSessionID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	const head = "abcdefghijklmnopqrstuvwxyz"
	buf := make([]byte, 24)
	// crypto/rand 失败时退化成时间派生（极不可能，但不 panic）。
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(uint64(time.Now().UnixNano()>>uint(i%8)) % 36)
		}
	} else {
		for i := range buf {
			buf[i] = byte(buf[i] % 36)
		}
	}
	var b strings.Builder
	b.WriteString("ses_")
	b.WriteByte(head[int(buf[0])%len(head)])
	for _, x := range buf[1:] {
		b.WriteByte(alphabet[int(x)%len(alphabet)])
	}
	return b.String()
}

// nodePlatformName 把 Go 的 GOOS 映射成官方客户端上报的 `process.platform` 形态。
//
// ★★ 2026-11-14：这个映射是**封号判定**的一部分，不是美观问题。
//
// 官方 sidecar 的 `x-phanthy-cli-device` 里 `platform` 取 `process.platform`，
// Node 在 Windows 上的值是 **"win32"**，而 Go 的 `runtime.GOOS` 是 **"windows"**。
// 直接透传会让服务端「用量」页的 OS 列显示成它不认识的平台名 ——
// 官方文档 PROTOCOL-INFERENCE.md §二之二 明确记着封号触发点：
//
//	「来源=桌面端、OS 为空/对不上、版本还是旧版」的异常组合 ⇒ 风控记账 ⇒ 封号
//
// 判据：TestCliDeviceHeaderUsesNodeNames。
func nodePlatformName() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	default:
		return runtime.GOOS
	}
}

// nodeArchName 把 GOARCH 映射成 `process.arch` 的形态（amd64→x64、386→ia32）。
func nodeArchName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "arm64":
		return "arm64"
	case "arm":
		return "arm"
	default:
		return runtime.GOARCH
	}
}

// ChatCompletionsStream 发一次推理请求（新版内核形态：**不签名**）。
//
// 与 ChatStream 的差异（逐条来自抓包实证，不是推测）：
//
//	· **没有** x-desktop-* / x-phanthy-proof-* 任何签名头
//	· 带 4 个会话头：x-phanthy-client / -project / -request / -session
//	· Authorization 仍是 `Bearer <access_token>`（**不是**推理令牌）
//	· accept: text/event-stream（上游按 body 的 stream 决定，但协商一致更稳）
//
// ⚠ 签名相关的东西**一个都不要加**：抓包里客户端全程零签名，
// 而旧实现带着 12 段 proof 打这条路径 —— 服务端会把"带签名但对不上"的请求
// 当异常处理（实测 /v1/messages 缺 product 头时回 500 空体就是这一类的表现）。
func (c *Client) ChatCompletionsStream(ctx context.Context, a *Auth, path string, body []byte, proof proofValues) (*http.Response, error) {
	if a == nil || a.OAuth.AccessToken == "" {
		return nil, errNoAccessToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))

	// ── 会话头 ──────────────────────────────────────────────────────────────
	//
	// ★★ 2026-11-14（第二次更正）：这两个值**不能**在这里各自随机生成 ——
	//   它们同时是 12 段 canonical 的**第 9 段与第 10 段**。
	//   一旦"签名时用 A、发头时用 B"，服务端重算出来的串必然对不上，
	//   症状就是 403 request_not_authorized（与时钟/UA/令牌过期的症状完全同形）。
	//   ⇒ 一律取 buildProof 产出的那一组（proof.RequestID / proof.SessionID），
	//     没有 proof（理论上不该发生）才回落到本地生成。
	sessionID := proof.SessionID
	if sessionID == "" {
		sessionID = newSessionID()
	}
	reqID := proof.RequestID
	if reqID == "" {
		reqID = randomUUID()
	}
	req.Header.Set("x-request-id", reqID)
	req.Header.Set("x-claude-code-session-id", sessionID)
	req.Header.Set("x-phanthy-session", sessionID)

	// ── 客户端身份头（★ 与官方 1:1 的关键之一）──────────────────────────────
	//
	// 真源：官方 sidecar 里 `/v1/chat/completions` 那条调用的头构造器（逐字）：
	//
	//	X = () => ({
	//	  Authorization: `Bearer ${token}`,
	//	  "Content-Type": "application/json",
	//	  Accept: "application/json",
	//	  "x-phanthy-client-source": "desktop",
	//	  "x-phanthy-cli-device": <设备指纹>,
	//	  "x-claude-code-session-id": sessionId,
	//	})
	//	// 调用处再补： "x-request-id": <uuid>
	//
	// ⚠ 注意 `X()` 是 **Auto 路由器**那条调用的头，它**不是**发往上游的完整形态 ——
	//   本地桥转发时还会再覆盖一层 4 个 `x-phanthy-desktop-session` / `x-phanthy-proof-*`
	//   （见函数末尾那段，以及 TestChatPathCarriesDesktopInferenceProof）。
	//   只看 `X()` 会得出"chat 全程零签名"的错误结论。
	//
	// # ★★ 2026-11-14 封号复盘：这段以前是错的，而且真机**封了号**
	//
	// 改之前这里发的是：
	//
	//	x-phanthy-client: cli            ← 官方 sidecar 里**根本不存在**这个头
	//	x-phanthy-project: global        ← 不存在
	//	x-phanthy-product: phanthy_code  ← 不存在
	//	x-phanthy-client-source: cli     ← **应为 desktop**（注册声称 edition=phanthy_work）
	//	（且完全没有 x-app / 版本头 / 设备指纹头）
	//
	// 后果与官方文档记的封号触发点逐条吻合（PROTOCOL-INFERENCE.md §二之二）：
	//
	//	「来源=桌面端、OS 为空、版本对不上」的异常组合 ⇒ 风控记账 ⇒ **封号**
	//
	// 我们那次更糟：来源自称 cli（与注册声称的桌面端**自相矛盾**）、没有设备指纹
	// （OS 为空）、没有版本头，还多了三个官方不存在的头。
	// 真机结果：`POST /v1/chat/completions` → 403 `disabled_account`
	//（「当前账户已被封禁，请加微信群联系客服处理」）。
	//
	// ⇒ 纪律：**推理路径的头必须与官方客户端逐项一致** ——
	//   多一个官方没有的头、少一个官方有的头，都是"异常客户端"特征。
	//   判据：TestInferenceHeadersMatchOfficialClient。
	req.Header.Set("x-phanthy-client-source", chatClientSource) // "desktop"
	req.Header.Set("x-app", "cli")
	req.Header.Set("x-phanthy-client-version", OfficialClientVersion)
	if dev := CliDeviceHeader(); dev != "" {
		req.Header.Set("x-phanthy-cli-device", dev)
	}

	// ── ★★ 4 个 desktop-inference-v2 证明头（2026-11-14 补上，这是本轮最重要的一处）──
	//
	// # 真源：官方 sidecar 的**本地推理桥** `gd3` + proof 构造器 `_O8`（逐字）
	//
	//	// 桥的入口集合（注意第三个成员）
	//	Sd3 = new Set(["/v1/messages", "/v1/responses", "/v1/chat/completions"])
	//
	//	// 转发前：先删掉入站的 4 个证明头，再重新算一份覆盖上去
	//	for (let x of ["x-phanthy-desktop-session","x-phanthy-proof-signature",
	//	               "x-phanthy-proof-nonce","x-phanthy-proof-timestamp"]) G.delete(x);
	//	let u = _O8(q.method, K.pathname, $, G);
	//	if (u) for (let [x, m] of Object.entries(u)) G.set(x, m);
	//
	// # ★ 这里曾经有一个**方向性错误**，值得留档
	//
	// 早先的实现（含大量注释）断言「chat 路径全程零签名，签名相关的一个都不要加」。
	// 那个结论来自**只抓到了链路的其中一跳**：
	//
	//	官方实际链路：内核（Phanthy Codeo） → **本地桥（sidecar, 127.0.0.1）** → 上游
	//	抓到的只是  ：内核 → 本地桥            ← 这一跳确实不签名
	//	真正发往上游的：本地桥 → 上游          ← **桥会补 12 段签名 + 4 个头**
	//
	// 而 `Sd3` 里明确包含 `/v1/chat/completions` —— 也就是说 chat 这条路**同样要签**。
	// 又因为 12 段 canonical 的拼装（`_O8`）与本包 `InferenceCanonical` 逐段一致，
	// 所以签名算法早就写对了，缺的只是「把它用在 chat 路径上」。
	//
	// 旁证（本包既有的假上游里写着）：上游对缺这个头的请求回的正是
	//	403 {"code":"request_not_authorized","message":"missing desktop session"}
	//
	// 判据：TestChatPathCarriesDesktopInferenceProof（必须带齐 4 头）
	//      + TestChatSendsNoSignatureHeaders 已**反转**（原判据在守护错误行为）。
	if proof.Signature != "" {
		req.Header.Set("x-phanthy-desktop-session", proof.InferenceToken)
		req.Header.Set("x-phanthy-proof-timestamp", proof.Timestamp)
		req.Header.Set("x-phanthy-proof-nonce", proof.Nonce)
		req.Header.Set("x-phanthy-proof-signature", proof.Signature)
	}
	for k, v := range proof.ExtraHeaders {
		req.Header.Set(k, v)
	}

	return c.httpClient().Do(req)
}

// newJSONRequest 造一个带 JSON body 的请求。
//
// body 必须是**已经序列化好的字节**（见 crypto.go 的 RequestBodyForHash）——
// 这里绝不重新序列化，那是坑⑧ 的全部内容。
func (c *Client) newJSONRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyCommonHeaders(req)
	return req, nil
}

// ── v1 签名（x-desktop-* 那 5 个头）───────────────────────────────────────

// applyV1 给请求挂上 v1 签名头（identity 私钥）。
//
// 头部形状（报告 §2.4 的 8 个头，去掉 content-type/accept/authorization）：
//
//	x-desktop-installation-id  di_…（**本地派生的那个**）
//	x-desktop-timestamp        与 canonical 里同一个值（毫秒字符串）
//	x-desktop-nonce            与 canonical 里同一个值
//	x-desktop-signature        identity 私钥签的 v1 canonical
//	idempotency-key            只在**有**的时候发（见 params.idempotencyKey）
//
// ⚠ 幂等键：v1 canonical 第 7 段带它，**且请求头也要带**。
// 但 activities/summary 那条路必须传空串且不发头（坑⑪）—— 判据由 params 给，
// 不在这里硬编码端点名。
type v1Params struct {
	// Method 用于 canonical 的 method（**保持原值**，坑⑫）。
	Method string
	// Path upstreamPath（canonical 第 3 段）。
	Path string
	// Body 最终发出的 body 原始字节。
	Body []byte
	// IdempotencyKey 空串 = 这条请求没有幂等键（canonical 第 7 段空串、不发头）。
	IdempotencyKey string
}

// signV1 生成 v1 签名头与用了的值（返回给调用方做断言/排查）。
func signV1(a *Auth, p v1Params, timestamp, nonce string) (sig string, err error) {
	// ★★ 2026-11-14 加的护栏：canonical 第 3 段必须是**不含 query 的 pathname**。
	//
	// 真源（magpie referrals.mjs 的警告，逐字）：
	//
	//	⚠ `request()` 的 v1 canonical 用的是 `new URL(url).pathname`。
	//	  本模块的两个端点都**没有 query**，所以签出来是对的；将来若要加
	//	  `referrals/relations?limit=…`，必须先修 `upstreamPath`，否则必然 401。
	//
	// 也就是说：**URL 可以带 query，签名的 path 不能带**。
	// 本包当前的调用点都传常量（都不带 query），所以现在是对的 —— 但这是个
	// "加个 ?limit= 就 401、而错误码指不到根因"的陷阱，值得用断言钉住：
	// 与其等将来真出 401 再查，不如在拼 canonical 之前当场报错。
	if strings.Contains(p.Path, "?") {
		return "", fmt.Errorf(
			"phanthywork: v1 canonical 的 path 段不能带 query（收到 %q）—— "+
				"URL 可以带 query，签名一律用 pathname（否则上游必然 401）", p.Path)
	}
	canonical := V1Canonical(p.Method, p.Path, timestamp, nonce, RequestBodyForHash(p.Body), p.IdempotencyKey)
	return a.signV1Canonical(canonical)
}

// applyV1 把 v1 签名挂到请求上。
func applyV1(req *http.Request, a *Auth, p v1Params, timestamp, nonce string) error {
	sig, err := signV1(a, p, timestamp, nonce)
	if err != nil {
		return err
	}
	id, err := a.installationID()
	if err != nil {
		return err
	}
	req.Header.Set("x-desktop-installation-id", id)
	req.Header.Set("x-desktop-timestamp", timestamp)
	req.Header.Set("x-desktop-nonce", nonce)
	req.Header.Set("x-desktop-signature", sig)
	if p.IdempotencyKey != "" {
		req.Header.Set("idempotency-key", p.IdempotencyKey)
	}
	req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	return nil
}

// ── 账号档案（accountScope 的来源）──────────────────────────────────────

// profileResponse GET /api/oauth/profile 里本包要的两个字段。
type profileResponse struct {
	Account struct {
		UUID        string `json:"uuid"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	} `json:"account"`
}

// FetchAccountScope 取账号 uuid（= device canonical 的 accountScope 段）。
//
// ⚠ 报告两处都强调过这个值不能用 JWT 的 sub / 邮箱代替（用错回
// 400 desktop_account_scope_invalid），所以这里**真的去问服务端**，
// 而不是拿本地能猜的东西凑。
//
// 这条路只需要 Bearer（实测 5 个只读端点全部 200，不需要 proof 头）。
func (c *Client) FetchAccountScope(ctx context.Context, a *Auth) (string, string, error) {
	if a == nil || a.OAuth.AccessToken == "" {
		return "", "", errNoAccessToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase()+pathProfile, nil)
	if err != nil {
		return "", "", err
	}
	c.applyCommonHeaders(req)
	req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	status, body, err := c.doJSON(req, httpTimeout)
	if err != nil {
		return "", "", fmt.Errorf("phanthywork: 取账号档案失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", "", &UpstreamError{Status: status, Body: string(body), Op: "取账号档案"}
	}
	var pr profileResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return "", "", fmt.Errorf("phanthywork: 账号档案不是 JSON: %w", err)
	}
	scope := strings.TrimSpace(pr.Account.UUID)
	if scope == "" {
		return "", "", fmt.Errorf("phanthywork: 账号档案里没有 account.uuid（签名必须要它）")
	}
	return scope, strings.TrimSpace(pr.Account.DisplayName), nil
}

// ── 模型目录 ─────────────────────────────────────────────────────────────

// FetchModels 拉上游模型目录（**只带 Bearer** —— 实测 200 时不需要 proof）。
//
// ⚠ 返回的 id **不带 phanthy/ 前缀**（坑③）。这是本包最容易踩的一处：
// 桌面端本地网关给的是 phanthy/Phanthy-fast，云端目录给的是 Phanthy-fast，
// 打云端就必须用云端那份。
func (c *Client) FetchModels(ctx context.Context, a *Auth) ([]remoteModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase()+pathModels, nil)
	if err != nil {
		return nil, err
	}
	c.applyCommonHeaders(req)
	if a != nil && a.OAuth.AccessToken != "" {
		req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	}
	status, body, err := c.doJSON(req, modelsTimeout)
	if err != nil {
		return nil, fmt.Errorf("phanthywork: 拉模型目录失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, &UpstreamError{Status: status, Body: string(body), Op: "拉模型目录"}
	}
	var doc struct {
		SchemaVersion int           `json:"schema_version"`
		DefaultModel  string        `json:"default_model"`
		Models        []remoteModel `json:"models"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("phanthywork: 模型目录不是 JSON: %w", err)
	}
	// 报告 §5：schema_version 不是 1 就当失败，**不许猜字段**。
	// 猜字段的后果是"模型列表看起来正常，模型 id 全是错的"。
	if doc.SchemaVersion != modelsSchemaVersion {
		return nil, fmt.Errorf("phanthywork: 模型目录 schema_version=%d（期望 %d），字段形状可能已变，拒绝猜",
			doc.SchemaVersion, modelsSchemaVersion)
	}
	return doc.Models, nil
}

// ── OAuth 换 token 与续期 ────────────────────────────────────────────────

// TokenSet 一次 token 响应的归一结果。
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAtMS 本机口径的到期时刻（毫秒）。
	ExpiresAtMS int64
	Scopes      []string
	// ExpiresIn 服务端给的秒数（排查用；0 = 服务端没给，用了兜底值）。
	ExpiresIn int64
}

// ExchangeCode 用授权码换 token（**form-urlencoded**，20 秒超时，不重试）。
//
// ⚠ 与续期**编码不同**（报告 §4.2 的对照表）：这里 URLSearchParams，
// 续期 JSON。照抄任一侧都会错。
//
// 6 个字段：grant_type / client_id / code / state（**再带一次**）/
// code_verifier / redirect_uri（必须与 authorize 那次**逐字相同**）。
func (c *Client) ExchangeCode(ctx context.Context, code, state, verifier, redirectURI string) (TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", OAuthClientID)
	form.Set("code", code)
	form.Set("state", state)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+pathOAuthToken,
		strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.applyCommonHeaders(req)

	status, body, err := c.doJSON(req, exchangeTimeout)
	if err != nil {
		return TokenSet{}, fmt.Errorf("phanthywork: 换取登录凭据失败: %w", err)
	}
	if status == 429 {
		return TokenSet{}, &UpstreamError{Status: status, Body: brief(body), Op: "换取登录凭据"}
	}
	if status >= 500 {
		return TokenSet{}, fmt.Errorf("phanthywork: 登录服务暂时不可用（HTTP %d），稍后重试: %s", status, brief(body))
	}
	if status < 200 || status >= 300 {
		return TokenSet{}, fmt.Errorf("phanthywork: 授权码已失效或已使用（HTTP %d）: %s", status, brief(body))
	}
	return parseTokenResponse(body, exchangeDefaultExpiresIn, nowMS())
}

// RefreshTokens 用 refresh_token 续期（**JSON body**，8 秒超时，3 次重试）。
//
// 三条照抄的边界（全部来自报告 §4.2 的逐字反编译）：
//
//	① body 是 JSON（Content-Type: application/json），与登录那次 form 不同
//	② 只对 **429 或 >= 500** 重试；其它 4xx **立即失败**
//	③ 400 + invalid_grant ⇒ 返回 ErrRefreshExpired（凭据废了，终态）
//
// 返回的 TokenSet.RefreshToken 可能为空 —— 那表示服务端**轮换**了它
// （实测轮换），调用方按"没给新的就沿用旧的"处理（见 Auth.applyTokenSet）。
func (c *Client) RefreshTokens(ctx context.Context, a *Auth) (TokenSet, error) {
	if a == nil || strings.TrimSpace(a.OAuth.RefreshToken) == "" {
		return TokenSet{}, ErrRefreshExpired
	}
	payload := map[string]any{
		"grant_type":    "refresh_token",
		"client_id":     OAuthClientID,
		"refresh_token": a.OAuth.RefreshToken,
	}
	if len(a.OAuth.Scopes) > 0 {
		payload["scope"] = strings.Join(a.OAuth.Scopes, " ")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return TokenSet{}, err
	}

	var lastErr error
	for attempt := 0; attempt < refreshMaxAttempts; attempt++ {
		req, rerr := c.newJSONRequest(ctx, http.MethodPost, pathOAuthToken, raw)
		if rerr != nil {
			return TokenSet{}, rerr
		}
		status, body, derr := c.doJSON(req, refreshTimeout)
		if derr != nil {
			lastErr = fmt.Errorf("phanthywork: 续期请求失败: %w", derr)
			if ctx.Err() != nil {
				return TokenSet{}, ctx.Err()
			}
			continue
		}
		switch {
		case status >= 200 && status < 300:
			return parseTokenResponse(body, refreshDefaultExpiresIn, nowMS())
		case status == 400 && gateway.ContainsAny(string(body), authExpiredMarkers):
			// invalid_grant 的语义（报告 §4.2）：refreshToken 已失效
			//（过期/被吊销/被轮换）⇒ 终态，不要重试。
			return TokenSet{}, ErrRefreshExpired
		case status == 429 || status >= 500:
			wait := backoffFor(attempt, retryAfterMS(body))
			lastErr = &UpstreamError{Status: status, Body: brief(body), Op: "续期"}
			if attempt == refreshMaxAttempts-1 || wait > maxBackoff {
				return TokenSet{}, lastErr
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return TokenSet{}, ctx.Err()
			}
			continue
		default:
			// 其它 4xx：立即失败（不含 invalid_grant 的 400 都走这里）。
			return TokenSet{}, &UpstreamError{Status: status, Body: string(body), Op: "续期"}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("phanthywork: 续期失败（重试 %d 次后放弃）", refreshMaxAttempts)
	}
	return TokenSet{}, lastErr
}

// parseTokenResponse 解析 token 响应（登录与续期同构）。
func parseTokenResponse(body []byte, fallbackExpiresIn int64, localMS int64) (TokenSet, error) {
	var doc struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Scope        string          `json:"scope"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return TokenSet{}, fmt.Errorf("phanthywork: token 响应不是 JSON: %w", err)
	}
	at := strings.TrimSpace(doc.AccessToken)
	if at == "" {
		return TokenSet{}, fmt.Errorf("phanthywork: token 响应缺少 access_token（响应体不完整）")
	}
	expiresIn := fallbackExpiresIn
	if v := parseIntRaw(doc.ExpiresIn); v > 0 {
		expiresIn = v
	}
	ts := TokenSet{
		AccessToken:  at,
		RefreshToken: strings.TrimSpace(doc.RefreshToken),
		ExpiresIn:    expiresIn,
		ExpiresAtMS:  localMS + expiresIn*1000,
	}
	if s := strings.TrimSpace(doc.Scope); s != "" {
		ts.Scopes = strings.Fields(s)
	}
	return ts, nil
}

// ── 设备注册 ─────────────────────────────────────────────────────────────

// registerResponse 注册响应的关键字段（报告 §2.4 实测）。
type registerResponse struct {
	Installation struct {
		ID string `json:"id"`
	} `json:"installation"`
	// ReasonCode 服务端的**拒绝原因**（★ 2026-11-14 真机封号铁证）。
	//
	// 实测：对"该账号已有第一台设备"的二次注册，服务端**不是**回 409，
	// 而是回 **HTTP 200** + `{"reason_code":"first_device_owner","reward_status":"denied"}`。
	// 只看状态码会把它当成注册成功 —— 后果见 RegisterDevice 的注释。
	ReasonCode   string `json:"reason_code"`
	RewardStatus string `json:"reward_status"`
	RewardReason string `json:"reward_reason"`
	Error        struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// RegisterDevice 设备注册（**写操作**，一次性）。
//
// body 7 个字段，**顺序即官方客户端的注入顺序**（JSON.stringify 保序，
// 签名覆盖这个串）—— 所以这里手写序列化，不用 map（map 的键序不保证）。
//
//	{
//	  "public_key":           identity 公钥
//	  "edition":              "phanthy_work"
//	  "platform":             windows/macos/linux
//	  "version":              **官方客户端版本号 2.48.0**（不是本包的版本！）
//	  "device_public_key":    device 公钥（**必须与 public_key 不同**）
//	  "device_signature":     device 私钥签的 device canonical
//	  "inference_public_key": 推理公钥
//	}
//
// 失败模式（报告 §2.4）：
//
//	409 desktop_installation_conflict → **终态**，返回 ErrRegistrationConflict
//	400/403 desktop_binding_invalid   → canonical 算错了（值得查代码）
//	400 desktop_account_scope_invalid → accountScope 不对
func (c *Client) RegisterDevice(ctx context.Context, a *Auth) (registerResponse, error) {
	var out registerResponse
	if a == nil {
		return out, errNilAuth
	}
	scope := a.accountScope()
	if scope == "" {
		return out, fmt.Errorf("phanthywork: 缺少 accountScope（设备注册的 canonical 必须要它）")
	}
	identityPub, err := a.identityPublicKey()
	if err != nil {
		return out, err
	}
	devicePub, err := a.devicePublicKey()
	if err != nil {
		return out, err
	}
	instID, err := a.installationID()
	if err != nil {
		return out, err
	}
	if a.Infer.PublicKey == "" {
		return out, fmt.Errorf("phanthywork: 缺少推理公钥（它是 device canonical 第 5 段）")
	}
	// ⚠ device 公钥必须与 identity 公钥不同（IDENTITY_POLICY 的
	// deviceKeyDistinct）。派生换域已经保证这一点，这里再挡一次：
	// 万一有人把两把密钥写成同一把，失败会是一条看不出原因的
	// desktop_binding_invalid。
	if devicePub == identityPub {
		return out, fmt.Errorf("phanthywork: device 公钥与 identity 公钥相同（IDENTITY_POLICY 要求两把必须不同）")
	}

	timestamp := msToString(a.serverNowMS())
	nonce := newDeviceNonce()
	deviceSig, err := a.signDeviceCanonical(
		DeviceCanonical(scope, instID, identityPub, a.Infer.PublicKey, timestamp, nonce))
	if err != nil {
		return out, err
	}

	// 手写序列化以保证**键序**（签名覆盖字节）。
	body := fmt.Sprintf(
		`{"public_key":%q,"edition":%q,"platform":%q,"version":%q,"device_public_key":%q,"device_signature":%q,"inference_public_key":%q}`,
		identityPub, Edition, platformName(), OfficialClientVersion, devicePub, deviceSig, a.Infer.PublicKey)

	req, err := c.newJSONRequest(ctx, http.MethodPost, pathRegister, []byte(body))
	if err != nil {
		return out, err
	}
	idem := registerIdempotencyKey(instID)
	if err := applyV1(req, a, v1Params{
		Method: http.MethodPost, Path: pathRegister, Body: []byte(body), IdempotencyKey: idem,
	}, timestamp, nonce); err != nil {
		return out, err
	}

	status, respBody, derr := c.doJSON(req, registerTimeout)
	if derr != nil {
		return out, fmt.Errorf("phanthywork: 设备注册请求失败: %w", derr)
	}
	if status < 200 || status >= 300 {
		ue := &UpstreamError{Status: status, Body: string(respBody), Op: "设备注册"}
		if status == 409 || strings.Contains(string(respBody), "desktop_installation_conflict") {
			// 终态：包一层，调用方用 errors.Is(err, ErrRegistrationConflict) 判。
			return out, fmt.Errorf("%w: %s", ErrRegistrationConflict, ue.Error())
		}
		return out, ue
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, fmt.Errorf("phanthywork: 注册响应不是 JSON: %w", err)
	}
	if out.Error.Code != "" {
		return out, &UpstreamError{Status: status, Body: string(respBody), Op: "设备注册"}
	}

	// ★ 2026-11-14 真机封号铁证：二次注册回的是 **HTTP 200 + reason_code**，不是 409。
	//
	// 凭证文件里留下的原文：{"registered_at_ms":…,"reason_code":"first_device_owner"}
	// —— 也就是说旧实现把它当成功写进了 RegisteredAtMS，然后拿一把服务端
	// **根本不认**的 key 去换推理令牌 / 发推理，一路 403，最后账号被
	// `disabled_account` 封掉（同一账号 11 分钟前还在 200）。
	//
	// 判据：**reason_code 非空即终态冲突**，与 409 同一条出口
	// （调用方用 errors.Is(err, ErrRegistrationConflict) 判，随后熔断，不再注册）。
	if rc := strings.TrimSpace(out.ReasonCode); rc != "" {
		ue := &UpstreamError{Status: status, Body: string(respBody), Op: "设备注册"}
		return out, fmt.Errorf(
			"%w: 服务端拒绝这台设备（reason_code=%s）—— 该账号已有第一台设备，"+
				"同一账号不可能再绑第二台（继续重试只会把账号打进 disabled_account）: %s",
			ErrRegistrationConflict, rc, ue.Error())
	}
	return out, nil
}

// ── 换推理令牌 ───────────────────────────────────────────────────────────

// summaryResponse activities/summary 的响应里本包要的字段。
//
// # 为什么一个结构体承载三种用途（这是报告 §4.7/full-platform §2.1 的结论）
//
// 实测这个端点是全平台**信息密度最高**的一个：一次调用同时给出
//
//	server_time                    → 时钟校准（坑①）
//	desktop_inference_token        → 换推理令牌（TTL 180s）
//	credits.available              → 钱包可用积分（额度）
//	daily.{status,server_date,...} → 签到状态 + **幂等键要用的服务端日期**
//	referral / feature_flags       → 邀请资格 / 今日签到是否开放
//
// 所以本包只发**这一条** v1 签名的请求就能同时满足续期、额度、签到三件事，
// 不必为每个功能各打一个端点（少一次往返，也少一处签名口径）。
//
// ⚠ 只声明用得上的字段：报告 §5 诚实说明过"逐条字段没有落盘"，所以
// **不编造**没采集到的字段（编了就会在解析时静默得到零值，而零值有语义）。
type summaryResponse struct {
	SchemaVersion int    `json:"schema_version"`
	ServerTime    string `json:"server_time"`
	// DesktopInferenceToken 推理令牌（JWT，实测 813 字符，exp-iat = 180s）。
	DesktopInferenceToken string `json:"desktop_inference_token"`
	// Credits 钱包（available = 可用积分，实测 3600）。
	Credits struct {
		Available int64 `json:"available"`
		Reserved  int64 `json:"reserved"`
	} `json:"credits"`
	// Daily 签到（**服务器自己的日键**在这里）。
	Daily struct {
		// Status 实测取值 granted_today（= 今天已领）。
		//
		// ⚠ 报告 §2.2 提醒：claim 端点的同义字段叫 already_granted ——
		// **两套词说同一件事**，判断时别只认一个。
		Status string `json:"status"`
		// ServerDate 服务端日键（UTC+8 换日）。幂等键**必须**用它，
		// 不许本地算日期（本地算在跨零点/时区差时会算出两个日键 ⇒ 发出两条）。
		ServerDate string `json:"server_date"`
		Points     int64  `json:"points"`
		StreakDay  int64  `json:"streak_day"`
		NextPoints int64  `json:"next_points"`
		// MaxStreakDay 连续上限（实测 5）。
		MaxStreakDay   int64  `json:"max_streak_day"`
		NextEligibleAt string `json:"next_eligible_at"`
	} `json:"daily"`
	// FeatureFlags 活动开关（daily_enabled=true 表示今天开放签到）。
	FeatureFlags struct {
		DailyEnabled bool `json:"daily_enabled"`
	} `json:"feature_flags"`
}

// FetchSummary 打一次 activities/summary（v1 签名，**无幂等键** —— 坑⑪）。
//
// 它是续期、额度、签到三条路的**唯一**上游读入口：调用方各取所需字段，
// 于是"签名怎么拼"只有一处实现（判据单源）。
func (c *Client) FetchSummary(ctx context.Context, a *Auth) (summaryResponse, int64, error) {
	var out summaryResponse
	localMS := nowMS()
	if a == nil {
		return out, localMS, errNilAuth
	}
	body := fmt.Sprintf(`{"inference_public_key":%q}`, a.Infer.PublicKey)
	timestamp := msToString(a.serverNowMS())
	nonce := newDeviceNonce()
	req, err := c.newJSONRequest(ctx, http.MethodPost, pathActivitiesSummary, []byte(body))
	if err != nil {
		return out, localMS, err
	}
	// ⚠ IdempotencyKey 留空（坑⑪：加了就签错）。
	if err := applyV1(req, a, v1Params{
		Method: http.MethodPost, Path: pathActivitiesSummary, Body: []byte(body),
	}, timestamp, nonce); err != nil {
		return out, localMS, err
	}
	status, respBody, derr := c.doJSON(req, summaryTimeout)
	localMS = nowMS() // 校准用"拿到响应"的时刻
	if derr != nil {
		return out, localMS, fmt.Errorf("phanthywork: 读账号状态请求失败: %w", derr)
	}
	if status < 200 || status >= 300 {
		return out, localMS, &UpstreamError{Status: status, Body: string(respBody), Op: "读账号状态"}
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, localMS, fmt.Errorf("phanthywork: 账号状态响应不是 JSON: %w", err)
	}
	return out, localMS, nil
}

// DailyLoginClaimResponse daily-login/claim 的响应（报告 §4.7 实测全字段）。
type DailyLoginClaimResponse struct {
	SchemaVersion int `json:"schema_version"`
	// Status 实测 already_granted = 今天已领（**也是 200**，不是错误）。
	//
	// ⚠ 首次领取的确切值是**未知量**（报告 §4.7 明说"没测到"：桌面端启动就自动领了）。
	// 所以判断"领到了没有"**不能**按某个固定字符串写死 —— 见 ClaimDailyLogin 的注释。
	Status string `json:"status"`
	// ServerDate 服务端日键（UTC+8）。**幂等键必须用它**。
	ServerDate string `json:"server_date"`
	Points     int64  `json:"points"`
	StreakDay  int64  `json:"streak_day"`
	// Reward 存在与否是"这次真的产生了奖励"的旁证（报告 §4.7 第 4 条判据：
	// already_granted 一定有 reward 对象；凭据问题一定没有）。
	Reward struct {
		Points     int64  `json:"points"`
		Status     string `json:"status"`
		RewardType string `json:"reward_type"`
	} `json:"reward"`
}

// ClaimDailyLogin 领取今日签到（POST daily-login/claim）。
//
// # 三条硬要求（报告 §4.7，逐条落在签名/幂等键/返回判定上）
//
//	① body 必须是**空对象** `{}` —— 不是空串（空串会让 canonical 第 6 段哈希对不上）
//	② 幂等键格式 `daily:<installationId>:<server_date>`，且**必须**用**响应里的**
//	   server_date（本地算日期会在跨零点/时区差时算出两个日键 ⇒ 发出两条）
//	③ "今日已领"是**幂等成功**（200 + already_granted），不是失败
func (c *Client) ClaimDailyLogin(ctx context.Context, a *Auth, serverDate string) (DailyLoginClaimResponse, error) {
	var out DailyLoginClaimResponse
	if a == nil {
		return out, errNilAuth
	}
	serverDate = strings.TrimSpace(serverDate)
	if serverDate == "" {
		// ⚠ 没有服务端日键就**不发**：本地算一个日期等于违反上面第 ② 条。
		return out, fmt.Errorf("phanthywork: 缺少服务端日键（server_date），拒绝用本地日期拼幂等键")
	}
	instID, err := a.installationID()
	if err != nil {
		return out, err
	}
	// ① body = 空对象（字面量 {}）
	const body = `{}`
	timestamp := msToString(a.serverNowMS())
	nonce := newDeviceNonce()
	req, err := c.newJSONRequest(ctx, http.MethodPost, pathDailyLoginClaim, []byte(body))
	if err != nil {
		return out, err
	}
	idem := DailyLoginIdempotencyKey(instID, serverDate)
	if err := applyV1(req, a, v1Params{
		Method: http.MethodPost, Path: pathDailyLoginClaim, Body: []byte(body), IdempotencyKey: idem,
	}, timestamp, nonce); err != nil {
		return out, err
	}
	status, respBody, derr := c.doJSON(req, summaryTimeout)
	if derr != nil {
		return out, fmt.Errorf("phanthywork: 签到请求失败: %w", derr)
	}
	if status < 200 || status >= 300 {
		return out, &UpstreamError{Status: status, Body: string(respBody), Op: "签到"}
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, fmt.Errorf("phanthywork: 签到响应不是 JSON: %w", err)
	}
	return out, nil
}

// DailyLoginIdempotencyKey 拼签到幂等键：`daily:<installationId>:<server_date>`。
//
// ⚠ 报告 §4.7 的实测值长这样：`daily:di_oit3noGRcRw6NNe4pz1NhPjrISfygoO7vTTkWmaib64:2026-11-12`。
// 开源实现写的是毫秒时间戳 —— **照抄会让幂等彻底失效**（每天发出多条，每条一个新键）。
func DailyLoginIdempotencyKey(installationID, serverDate string) string {
	return "daily:" + installationID + ":" + serverDate
}

// RequestInferenceToken 换推理令牌（POST /api/oauth/activities/summary）。
//
// body **只有 1 个字段**：
//
//	{ "inference_public_key": "<推理公钥 SPKI/b64url>" }
//
// ⚠ v1 canonical 第 7 段是**空串**，请求头**不发** idempotency-key（坑⑪）。
// 报告的原话：「别"顺手加一个幂等键" —— canonical 会变，签名立刻失败」。
// 所以这里 idempotencyKey 恒为空串（对比 RegisterDevice 用的是 register:<id>）。
//
// 响应同时带 server_time —— 本包在校准时钟时只认它（坑①）。
func (c *Client) RequestInferenceToken(ctx context.Context, a *Auth) (summaryResponse, int64, error) {
	if a == nil {
		return summaryResponse{}, nowMS(), errNilAuth
	}
	if a.Infer.PublicKey == "" {
		var zero summaryResponse
		return zero, nowMS(), fmt.Errorf("phanthywork: 缺少推理公钥")
	}
	// 与额度/签到共用同一次上游读（判据单源：签名怎么拼只有一处实现）。
	out, localMS, err := c.FetchSummary(ctx, a)
	if err != nil {
		return out, localMS, err
	}
	if strings.TrimSpace(out.DesktopInferenceToken) == "" {
		// 200 但没有令牌 = 结构变了。**明确失败**而不是静默继续
		//（静默继续的表现是随后 403 request_not_authorized，查不到根因）。
		return out, localMS, fmt.Errorf("phanthywork: 响应里没有 desktop_inference_token（上游字段形状可能已变）")
	}
	return out, localMS, nil
}

// ── 调模型 ───────────────────────────────────────────────────────────────

// ChatStream 发一次 /v1/messages，返回原始响应（**调用方负责 Close**）。
//
// 与 gateway.Provider.Chat 的契约一致：非 2xx 也算正常返回（把上游错误体
// 带回来给核心分类），只有网络层失败才返回 error。
func (c *Client) ChatStream(ctx context.Context, a *Auth, path string, body []byte, proof proofValues) (*http.Response, error) {
	if a == nil || a.OAuth.AccessToken == "" {
		return nil, errNoAccessToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyCommonHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	// ⚠ 这条是**推理**路径（/v1/messages），UA 与 `/api/*` 那一族**不同**：
	//
	//	/api/*        → phanthy-code-cli/<APP_VERSION>（applyCommonHeaders 设的，正确）
	//	/v1/messages  → 官方是 `claude-cli/<ver> (<USER_TYPE>, cli)`（sidecar 的 OR()）
	//
	// 那个 UA 里的 Claude Code 版本段与 USER_TYPE 我们**拿不到可靠值**，
	// 所以这里不改写它、也不发明一个值：显式钉回本包的默认 UA，
	// 避免 applyCommonHeaders 的 /api 口径"漏"进推理路径。
	// （这条路径是旧内核，默认不走 —— 见 Protocol 配置；新版内核走 ChatCompletionsStream。）
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	// ★ 2026-11-13：官方桌面端 2.51.0 在**每个**出站请求上都带这三个头，
	//   缺任何一个都会被服务端风控当成异常客户端（实测 500 空体）。
	//
	// 依据（magpie-PhanthyCode 的 PROTOCOL-INFERENCE.md §二之二，二进制取证）：
	//
	//	x-app: cli
	//	x-phanthy-client-source: desktop      ← applyCommonHeaders 已设
	//	x-phanthy-client-version: 2.51.0
	//	x-phanthy-cli-device: base64url({version,platform,arch,osRelease,cliVersion,ipCandidates})
	//
	//   服务端「用量」页的「来源 / OS」两列就是由后两个推出来的；
	//   「来源=desktop 但 OS 为空、版本对不上」会被判为异常客户端。
	req.Header.Set("x-app", "cli")
	req.Header.Set("x-phanthy-client-version", OfficialClientVersion)
	req.Header.Set("x-phanthy-cli-device", CliDeviceHeader())
	if proof.RequestID != "" {
		req.Header.Set("x-request-id", proof.RequestID)
		// ⚠ 这 4 段在 canonical 里有位置，但**头只在有值时才发**：
		// 报告列的 4 个 proof 头里没有它们（它们是别处的可选上下文头）。
	}
	// ★ 2026-11-14 真机：这两个头是**必需**的（缺 → 上游 500 空体，0 字节）。
	//
	// 官方 sidecar 的拦截器把内核的 `x-phanthy-session` 改写成
	// `x-claude-code-session-id`（原头保留），再用改写后的值算 canonical 第 10 段。
	// 本包在 buildProof 里生成一次，三处共用 —— 保证同源。
	req.Header.Set("x-phanthy-session", proof.SessionID)
	req.Header.Set("x-claude-code-session-id", proof.SessionID)
	req.Header.Set("x-phanthy-desktop-session", proof.InferenceToken)
	req.Header.Set("x-phanthy-proof-timestamp", proof.Timestamp)
	req.Header.Set("x-phanthy-proof-nonce", proof.Nonce)
	req.Header.Set("x-phanthy-proof-signature", proof.Signature)
	for k, v := range proof.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return c.httpClient().Do(req)
}

// proofValues 一次推理请求的 proof 头（由 buildProof 产出）。
type proofValues struct {
	// InferenceToken **推理令牌本身**（⚠ 不是 sessionId，坑⑨）。
	InferenceToken string
	Timestamp      string
	Nonce          string
	Signature      string
	// RequestID uuid v4（canonical 第 9 段、也作为 x-request-id 头发出去）。
	RequestID string
	// SessionID 本请求的会话 id（canonical **第 10 段**）。
	//
	// ★ 2026-11-14 真机：它必须同时作为 x-phanthy-session 与
	// x-claude-code-session-id 发出去，且与第 10 段同一个值。
	// 缺这两头任意一个 → 上游 500 空体（kong upstream-latency 9~10ms）。
	SessionID string
	// ExtraHeaders 可选上下文头（canonical 第 10~12 段对应的那三个）。
	ExtraHeaders map[string]string
}

// buildProof 构造一次推理请求的 12 段签名与 4 个 proof 头。
//
// ⚠ 5 个规则（报告 §2.6 + 2026-11-14 真机），每一条都有对应的测试：
//
//	① 段间 **LF**（⚠ 与 device/v1 的空格不同，见 crypto.go 包注释）
//	② 第 6 段是**最终发出的 body 原始字节**的 sha256
//	③ 后 4 段缺项写**空串**但分隔符不能省；
//	   但第 10 段（session）在**线上**由本函数填真实 ses_ id（见下）
//	④ method 在这条串里大写（与 v1 串相反 —— 坑⑫）
//	⑤ nonce 是 **24 字节**（与 device/v1 的 18 不同）
//
// ★ 2026-11-14 真机对照（同一个账号、同一把 key、同一份 body，单变量）：
//
//	第 10 段为空 + 不发 session 头 → **500 Internal Error，0 字节**，kong upstream-latency 9~10ms
//	第 10 段 = 真实 ses_ id + 两个 session 头同值 → **200**
//
// 所以线上路径上第 10 段**不许为空**。官方 sidecar 的拦截器（claude-sidecar.exe @99956884）
// 做的是：把内核发来的 `x-phanthy-session` **写成** `x-claude-code-session-id`
// （原头保留不删），再用**改写后**的头去算 canonical。
// 本包等价地在同一处生成 sessionID，canonical 与两个头共用它 —— 三者必然同源。
func (c *Client) buildProof(a *Auth, method, path string, body []byte, token string, localMS int64) (proofValues, error) {
	if a == nil {
		return proofValues{}, errNilAuth
	}
	// ★★ 2026-11-14 加的护栏：inference canonical 第 3 段必须是**不含 query 的 pathname**。
	//
	// 真源：官方 sidecar 的 proof 构造器 `_O8(q, K, _, z)` 里，第 3 段传的是
	// **`K.pathname`**（逐字，不是 `K.path` 也不是整个 url）。
	// 而官方的本地推理桥 `gd3` 入口还要求 query 必须为空
	// （唯一例外 `/v1/messages?beta=true`）—— 两条一起说明：
	// **这条路径上 query 既不进签名、也不该出现。**
	//
	// 本包当前两个调用点都传 `pathMessages` / `pathChatCompletions` 常量（都不带 query），
	// 所以现在是对的；加断言是为了让将来"顺手加个 ?beta=true"当场报错，
	// 而不是签出一个服务端重算不出来的串（症状是 403 request_not_authorized，
	// 与时钟/UA/令牌过期的症状完全一样，指不到根因）。
	if strings.Contains(path, "?") {
		return proofValues{}, fmt.Errorf(
			"phanthywork: inference canonical 的 path 段不能带 query（收到 %q）—— "+
				"官方 proof 构造器用的是 pathname，URL 的 query 既不进签名也不该出现在这条路径上", path)
	}
	ts := msToString(serverNowMS(localMS, a.skew()))
	nonce := newInferenceNonce()
	reqID := randomUUID()
	// ★ session id 在这里生成一次，canonical 第 10 段与两个请求头共用它。
	sessionID := newSessionID()
	authHeader := AuthorizationHeaderValue(a.OAuth.AccessToken)

	// 第 11/12 段（expert / skills）本包不发对应头，留空串占位。
	canonical := InferenceCanonical(
		method, path, ts, nonce, RequestBodyForHash(body),
		authHeader, token,
		reqID, sessionID, "", "")

	sig, err := a.signInferenceCanonical(canonical)
	if err != nil {
		return proofValues{}, err
	}
	return proofValues{
		InferenceToken: token,
		Timestamp:      ts,
		Nonce:          nonce,
		Signature:      sig,
		RequestID:      reqID,
		SessionID:      sessionID,
	}, nil
}

// ── 小工具 ───────────────────────────────────────────────────────────────

// brief 把响应体压成一行短串（日志/错误文案用）。
func brief(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// parseIntRaw 解析可能是数字也可能是字符串的数字字段。
func parseIntRaw(raw json.RawMessage) int64 {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return 0
	}
	if n, err := strconv.ParseInt(strings.Trim(t, `"`), 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(strings.Trim(t, `"`), 64); err == nil {
		return int64(f)
	}
	return 0
}

// retryAfterMS 从响应体里尽力取 retry-after（拿不到就 0）。
func retryAfterMS(body []byte) time.Duration {
	var doc struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(doc.RetryAfter * float64(time.Second))
}

// backoffFor 第 attempt 次重试前的等待（指数 + 上限）。
func backoffFor(attempt int, hint time.Duration) time.Duration {
	base := 500 * time.Millisecond * time.Duration(1<<uint(attempt))
	if hint > base {
		base = hint
	}
	if base > maxBackoff {
		base = maxBackoff
	}
	return base
}
