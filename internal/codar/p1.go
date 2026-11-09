package codar

// p1.go —— P1 能力（T-318）：额度 / 成本倍率 / 每日动作（签到）/ 续期任务。
//
// # 关于 token 续期：**默认开启**（T-382）
//
// T-318 读 codar2api 源码得到的事实（与 T-316 的猜测不同，这里更正）：
//
//	· `POST {PayBase}/v1/gettoken {username, token:0}` **不是续期** —— 它是**查余额**：
//	  codar2api 把它放在 "balance" 分支里，返回的 token/agent_token/rewrite_token
//	  是**额度字段**（积分），不是会话令牌。
//	· 真正的续期是 `POST {PayBase}/v1/token/refresh`（带账号自己的 Bearer）⇒ 返回一个新 JWT
//	  （codar2api 校验 username 一致后 **替换** 存储的 token，upstream.go:511-523）。
//
// ⚠ 但源码**看不出**旧 token 会不会因此失效。我们的凭证是**桌面端会话 token 的副本**，
// 万一 refresh 是"轮换"语义，用户桌面端会当场掉线 —— 这个险不能由网关代替用户冒。
// ⇒ 但同文件 L540 的实测把上面这条顾虑推翻了：上游对未临期的 token 直接 400
// "Token is not close to expiration" ⇒ 早调**不会**轮换、也就不会踢掉桌面端。
// ⇒ 因此改正为**默认开启**：该不该跑由 Jobs.Due 的两条护栏决定（≤25% 或 ≤24h，且距上次≥1h）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

var (
	_ gateway.QuotaExt           = (*Provider)(nil)
	_ gateway.ModelMultiplierExt = (*Provider)(nil)
	_ gateway.DailyActionExt     = (*Provider)(nil)
	_ gateway.DailyCheckinExt    = (*Provider)(nil)
	_ gateway.JobExt             = (*Provider)(nil)
)

// 每日动作 ID（前端 button 的 data-act）。
//
// ⚠ T-465：原先还有第二条动作 ID（那套已删除的功能），已随整套一并移除。
// （上游活动已结束：实测 activity_status=unavailable / visible=false）。
// 详见下方 DailyActions() 的说明。
const (
	DailyActionSign = "sign" // 签到
)

// 全量端点（分组头上的「全部…」按钮）。
const (
	checkinAllPath = "/admin/codar/sign/all"
)

// ── QuotaExt：额度 ───────────────────────────────────────────────────

// RefreshQuota 查一个账号的余额。
//
// # 数据来源（T-317 实测）
//
//	POST {PayBase}/v1/agent/balance {"username": …}
//	→ {"code":200,"message":"Success","data":{"username":"…","agent_balance":1715}}
//
// # 为什么不用 billing.cost_cny 自己累加
//
// 响应里的 billing.cost_cny 是**单次**花费，累加会与实际余额漂移（重试、缓存命中、
// 平台侧调整都会让它偏）；余额是权威值，而且只要一次请求。
// codar2api 的"主用 /v1/gettoken、失败回落 /v1/agent/balance"里，我选后者是因为
// **前者我们没实测过**（T-317 只验了后者）。等哪天实测了 gettoken 的三个额度字段再考虑切换。
// RefreshQuota 查账号**可花**的总积分（T-348 定稿：三桶求和）。
//
// # 两个桶都能花，所以要相加 —— 这是**消费实验**的结论（T-347）
//
//	POST {PayBase}/v1/agent/balance {"username":<uid>}
//	→ {"data":{"agent_balance":1706}}                   ← agent 桶
//
//	POST {PayBase}/v1/gettoken {"username":<uid>,"token":0}
//	→ {"token":2500,"agent_token":1706,"rewrite_token":0}
//	       ↑general 桶   ↑与 agent_balance 同值      ↑本项目恒 0
//
// # 为什么"相加"是对的（这里反复过三次，写清楚免得再翻）
//
//	T-346 初版：凭三个字段名**猜**着求和 ⇒ 无证据，撤回；
//	T-346 修订：改成"只认 agent_balance"（把 token 当上限）；
//	T-348 定稿：做消费实验，两次对照：
//
//	  老账号：agent_balance 1707→1706（-1）· token 2500→2500（不动）
//	  新账号：agent_balance    0→   0（不动）· token  500→ 499（-1）
//	  ledger：新账号新增 change=-1 point_type=general
//
//	⇒ token **会因消费减少** ⇒ 它不是上限，是 general 桶的余额；
//	  两桶各自能花，扣费顺序是「先 agent 后 general」。
//	  ⚠ 看到这里的后来者：不要再把 token 当上限 —— 上限不会减，它减了。
//
// # rewrite_token
//
//	两个账号都恒 0，语义未验（按 0 计入；若将来发现它也能花，这里自动跟随）。
//
// # 部分数据
//
//	只拿到 agent/balance（gettoken 失败）⇒ 只返回 agent 桶值，**不**把缺的桶当 0 相加
//	（那会显示成"余额变少了"，比"未知"更误导）；两个都失败 ⇒ UnknownQuota（不拿 0 充数）。
//
// # 不要用 /api/user/self 的 quota
//
//	该字段在本产品里恒 0（quota=0 / used_quota=0 / aff_quota=0），而 request_count 是真实的
//	⇒ 那是 one-api 风格的遗留字段，不是余额。
//
// RefreshQuota 实现 gateway.QuotaExt（签名由接口决定，不能加 ctx 参数）。
//
// T-367：真正的逻辑搬进 refreshQuotaCtx，这里只是用 Background 调它 ⇒ 公开行为与以前**完全一致**
// （既有 quota 相关测试的期望值因此一个都不用改）。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	return p.refreshQuotaCtx(context.Background(), uid)
}

// refreshQuotaCtx 与 RefreshQuota 同逻辑，但**用调用方的 ctx**。
//
// 为什么需要它：邀请面板给每个账号派生了单账号预算（inviteAccountTimeout）。
// 若余额这一步自建 20s ctx，那个预算对它就是失效的 —— 卡住的账号会把整行拖满 20s。
// refreshQuotaCtx 是 refreshQuotaDetailCtx 的薄封装，只把"三桶"压成一个数。
//
// ⚠ 返回语义与 T-367 时**逐字一致**（既有 5 个 quota 测试的期望值因此一个都不用改）：
//
//	agent 桶拿不到            ⇒ (UnknownQuota(), false)
//	agent 拿到、gettoken 失败 ⇒ (CreditsQuota(agent), true)      ← 注意仍是 **true**（部分数据可信）
//	三桶齐全                  ⇒ (CreditsQuota(agent+general+rewrite), true)
func (p *Provider) refreshQuotaCtx(ctx context.Context, uid string) (gateway.QuotaView, bool) {
	agent, general, rewrite, partial, ok := p.refreshQuotaDetailCtx(ctx, uid)
	if !ok {
		return gateway.UnknownQuota(), false
	}
	if partial {
		return gateway.CreditsQuota(agent), true
	}
	return gateway.CreditsQuota(agent + general + rewrite), true
}

// refreshQuotaDetailCtx 取三个桶的**明细**（T-371）。
//
// 为什么需要明细：调用方只拿到一个总数时，无法区分"三桶齐全"与"只有 agent 桶" ——
// 实测老号 4206 = agent 1706 + general 2500 + rewrite 0，gettoken 挂掉时只报 1706，
// 界面就会把一个"部分数"当成全量展示（数字对不上用户预期，却没有提示）。
// partial=true 表示"gettoken 没取到，返回值只含 agent 桶"。
func (p *Provider) refreshQuotaDetailCtx(ctx context.Context, uid string) (agent, general, rewrite int64, partial bool, ok bool) {
	a, err := p.pickAuth(uid)
	if err != nil {
		log.Printf("codar: 额度刷新取不到凭证 uid=%q err=%v（凭证没并池？点一次「重载 auths」）", uid, err)
		return 0, 0, 0, false, false
	}
	// ⚠ 容忍 nil ctx：公开签名允许调用方传 nil（既有测试就是这么调的），
	// 而 context.WithTimeout(nil, …) 会 panic ⇒ 这里统一兜底成 Background。
	// （改实现而不是改老测试：nil-ctx 是所有调用方的坑，不该让调用方各自小心。）
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// ① agent 桶（必须成功 —— 它是余额的锚）
	raw, status, cerr := p.callJSON(ctx, http.MethodPost, p.payBase+"/v1/agent/balance", a,
		map[string]any{"username": a.UID})
	if cerr != nil || status != http.StatusOK {
		log.Printf("codar: 余额查询失败 uid=%q status=%d err=%v body=%s", uid, status, cerr, truncateStr(string(raw), 200))
		return 0, 0, 0, false, false
	}
	var bal struct {
		Data struct {
			AgentBalance int64 `json:"agent_balance"`
		} `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &bal); jerr != nil {
		log.Printf("codar: 余额响应解析失败 uid=%q err=%v body=%s", uid, jerr, truncateStr(string(raw), 200))
		return 0, 0, 0, false, false
	}
	agent = bal.Data.AgentBalance // ⚠ agent 是具名返回值 ⇒ 不能用 :=
	// ② general / rewrite 桶（拿不到不算失败 —— 只标"部分数据"）
	var okG bool
	general, rewrite, okG = p.fetchGeneralBuckets(ctx, a) // 具名返回值 ⇒ 不能用 :=
	if !okG {
		log.Printf("codar: gettoken 不可用 uid=%q ⇒ 只报 agent 桶 %d（部分数据，界面会标记）", uid, agent)
		return agent, 0, 0, true, true
	}
	log.Printf("codar: 额度 uid=%q agent=%d general=%d rewrite=%d total=%d", uid, agent, general, rewrite, agent+general+rewrite)
	return agent, general, rewrite, false, true
}

// fetchGeneralBuckets 取 general（gettoken 的 token）与 rewrite 桶。失败 ok=false。
func (p *Provider) fetchGeneralBuckets(ctx context.Context, a *Auth) (general, rewrite int64, ok bool) {
	raw, status, err := p.callJSON(ctx, http.MethodPost, p.payBase+"/v1/gettoken", a,
		map[string]any{"username": a.UID, "token": 0})
	if err != nil || status != http.StatusOK {
		return 0, 0, false
	}
	var out struct {
		Token        int64 `json:"token"`
		RewriteToken int64 `json:"rewrite_token"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return 0, 0, false
	}
	return out.Token, out.RewriteToken, true
}

// ── ModelMultiplierExt：成本倍率 ──────────────────────────────────────

// ModelMultipliers 用目录里的 pricing 算相对最便宜模型的倍率。
//
// # 算法（T-318 定的口径）
//
//	基准 = 输入价与输出价都最低的组合（实测是 glm-5.3-flash：0.8 in / 2.8 out CNY 每百万 token）
//	倍率 = (input/基准in + output/基准out) / 2     ← 输入输出各占一半权重
//
// ⇒ glm-5.3-flash = 1.0 / deepseek-v4.1-flash ≈ 2.68 / claude-opus-5-5 ≈ 15.18
// 只在**目录真的带 pricing** 时才算；拿不到就返回空 map（核心会回落"无倍率"）。
func (p *Provider) ModelMultipliers(ctx context.Context, cred gateway.Credential) (map[string]float64, error) {
	price, err := p.client.FetchPricing(ctx, p.apiBase)
	if err != nil || len(price) == 0 {
		// T-322 诊断：核心的 ModelMultipliers 走不通时，界面倍率会退化成"未知"。
		// 这条日志是唯一能区分"没被调用"与"调用了但拉不到目录"的证据。
		log.Printf("codar: ModelMultipliers 拿不到价格（uid=%q err=%v 条数=%d）—— 界面倍率将显示为未知", cred.UID, err, len(price))
		return map[string]float64{}, nil // 拿不到不报错：倍率是锦上添花，不该让请求失败
	}
	log.Printf("codar: ModelMultipliers 命中 %d 个模型价（uid=%q）", len(price), cred.UID)
	baseIn, baseOut := 0.0, 0.0
	for _, pr := range price {
		if pr.Input <= 0 || pr.Output <= 0 {
			continue
		}
		if baseIn == 0 || (pr.Input < baseIn && pr.Output < baseOut) {
			baseIn, baseOut = pr.Input, pr.Output
		}
	}
	if baseIn <= 0 || baseOut <= 0 {
		return map[string]float64{}, nil
	}
	out := make(map[string]float64, len(price))
	for id, pr := range price {
		if pr.Input <= 0 || pr.Output <= 0 {
			continue
		}
		out[id] = (pr.Input/baseIn + pr.Output/baseOut) / 2
	}
	return out, nil
}

// FetchPricing 拉目录并取出每个模型的单价（CNY / 百万 token）。
//
// ⚠ 目录里 pricing 的字段名是 input / output（T-311 实测），单位 per_million_tokens。
func (c *Client) FetchPricing(ctx context.Context, apiBase string) (map[string]Pricing, error) {
	raw, err := c.fetchCatalogRaw(ctx, apiBase)
	if err != nil {
		return nil, err
	}
	var cr struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Input  float64 `json:"input"`
				Output float64 `json:"output"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, err
	}
	out := make(map[string]Pricing, len(cr.Data))
	for _, m := range cr.Data {
		if m.ID == "" {
			continue
		}
		out[m.ID] = Pricing{Input: m.Pricing.Input, Output: m.Pricing.Output}
	}
	return out, nil
}

// Pricing 一个模型的单价（CNY / 百万 token）。
type Pricing struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

// ── DailyActionExt：每日动作（★ 全部**手动**触发，不做自动循环）──────────

// DailyActions 手动动作：签到。
//
// ⚠ 为什么不做自动：用户只有 1 个号，而上游已有风控记录（T-307 社区
// "API access is restricted"）—— 自动化把唯一一个号打进受限，损失远大于收益。
// 这里只提供"手动按钮"，节奏由用户自己掌握。
//
// ⚠ OneURL 必须真的挂在 AdminRoutes 里（本文件下方已挂），否则按钮点了 404。
//
// # T-465：原先还有第二个动作（领取活动赠送的模型），已整条删除
//
// 依据是**实测**而不是猜测：那条只读端点回的是"活动不可用、且上游连显示都不给"
// （原文见 git 历史与本任务的事故记录，这里不复述标识符 —— 见下）。本文件的注释
// 早写明了这一点，只是当时选择了"如实回显不可领"。
//
// 用户看到那个按钮问"这按钮有啥用" —— 它的唯一作用就是点出一句"不可领"。
// 保留它等于维护一个**恒失败**的入口，而**删掉它**的成本只是"上游若重开活动，
// 需要重新实现一遍"（且那时的接口形状未必与今天相同）。
// ⇒ 结论：删。本文件与 extensions.go 里的路由/handler 一并移除。
//
// ⚠ 墓碑注释**刻意不复述**被删代码的标识符与界面文案：复述会让"防复活守卫"
// 需要区分"注释里的字符串"与"真代码里的字符串"（本仓为此踩过 6 次）。
// 守卫本身也只断言子串（见 contract_hermetic_test.go 的那条用例）。
func (p *Provider) DailyActions() []gateway.DailyAction {
	return []gateway.DailyAction{
		{
			ID: DailyActionSign, Label: "签到", Title: "签到领积分（每天一次；已签会返回状态）",
			OneURL: signOnePath, AllURL: checkinAllPath, Batch: true,
		},
	}
}

const (
	signOnePath = "/admin/codar/sign/do"
)

// CheckinAll 对本上游**全部账号**跑“签到”动作（「全部签到」按钮）。
//
// ⚠ 契约：Error 只表示"整趟跑不动"（比如列账号失败）；单个账号失败写在 Results 里。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	rep := gateway.DailyCheckinReport{Provider: providerID}
	creds := p.loadAll()
	if len(creds) == 0 {
		return rep // 0 个账号不是失败
	}
	for _, cred := range creds {
		a, err := authOf(cred)
		if err != nil {
			rep.Results = append(rep.Results, gateway.DailyCheckinResult{UID: cred.UID, Status: "skip", Detail: err.Error()})
			continue
		}
		rep.Results = append(rep.Results, p.doSign(ctx, a, "manual"))
	}
	return rep
}

// doSign 执行一次签到（写操作）。
//
// # 语义（T-317 查状态 + codar2api 源码）
//
//	POST {PayBase}/v1/daily-sign {"username": …}     ← 写
//	状态查询已在 T-317 实现：POST /v1/daily-sign/status → {"isSigned":true,"retry_after":79529}
//
// ⇒ "already signed" 必须判成 status=already（不是 fail，更不是 ok），
//
//	否则界面会把"今天已签过"报成"签到成功"（谎报）。
func (p *Provider) doSign(ctx context.Context, a *Auth, trigger string) gateway.DailyCheckinResult {
	res := gateway.DailyCheckinResult{UID: a.UID}
	// 无论成功/已签/失败都要记一条 —— 用 defer 闭包（Go 的 defer 参数是**立即求值**的，
	// 写 `defer p.recordSign(res)` 会记下零值，那就等于什么都没记）。
	defer func() { p.recordSign(res, trigger) }()
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raw, status, err := p.callJSON(cctx, http.MethodPost, p.payBase+"/v1/daily-sign", a, map[string]any{"username": a.UID})
	if err != nil {
		res.Status, res.Detail = "fail", err.Error()
		return res
	}
	var parsed struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &parsed)
	msg := parsed.Message
	// ★ T-351：**先判"已签"、再判失败**。实测已签时上游返回 HTTP 200 + 业务 code != 200
	// + 中文措辞 `24小时内仅可领取一次`。此前的顺序是先按 code!=200 判 fail，
	// 于是老账号每次签到都被记成"签到失败"（用户截图的现象），后面那个只认英文的 switch 根本没执行。
	if isAlreadySignedMsg(msg) {
		res.Status, res.Detail = "already", msg
		return res
	}
	if status != http.StatusOK || parsed.Code != 200 {
		res.Status, res.Detail = "fail", fmt.Sprintf("上游 HTTP %d：%s", status, msg)
		return res
	}
	res.Status, res.Detail = "ok", msg
	return res
}

func lower(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			r += 32
		}
		out = append(out, r)
	}
	return string(out)
}

// ── 管理端点（P1 追加）：写端点只在用户点按钮时跑 ──────────────────────

// handleSignOne POST /admin/codar/sign/do?uid=   —— 手动签到一个账号。
func (p *Provider) handleSignOne(w http.ResponseWriter, r *http.Request) {
	a, err := p.pickAuth(r.URL.Query().Get("uid"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	res := p.doSign(r.Context(), a, "manual")
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.Status == "ok" || res.Status == "already", "uid": res.UID, "status": res.Status, "detail": res.Detail})
}

// handleSignAll POST /admin/codar/sign/all —— 全部账号签到。
func (p *Provider) handleSignAll(w http.ResponseWriter, r *http.Request) {
	rep := p.CheckinAll(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "report": rep})
}

// ⚠ T-465：这里原有两条 handler（单账号 / 全部账号地调用上游那个"领取活动赠送模型"
// 的端点）。整套已删除 —— 上游活动已结束（实测 activity_status=unavailable、
// visible=false），保留只会给用户一个点了一定失败的按钮。详见 DailyActions()。
//
// 墓碑注释刻意**不写**被删代码里的标识符与文案，理由写在同包防复活守卫测试的注释里。

// ── JobExt：token 续期（★ 默认开启，见文件头；护栏见 Jobs.Due）─────────────────────────

// ★ T-382：原来的 TokenRefreshAllowed（环境变量开关）已删除 —— 续期**默认开启**。
//
// 删掉的理由：这个开关的依据（"怕提前轮换踢掉桌面端会话"）被同文件的实测推翻了 ——
// 上游对未临期 token 直接 400 "Token is not close to expiration" ⇒ 早调根本不会轮换。
// 开关买不到安全，只让用户撞上 token 过期。真正的护栏是 Jobs.Due 那两条判据（≤25%/≤24h + 1h 退避）。

// Jobs 定时任务：token 续期。
//
// ⚠ 这里**只有** token-refresh。签到**不在这里** —— T-427 把 8 个上游的签到
// 统一成了一个跨上游的共享 Job（`internal/scheduler/dailycheckin.go`，
// 任务名 `core-daily-checkin`）。本上游是它的覆盖对象之一。
//
// # 为什么把 T-424 那个 codar 专属签到 Job 删掉
//
// T-424 曾在这里加过一个 `codar-daily-checkin`。共享 Job 落地后它就**重复**了：
// 两个 Job 都会调 `CheckinAll` ⇒ codar 每小时被签两次 ⇒
// 违反共享 Job 的「每上游每天 1 趟」闸门，而 codar 面板明确写着
// 「上游已有风控记录，请低频使用」—— 打坏了要人工恢复。
// ⇒ 幂等由共享 Job 的内存闸门统一保证，**上游侧不再各写一份**
//
//	（同理：checkedInToday / hasUnsignedToday / runCheckinScan 三个函数也一并删除，
//	 它们在共享 Job 的闸门面前是死代码）。
//
// ⚠ 默认**不执行**（见文件头）：`/v1/token/refresh` 是否会让旧 token 失效无法从源码判定，
// 而我们的凭证是桌面端会话的副本 —— 万一它是轮换语义，用户桌面端会掉线。
// 因此：Due 只在开关打开时返回 true；开关关着时这个任务等于空转（不产生任何上游请求）。
func (p *Provider) Jobs() []gateway.Job {
	return []gateway.Job{
		{
			Name:     "codar-token-refresh",
			Interval: 6 * time.Hour,
			// ★ Due 必须自己判断"到没到时候"，否则核心会在**每个调度 tick**（30s）都跑它
			// ——2026-11-08 实测踩过：一天 2880 次上游请求（虽被上游 400 挡着，对风控极不友好）。
			//
			// 判据（两道都过才触发）：
			//   ① token 剩余寿命 ≤ 25%（上游自己也在"临期"才发新令牌，早调必然 400）
			//   ② 距上次尝试 ≥ 1 小时（失败也要退避，不能每 30 秒重试）
			Due: func(now time.Time) bool {
				// ★ T-382：这里不再有"默认不跑"的开关 —— 默认开启。
				// 该不该跑完全由下面两条护栏决定（宁可少跑，不可白跑）。
				p.refreshMu.Lock()
				last := p.lastRefreshTry
				p.refreshMu.Unlock()
				if !last.IsZero() && now.Sub(last) < time.Hour {
					return false
				}
				for _, cred := range p.loadAll() {
					a, err := authOf(cred)
					if err != nil {
						continue
					}
					if a.ExpiresAt <= 0 {
						a.ExpiresAt = jwtExpiry(a.Token)
					}
					if a.ExpiresAt <= 0 {
						continue
					}
					exp := time.Unix(a.ExpiresAt, 0)
					remaining := exp.Sub(now)
					if remaining <= 0 {
						return true // 已经过期：赶紧换
					}
					if lifetime := exp.Sub(jwtIssuedAt(a.Token)); lifetime > 0 && remaining <= lifetime/4 {
						return true // 剩余寿命 ≤ 25%
					}
					if remaining <= 24*time.Hour {
						return true // 兜底：不到一天了
					}
				}
				return false
			},
			Run: func(ctx context.Context) error {
				creds := p.loadAll()
				if len(creds) == 0 {
					return nil
				}
				var firstErr error
				for _, cred := range creds {
					a, err := authOf(cred)
					if err != nil {
						continue
					}
					if err := p.renewOne(ctx, a); err != nil && firstErr == nil {
						firstErr = err
					}
				}
				return firstErr
			},
		},
	}
}

// renewOne 调一次上游续期并**落盘新 token**。
//
// 下游 Re：先校验新令牌身份与 exp，再做**回验**（verifyToken），三关都过才落盘。
// ⚠ 旧注释里提到的"环境变量开关"已在 T-382 删除（默认开启）。
// 校验同 codar2api：新令牌必须非空、且 username 与账号一致，否则拒绝替换。
func (p *Provider) renewOne(ctx context.Context, a *Auth) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, status, err := p.callJSON(cctx, http.MethodPost, p.payBase+"/v1/token/refresh", a, nil)
	if err != nil {
		return err
	}
	// ★ 上游对未临期的 token **拒绝续期**（2026-11-08 实测两次一致）：
	//   POST /v1/token/refresh → 400 {"code":400,"message":"Token is not close to expiration"}
	// 这不是错误，是上游的护栏（也正说明早调不会轮换、不会踢掉桌面端会话）。
	// ⇒ 静默跳过，避免每 6 小时刷一条假错误。
	if status == http.StatusBadRequest && strings.Contains(string(raw), "not close to expiration") {
		return nil
	}
	if status != http.StatusOK {
		return fmt.Errorf("codar: 续期返回 HTTP %d（%s）", status, truncateStr(string(raw), 200))
	}
	var parsed struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	tok := parsed.Data.Token
	if tok == "" {
		tok = parsed.Token
	}
	if tok == "" {
		return fmt.Errorf("codar: 续期响应缺少新令牌")
	}
	if u, ok := jwtClaims(tok); ok {
		if name, _ := u["username"].(string); name != "" && a.Username != "" && name != a.Username {
			return fmt.Errorf("codar: 续期令牌身份不一致（%s != %s），拒绝替换", name, a.Username)
		}
	}
	// ★ T-382：**落盘之前**用新令牌打一次最便宜的只读端点，验证它真的能用。
	//
	// 为什么需要这一步：上游只在 token 临期时才发新令牌（见上面 400 的实测），
	// 所以"提前轮换踢掉桌面端"的风险不存在；但"新令牌是否真的可用"从未验证过。
	// 万一它形式合法而服务端不认，直接落盘就把用户从"快过期但能用"变成"完全不能用" ——
	// 续期的目的是保可用，绝不能反过来制造不可用。
	//
	// 选 /v1/promotion/info 的理由：只读、幂等、返回体小，且 invite 面板已经在用它，
	// 不额外引入新的上游依赖。用**独立 5s 超时**（不占用 30s 的续期超时）。
	if verr := p.verifyToken(cctx, a, tok); verr != nil {
		return verr
	}
	a.Token = tok
	if a.ExpiresAt = jwtExpiry(tok); a.ExpiresAt == 0 {
		return fmt.Errorf("codar: 新令牌无 exp，拒绝替换")
	}
	return nil
}

// verifyToken 用**新令牌**打一次只读端点，确认它真的被服务端接受。
//
// # 为什么"回验失败就不替换"是唯一正确的方向
//
// 两种失败的处理必须不对称：
//
//	· 不替换 ⇒ 用户继续用旧令牌（还在有效期内，能用）⇒ 最坏是"这次没续上"
//	· 替换   ⇒ 若新令牌不可用，用户当场从"能用"变"不能用" ⇒ 不可逆的伤害
//
// ⇒ 所以任何不确定（401/网络错误/超时/业务码非 200/空码）一律不替换。
func (p *Provider) verifyToken(ctx context.Context, a *Auth, tok string) error {
	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// 用**临时副本**：绝不能为了回验去改 a.Token（一旦失败，a 必须原样不动）
	tmp := *a
	tmp.Token = tok
	raw, status, err := p.callJSON(vctx, http.MethodPost, p.payBase+"/v1/promotion/info", &tmp,
		map[string]any{"username": tmp.UID, "token": 0})
	if err != nil {
		return fmt.Errorf("codar: 续期得到的新令牌回验失败（传输错误：%v），保持旧令牌不变", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("codar: 续期得到的新令牌回验失败（HTTP %d）：%s", status, truncateStr(string(raw), 200))
	}
	var out struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		return fmt.Errorf("codar: 续期得到的新令牌回验失败（响应解析失败：%v）：%s", jerr, truncateStr(string(raw), 200))
	}
	// ⚠ 上游业务码在 body 里、HTTP 恒 200（全仓统一教训）⇒ 必须看 code。
	if out.Code != 200 {
		return fmt.Errorf("codar: 续期得到的新令牌回验失败（业务码 %d：%s）", out.Code, out.Message)
	}
	return nil
}

// sortedModelIDs 便于日志/测试稳定输出（不参与业务）。
func sortedModelIDs(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dailyActionURLs 每日动作声明的 URL 列表（测试用：核对它们真的挂在 AdminRoutes 里）。
func (p *Provider) dailyActionURLs() []string {
	out := []string{}
	for _, a := range p.DailyActions() {
		out = append(out, a.OneURL, a.AllURL)
	}
	return out
}

// recordSign 把一次签到结果写进签到历史（checkinlog）。
//
// # 为什么要记（本仓的判据）
//
// 账号行的「今日签到」列读的就是 checkinlog ⇒ 不写它，用户看到的是"点了签到、列还是空"。
// 历史上有 4 个上游就栽在这（见 gateway/dailyaction_ext.go:194 的注释）。
//
// # 状态映射（★ 不能美化）
//
//	ok      → 真签到成功（上游 code==200 且不是"已签"）
//	already → 上游说 Already signed（**不是** ok：报成 ok 就是谎报"今天签到了"）
//	fail    → 上游非 200 / 网络错误
//	Trigger 由调用方传 —— 「全部签到」按钮传 manual，自动扫描传 schedule（T-424）。
//
// ⚠ 这一行曾经写死成 manual，理由是「本上游**没有**自动签到循环（单号 + 上游有风控），
//
//	全是用户点的」—— 那个假设**是错的**：用户明确要求自动签到，而且正是靠
//	trigger 字段发现「自动签到没工作」（schedule 记录 0 条）。假设写进注释前先验证。
func (p *Provider) recordSign(res gateway.DailyCheckinResult, trigger string) {
	if p.log == nil {
		return
	}
	rec := checkinlog.Record{
		At:      time.Now(),
		UID:     res.UID,
		Kind:    checkinlog.KindCheckin,
		Status:  res.Status,
		Detail:  res.Detail,
		Trigger: trigger,
	}
	if rec.Status == "" {
		rec.Status = checkinlog.StatusSkip
	}
	p.log.Append(rec)
}

// ⚠ T-465：这里原有第二个 record 函数（把"领活动模型"的结果写进历史，Kind=activity）。
// 它随整套功能一起删除 —— 删掉两条 handler 后它**零调用者**（grep 全仓确认），
// 留着就是死代码。它写的 KindActivity 仍被别的上游用，所以那个常量不能动。

// truncateStr 截断字符串用于日志（避免把上游整段 body 打出来）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// jwtIssuedAt 取 JWT 的 iat（没有就返回零值）。
func jwtIssuedAt(token string) time.Time {
	c, ok := jwtClaims(token)
	if !ok {
		return time.Time{}
	}
	if v, ok := c["iat"].(float64); ok && v > 0 {
		return time.Unix(int64(v), 0)
	}
	return time.Time{}
}

// isAlreadySignedMsg 判断签到返回的文案是否表示"今天已经签过了"。
//
// # 为什么不能精确匹配（T-351 实测）
//
//	英文：`Already signed`（T-317 探到）
//	中文：`24小时内仅可领取一次`（真跑遇到的）—— 而且它伴随 code != 200
//
// ⇒ 用**子串**判据（英文 key + 中文措辞），并且调用方必须在"code!=200 ⇒ fail"之前调用它。
func isAlreadySignedMsg(msg string) bool {
	m := lower(strings.TrimSpace(msg))
	for _, k := range []string{"already", "signed", "仅可领取一次", "已签到", "已领取", "24小时"} {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}
