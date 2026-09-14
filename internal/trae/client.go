// client.go TRAE SOLO 上游 HTTP 客户端。
//
// # 三个 host、三类请求头（实测自 traework2api 的 SPEC）
//
//	AgentHost  https://trae-api-cn.mchost.guru   对话/模型（SOLO 专属头）
//	UgHost     https://api.trae.cn               签到/积分（ug 头）
//	OAuthHost  https://api.trae.com.cn           ExchangeToken/GetUserInfo（oauth 头）
//
// 所有端点用 POST + JSON。鉴权是 `Authorization: Cloud-IDE-JWT <accessToken>`
// （对话侧还要带一串 X-* 设备头，缺失会 4xx —— 这些是客户端指纹，如实照抄）。
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// 上游技术常量（实测，禁止改动）。
const (
	AgentHost      = "https://trae-api-cn.mchost.guru"
	UgHost         = "https://api.trae.cn"
	OAuthHost      = "https://api.trae.com.cn"
	ClientID       = "en1oxy7wnw8j9n" // SOLO stable
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.43"
	IdeVersionCode = "20260716"
	DeviceBrand    = "83DG"
	OSVersion      = "Windows 11 Pro"
	Function       = "solo_work_lite"

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)

// clientUA 客户端 User-Agent（照抄上游客户端）。
const clientUA = "Trae/" + IdeVersion

// maxRespBody 上游回执读取上限。
const maxRespBody = 1 << 20

// Client TRAE 上游 HTTP 客户端。Host 字段可覆盖（测试/换域名）。
type Client struct {
	// HTTP 用于短 JSON 请求（模型/签到/积分/ExchangeToken），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，避免长流被截断；
	// 通过 Transport.ResponseHeaderTimeout 兜底"上游一直不返回首字节"。
	StreamHTTP *http.Client

	AgentHost string
	UgHost    string
	OAuthHost string
	ClientID  string
}

// New 生产默认值。配置连接池减少 TLS 握手。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 无总超时
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

// NewWithBase 用一个基址同时覆盖三个 host（hermetic 测试用）。
//
// 假上游在测试里只实现对话/模型两个端点时，Ug/OAuth 端点仍会打到假上游，
// 由假上游决定回什么 —— 测试负责把不需要的端点也挂上。
func NewWithBase(base string) *Client {
	c := New()
	c.AgentHost = base
	c.UgHost = base
	c.OAuthHost = base
	return c
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// soloHeaders 设置对话/模型请求的 SOLO 专属头。
func soloHeaders(req *http.Request, a *Auth, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", clientUA)
	at := a.JWT() // 读锁快照
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-App-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "windows")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// ugHeaders 设置签到/积分（api.trae.cn）所需头。
func ugHeaders(req *http.Request, a *Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT())
	req.Header.Set("X-User-Region", "CN")
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// oauthHeaders 设置 ExchangeToken / GetUserInfo 所需头（无签名，仅 UA）。
func oauthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}

// doJSON 发请求并解 JSON；HTTP 非 2xx 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
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

// ChatStream 发 llm_utils_chat 请求并返回原始 SSE body 流（调用方负责 Close）。
//
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方分类）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	prepared := PrepareBody(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.agentBase()+EpChat, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	soloHeaders(req, a, true)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("trae: chat_stream uid=%s transport error: %v", shortUID(a.UID), err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
		_ = resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息。
type ModelInfo struct {
	ID      string
	Name    string
	Unavail bool // 已知不可用（生图类）—— 预留，当前不填
}

// FetchModels 拉 SOLO 模型表（get_detail_param）。
//
// 模型 ID = config_name（如 glm-5.2）。上游还会返回每个 config 的
// model_detail_list（具体模型），但反代链路只需要 config 级 ID。
func (c *Client) FetchModels(ctx context.Context, a *Auth) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	soloHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("trae: 模型回执解析失败: %w", err)
	}
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		if strings.TrimSpace(cfg.ConfigName) == "" {
			continue
		}
		out = append(out, ModelInfo{ID: cfg.ConfigName, Name: cfg.DisplayConfig.DisplayName})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("trae: 模型接口返回空列表")
	}
	return out, nil
}

// RefreshToken 通过 ExchangeToken 强制刷新 accessToken（refreshToken 轮换）。
//
// ⚠ refreshToken 是消费型：全程持 a 写锁，任何失败路径都不改写 a 字段
// （旧 refreshToken 可重试）。成功时更新 a，调用方负责 SaveAtomic。
func (c *Client) RefreshToken(a *Auth) error {
	if a == nil {
		return fmt.Errorf("trae: 空凭证")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return c.refreshLocked(a)
}

// refreshLocked 是 RefreshToken 的持锁内部实现；调用方必须已持有 a.mu 写锁。
func (c *Client) refreshLocked(a *Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("trae: 没有 refreshToken（无法自动续期）")
	}
	host := strings.TrimSpace(a.ApiHost)
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	oauthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("trae: exchange 回执解析失败: %w", err)
	}
	if strings.TrimSpace(resp.Result.Token) == "" {
		return fmt.Errorf("trae: 刷新失败 —— 回执里没有 Token，需要重新登录")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// normalizeExpiresAt 把 ExchangeToken 的 TokenExpireAt 归一化为 Unix 秒。
//
// 上游返回毫秒（~1.7e12），auth 文件用秒（~1.7e9），用 1e12 区分。
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// CheckinStatus 查询签到状态。
func (c *Client) CheckinStatus(ctx context.Context, a *Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("trae: 签到状态解析失败: %w", err)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim 执行签到。
func (c *Client) CheckinClaim(ctx context.Context, a *Auth) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	ugHeaders(req, a)
	_, err = c.doJSON(req)
	return err
}

// UserEntUsage 聚合积分（ide_user_ent_usage 的 credits_limit 求和）。
//
// TRAE 的额度是"权益包"形态：每个包带一个 credits_limit，可用额度 =
// 各包 credits_limit 之和（实测 traework2api 的 credit.sh 口径）。
func (c *Client) UserEntUsage(ctx context.Context, a *Auth) (remain int64, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}
	var resp struct {
		IsCreditsBilling        bool `json:"is_credits_billing"`
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, fmt.Errorf("trae: 额度回执解析失败: %w", err)
	}
	for _, p := range resp.UserEntitlementPackList {
		remain += p.EntitlementBaseInfo.Quota.CreditsLimit
	}
	return remain, nil
}

// GetUserInfo 查询账号信息（登录用）。
func (c *Client) GetUserInfo(ctx context.Context, a *Auth) (uid, nickname, enterpriseID string, err error) {
	host := strings.TrimSpace(a.ApiHost)
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	oauthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT())
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("trae: 用户信息解析失败: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

// truncate 截断上游错误体用于日志。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// UpstreamError 带 HTTP 状态码的上游错误（分类见 errorclassifier.go）。
type UpstreamError struct {
	Status int
	Msg    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("trae upstream http %d: %s", e.Status, e.Msg)
}
