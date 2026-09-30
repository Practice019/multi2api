// client.go LobsterAI 的 HTTP 出口层。
//
// 对应参照项目：
//
//	lobsterai-oauth.ts   → exchange / 登录 URL
//	lobsterai-auth.ts    → refresh / 版本号
//	lobsterai-credits.ts → 签到三步 + 余额
//	lobsterai-adapter.ts → 聊天转发 / 模型列表
package lobsterai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client LobsterAI 的 HTTP 客户端。三个基址都可注入（测试用 httptest）。
type Client struct {
	HTTP     *http.Client
	APIBase  string
	Portal   string
	VersionA string

	verOnce sync.Once
	verCach string
	verAt   time.Time
}

// New 生产默认值。
func New() *Client {
	return &Client{
		HTTP:     &http.Client{Timeout: 0},
		APIBase:  DefaultAPIBase,
		Portal:   DefaultPortalBase,
		VersionA: DefaultClientVersionAPI,
	}
}

// NewWithBase 测试构造（三个基址都指向同一个假上游）。
func NewWithBase(base string) *Client {
	return &Client{
		HTTP:     &http.Client{Timeout: 0},
		APIBase:  base,
		Portal:   base,
		VersionA: base + "/version",
	}
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: requestTimeoutMS * time.Millisecond}
}

func (c *Client) api() string {
	if c != nil && c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) portal() string {
	if c != nil && c.Portal != "" {
		return strings.TrimRight(c.Portal, "/")
	}
	return DefaultPortalBase
}

func (c *Client) versionAPI() string {
	if c != nil && c.VersionA != "" {
		return c.VersionA
	}
	return DefaultClientVersionAPI
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// do 发一次请求，返回 (状态码, 响应体)。
func (c *Client) do(ctx context.Context, req *http.Request, timeoutMS int) (int, []byte, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	resp, err := c.httpClient().Do(req.WithContext(cctx))
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// applyAnonymousHeaders exchange / refresh 用（**不需要 Authorization**）。
//
// 换 token 时还没有 token；续期时服务端只认请求体里的 refreshToken。
func applyAnonymousHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
}

// applyAuthHeaders 带认证的基础头。
func applyAuthHeaders(req *http.Request, a *Auth, accept string) {
	req.Header.Set("Accept", accept)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if a != nil {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	}
}

// applyCapabilityHeaders 加两个 X-LobsterAI-Client-* 头。
//
// ⚠ **在本端点是必需的，不是可有可无的元数据**（实测）：
// 服务端按 `X-LobsterAI-Client-Capabilities` 声明的能力**过滤模型集合** ——
// 不带该头时 `kimi-k3` 不会出现在返回里（25 个模型），
// 带上 `kimi-k3-agentic-v1` 才返回 26 个。
//
// 早先的实现用"只有 4 个基础头"的请求打模型端点，因此即使解析正确
// 也会**永久缺少 kimi-k3**。
func applyCapabilityHeaders(req *http.Request, clientVersion string) {
	req.Header.Set("X-LobsterAI-Client-Capabilities", clientCapabilities)
	req.Header.Set("X-LobsterAI-Client-Version", clientVersion)
}

// ── 客户端版本号 ────────────────────────────────────────────────────────

// ClientVersion 取生效的客户端版本号（带 12 小时缓存 + 兜底）。
//
// ⚠ **必须保留兜底**：版本接口在第三方域名上，实测存在网络层/证书层差异
// （直连曾失败）。拿不到就用 DefaultClientVersion。
//
// 版本号是签到接口的必填参数，所以这里的失败会让签到降级 ——
// 那比"整个上游不可用"轻得多。
func (c *Client) ClientVersion(ctx context.Context) string {
	c.verOnce.Do(func() {
		req, err := http.NewRequest(http.MethodGet, c.versionAPI(), nil)
		if err == nil {
			req.Header.Set("Accept", "application/json")
			if _, body, derr := c.do(ctx, req, requestTimeoutMS); derr == nil {
				if v := ParseClientVersionFromUpdate(body); v != "" {
					c.verCach = v
					c.verAt = time.Now()
					return
				}
			}
		}
		// 失败/格式非法 → 兜底
		c.verCach = DefaultClientVersion
		c.verAt = time.Now()
	})
	// 缓存过期后重新探测（下一次调用时）
	if time.Since(c.verAt) > time.Duration(versionCacheTTLMS)*time.Millisecond {
		c.verOnce = sync.Once{}
		c.verCach = ""
	}
	return c.verCach
}

// ── 登录（exchange）─────────────────────────────────────────────────────

// LoginURL 构造 portal 登录 URL（参照 buildLobsteraiLoginUrl）。
//
//	{portal}/portal#/login?source=electron&redirect_uri=...&state=...
//
// `source=electron` 声明登录来源是桌面客户端（portal 据此选择交互流程）。
func (c *Client) LoginURL(port int, state string) string {
	q := url.Values{}
	q.Set("source", "electron")
	q.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d%s", port, callbackPath))
	q.Set("state", state)
	return c.portal() + "/portal#/login?" + q.Encode()
}

// LoginSession 一次性登录会话（uuid + firstKeyfrom）。
//
// ⚠ uuid / firstKeyfrom 由**客户端**生成并贯穿整个账号生命周期：
// 它们不在服务端响应里，而是要在 exchange 时提交、并在之后**每次续期**时
// 原样回传。因此必须随凭据持久化。
type LoginSession struct {
	UUID          string
	FirstKeyfrom  string
	LatestKeyfrom string
}

// NewLoginSession 生成一次性登录会话。
func NewLoginSession() (LoginSession, error) {
	u, err := NewUUID()
	if err != nil {
		return LoginSession{}, err
	}
	now := fmt.Sprintf("%d", time.Now().UnixMilli())
	return LoginSession{UUID: u, FirstKeyfrom: now, LatestKeyfrom: now}, nil
}

// ExchangeAuthCode 用授权码换凭据。
//
// 请求体**必须**含 5 个字段：authCode / firstKeyfrom / latestKeyfrom / uuid / version。
// ⚠ 该端点**不需要** Authorization（换 token 时还没有 token）。
func (c *Client) ExchangeAuthCode(ctx context.Context, code string, s LoginSession) (*Auth, error) {
	ver := c.ClientVersion(ctx)
	payload, _ := json.Marshal(map[string]any{
		"authCode":      code,
		"firstKeyfrom":  s.FirstKeyfrom,
		"latestKeyfrom": s.LatestKeyfrom,
		"uuid":          s.UUID,
		"version":       ver,
	})
	req, err := http.NewRequest(http.MethodPost, c.api()+exchangePath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyAnonymousHeaders(req)

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("lobsterai: exchange HTTP %d: %s", status, truncate(string(body), 160))
	}
	env, err := parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	return c.buildAuthFromTokenData(env.Data, s)
}

// buildAuthFromTokenData 由令牌段组装凭据。
//
// expires_at 取值顺序（与 Go 一致）：
//
//  1. expiresIn（相对秒数）→ 以**当前时刻**为基准换算
//  2. 缺失时用 access token 的 JWT exp
//  3. 都拿不到则留空（ExpiresAtMS 会再试 JWT，仍失败则"不判定过期"）
func (c *Client) buildAuthFromTokenData(data []byte, s LoginSession) (*Auth, error) {
	var rec struct {
		AccessToken  string  `json:"accessToken"`
		RefreshToken string  `json:"refreshToken"`
		ExpiresIn    float64 `json:"expiresIn"`
		User         struct {
			ID       string `json:"id"`
			YID      string `json:"yid"`
			UserID   string `json:"userId"`
			Nickname string `json:"nickname"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("lobsterai: 令牌响应无法解析: %w", err)
	}
	if strings.TrimSpace(rec.AccessToken) == "" {
		return nil, fmt.Errorf("lobsterai: 令牌响应缺少 accessToken")
	}

	a := &Auth{
		AccessToken:   rec.AccessToken,
		RefreshToken:  rec.RefreshToken,
		Nickname:      strings.TrimSpace(rec.User.Nickname),
		UUID:          s.UUID,
		FirstKeyfrom:  s.FirstKeyfrom,
		LatestKeyfrom: s.LatestKeyfrom,
	}
	// uid 候选顺序：user.id > user.userId > user.yid
	a.UID = firstNonEmpty(rec.User.ID, rec.User.UserID, rec.User.YID)
	a.UserID = strings.TrimSpace(rec.User.UserID)
	// expires_at：expiresIn 优先，其次 JWT
	if rec.ExpiresIn > 0 {
		a.ExpiresAt = fmt.Sprintf("%d", time.Now().UnixMilli()+int64(rec.ExpiresIn*1000))
	} else if ms := jwtExpMS(rec.AccessToken); ms > 0 {
		a.ExpiresAt = fmt.Sprintf("%d", ms)
	}
	return a, nil
}

// ── 续期 ────────────────────────────────────────────────────────────────

// RefreshCredential 续期一份凭证。
//
// ⚠ 该端点**不带 Authorization**（服务端只认请求体里的 refreshToken）。
// ⚠ 请求体里的 keyfrom 值都用**凭据里存储的原值**，不取当前时刻。
func (c *Client) RefreshCredential(ctx context.Context, a *Auth) (*Auth, error) {
	if a == nil || !a.Renewable() {
		return nil, ErrRefreshExpired
	}
	ver := c.ClientVersion(ctx)
	payload, _ := json.Marshal(refreshBody(a, ver))
	req, err := http.NewRequest(http.MethodPost, c.api()+refreshPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyAnonymousHeaders(req)

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return nil, err // 网络失败：可重试
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, ErrRefreshExpired
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("lobsterai: 续期 HTTP %d: %s", status, truncate(string(body), 160))
	}
	env, err := parseEnvelope(body)
	if err != nil {
		// ⚠ data 非对象（信封解析失败）在这条路径上通常意味着 refreshToken 失效，
		// 参照实现把这种形态与"业务失败"同等对待。
		return nil, fmt.Errorf("%w: %v", ErrRefreshExpired, err)
	}

	var rec struct {
		AccessToken  string  `json:"accessToken"`
		RefreshToken string  `json:"refreshToken"`
		ExpiresIn    float64 `json:"expiresIn"`
	}
	if err := json.Unmarshal(env.Data, &rec); err != nil {
		return nil, fmt.Errorf("lobsterai: 续期响应无法解析: %w", err)
	}
	if strings.TrimSpace(rec.AccessToken) == "" {
		return nil, fmt.Errorf("%w：续期响应缺少 accessToken", ErrRefreshExpired)
	}

	next := *a // 保留 uuid / first_keyfrom / latest_keyfrom / uid / nickname
	next.AccessToken = rec.AccessToken
	if strings.TrimSpace(rec.RefreshToken) != "" {
		next.RefreshToken = rec.RefreshToken
	}
	if rec.ExpiresIn > 0 {
		next.ExpiresAt = fmt.Sprintf("%d", time.Now().UnixMilli()+int64(rec.ExpiresIn*1000))
	} else if ms := jwtExpMS(rec.AccessToken); ms > 0 {
		next.ExpiresAt = fmt.Sprintf("%d", ms)
	}
	return &next, nil
}

// ── 模型列表 ────────────────────────────────────────────────────────────

// Model 一个可用模型。
type Model struct {
	ID        string
	Name      string
	Provider  string
	APIFormat string
}

// FetchModels 拉模型列表。
//
// ⚠ query 用 keyfrom body 的等价物（**不含 refreshToken**），
// 且必须带两个 X-LobsterAI-Client-* 头 —— 否则 kimi-k3 不会出现。
func (c *Client) FetchModels(ctx context.Context, a *Auth) ([]Model, error) {
	ver := c.ClientVersion(ctx)
	u := c.api() + modelsPath + "?" + modelListQuery(a, ver)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	applyAuthHeaders(req, a, "application/json")
	applyCapabilityHeaders(req, ver)

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("lobsterai: 模型列表 HTTP %d", status)
	}

	// ⚠ 模型列表套信封，data 是**数组**（不是对象）——
	// 不能走 parseEnvelope（它要求 data 是对象）。
	var rec struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			ModelID   string `json:"modelId"`
			ModelName string `json:"modelName"`
			Provider  string `json:"provider"`
			APIFormat string `json:"apiFormat"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("lobsterai: 模型列表无法解析: %w", err)
	}
	if rec.Code != 0 {
		return nil, fmt.Errorf("lobsterai: 模型列表 code=%d %s", rec.Code, rec.Msg)
	}
	out := make([]Model, 0, len(rec.Data))
	for _, m := range rec.Data {
		if strings.TrimSpace(m.ModelID) == "" {
			continue
		}
		out = append(out, Model{
			ID: m.ModelID, Name: firstNonEmpty(m.ModelName, m.ModelID),
			Provider: m.Provider, APIFormat: m.APIFormat,
		})
	}
	return out, nil
}

// ── 签到（三步）─────────────────────────────────────────────────────────

// ClaimOutcome 签到结果。
type ClaimOutcome struct {
	// Kind：claimed / already-claimed / inactive / failed
	Kind    string
	Credit  float64
	Message string
}

// ActivitySlot 活动槽位。
type ActivitySlot struct {
	SlotState      string
	ActivityCode   string
	ConfigRevision string
	Raw            map[string]any
}

// fetchActivitySlot 第 1 步：查活动槽位。
func (c *Client) fetchActivitySlot(ctx context.Context, a *Auth) (*ActivitySlot, error) {
	ver := c.ClientVersion(ctx)
	q := url.Values{}
	q.Set("placement", slotPlacement)
	q.Set("clientVersion", ver)
	q.Set("containerApiVersion", slotContainerAPIVer)
	q.Set("platform", slotPlatform)

	req, err := http.NewRequest(http.MethodGet, c.api()+activitySlotPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	applyAuthHeaders(req, a, "application/json")
	applyCapabilityHeaders(req, ver)

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("lobsterai: 活动槽位 HTTP %d", status)
	}
	env, err := parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	var rec struct {
		SlotState string `json:"slotState"`
		Activity  struct {
			ActivityCode   string `json:"activityCode"`
			ConfigRevision any    `json:"configRevision"`
		} `json:"activity"`
	}
	if err := json.Unmarshal(env.Data, &rec); err != nil {
		return nil, fmt.Errorf("lobsterai: 槽位响应无法解析: %w", err)
	}
	return &ActivitySlot{
		SlotState:      rec.SlotState,
		ActivityCode:   rec.Activity.ActivityCode,
		ConfigRevision: toStr(rec.Activity.ConfigRevision),
	}, nil
}

// fetchActivityContext 第 2 步：查活动上下文（判是否已签、能否签）。
func (c *Client) fetchActivityContext(ctx context.Context, a *Auth, slot *ActivitySlot) (bool, []string, error) {
	ver := c.ClientVersion(ctx)
	q := url.Values{}
	q.Set("configRevision", slot.ConfigRevision)

	u := c.api() + activityContextPath + "/" + url.PathEscape(slot.ActivityCode) + "/context?" + q.Encode()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return false, nil, err
	}
	applyAuthHeaders(req, a, "application/json")
	applyCapabilityHeaders(req, ver)

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return false, nil, err
	}
	if status < 200 || status >= 300 {
		return false, nil, fmt.Errorf("lobsterai: 活动上下文 HTTP %d", status)
	}
	env, err := parseEnvelope(body)
	if err != nil {
		return false, nil, err
	}
	var rec struct {
		State struct {
			ClaimedToday bool `json:"claimedToday"`
		} `json:"state"`
		// ⚠ Actions 是**字符串数组**（`["check_in"]`），不是对象数组。
		//
		// # 这是个真实缺陷（用户报"签到失败"）
		//
		// 旧实现把它解析成 `[]struct{ ActionName string }`（即期望
		// `[{"actionName":"check_in"}]`）。2026-09-30 实测：上游返回的是
		// **裸字符串数组**，于是 json.Unmarshal 报
		//
		//	json: cannot unmarshal string into Go struct field .actions
		//	  of type struct { ActionName string "json:\"actionName\"" }
		//
		// 后果：**每一次**签到都在第 3 步（上下文查询）失败 ——
		// 界面上「今日签到」永远显示"失败"，而额度、Token 都正常，
		// 看起来像"上游不给签"，实际是我们解析错了。
		//
		// 参照实现（dsh-codearts-auth 的 lobsterai-credits.ts）里
		// `actions: string[]`，`readActions` 就是 `filter(typeof === 'string')`
		// —— 与这里一致。
		//
		// ⚠ 用 []string 直接收：形状不对时 Unmarshal 会**响亮报错**，
		// 而不是像 []any 那样把类型错悄悄吞掉、留下一串空动作名
		// （那会让判据 `actions 含 check_in` 恒为假 → 静默判成 inactive）。
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(env.Data, &rec); err != nil {
		return false, nil, fmt.Errorf("lobsterai: 上下文响应无法解析: %w", err)
	}
	return rec.State.ClaimedToday, rec.Actions, nil
}

// CheckinStatus 只查签到状态，**不领取**。
//
// # 为什么必须把它从 ClaimDailyCheckin 里拆出来
//
// ClaimDailyCheckin 的判据链是（见其注释的 7 步）：
//
//  1. 槽位查询失败          → failed
//  2. slotState 不可用      → inactive
//  3. 上下文查询失败        → failed
//  4. claimedToday          → already-claimed
//  5. actions 不含 check_in → inactive
//  6. 领取失败              → failed
//  7. 成功                  → claimed
//
// 前 5 步**全是只读的**，只有第 6 步真的提交。而"今天签过没有"这个问题
// 只需要前 5 步 —— 共享驱动（internal/dailycheckin）在决定"要不要领"
// 之前必须先问它，否则每天都会重复提交一次领取请求（靠上游幂等兜住，
// 但那既浪费一次往返，也可能被风控当异常）。
func (c *Client) CheckinStatus(ctx context.Context, a *Auth) ClaimOutcome {
	slot, err := c.fetchActivitySlot(ctx, a)
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: "活动槽位查询失败: " + err.Error()}
	}
	if slot.SlotState != "available" || slot.ActivityCode == "" {
		return ClaimOutcome{Kind: "inactive",
			Message: fmt.Sprintf("无可用活动（slotState=%s）", slot.SlotState)}
	}

	claimed, actions, err := c.fetchActivityContext(ctx, a, slot)
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: "活动上下文查询失败: " + err.Error()}
	}
	if claimed {
		return ClaimOutcome{Kind: "already-claimed", Message: "今天已签到"}
	}
	if !containsStr(actions, checkInActionName) {
		return ClaimOutcome{Kind: "inactive", Message: "当前不可签到"}
	}
	// ⚠ 与 ClaimDailyCheckin 的**唯一**区别：到这里返回 "claimable"
	//（"还没签、可以签"），不提交领取。
	//
	// 用新 Kind 而不是复用 "claimed"：复用会让调用方误以为刚领过了，
	// 于是永远不领 —— 那是与"重复提交"相反方向、同样错的一种。
	return ClaimOutcome{Kind: "claimable", Message: "可签到"}
}

// ClaimDailyCheckin 执行每日签到（三步）。
//
// 步骤与判据（参照 claimLobsteraiDailyCheckin）：
//
//  1. 槽位查询失败          → failed
//  2. slotState != available 或无 activityCode → inactive
//  3. 上下文查询失败        → failed
//  4. claimedToday          → already-claimed
//  5. actions 不含 check_in → inactive
//  6. 领取失败              → failed
//  7. 成功                  → claimed（积分三级回退）
//
// ⚠ 第 4 步与第 5 步刻意分开："今天已领"用户无需动作；
// "当前不可签到"（未开始/已结束/无资格）是另一回事。
func (c *Client) ClaimDailyCheckin(ctx context.Context, a *Auth) ClaimOutcome {
	slot, err := c.fetchActivitySlot(ctx, a)
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: "活动槽位查询失败: " + err.Error()}
	}
	if slot.SlotState != "available" || slot.ActivityCode == "" {
		return ClaimOutcome{Kind: "inactive",
			Message: fmt.Sprintf("无可用活动（slotState=%s）", slot.SlotState)}
	}

	claimed, actions, err := c.fetchActivityContext(ctx, a, slot)
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: "活动上下文查询失败: " + err.Error()}
	}
	if claimed {
		return ClaimOutcome{Kind: "already-claimed", Message: "今天已签到"}
	}
	if !containsStr(actions, checkInActionName) {
		return ClaimOutcome{Kind: "inactive", Message: "当前不可签到"}
	}

	// 第 3 步：领取
	idem, err := NewUUID()
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: "生成幂等键失败: " + err.Error()}
	}
	ver := c.ClientVersion(ctx)
	payload, _ := json.Marshal(map[string]any{
		"configRevision": slot.ConfigRevision,
		// 客户端幂等键：服务端据此去重。
		"idempotencyKey": idem,
		"payload":        map[string]any{},
	})
	u := c.api() + activityContextPath + "/" + url.PathEscape(slot.ActivityCode) + "/actions/" + checkInActionName
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: err.Error()}
	}
	applyAuthHeaders(req, a, "application/json")
	applyCapabilityHeaders(req, ver)

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: "领取请求失败: " + err.Error()}
	}
	if status < 200 || status >= 300 {
		return ClaimOutcome{Kind: "failed",
			Message: fmt.Sprintf("领取 HTTP %d: %s", status, truncate(string(body), 160))}
	}
	env, err := parseEnvelope(body)
	if err != nil {
		return ClaimOutcome{Kind: "failed", Message: err.Error()}
	}
	// 积分的**三级回退链**：creditsGranted → rewardCredits → credits
	//（不同活动/版本用不同字段名）。
	var rec struct {
		Result struct {
			CreditsGranted float64 `json:"creditsGranted"`
			RewardCredits  float64 `json:"rewardCredits"`
			Credits        float64 `json:"credits"`
			Message        string  `json:"message"`
		} `json:"result"`
	}
	_ = json.Unmarshal(env.Data, &rec)
	credit := rec.Result.CreditsGranted
	if credit == 0 {
		credit = rec.Result.RewardCredits
	}
	if credit == 0 {
		credit = rec.Result.Credits
	}
	return ClaimOutcome{Kind: "claimed", Credit: credit, Message: rec.Result.Message}
}

// ── 余额 ────────────────────────────────────────────────────────────────

// Balance 积分余额。
type Balance struct {
	Total float64
	Items []BalanceItem
}

// BalanceItem 一个积分项。
type BalanceItem struct {
	Type      string
	Remaining float64
	ExpiresAt string
}

// FetchBalance 查积分余额。
//
// ⚠ 端点用 `profile-summary` 而**不是** `/api/user/quota`：
// 后者只显示 freeCreditsTotal=300，**不含活动积分**
// （实测某账号 profile-summary 有 5297.72，quota 只有 300）。
func (c *Client) FetchBalance(ctx context.Context, a *Auth) (*Balance, error) {
	ver := c.ClientVersion(ctx)
	req, err := http.NewRequest(http.MethodGet, c.api()+profileSummaryPath, nil)
	if err != nil {
		return nil, err
	}
	applyAuthHeaders(req, a, "application/json")
	applyCapabilityHeaders(req, ver)

	status, body, err := c.do(ctx, req, requestTimeoutMS)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("lobsterai: 余额 HTTP %d", status)
	}
	env, err := parseEnvelope(body)
	if err != nil {
		return nil, err
	}
	var rec struct {
		TotalCreditsRemaining float64 `json:"totalCreditsRemaining"`
		CreditItems           []struct {
			Type             string  `json:"type"`
			CreditsRemaining float64 `json:"creditsRemaining"`
			ExpiresAt        string  `json:"expiresAt"`
		} `json:"creditItems"`
	}
	if err := json.Unmarshal(env.Data, &rec); err != nil {
		return nil, fmt.Errorf("lobsterai: 余额响应无法解析: %w", err)
	}
	out := &Balance{Total: rec.TotalCreditsRemaining}
	for _, it := range rec.CreditItems {
		out.Items = append(out.Items, BalanceItem{
			Type: it.Type, Remaining: it.CreditsRemaining, ExpiresAt: it.ExpiresAt,
		})
	}
	return out, nil
}

// ── 聊天 ────────────────────────────────────────────────────────────────

// ChatStream 发一次流式对话。
//
// ⚠ **仅 SSE**：`stream:false` 会让上游回 500。
// ⚠ 返回**裸 SSE，不套信封**（模型列表套信封、聊天不套 —— 最容易搞错的一点）。
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	ver := c.ClientVersion(ctx)
	req, err := http.NewRequest(http.MethodPost, c.api()+chatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	applyAuthHeaders(req, a, "text/event-stream, application/json")
	applyCapabilityHeaders(req, ver)

	resp, err := c.httpClient().Do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// toStr 把 JSON 值转成字符串（数字/字符串都接受）。
func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case json.Number:
		return t.String()
	}
	return ""
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
