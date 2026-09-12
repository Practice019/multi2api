// growth.go 管理台「成长计划」面板的后端：快照读取 + 手动动作 + 明细查询。
//
// 全部动作都走上游提供的单账号方法（与自动守卫共用同一份判定逻辑），
// 管理台只负责「取参数 → 调用 → 翻译成 HTTP 语义」，不重复实现领取条件判断。
//
// # 解耦说明（Task 3b）
//
// 本文件原先直接引用 scheduler 的成长方法。那让核心的管理台被迫认识
// CodeBuddy 的任务体系。现在改走 admin 自己声明的 UpstreamBusiness 接口 ——
// 加第二个上游时本文件零改动。
package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/scheduler"
)

// growthList 账号级成长计划列表。
// refresh=1 时强制回源（对应界面「刷新」按钮）；默认读守卫维护的内存快照，不发上游请求。
func (h *Handler) growthList(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		b.RefreshGrowth(true, false)
	}
	snaps := b.GrowthSnapshots()

	seen := map[string]bool{}
	for _, s := range snaps {
		seen[s.UID] = true
	}
	// 池里有、但快照还没建起来的账号补一行，避免界面凭空少一个账号。
	for _, st := range h.cfg.Pool.List() {
		if seen[st.UID] {
			continue
		}
		snaps = append(snaps, GrowthSnapshot{
			UID: st.UID, Nickname: st.Nickname, Error: "尚未探测（点「刷新」）",
		})
	}

	accept, makeup, redeem, open, draw, claim := b.GrowthToggles()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": snaps,
		"auto": map[string]bool{
			"accept": accept, "makeup": makeup, "redeem": redeem,
			"open": open, "draw": draw, "claim": claim,
		},
		"watch_interval_s": int64(b.GrowthWatchInterval().Seconds()),
	})
}

// growthTasks 拉某账号的**原始任务清单**（含未达成的），供界面展开全部任务。
// 与 growthList 的区别：列表接口只带「可领取」的任务摘要，这里给完整明细。
func (h *Handler) growthTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	a := h.cfg.Pool.AuthByUID(uid)
	if uid == "" || a == nil || a.RefreshToken == "" {
		writeError(w, http.StatusNotFound, "账号不存在或无可用凭证: "+uid)
		return
	}
	tasks, err := h.cfg.Upstream.GrowthTasks(a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "拉取成长任务失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "tasks": tasks, "total": len(tasks)})
}

// growthTravelConfig 旅行地点完整配置（含各地点时长/奖励区间）。
func (h *Handler) growthTravelConfig(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	a := h.cfg.Pool.AuthByUID(uid)
	if uid == "" || a == nil || a.RefreshToken == "" {
		writeError(w, http.StatusNotFound, "账号不存在或无可用凭证: "+uid)
		return
	}
	cfg, err := h.cfg.Upstream.GrowthTravelConfig(a)
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

// growthClaim 领取**奖励**：把条件已达成（completed）但还没领的任务奖励领回来。
// 这是唯一真正让积分到账的动作。uid 为空 = 全部账号（后台任务）；
// task_code 为空 = 该账号全部可领任务。
func (h *Handler) growthClaim(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := b.GrowthClaimFor(body.UID, body.TaskCode, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.start("growth-claim", func() []scheduler.CheckinResult {
		return h.growthAll(func(uid string) GrowthActionResult {
			return b.GrowthClaimFor(uid, body.TaskCode, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// growthAccept 接单：把未接单的任务接进列表开始计进度。
//
// 注意语义：接单**不发奖励**。以前这里的注释写成「领取任务奖励」，
// 结果把接单误当成了领奖，导致「做完任务积分没涨」这个现象被误判成上游行为。
// 领奖是 growthClaim。
func (h *Handler) growthAccept(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := b.GrowthAcceptFor(body.UID, body.TaskCode, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.start("growth-accept", func() []scheduler.CheckinResult {
		return h.growthAll(func(uid string) GrowthActionResult {
			return b.GrowthAcceptFor(uid, body.TaskCode, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

func (h *Handler) growthRedeem(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := b.GrowthRedeemFor(body.UID, body.Tier, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.start("growth-redeem", func() []scheduler.CheckinResult {
		return h.growthAll(func(uid string) GrowthActionResult {
			return b.GrowthRedeemFor(uid, body.Tier, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

func (h *Handler) growthMakeup(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := b.GrowthMakeupFor(body.UID, body.Date, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.start("growth-makeup", func() []scheduler.CheckinResult {
		return h.growthAll(func(uid string) GrowthActionResult {
			return b.GrowthMakeupFor(uid, body.Date, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

func (h *Handler) growthOpen(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := b.GrowthOpenFor(body.UID, body.Count, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.start("growth-open", func() []scheduler.CheckinResult {
		return h.growthAll(func(uid string) GrowthActionResult {
			return b.GrowthOpenFor(uid, body.Count, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

func (h *Handler) growthDraw(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无成长中心能力")
		return
	}
	body := decodeGrowthAct(r)
	if body.UID != "" {
		res := b.GrowthDrawFor(body.UID, "manual")
		h.writeGrowthSingle(w, body.UID, res)
		return
	}
	if !h.task.start("growth-draw", func() []scheduler.CheckinResult {
		return h.growthAll(func(uid string) GrowthActionResult {
			return b.GrowthDrawFor(uid, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// writeGrowthSingle 把单账号动作结果翻译成 HTTP：skip 用 409（「现在不用做」而非失败），
// fail 用 502/404，成功 200。语义与旅行接口保持一致。
func (h *Handler) writeGrowthSingle(w http.ResponseWriter, uid string, res GrowthActionResult) {
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

// growthAll 对全部账号跑同一动作，转成 CheckinResult 供任务槽统一呈现。
// skip 不进结果列表（否则「全部领取」会返回一堆 no-op），但已由上游记进历史。
func (h *Handler) growthAll(fn func(uid string) GrowthActionResult) []scheduler.CheckinResult {
	out := []scheduler.CheckinResult{}
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		res := fn(st.UID)
		if res.Status == checkinlog.StatusSkip {
			continue
		}
		out = append(out, scheduler.CheckinResult{
			UID: res.UID, Status: res.Status, Detail: res.Detail, Credits: res.Credits,
		})
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
