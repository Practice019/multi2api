// adminpoints.go loomy 的两个**专属标签页**背后的端点：新手任务 与 邀请码。
//
// # 为什么它们住在 loomy 里，而不是账号池里
//
// 用户的要求原文：「不要去改账号池的 ui 了，账号池保持简单统一就行，
// 多的直接写 loomy 专属标签页里面去」。
//
// 架构上正好也是这么分的：
//
//	账号池（accountRow）    只画**每个上游都一样**的那几列：上游/昵称/UID/额度/状态/操作
//	上游专属标签页          承载"只有这个上游才有的整块功能"
//
// 通用面板的生成是**数据驱动**的（见 webui.html 的 buildProviderPanels）：
// 上游声明一个能力位 + 提供一条**非 hidden 的 GET 路由**，导航里就会出现
// 一个标签页，内容由该能力位对应的渲染器填。所以这里加两条 GET
// （tasks / invite）就够了 —— 前端**不认识 loomy 这个名字**。
//
// # 账号从哪来（本包不需要核心给）
//
// 凭证目录就是 `p.authDir`，格式是本包自己的 `loomy*.json`。
// 所以"遍历本上游所有账号"是自足的（见 authByUID 的同款理由）。
// 这正是"加新上游核心零改动"那条判据在**管理端点**上的体现。
package loomy

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
)

// 本轮新增的路径。
const (
	tasksPath      = "/admin/loomy/tasks"
	tasksAutoPath  = "/admin/loomy/tasks/auto"
	tasksAllPath   = "/admin/loomy/tasks/auto-all"
	invitePath     = "/admin/loomy/invite"
	inviteBindPath = "/admin/loomy/invite/bind"
)

// accountRef 一个 loomy 账号在管理端点里的投影。
type accountRef struct {
	UID      string
	Nickname string
	Session  string
}

// accounts 列出本上游凭证目录里的全部账号。
//
// 读不到就是空列表（**不是错误**）：面板显示"还没有账号"比报 500 有用。
func (p *Provider) accounts() []accountRef {
	list, err := LoadDir(p.authDir)
	if err != nil {
		return nil
	}
	out := make([]accountRef, 0, len(list))
	for _, a := range list {
		if a.UID == "" || a.Session == "" {
			continue
		}
		out = append(out, accountRef{UID: a.UID, Nickname: a.Nickname, Session: a.Session})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// ─ 新手任务 ───────────────────────────────────────────────────────────

// accountTasksView 一个账号的任务状态。
type accountTasksView struct {
	UID      string          `json:"uid"`
	Nickname string          `json:"nickname"`
	OK       bool            `json:"ok"`
	Tasks    map[string]bool `json:"tasks,omitempty"`
	Done     int             `json:"done"`
	Total    int             `json:"total"`
	Earned   int64           `json:"earned"`
	Max      int64           `json:"max"`
	Balance  int64           `json:"balance"`
	Pending  []string        `json:"pending,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// handleTasksList GET /admin/loomy/tasks —— 每个账号的任务进度。
func (p *Provider) handleTasksList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := pointsCtx()
	defer cancel()

	accts := p.accounts()
	out := make([]accountTasksView, 0, len(accts))
	var totalEarned, totalMax int64
	for _, a := range accts {
		v := accountTasksView{UID: a.UID, Nickname: maskDisplay(a.Nickname),
			Total: len(TaskDefs), Max: taskMaxPoints()}
		tasks, err := p.Tasks(ctx, a.Session)
		if err != nil {
			v.Error = err.Error()
			out = append(out, v)
			continue
		}
		v.OK = true
		v.Tasks = tasks
		for _, d := range TaskDefs {
			if tasks[d.Key] {
				v.Done++
				v.Earned += d.Points
			} else {
				v.Pending = append(v.Pending, d.Key)
			}
		}
		if bal, _, berr := p.Balance(ctx, a.Session); berr == nil {
			v.Balance = bal
		}
		totalEarned += v.Earned
		totalMax += v.Max
		out = append(out, v)
	}
	writeLoomyJSON(w, http.StatusOK, map[string]any{
		"defs":     TaskDefs,
		"accounts": out,
		"summary": map[string]any{
			"accounts": len(out),
			"earned":   totalEarned,
			"max":      totalMax,
			"pending":  countPending(out),
		},
	})
}

// taskMaxPoints 全部任务的总分。
func taskMaxPoints() int64 {
	var n int64
	for _, d := range TaskDefs {
		n += d.Points
	}
	return n
}

// countPending 还有多少个未完成任务（合计）。
func countPending(list []accountTasksView) int {
	n := 0
	for _, v := range list {
		n += len(v.Pending)
	}
	return n
}

// taskRunResult 一次"一键完成"的结果。
type taskRunResult struct {
	UID       string   `json:"uid"`
	Nickname  string   `json:"nickname,omitempty"`
	Completed []string `json:"completed,omitempty"`
	Already   []string `json:"already,omitempty"`
	Failed    []string `json:"failed,omitempty"`
	Balance   int64    `json:"balance"`
	Error     string   `json:"error,omitempty"`
}

// runTasks 对一份凭证把还没完成的任务逐个点掉。
//
// # 为什么逐个 POST 而不是"一条全量端点"
//
// 上游只有 `POST /onboarding/tasks/complete {key}` 这一个粒度。
// 所以"一键完成"在**我们**这侧就是循环 —— 这也是它必须显示进度的原因
// （8 个任务 = 8 次往返，上游慢的时候会等几秒）。
//
// 已完成的会产生 `alreadyCompleted=true`，那不是失败（幂等），单列一栏。
func (p *Provider) runTasks(ctx context.Context, a accountRef) taskRunResult {
	res := taskRunResult{UID: a.UID, Nickname: maskDisplay(a.Nickname)}

	tasks, err := p.Tasks(ctx, a.Session)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	done := make(map[string]bool, len(tasks))
	for k, v := range tasks {
		done[k] = v
	}
	for _, d := range TaskDefs {
		if done[d.Key] {
			res.Already = append(res.Already, d.Key)
			continue
		}
		already, bal, cerr := p.CompleteTask(ctx, a.Session, d.Key)
		if cerr != nil {
			res.Failed = append(res.Failed, d.Key+": "+cerr.Error())
			continue
		}
		if already {
			// 上游说这次是重复点（并发/上一轮已经入账）——照记 already。
			res.Already = append(res.Already, d.Key)
		} else {
			res.Completed = append(res.Completed, d.Key)
		}
		// 余额取**最后一次**成功回执里的值（它每次都会带回最新余额）。
		if bal > 0 {
			res.Balance = bal
		}
	}
	if res.Balance == 0 {
		if bal, _, berr := p.Balance(ctx, a.Session); berr == nil {
			res.Balance = bal
		}
	}
	return res
}

// handleTasksAuto POST /admin/loomy/tasks/auto —— 一键完成**一个**账号。
func (p *Provider) handleTasksAuto(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := strings.TrimSpace(req.UID)
	if uid == "" {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 uid"})
		return
	}
	var target *accountRef
	for _, a := range p.accounts() {
		if a.UID == uid {
			cp := a
			target = &cp
			break
		}
	}
	if target == nil {
		writeLoomyJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有这个 loomy 账号: " + uid})
		return
	}
	ctx, cancel := pointsCtx()
	defer cancel()
	res := p.runTasks(ctx, *target)
	writeLoomyJSON(w, http.StatusOK, map[string]any{"ok": res.Error == "", "result": res})
}

// handleTasksAutoAll POST /admin/loomy/tasks/auto-all —— 一键完成**全部** loomy 账号。
func (p *Provider) handleTasksAutoAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := pointsCtx()
	defer cancel()
	accts := p.accounts()
	results := make([]taskRunResult, 0, len(accts))
	var completed, failed int
	for _, a := range accts {
		res := p.runTasks(ctx, a)
		completed += len(res.Completed)
		failed += len(res.Failed)
		results = append(results, res)
	}
	writeLoomyJSON(w, http.StatusOK, map[string]any{
		"ok":        failed == 0,
		"results":   results,
		"accounts":  len(accts),
		"completed": completed,
		"failed":    failed,
	})
}

// ── 邀请码 ──────────────────────────────────────────────────────────────

// accountInviteView 一个账号的邀请码状态。
type accountInviteView struct {
	UID       string       `json:"uid"`
	Nickname  string       `json:"nickname"`
	OK        bool         `json:"ok"`
	Activated bool         `json:"activated"`
	Applied   string       `json:"applied,omitempty"`
	Balance   int64        `json:"balance"`
	Codes     []InviteCode `json:"codes,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// handleInviteList GET /admin/loomy/invite —— 每个账号的激活状态 + 自己生成的码。
func (p *Provider) handleInviteList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := pointsCtx()
	defer cancel()

	accts := p.accounts()
	out := make([]accountInviteView, 0, len(accts))
	activatedN := 0
	for _, a := range accts {
		v := accountInviteView{UID: a.UID, Nickname: maskDisplay(a.Nickname)}
		act, applied, err := p.Activation(ctx, a.Session)
		if err != nil {
			v.Error = err.Error()
			out = append(out, v)
			continue
		}
		v.OK = true
		v.Activated = act
		v.Applied = applied
		if act {
			activatedN++
		}
		// 自己的码是**附加信息**：查不到不影响激活状态那一行（所以错误只丢进 Error）。
		codes, cerr := p.InvitationCodes(ctx, a.Session)
		if cerr == nil && len(codes) == 0 {
			// ⚠ 上游没给码但查询是成功的 → 大概率是"从没做过首登初始化"
			//（客户端每次登录后会自动调 first-login，而导入/添加的号跳过了这步）。
			// 实测：初始化后 invitation-codes 立刻返回 5 个码、并补发注册奖励。
			// 服务端有 alreadyProcessed 幂等保护，重复调用不会重复发奖 ——
			// 所以这里**自动补一次**，让"我生成的码"不用用户手动点任何东西就出现。
			if ferr := p.FirstLogin(ctx, a.Session); ferr == nil {
				codes, cerr = p.InvitationCodes(ctx, a.Session)
			} else {
				cerr = ferr
			}
		}
		if cerr == nil {
			v.Codes = codes
		} else if v.Error == "" {
			v.Error = "邀请码列表查询失败：" + cerr.Error()
		}
		if bal, _, berr := p.Balance(ctx, a.Session); berr == nil {
			v.Balance = bal
		}
		out = append(out, v)
	}
	writeLoomyJSON(w, http.StatusOK, map[string]any{
		"accounts": out,
		"summary": map[string]any{
			"accounts":  len(out),
			"activated": activatedN,
			"pending":   len(out) - activatedN,
		},
	})
}

// handleInviteBind POST /admin/loomy/invite/bind —— 给某个账号绑定邀请码。
//
// 回执带**绑定后重新查询**的激活状态：绑定是一次性的（码 maxUses=1），
// 前端据此立刻把那一行刷成"已激活"，不必等下一次列表刷新。
func (p *Provider) handleInviteBind(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID  string `json:"uid"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	uid := strings.TrimSpace(req.UID)
	code := strings.TrimSpace(req.Code)
	// ⚠ 全程日志（用户报"绑定失败：未知原因，且没有日志"）。
	// 每个分支都落一条，从"请求到达"起 —— 若复现后连到达行都没有，
	// 说明请求根本没进这个 handler（前端路径/页面缓存问题），那本身就是答案。
	log.Printf("loomy: 绑定邀请码请求到达 uid=%s code=%s", uid, code)
	if uid == "" || code == "" {
		log.Printf("loomy: 绑定邀请码被拒（uid/code 为空）uid=%q code=%q", uid, code)
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "uid 与 code 都不能为空"})
		return
	}
	if len(code) < 4 || len(code) > 32 {
		// 实测邀请码 6 位，但不设死 —— 上游才是权威，长度怪异的码交给上游
		// 判定（200003 邀请码不可用），而不是在这里因为"不是 6 位"就白拒。
		log.Printf("loomy: 绑定邀请码被拒（长度异常）uid=%s code=%s len=%d", uid, code, len(code))
		writeLoomyJSON(w, http.StatusBadRequest,
			map[string]any{"ok": false, "error": "邀请码长度异常（应为 6 位，实测形如 E3HRN8）"})
		return
	}
	var target *accountRef
	for _, a := range p.accounts() {
		if a.UID == uid {
			cp := a
			target = &cp
			break
		}
	}
	if target == nil {
		log.Printf("loomy: 绑定邀请码被拒（账号不存在）uid=%s code=%s", uid, code)
		writeLoomyJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有这个 loomy 账号: " + uid})
		return
	}
	ctx, cancel := pointsCtx()
	defer cancel()

	if err := p.BindInvite(ctx, target.Session, code); err != nil {
		// 上游错误码原文很晦涩（实测：自绑=100001"请求参数错误"、已用/失效码=200003
		// "邀请码不可用"），用户根本看不出"码为什么不行"。这里翻译成人话，
		// 并把**原文**落日志 —— 排查时能看真实错误码，界面上不甩晦涩报错。
		translated := translateBindError(err)
		log.Printf("loomy: 绑定邀请码失败 uid=%s code=%s 原文=%v", shortUID(uid), code, err)
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": translated})
		return
	}
	log.Printf("loomy: 绑定邀请码上游成功 uid=%s code=%s（开始复查激活状态）", shortUID(uid), code)
	act, applied, err := p.Activation(ctx, target.Session)
	bal, _, _ := p.Balance(ctx, target.Session)
	resp := map[string]any{
		"ok":        err == nil,
		"uid":       uid,
		"activated": act,
		"applied":   applied,
		"balance":   bal,
	}
	if err != nil {
		resp["error"] = "绑定已提交，但复查激活状态失败：" + err.Error()
		log.Printf("loomy: 绑定已提交但复查失败 uid=%s code=%s 复查错误=%v", shortUID(uid), code, err)
	}
	log.Printf("loomy: 绑定邀请码结果 uid=%s code=%s ok=%v activated=%v applied=%q balance=%d",
		shortUID(uid), code, resp["ok"], act, applied, bal)
	writeLoomyJSON(w, http.StatusOK, resp)
}

// inviteCodeLen 实测邀请码长度（见 handleInviteBind）。
const inviteCodeLen = 6

// translateBindError 把绑定邀请码的上游错误翻译成人话。
//
// 实测错误码（2026-09-15）：
//
//	100001  请求参数错误  —— 自绑（拿自己账号生成的码绑自己）时返回
//	200002  邀请码不存在  —— 抄错/多复制字符/码格式不对
//	200003  邀请码不可用  —— 已用（maxUses=1 被消费过）/ 失效
//
// 三个原文都让人看不出"码为什么不行"，用户会以为是 bug。这里按码翻译，
// 未命中时保留原文并补一句通用提示（也不让界面光秃秃一行晦涩报错）。
func translateBindError(err error) string {
	if err == nil {
		return "未知错误"
	}
	msg := err.Error()
	hint := "（请确认用的是**其他账号**「我生成的码」里 active 的码、且该码还没被用过）"
	switch {
	case strings.Contains(msg, "100001"):
		return "不能绑定自己账号生成的邀请码 —— " + hint
	case strings.Contains(msg, "200002"):
		return "邀请码不存在：可能抄错了字符或多复制了内容 —— " + hint
	case strings.Contains(msg, "200003"):
		return "邀请码不可用：已被使用或已失效 —— " + hint
	default:
		return msg + " —— " + hint
	}
}
