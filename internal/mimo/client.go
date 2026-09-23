// client.go MiMo 上游 HTTP 客户端。
//
// # 三个面（评审报告 §2）
//
//	paid   {base}/chat/completions、{base}/models       —— 主目标，OpenAI 兼容
//	free   {freeBase}/api/free-ai/{bootstrap,openai/chat} —— 已死通道，留位
//	oauth  account.xiaomi.com/oauth2/token              —— 孤证链路，只做导入兼容
//
// # SSE 直通纪律（MiMo2API 的反面教材）
//
// 响应流**逐字节原样透传**，聚合只旁路读（tee）不重建 chunk ——
// MiMo2API 重建流丢了 tool_calls delta 和 choices:[] 的 usage 尾帧，
// 对 coding-agent 客户端是致命的。tool_calls/reasoning/usage 都靠
// streamAggregator 从原始帧里收集进缓存。
package mimo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 技术常量（实测/官方源码定稿，见报告 §2.1）。
const (
	DefaultPaidBase  = "https://api.xiaomimimo.com/v1"
	DefaultFreeBase  = "https://api.xiaomimimo.com"
	DefaultOAuthHost = "https://account.xiaomi.com"
	DefaultClientID  = "mimocode-desktop" // oauth refresh 的 client_id（MiMo2API 孤证；随凭证可覆盖）
	DefaultVersion   = "0.1.3"

	EpChat     = "/chat/completions" // paid（OpenAI 兼容）
	EpModels   = "/models"           // paid（探活 validator 同款）
	EpFreeBoot = "/api/free-ai/bootstrap"
	EpFreeChat = "/api/free-ai/openai/chat"
	EpToken    = "/oauth2/token"

	// bootstrap JWT 提前量：距过期 <300s 即重取（mimo-code-proxy 的 300s 双检锁同款）。
	jwtSkew = 300 * time.Second
)

// maxRespBody 非流回执读取上限。
const maxRespBody = 1 << 20

// Client MiMo HTTP 客户端。
type Client struct {
	HTTP       *http.Client
	StreamHTTP *http.Client // 无总超时（长流）

	PaidBase    string
	FreeBase    string
	OAuthHost   string
	AuthHeader  string // ""|"bearer" → Authorization: Bearer；"api-key" → api-key 头
	ClientVer   string // UA: mimocode/<ClientVer>（官方两段式，报告裁决表）
	FreeEnabled bool

	// bootstrap 缓存：fingerprint → (jwt, exp)。单飞防同指纹并发重 bootstrap。
	bootMu    sync.Mutex
	bootCache map[string]*bootEntry
	bootFlt   map[string]*sync.WaitGroup
}

// ModelInfo 本包模型投影。
type ModelInfo struct {
	ID   string
	Name string
}

type bootEntry struct {
	jwt     string
	expires int64 // Unix 秒
}

// New 生产默认。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		PaidBase:   DefaultPaidBase,
		FreeBase:   DefaultFreeBase,
		OAuthHost:  DefaultOAuthHost,
		AuthHeader: "bearer",
		ClientVer:  DefaultVersion,
		bootCache:  map[string]*bootEntry{},
		bootFlt:    map[string]*sync.WaitGroup{},
	}
}

// NewWithBase hermetic 测试：三 host 全指向假上游。
func NewWithBase(base string) *Client {
	c := New()
	c.PaidBase = base + "/v1"
	c.FreeBase = base
	c.OAuthHost = base
	return c
}

func (c *Client) ua() string { return "mimocode/" + nonEmpty(c.ClientVer, DefaultVersion) }

func nonEmpty(vals ...string) string { return firstNonEmpty(vals...) }

// effectiveBase 凭证自带的账号专属网关优先（OAuth url 字段/区域网关），
// 空才回落配置默认 —— baseUrl 跟凭证走（报告 §4.1-3）。
func (c *Client) effectiveBase(a *Auth) string {
	if b := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/"); b != "" {
		return b
	}
	return strings.TrimRight(c.PaidBase, "/")
}

// applyAuth 按配置写鉴权头。api-key 形态**先删 Authorization**
// （OmniProxy auth.go:20-24：两把头都给上游会触发歧义，删比"优先级靠运"对）。
func (c *Client) applyAuth(req *http.Request, token string) {
	if strings.TrimSpace(strings.ToLower(c.AuthHeader)) == "api-key" {
		req.Header.Set("api-key", token)
		req.Header.Del("Authorization")
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}

// paidHeaders 对话/模型请求头（官方 CLI 实发头集，报告 §2.1 表格）。
func (c *Client) paidHeaders(req *http.Request, a *Auth, stream bool, affinity string) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", c.ua())
	c.applyAuth(req, a.BearerToken())
	// X-Mimo-Source 恒发（官方仅 providerID=="xiaomi" 发；多头发头的风险
	// < 缺头被判异类 —— 报告裁决，M2 观察项）。
	req.Header.Set("X-Mimo-Source", "mimocode-cli")
	if affinity != "" {
		req.Header.Set("x-session-affinity", affinity)
	}
}

// freeHeaders 免费通道头（留位；X-Mimo-Source 带 -free 后缀）。
func (c *Client) freeHeaders(req *http.Request, a *Auth, jwt, affinity string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", c.ua())
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-Mimo-Source", "mimocode-cli-free")
	if affinity != "" {
		req.Header.Set("x-session-affinity", affinity)
	}
}

// Bootstrap free 轨取 JWT：**per-fingerprint 缓存 + 300s 提前量 + 并发单飞**。
//
// 单飞不是优化是正确性：同一指纹并发 bootstrap 会白打上游，且 mimo-code-proxy
// 已证明"旧票立刻被顶掉"（401 死循环的成因之一）。
func (c *Client) Bootstrap(ctx context.Context, fp string) (string, error) {
	if strings.TrimSpace(c.FreeBase) == "" {
		return "", fmt.Errorf("mimo: 免费通道 base 未配置")
	}
	c.bootMu.Lock()
	if e, ok := c.bootCache[fp]; ok && time.Now().Add(jwtSkew).Unix() < e.expires {
		tok := e.jwt
		c.bootMu.Unlock()
		return tok, nil
	}
	if wg, inflight := c.bootFlt[fp]; inflight {
		c.bootMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-func() chan struct{} {
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			return done
		}():
			c.bootMu.Lock()
			e := c.bootCache[fp]
			c.bootMu.Unlock()
			if e == nil {
				return "", fmt.Errorf("mimo: 并发 bootstrap 无结果")
			}
			return e.jwt, nil
		}
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	c.bootFlt[fp] = wg
	c.bootMu.Unlock()

	tok, exp, err := c.bootstrapFetch(ctx, fp)

	c.bootMu.Lock()
	if err == nil {
		c.bootCache[fp] = &bootEntry{jwt: tok, expires: exp}
	}
	delete(c.bootFlt, fp)
	c.bootMu.Unlock()
	wg.Done()
	if err != nil {
		return "", err
	}
	return tok, nil
}

func (c *Client) bootstrapFetch(ctx context.Context, fp string) (string, int64, error) {
	payload, _ := json.Marshal(map[string]any{"client": fp})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.FreeBase, "/")+EpFreeBoot, bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.ua())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if resp.StatusCode >= 400 {
		return "", 0, &UpstreamError{Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	// 兼容 {jwt}|{token}|{data:{jwt|token}} 几种包装（通道已死，形态以存档为准）。
	var out struct {
		JWT   string `json:"jwt"`
		Token string `json:"token"`
		Data  struct {
			JWT   string `json:"jwt"`
			Token string `json:"token"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", 0, fmt.Errorf("mimo: bootstrap 回执解析失败: %s", truncate(string(raw), 120))
	}
	tok := firstNonEmpty(out.JWT, out.Token, out.Data.JWT, out.Data.Token)
	if tok == "" {
		return "", 0, fmt.Errorf("mimo: bootstrap 回执没有 token")
	}
	exp := jwtExpiry(tok)
	if exp == 0 {
		exp = time.Now().Add(time.Hour).Unix() // 实测 TTL=3600s，解不了 JWT 时按官方 TTL 记
	}
	return tok, exp, nil
}

// jwtExpiry 解 JWT payload 的 exp（三段 base64url；解不动返回 0）。
func jwtExpiry(tok string) int64 {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0
	}
	raw, err := base64URLDecode(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	n, _ := claims.Exp.Int64()
	return n
}

// ChatStream 发一次对话。返回语义与 trae 对齐：
// 非 2xx → (nil, status, respBody, nil)；传输层失败 → err。
// 2xx → Body 为**原样透传**的 OpenAI SSE；聚合在调用方的旁路 reader 里做。
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte, affinity string) (io.ReadCloser, int, []byte, error) {
	if a.Channel == ChannelFree {
		return c.freeChatStream(ctx, a, body, affinity)
	}
	prepared := body
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.effectiveBase(a)+EpChat, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	c.paidHeaders(req, a, true, affinity)
	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		log.Printf("mimo: chat uid=%s transport error: %v", shortUID(a.UID), err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
		_ = resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

func (c *Client) freeChatStream(ctx context.Context, a *Auth, body []byte, affinity string) (io.ReadCloser, int, []byte, error) {
	jwt, err := c.Bootstrap(ctx, a.Fingerprint)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.FreeBase, "/")+EpFreeChat, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	c.freeHeaders(req, a, jwt, affinity)
	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
		_ = resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// DropCachedJWT free 轨 401 自愈前作废缓存票（下一次 Bootstrap 必然重取）。
func (c *Client) DropCachedJWT(fp string) {
	c.bootMu.Lock()
	delete(c.bootCache, fp)
	c.bootMu.Unlock()
}

// FetchModels 拉模型目录（GET {base}/models，官方 OpenAI 形态）。
func (c *Client) FetchModels(ctx context.Context, a *Auth) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.effectiveBase(a)+EpModels, nil)
	if err != nil {
		return nil, err
	}
	c.paidHeaders(req, a, false, "")
	data, err := c.doJSON(ctx, req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("mimo: 模型回执解析失败: %w", err)
	}
	out := make([]ModelInfo, 0, len(resp.Data))
	for _, m := range resp.Data {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		out = append(out, ModelInfo{ID: m.ID})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mimo: 模型接口返回空列表")
	}
	return out, nil
}

// RefreshOAuth oauth 轨刷新：grant_type=refresh_token 整段持凭证写锁。
//
// ⚠ 链路是孤证（MiMo2API 一家，官方零引用，报告 R4）—— 只做导入兼容与
// 尽力刷新；失败语义走 onRefreshFailure 计数，不做更复杂的轮换假设。
// refreshToken 是否一次性未证 → 成功后**只有回执带回新 refresh 才替换**
// （带回才写，杜绝"把链刷断"）。
func (c *Client) RefreshOAuth(a *Auth) error {
	if a == nil {
		return fmt.Errorf("mimo: 空凭证")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Type != TypeOAuth || a.RefreshToken == "" {
		return fmt.Errorf("mimo: 非 oauth 凭证或无 refresh_token")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {firstNonEmpty(a.ClientID, DefaultClientID)},
		"refresh_token": {a.RefreshToken},
	}
	// token 端点恒在账号域（baseUrl 是 API 网关，不是授权域）。
	endpoint := c.OAuthHost
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		strings.TrimRight(endpoint, "/")+EpToken, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.ua())
	raw, err := c.doJSON(context.Background(), req)
	if err != nil {
		return err
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.Unmarshal(raw, &out) != nil || out.AccessToken == "" {
		return fmt.Errorf("mimo: oauth 刷新回执无 access_token")
	}
	a.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		a.RefreshToken = out.RefreshToken
	}
	if out.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).Unix()
	} else {
		a.ExpiresAt = time.Now().Add(2 * time.Hour).Unix() // 缺省 7200s（oauth-client.ts:16-23）
	}
	return nil
}

// doJSON 短 JSON 请求（带 ctx）。
func (c *Client) doJSON(ctx context.Context, req *http.Request) (json.RawMessage, error) {
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if resp.StatusCode >= 400 {
		return nil, &UpstreamError{Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// base64URLDecode JWT 段解码（无填充与有填充两种都试）。
func base64URLDecode(seg string) ([]byte, error) {
	if raw, err := base64.RawURLEncoding.DecodeString(seg); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(seg)
}

// fetchModelsErr 只关心"通不通"（验活/健康检查用，目录本体丢弃）。
func (c *Client) fetchModelsErr(ctx context.Context, a *Auth) error {
	_, err := c.FetchModels(ctx, a)
	return err
}

// UpstreamError 带 HTTP 状态码的上游错误。
type UpstreamError struct {
	Status int
	Msg    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("mimo upstream http %d: %s", e.Status, e.Msg)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// passThroughAggReader 旁路聚合 reader：字节**原样**流向调用方，
// 同时把行喂给 streamAggregator（SSE 直通不重建 —— 见文件头纪律）。
type passThroughAggReader struct {
	src io.Reader
	agg *streamAggregator
	buf []byte // 未完成行
}

func (r *passThroughAggReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.buf = append(r.buf, p[:n]...)
		for {
			i := bytes.IndexByte(r.buf, '\n')
			if i < 0 {
				break
			}
			r.agg.feedLine(string(r.buf[:i]))
			r.buf = r.buf[i+1:]
		}
		// 防无界增长：若上游不按行发（非 SSE），封顶丢一半继续。
		if len(r.buf) > 64<<10 {
			r.buf = r.buf[len(r.buf)/2:]
		}
	}
	return n, err
}

func shortUID(uid string) string {
	rs := []rune(uid)
	if len(rs) <= 8 {
		return uid
	}
	return string(rs[:8])
}
