// adminendpoints.go workbuddy 22 个管理端点的 HTTP 实现。
//
// # 搬运说明（Task 3c）
//
// 本文件的内容原先在 internal/admin/admin.go（checkin/keepalive/creditsRefresh/
// travel*/schedule/history/taskStatus）、internal/admin/growth.go 与
// internal/admin/clientlogin.go。**判定逻辑、状态码、错误文案逐字保留** ——
// 这是"搬运没有改行为"的唯一证据。
//
// 唯一的结构性变化：原先这些 handler 读 h.cfg.Pool / h.cfg.Scheduler /
// h.cfg.Business / h.cfg.Upstream 这些宿主字段，现在读 p（Provider）自己的字段。
// 两者指向的是同一批对象（账号池适配器、上游客户端、调度器视图），
// 所以外部观察不到差别。
package workbuddy

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
)

// writeJSON 写一个 JSON 响应（与改造前 admin.writeJSON 逐字一致）。
//
// 序列化失败时写空 body 而不是 500：改造前就是 `raw, _ := json.Marshal`，
// 改变这个行为会改变"Marshal 失败时对端看到什么"。
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeError 写一个 {"error": "..."} 响应（与改造前 admin.writeError 逐字一致）。
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// ReqBody 通用请求体：uid 用于单账号定向，其余字段各接口自取。
type ReqBody struct {
	UID        string `json:"uid"`
	LocationID int    `json:"location_id"`
	RecordID   int64  `json:"record_id"`
}

func decodeBody(r *http.Request) ReqBody {
	var b ReqBody
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&b)
	}
	return b
}

// resultView 全量任务槽里一条结果的 JSON 形状（与改造前 scheduler.CheckinResult 一致）。
type resultView struct {
	UID     string `json:"uid"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Credits int64  `json:"credits"`
}

func (v resultView) map_() map[string]any {
	return map[string]any{"uid": v.UID, "status": v.Status, "detail": v.Detail, "credits": v.Credits}
}

// statusOK 与 checkinlog.StatusOK 同值。
//
// 刻意不直接引用 checkinlog.StatusOK 的地方：任务槽读的是中转后的 JSON 形状，
// 这里集中定义一次，避免各处散落。
const statusOK = checkinlog.StatusOK

// ---------------------------------------------------------------------------
// 账号池 / 凭证
// ---------------------------------------------------------------------------

// accountList 账号池的账号列表（池未接线时为空）。
func (h *AdminHandler) accountList() []Account {
	if h.p == nil || h.p.cfg.Pool == nil {
		return nil
	}
	return h.p.cfg.Pool.List()
}

// authOf 取账号凭证（池未接线时返回 nil）。
func (h *AdminHandler) authOf(uid string) *auth.Auth {
	if h.p == nil || h.p.cfg.Pool == nil {
		return nil
	}
	return h.p.cfg.Pool.AuthByUID(uid)
}

// record 统一写一条手动任务历史。
// 关键：Nickname 必须由这里补上——直接把 Record 丢给 Log.Append 会漏掉昵称，
// 历史表里就会退化成显示 uid 前缀（同一张表出现两种显示形态）。
func (h *AdminHandler) record(uid, kind, status, detail string, credits int64) {
	if h.p == nil || h.p.cfg.Log == nil {
		return
	}
	nick := ""
	if a := h.authOf(uid); a != nil {
		nick = a.Nickname
	}
	h.p.cfg.Log.Append(checkinlog.Record{
		UID:      checkinlog.NormalizeUID(uid),
		Nickname: nick,
		Kind:     kind,
		Status:   status,
		Detail:   detail,
		Credits:  credits,
		Trigger:  "manual",
	})
}

// ---------------------------------------------------------------------------
// 签到 / 保活
// ---------------------------------------------------------------------------

// Checkin POST /admin/checkin —— 巡检签到。
//
// 单账号同步返回（一次上游往返，够快）；全量含旅行巡检（账号间 800ms 限速），
// 同步会阻塞浏览器，改后台任务。
func (h *AdminHandler) Checkin(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)

	if body.UID != "" {
		res, ok := h.p.RunCheckinFor(body.UID, "manual")
		if !ok {
			writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "single", "result": res})
		return
	}

	if !h.task.Start("checkin", func() []map[string]any {
		return h.runCheckinAll()
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// runCheckinAll 全量签到：逐账号收集结果（旅行不并入结果列表，只进历史）。
//
// 与改造前 admin.runCheckinAll 的差异只有类型：结果是中转后的 JSON 形状，
// 顺序、跳过条件、旅行搭车的位置都逐字未变。
func (h *AdminHandler) runCheckinAll() []map[string]any {
	list := h.accountList()
	out := make([]map[string]any, 0, len(list))
	for _, st := range list {
		if st.Disabled {
			continue
		}
		if res, ok := h.p.RunCheckinFor(st.UID, "manual"); ok {
			out = append(out, resultView{
				UID: res.UID, Status: res.Status, Detail: res.Detail, Credits: res.Credits,
			}.map_())
		}
	}
	// 旅行搭签到的便车：必须在签到之后跑（签到会解冻刚充值的账号）。
	h.p.RunTravelManual()
	return out
}

// Keepalive POST /admin/keepalive —— token 保活。
func (h *AdminHandler) Keepalive(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	if body.UID != "" {
		res, ok := h.p.RunKeepaliveFor(body.UID, "manual")
		if !ok {
			writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "single", "result": res})
		return
	}
	if !h.task.Start("keepalive", func() []map[string]any {
		out := []map[string]any{}
		for _, st := range h.accountList() {
			if st.Disabled {
				continue
			}
			if res, ok := h.p.RunKeepaliveFor(st.UID, "manual"); ok {
				out = append(out, resultView{
					UID: res.UID, Status: res.Status, Detail: res.Detail, Credits: res.Credits,
				}.map_())
			}
		}
		return out
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// CreditsRefresh POST /admin/credits/refresh —— 只查余额并同步进池（不签到）。
func (h *AdminHandler) CreditsRefresh(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无额度刷新能力")
		return
	}
	if body.UID != "" {
		res, ok := h.p.RefreshCredits(body.UID, "manual")
		if !ok {
			writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "single", "result": res})
		return
	}
	out := []CreditsResult{}
	for _, st := range h.accountList() {
		if res, ok := h.p.RefreshCredits(st.UID, "manual"); ok {
			out = append(out, res)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": "all", "results": out})
}

// ---------------------------------------------------------------------------
// 猫猫旅行
// ---------------------------------------------------------------------------

// travelAuth 取可用于旅行接口的凭证（无 refresh token 视为不可用）。
func (h *AdminHandler) travelAuth(uid string) (*auth.Auth, bool) {
	if uid == "" {
		return nil, false
	}
	a := h.authOf(uid)
	return a, a != nil && a.RefreshToken != ""
}

// TravelList GET /admin/travel —— 账号级旅行列表。
//
// 直接读守卫维护的内存快照，不发上游请求；refresh=1 时强制全量回源一次。
func (h *AdminHandler) TravelList(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无旅行能力")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		h.p.RefreshTravel(true, false)
	}
	snaps := h.p.TravelSnapshots()
	// 池里有、但快照还没建起来的账号补一个空行，避免界面缺行让人以为是 bug。
	seen := map[string]bool{}
	for _, s := range snaps {
		seen[s.UID] = true
	}
	for _, st := range h.accountList() {
		if seen[st.UID] {
			continue
		}
		snaps = append(snaps, TravelSnapshot{
			UID: st.UID, Nickname: st.Nickname, Error: "尚未探测（点「刷新」）",
		})
	}
	// 把「自动派送挂在签到时点上」这件事所需的事实一并返回，让界面能显示真实时点
	// 而不是写死一句说明：签到时点可被设置页改，写死就会与实际不一致。
	checkinHours, _ := h.scheduleHours()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":         snaps,
		"auto_claim":       h.p.TravelAutoClaimEnabled(),
		"auto_depart":      h.checkinEnabled(),
		"checkin_hours":    checkinHours,
		"location_id":      4,
		"watch_interval_s": int64(h.p.WatchInterval().Seconds()),
	})
}

// TravelStatus GET /admin/travel/status —— 单账号详细状态（含猫档案全字段）。
//
// uid 缺失或账号无可用凭证时返回 404（改造前行为，保持）。
func (h *AdminHandler) TravelStatus(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	a, ok := h.travelAuth(uid)
	if !ok {
		writeError(w, http.StatusNotFound, "账号不存在或无可用凭证: "+uid)
		return
	}
	buddy, err := h.p.client.BuddyInfo(a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "查询猫档案失败: "+err.Error())
		return
	}
	resp := map[string]any{"uid": uid, "has_buddy": buddy != nil}
	if buddy != nil {
		resp["buddy"] = buddy
	}
	ts, err := h.p.client.TravelStatus(a)
	if err != nil {
		resp["travel_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["travel"] = ts
	// 顺带把换算后的到站时刻给出去，省得每个调用方各算一遍时钟偏差。
	if at := ts.ArriveAtTime(time.Now()); !at.IsZero() {
		resp["arrive_at_local"] = at
		resp["clock_skew_sec"] = int64(ts.ClockSkew(time.Now()).Seconds())
		if rem, ok := ts.RemainingUntilArrive(time.Now()); ok {
			resp["remaining_sec"] = int64(rem.Seconds())
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// TravelDepart POST /admin/travel/depart —— 派猫。
// uid 为空 = 全部账号（后台任务，含账号间限速）。
func (h *AdminHandler) TravelDepart(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无旅行能力")
		return
	}
	body := decodeBody(r)

	if body.UID != "" {
		res := h.p.TravelDepartFor(body.UID, "manual")
		if res.Status == checkinlog.StatusFail && strings.Contains(res.Detail, "账号不存在") {
			writeError(w, http.StatusNotFound, res.Detail)
			return
		}
		if res.Status == checkinlog.StatusSkip {
			writeError(w, http.StatusConflict, res.Detail)
			return
		}
		if res.Status == checkinlog.StatusFail {
			writeError(w, http.StatusBadGateway, res.Detail)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	if !h.task.Start("travel-depart", func() []map[string]any {
		return h.travelAll(func(uid string) TravelActionResult {
			return h.p.TravelDepartFor(uid, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// TravelClaim POST /admin/travel/claim —— 领奖。uid 为空 = 全部账号（后台任务）。
func (h *AdminHandler) TravelClaim(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无旅行能力")
		return
	}
	body := decodeBody(r)

	if body.UID != "" {
		res := h.p.TravelClaimFor(body.UID, "manual")
		if res.Status == checkinlog.StatusFail && strings.Contains(res.Detail, "账号不存在") {
			writeError(w, http.StatusNotFound, res.Detail)
			return
		}
		if res.Status == checkinlog.StatusSkip {
			writeError(w, http.StatusConflict, res.Detail)
			return
		}
		if res.Status == checkinlog.StatusFail {
			writeError(w, http.StatusBadGateway, res.Detail)
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	if !h.task.Start("travel-claim", func() []map[string]any {
		return h.travelAll(func(uid string) TravelActionResult {
			return h.p.TravelClaimFor(uid, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// travelAll 对全部非禁用账号跑同一旅行动作，转成任务槽可展示的形状。
// 跳过不写入结果列表（否则「全部派猫」会返回一堆 no-op 行），但已经由上游记进历史。
func (h *AdminHandler) travelAll(fn func(uid string) TravelActionResult) []map[string]any {
	out := []map[string]any{}
	for _, st := range h.accountList() {
		if st.Disabled {
			continue
		}
		res := fn(st.UID)
		if res.Status == checkinlog.StatusSkip {
			continue
		}
		out = append(out, resultView{
			UID: res.UID, Status: res.Status, Detail: res.Detail, Credits: res.Credits,
		}.map_())
	}
	return out
}

// ---------------------------------------------------------------------------
// 调度（两条端点读的是核心调度器，依赖经 AdminEnv 注入）
// ---------------------------------------------------------------------------

// Schedule GET /admin/schedule —— 下一次唤醒时刻与当前时点设置。
func (h *AdminHandler) Schedule(w http.ResponseWriter, r *http.Request) {
	sv := h.env.Schedule
	if sv == nil {
		writeError(w, http.StatusNotImplemented, "调度器未接线")
		return
	}
	at, names := sv.NextWake()
	checkinH, keepaliveH := sv.Hours()
	resp := map[string]any{
		"checkin_enabled":   sv.CheckinEnabled(),
		"keepalive_enabled": sv.KeepaliveEnabled(),
		"checkin_hours":     checkinH,
		"keepalive_hours":   keepaliveH,
	}
	if !at.IsZero() {
		resp["next_at"] = at
		resp["next_in_sec"] = int64(time.Until(at).Seconds())
		resp["next_tasks"] = names
	}
	writeJSON(w, http.StatusOK, resp)
}

// TaskStatus GET /admin/task —— 全量任务槽状态。
func (h *AdminHandler) TaskStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.task.Snapshot())
}

// scheduleHours 取签到时点；调度器未接线时返回 nil（与"未接线"降级一致）。
func (h *AdminHandler) scheduleHours() ([]int, []int) {
	if h.env.Schedule == nil {
		return nil, nil
	}
	return h.env.Schedule.Hours()
}

// checkinEnabled 签到是否启用；调度器未接线时按"未启用"报告。
func (h *AdminHandler) checkinEnabled() bool {
	if h.env.Schedule == nil {
		return false
	}
	return h.env.Schedule.CheckinEnabled()
}

// ---------------------------------------------------------------------------
// 任务历史
// ---------------------------------------------------------------------------

// History GET /admin/checkin/history —— 任务历史（签到/保活/旅行/积分/成长）。
//
// 支持 offset/limit 分页，契约与 /admin/logs/history 一致：
// 时间倒序（最新在前），total 是**过滤后**总数，前端据此算总页数。
func (h *AdminHandler) History(w http.ResponseWriter, r *http.Request) {
	lg := h.log()
	if lg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "total": 0})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = checkinlog.DefaultPageSize
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	items, total := lg.Page(offset, limit, r.URL.Query().Get("kind"))
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
}

// log 取历史日志（未接线时返回 nil）。
func (h *AdminHandler) log() *checkinlog.Log {
	if h.p == nil {
		return nil
	}
	return h.p.cfg.Log
}

// ---------------------------------------------------------------------------
// 成长中心
// ---------------------------------------------------------------------------

// GrowthList GET /admin/growth —— 账号级成长计划列表。
// refresh=1 时强制回源（对应界面「刷新」按钮）；默认读守卫维护的内存快照，不发上游请求。
func (h *AdminHandler) GrowthList(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		h.p.RefreshGrowth(true, false)
	}
	snaps := h.p.GrowthSnapshots()

	seen := map[string]bool{}
	for _, s := range snaps {
		seen[s.UID] = true
	}
	// 池里有、但快照还没建起来的账号补一行，避免界面凭空少一个账号。
	for _, st := range h.accountList() {
		if seen[st.UID] {
			continue
		}
		snaps = append(snaps, GrowthSnapshot{
			UID: st.UID, Nickname: st.Nickname, Error: "尚未探测（点「刷新」）",
		})
	}

	accept, makeup, redeem, open, draw, claim := h.p.GrowthToggles()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": snaps,
		"auto": map[string]bool{
			"accept": accept, "makeup": makeup, "redeem": redeem,
			"open": open, "draw": draw, "claim": claim,
		},
		"watch_interval_s": int64(h.p.GrowthWatchInterval().Seconds()),
	})
}

// GrowthTasks GET /admin/growth/tasks —— 拉某账号的**原始任务清单**（含未达成的）。
// 与 growthList 的区别：列表接口只带「可领取」的任务摘要，这里给完整明细。
func (h *AdminHandler) GrowthTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	a := h.authOf(uid)
	if uid == "" || a == nil || a.RefreshToken == "" {
		writeError(w, http.StatusNotFound, "账号不存在或无可用凭证: "+uid)
		return
	}
	tasks, err := h.p.client.GrowthTasks(a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "拉取成长任务失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "tasks": tasks, "total": len(tasks)})
}

// GrowthTravelConfig GET /admin/growth/travel/config —— 旅行地点完整配置（含各地点时长/奖励区间）。
func (h *AdminHandler) GrowthTravelConfig(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	a := h.authOf(uid)
	if uid == "" || a == nil || a.RefreshToken == "" {
		writeError(w, http.StatusNotFound, "账号不存在或无可用凭证: "+uid)
		return
	}
	cfg, err := h.p.client.GrowthTravelConfig(a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "拉取旅行配置失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// growthAct 是所有成长动作的公共请求体。
type growthAct struct {
	UID      string `json:"uid"`
	TaskCode string `json:"task_code"`
	Tier     string `json:"tier"`
	Date     string `json:"date"`
	Count    int    `json:"count"`
}

// GrowthClaim POST /admin/growth/claim —— 领取**奖励**。
//
// 把条件已达成（completed）但还没领的任务奖励领回来，是唯一真正让积分到账的动作。
// uid 为空 = 全部账号（后台任务）；task_code 为空 = 该账号全部可领任务。
func (h *AdminHandler) GrowthClaim(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := h.p.GrowthClaimFor(body.UID, body.TaskCode, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.Start("growth-claim", func() []map[string]any {
		return h.growthAll(func(uid string) GrowthActionResult {
			return h.p.GrowthClaimFor(uid, body.TaskCode, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// GrowthAccept POST /admin/growth/accept —— 接单。
//
// 注意语义：接单**不发奖励**。领奖是 GrowthClaim。
func (h *AdminHandler) GrowthAccept(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := h.p.GrowthAcceptFor(body.UID, body.TaskCode, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.Start("growth-accept", func() []map[string]any {
		return h.growthAll(func(uid string) GrowthActionResult {
			return h.p.GrowthAcceptFor(uid, body.TaskCode, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// GrowthRedeem POST /admin/growth/redeem —— 连登兑换。
func (h *AdminHandler) GrowthRedeem(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := h.p.GrowthRedeemFor(body.UID, body.Tier, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.Start("growth-redeem", func() []map[string]any {
		return h.growthAll(func(uid string) GrowthActionResult {
			return h.p.GrowthRedeemFor(uid, body.Tier, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// GrowthMakeup POST /admin/growth/makeup —— 补签。
func (h *AdminHandler) GrowthMakeup(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := h.p.GrowthMakeupFor(body.UID, body.Date, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.Start("growth-makeup", func() []map[string]any {
		return h.growthAll(func(uid string) GrowthActionResult {
			return h.p.GrowthMakeupFor(uid, body.Date, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// GrowthOpen POST /admin/growth/open —— 开盲盒。
func (h *AdminHandler) GrowthOpen(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := h.p.GrowthOpenFor(body.UID, body.Count, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.Start("growth-open", func() []map[string]any {
		return h.growthAll(func(uid string) GrowthActionResult {
			return h.p.GrowthOpenFor(uid, body.Count, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// GrowthDraw POST /admin/growth/draw —— 抽奖。
func (h *AdminHandler) GrowthDraw(w http.ResponseWriter, r *http.Request) {
	if h.p == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := h.p.GrowthDrawFor(body.UID, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.Start("growth-draw", func() []map[string]any {
		return h.growthAll(func(uid string) GrowthActionResult {
			return h.p.GrowthDrawFor(uid, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// writeGrowthSingle 把单账号动作结果翻译成 HTTP：skip 用 409（「现在不用做」而非失败），
// fail 用 502/404，成功 200。语义与旅行接口保持一致。
func (h *AdminHandler) writeGrowthSingle(w http.ResponseWriter, uid string, res GrowthActionResult) {
	switch res.Status {
	case checkinlog.StatusSkip:
		writeError(w, http.StatusConflict, res.Detail)
	case checkinlog.StatusFail:
		if strings.Contains(res.Detail, "账号不存在") {
			writeError(w, http.StatusNotFound, res.Detail)
			return
		}
		writeError(w, http.StatusBadGateway, res.Detail)
	default:
		log.Printf("admin: growth %s uid=%s %s", res.Action, uid[:min(8, len(uid))], res.Detail)
		writeJSON(w, http.StatusOK, res)
	}
}

// growthAll 对全部账号跑同一动作，转成任务槽可展示的形状。
// skip 不进结果列表（否则「全部领取」会返回一堆 no-op），但已由上游记进历史。
func (h *AdminHandler) growthAll(fn func(uid string) GrowthActionResult) []map[string]any {
	out := []map[string]any{}
	for _, st := range h.accountList() {
		if st.Disabled {
			continue
		}
		res := fn(st.UID)
		if res.Status == checkinlog.StatusSkip {
			continue
		}
		out = append(out, resultView{
			UID: res.UID, Status: res.Status, Detail: res.Detail, Credits: res.Credits,
		}.map_())
	}
	return out
}

func decodeGrowthAct(r *http.Request) growthAct {
	var b growthAct
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&b)
	}
	return b
}

// ---------------------------------------------------------------------------
// 本机客户端登录
// ---------------------------------------------------------------------------

// ClientLoginStatus GET /admin/client-login —— 客户端登录态 + 可切换候选人列表。
func (h *AdminHandler) ClientLoginStatus(w http.ResponseWriter, r *http.Request) {
	m := h.p.clientLogin()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "本控制台未启用客户端登录管理")
		return
	}
	st, err := m.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取客户端登录态失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// clientLoginAct 是所有客户端登录动作的请求体。
// Confirm 必须是显式的 true：零值 false 代表「没确认」，一律拒绝。
type clientLoginAct struct {
	UID     string `json:"uid"`
	Confirm bool   `json:"confirm"`
}

func decodeClientLoginAct(r *http.Request) clientLoginAct {
	var b clientLoginAct
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&b)
	}
	return b
}

// ClientLoginSwitch POST /admin/client-login/switch —— 把客户端登录态切到指定账号。
//
// 这是本控制台**唯一会改动客户端本机状态**的接口，所以强制 confirm=true
// 且切换前必须能备份（备份逻辑在 clientlogin 包里）。
func (h *AdminHandler) ClientLoginSwitch(w http.ResponseWriter, r *http.Request) {
	m := h.p.clientLogin()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "本控制台未启用客户端登录管理")
		return
	}
	body := decodeClientLoginAct(r)
	if !body.Confirm {
		writeError(w, http.StatusPreconditionRequired, "切换客户端登录态是破坏性操作，需要在界面上二次确认")
		return
	}
	if body.UID == "" {
		writeError(w, http.StatusBadRequest, "缺少 uid")
		return
	}

	res, err := m.Switch(body.UID)
	if err != nil {
		switch {
		case err == clientloginErrSameAccount:
			writeError(w, http.StatusConflict, err.Error())
		case errorsIs(err, clientloginErrClientRunning):
			// 客户端在跑时写盘会被它原地覆盖：这不是故障，是前置条件不满足。
			// 原样返回错误文本（里面已写明要先退出客户端），不加「切换失败」前缀。
			writeError(w, http.StatusConflict, err.Error())
		default:
			// 切换失败的常见原因（找不到凭证、备份失败、快照写不进去）
			// 都属于「当前环境不允许」，用 409 比 500 更贴近语义，前端也好区分。
			writeError(w, http.StatusConflict, "切换失败: "+err.Error())
		}
		return
	}
	log.Printf("admin: client-login switch uid=%s source=%s backup=%s", body.UID, res.Source, res.BackupPath)
	writeJSON(w, http.StatusOK, res)
}

// ClientLoginRestore POST /admin/client-login/restore —— 回滚到上一次切换前的登录态。
func (h *AdminHandler) ClientLoginRestore(w http.ResponseWriter, r *http.Request) {
	m := h.p.clientLogin()
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "本控制台未启用客户端登录管理")
		return
	}
	body := decodeClientLoginAct(r)
	if !body.Confirm {
		writeError(w, http.StatusPreconditionRequired, "回滚是破坏性操作，需要在界面上二次确认")
		return
	}
	res, err := m.Restore()
	if err != nil {
		// 无备份 / 备份即当前态都属于「这次回滚没有意义」；
		// 客户端在跑则是前置条件不满足 —— 三种都用 409，且原样给出错误文本。
		if errorsIs(err, clientloginErrAlreadyBackedUp) || errorsIs(err, clientloginErrClientRunning) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusConflict, "回滚失败: "+err.Error())
		return
	}
	log.Printf("admin: client-login restore uid=%s", res.UID)
	writeJSON(w, http.StatusOK, res)
}

// 编译期断言：默认任务槽满足接口。
var _ TaskSlot = (*taskSlot)(nil)
