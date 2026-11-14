// invite.go 邀请码面板（gateway.CapInvite）：一行一个账号的邀请码。
//
// # 取证（2026-11-12，3 次真实 GET + 报告复核；证据 D:/temp/ph2-referrals.txt）
//
//	GET  /api/oauth/referrals/me        200  → {schema_version, referral:{22 个键}}（**没有任何"码"字段**）
//	GET  /api/oauth/referrals/relations 200  → {schema_version, next_cursor, relations:[]}
//	GET  /api/oauth/referrals/code      405  → **POST-only**
//
// ⇒ **平台不提供"看自己的邀请码"的只读字段**；码的唯一来源是 `POST /api/oauth/referrals/code`
//
//	（报告 §1.1/§1.2 实测：body 空对象 `{}`、幂等键 `referral-code:<installationId>`（**无日期段**，
//	2026-11-14 按官方前端 bundle 更正）、
//	v1 签名；返回**固定 5 槽**，每槽 `{slot, code(32字符,URL用), invite_code(XXXX-XXXX-XXXX,给人念), share_url}`；
//	**同日重复 POST 返回完全相同的 5 槽** —— 重取不换码）。
//
// # 因此这是一个"读了会写"的面板，护栏写死在下面（用户 2026-11-12 明确授权）
//
//	① **只在面板被打开 / 点刷新时**调用，**不挂定时任务、不后台预热**（本文件不注册任何 Job）；
//	② **每账号每自然日最多一次 POST** —— 当日缓存命中就不再打（见 inviteRowsSerial + inviteViewFor）；
//	③ **按账号串行 + 两次写之间 ≥ inviteCodeSpacing（1s）**；失败**不回滚、不重试**，只把该行标成可读错误；
//	④ `eligible_to_invite=false` 或拿不到 installationId ⇒ **一次都不调**，该行给可读原因；
//	⑤ 真实调用只用于一次自验（≤1 账号 1 次），其余验证全离线 httptest。
package phanthywork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

const (
	// pathReferralsMe 邀请资格与额度（**只读**，实测不需要签名，只带 Bearer）。
	pathReferralsMe = "/api/oauth/referrals/me"
	// pathReferralsCode 取/建邀请码（**写**：v1 签名 + 幂等键；同日幂等）。
	pathReferralsCode = "/api/oauth/referrals/code"
	// invitePath 面板端点（与 loomy 同形：GET /admin/<id>/invite）。
	invitePath = "/admin/phanthywork/invite"
	// invitePageSize 每页行数（照 loomy 写死 25）。
	invitePageSize = 25
)

// inviteCodeSpacing 两次**写**（取/建码）之间的最小间隔。
//
// 抽成变量是为了测试能压成 0（照 dailycheckin 的 refreshSpacing 形态）。
var inviteCodeSpacing = time.Second

// ── 上游响应结构（键名逐字来自实测，不猜测）────────────────────────────

// referralsMeResponse GET /api/oauth/referrals/me（2026-11-12 实测 22 个键）。
type referralsMeResponse struct {
	SchemaVersion int `json:"schema_version"`
	Referral      struct {
		EligibleToInvite bool `json:"eligible_to_invite"`
		// EligibilityReason 实测为 null（资格 OK 时不给原因）。
		EligibilityReason any    `json:"eligibility_reason"`
		ProgramStatus     string `json:"program_status"`
		RewardPoints      int64  `json:"reward_points"`
		NextRewardPoints  int64  `json:"next_reward_points"`
		// InviteeRewardPoints 被邀请人拿到的分（实测 3000，与邀请人同额）。
		InviteeRewardPoints int64   `json:"invitee_reward_points"`
		RewardTiers         []int64 `json:"reward_tiers"`
		// InvitedProgress 实测 null（还没有邀请记录）。
		InvitedProgress    any   `json:"invited_progress"`
		PendingCount       int64 `json:"pending_count"`
		TotalGranted       int64 `json:"total_granted"`
		TotalLimit         int64 `json:"total_limit"`
		TotalOccupied      int64 `json:"total_occupied"`
		RemainingSlots     int64 `json:"remaining_slots"`
		DailyLimit         int64 `json:"daily_limit"`
		DailyGranted       int64 `json:"daily_granted"`
		DailyCommitted     int64 `json:"daily_committed"`
		TierAllocatedCount int64 `json:"tier_allocated_count"`
		TierGrantedCount   int64 `json:"tier_granted_count"`
		RecentRelations    []any `json:"recent_relations"`
	} `json:"referral"`
}

// referralCodeSlot 一个邀请槽位。四个字段都有**实测出处**（报告 §1.2 的五槽表）。
type referralCodeSlot struct {
	Slot       int64  `json:"slot"`
	Code       string `json:"code"`
	InviteCode string `json:"invite_code"`
	ShareURL   string `json:"share_url"`
}

// referralCodeContainers 顶层容器名的候选。
//
// ⚠ 报告只写了"返回固定 5 个槽位"，**没写数组挂在哪个键下** —— 所以这里先认几个候选，
//
//	认不出再走 scanReferralSlots 扫顶层数组（判据是"元素里有 invite_code/code 字段"）。
//	这不是"猜字段"（槽内字段名都有实测出处），而是不把"厂商把数组放哪"当成已知。
var referralCodeContainers = []string{"slots", "codes", "invite_codes", "items", "list"}

// ── 客户端调用 ──────────────────────────────────────────────────────────

// FetchReferrals 读邀请资格与额度（只读；实测不需要 v1 签名 ⇒ 只带 Bearer）。
func (c *Client) FetchReferrals(ctx context.Context, a *Auth) (referralsMeResponse, error) {
	var out referralsMeResponse
	if a == nil || a.OAuth.AccessToken == "" {
		return out, errNoAccessToken
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase()+pathReferralsMe, nil)
	if err != nil {
		return out, err
	}
	c.applyCommonHeaders(req)
	req.Header.Set("Authorization", AuthorizationHeaderValue(a.OAuth.AccessToken))
	status, body, derr := c.doJSON(req, httpTimeout)
	if derr != nil {
		return out, fmt.Errorf("phanthywork: 读邀请资格失败: %w", derr)
	}
	if status < 200 || status >= 300 {
		return out, &UpstreamError{Status: status, Body: string(body), Op: "读邀请资格"}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("phanthywork: 邀请资格响应不是 JSON: %w", err)
	}
	return out, nil
}

// FetchReferralCodes 取/建邀请码（**写**：v1 签名 + 幂等键；同日幂等、重取不换码）。
func (c *Client) FetchReferralCodes(ctx context.Context, a *Auth, idempotencyKey string) ([]inviteSlot, error) {
	if a == nil || a.OAuth.AccessToken == "" {
		return nil, errNoAccessToken
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		// 与签到同一条纪律：**没有幂等键就不发** —— 这条是写操作，重放代价不对称。
		return nil, errors.New("phanthywork: 缺少取码幂等键，拒绝发送")
	}
	const body = `{}` // ① body = 字面量空对象（报告 §1.1 实测）
	timestamp := msToString(a.serverNowMS())
	nonce := newDeviceNonce()
	req, err := c.newJSONRequest(ctx, http.MethodPost, pathReferralsCode, []byte(body))
	if err != nil {
		return nil, err
	}
	if err := applyV1(req, a, v1Params{
		Method: http.MethodPost, Path: pathReferralsCode, Body: []byte(body), IdempotencyKey: idempotencyKey,
	}, timestamp, nonce); err != nil {
		return nil, err
	}
	status, respBody, derr := c.doJSON(req, summaryTimeout)
	if derr != nil {
		return nil, fmt.Errorf("phanthywork: 取邀请码失败: %w", derr)
	}
	if status < 200 || status >= 300 {
		return nil, &UpstreamError{Status: status, Body: string(respBody), Op: "取邀请码"}
	}
	slots, perr := parseReferralSlots(respBody)
	if perr != nil {
		return nil, perr
	}
	return slots, nil
}

// parseReferralSlots 从响应里取出槽位数组（先认候选容器，再扫顶层数组）。
func parseReferralSlots(raw []byte) ([]inviteSlot, error) {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("phanthywork: 取码响应不是 JSON 对象: %w", err)
	}
	for _, k := range referralCodeContainers {
		if arr, ok := top[k].([]any); ok {
			if s := slotsFromArray(arr); len(s) > 0 {
				return s, nil
			}
		}
	}
	// 候选都没命中：扫所有顶层数组，取"元素里带 invite_code 或 code"的那个。
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		arr, ok := top[k].([]any)
		if !ok {
			continue
		}
		if s := slotsFromArray(arr); len(s) > 0 {
			return s, nil
		}
	}
	// 空数组是**合法**结果（例如平台认为没有可用的槽）⇒ 报 nil 错误 + 空切片。
	return nil, nil
}

func slotsFromArray(arr []any) []inviteSlot {
	out := make([]inviteSlot, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if _, hasInvite := m["invite_code"]; !hasInvite {
			if _, hasCode := m["code"]; !hasCode {
				continue // 这一层不是槽位（可能是别的数组）
			}
		}
		var raw referralCodeSlot
		b, err := json.Marshal(m)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			continue
		}
		out = append(out, inviteSlot{
			Slot: raw.Slot, InviteCode: raw.InviteCode, Code: raw.Code, ShareURL: raw.ShareURL,
		})
	}
	return out
}

// ── 面板行 ──────────────────────────────────────────────────────────────

// inviteSlot 面板 JSON 里的一个槽（字段名与上游一致；`invite_code` 是给人念的那套）。
type inviteSlot struct {
	Slot       int64  `json:"slot"`
	InviteCode string `json:"invite_code,omitempty"`
	Code       string `json:"code,omitempty"`
	ShareURL   string `json:"share_url,omitempty"`
}

// accountInviteView 一个账号的邀请码行。
//
// ⚠ 字段全部来自 2026-11-12 实测的那 22 个键（一个不编）；`slots` 只在**成功取到码**时出现，
//
//	失败时该行给 `error`（**不给空数组** —— 空数组会被界面读成"这人没有码"）。
type accountInviteView struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	// OK 这一行的码是否取到（与 Error 互斥）。
	OK               bool         `json:"ok"`
	EligibleToInvite bool         `json:"eligible_to_invite"`
	ProgramStatus    string       `json:"program_status,omitempty"`
	RewardPoints     int64        `json:"reward_points"`
	NextRewardPoints int64        `json:"next_reward_points"`
	InviteeReward    int64        `json:"invitee_reward_points"`
	RewardTiers      []int64      `json:"reward_tiers,omitempty"`
	PendingCount     int64        `json:"pending_count"`
	TotalGranted     int64        `json:"total_granted"`
	TotalLimit       int64        `json:"total_limit"`
	RemainingSlots   int64        `json:"remaining_slots"`
	Slots            []inviteSlot `json:"slots,omitempty"`
	// FetchedAt 这一行**什么时候读到的**（照 loomy 的每行时刻纪律）。
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	// FromCache 这一行是当日缓存（护栏②生效的证据：没有再打上游）。
	FromCache bool   `json:"from_cache,omitempty"`
	Error     string `json:"error,omitempty"`

	// ── 前端渲染契约字段（2026-11-12 加）────────────────────────────────
	//
	// ⚠ 前端（internal/server/ui/app.js）的列表头**完全由字段存在与否驱动**
	//   （`colSet`：`a[f] !== undefined && a[f] !== null` ⇒ 有该字段才画那一列），
	//   **不认上游名**（有守卫 TestWebUIHasNoUpstreamNameBranch 禁止分叉）。
	//   所以"让某几列出现/不出现"的唯一手段就是**给不给这些字段** —— 下面每一个都注明了：
	//   给了会长出哪一列、为什么给这个值。
	//
	// 「我生成的码」列：存在 ⇒ 出现。每项只给 `code`/`status`，
	// ⚠ **不编** `usedCount`/`maxUses`（平台没给，编了会渲染成"用过 x/y"的假数据）。
	Codes []inviteCodeView `json:"codes,omitempty"`
	// 「我的邀请码」列：存在 ⇒ 出现（取第一个槽：给人念的那套 + 分享链接）。
	MyCode string `json:"my_code,omitempty"`
	// 「邀请链接」跟在 my_code 后面渲染。
	MyLink string `json:"my_link,omitempty"`
	// BalanceKnown 三态：false ⇒ 该格画 `—`（**不是** 0）。
	// ⚠ 本上游我们**没有**取余额口径（额度在另两块：summary 的 credits / 四窗口），
	//   所以这里必须诚实地说"没查到"，而不是填 0 让它画一个假的余额。
	BalanceKnown bool `json:"balance_known"`
	// InvitedKnown 同上：本账号的"是否被别人邀请"我们读不到（平台不给只读字段）。
	// ⚠ 只给 `invited_known`、**不给 `invited`** —— 后者会让「绑定状态」列出现。
	InvitedKnown bool `json:"invited_known"`
	// WechatBoundKnown 同上（本上游没有微信绑定这一套）。
	// ⚠ 只给 `*_known`、不给 `wechat_bound` —— 后者会让「微信」列出现。
	WechatBoundKnown bool `json:"wechat_bound_known"`
	// BindSupported **必须为 false**：本上游"事后绑定别人的码"实测 409
	//   `existing_user_ineligible`（不可补绑）。前端据此**不画**绑定输入框 ——
	//   否则界面上会出现一个点了必然失败的控件（本仓明确的"假按钮"反模式）。
	//   前端判据是 `a.bind_supported !== false`（老上游没有这个字段 ⇒ 行为不变）。
	BindSupported bool `json:"bind_supported"`
}

// inviteCodeView 前端契约里的一个码（只要这两项 —— 见 Codes 的说明）。
type inviteCodeView struct {
	Code   string `json:"code"`
	Status string `json:"status"`
}

// inviteRow 一个账号的当日行缓存。
type inviteRow struct {
	day  string
	view accountInviteView
}

// inviteDay 面板的"今天"基准：**UTC+8** 自然日。
//
// ⚠ 与平台的换日口径一致（报告 §2.2/§4.7：服务端日键是 UTC+8）—— 取码幂等键里的日期也是它。
//
//	两边基准不同会出现"面板以为今天取过了、平台以为是新的一天"这类错配。
func inviteDay(now time.Time) string {
	return now.UTC().Add(8 * time.Hour).Format("2006-01-02")
}

// ReferralCodeIdempotencyKey 拼取码幂等键：`referral-code:<installationId>`。
//
// ★★ 2026-11-14 修正：此前这里是 `referral-code:<installationId>:<YYYY-MM-DD>`（带日期段），
// **错了** —— 多一个 `:<day>` 段就与官方不是同一个键，上游的幂等语义（"同一天重复调用命中同一条"）
// 对我们等于失效。
//
// 真源：官方桌面端前端 bundle 的 API 定义（`_up_\dist\assets\App-dSHMvpu3.js`，逐字）：
//
//	code:e=>No({localPath:`/api/daily-activity/referral-code`,
//	            upstreamPath:`/api/oauth/referrals/code`,method:`POST`,body:`{}`,
//	            idempotencyKey:`referral-code:${e.installationId}`,identity:e})
//
// ⚠ 这个键**不带日期**，所以它其实是"每个安装一个码"的语义（与签到的
// `daily:<installId>:<day>` 不同 —— 那条**带**日期）。两条不要互相类推。
//
// 也纠正了第三方插件 magpie 的记载（它写 `referral-code:<installationId>:<ms>`）：
// 毫秒段同样不存在。
func ReferralCodeIdempotencyKey(installationID string) string {
	return "referral-code:" + installationID
}

// ── Provider 侧 ─────────────────────────────────────────────────────────

// inviteViewFor 取一个账号的面板行。第二个返回值 = **这一行是否打了写接口**（供串行节奏用）。
//
// 顺序（每一步都有护栏对应）：
//
//	0 拿不到 installationId ⇒ 直接给可读原因，**零调用**（护栏④）
//	1 当日缓存命中且非强制刷新 ⇒ 直接回缓存（护栏② 的兑现点）
//	2 GET /referrals/me（只读）—— 拿资格与额度
//	3 eligible_to_invite=false ⇒ **不打写接口**，给可读原因（护栏④）
//	4 POST /referrals/code（写；幂等键 `referral-code:<installationId>`，**不带日期**）—— 失败只标这一行（护栏③，不回滚不重试）
func (p *Provider) inviteViewFor(ctx context.Context, a *Auth, refresh bool) (accountInviteView, bool) {
	// ⚠ 只取**显式昵称字段**（`NicknameValue`），不走 `Auth.Nickname()` 的回落链 ——
	//   后者会回落到 AccountScope/uuid 甚至 uid，那些都不是"名字"（见 credential.go:262）。
	v := newInviteRow(a.UID(), inviteDisplayName(a.NicknameValue))
	instID, ierr := a.installationID()
	if ierr != nil || strings.TrimSpace(instID) == "" {
		v.Error = "这个账号拿不到设备安装 id（installation id）；取邀请码要用它拼幂等键，所以没有调用上游"
		return v, false
	}
	// ★ 统一入口（2026-11-13）：取码那条 POST 是**签名请求**，未注册必被拒 ⇒ 先就绪。
	//   ⚠ 放在缓存判定**之前**："就绪"是这一行的前置条件，与缓存无关（缓存命中时它也只读一次内存）。
	if err := p.ensureReady(ctx, a); err != nil {
		v.Error = inviteErrText("设备就绪失败", err)
		return v, false
	}
	today := inviteDay(time.Now())

	if cached, ok := p.inviteCached(a.UID(), today, refresh); ok {
		return cached, false
	}

	// 2 只读：资格与额度。
	me, merr := p.client.FetchReferrals(ctx, a)
	if merr != nil {
		v.Error = inviteErrText("读邀请资格失败", merr)
		p.storeInviteRow(a.UID(), today, v)
		return v, false
	}
	applyReferralMe(&v, me)

	// 3 前置条件不满足 ⇒ 一次都不调写接口。
	if !v.EligibleToInvite {
		reason := "平台说这个账号当前不可邀请（eligible_to_invite=false）"
		if s := strings.TrimSpace(fmt.Sprint(me.Referral.EligibilityReason)); s != "" && s != "<nil>" {
			reason += "：" + s
		} else if v.ProgramStatus != "" {
			reason += "（program_status=" + v.ProgramStatus + "）"
		}
		v.Error = reason
		p.storeInviteRow(a.UID(), today, v)
		return v, false
	}

	// 3.5 今日已取过码（缓存里已有槽）⇒ 不重复打写接口，只刷新只读那部分。
	if prev, ok := p.invitePrevDayRow(a.UID(), today); ok && len(prev.Slots) > 0 {
		prev.Nickname = v.Nickname
		prev.EligibleToInvite = v.EligibleToInvite
		prev.ProgramStatus = v.ProgramStatus
		prev.RewardPoints = v.RewardPoints
		prev.NextRewardPoints = v.NextRewardPoints
		prev.InviteeReward = v.InviteeReward
		prev.RewardTiers = v.RewardTiers
		prev.PendingCount = v.PendingCount
		prev.TotalGranted = v.TotalGranted
		prev.TotalLimit = v.TotalLimit
		prev.RemainingSlots = v.RemainingSlots
		prev.OK = true
		prev.Error = ""
		prev.FromCache = true
		p.storeInviteRow(a.UID(), today, prev)
		return prev, false
	}

	// 4 写：取/建邀请码。
	//   幂等键 = `referral-code:<installationId>`（**不带日期**，见 ReferralCodeIdempotencyKey 的真源）。
	slots, cerr := p.client.FetchReferralCodes(ctx, a, ReferralCodeIdempotencyKey(instID))
	if cerr != nil {
		v.Error = inviteErrText("取邀请码失败", cerr)
		p.storeInviteRow(a.UID(), today, v)
		return v, true // 已经打过写接口（节奏按"尝试"算）
	}
	v.OK = true
	v.Slots = slots
	applyInviteCodes(&v)
	p.storeInviteRow(a.UID(), today, v)
	return v, true
}

// applyReferralMe 把只读响应填进行里（字段一一对应，实测键名）。
func applyReferralMe(v *accountInviteView, me referralsMeResponse) {
	r := me.Referral
	v.EligibleToInvite = r.EligibleToInvite
	v.ProgramStatus = r.ProgramStatus
	v.RewardPoints = r.RewardPoints
	v.NextRewardPoints = r.NextRewardPoints
	v.InviteeReward = r.InviteeRewardPoints
	v.RewardTiers = r.RewardTiers
	v.PendingCount = r.PendingCount
	v.TotalGranted = r.TotalGranted
	v.TotalLimit = r.TotalLimit
	v.RemainingSlots = r.RemainingSlots
	v.FetchedAt = time.Now()
}

// newInviteRow 造一行，并把**前端契约里恒定的那四个布尔**一次性写死。
//
// ⚠ 四个都放在这里而不是各分支各写一遍：漏写一个的后果是静默的 ——
//
//	例如漏 `bind_supported:false` ⇒ 界面上立刻多出一个点了必然失败的「绑定」框。
func newInviteRow(uid, nickname string) accountInviteView {
	return accountInviteView{
		UID: uid, Nickname: nickname,
		BalanceKnown:     false, // 没取余额口径 ⇒ 画 —
		InvitedKnown:     false, // 读不到"被别人邀请"的状态 ⇒ 画 —
		WechatBoundKnown: false, // 本上游没有微信绑定 ⇒ 画 —
		BindSupported:    false, // 事后绑定 409 ⇒ **不画**绑定框
	}
}

// applyInviteCodes 把 5 个槽映射成前端契约字段（codes / my_code / my_link）。
//
// ⚠ 只搬我们**真的知道**的：`code`（给人念的 `invite_code`）与 `status`（`active`）。
//
//	平台的槽位没有"用过几次/最多几次"这两个量（报告 §1.2 的五槽表就只有 code/invite_code/share_url）——
//	编一个 `usedCount:0/maxUses:1` 会让前端渲染出"用过 0/1"，那是**我们编的事实**。
//	⚠ 所以前端那条 tooltip 也改成"字段缺就不显示那半句"（见 app.js 的同日改动）。
func applyInviteCodes(v *accountInviteView) {
	if v == nil || len(v.Slots) == 0 {
		return
	}
	v.Codes = make([]inviteCodeView, 0, len(v.Slots))
	for _, s := range v.Slots {
		if strings.TrimSpace(s.InviteCode) == "" {
			continue
		}
		v.Codes = append(v.Codes, inviteCodeView{Code: s.InviteCode, Status: "active"})
	}
	// 「我的邀请码」取第一个槽（用户在界面上要"我这个账号的码"，第一条就是）。
	v.MyCode = v.Slots[0].InviteCode
	v.MyLink = v.Slots[0].ShareURL
}

// inviteCached 当日缓存命中（refresh=true 时不命中 —— 允许用户手动重读**只读**部分）。
func (p *Provider) inviteCached(uid, day string, refresh bool) (accountInviteView, bool) {
	if refresh {
		return accountInviteView{}, false
	}
	p.inviteMu.Lock()
	defer p.inviteMu.Unlock()
	row, ok := p.inviteRows[uid]
	if !ok || row == nil || row.day != day {
		return accountInviteView{}, false
	}
	out := row.view
	out.FromCache = true
	return out, true
}

// invitePrevDayRow 今日缓存里**已经取到码**的那一行（用于"同日不重复写"）。
func (p *Provider) invitePrevDayRow(uid, day string) (accountInviteView, bool) {
	p.inviteMu.Lock()
	defer p.inviteMu.Unlock()
	row, ok := p.inviteRows[uid]
	if !ok || row == nil || row.day != day {
		return accountInviteView{}, false
	}
	return row.view, true
}

func (p *Provider) storeInviteRow(uid, day string, v accountInviteView) {
	if v.FetchedAt.IsZero() {
		v.FetchedAt = time.Now()
	}
	p.inviteMu.Lock()
	defer p.inviteMu.Unlock()
	if p.inviteRows == nil {
		p.inviteRows = map[string]*inviteRow{}
	}
	p.inviteRows[uid] = &inviteRow{day: day, view: v}
}

// inviteErrText 把上游错误翻成人能读的一句（**不带凭证、不带整段 body**）。
func inviteErrText(op string, err error) string {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		switch ue.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return op + "：凭据失效或被拒（HTTP " + fmt.Sprint(ue.Status) + "），请重新登录这个账号"
		case http.StatusMethodNotAllowed:
			return op + "：平台不允许这个方法（HTTP 405）"
		case http.StatusTooManyRequests:
			return op + "：被限流（HTTP 429），今天不再重试（取码是写操作）"
		default:
			if strings.Contains(ue.Body, "existing_user_ineligible") {
				return op + "：平台判定这个账号不满足邀请条件"
			}
			return op + "：上游回 HTTP " + fmt.Sprint(ue.Status)
		}
	}
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return op + "：" + s
}

// inviteDisplayName 挑一个**有意义**的展示名；拿不到就返回空串（= 不给这个字段）。
//
// # 为什么必须挑（2026-11-13 用户原话：「这个名字为什么也看不到啊？名字看不到，这一列就不要了」）
//
// 本上游账号的 `Auth.Nickname()` 会**回落**成手机号（凭据里的昵称值）或 AccountScope/uid ——
// 把打码后的 `+*************1` 塞进 nickname，用户既认不出是谁，面板还被迫长出一整列废列。
// ⇒ 只放行"看着像名字"的值：手机号、uuid/uid 这类一律空串 ——
//
//	前端 `cols.nickname` 就判不出有值，整列自然不渲染（判据在前端，**不是**按上游名分叉）。
//
// ⚠ 返回**空串**而不是"给个占位"：前端那条判据要求 trim 后非空，给 `—` 或空格会把列重新拉出来。
func inviteDisplayName(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || looksLikePhone(s) || looksLikeOpaqueID(s) {
		return ""
	}
	return maskInviteDisplay(s)
}

// looksLikePhone 整串只有数字与 +-()空白 ⇒ 当手机号/座机（不是名字）。
func looksLikePhone(s string) bool {
	digit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		// ⚠ `*` 也要算进去：平台自己给的昵称就是**脱敏手机号**（`+86 150****3411`，见 credential.go:163）
		//   —— 不放过它，那一列就会重新长出来（用户报障看到的正是它）。
		case r == '+' || r == '-' || r == '(' || r == ')' || r == ' ' || r == '\t' || r == '*':
		default:
			return false
		}
	}
	return digit
}

// looksLikeOpaqueID 看着像机器标识（长、无空格无大写、含足量数字，如本仓 uid `cmwc64nj…`）⇒ 不是名字。
//
// ⚠ 判据刻意**宽松**：宁可少给一列，也不要把 uid 当"名字"展示 —— 那正是这轮报障的形态。
func looksLikeOpaqueID(s string) bool {
	// ⚠ 先去掉分隔符再判：uuid（`1a2b3c4d-…-…`）带横线，不去掉就漏过 ——
	//   我第一版就漏了它，夹具（AccountScope 是 uuid）当场把「昵称」列又拉了出来（被断言抓住）。
	c0 := strings.NewReplacer("-", "", "_", "", " ", "").Replace(s)
	if len(c0) < 16 {
		return false
	}
	digits := 0
	for _, r := range c0 {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'a' && r <= 'z':
		default:
			return false
		}
	}
	// ⚠ 阈值 4（不是 8）：本仓 uid 形如 cmwc64nj21ejmsw3ilrzs2iq2 —— 26 字符、纯小写+数字，
	//   但只有 6 位数字 ⇒ 8 的阈值漏过它（我的用例当场抓住）。判定组合为「≥16 字符 + 纯小写/数字 + 含数字」，
	//   足够不像人名 ⇒ 宁可少给一列。
	return digits >= 4
}

// maskInviteDisplay 昵称脱敏（照 loomy 的 maskDisplay 口径）。
func maskInviteDisplay(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= 2 {
		return string(r[:1]) + "*"
	}
	return string(r[:1]) + strings.Repeat("*", len(r)-2) + string(r[len(r)-1:])
}

// inviteAccounts 面板要展示的账号（复用签到那条取法：uid 枚举 + 本地凭证，最后回落本地）。
func (p *Provider) inviteAccounts(ctx context.Context) ([]*Auth, error) {
	up := &checkinUpstream{p: p}
	accs, err := up.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Auth, 0, len(accs))
	for _, acc := range accs {
		a, ok := acc.Secret.(*Auth)
		if !ok || a == nil {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// inviteRowsSerial 逐个账号取行：**串行** + 两次写之间 ≥ inviteCodeSpacing（护栏③）。
//
// ⚠ 刻意**不**用 fanOutConcurrent（loomy 那套）：那边是只读扇出，这边是**写**，
//
//	并发写会在上游看到"同一毫秒的 N 个建码请求"，正是风控想看的形状。
func (p *Provider) inviteRowsSerial(ctx context.Context, accts []*Auth, refresh bool) []accountInviteView {
	out := make([]accountInviteView, 0, len(accts))
	wrote := false
	for _, a := range accts {
		if ctx.Err() != nil {
			break
		}
		if wrote && inviteCodeSpacing > 0 {
			time.Sleep(inviteCodeSpacing)
		}
		v, w := p.inviteViewFor(ctx, a, refresh)
		wrote = w
		out = append(out, v)
	}
	return out
}

// handleInviteList GET /admin/phanthywork/invite —— 一行一个账号的邀请码。
//
// 查询参数与 loomy 同形：`?uids=a,b`（小批取）、`?uid=`（单个）、`?page=` + `?refresh=1`。
//
// ⚠ 本面板的"打开"就是"按需调用"的触发点（护栏①）—— 它**不注册任何 Job**，也不会被后台预热。
func (p *Provider) handleInviteList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	refresh := q.Get("refresh") == "1"
	all, err := p.inviteAccounts(ctx)
	if err != nil {
		writeInviteJSON(w, http.StatusOK, map[string]any{
			"accounts": []accountInviteView{},
			"page":     1, "pages": 1, "total": 0, "size": invitePageSize,
			"updated_at": time.Now().Format(time.RFC3339),
			"summary":    map[string]any{"accounts": 0, "eligible": 0, "with_codes": 0, "errors": 1},
			"error":      inviteErrText("列账号失败", err),
		})
		return
	}
	totalN := len(all)

	byUID := map[string]*Auth{}
	for _, a := range all {
		byUID[a.UID()] = a
	}

	// `?uids=` 小批取（形状与 loomy 一致）。
	if uids := parseInviteUIDList(q.Get("uids")); len(uids) > 0 {
		picked := make([]*Auth, 0, len(uids))
		out := make([]accountInviteView, 0, len(uids))
		for _, u := range uids {
			if a, ok := byUID[u]; ok {
				picked = append(picked, a)
				continue
			}
			out = append(out, accountInviteView{UID: u, Error: "这个账号不在凭证目录里"})
		}
		rows := p.inviteRowsSerial(ctx, picked, refresh)
		// 保持请求顺序：先按请求序把已取到的行插回去，再把"目录里没有"的行插回原位。
		merged := make([]accountInviteView, 0, len(uids))
		ri := 0
		for _, u := range uids {
			if _, ok := byUID[u]; ok {
				merged = append(merged, rows[ri])
				ri++
				continue
			}
			for _, miss := range out {
				if miss.UID == u {
					merged = append(merged, miss)
					break
				}
			}
		}
		writeInviteJSON(w, http.StatusOK, map[string]any{
			"accounts": merged,
			"page":     1, "pages": 1, "total": len(merged), "size": invitePageSize,
			"updated_at": time.Now().Format(time.RFC3339),
			"summary":    inviteSummary(merged),
		})
		return
	}

	if uid := strings.TrimSpace(q.Get("uid")); uid != "" {
		if a, ok := byUID[uid]; ok {
			rows := p.inviteRowsSerial(ctx, []*Auth{a}, refresh)
			writeInviteJSON(w, http.StatusOK, map[string]any{
				"accounts": rows,
				"page":     1, "pages": 1, "total": 1, "size": invitePageSize,
				"updated_at": time.Now().Format(time.RFC3339),
				"summary":    inviteSummary(rows),
			})
			return
		}
		writeInviteJSON(w, http.StatusOK, map[string]any{
			"accounts": []accountInviteView{{UID: uid, Error: "这个账号不在凭证目录里"}},
			"page":     1, "pages": 1, "total": 1, "size": invitePageSize,
			"updated_at": time.Now().Format(time.RFC3339),
			"summary":    inviteSummary(nil),
		})
		return
	}

	page, pages, lo, hi := invitePage(q.Get("page"), totalN)
	rows := p.inviteRowsSerial(ctx, all[lo:hi], refresh)
	writeInviteJSON(w, http.StatusOK, map[string]any{
		"accounts": rows,
		"page":     page, "pages": pages, "total": totalN, "size": invitePageSize,
		"updated_at": time.Now().Format(time.RFC3339),
		"summary":    inviteSummary(rows),
	})
}

// invitePage 分页（照 loomy 的 panelPage 口径：写死 25 行/页，页码越界夹紧）。
func invitePage(raw string, total int) (page, pages, lo, hi int) {
	pages = (total + invitePageSize - 1) / invitePageSize
	if pages < 1 {
		pages = 1
	}
	page = 1
	if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && v > 0 {
		page = v
	}
	if page > pages {
		page = pages
	}
	lo = (page - 1) * invitePageSize
	hi = lo + invitePageSize
	if hi > total {
		hi = total
	}
	if lo > total {
		lo = total
	}
	return page, pages, lo, hi
}

// parseInviteUIDList 解析 `?uids=a,b,c`（去空、去重、保持顺序）。
func parseInviteUIDList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	for _, part := range strings.Split(raw, ",") {
		u := strings.TrimSpace(part)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// inviteSummary 面板汇总（字段与 loomy 的 summary **形状同族**，但语义按本上游实测的键来）。
func inviteSummary(rows []accountInviteView) map[string]any {
	eligible, withCodes, errs := 0, 0, 0
	for _, v := range rows {
		if v.EligibleToInvite {
			eligible++
		}
		if len(v.Slots) > 0 {
			withCodes++
		}
		if strings.TrimSpace(v.Error) != "" {
			errs++
		}
	}
	return map[string]any{
		"accounts": len(rows), "eligible": eligible, "with_codes": withCodes, "errors": errs,
	}
}

// writeInviteJSON 写 JSON 响应（与 loomy 的 writer 同形：UTF-8 + 不转义 HTML）。
func writeInviteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// inviteRoutes 面板路由（Capability 与 Caps() 的声明**必须一致** —— 面板由能力位驱动）。
func (p *Provider) inviteRoute() gateway.AdminRoute {
	return gateway.AdminRoute{
		Method:     http.MethodGet,
		Path:       invitePath,
		Handler:    p.handleInviteList,
		Capability: gateway.CapInvite,
		Title:      "邀请码",
	}
}
