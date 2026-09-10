// Package oauth 实现 WorkBuddy CN 的 OAuth 设备授权流程（服务端侧）。
//
// 与 cmd/login 的差异：本包把 device state 放在进程内存里（带 TTL），不落 /tmp 文件，
// 因此天然适配「一个网关进程服务多个浏览器会话」的场景；cmd/login 保持原样不动。
package oauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 上游常量（CN only）。与 cmd/login/main.go 保持一致。
const (
	defaultBaseURL = "https://copilot.tencent.com"
	clientUA       = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer  = "https://www.codebuddy.cn"
)

// ErrPending 表示用户尚未在浏览器完成授权；调用方应继续轮询。
var ErrPending = errors.New("oauth: 授权尚未完成")

// stateTTL device state 在内存中的存活时间。上游 state 本身也有有效期，
// 这里取一个保守值：超时后提示重新发起，而不是拿着废 state 一直轮询。
const stateTTL = 15 * time.Minute

// Credential 一次成功授权得到的完整凭证。
type Credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
}

// ExpiresAt 返回 access token 的绝对过期时刻（Unix 秒）。
func (c *Credential) ExpiresAt() int64 {
	if c.ExpiresIn <= 0 {
		return 0
	}
	return time.Now().Unix() + c.ExpiresIn
}

// authDoc 是落盘格式（嵌套形）：internal/auth 的 Parse 认这个形状，
// 与 cmd/login + login.sh 写出的文件逐字段一致。
type authDoc struct {
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	} `json:"auth"`
	Account struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	} `json:"account"`
}

// MarshalAuthFile 生成 auths/workbuddy-<uid>.json 的文件名与内容。
func (c *Credential) MarshalAuthFile() (name string, raw []byte, err error) {
	if strings.TrimSpace(c.AccessToken) == "" {
		return "", nil, errors.New("oauth: 凭证缺少 access_token")
	}
	if strings.TrimSpace(c.UID) == "" {
		return "", nil, errors.New("oauth: 凭证缺少 uid")
	}
	var doc authDoc
	doc.Auth.AccessToken = c.AccessToken
	doc.Auth.RefreshToken = c.RefreshToken
	doc.Auth.ExpiresAt = c.ExpiresAt()
	doc.Auth.Domain = c.Domain
	doc.Account.UID = c.UID
	doc.Account.EnterpriseID = c.EnterpriseID
	doc.Account.Nickname = c.Nickname
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return "workbuddy-" + c.UID + ".json", b, nil
}

// SaveToDir 原子写盘到 dir/workbuddy-<uid>.json（0600）。
func (c *Credential) SaveToDir(dir string) (string, error) {
	name, raw, err := c.MarshalAuthFile()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

type pending struct {
	authURL   string
	createdAt time.Time
}

// Client 设备授权客户端。
type Client struct {
	BaseURL string
	HTTP    *http.Client

	mu      sync.Mutex
	pending map[string]pending
}

// New 构建客户端；baseURL 为空时用 CN 默认站点。
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		pending: make(map[string]pending),
	}
}

func (c *Client) headers(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发一次带信封的请求；HTTP 非 2xx 或 code!=0 都算失败。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, int, error) {
	c.headers(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// Start 申请一个 device state，返回 (state, authURL)。
// 每个 state 用独立的 cookie jar —— 多账号登录互不串会话。
func (c *Client) Start() (state, authURL string, err error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	req, err := http.NewRequest(http.MethodPost,
		c.BaseURL+"/v2/plugin/auth/state?platform=CLI", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", err
	}
	c.headers(req)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", "", fmt.Errorf("auth state parse: %w", err)
	}
	if env.Code != 0 {
		return "", "", fmt.Errorf("auth state code=%d msg=%s", env.Code, env.Msg)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(env.Data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", errors.New("auth state: missing state/authUrl")
	}

	c.mu.Lock()
	c.gcLocked()
	c.pending[st.State] = pending{authURL: st.AuthURL, createdAt: time.Now()}
	c.mu.Unlock()
	return st.State, st.AuthURL, nil
}

// gcLocked 清理超期 state；调用方需持有 c.mu。
func (c *Client) gcLocked() {
	for k, v := range c.pending {
		if time.Since(v.createdAt) > stateTTL {
			delete(c.pending, k)
		}
	}
}

// StateTTL 暴露 state 存活时长供 UI 显示倒计时。
func StateTTL() time.Duration { return stateTTL }

// Poll 查询某个 state 的授权结果。
// 用户还没点完成 → ErrPending；state 未知或超期 → ErrStateUnknown。
func (c *Client) Poll(state string) (*Credential, error) {
	c.mu.Lock()
	c.gcLocked()
	_, ok := c.pending[state]
	c.mu.Unlock()
	if !ok {
		return nil, ErrStateUnknown
	}

	req, err := http.NewRequest(http.MethodGet, c.BaseURL+"/v2/plugin/auth/token?state="+state, nil)
	if err != nil {
		return nil, err
	}
	data, _, err := c.doJSON(req)
	if err != nil {
		// token 端点在 pending 时返回业务 code!=0；统一按「还没完成」处理，
		// 真正的 5xx/网络错误原样上抛，避免把上游故障伪装成等待用户。
		if isBusinessCodeErr(err) {
			return nil, ErrPending
		}
		return nil, err
	}

	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, ErrPending
	}

	cred := &Credential{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
	}

	// 账号信息拿不到不算失败：凭证已可用，UID 缺失才致命（文件名/池主键都靠它）。
	areq, err := http.NewRequest(http.MethodGet, c.BaseURL+"/v2/plugin/login/account?state="+state, nil)
	if err == nil {
		areq.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if adata, _, aerr := c.doJSON(areq); aerr == nil {
			var acct struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			}
			if json.Unmarshal(adata, &acct) == nil {
				cred.UID = acct.UID
				cred.EnterpriseID = acct.EnterpriseID
				cred.Nickname = acct.Nickname
			}
		}
	}
	if cred.UID == "" {
		return nil, errors.New("oauth: 登录成功但未取到 uid，无法落盘凭证")
	}

	c.mu.Lock()
	delete(c.pending, state)
	c.mu.Unlock()
	return cred, nil
}

// ErrStateUnknown 表示 state 不存在或已超期（需重新发起授权）。
var ErrStateUnknown = errors.New("oauth: state 不存在或已超期，请重新发起授权")

// isBusinessCodeErr 判定「上游业务码非 0」这类软失败（pending 的典型表现）。
func isBusinessCodeErr(err error) bool {
	return strings.HasPrefix(err.Error(), "code=")
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}

// SortedPending 返回当前未完成的 state 列表（调试用，按创建时间排序）。
func (c *Client) SortedPending() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.pending))
	for k := range c.pending {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
