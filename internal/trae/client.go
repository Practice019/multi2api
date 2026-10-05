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
	"crypto/rand"
	"encoding/hex"
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
	DeviceCPU      = "Intel(R) Core(TM) Ultra 9 285K"
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
	// 与 trae-api-proxy / trae-local-api 对齐的指纹头：设备 CPU 与链路追踪 ID。
	// 上游风控以"请求头指纹是否完整一致"为常用判据，缺头是异常特征。
	req.Header.Set("X-Device-Cpu", DeviceCPU)
	rid := newTraceID()
	req.Header.Set("X-Request-ID", rid)
	req.Header.Set("X-Trae-Request-ID", rid)
	req.Header.Set("X-Custom-Trace-Id", newTraceID())
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// ugHeaders 设置签到/积分（api.trae.cn）所需头。
//
// # ⚠ `X-Device-Id` 必须用**账号 uid**，不是凭证里的 32hex deviceId
//
// 这是用户报障「签到不了」的**主因**。实测（真实上游，同一份凭证）：
//
//	体检状态            claim 结果
//	X-Device-Id=uid     {"code":0}           → 到手（credits=100+extra=100）
//	X-Device-Id=32hex   {"code":9074}        → 拒绝
//	不带该头            {"code":9004}        → 拒绝
//
// 三者的**设备维度**回执也印证了同一点（status 的 did_checked_in 字段
// 是"这台设备今天的名额用掉了没"，与账号维度的 checked_in 是两回事）：
//
//	uid       → did_checked_in=true     ← 上游承认这台"设备"
//	32hex     → did_checked_in=false    ← 不承认
//	随机 16 位 → did_checked_in=false    ← 不承认
//
// # 为什么会这样（两种设备标识是不同的东西）
//
//	credential.DeviceID  登录/对话用的设备指纹（login.go 里 randomHex32 生成，
//	                     参与登录 URL 的 device_id / x_device_id 参数）
//	签到接口要的            账号在 CN 签到体系里的**设备标识 = uid**
//
// 本包最初把 `X-Device-Id` 直接填成登录用的 32hex —— 端点、头名、body 都对，
// 唯独这个**值**错了，于是每次都拿 9074，而旧代码又只看 HTTP 状态
// （200），把拒绝记成"签到成功"。两个缺陷叠加就成了「签到不了」。
//
// # 与社区实现的关系
//
// connectedGraph/trae2api-web 等实现同样填 `a.DeviceID`，所以它们对
// "桌面客户端导入"的凭证会撞上同一个坑 —— 本仓的取值是**实测结论**，
// 与那份参照不同，且优先于它（实例事实 > 参照实现）。
func ugHeaders(req *http.Request, a *Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT())
	req.Header.Set("X-User-Region", "CN")
	if dev := checkinDeviceID(a); dev != "" {
		req.Header.Set("X-Device-Id", dev)
	}
}

// checkinDeviceID 返回签到/积分接口该用的 `X-Device-Id` 值。
//
// 优先 uid（实测唯一被接受的形态）；uid 缺失时才退回登录用的 DeviceID ——
// 那种凭证本来也签不了（没有账号主键），但发一个值比发空串更接近既有行为
// （空串会让上游回 9004，看起来像"缺头"而不是"凭证不全"）。
//
// ⚠ 不加锁：UID 与 DeviceID 都是**装载期之后不再改写**的字段
// （`mu` 只保护 AccessToken / RefreshToken / ExpiresAt —— 见 credential.go
// 的并发模型注释），所以读它们不需要 RLock。
func checkinDeviceID(a *Auth) string {
	if a == nil {
		return ""
	}
	if a.UID != "" {
		return a.UID
	}
	return a.DeviceID
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
	// ClientID 必须与**签发** refreshToken 的那个一致（桌面端导入的凭证
	// 用的 client id 可能与默认值不同，见 credential.go 的 ClientID 注释）。
	clientID := strings.TrimSpace(a.ClientID)
	if clientID == "" {
		clientID = c.ClientID
	}
	body := map[string]any{
		"ClientID":     clientID,
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
	// 与 trae-api-proxy 对齐：ExchangeToken 带当前 accessToken 作为
	// X-Cloudide-Token（会话绑定/风控识别，少带是缺头特征）。
	// ⚠ 这里**不能**用 a.JWT()：refreshLocked 全程持 a.mu 写锁，
	// JWT() 会再取读锁 —— Go 的 RWMutex 不可重入，直接死锁。
	if at := a.AccessToken; at != "" {
		req.Header.Set("X-Cloudide-Token", at)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	// 兼容两种回执形态（实测）：`{"Result":{...}}`（PascalCase）与
	// 顶层 camelCase（trae-local-api 读的就是顶层 token/expiredAt/refreshExpiredAt）。
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
		Token               string `json:"token"`
		TokenExpireAt       int64  `json:"expiredAt"`
		TokenExpireDuration int64  `json:"tokenExpireDuration"`
		RefreshToken        string `json:"refreshToken"`
		RefreshExpireAt     int64  `json:"refreshExpiredAt"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("trae: exchange 回执解析失败: %w", err)
	}
	newToken := resp.Result.Token
	if newToken == "" {
		newToken = resp.Token
	}
	if strings.TrimSpace(newToken) == "" {
		return fmt.Errorf("trae: 刷新失败 —— 回执里没有 Token，需要重新登录")
	}
	a.AccessToken = newToken
	newRefresh := resp.Result.RefreshToken
	if newRefresh == "" {
		newRefresh = resp.RefreshToken
	}
	if newRefresh != "" {
		a.RefreshToken = newRefresh
	}
	expAt := resp.Result.TokenExpireAt
	if expAt <= 0 {
		expAt = resp.TokenExpireAt
	}
	if expAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(expAt)
	} else if d := resp.Result.TokenExpireDuration; d > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(d) * time.Second).Unix()
	} else if d := resp.TokenExpireDuration; d > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(d) * time.Second).Unix()
	}
	rExp := resp.Result.RefreshExpireAt
	if rExp <= 0 {
		rExp = resp.RefreshExpireAt
	}
	if rExp > 0 {
		a.RefreshExpiresAt = normalizeExpiresAt(rExp)
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

// checkinEnvelope 签到接口的**业务信封**字段（status / claim 共用）。
//
// # ⚠ 业务失败是 HTTP 200 + `code != 0`，不是 HTTP 4xx/5xx
//
// 这是本包最初漏掉的一层：`doJSON` 只在 `StatusCode >= 400` 时报错，
// 于是 `{"code":9074,"message":"当前参与用户太多"}` 这类**业务拒绝**
// 被当成成功 —— `CheckinClaim` 返回 nil，上层记 `StatusOK`，
// 而**一分积分都没到账**。
//
// 实测（真实上游，同一份凭证连打 8 次，间隔 30 秒）：
//
//	{"code":9074,"message":"当前参与用户太多，请稍后再试"}   HTTP 200 × 8/8
//
// 即 9074 会**持续数分钟**，不是瞬时抖动。界面显示"签到成功"、
// 历史表记 `ok`，而实际没有奖励 —— 正是用户报的「签到不了」。
//
// 参照实现（connectedGraph/trae2api-web、caigee-cmd/cli2api、
// yetone/magpie）都显式解析 code，并把 9074/9095 区分对待。
type checkinEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// status 专有
	CheckedIn    bool  `json:"checked_in"`
	DidCheckedIn bool  `json:"did_checked_in"`
	Credits      int64 `json:"credits"`
	ExtraCredits int64 `json:"extra_credits"`
	Enable       bool  `json:"enable"`
}

// 签到的业务错误码（多源实证，见 checkinEnvelope 注释）。
const (
	checkinCodeOK       = 0
	checkinCodeNoDevice = 9004 // 缺/无效 x-device-id → "order parameters are incorrect"
	checkinCodeBusy     = 9074 // 当前参与用户太多（设备级限流，可持续数分钟）
	checkinCodeDevDone  = 9095 // 当前设备今日已签
)

// CheckinStatus 查询签到状态。
//
// 返回 (账号今日已签, 基础奖励, 额外奖励, 活动是否开启, 错误)。
//
// ⚠ 与旧版的差异：现在**校验业务 code**。旧版不读 code，于是上游返回
// 错误信封（如鉴权失败）时全部字段为零值 → `checked_in=false, enable=false`
// → 被当成"活动未开启"静默跳过，或继续去 claim。
func (c *Client) CheckinStatus(ctx context.Context, a *Auth) (checkedIn bool, credits, extraCredits int64, enable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, 0, false, err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, 0, false, err
	}
	var resp checkinEnvelope
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, 0, false, fmt.Errorf("trae: 签到状态解析失败: %w", err)
	}
	if resp.Code != checkinCodeOK {
		return false, 0, 0, false, checkinError("查询签到状态", resp)
	}
	return resp.CheckedIn, resp.Credits, resp.ExtraCredits, resp.Enable, nil
}

// CheckinClaim 执行签到。
//
// # ⚠ 只能靠「复查 status」判定成败（实测）
//
// claim 的业务失败是 HTTP 200 + code != 0，所以**必须解析 code**。
// 但解析 code 仍不够 —— 参照实现 caigee-cmd/cli2api 的结论是：
//
//	"The claim endpoint answers code 0 even when the device identity is
//	 refused (9074) or the daily grant was already taken, so success is
//	 decided by re-probing: a real claim flips checked_in to true."
//
// 即存在「code=0 但没真到账」的情形。所以判据是**复查一次 status**，
// 只有 `checked_in` 真的翻成 true 才算成功。
//
// 返回的 checkinClaimResult 带上复查到的信息，供上层写历史与展示。
func (c *Client) CheckinClaim(ctx context.Context, a *Auth) (checkinClaimResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte("{}")))
	if err != nil {
		return checkinClaimResult{}, err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return checkinClaimResult{}, err
	}
	var claim checkinEnvelope
	if err := json.Unmarshal(data, &claim); err != nil {
		return checkinClaimResult{}, fmt.Errorf("trae: 签到响应解析失败: %w", err)
	}
	// 业务拒绝：如实报错（含 code 与上游原文），**不**记成功。
	if claim.Code != checkinCodeOK {
		return checkinClaimResult{}, checkinError("签到", claim)
	}

	// 复查：code=0 也可能是"没真到账"。
	//
	// ⚠ 这里刻意**不**把复查失败当签到失败 —— 复查是额外的网络往返，
	// 它自己失败（网络抖动）不代表签到没成功。所以复查出错时返回
	// "不确定"，由上层决定措辞，而不是谎报成功或谎报失败。
	afterIn, afterCredits, afterExtra, _, err := c.CheckinStatus(ctx, a)
	if err != nil {
		return checkinClaimResult{Credits: claim.Credits, Confirmed: false}, nil
	}
	// 额外奖励（实测 status 的 extra_credits 与 credits 是两个口径）。
	total := claim.Credits
	if total <= 0 {
		total = afterCredits + afterExtra
	}
	return checkinClaimResult{Credits: total, Confirmed: afterIn}, nil
}

// checkinClaimResult claim 之后的**已核实**结果。
type checkinClaimResult struct {
	// Credits 本次实得（claim 回执优先，缺失时取复查的 status）。
	Credits int64
	// Confirmed 复查确认 `checked_in` 已为 true（真的到账了）。
	Confirmed bool
}

// checkinError 把业务信封转成可读错误（带 code，便于排障与分类）。
//
// ⚠ 带上 code 而不是只带 message：message 可能是中文提示语
// （"当前参与用户太多，请稍后再试"），而 code 是**稳定的判据**——
// 上层要按它区分"限流/设备已签/缺设备"三种完全不同的处置。
func checkinError(what string, env checkinEnvelope) error {
	msg := strings.TrimSpace(env.Message)
	if msg == "" {
		msg = "(上游未给 message)"
	}
	return fmt.Errorf("trae: %s被上游拒绝（code %d）：%s", what, env.Code, msg)
}

// IsCheckinBusy 报告错误是否是"当前参与用户太多"（9074，设备级限流）。
//
// 与"今天已签到"（9095）区分：前者**过一会儿可能成功**，后者今天不必再试。
// 混为一谈会让界面把限流说成"已签到"（用户以为签过了，实际没有）。
func IsCheckinBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), fmt.Sprintf("code %d", checkinCodeBusy))
}

// IsCheckinDeviceDone 报告错误是否是"当前设备今日已签"（9095）。
func IsCheckinDeviceDone(err error) bool {
	return err != nil && strings.Contains(err.Error(), fmt.Sprintf("code %d", checkinCodeDevDone))
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

// newTraceID 生成一个 32 位 hex 的链路追踪 ID（X-Request-ID 系请求头用，
// 与上游客户端同形状）。
func newTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// UpstreamError 带 HTTP 状态码的上游错误（分类见 errorclassifier.go）。
type UpstreamError struct {
	Status int
	Msg    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("trae upstream http %d: %s", e.Status, e.Msg)
}
