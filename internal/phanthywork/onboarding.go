// onboarding.go 活动面里原先缺的四个端点 + 邀请关系。
//
// # 为什么这些端点此前是空的
//
// 2026-11-14 之前，本包只实现了 summary / claim / register / referrals(me,code) / models。
// 缺的四个（rewards、onboarding reconcile、onboarding tasks start/complete）**无法从
// sidecar 二进制里挖出 body** —— 因为 sidecar 对它们是纯透传（`X59` 里 body = `q.text()`
// 原样转发），真正的 body 由客户端定义，而客户端是另一个进程里的前端。
//
// # 真源：官方桌面端前端 bundle（**这是唯一的真源，不是推测**）
//
//	路径：D:\software\PhanthyWork-windows-x64\_up_\dist\assets\App-dSHMvpu3.js
//	      （1.9 MB，Vite 产物，未混淆到不可读；`_up_\dist` 就是 magpie 引用的那个目录）
//
// 官方把整组活动面 API 写成一个对象（下文称 `Po`），逐字：
//
//	startOnboardingTask:(e,t)=>No({localPath:`/api/daily-activity/onboarding/tasks/${encodeURIComponent(t)}/start`,
//	  upstreamPath:`/api/oauth/activities/onboarding/tasks/${encodeURIComponent(t)}/start`,
//	  method:`POST`,body:`{}`,idempotencyKey:`onboarding:start:${e.installationId}:${t}`,identity:e}),
//
//	completeOnboardingAction:(e,t)=>No({localPath:`/api/daily-activity/onboarding/tasks/${encodeURIComponent(t)}/complete`,
//	  upstreamPath:`/api/oauth/activities/onboarding/tasks/${encodeURIComponent(t)}/complete`,
//	  method:`POST`,body:`{}`,idempotencyKey:`onboarding:complete:${e.installationId}:${t}`,identity:e}),
//
//	reconcileOnboarding:(e,t)=>No({localPath:`/api/daily-activity/onboarding/reconcile`,
//	  upstreamPath:`/api/oauth/activities/onboarding/reconcile`,
//	  method:`POST`,body:JSON.stringify({...t?{session_id:t}:{}}),
//	  idempotencyKey:`onboarding:reconcile:${e.installationId}:${t??`latest`}:${Date.now()}`,identity:e}),
//
//	rewards:e=>No({localPath:`/api/daily-activity/rewards?type=daily_login`,
//	  upstreamPath:`/api/oauth/rewards`,method:`GET`,identity:e})
//
// 邀请面（另一个对象，下文称 `Id`）：
//
//	relations:e=>No({localPath:`/api/daily-activity/referral-relations`,
//	  upstreamPath:`/api/oauth/referrals/relations`,method:`GET`,identity:e})
//
// # 从这段官方代码里读出的四条硬事实
//
//	① **GET 也要 v1 签名**。`No()` 对所有方法都算 `sign_desktop_activity`，
//	   GET 的 body 是 `undefined` → 哈希的是空串。所以这里两个 GET 同样走 applyV1。
//	② **签名用的是 upstreamPath，不是 localPath**。`rewards` 的 localPath 带
//	   `?type=daily_login`，upstreamPath 是**干净的** `/api/oauth/rewards` ——
//	   这正好印证了 signV1 里那条"canonical 不许带 query"的护栏。
//	③ **reconcile 的幂等键带 Date.now()** → 它不是"去重"语义，每次调用都是一个新操作；
//	   而 start/complete 的键是稳定的（`<installationId>:<taskId>`），重复调用命中同一条。
//	④ **任务 id 由服务端给**，客户端只是搬运。sidecar 侧对它有校验
//	   `/^[a-z0-9_]{1,80}$/`（`J59` 里逐字），本文件照抄这条校验：
//	   非法 id 在本地就挡掉，不去打一个注定 400 的请求。
//
// # 本文件**不做**什么
//
//	· 不声明 CapTasks / 不改面板：能力声明是"承诺"，要有 UI 才能兑现（见 Caps 注释里的断言）。
//	  本文件只提供**能被调用的客户端方法** —— 先把地基做对，UI 单独一轮。
//	· 不猜响应结构：四个端点的响应形状官方前端没在本 bundle 里展开（它只是把 JSON 往下传），
//	  所以这里返回 json.RawMessage。**给未知结构编字段就是重复今天的错误。**
package phanthywork

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// ── 端点（与官方 upstreamPath 逐字一致）────────────────────────────────

const (
	// pathRewards 奖励/钱包。⚠ 上游**不带** `?type=daily_login`（那只在本地路径上）。
	pathRewards = "/api/oauth/rewards"

	// pathReferralsRelations 邀请关系列表（响应含 next_cursor）。
	pathReferralsRelations = "/api/oauth/referrals/relations"

	// pathOnboardingReconcile 新手任务对账。
	pathOnboardingReconcile = "/api/oauth/activities/onboarding/reconcile"

	// pathOnboardingTasksPrefix 新手任务动作前缀，完整形态 tasks/<taskId>/{start|complete}。
	pathOnboardingTasksPrefix = "/api/oauth/activities/onboarding/tasks/"
)

// onboardingTaskIDRe 任务 id 的合法形状。
//
// 真源：sidecar 的路由校验（`J59` 里逐字 `if(!/^[a-z0-9_]{1,80}$/.test(O))return null`）。
// 在本地挡掉非法 id，比让它去打一个必然 400 的请求更有信息量。
var onboardingTaskIDRe = regexp.MustCompile(`^[a-z0-9_]{1,80}$`)

// ── 幂等键（与官方模板逐字一致）──────────────────────────────────────

// OnboardingStartKey 拼 `onboarding:start:<installationId>:<taskId>`。
func OnboardingStartKey(installationID, taskID string) string {
	return "onboarding:start:" + installationID + ":" + taskID
}

// OnboardingCompleteKey 拼 `onboarding:complete:<installationId>:<taskId>`。
func OnboardingCompleteKey(installationID, taskID string) string {
	return "onboarding:complete:" + installationID + ":" + taskID
}

// OnboardingReconcileKey 拼 `onboarding:reconcile:<installationId>:<sessionID|"latest">:<ms>`。
//
// ⚠ 官方把 `Date.now()` 拼在键尾 —— 所以这条键**每次调用都不同**，它表达的是
// "这一次对账"而不是"这一天对账"。不要出于"省请求"把它改成固定值：
// 改了语义就变了（服务端可能把它当成同一个操作而丢弃后一次）。
func OnboardingReconcileKey(installationID, sessionID string, nowMS int64) string {
	seg := strings.TrimSpace(sessionID)
	if seg == "" {
		seg = "latest"
	}
	return fmt.Sprintf("onboarding:reconcile:%s:%s:%d", installationID, seg, nowMS)
}

// ── 两个通用发送器（对应官方 No() 的一条代码路径）────────────────────

// signedGetJSON 发一个带 v1 签名的 GET（无 body、无幂等键）。
//
// 官方 `No()` 对 GET 同样算 v1 签名，body 取 `undefined` → 哈希空串；
// `V1Canonical(..., nil, "")` 与之一致（sha256("") 的 base64url）。
func (c *Client) signedGetJSON(ctx context.Context, a *Auth, path, op string) (json.RawMessage, error) {
	if a == nil || a.OAuth.AccessToken == "" {
		return nil, errNoAccessToken
	}
	req, err := c.newJSONRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	timestamp := msToString(a.serverNowMS())
	nonce := newDeviceNonce()
	if err := applyV1(req, a, v1Params{
		Method: http.MethodGet, Path: path, Body: nil,
	}, timestamp, nonce); err != nil {
		return nil, err
	}
	status, body, derr := c.doJSON(req, summaryTimeout)
	if derr != nil {
		return nil, fmt.Errorf("phanthywork: %s请求失败: %w", op, derr)
	}
	if status < 200 || status >= 300 {
		return nil, &UpstreamError{Status: status, Body: string(body), Op: op}
	}
	return json.RawMessage(body), nil
}

// signedPostJSON 发一个带 v1 签名的 POST。
//
// idempotencyKey 为空串时**不发** `idempotency-key` 头，但 canonical 第 7 段仍是空串
// （分隔符不能省）—— 与官方 `e.idempotencyKey ?? \`\“ 一致。
func (c *Client) signedPostJSON(ctx context.Context, a *Auth, path string, body []byte, idempotencyKey, op string) (json.RawMessage, error) {
	if a == nil || a.OAuth.AccessToken == "" {
		return nil, errNoAccessToken
	}
	req, err := c.newJSONRequest(ctx, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	timestamp := msToString(a.serverNowMS())
	nonce := newDeviceNonce()
	if err := applyV1(req, a, v1Params{
		Method: http.MethodPost, Path: path, Body: body, IdempotencyKey: idempotencyKey,
	}, timestamp, nonce); err != nil {
		return nil, err
	}
	status, respBody, derr := c.doJSON(req, summaryTimeout)
	if derr != nil {
		return nil, fmt.Errorf("phanthywork: %s请求失败: %w", op, derr)
	}
	if status < 200 || status >= 300 {
		return nil, &UpstreamError{Status: status, Body: string(respBody), Op: op}
	}
	return json.RawMessage(respBody), nil
}

// ── 奖励 / 钱包 ────────────────────────────────────────────────────────

// FetchRewards `GET /api/oauth/rewards`（v1 签名）。
//
// 官方的本地路径带 `?type=daily_login`，但 upstreamPath 是干净的 ——
// 那个 query 是**本地代理侧**的过滤意图，不发往上游，也不进签名。
// 所以这里既不拼 query，也不在 canonical 里带 query（signV1 另有护栏兜底）。
//
// 响应里官方前端读的是 `wallet{availablePoints,totalPoints,balancePoints,reservedPoints}`
// 以及 `rewards[]`；本包不臆造结构，原样返回。
func (c *Client) FetchRewards(ctx context.Context, a *Auth) (json.RawMessage, error) {
	return c.signedGetJSON(ctx, a, pathRewards, "取奖励")
}

// ── 邀请关系 ──────────────────────────────────────────────────────────

// FetchReferralRelations `GET /api/oauth/referrals/relations`（v1 签名）。
//
// 响应形状来自本包既有注释（实测过 key 名）：`{schema_version, next_cursor, relations:[]}`。
// ⚠ 将来若要带 `?limit=`/`?cursor=`：**URL 可以带 query，签名的 path 不能带**
// （signV1 会当场报错拦住）。
func (c *Client) FetchReferralRelations(ctx context.Context, a *Auth) (json.RawMessage, error) {
	return c.signedGetJSON(ctx, a, pathReferralsRelations, "取邀请关系")
}

// ── 新手任务 ──────────────────────────────────────────────────────────

// onboardingTaskPath 拼 `…/tasks/<taskId>/<action>`，先校验 id 形状。
func onboardingTaskPath(taskID, action string) (string, error) {
	id := strings.TrimSpace(taskID)
	if !onboardingTaskIDRe.MatchString(id) {
		// 不把非法 id 拼进 URL 去打请求：官方路由对它的判据是
		// `/^[a-z0-9_]{1,80}$/`，不匹配就是 null（405/404），没有别的可能。
		return "", fmt.Errorf("phanthywork: 非法的新手任务 id %q（要求 ^[a-z0-9_]{1,80}$）", taskID)
	}
	return pathOnboardingTasksPrefix + url.PathEscape(id) + "/" + action, nil
}

// StartOnboardingTask `POST /api/oauth/activities/onboarding/tasks/<taskId>/start`。
//
// body 是**字面量空对象** `{}`（官方逐字 `body:\`{}\“）——不是空串，也不是 null。
// 幂等键 `onboarding:start:<installationId>:<taskId>`（稳定，重复调用命中同一条）。
func (c *Client) StartOnboardingTask(ctx context.Context, a *Auth, taskID string) (json.RawMessage, error) {
	if a == nil {
		return nil, errNilAuth
	}
	path, err := onboardingTaskPath(taskID, "start")
	if err != nil {
		return nil, err
	}
	instID, err := a.installationID()
	if err != nil {
		return nil, err
	}
	return c.signedPostJSON(ctx, a, path, []byte(`{}`),
		OnboardingStartKey(instID, strings.TrimSpace(taskID)), "开始新手任务")
}

// CompleteOnboardingAction `POST /api/oauth/activities/onboarding/tasks/<taskId>/complete`。
//
// body 同样是字面量 `{}`；幂等键 `onboarding:complete:<installationId>:<taskId>`。
//
// ⚠ 方法名按官方叫法（`completeOnboardingAction`，"action"而不是"task"）——
// 名字不同但打的是同一个 tasks 前缀。
func (c *Client) CompleteOnboardingAction(ctx context.Context, a *Auth, taskID string) (json.RawMessage, error) {
	if a == nil {
		return nil, errNilAuth
	}
	path, err := onboardingTaskPath(taskID, "complete")
	if err != nil {
		return nil, err
	}
	instID, err := a.installationID()
	if err != nil {
		return nil, err
	}
	return c.signedPostJSON(ctx, a, path, []byte(`{}`),
		OnboardingCompleteKey(instID, strings.TrimSpace(taskID)), "完成新手任务")
}

// ReconcileOnboarding `POST /api/oauth/activities/onboarding/reconcile`。
//
// body：官方是 `JSON.stringify({...t?{session_id:t}:{}})` ——
//
//	没有 sessionId ⇒ 字面量 `{}`
//	有   sessionId ⇒ `{"session_id":"<id>"}`
//
// 幂等键含 `Date.now()`（见 OnboardingReconcileKey 的说明），所以每次调用都是新操作。
func (c *Client) ReconcileOnboarding(ctx context.Context, a *Auth, sessionID string) (json.RawMessage, error) {
	if a == nil {
		return nil, errNilAuth
	}
	instID, err := a.installationID()
	if err != nil {
		return nil, err
	}
	sid := strings.TrimSpace(sessionID)
	body := []byte(`{}`)
	if sid != "" {
		// 手写序列化以保证键序与官方一致（签名覆盖字节）。
		body = []byte(fmt.Sprintf(`{"session_id":%s}`, mustJSONQuote(sid)))
	}
	return c.signedPostJSON(ctx, a, pathOnboardingReconcile, body,
		OnboardingReconcileKey(instID, sid, nowMS()), "新手任务对账")
}

// mustJSONQuote 用 encoding/json 生成一个合法的 JSON 字符串字面量（含引号）。
//
// 单独抽出来是为了让上面的 fmt.Sprintf 模板读起来就是官方那行 `{"session_id":%s}`。
func mustJSONQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal 对 string 不会失败；真失败了返回一个空串字面量，
		// 让上游去拒绝，而不是在这里 panic 掉整个出站循环。
		return `""`
	}
	return string(b)
}
