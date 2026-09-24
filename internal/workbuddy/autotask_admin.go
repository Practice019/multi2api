// autotask_admin.go 任务自动化的管理端点（gateway.AdminExt 路由）。
//
// # 移植说明
//
// 对应 B 的 panel 层 HTTP handler（`accountTaskAuto` / `accountTaskAutoAll` /
// `tasksScanAll` / `tasksRunQueue` / `schoolStatus` / `schoolRunAll`），
// 但**挂载方式**按本仓库的扩展点体系改写：上游经 AdminExt 自报路由，
// 核心只负责遍历挂载（见 gateway/extension.go）。
//
// 与 B 的差异只在 HTTP 外壳：请求/响应字段名、业务语义、错误分类一一对应。
package workbuddy

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// ── 路由挂载 ────────────────────────────────────────────────────────────

// autoTaskRoutes 返回任务自动化相关的管理端点。
//
// 由 admin.go 的 Routes() 追加（不直接改那个文件，便于对照 B 的移植范围）。
// 路径同样经 prefixed 按实例 ID 加前缀（见 admin.go 的 pathPrefix）。
func (h *AdminHandler) autoTaskRoutes() []gateway.AdminRoute {
	return h.prefixed([]gateway.AdminRoute{
		// 可自动化任务清单（前端渲染"一键完成"按钮用）。
		{Method: "GET", Path: "/admin/growth/auto/actions", Handler: h.AutoActions, Title: "可自动化任务"},
		// 单任务一键完成。
		{Method: "POST", Path: "/admin/growth/auto", Handler: h.AutoOne, Title: "一键完成单个任务"},
		// 全量一键完成（单账号或全池）。
		{Method: "POST", Path: "/admin/growth/auto-all", Handler: h.AutoAll, Title: "一键完成全部可自动任务"},
		// 任务中心：全账号扫描待办。
		{Method: "GET", Path: "/admin/growth/scan", Handler: h.TasksScanAll, Title: "待办扫描"},
		// 开学季。
		{Method: "GET", Path: "/admin/school", Handler: h.SchoolStatus, Title: "开学季"},
		{Method: "POST", Path: "/admin/school/run", Handler: h.SchoolRun, Title: "开学季一键完成"},
		// 夜猫子。
		{Method: "POST", Path: "/admin/blackcat/run", Handler: h.BlackcatRun, Title: "夜猫子补足"},
	})
}

// ── 可自动化任务清单 ────────────────────────────────────────────────────

// AutoActions GET /admin/growth/auto/actions —— 返回动作表。
//
// 前端据此渲染每个任务行的「一键完成」按钮：表里有的才给按钮，
// 没给的用户点下去会得到 501（见 AutoOne 的 errTaskNotAuto 分支）。
func (h *AdminHandler) AutoActions(w http.ResponseWriter, r *http.Request) {
	type item struct {
		TaskCode string `json:"task_code"`
		Desc     string `json:"desc"`
		Attempt  bool   `json:"attempt"`
	}
	out := make([]item, 0, len(autoActions))
	for _, a := range autoActions {
		out = append(out, item{TaskCode: a.TaskCode, Desc: a.Desc, Attempt: a.Attempt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": out, "count": len(out)})
}

// ── 单任务一键完成 ──────────────────────────────────────────────────────

// AutoOne POST /admin/growth/auto —— body {"uid":"...","task_code":"..."}。
//
// 与 B 的 accountTaskAuto 一一对应，状态码映射：
//
//	400 缺 task_code
//	404 该账号没有此任务
//	409 该账号已有任务动作在执行
//	501 该任务无自动化实现（需官方客户端内交互）
//	502 执行失败（上游错误）
//	200 成功（body 形状与 B 完全一致）
func (h *AdminHandler) AutoOne(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	var body struct {
		UID      string `json:"uid"`
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeError(w, http.StatusBadRequest, "task_code required")
		return
	}
	uid := body.UID
	if uid == "" {
		writeError(w, http.StatusBadRequest, "uid required（单任务一键完成必须指定账号）")
		return
	}
	resp, err := h.p.AutoTaskOne(uid, body.TaskCode)
	if err != nil {
		writeError(w, autoTaskStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// autoTaskStatus 把业务错误映射成 HTTP 状态码（与 B 的映射一致）。
func autoTaskStatus(err error) int {
	switch err {
	case errTaskNotAuto:
		return http.StatusNotImplemented
	case errTaskBusy:
		return http.StatusConflict
	case errTaskNotFound:
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

// ── 全量一键完成 ────────────────────────────────────────────────────────

// AutoAll POST /admin/growth/auto-all —— body {"uid":"..."}（省略则全池）。
//
// # 为什么是后台任务而不是同步返回
//
// 单账号跑完 17 项实测需要数分钟（expert 系每项含一次真实对话 + 6s 间隔）。
// 同步返回会让浏览器/反代超时，而超时后**流水线仍在跑** ——
// 用户再点一次就会看到 409（互斥锁还在），这是正确的，但体验上像卡死。
//
// 因此这里复用本仓库既有的共享任务槽（h.task）：立即返回 202，
// 前端轮询 /admin/task 查进度。B 的做法是 5 分钟 context 兜底 + 超时后
// 后台继续，本仓库的槽位机制更贴合既有控制台。
func (h *AdminHandler) AutoAll(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	var body struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.UID != "" {
		uid := body.UID
		if !h.task.Start("growth-auto-all", func() []map[string]any {
			res, err := h.p.AutoTaskAll(uid)
			if err != nil {
				return []map[string]any{{"uid": uid, "status": statusFail, "message": err.Error()}}
			}
			return []map[string]any{{"uid": uid, "status": statusOK, "results": res}}
		}) {
			writeError(w, http.StatusConflict, errTaskBusy.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"mode": "one", "uid": uid, "started": true})
		return
	}

	// 全池：**受限并发**（账号内已互斥，账号间并发但受共享预算夹住）。
	//
	// # 为什么从串行改成并发（用户要求）
	//
	// 串行时 4 个账号约 60s（单账号实测 ~15s：专家链 8 次 × 6s 节流 +
	// 事件上报 1.05s 间隔），前端「一键完成」要等第一步全跑完才启动第二步，
	// 于是按钮被禁用近一分钟，用户读成卡死。
	//
	// # 为什么不是"无限制并发"
	//
	// 上游是同一个腾讯服务，多账号同时打过去有触发风控的风险 ——
	// 这正是原先串行注释担心的事（"账号间串行是为了不给上游造成并发风控画像"），
	// 也是 growth.go 里 GrowthProbeConcurrency 存在的理由
	//（"账号多时同时打过去有触发风控的风险；旅行模块已有账号间间隔 800ms 的先例"）。
	//
	// 所以这里**沿用同一套共享预算**（p.probeSem），而不是新造一个上限：
	// 它约束的是「同时在途的账号数」，账号内部的串行节奏（reportGap /
	// expertSummonGap）一个都不动 —— 那些节流是过风控的关键，调小才是真风险。
	//
	// 并发安全性：AutoTaskAll 有 per-account 互斥（tryLockAccount），
	// 不同账号之间不共享可变状态；结果按下标写回 out，无需加锁。
	accts := h.p.ownAccounts()
	if !h.task.Start("growth-auto-all", func() []map[string]any {
		out := make([]map[string]any, len(accts))
		var wg sync.WaitGroup
		for i, st := range accts {
			if st.Disabled {
				out[i] = map[string]any{"uid": st.UID, "status": "skipped", "message": "已禁用"}
				continue
			}
			wg.Add(1)
			go func(i int, uid string) {
				defer wg.Done()
				// 共享预算：同时在途的账号数受 GrowthProbeConcurrency 夹住
				h.p.probeSem.acquire()
				defer h.p.probeSem.release()

				res, err := h.p.AutoTaskAll(uid)
				if err != nil {
					out[i] = map[string]any{"uid": uid, "status": statusFail, "message": err.Error()}
					return
				}
				out[i] = map[string]any{"uid": uid, "status": statusOK, "results": res}
			}(i, st.UID)
		}
		wg.Wait()
		return out
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "accounts": len(accts), "started": true})
}

// ── 待办扫描（任务中心的"看"这一半）────────────────────────────────────

// TasksScanAll GET /admin/growth/scan —— 全账号扫描未完成且可自动化的任务。
//
// 对应 B 的 tasksScanAll。返回结构刻意保持"每账号一项 + 该账号的待办列表"，
// 便于前端直接渲染成可勾选队列。
func (h *AdminHandler) TasksScanAll(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	type pendingItem struct {
		TaskCode string `json:"task_code"`
		Desc     string `json:"desc"`
		Progress string `json:"progress"`
	}
	type accountScan struct {
		UID     string        `json:"uid"`
		Pending []pendingItem `json:"pending"`
		School  int           `json:"school_pending"`
		ScanErr string        `json:"scan_error,omitempty"`
		Busy    bool          `json:"busy"`
	}
	out := make([]accountScan, 0)
	for _, st := range h.p.ownAccounts() {
		if st.Disabled {
			continue
		}
		item := accountScan{UID: st.UID, Busy: h.p.TaskLockHeld(st.UID)}
		a := h.p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		tasks, err := h.p.growthTasks(a)
		if err != nil {
			item.ScanErr = err.Error()
			out = append(out, item)
			continue
		}
		byCode := map[string]*upstream.GrowthTask{}
		for i := range tasks {
			byCode[tasks[i].TaskCode] = &tasks[i]
		}
		for _, act := range autoActions {
			t := byCode[act.TaskCode]
			if t == nil {
				continue // 该账号没有这个任务
			}
			if t.Claimed() || (t.Target() > 0 && t.Current() >= t.Target()) {
				continue // 已完成
			}
			item.Pending = append(item.Pending, pendingItem{
				TaskCode: act.TaskCode, Desc: act.Desc, Progress: taskProgressText(t),
			})
		}
		// 开学季待办数（活动不在期时接口会报错，忽略即可）。
		if sts, inPeriod, serr := h.p.client.SchoolTasks(a); serr == nil && inPeriod {
			for _, t := range sts {
				if t.Status != "claimed" {
					item.School++
				}
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "count": len(out)})
}

// ── 开学季 ──────────────────────────────────────────────────────────────

// SchoolStatus GET /admin/school —— 每账号的开学季任务矩阵 + 剩余抽奖次数。
func (h *AdminHandler) SchoolStatus(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	type acctSchool struct {
		UID      string                `json:"uid"`
		InPeriod bool                  `json:"in_period"`
		Tasks    []upstream.SchoolTask `json:"tasks"`
		Chances  int                   `json:"chances"`
		Err      string                `json:"error,omitempty"`
	}
	out := make([]acctSchool, 0)
	for _, st := range h.p.ownAccounts() {
		if st.Disabled {
			continue
		}
		a := h.p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		item := acctSchool{UID: st.UID}
		tasks, inPeriod, err := h.p.client.SchoolTasks(a)
		if err != nil {
			item.Err = err.Error()
			out = append(out, item)
			continue
		}
		item.Tasks, item.InPeriod = tasks, inPeriod
		if inPeriod {
			if ch, cerr := h.p.client.SchoolChances(a); cerr == nil {
				item.Chances = ch
			}
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// SchoolRun POST /admin/school/run —— body {"uid":"..."}（省略则全池）。
func (h *AdminHandler) SchoolRun(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	var body struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	targets := []*auth.Auth{}
	if body.UID != "" {
		a := h.p.cfg.Pool.AuthByUID(body.UID)
		if a == nil {
			writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
			return
		}
		targets = append(targets, a)
	} else {
		for _, st := range h.p.ownAccounts() {
			if st.Disabled {
				continue
			}
			if a := h.p.cfg.Pool.AuthByUID(st.UID); a != nil {
				targets = append(targets, a)
			}
		}
	}

	if !h.task.Start("school-run", func() []map[string]any {
		out := make([]map[string]any, 0, len(targets))
		for _, a := range targets {
			steps := h.p.runSchoolFor(a)
			out = append(out, map[string]any{"uid": a.UID, "status": schoolRowStatus(steps), "result": steps})
			time.Sleep(reportGap)
		}
		return out
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "accounts": len(targets)})
}

// schoolRowStatus 把单账号的开学季步骤结果折算成**任务槽词汇表**里的一个状态。
//
// # 为什么必须填这个字段
//
// `/admin/task` 的完成摘要是按行统计 status 的（taskslot.sumStatus 只认
// ok / fail / already）。聚合行不填 status 的后果是**摘要恒报"成功 0"** ——
// 实测 2026-09-14：开学季真跑完（分享 + 3 次领奖 + 3 次抽奖得 +78c）之后，
// 前端进度条显示"完成：成功 0 · 已签到 0 · 失败 0"，看起来像什么都没做。
//
// 这不是显示问题而是**判据缺口**：摘要的来源是槽，槽认的字只有这三个，
// 所以聚合行必须讲槽的语言 —— 不能各写各的（blackcat 用 ok/partial、
// AutoAll 原用 done/skipped/error、SchoolRun 原来什么都不填，三种词汇并存）。
//
// 判据取"任一步骤 error 即 fail"：开学季的步骤是独立可跳过的（已 claimed 的直接
// 不跑），全部没报错就算成功；有 error 的行必须计数，否则真实故障会被摘要吃掉。
func schoolRowStatus(steps []map[string]any) string {
	for _, s := range steps {
		if st, _ := s["status"].(string); st == "error" {
			return statusFail
		}
	}
	return statusOK
}

// runSchoolFor 对单账号跑一遍开学季闭环（幂等：已 claimed 的跳过）。
//
// # 步骤（对齐 B 的 scheduler.schoolAccount）
//
//	① 分享任务：share-complete 直调即点亮 → 领奖
//	② chat_3_times：viewed 激活 → 3 条 chat_request_send 埋点 → 领奖
//	③ expert_use：viewed 激活 → mp 专家事件链 → 领奖
//	④ 抽奖：把剩余次数抽完
//
// 学生认证（需微信真实认证）与桌面端体验（需真实客户端）不在此列。
func (p *Provider) runSchoolFor(a *auth.Auth) []map[string]any {
	out := []map[string]any{}
	tasks, inPeriod, err := p.client.SchoolTasks(a)
	if err != nil {
		return append(out, map[string]any{"step": "list", "status": "error", "message": err.Error()})
	}
	if !inPeriod {
		return append(out, map[string]any{"step": "list", "status": "skip", "message": "活动不在期"})
	}
	status := map[string]string{}
	for _, t := range tasks {
		status[t.TaskCode] = t.Status
	}

	// ① 分享
	if status["share_invite"] != "claimed" {
		if err := p.client.SchoolShareComplete(a); err != nil {
			out = append(out, map[string]any{"step": "share", "status": "error", "message": err.Error()})
		} else {
			out = append(out, map[string]any{"step": "share", "status": "ok"})
		}
	}

	// ② chat_3_times：先 viewed 激活，再补 3 条埋点（各带独立 conversationId）。
	if status["chat_3_times"] != "claimed" {
		if err := p.client.SchoolTaskViewed(a, "chat_3_times"); err != nil {
			log.Printf("school %s: viewed chat_3_times: %v", a.UID, err)
		}
		for i := 0; i < 3; i++ {
			cid := "wb2api-school-chat-" + upstream.NewReportConversationID()
			if err := p.client.ReportMPEvent(a, upstream.SchoolChatTimesEvents(cid)); err != nil {
				out = append(out, map[string]any{"step": "chat_3_times", "status": "error", "message": err.Error()})
				break
			}
			time.Sleep(reportGap)
		}
	}

	// ③ expert_use：viewed 激活 + mp 专家事件链。
	if status["expert_use"] != "claimed" {
		if err := p.client.SchoolTaskViewed(a, "expert_use"); err != nil {
			log.Printf("school %s: viewed expert_use: %v", a.UID, err)
		}
		cid := "wb2api-school-exp-" + upstream.NewReportConversationID()
		events := upstream.SchoolExpertUseEvents("ex_jB0dyFIQJEWa", "论文写作导师", cid)
		if err := p.client.ReportMPEvent(a, events...); err != nil {
			out = append(out, map[string]any{"step": "expert_use", "status": "error", "message": err.Error()})
		} else {
			out = append(out, map[string]any{"step": "expert_use", "status": "ok"})
		}
	}

	// ④ 领奖 + 抽奖（回读状态，达标即领）。
	if chance, err := p.client.SchoolClaimTask(a, "share_invite"); err == nil {
		out = append(out, map[string]any{"step": "claim share", "status": "ok", "chances": chance})
	}
	for _, code := range []string{"chat_3_times", "expert_use"} {
		if chance, err := p.client.SchoolClaimTask(a, code); err == nil && chance > 0 {
			out = append(out, map[string]any{"step": "claim " + code, "status": "ok", "chances": chance})
		}
	}
	if chances, err := p.client.SchoolChances(a); err == nil {
		for i := 0; i < chances; i++ {
			prize, derr := p.client.SchoolDraw(a)
			if derr != nil {
				out = append(out, map[string]any{"step": "draw", "status": "error", "message": derr.Error()})
				break
			}
			out = append(out, map[string]any{"step": "draw", "status": "ok", "prize": prize})
		}
	}
	p.record(a.UID, checkinlog.KindGrowth, checkinlog.StatusOK, "开学季闭环", 0, triggerManual)
	return out
}

// ── 夜猫子 ──────────────────────────────────────────────────────────────

// BlackcatRun POST /admin/blackcat/run —— 手动触发夜猫子补足。
//
// 窗口外会被 RunNightChats 的调用方（runBlackCat 逻辑）拒绝；
// 这里直接走 BlackcatNeed + RunNightChats，并在窗口外明确拒绝。
func (h *AdminHandler) BlackcatRun(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	var body struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.UID == "" {
		writeError(w, http.StatusBadRequest, "uid required（夜猫子补足必须指定账号）")
		return
	}
	a := h.p.cfg.Pool.AuthByUID(body.UID)
	if a == nil {
		writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
		return
	}
	if !upstream.InNightWindow(time.Now()) {
		writeError(w, http.StatusConflict,
			"当前不在 23:00–08:00 计数窗口，行为不计分；请等每日 23 点的排程")
		return
	}
	if !h.task.Start("blackcat-run", func() []map[string]any {
		return []map[string]any{h.p.runBlackcatFor(a)}
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "uid": body.UID})
}

// runBlackcatFor 单账号夜猫子补足（窗口外返回 skip，不报错）。
//
// # status 用任务槽的词汇表
//
// 本函数的返回值会**直接**作为任务槽的一行（见上面 BlackcatRun 的 h.task.Start），
// 所以 status 必须是槽认的字（ok / fail / already）—— 否则 /admin/task 的完成
// 摘要会数不出来（实测踩过：写成 done/error/partial 时摘要恒报成功 0）。
// "skip" 只在**窗口外**这一个情形保留：那种情况什么都没做，
// 既不是成功也不是失败，硬算成任何一边都是谎报（且 handler 已先行 409 拦掉，
// 这里是每日 23 点排程路径的防御）。
func (p *Provider) runBlackcatFor(a *auth.Auth) map[string]any {
	if !upstream.InNightWindow(time.Now()) {
		return map[string]any{"uid": a.UID, "status": "skip", "message": "不在 23:00–08:00 窗口"}
	}
	need, err := p.client.BlackcatNeed(a)
	if err != nil {
		return map[string]any{"uid": a.UID, "status": statusFail, "message": err.Error()}
	}
	if need <= 0 {
		// "already" = 无需动作（进度已达标），与签到"今天已签到"同一语义。
		return map[string]any{"uid": a.UID, "status": "already", "message": "进度已达标"}
	}
	ok, err := p.client.RunNightChats(a, int(need))
	if err != nil {
		// 部分成功也算 fail："补了 ok 条之后失败"必须计入失败数，
		// 否则真实故障会被摘要吞掉（done/need 保留细节供排查）。
		return map[string]any{"uid": a.UID, "status": statusFail, "done": ok, "need": need, "message": err.Error()}
	}
	p.record(a.UID, checkinlog.KindGrowth, checkinlog.StatusOK, "夜猫子补足", 0, triggerManual)
	return map[string]any{"uid": a.UID, "status": statusOK, "done": ok}
}
