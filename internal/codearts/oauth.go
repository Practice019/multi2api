// Package oauth 实现 CodeArts Agent 的 OAuth 2.0 授权（PKCE + DPoP）。
//
// 与 CodeBuddy 设备授权的差异：
//   - CodeBuddy: 服务端签发 state，轮询换 token（无 PKCE）
//   - CodeArts : 标准 Authorization Code + PKCE，且 token 端点**要求 DPoP 证明**
//
// 为什么要服务端实现这套流程（而不是让用户手工贴凭证）：
// 只有走完整授权才能拿到 refresh_token 与 DPoP 私钥，
// 而这两者是**自动续期的前提** —— CodeArts 的 STS 凭证只有约 30 分钟寿命，
// 没有 refresh_token 就只能每半小时重新登录一次。
//
// 流程（逆向自 vscode-codebot 扩展的 WebLoginStrategy）：
//
//  1. 生成 PKCE (verifier 128hex / challenge base64url(SHA256))
//  2. 生成 DPoP 密钥对 (ES256 / P-256)
//  3. 起本地回调服务器 127.0.0.1:<随机端口> /oauth/callback
//  4. 浏览器打开 /portal/authorize?...&code_challenge=...
//  5. 回调拿到 code
//  6. POST /v1/oauth2/tokens (DPoP 签名) 换取 AK/SK/securityToken + refresh_token
//  7. 落盘 auths/codearts-<uid>.json（含 DPoP 私钥，供后续续期）
package codearts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 授权流程特有的常量。
//
// ⚠ 移植时我用了一句正则 `\bcodearts\.` 去包名前缀（替换成空串），
// 结果把下面这个 URL 里的 `codearts.` **也吃掉了**，
// 变成 `https://huaweicloud.com` —— 一个能编译、能跑、
// 但授权页地址完全错误的 bug。
//
// 教训：**跨包改名不能用无差别正则**。域名里恰好含有包名时，
// 盲替换会静默改坏数据（而不是报错）。
const (
	// DefaultPortalBase 是授权页所在站点。
	DefaultPortalBase = "https://codearts.huaweicloud.com"
	// CallbackPath 是本地回调路径。
	CallbackPath = "/oauth/callback"
	// DefaultClientID 是扩展名（client_id 取 package.json 的 name）。
	DefaultClientID = "vscode-codebot"
)

// ⚠ `DefaultSTSBase` 与 `TokenPath` **刻意不在上面重复声明** ——
// client.go 已有同名同值的定义（移植时实测两者逐字相同）：
//
//	client.go:39  DefaultSTSBase = "https://sts.cn-north-4.myhuaweicloud.com"
//	client.go:44  TokenPath      = "/v1/oauth2/tokens"
//
// 重复声明会编译报错（redeclared），而**两份定义将来会漂移** ——
// 续期路径与授权路径若指向不同端点，表现是"登录成功但半小时后必须重登"，
// 且不会有明显报错。所以只保留 client.go 那一份。

// PKCE 是授权码流程的防拦截对。
type PKCE struct {
	Verifier  string
	Challenge string
	Method    string
}

// GeneratePKCE 生成 PKCE 对。
//
// verifier 必须是 128 个 hex 字符（对应 64 随机字节），
// challenge 是 verifier 的 SHA256 再 base64url（无 padding）。
// 这两点与服务端校验严格对齐，改长度会被拒。
func GeneratePKCE() (PKCE, error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return PKCE{}, fmt.Errorf("生成 PKCE 随机数: %w", err)
	}
	verifier := fmt.Sprintf("%x", buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCE{Verifier: verifier, Challenge: challenge, Method: "SHA-256"}, nil
}

// Session 是一次进行中的授权会话。
type Session struct {
	State     string // 本地标识（回调时校验，防串号）
	AuthURL   string // 给用户打开的授权页
	CreatedAt time.Time

	pkce      PKCE
	dpopPriv  json.RawMessage // DPoP 私钥 JWK，换 token 与后续续期都要用
	callbackS *http.Server
	resultCh  chan callbackResult
	once      sync.Once
}

type callbackResult struct {
	Code string
	Err  error
}

// TTL 是会话存活时间。上游 authorize 页的 code 有独立有效期，
// 这里取保守值，超时提示重新发起而不是拿废 code 去换。
const TTL = 15 * time.Minute

// credentialResponse 是 token 端点的响应。
type credentialResponse struct {
	Credentials *struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		Expiration      string `json:"expiration"`
	} `json:"credentials"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// Manager 管理多个进行中的授权会话。
type Manager struct {
	PortalBase string
	STSBase    string
	ClientID   string
	HTTP       *http.Client

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager 构造管理器（空值走默认）。
func NewManager(portalBase, stsBase, clientID string) *Manager {
	if portalBase == "" {
		portalBase = DefaultPortalBase
	}
	if stsBase == "" {
		stsBase = DefaultSTSBase
	}
	if clientID == "" {
		clientID = DefaultClientID
	}
	return &Manager{
		PortalBase: strings.TrimRight(portalBase, "/"),
		STSBase:    strings.TrimRight(stsBase, "/"),
		ClientID:   clientID,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		sessions:   make(map[string]*Session),
	}
}

// CloseCallback 关掉本地回调监听（幂等）。
//
// # 为什么需要它（源实现里没有）
//
// 源实现只在 `Wait()` 返回时关掉回调服务器。那是**命令行**的用法：
// 进程跑一次、等一次、退出。所以不存在"会话没等到回调就没人管"的情况。
//
// 接进**常驻进程**后多了两条路径：
//   - 用户点了「添加账号」但从未去授权（会话超时）
//   - 用户点了多次，留下若干并发会话
//
// 这些会话各自占着一个监听端口（`net.Listen("tcp", "127.0.0.1:0")`），
// 不主动关就是**端口泄漏** —— 表现为运行几天后"添加账号"开始报
// "监听本地回调端口失败：address already in use"。
//
// 幂等：`Close()` 对已关闭的 Server 返回 ErrServerClosed，忽略即可。
func (s *Session) CloseCallback() {
	if s == nil || s.callbackS == nil {
		return
	}
	_ = s.callbackS.Close()
}

// HTTPClient 暴露内部 HTTP 客户端（供 loginManager 接口用）。
//
// 加这一层是为了**不让适配器直接读导出字段** —— 那会把 Manager 的
// 内部表示变成事实上的公共契约。方法可以被替换/包装，字段不能。
//
// nil 安全：未初始化的 Manager 返回 nil，调用方判空即可。
// （零值 Manager 不该被使用，但"读一个字段"不该 panic。）
func (m *Manager) HTTPClient() *http.Client {
	if m == nil {
		return nil
	}
	return m.HTTP
}

// 编译期断言：*Manager 与 *Session 必须满足适配器的窄接口。
//
// 这两行是**契约检查** —— 若将来给接口加了方法而实现没跟上，
// 会在这里编译失败，而不是在运行期某个分支里静默失效。
var (
	_ loginManager       = (*Manager)(nil)
	_ loginSessionHandle = (*Session)(nil)
)

// Start 开始一次授权：生成 PKCE/DPoP、起本地回调服务器、返回授权 URL。
//
// 返回的 Session.State 用于后续 Poll 查询。
func (m *Manager) Start() (*Session, error) {
	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, err
	}

	// DPoP 密钥对由 codearts 包生成（复用其 JWK 序列化逻辑），
	// 这里通过回调注入，避免 oauth 反向依赖 codearts 造成包环。
	kp, err := newDPoPKeyPair()
	if err != nil {
		return nil, fmt.Errorf("生成 DPoP 密钥对: %w", err)
	}

	// 监听随机端口：0 让内核分配。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("监听本地回调端口: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath)

	s := &Session{
		State:     randomState(),
		CreatedAt: time.Now(),
		pkce:      pkce,
		dpopPriv:  privateJWKJSON(kp),
		resultCh:  make(chan callbackResult, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "缺少 code 参数")
			s.deliver(callbackResult{Err: errors.New("回调未携带 authorization code")})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<h2>授权成功，可以关闭此页面</h2>")
		s.deliver(callbackResult{Code: code})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.callbackS = srv
	go func() { _ = srv.Serve(ln) }()

	// authorize URL。ticket_id 用随机 UUID —— 实测授权页不校验它
	// （无值/随机值/垃圾串返回完全一致），它只是前端 SPA 的路由参数。
	authURL := m.PortalBase + "/portal/authorize?" + url.Values{
		"locale":                {"zh-cn"},
		"uri_scheme":            {m.ClientID},
		"client_id":             {m.ClientID},
		"port":                  {fmt.Sprint(port)},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {pkce.Method},
		"ticket_id":             {randomState()},
		"auth_callback_url":     {callbackURL},
		"plugin-name":           {"codemate_vscode"},
		"plugin-version":        {"26.9.100"},
	}.Encode()
	s.AuthURL = authURL

	m.mu.Lock()
	m.gcLocked()
	m.sessions[s.State] = s
	m.mu.Unlock()

	return s, nil
}

func (s *Session) deliver(r callbackResult) {
	s.once.Do(func() { s.resultCh <- r })
}

// Wait 阻塞等待回调（最多 TTL）。
// 拿到 code 后由调用方调 Manager.Exchange 换取凭证。
func (s *Session) Wait() (string, error) {
	defer func() {
		if s.callbackS != nil {
			_ = s.callbackS.Close()
		}
	}()
	select {
	case r := <-s.resultCh:
		return r.Code, r.Err
	case <-time.After(TTL):
		return "", errors.New("授权超时（15 分钟内未完成），请重新发起")
	}
}

// Exchange 用 authorization code 换凭证。
//
// 返回的 raw 可直接落盘为 auths/codearts-<uid>.json。
func (m *Manager) Exchange(s *Session, code string) (json.RawMessage, error) {
	kp, err := loadDPoPKeyPair(s.dpopPriv)
	if err != nil {
		return nil, err
	}
	tokenURL := m.STSBase + TokenPath
	form := url.Values{
		"client_id":     {m.ClientID},
		"code":          {code},
		"code_verifier": {s.pkce.Verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {callbackURLOf(s)},
	}
	proof, err := kp.DPoPProof(http.MethodPost, tokenURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)

	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 token 端点: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("换 token 失败 (http %d): %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var cr credentialResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, fmt.Errorf("解析 token 响应: %w", err)
	}
	if cr.Credentials == nil || cr.Credentials.AccessKeyID == "" {
		return nil, fmt.Errorf("响应缺少 credentials: %s", truncate(string(raw), 300))
	}

	// 组装落盘结构。DPoP 私钥必须一起存 —— 没有它后续无法续期。
	var expiresAt int64
	if cr.Credentials.Expiration != "" {
		if ts, perr := time.Parse(time.RFC3339, cr.Credentials.Expiration); perr == nil {
			expiresAt = ts.Unix()
		}
	}
	if expiresAt == 0 && cr.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(cr.ExpiresIn) * time.Second).Unix()
	}

	doc := map[string]any{
		"auth": map[string]any{
			"accessKeyId":     cr.Credentials.AccessKeyID,
			"secretAccessKey": cr.Credentials.SecretAccessKey,
			"securityToken":   cr.Credentials.SecurityToken,
			"expiresAt":       expiresAt,
			"refresh_token":   cr.RefreshToken,
			"clientId":        m.ClientID,
		},
		// account.uid 用 AK 兜底。
		//
		// 授权响应只给 credentials，**不含账号 id**，而 uid 是账号池的主键：
		// 空 uid 会让所有账号塌成同一个键，在途计数互相干扰，表现为
		// 「连续 N 次成功后 in_flight 卡住，之后恒 503 no_healthy_account」。
		// 写在这里保证落盘凭证自洽（凭证解析层另有同样的兜底）。
		"account":  map[string]any{"uid": cr.Credentials.AccessKeyID, "nickname": ""},
		"dpop":     map[string]any{"privateKeyJwk": json.RawMessage(s.dpopPriv)},
		"clientId": m.ClientID,
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	delete(m.sessions, s.State)
	m.mu.Unlock()
	return out, nil
}

// callbackURLOf 重建该会话的回调 URL（redirect_uri 必须与授权时**完全一致**，
// 否则服务端会以 redirect_uri mismatch 拒绝）。
func callbackURLOf(s *Session) string {
	if s.callbackS == nil {
		return ""
	}
	// 从 AuthURL 里取 port 参数还原
	u, err := url.Parse(s.AuthURL)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%s%s", u.Query().Get("port"), CallbackPath)
}

func (m *Manager) gcLocked() {
	for k, s := range m.sessions {
		if time.Since(s.CreatedAt) > TTL {
			if s.callbackS != nil {
				_ = s.callbackS.Close()
			}
			delete(m.sessions, k)
		}
	}
}

// 下面几个是对 internal/codearts 的薄适配。
// 为什么不让 oauth 直接用 DPoPKeyPair 的方法：
// 授权流程与「上游调用」是两个关注点，oauth 只借其密钥能力，
// 通过小函数隔离，将来 codearts 换实现不影响本包。
//
// 依赖方向：oauth -> codearts（codearts 不依赖 oauth，无环）。

func newDPoPKeyPair() (*DPoPKeyPair, error) {
	return NewDPoPKeyPair()
}

func loadDPoPKeyPair(raw json.RawMessage) (*DPoPKeyPair, error) {
	return FromPrivateJWK(raw)
}

// privateJWKJSON 把私钥 JWK 序列化成可持久化的 JSON。
func privateJWKJSON(kp *DPoPKeyPair) json.RawMessage {
	b, _ := json.Marshal(kp.PrivateJWK())
	return b
}

func randomState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// ⚠ 源包里这里有一个 `truncate`，**移植时删掉了** ——
// client.go:656 已有同实现的版本。同一个包内不能重名，
// 而两份实现将来会漂移（日志截断长度不一致会让排查时看到的
// 错误信息长度不同，是纯粹的困惑源）。统一用 client.go 那份。
