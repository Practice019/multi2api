// login.go 本上游的页内登录流程（gateway.LoginFlow）。
//
// # 为什么必须有这个文件
//
// 用户报障："zcode 该上游未声明页内登录流程" —— 前端「账号池」不给它渲染
// 「＋ 添加账号」按钮。那个按钮的判据是 `/admin/providers` 里的 `login` 字段，
// 而它**只在上游实现了 `gateway.LoginFlow` 时才有值**（类型断言，不是看名字）。
//
// 没有它，用户只能"手工把凭证文件拷进 auths/zcode/" —— 而界面上
// 完全没有入口提示这件事，看起来就像该上游坏了。
//
// # 抄官方源码（能抄则抄）
//
// 契约逐字来自 `zai-org/ZCode/apps/zcode-cli/packages/adapters/src/auth/cli-oauth.ts`：
//
//	POST {base}/oauth/cli/init    Bearer <pollToken>  {"provider":"zai"}
//	  → data{flow_id, authorize_url, expires_at, poll_interval_sec, poll_token}
//	GET  {base}/oauth/cli/poll/{flowId}  Bearer <pollToken>
//	  → data{status:"pending"|"failed"}
//	  → data{status:"ready", token, user{user_id,email,name,avatar},
//	         zai|bigmodel{access_token, refresh_token?}}
//
// 我的实测（2026-10-06，真实请求）：
//
//	POST /api/v1/oauth/cli/init  → 200 {"code":0,"data":{...}}  ← 端点存在且可用
//	GET  /api/v1/oauth/cli/poll/fake → 400 {"code":3004,"msg":"invalid_flow"}
//
// # 为什么选这条流程（而不是官方桌面端的授权码流程）
//
// 官方桌面端走 `zcode://oauth/callback`（自定义协议回调），需要本机注册
// 协议处理器 —— 那是**桌面应用**的特权，网关做不到。
//
// 而 CLI OAuth 是**服务端中转**的：授权后浏览器被引到 `zcode.z.ai` 的
// 回调地址，再由我们轮询取号。所以它：
//
//	不需要本机监听端口     ↔ 和其它上游的"绑 127.0.0.1 回调"不同
//	不需要浏览器自动化     ↔ 不需要图形界面，服务器部署可用
//	不需要自定义协议       ↔ 网关没法注册协议处理器
//
// 换句话说它是本仓唯一**服务器部署也能完成登录**的浏览器类流程。
//
// # 为什么不支持「手机号登录」
//
// 官方 CLI 流程里没有手机号分支（`CliOAuthProviderId` 只有 `zai` / `bigmodel`）。
// 手机号登录在 Web 端点（`/login`）里，那条要过阿里云滑块 —— 见 captcha.go。
package zcode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// CLI OAuth 的端点路径（抄官方 cli-oauth.ts 的常量拼接）。
const (
	cliOAuthBase = planOrigin + "/api/v1"
	// cliInitPath 申请一次登录，拿到授权 URL 与 flow_id。
	cliInitPath = "/oauth/cli/init"
	// cliPollPathFmt 轮询登录结果（flow_id 要 URL 编码）。
	cliPollPathFmt = "/oauth/cli/poll/%s"
)

// pollTokenBytes 轮询令牌的字节数（抄官方 POLL_TOKEN_BYTES = 32 → 64 hex）。
const pollTokenBytes = 32

// loginTimeout 一次登录的总时限（官方 OAUTH_TIMEOUT_MS = 5 分钟）。
const loginTimeout = 5 * time.Minute

// defaultPollInterval 上游没给 poll_interval_sec 时的兜底（官方线上实测给 2 秒）。
const defaultPollInterval = 2 * time.Second

// oauthProviderID 该部署用哪个 provider 申请登录。
//
// 官方 `CliOAuthProviderId = "zai" | "bigmodel"` —— 两者是**不同平台**，
// 授权页与签发的令牌都不同，所以这个值必须与账号要打的域一致。
//
// 默认取 `zai`（国际版）：它的授权页在 chat.z.ai，用户最多。
// bigmodel 的账号走同一个流程但 provider 参数不同 —— 目前不暴露成配置项，
// 因为本包还没有"多平台同时登录"的需求场景（API Key 通道已覆盖）。
const oauthProviderID = "zai"

// loginEntry 一次进行中的登录。
//
// 与 raccoon 的 loginEntry 同构：状态 + 一次性结果通道 + 互斥。
type loginEntry struct {
	mu   sync.Mutex
	flow string
	// pollToken 轮询凭证。**每个 flow 一个**，由我们生成并随 init 提交。
	//
	// ⚠ 上游会在 init 的响应里回显它（实测），但我们**用自己的那份**：
	// 回显值只是确认，用自己生成的值可以避免"上游回显被改"这类攻击面。
	pollToken string
	// interval 上游给的轮询间隔（它有权限流，所以按它说的来）。
	interval time.Duration
	// authURL 要交给用户打开的授权页。
	authURL string
	// expiresAt 这次 flow 的过期时刻（上游给的）。
	expiresAt time.Time
	// issuedAt 本地签发时刻（用于总超时判断）。
	issuedAt time.Time

	// 结果（一次性）。
	done chan loginResult
	once sync.Once
}

// loginResult 一次登录的终态。
type loginResult struct {
	cred *Auth
	err  error
}

// ---- gateway.LoginFlow ----

// Start 申请一次登录，返回 (state, 授权URL)。
//
// state 就是上游的 flow_id —— 它是本次登录的唯一标识，Poll 按它查。
// 前端把它原样回传，我们不需要另造一个标识（那只会多一处不一致的可能）。
func (p *Provider) Start() (state, authURL string, err error) {
	if !p.Configured() {
		return "", "", fmt.Errorf("zcode: 未配置（zcode.enabled=false）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	entry, err := p.client.startCLIOAuth(ctx)
	if err != nil {
		return "", "", err
	}
	p.putLogin(entry.flow, entry)
	// flow_id 与授权 URL 一起回去。前端会：
	//   auth_url 非空 → 打开/复制链接让用户授权，然后轮询 /admin/login/poll
	return entry.flow, entry.authURL, nil
}

// Poll 查询授权结果。未完成时返回 gateway.ErrLoginPending。
//
// # 为什么轮询要服务端做（而不是让前端等）
//
// 上游的 poll 是 **GET 且要 Bearer pollToken**。把 pollToken 交给前端
// 等于把"能取走凭证的东西"交给浏览器 —— 所以轮询留在服务端，
// 前端只是反复问我们"好了没"。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	entry := p.getLogin(state)
	if entry == nil {
		return gateway.Credential{}, fmt.Errorf("zcode: 未知的登录会话 %q（可能已过期或服务重启过）", state)
	}
	if time.Since(entry.issuedAt) > loginTimeout {
		p.dropLogin(state)
		return gateway.Credential{}, fmt.Errorf("zcode: 登录超时（超过 %s）—— 请重新点「添加账号」", loginTimeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, cred, err := p.client.pollCLIOAuth(ctx, entry)
	switch {
	case err != nil:
		// 网络/上游错误**不消费**这次会话（用户可能只是网络抖了一下）。
		return gateway.Credential{}, err
	case st == cliPollPending:
		return gateway.Credential{}, gateway.ErrLoginPending
	case st == cliPollFailed:
		// 终态：上游明确说这次授权失败了。消费掉，让用户重新申请。
		p.dropLogin(state)
		return gateway.Credential{}, fmt.Errorf("zcode: 上游报告本次授权失败 —— 请重新点「添加账号」")
	default:
		p.dropLogin(state)
		// ⚠ Secret 必须是 `*authFile` 而**不是** `*Auth`。
		//
		// 核心的 pollViaFlow 落盘那一步是类型断言：
		//
		//	mw, ok := cred.Secret.(authFileWriter)  // MarshalAuthFile() (name, raw, err)
		//	if !ok → 501「该上游的凭证结构尚未接入落盘」
		//
		// 直接放 `cred` 会让**浏览器授权成功、然后停在 501** ——
		// 用户看到的是"登录不了"，而上游那边其实已经授权过了。
		//
		// 我在 raccoon 上踩过完全相同的坑（并在那次总结里记下过），
		// 这次又犯了一遍 —— 说明这条约束该由**测试**守，不该靠记性。
		// 见 login_test.go 的 TestPollSecretSupportsAuthFileMarshal。
		return gateway.Credential{
			Provider: providerID,
			UID:      cred.UID,
			Nickname: cred.Nickname,
			Secret:   &authFile{A: cred},
		}, nil
	}
}

// Configured 报告这份部署**真的能**走登录流程吗。
//
// # 为什么不是恒 true（这个方法的判据来自实测踩的坑）
//
// `gateway.LoginFlow` 的注释把这条讲得很清楚：上游为了让类型断言认出自己，
// 必须把 Start/Poll 挂上来，那是**编译期**的事实 —— 与"这次部署有没有
// 配置好"无关。只看有没有实现会导致"没配的部署也看起来能用"，
// 于是前端渲染出点了报错的**假按钮**。
//
// 对本上游，判据是：API 基址可用即可 —— 因为 CLI OAuth 是**服务端中转**的，
// 不需要本机端口、不需要图形界面、也不需要额外配置项。
// 所以"启用即已配置"，这与 raccoon / lobsterai（要绑本机端口）不同。
func (p *Provider) Configured() bool {
	return p != nil && p.client != nil
}

// ---- 会话表 ----

// putLogin 登记一次登录会话。
func (p *Provider) putLogin(flow string, e *loginEntry) {
	p.mu.Lock()
	if p.logins == nil {
		p.logins = map[string]*loginEntry{}
	}
	p.logins[flow] = e
	p.mu.Unlock()
}

// getLogin 取一次登录会话（不在则 nil）。
func (p *Provider) getLogin(flow string) *loginEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.logins[flow]
}

// dropLogin 移除会话（终态或超时后）。
//
// 为什么要移除而不是留着：它持有 pollToken，而 pollToken 能取走凭证。
// 终态之后留着它只有风险没有收益。
func (p *Provider) dropLogin(flow string) {
	p.mu.Lock()
	delete(p.logins, flow)
	p.mu.Unlock()
}

// ---- 客户端侧：CLI OAuth 的两个请求 ----

// cliPollStatus 轮询状态。
type cliPollStatus string

const (
	cliPollPending cliPollStatus = "pending"
	cliPollReady   cliPollStatus = "ready"
	cliPollFailed  cliPollStatus = "failed"
)

// startCLIOAuth 调 init 申请一次登录。
//
// 反序列化的结构逐字对应官方 `CliOAuthInitData`。
// 官方对响应的校验（parseInitData）我也照做了 —— 它校验 authorize_url
// 必须是 **https**，理由很实在：那个 URL 会交给用户打开，
// 一个 `http:` 或 `javascript:` 的授权页是安全问题，不是小瑕疵。
func (c *Client) startCLIOAuth(ctx context.Context) (*loginEntry, error) {
	pollToken, err := randomPollToken()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"provider": oauthProviderID})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.oauthBase+cliInitPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// 官方：`Authorization: Bearer ${input.pollToken}`，**不加任何伪装头**。
	// 这一点值得照抄 —— 其它端点要伪装成客户端，而这个流程不要。
	req.Header.Set("Authorization", "Bearer "+pollToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zcode: 申请登录失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("zcode: 申请登录 HTTP %d: %s", resp.StatusCode, firstLine(errMessage(raw)))
	}

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AuthorizeURL    string `json:"authorize_url"`
			ExpiresAt       int64  `json:"expires_at"`
			FlowID          string `json:"flow_id"`
			PollIntervalSec int    `json:"poll_interval_sec"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("zcode: 申请登录的响应不是合法 JSON: %w", err)
	}
	// ⚠ 必须按业务码判（本上游的失败可能是 HTTP 200 + code != 0）。
	if env.Code != 0 {
		msg := strings.TrimSpace(env.Msg)
		if msg == "" {
			msg = fmt.Sprintf("code %d", env.Code)
		}
		return nil, fmt.Errorf("zcode: 申请登录被上游拒绝: %s", msg)
	}
	if strings.TrimSpace(env.Data.FlowID) == "" || strings.TrimSpace(env.Data.AuthorizeURL) == "" {
		return nil, fmt.Errorf("zcode: 申请登录的响应缺少 flow_id 或 authorize_url")
	}
	// 授权页必须是 https（抄官方的判据，理由见函数注释）。
	u, err := url.Parse(env.Data.AuthorizeURL)
	if err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("zcode: 上游给的授权 URL 不是 https（拒绝打开）: %q", env.Data.AuthorizeURL)
	}

	interval := defaultPollInterval
	if env.Data.PollIntervalSec > 0 {
		interval = time.Duration(env.Data.PollIntervalSec) * time.Second
	}
	return &loginEntry{
		flow:      env.Data.FlowID,
		pollToken: pollToken,
		interval:  interval,
		authURL:   env.Data.AuthorizeURL,
		expiresAt: time.Unix(env.Data.ExpiresAt, 0),
		issuedAt:  time.Now(),
	}, nil
}

// pollCLIOAuth 查一次登录结果。
//
// 三种返回（与官方 `CliOAuthPollData` 一致）：
//
//	pending  → 还没授权完，调用方继续等
//	ready    → 拿到凭证
//	failed   → 这次授权失败（终态）
func (c *Client) pollCLIOAuth(ctx context.Context, e *loginEntry) (cliPollStatus, *Auth, error) {
	target := c.oauthBase + fmt.Sprintf(cliPollPathFmt, url.PathEscape(e.flow))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.pollToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("zcode: 查询登录结果失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, err
	}

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Status string `json:"status"`
			// Token 就是 AI 调用与计费用的 JWT（官方 `data.token`）。
			Token string `json:"token"`
			User  struct {
				UserID string `json:"user_id"`
				Email  string `json:"email"`
				Name   string `json:"name"`
			} `json:"user"`
			// provider 段是**动态键**（zai / bigmodel），所以用 map 收。
			Zai      *cliOAuthProviderToken `json:"zai"`
			BigModel *cliOAuthProviderToken `json:"bigmodel"`
		} `json:"data"`
	}

	// ⚠ 先解一次信封（下面所有分支都基于它判断）。
	//
	// 早先的写法在错误分支里也解一次，属于重复劳动 —— 而且那样会让
	// "HTTP 4xx 但 body 是合法信封"的路径走两遍解析。
	if err := json.Unmarshal(raw, &env); err != nil {
		// 解不出信封且 HTTP 也是错的 → 报传输层错误（带着状态码与原文）。
		if resp.StatusCode >= 400 {
			return "", nil, fmt.Errorf("zcode: 查询登录结果 HTTP %d: %s",
				resp.StatusCode, firstLine(string(raw)))
		}
		return "", nil, fmt.Errorf("zcode: 查询登录结果的响应不是合法 JSON: %w", err)
	}

	// ⚠ 轮询期间的失败要分两类：
	//
	//	invalid_flow（3004）  → 终态。这次会话已经无效（过期 / 服务重启），
	//	                        重试一万次也一样 → 报 failed 让用户重新申请
	//	其它非零 code         → 上游侧的错误，如实上报（不消费会话）
	if env.Code != 0 {
		if env.Code == codeInvalidFlow {
			return cliPollFailed, nil, nil
		}
		if resp.StatusCode >= 400 && env.Code == 0 {
			return "", nil, fmt.Errorf("zcode: 查询登录结果 HTTP %d", resp.StatusCode)
		}
		return "", nil, fmt.Errorf("zcode: 查询登录结果被上游拒绝（code %d）: %s",
			env.Code, firstLine(env.Msg))
	}
	if resp.StatusCode >= 400 {
		// 业务码是 0 但 HTTP 是错误状态 —— 契约之外的情形，如实报。
		return "", nil, fmt.Errorf("zcode: 查询登录结果 HTTP %d（但业务码为 0）: %s",
			resp.StatusCode, firstLine(string(raw)))
	}

	switch cliPollStatus(strings.ToLower(strings.TrimSpace(env.Data.Status))) {
	case cliPollPending:
		return cliPollPending, nil, nil
	case cliPollFailed:
		return cliPollFailed, nil, nil
	case cliPollReady:
		// 官方 parseReadyData 的校验：token 与 user_id 与 access_token 都非空。
		// 少任何一个都说明上游行为和契约不符 —— 报错比存一份半残凭证好。
		if strings.TrimSpace(env.Data.Token) == "" {
			return "", nil, fmt.Errorf("zcode: 上游报告 ready 但没有 token")
		}
		if strings.TrimSpace(env.Data.User.UserID) == "" {
			return "", nil, fmt.Errorf("zcode: 上游报告 ready 但没有 user_id")
		}
		pt := env.Data.Zai
		if pt == nil {
			pt = env.Data.BigModel
		}
		access := ""
		refresh := ""
		if pt != nil {
			access = pt.AccessToken
			refresh = pt.RefreshToken
		}
		if strings.TrimSpace(access) == "" {
			return "", nil, fmt.Errorf("zcode: 上游报告 ready 但没有 access_token")
		}

		a := &Auth{
			Kind:         CredKindJWT,
			JWT:          strings.TrimSpace(env.Data.Token),
			RefreshToken: strings.TrimSpace(refresh),
			UID:          strings.TrimSpace(env.Data.User.UserID),
			Nickname:     loginNickname(env.Data.User.Name, env.Data.User.Email, env.Data.User.UserID),
			// 验证码区域随账号存下来（见 Auth.CaptchaRegion 的注释）。
			CaptchaRegion: "cn",
		}
		// JWT 的过期时刻从 token 里解（本上游没有单独的 expires 字段）。
		if exp := jwtExpiry(a.JWT); exp > 0 {
			a.ExpiresAt = exp
		}
		return cliPollReady, a, nil
	}

	// 未知状态：按 pending 处理更安全的反面 —— 这里选**报错**。
	// 理由：未知状态意味着契约变了，静默按 pending 会让用户一直等到超时，
	// 而明确的报错能立刻指出"上游改了协议"。
	return "", nil, fmt.Errorf("zcode: 上游返回了未知的登录状态 %q（契约可能已变）", env.Data.Status)
}

// cliOAuthProviderToken provider 段（zai / bigmodel 同形）。
type cliOAuthProviderToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// randomPollToken 生成 32 字节的 hex（抄官方 createCliOAuthPollToken）。
//
// 用 crypto/rand：它是**取走凭证的凭证** —— 可预测的话别人能抢在我们前面
// 把账号拿走。这不是"随机数不够随机"的美观问题，是安全问题。
func randomPollToken() (string, error) {
	b := make([]byte, pollTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("zcode: 生成轮询令牌失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// loginNickname 给登录得到的账号起一个能区分的展示名。
//
// 上游可能三个字段都空（只给了 user_id），所以兜底用 user_id。
// 若只显示 user_id，多账号时那一列会是几串无意义的数字 ——
// 而它恰恰是排障时第一眼要看的东西。
func loginNickname(name, email, uid string) string {
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	if s := strings.TrimSpace(email); s != "" {
		return s
	}
	return "ZCode ·" + shortTail(uid)
}

// shortTail 取字符串尾部 6 位（不足以区分时返回原串）。
func shortTail(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[len(s)-6:]
}

// jwtExpiry 从 JWT 里解出 exp（秒）。解不出返回 0。
//
// # ⚠ zcode 的 JWT **真的没有 exp**（实测，不是我们没解出来）
//
// 解码一个真实登录得到的 token，payload 只有：
//
//	{"user_id":"<uuid>","token_version":0,"sub":"<uuid>","iat":1700000000}
//
// 没有 `exp`。而官方自己的 `resolveJwtExpiration`
// （packages/shared/src/oauth.ts）对这种情况也返回 `"unknown"`：
//
//	if (typeof payload.exp !== "number" || !Number.isFinite(payload.exp) || …)
//	  return { kind: "unknown" }
//
// 所以"没有 exp"是**上游的凭证形态**，不是解析缺陷 ——
// 令牌的真实有效期由服务端决定（`token_version` 字段暗示服务端可撤销）。
//
// 由此推出一条**列声明上的纪律**：本上游不该声明依赖过期时刻的列
// （Token / Token 到期）—— 那种列会永远是 `—`，而用户把 `—` 读成
// "这功能没做"。见 extensions.go 的 AccountColumns。
//
// 函数本身保留的两个理由：① token 形态可能变（上游加 exp 就自动生效）；
// ② 读本地时间戳不需要信任签名，所以不做签名校验。
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.Exp
}
