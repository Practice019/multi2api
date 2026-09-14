// points.go loomy 的**积分/任务/邀请码**接口（`https://loomyad.xunfei.cn/api/v1/...`）。
//
// # 与前两个客户端的关系
//
//	client.go     模型代理  /api/v1/chat/completions、/api/v1/models   Bearer / token 双轨
//	smslogin.go   账号网关  account.xfinfr.com                          HMAC-SHA1 签名
//	**points.go** 积分网关  同一个 host 的 /api/v1/points|onboarding|invitation-codes
//	                        鉴权头**只有** `token: <session>`
//
// 三个是不同的服务，只是积分网关与模型代理恰好同域。所以本文件独立于 client.go，
// 只共用客户端已有的 BaseURL（去掉 `/api/v1` 后缀就是积分网关的根）。
//
// # 本轮修掉的那个"额度查询有问题"
//
// 上一轮的 `QuotaExt` 读的是**本机客户端缓存的 `loomy-points-summary`**，
// 于是有两个天然缺陷：① 只有"本机登录的那一个账号"有值，池里其它号恒为 `—`；
// ② 值只在客户端运行时才更新，网关自己的消耗不会写回。
//
// 而积分网关**直接就能查**（`GET /api/v1/points/records` 回执里带 `balance`），
// 而且是**按 session 逐个账号查** —— 上面两个缺陷同时消失。
//
// # 业务错误也是 HTTP 200（三处都一样，最容易踩）
//
// 回执统一是 `{"code":"000000","desc":"...","data":{...}}`。
// 判据必须看 `code`，不能看状态码 —— 与模型代理的"200 + 缺少 token"同源。
package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 积分网关的路径前缀（与模型代理同域、同 `/api/v1` 段）。
const pointsPrefix = "/api/v1"

// TaskPoints 新手任务注册表：任务 key → 奖励积分。
//
// 来自 `electron/onboarding-service.js` 的 TASK_POINTS，共 10000 分。
// 顺序**就是界面上的顺序**（先聊天、再选技能…），所以用切片而不是 map ——
// map 遍历顺序随机会让每次刷新按钮顺序都变。
type TaskDef struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Points int64  `json:"points"`
}

// TaskDefs 八个新手任务（顺序即展示顺序）。
var TaskDefs = []TaskDef{
	{"first_message", "首次对话", 500},
	{"pick_skill", "选择技能", 1000},
	{"generate_ppt", "生成 PPT", 1500},
	{"set_schedule", "设置日程", 1000},
	{"install_skill", "安装技能", 1500},
	{"configure_remote", "配置远程", 1000},
	{"create_soul", "创建人格", 1500},
	{"share_soul", "分享人格", 2000},
}

// taskPointsOf 取某任务的奖励分；未知 key 返回 0（照做，不报错）。
func taskPointsOf(key string) int64 {
	for _, d := range TaskDefs {
		if d.Key == key {
			return d.Points
		}
	}
	return 0
}

// pointsBase 积分网关的根（把模型代理基址的 `/api/v1` 去掉）。
//
// 为什么从 client.BaseURL 推导而不是再开一个配置项：
// 两者实测就是同一个 host（`loomyad.xunfei.cn`），多一个配置项就多一个
// "配了 A 忘了配 B"的分叉面。上游真拆域时再加即可。
func (p *Provider) pointsBase() string {
	base := ""
	if p != nil && p.client != nil {
		base = p.client.BaseURL
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/api/v1")
	if base == "" {
		base = "https://loomyad.xunfei.cn"
	}
	return base
}

// pointsCall 发一次积分网关请求并解出 `data`。
//
// 请求头**只有** `token: <session>` —— 实测这个网关不认 Authorization
// （与 `/models` 同一条轨，不是 `/chat/completions` 那条）。
func (p *Provider) pointsCall(ctx context.Context, session, method, path string, body any) (json.RawMessage, error) {
	if strings.TrimSpace(session) == "" {
		return nil, fmt.Errorf("loomy: 缺少 session（积分接口的唯一鉴权材料）")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("loomy: 序列化积分请求失败: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	url := p.pointsBase() + pointsPrefix + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("loomy: 构造积分请求失败（%s）: %w", path, err)
	}
	req.Header.Set("token", session)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 与 client.go 同一条：不设 http.Client.Timeout，超时交给 ctx。
	h := defaultHTTP()
	if p != nil && p.client != nil && p.client.HTTP != nil {
		h = p.client.HTTP
	}
	resp, err := h.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loomy: 调用积分接口失败（%s）: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, maxAccountBody))

	var env struct {
		Code string          `json:"code"`
		Desc string          `json:"desc"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(buf, &env); err != nil {
		return nil, fmt.Errorf("loomy: 积分接口回执不是合法 JSON（HTTP %d %s）: %s",
			resp.StatusCode, path, summarize(buf))
	}
	if env.Code != "" && env.Code != "000000" {
		// desc 是给人看的（"邀请码无效"/"验证码错误"…），原样带出去。
		return nil, fmt.Errorf("loomy: 积分接口 %s 返回 %s: %s", path, env.Code, env.Desc)
	}
	return env.Data, nil
}

// ── 额度 ────────────────────────────────────────────────────────────────

// Balance 查该 session 对应的账号当前**可用总额度**。
//
// # 为什么是 availableBalance 而不是 balance（用户实测报的"没加上送的额度"）
//
// `GET /api/v1/points/records` 的回执实测（2026-09-15）带**三个**余额字段：
//
//	balance           25000   总余额（邀请/充值累积，**不含**每日赠送）
//	dailyBalance      1434    每日剩余（当天还能花多少，次日重置）
//	availableBalance  26434   **可用总额度** = balance + 当日剩余（25000+1434）
//
// 而且流水里的 `balanceBefore/balanceAfter` 走的正是 `availableBalance`
// （26484 → 26434，扣的 50 分就是从它扣的）。第一版只读了 `balance`，
// 于是"送的额度"（每日赠送那部分）在界面上**永远看不到** —— 用户报的
// "是不是没有加上送的额度"就是它。
//
// # 返回值
//
//	(可用总额度, 每日剩余, error) —— 可用总额度优先取 availableBalance，
//	拿不到（老接口）时回落 balance。每日剩余只是给界面展示用，不是主值。
func (p *Provider) Balance(ctx context.Context, session string) (int64, int64, error) {
	data, err := p.pointsCall(ctx, session, http.MethodGet,
		"/points/records?pageNo=1&pageSize=1", nil)
	if err != nil {
		return 0, 0, err
	}
	// 优先看有 balance 的那一层；两层都试。
	for _, raw := range []json.RawMessage{data, nestedData(data)} {
		if len(raw) == 0 {
			continue
		}
		var w struct {
			Balance          *int64 `json:"balance"`
			DailyBalance     *int64 `json:"dailyBalance"`
			AvailableBalance *int64 `json:"availableBalance"`
		}
		if err := json.Unmarshal(raw, &w); err != nil ||
			(w.Balance == nil && w.AvailableBalance == nil) {
			continue
		}
		var out, daily int64
		// 可用总额度优先；这是用户要求展示的那个数。
		if w.AvailableBalance != nil {
			out = *w.AvailableBalance
		} else {
			out = *w.Balance
		}
		if w.DailyBalance != nil {
			daily = *w.DailyBalance
		}
		return out, daily, nil
	}
	return 0, 0, fmt.Errorf("loomy: 积分账本回执里没有 balance/availableBalance 字段: %s", summarize(data))
}

// nestedData 取 `{"data":{...}}` 里那一层；没有就返回 nil。
func nestedData(raw json.RawMessage) json.RawMessage {
	var w struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil
	}
	return w.Data
}

// ── 新手任务 ────────────────────────────────────────────────────────────

// Tasks 查该账号的新手任务完成状态（key → 是否已完成）。
func (p *Provider) Tasks(ctx context.Context, session string) (map[string]bool, error) {
	data, err := p.pointsCall(ctx, session, http.MethodGet, "/onboarding/tasks", nil)
	if err != nil {
		return nil, err
	}
	for _, raw := range []json.RawMessage{data, nestedData(data)} {
		if len(raw) == 0 {
			continue
		}
		var w struct {
			Tasks map[string]bool `json:"tasks"`
		}
		if err := json.Unmarshal(raw, &w); err != nil || w.Tasks == nil {
			continue
		}
		return w.Tasks, nil
	}
	// 形状不认时返回空表 + 错误：调用方据此显示"查不到"，
	// 而不是把"全部未完成"当成事实（那会让用户重复点一遍已完成的任务）。
	return nil, fmt.Errorf("loomy: 任务回执里没有 tasks 字段: %s", summarize(data))
}

// CompleteTask 完成一个任务；返回 (本次是否已入账, 完成后的余额)。
func (p *Provider) CompleteTask(ctx context.Context, session, key string) (bool, int64, error) {
	data, err := p.pointsCall(ctx, session, http.MethodPost,
		"/onboarding/tasks/complete", map[string]any{"key": key})
	if err != nil {
		return false, 0, err
	}
	var w struct {
		AlreadyCompleted bool   `json:"alreadyCompleted"`
		Balance          *int64 `json:"balance"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &w)
	}
	var bal int64
	if w.Balance != nil {
		bal = *w.Balance
	}
	return w.AlreadyCompleted, bal, nil
}

// ─ 邀请码 ──────────────────────────────────────────────────────────────

// Activation 查激活状态：是否已绑定邀请码、绑的是哪一个。
func (p *Provider) Activation(ctx context.Context, session string) (activated bool, applied string, err error) {
	data, err := p.pointsCall(ctx, session, http.MethodGet, "/points/activation", nil)
	if err != nil {
		return false, "", err
	}
	for _, raw := range []json.RawMessage{data, nestedData(data)} {
		if len(raw) == 0 {
			continue
		}
		var w struct {
			Activated             *bool  `json:"activated"`
			AppliedInvitationCode string `json:"appliedInvitationCode"`
		}
		if err := json.Unmarshal(raw, &w); err != nil || w.Activated == nil {
			continue
		}
		return *w.Activated, w.AppliedInvitationCode, nil
	}
	return false, "", fmt.Errorf("loomy: 激活状态回执里没有 activated 字段: %s", summarize(data))
}

// BindInvite 绑定邀请码（激活）。
//
// deviceId 是协议要求的（客户端用 `loomy-campus-<uuid>`）。我们每次生成一个新的：
// 它只用于风控归因，不参与鉴权；复用同一个反而会让多个账号被识别成同一台设备。
func (p *Provider) BindInvite(ctx context.Context, session, code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return fmt.Errorf("loomy: 邀请码不能为空")
	}
	_, err := p.pointsCall(ctx, session, http.MethodPost, "/points/activation",
		map[string]any{"inviteCode": code, "deviceId": "loomy-campus-" + randomUUID()})
	return err
}

// FirstLogin 首登初始化（新号的积分账号引导）。
//
// # 为什么需要它（用户报"最后一个账号的邀请码没有显示"的根因）
//
// 客户端**每次登录后**会自动调 `POST /api/v1/points/first-login` 完成积分账号
// 初始化：服务端下发注册奖励（实测 registerReward=5000）并**生成 5 个邀请码**。
// 而通过本网关**导入/添加**的账号跳过了这一步 —— 表现就是：
//
//	invitation-codes 返回 {"list":[]}（"我生成的码"空）
//	注册奖励也没到账
//
// 实测（2026-09-15）：对空账号调用本端点后，invitation-codes 立刻返回 5 个
// active 码（FVZJTM/V8JFV6/XJUXH2/6H9JRB/5PJDRM），balance 补上注册奖励。
// 服务端有 `alreadyProcessed` 幂等保护，重复调用不会重复发奖。
//
// ⚠ 不带邀请码调用（inviteCode 留空）：这里是"补初始化"不是"绑定"。
func (p *Provider) FirstLogin(ctx context.Context, session string) error {
	_, err := p.pointsCall(ctx, session, http.MethodPost, "/points/first-login",
		map[string]any{"deviceId": "loomy-campus-" + randomUUID()})
	return err
}

// InviteCode 一个**本账号生成的**邀请码。
//
// # 字段名来自实测（这里踩过一个坑，用户报的"我生成的码没有显示"）
//
// 回执实测（2026-09-15，`GET /api/v1/invitation-codes`）：
//
//	{"data":{"list":[{"inviteCode":"E3HRN8","usedCount":1,"maxUses":1,
//	                  "status":"exhausted"}, {"inviteCode":"SSK5MC",...}]}}
//
// 字段是 **`inviteCode`**，不是 `code`。第一版只认 `code`，于是
// "我生成的码"那一列**永远是空的** —— 而接口明明返回了 5 个。
// 一个字段名猜错 = 整块功能看起来没做，且没有任何报错。
//
// 现在两个都认（`inviteCode` 优先），并把**状态**一起带出来：
// `status`/`usedCount`/`maxUses` 让界面能区分「还能用」与「已用完」——
// 后者是 maxUses=1 的码被别人绑掉之后的常态，混在一起显示会误导用户。
type InviteCode struct {
	Code      string `json:"code"`
	UsedCount int64  `json:"usedCount"`
	MaxUses   int64  `json:"maxUses"`
	Status    string `json:"status"`
}

// InvitationCodes 查本账号**生成的**邀请码列表。
//
// 形状兼容三种（实测是第一种）：
//
//	{"list":[{inviteCode,...}]}
//	{"data":{"list":[...]}}      ← v2 风格的外层包装
//	[{...}] 或 ["ABC123"]        ← 裸数组 / 纯字符串元素
func (p *Provider) InvitationCodes(ctx context.Context, session string) ([]InviteCode, error) {
	data, err := p.pointsCall(ctx, session, http.MethodGet, "/invitation-codes", nil)
	if err != nil {
		return nil, err
	}
	list := extractCodeList(data)
	out := make([]InviteCode, 0, len(list))
	for _, raw := range list {
		// 元素可能是纯字符串，也可能是对象。
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, InviteCode{Code: s, MaxUses: 1, Status: "active"})
			}
			continue
		}
		var obj struct {
			InviteCode string `json:"inviteCode"`
			Code       string `json:"code"`
			UsedCount  *int64 `json:"usedCount"`
			MaxUses    *int64 `json:"maxUses"`
			Status     string `json:"status"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		code := firstNonEmpty(obj.InviteCode, obj.Code)
		if code == "" {
			continue
		}
		c := InviteCode{Code: code, Status: obj.Status}
		if obj.UsedCount != nil {
			c.UsedCount = *obj.UsedCount
		}
		if obj.MaxUses != nil {
			c.MaxUses = *obj.MaxUses
		}
		out = append(out, c)
	}
	return out, nil
}

// extractCodeList 从三种可能的形状里取出那一列元素。
func extractCodeList(data json.RawMessage) []json.RawMessage {
	var list []json.RawMessage
	if err := json.Unmarshal(data, &list); err == nil && list != nil {
		return list
	}
	take := func(raw json.RawMessage) []json.RawMessage {
		if len(raw) == 0 {
			return nil
		}
		var w struct {
			List []json.RawMessage `json:"list"`
		}
		if err := json.Unmarshal(raw, &w); err == nil && w.List != nil {
			return w.List
		}
		return nil
	}
	if l := take(data); l != nil {
		return l
	}
	if inner := nestedData(data); len(inner) > 0 {
		if l := take(inner); l != nil {
			return l
		}
		var l2 []json.RawMessage
		if err := json.Unmarshal(inner, &l2); err == nil {
			return l2
		}
	}
	return nil
}

// pointsTimeout 积分接口的默认超时（这些都是短请求）。
const pointsTimeout = 20 * time.Second

// pointsCtx 给积分接口用的 ctx。
func pointsCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), pointsTimeout)
}

// parseIntLoose 把可能是字符串的数字解出来（上游有把数值写成字符串的先例）。
func parseIntLoose(raw json.RawMessage) (int64, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
