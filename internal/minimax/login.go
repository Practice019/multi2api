// login.go MiniMax 的设备码 + PKCE 登录（gateway.LoginFlow）。
//
// # 为什么用设备码而不是本地回调
//
// 官方桌面端就是这么登的（参照项目 `minimax-oauth.ts` 复刻自 asar 的
// `@mavis/oauth-core`）：申请设备码 → 用户在浏览器完成授权 → 我们轮询换 token。
// **不起本地监听端口**（与 qoder 同型，与 workbuddy/lobsterai 不同）。
//
// 对本仓的实际好处：**服务器部署也能完成登录** —— 不需要图形界面、
// 不需要本机端口、不需要自定义协议回调。
//
// # ⚠ 最容易踩的坑：`pending` 是 **HTTP 200**
//
// OAuth 标准的设备码轮询用 `400 + error=authorization_pending` 表示"还在等"，
// 而 MiniMax 的账号服务用 **`200 + status=pending`**。
//
// 只认标准形态会**立刻抛错**，用户来不及授权；只认非标准形态则会在
// 别人实现的标准上游上卡住。所以**两种形态都必须认**（参照项目的
// `pollMinimaxDeviceToken` 就是分开处理的，逐行对齐）。
//
// 症状上，这个坑表现为「令牌响应缺少 access_token」—— 因为
// `200 + pending` 被当成了"拿到 token"，然后去解析一个没有 token 的响应。
package minimax

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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

// 登录会话的常量。
const (
	// deviceCodeTTL 设备码有效期上限（上游会给 expires_in，这是兜底）。
	deviceCodeTTL = 15 * time.Minute
	// defaultPollInterval 上游没给 interval 时的轮询间隔（参照项目缺省 5 秒）。
	defaultPollInterval = 5 * time.Second
	// slowDownStep `slow_down` 时增加的间隔（参照项目是 +5000ms）。
	slowDownStep = 5 * time.Second
	// loginTimeout 整个登录会话的上限。
	loginTimeout = 15 * time.Minute
)

// loginEntry 一次登录会话（一个 state 一份）。
type loginEntry struct {
	mu sync.Mutex
	// deviceCode 申请设备码拿到的 device_code（轮询要带它）。
	deviceCode string
	// codeVerifier PKCE 的 verifier（轮询要带它）。
	codeVerifier string
	// authURL 给用户打开的授权链接。
	authURL string
	// expiresAt 会话过期时刻。
	expiresAt time.Time
	// interval 当前轮询间隔（slow_down 会增加）。
	interval time.Duration
	// done 是否已消费（拿到凭证或失败）。
	done bool
	// once 保证 Poll 成功时只在回调里走一次。
	once sync.Once
	// issuedAt 会话创建时刻（回执里报 expires_in 用）。
	issuedAt time.Time
}

// cliPollStatus 轮询返回的三种状态（本包内部词汇）。
const (
	pollPending = "pending"
	pollReady   = "ready"
	pollFailed  = "failed"
)

// ---- gateway.LoginFlow ----

// Start 发起一次设备码登录。
//
// 返回 (state, authURL, err)：
//
//	state    本会话的标识（**就是上游的 device_code**）—— Poll 按它查
//	authURL  给用户打开的授权链接（优先用带 user_code 的完整链接）
func (p *Provider) Start() (string, string, error) {
	if p == nil || p.client == nil {
		return "", "", fmt.Errorf("minimax: 未配置，无法登录")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	auth, err := p.client.startDeviceAuth(ctx)
	if err != nil {
		return "", "", err
	}
	// ⚠ state 用 device_code（上游的主键）。
	//
	// 不用"我们自己生成的随机串"：那样 Poll 时还要维护一张
	// state→device_code 的映射，多一层可以不同步的状态。
	if strings.TrimSpace(auth.DeviceCode) == "" {
		return "", "", fmt.Errorf("minimax: 上游没有返回 device_code")
	}
	ttl := time.Duration(auth.ExpiresInSec) * time.Second
	if ttl <= 0 || ttl > deviceCodeTTL {
		ttl = deviceCodeTTL
	}
	e := &loginEntry{
		deviceCode:   auth.DeviceCode,
		codeVerifier: auth.CodeVerifier,
		authURL:      auth.VerificationURIComplete,
		expiresAt:    time.Now().Add(ttl),
		interval:     time.Duration(auth.IntervalSec) * time.Second,
		issuedAt:     time.Now(),
	}
	if e.interval <= 0 {
		e.interval = defaultPollInterval
	}
	p.putLogin(auth.DeviceCode, e)
	return auth.DeviceCode, e.authURL, nil
}

// Poll 查询一次登录结果。
//
// 未完成时返回 `gateway.ErrLoginPending` —— **不是** error：
// 返回 error 会让前端把"等待中"渲染成"失败"，用户看到红色报错就放弃了。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	e := p.getLogin(state)
	if e == nil {
		return gateway.Credential{}, fmt.Errorf("minimax: 未知的登录会话（可能已过期，请重新发起）")
	}
	e.mu.Lock()
	if e.done {
		e.mu.Unlock()
		p.dropLogin(state)
		return gateway.Credential{}, fmt.Errorf("minimax: 该登录会话已完成（请重新发起）")
	}
	if time.Now().After(e.expiresAt) {
		e.done = true
		e.mu.Unlock()
		p.dropLogin(state)
		return gateway.Credential{}, fmt.Errorf("minimax: 设备码已过期，请重新发起登录")
	}
	interval := e.interval
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	res, err := p.client.pollDeviceToken(ctx, e.deviceCode, e.codeVerifier)
	if err != nil {
		// 网络/协议错误**不消费会话** —— 用户可能只是抖了一下。
		return gateway.Credential{}, err
	}
	switch res.Status {
	case pollPending:
		return gateway.Credential{}, gateway.ErrLoginPending
	case "slow_down":
		e.mu.Lock()
		e.interval = interval + slowDownStep
		e.mu.Unlock()
		return gateway.Credential{}, gateway.ErrLoginPending
	case pollFailed:
		e.mu.Lock()
		e.done = true
		e.mu.Unlock()
		p.dropLogin(state)
		return gateway.Credential{}, fmt.Errorf("minimax: %s", res.Message)
	}

	if res.Auth == nil || !res.Auth.Usable() {
		return gateway.Credential{}, fmt.Errorf("minimax: 令牌响应缺少 access_token")
	}
	e.mu.Lock()
	e.done = true
	e.mu.Unlock()
	p.dropLogin(state)

	a := res.Auth
	if a.TokenType == "" {
		a.TokenType = "Bearer"
	}
	// 用户名：拿不到就留空（账号池会回落显示 token 短哈希）。
	if nick := strings.TrimSpace(res.Nickname); nick != "" {
		a.Nickname = nick
	}

	// ---- 判重：同一个号**更新已有那条**，而不是新增一条 ----
	//
	// 用户实测报的缺陷：同一个 MiniMax 账号登录两次 ⇒ 账号池里两条。
	// 根因是我把 uid 建成 access_token 的哈希，而重登必然换发新令牌。
	//
	// 上游不给账号身份（实测 9 个可能的端点全 404、可用端点的真实响应里
	// 也没有任何 id 字段），所以这里改用**账号作用域的数据指纹**做证据：
	// 同一个号的两条不同令牌，打积分端点返回逐字段一致的桶标识。
	//
	// ⚠ 三条硬约束（见 accountkey.go 文件头）：
	//	· 指纹为空 ⇒ 不合并（否则两个"零积分的不同账号"会被并成一个）
	//	· 判不准 ⇒ 保持新增（可逆，用户能删；覆盖别人的凭证不可逆）
	//	· 只在登录落盘前做一次，不进任何每请求路径
	if matched, ok := p.dedupIdentity(a); ok {
		a.Identity = matched
		// 昵称继承已有的那条 —— 否则重登会把用户认得出的名字
		// 变成陌生的哈希前缀（上游不下发昵称时必然发生）。
		if strings.TrimSpace(a.Nickname) == "" {
			if old := p.cred(matched); old != nil {
				a.Nickname = old.Nickname
			}
		}
		logf("minimax: 登录判重命中已有账号 uid=%s ⇒ 更新它（不新增）", matched)
	} else {
		logf("minimax: 登录判重未命中（新账号或指纹不足）⇒ 作为新账号加入")
	}

	return gateway.Credential{
		Provider: providerID,
		UID:      a.UID(),
		Nickname: a.DisplayName(),
		// ⚠ Secret 必须是 `*authFile`（能 MarshalAuthFile 的包装），
		// **不是**裸 `*Auth` —— 核心落盘那一步是类型断言，
		// 裸 Auth 会让"授权成功后停在 501 该上游的凭证结构尚未接入落盘"。
		// 本仓在 raccoon 与 zcode 上各踩过一次。
		Secret: &authFile{A: a},
	}, nil
}

// Configured 报告这份部署**真的能**走登录流程吗。
//
// 判据与 zcode 一致：实例与它的 client 都在就算能 —— 设备码登录
// **不需要额外配置**（没有"回调地址"之类要用户填的东西）。
//
// ⚠ 核心不只看 `ExtOf` 是否成功，还要 `Configured()` 为真才下发 login 字段。
// 只看前者会在"没配好的部署"上渲染出一个点了报错的假按钮。
func (p *Provider) Configured() bool {
	return p != nil && p.client != nil
}

// ---- 会话状态 ----

func (p *Provider) putLogin(state string, e *loginEntry) {
	p.mu.Lock()
	if p.logins == nil {
		p.logins = map[string]*loginEntry{}
	}
	p.logins[state] = e
	p.mu.Unlock()
}

func (p *Provider) getLogin(state string) *loginEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.logins[state]
}

func (p *Provider) dropLogin(state string) {
	p.mu.Lock()
	delete(p.logins, state)
	p.mu.Unlock()
}

// ---- 协议实现 ----

// deviceAuth 一次设备码申请的结果。
type deviceAuth struct {
	DeviceCode              string
	CodeVerifier            string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresInSec            int
	IntervalSec             int
}

// startDeviceAuth 申请设备码（PKCE S256）。
func (c *Client) startDeviceAuth(ctx context.Context) (*deviceAuth, error) {
	verifier, challenge, err := newPKCE()
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("scope", oauthScope)
	form.Set("audience", oauthAudience)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")

	raw, status, err := c.postForm(ctx, c.accountBase+pathDeviceCode, form)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("minimax: 设备码申请失败（HTTP %d）：%s", status, snippet(raw))
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("minimax: 设备码响应不是合法 JSON: %w", err)
	}
	// ⚠ `verification_uri` 在参照实现里**回退** `verification_url`
	//（两个名字都出现过）；`verification_uri_complete` 回退到前者。
	verification := firstNonEmpty(str(m["verification_uri"]), str(m["verification_url"]))
	complete := firstNonEmpty(str(m["verification_uri_complete"]), verification)
	deviceCode := str(m["device_code"])
	if deviceCode == "" || verification == "" {
		return nil, fmt.Errorf("minimax: 设备码响应缺字段（device_code=%q verification_uri=%q）",
			deviceCode, verification)
	}
	// ⚠ 授权链接会交给用户打开 —— 必须是 https。
	if !strings.HasPrefix(complete, "https://") {
		return nil, fmt.Errorf("minimax: 上游给的授权链接不是 https（拒绝打开）: %q", complete)
	}
	expires := intOf(m["expires_in"])
	interval := intOf(m["interval"])
	if interval <= 0 {
		interval = int(defaultPollInterval / time.Second)
	}
	return &deviceAuth{
		DeviceCode:              deviceCode,
		CodeVerifier:            verifier,
		UserCode:                str(m["user_code"]),
		VerificationURI:         verification,
		VerificationURIComplete: complete,
		ExpiresInSec:            expires,
		IntervalSec:             interval,
	}, nil
}

// pollResult 一次轮询的结果。
type pollResult struct {
	Status   string
	Message  string
	Auth     *Auth
	Nickname string
}

// pollDeviceToken 轮询一次令牌端点。
//
// ⚠ **两种"还在等"的形态都要认**（本文件头那段注释）：
//
//	HTTP 200 + status:"pending"                非标准（MiniMax 用的）
//	非 200  + error:"authorization_pending"     标准 OAuth
//
// 其余分支与参照项目逐行对齐：slow_down / denied / expired。
func (c *Client) pollDeviceToken(ctx context.Context, deviceCode, codeVerifier string) (pollResult, error) {
	form := url.Values{}
	form.Set("grant_type", deviceCodeGrant)
	form.Set("device_code", deviceCode)
	form.Set("client_id", oauthClientID)
	form.Set("code_verifier", codeVerifier)

	raw, status, err := c.postForm(ctx, c.accountBase+pathToken, form)
	if err != nil {
		return pollResult{}, err
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)

	st := strings.TrimSpace(str(m["status"]))
	er := strings.TrimSpace(str(m["error"]))
	ok := status == http.StatusOK

	// ---- 非标准形态：HTTP 200 + status ----
	if ok && st == "pending" {
		return pollResult{Status: pollPending}, nil
	}
	if ok && st == "slow_down" {
		return pollResult{Status: "slow_down"}, nil
	}
	if ok && (st == "denied" || st == "access_denied") {
		return pollResult{Status: pollFailed, Message: "用户拒绝了授权"}, nil
	}
	if ok && (st == "expired" || st == "expired_token") {
		return pollResult{Status: pollFailed, Message: "设备码已过期，请重新发起登录"}, nil
	}

	// ---- 标准形态：error ----
	if er == "authorization_pending" {
		return pollResult{Status: pollPending}, nil
	}
	if er == "slow_down" {
		return pollResult{Status: "slow_down"}, nil
	}
	if ok {
		auth, perr := parseTokenGrant(m, "")
		if perr != nil {
			return pollResult{Status: pollFailed, Message: perr.Error()}, nil
		}
		return pollResult{Status: pollReady, Auth: auth, Nickname: nicknameOf(m)}, nil
	}
	// 非 200 且不是"还在等"：如实报错（带上上游的说明）。
	msg := firstNonEmpty(er, fmt.Sprintf("HTTP %d", status))
	return pollResult{Status: pollFailed, Message: "授权失败：" + msg}, nil
}

// parseTokenGrant 解析令牌响应。
//
// 硬校验（照抄参照项目 `parseMinimaxTokenGrant`，不满足即抛）：
//
//	access_token 非空
//	refresh_token 非空（缺失时回退上一份）
//	token_type.toLowerCase() == "bearer"
//	expires_in 是正数
//	scope 必须含产品声明的 scope（`agent.default`）
//
// ⚠ **过期时间以 `expires_in` 为准，不能指望从 token 里解** ——
// 实测 access_token **不是 JWT**（`mmoat_` 前缀、60 字符、0 个点）。
// 若改成"优先解 JWT、解不出就不写 expires_at"，过期时间会彻底丢失、
// 账号永远显示"未知"。
func parseTokenGrant(m map[string]any, previousRefresh string) (*Auth, error) {
	access := strings.TrimSpace(str(m["access_token"]))
	if access == "" {
		return nil, fmt.Errorf("令牌响应缺少 access_token")
	}
	refresh := strings.TrimSpace(str(m["refresh_token"]))
	if refresh == "" {
		refresh = previousRefresh
	}
	if refresh == "" {
		return nil, fmt.Errorf("令牌响应缺少 refresh_token")
	}
	tt := strings.ToLower(strings.TrimSpace(str(m["token_type"])))
	if tt != "bearer" {
		return nil, fmt.Errorf("令牌响应的 token_type 不是 Bearer（实际 %q）", tt)
	}
	expiresIn := intOf(m["expires_in"])
	if expiresIn <= 0 {
		return nil, fmt.Errorf("令牌响应缺少 expires_in")
	}
	scope := strings.TrimSpace(str(m["scope"]))
	if !hasScope(scope, oauthScope) {
		return nil, fmt.Errorf("令牌响应的 scope 不含 %s（实际 %q）", oauthScope, scope)
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(expiresIn) * time.Second).UnixMilli()
	return &Auth{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		// ⚠ 毫秒时间戳的**字符串**（参照项目的原形状就是 string）。
		ExpiresAt: fmt.Sprintf("%d", expiresAt),
		// 记下发证时刻 ⇒ 寿命可推（RefreshSkew 按比例算窗口要用）。
		IssuedAt: fmt.Sprintf("%d", now.UnixMilli()),
		Scope:    scope,
	}, nil
}

// nicknameOf 从令牌响应里尽量取一个展示名。
//
// ⚠ 取不到就返回空 —— **不编造**（账号池会回落显示 token 短哈希）。
// 参照项目实测真实凭据里没有可用的账号名。
func nicknameOf(m map[string]any) string {
	for _, k := range []string{"nickname", "name", "user_name", "username", "email"} {
		if s := strings.TrimSpace(str(m[k])); s != "" {
			return s
		}
	}
	return ""
}

// hasScope scope 串（空格分隔）里有没有目标项。
func hasScope(scope, want string) bool {
	for _, f := range strings.Fields(scope) {
		if f == want {
			return true
		}
	}
	return false
}

// newPKCE 生成 PKCE 的 verifier 与 challenge（S256）。
func newPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("minimax: 生成 PKCE 失败: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// postForm 发一个 form-urlencoded POST。
func (c *Client) postForm(ctx context.Context, fullURL string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header = oauthHeaders()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

// str 宽容地取字符串。
func str(v any) string {
	s, _ := v.(string)
	return s
}

// firstNonEmpty 取第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// intOf 宽容地取整数。
func intOf(v any) int {
	if f, ok := toFloat(v); ok {
		return int(f)
	}
	return 0
}
