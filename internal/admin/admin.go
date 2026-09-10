// Package admin 管理台后端：账号增删、签到/保活/旅行触发、调度开关、日志与历史。
//
// 安全模型（方案①）：整个 /admin/* 子树只接受 loopback 直连，非本机一律 403。
// 理由：这些接口能改账号池、能触发上游请求、能读到账号昵称与积分，
// 而浏览器从别的设备打开时无法自动携带 Bearer，与其做半吊子鉴权，不如直接关在门外。
// 需要远程操作时请走 SSH 隧道。
package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/oauth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/upstream"
)

// Config 管理台依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler
	OAuth     *oauth.Client
	Log       *checkinlog.Log
	Ring      *logbuf.Ring
	AuthDir   string

	// ResetModelsCache 清空模型目录缓存（由 server 包注入，避免 admin 反向依赖 server）。
	ResetModelsCache func()
	// BuildTime 进程启动时间，UI 用来算运行时长。
	StartedAt time.Time
}

// Handler 管理台路由（/admin/ 子树）。
type Handler struct {
	cfg  Config
	mux  *http.ServeMux
	task *taskSlot
}

// New 构建管理台 handler。
func New(cfg Config) *Handler {
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), task: &taskSlot{}}

	h.mux.HandleFunc("GET /admin/accounts", h.accounts)
	h.mux.HandleFunc("POST /admin/accounts/reload", h.accountsReload)
	h.mux.HandleFunc("POST /admin/accounts/{uid}/enable", h.accountEnable)
	h.mux.HandleFunc("POST /admin/accounts/{uid}/disable", h.accountDisable)
	h.mux.HandleFunc("POST /admin/accounts/{uid}/cooldown/clear", h.accountClearCooldown)
	h.mux.HandleFunc("DELETE /admin/accounts/{uid}", h.accountDelete)

	h.mux.HandleFunc("POST /admin/login/start", h.loginStart)
	h.mux.HandleFunc("POST /admin/login/poll", h.loginPoll)

	h.mux.HandleFunc("POST /admin/checkin", h.checkin)
	h.mux.HandleFunc("POST /admin/keepalive", h.keepalive)
	h.mux.HandleFunc("POST /admin/credits/refresh", h.creditsRefresh)

	h.mux.HandleFunc("GET /admin/travel/status", h.travelStatus)
	h.mux.HandleFunc("POST /admin/travel/depart", h.travelDepart)
	h.mux.HandleFunc("POST /admin/travel/claim", h.travelClaim)

	h.mux.HandleFunc("GET /admin/schedule", h.schedule)
	h.mux.HandleFunc("POST /admin/schedule/toggle", h.scheduleToggle)

	h.mux.HandleFunc("POST /admin/models/refresh", h.modelsRefresh)

	h.mux.HandleFunc("GET /admin/logs", h.logs)
	h.mux.HandleFunc("GET /admin/stats", h.stats)
	h.mux.HandleFunc("GET /admin/checkin/history", h.history)
	h.mux.HandleFunc("GET /admin/task", h.taskStatus)

	return h
}

// ServeHTTP 先做本机校验，再进路由。
//
// 这里统一把请求体读完再回应：Go 的 http server 在 body 未被耗尽时会直接关闭连接
// 而非复用，客户端可能观察到 ECONNRESET（表现为「服务端日志成功、浏览器/脚本报连接重置」）。
// 不要求每个 handler 自己记得 drain，放在唯一入口一次做掉。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
	}()
	if !isLoopback(r.RemoteAddr) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "管理接口仅允许本机直连（RemoteAddr=" + r.RemoteAddr + "）。需要远程访问请使用 SSH 隧道。",
		})
		return
	}
	h.mux.ServeHTTP(w, r)
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// 账号视图
// ---------------------------------------------------------------------------

// AccountView 是 pool.Status 的管理台增强视图（补 token 有效期、凭证文件、今日签到）。
type AccountView struct {
	pool.Status
	HasToken       bool   `json:"has_token"`
	TokenExpireAt  int64  `json:"token_expire_at,omitempty"`
	TokenExpireSec int64  `json:"token_expire_sec,omitempty"`
	File           string `json:"file,omitempty"`
	TodayCheckin   string `json:"today_checkin,omitempty"`
	TodayCheckinAt int64  `json:"today_checkin_at,omitempty"`
}

func (h *Handler) accountViews() []AccountView {
	list := h.cfg.Pool.List()
	today := checkinlog.TodayStart()
	out := make([]AccountView, 0, len(list))
	for _, st := range list {
		v := AccountView{Status: st}
		if a := h.cfg.Pool.AuthByUID(st.UID); a != nil {
			v.HasToken = a.AccessToken != ""
			v.TokenExpireAt = a.ExpiresAt
			if a.ExpiresAt > 0 {
				v.TokenExpireSec = a.ExpiresAt - time.Now().Unix()
			}
			if a.FilePath != "" {
				v.File = filepath.Base(a.FilePath)
			}
		}
		// 今日签到结果：从历史里取当天该 uid 的最近一条 checkin。
		if h.cfg.Log != nil {
			for _, rec := range h.cfg.Log.Since(today) {
				if rec.UID == st.UID && rec.Kind == checkinlog.KindCheckin {
					v.TodayCheckin = rec.Status
					v.TodayCheckinAt = rec.At.UnixMilli()
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) accounts(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.accountViews(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		"auth_dir":       h.cfg.AuthDir,
	})
}

// accountsReload 重新扫描 auths 目录并对齐池（手工拷入凭证后无需重启网关）。
func (h *Handler) accountsReload(w http.ResponseWriter, r *http.Request) {
	auths, err := auth.LoadDir(h.cfg.AuthDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取 auths 目录失败: "+err.Error())
		return
	}
	before := len(h.cfg.Pool.List())
	h.cfg.Pool.SyncToDir(auths)
	after := len(h.cfg.Pool.List())
	log.Printf("admin: reload auths dir=%s scanned=%d pool %d -> %d", h.cfg.AuthDir, len(auths), before, after)
	writeJSON(w, http.StatusOK, map[string]any{
		"scanned": len(auths), "before": before, "after": after,
	})
}

func (h *Handler) accountEnable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !h.cfg.Pool.Enable(uid) {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	log.Printf("admin: enable %s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": false})
}

func (h *Handler) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Reason == "" {
		body.Reason = "manual disable"
	}
	if _, ok := h.cfg.Pool.Status(uid); !ok {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	h.cfg.Pool.Disable(uid, body.Reason)
	log.Printf("admin: disable %s reason=%s", uid, body.Reason)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": true, "reason": body.Reason})
}

func (h *Handler) accountClearCooldown(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !h.cfg.Pool.ClearCooldown(uid) {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	log.Printf("admin: clear cooldown %s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "cleared": true})
}

// accountDelete 把账号移出池。默认只出池、保留 auths 文件；purge_file=1 才连文件一起删。
func (h *Handler) accountDelete(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	purge := r.URL.Query().Get("purge_file") == "1"

	var filePath string
	if a := h.cfg.Pool.AuthByUID(uid); a != nil {
		filePath = a.FilePath
	}
	if _, ok := h.cfg.Pool.Status(uid); !ok {
		writeError(w, http.StatusNotFound, "账号不存在: "+uid)
		return
	}
	h.cfg.Pool.Remove(uid)

	deleted := ""
	if purge && filePath != "" {
		// 只允许删 auths 目录内的文件：防止 FilePath 被构造成任意路径删除。
		absDir, _ := filepath.Abs(h.cfg.AuthDir)
		absFile, _ := filepath.Abs(filePath)
		if strings.HasPrefix(absFile, absDir+string(os.PathSeparator)) {
			if err := os.Remove(absFile); err != nil {
				log.Printf("admin: delete %s pool 移除成功但删文件失败: %v", uid, err)
			} else {
				deleted = absFile
			}
		} else {
			log.Printf("admin: delete %s 拒绝删目录外文件 %s", uid, absFile)
		}
	}
	log.Printf("admin: remove %s (purge_file=%v deleted=%q)", uid, purge, deleted)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "removed": true, "purged_file": deleted, "file": filePath,
	})
}

// ---------------------------------------------------------------------------
// 添加账号（OAuth 设备授权）
// ---------------------------------------------------------------------------

func (h *Handler) loginStart(w http.ResponseWriter, r *http.Request) {
	state, authURL, err := h.cfg.OAuth.Start()
	if err != nil {
		writeError(w, http.StatusBadGateway, "向上游申请授权链接失败: "+err.Error())
		return
	}
	log.Printf("admin: oauth start state=%s", state[:8])
	writeJSON(w, http.StatusOK, map[string]any{
		"state":          state,
		"auth_url":       authURL,
		"expires_in_sec": int64(oauth.StateTTL().Seconds()),
	})
}

func (h *Handler) loginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.State == "" {
		writeError(w, http.StatusBadRequest, "缺少 state")
		return
	}
	cred, err := h.cfg.OAuth.Poll(body.State)
	switch {
	case errors.Is(err, oauth.ErrPending):
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
		return
	case errors.Is(err, oauth.ErrStateUnknown):
		writeError(w, http.StatusGone, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "轮询失败: "+err.Error())
		return
	}

	path, err := cred.SaveToDir(h.cfg.AuthDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "凭证落盘失败: "+err.Error())
		return
	}
	// 落盘后重扫目录对齐池：新账号立即参与选号，无需重启。
	auths, lerr := auth.LoadDir(h.cfg.AuthDir)
	if lerr != nil {
		writeError(w, http.StatusInternalServerError, "凭证已写入但重扫目录失败: "+lerr.Error())
		return
	}
	h.cfg.Pool.SyncToDir(auths)
	log.Printf("admin: oauth 成功 uid=%s nick=%s file=%s", cred.UID, cred.Nickname, filepath.Base(path))
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"uid":           cred.UID,
		"nickname":      cred.Nickname,
		"enterprise_id": cred.EnterpriseID,
		"domain":        cred.Domain,
		"expires_in":    cred.ExpiresIn,
		"file":          filepath.Base(path),
	})
}

// ---------------------------------------------------------------------------
// 签到 / 保活 / 积分
// ---------------------------------------------------------------------------

// CheckinBody 全量与单账号共用请求体。
type CheckinBody struct {
	UID string `json:"uid"`
}

func (h *Handler) checkin(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)

	// 单账号：直接同步返回结果（一次上游往返，够快）。
	if body.UID != "" {
		res, ok := h.cfg.Scheduler.RunCheckinFor(body.UID, "manual")
		if !ok {
			writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "single", "result": res})
		return
	}

	// 全量：含旅行巡检（账号间 800ms 限速），同步会阻塞浏览器，改后台任务。
	if !h.task.start("checkin", func() []scheduler.CheckinResult {
		return h.runCheckinAll()
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// runCheckinAll 全量签到：逐账号收集结果（旅行不并入结果列表，只进历史）。
func (h *Handler) runCheckinAll() []scheduler.CheckinResult {
	list := h.cfg.Pool.List()
	out := make([]scheduler.CheckinResult, 0, len(list))
	for _, st := range list {
		if st.Disabled {
			continue
		}
		if res, ok := h.cfg.Scheduler.RunCheckinFor(st.UID, "manual"); ok {
			out = append(out, res)
		}
	}
	h.cfg.Scheduler.RunTravelManual()
	return out
}

func (h *Handler) keepalive(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	if body.UID != "" {
		res, ok := h.cfg.Scheduler.RunKeepaliveFor(body.UID, "manual")
		if !ok {
			writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "single", "result": res})
		return
	}
	if !h.task.start("keepalive", func() []scheduler.CheckinResult {
		out := []scheduler.CheckinResult{}
		for _, st := range h.cfg.Pool.List() {
			if st.Disabled {
				continue
			}
			if res, ok := h.cfg.Scheduler.RunKeepaliveFor(st.UID, "manual"); ok {
				out = append(out, res)
			}
		}
		return out
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

func (h *Handler) creditsRefresh(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	if body.UID != "" {
		res, ok := h.cfg.Scheduler.RefreshCredits(body.UID, "manual")
		if !ok {
			writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "single", "result": res})
		return
	}
	out := []scheduler.CheckinResult{}
	for _, st := range h.cfg.Pool.List() {
		if res, ok := h.cfg.Scheduler.RefreshCredits(st.UID, "manual"); ok {
			out = append(out, res)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": "all", "results": out})
}

// ---------------------------------------------------------------------------
// 猫猫旅行
// ---------------------------------------------------------------------------

func (h *Handler) travelAuth(uid string) (*auth.Auth, bool) {
	if uid == "" {
		return nil, false
	}
	a := h.cfg.Pool.AuthByUID(uid)
	return a, a != nil && a.RefreshToken != ""
}

func (h *Handler) travelStatus(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	a, ok := h.travelAuth(uid)
	if !ok {
		writeError(w, http.StatusNotFound, "账号不存在或无可用凭证: "+uid)
		return
	}
	buddy, err := h.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "查询猫档案失败: "+err.Error())
		return
	}
	resp := map[string]any{"uid": uid, "has_buddy": buddy != nil}
	if buddy != nil {
		resp["buddy"] = buddy
	}
	ts, err := h.cfg.Upstream.TravelStatus(a)
	if err != nil {
		resp["travel_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["travel"] = ts
	writeJSON(w, http.StatusOK, resp)
}

// record 统一写一条手动任务历史。
// 关键：Nickname 必须由这里补上——直接把 Record 丢给 Log.Append 会漏掉昵称，
// 历史表里就会退化成显示 uid 前缀（同一张表出现两种显示形态）。
func (h *Handler) record(uid, kind, status, detail string, credits int64) {
	if h.cfg.Log == nil {
		return
	}
	nick := ""
	if a := h.cfg.Pool.AuthByUID(uid); a != nil {
		nick = a.Nickname
	}
	h.cfg.Log.Append(checkinlog.Record{
		UID:      checkinlog.NormalizeUID(uid),
		Nickname: nick,
		Kind:     kind,
		Status:   status,
		Detail:   detail,
		Credits:  credits,
		Trigger:  "manual",
	})
}

func (h *Handler) travelDepart(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	a, ok := h.travelAuth(body.UID)
	if !ok {
		writeError(w, http.StatusNotFound, "账号不存在或无可用凭证: "+body.UID)
		return
	}
	// 先查状态再动手：当日已派出 / 猫在途时上游会回 400，
	// 那是「按规则不该派」而不是「派失败」，必须区分开——否则历史表里全是假失败。
	if ts, err := h.cfg.Upstream.TravelStatus(a); err == nil {
		switch {
		case ts.DailyLimitReached:
			h.record(body.UID, checkinlog.KindTravel, checkinlog.StatusSkip, "今日已派出（每日 1 次）", 0)
			writeError(w, http.StatusConflict, "今天已经派过了（每日 1 次，CST 00:00 重置）")
			return
		case ts.State == "traveling":
			h.record(body.UID, checkinlog.KindTravel, checkinlog.StatusSkip, "猫还在路上", 0)
			writeError(w, http.StatusConflict, fmt.Sprintf("猫还在路上（record=%d），等它到站再操作", ts.RecordID))
			return
		case ts.State == "arrived":
			h.record(body.UID, checkinlog.KindTravel, checkinlog.StatusSkip, "猫已到站，先去领奖", 0)
			writeError(w, http.StatusConflict, "猫已到站，请先「领奖」再派出")
			return
		}
	}

	loc := body.LocationID
	if loc <= 0 {
		loc = 4 // 四个地点收益/时长相同，默认古镇客栈
	}
	if err := h.cfg.Upstream.TravelDepart(a, loc); err != nil {
		h.record(body.UID, checkinlog.KindTravel, checkinlog.StatusFail, "派出失败: "+short(err.Error()), 0)
		writeError(w, http.StatusBadGateway, "派出失败: "+err.Error())
		return
	}
	h.record(body.UID, checkinlog.KindTravel, checkinlog.StatusOK, fmt.Sprintf("已派出（地点 %d）", loc), 0)
	writeJSON(w, http.StatusOK, map[string]any{"uid": body.UID, "departed": true, "location_id": loc})
}

func (h *Handler) travelClaim(w http.ResponseWriter, r *http.Request) {
	body := decodeBody(r)
	a, ok := h.travelAuth(body.UID)
	if !ok {
		writeError(w, http.StatusNotFound, "账号不存在或无可用凭证: "+body.UID)
		return
	}
	// record_id 必须来自上游实时状态：调用方未给就现查一次，避免让 UI 维护这个值。
	recordID := body.RecordID
	if recordID == 0 {
		ts, err := h.cfg.Upstream.TravelStatus(a)
		if err != nil {
			writeError(w, http.StatusBadGateway, "查询旅行状态失败: "+err.Error())
			return
		}
		if ts.State != "arrived" {
			// 没到站不是失败，是「现在没奖可领」。
			h.record(body.UID, checkinlog.KindTravel, checkinlog.StatusSkip,
				fmt.Sprintf("猫未到站（%s），无奖可领", ts.State), 0)
			writeError(w, http.StatusConflict, "猫还没到站（当前状态 "+ts.State+"），无奖可领")
			return
		}
		recordID = ts.RecordID
	}
	if recordID == 0 {
		writeError(w, http.StatusConflict, "上游未返回 record_id，无法领奖")
		return
	}
	reward, err := h.cfg.Upstream.TravelClaim(a, recordID)
	if err != nil {
		h.record(body.UID, checkinlog.KindTravel, checkinlog.StatusFail, "领奖失败: "+short(err.Error()), 0)
		writeError(w, http.StatusBadGateway, "领奖失败: "+err.Error())
		return
	}
	h.record(body.UID, checkinlog.KindTravel, checkinlog.StatusOK, "已领奖", reward)
	writeJSON(w, http.StatusOK, map[string]any{"uid": body.UID, "claimed": true, "reward_credit": reward})
}

// short 把上游错误压成一行短文本（历史表里只放这个，完整原文留给进程日志）。
func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

// ---------------------------------------------------------------------------
// 调度
// ---------------------------------------------------------------------------

func (h *Handler) schedule(w http.ResponseWriter, r *http.Request) {
	at, names := h.cfg.Scheduler.NextWake()
	checkinH, keepaliveH := h.cfg.Scheduler.Hours()
	resp := map[string]any{
		"checkin_enabled":   h.cfg.Scheduler.CheckinEnabled(),
		"keepalive_enabled": h.cfg.Scheduler.KeepaliveEnabled(),
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

func (h *Handler) scheduleToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Checkin   *bool `json:"checkin"`
		Keepalive *bool `json:"keepalive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if body.Checkin != nil {
		h.cfg.Scheduler.SetCheckinEnabled(*body.Checkin)
		log.Printf("admin: 签到排程 -> %v", *body.Checkin)
	}
	if body.Keepalive != nil {
		h.cfg.Scheduler.SetKeepaliveEnabled(*body.Keepalive)
		log.Printf("admin: 保活排程 -> %v", *body.Keepalive)
	}
	h.schedule(w, r)
}

// ---------------------------------------------------------------------------
// 模型目录 / 日志 / 历史
// ---------------------------------------------------------------------------

func (h *Handler) modelsRefresh(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ResetModelsCache == nil {
		writeError(w, http.StatusNotImplemented, "模型缓存刷新未接线")
		return
	}
	h.cfg.ResetModelsCache()
	log.Printf("admin: 模型目录缓存已清空")
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "cursor": 0})
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	items, cursor := h.cfg.Ring.Snapshot(since)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":    items,
		"cursor":   cursor,
		"capacity": h.cfg.Ring.Cap(),
		"held":     h.cfg.Ring.Len(),
	})
}

// stats 汇总环形缓冲，给出「这个网关到底跑了多少、跑得怎么样」的只读视图。
// 数据窗口 = 进程启动至今（缓冲上限 2000 条），并在响应里显式说明，避免被误读成全量历史。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil {
		writeJSON(w, http.StatusOK, map[string]any{"total": 0})
		return
	}
	items, _ := h.cfg.Ring.Snapshot(0)

	byModel := map[string]int{}
	byStatus := map[string]int{}
	byUID := map[string]int{}
	var okN, failN, tokens int64
	var ttfbSum, totalSum int64
	var ttfbN int64
	var oldest, newest time.Time

	for _, e := range items {
		byModel[e.Model]++
		byStatus[strconv.Itoa(e.Status)]++
		if e.UID != "" {
			byUID[e.UID]++
		}
		switch {
		case e.Status >= 200 && e.Status < 300:
			okN++
		case e.Status >= 400:
			failN++
		}
		if e.Tokens > 0 {
			tokens += int64(e.Tokens)
		}
		totalSum += e.TotalMS
		if e.TTFBMS > 0 {
			ttfbSum += e.TTFBMS
			ttfbN++
		}
		if oldest.IsZero() || e.At.Before(oldest) {
			oldest = e.At
		}
		if e.At.After(newest) {
			newest = e.At
		}
	}

	resp := map[string]any{
		"total":     len(items),
		"ok":        okN,
		"fail":      failN,
		"by_model":  byModel,
		"by_status": byStatus,
		"by_uid":    byUID,
		"capacity":  h.cfg.Ring.Cap(),
		"held":      h.cfg.Ring.Len(),
		"tokens":    tokens,
	}
	if len(items) > 0 {
		resp["avg_total_ms"] = totalSum / int64(len(items))
	}
	if ttfbN > 0 {
		resp["avg_ttfb_ms"] = ttfbSum / ttfbN
	}
	if !oldest.IsZero() {
		resp["window_from"] = oldest
		resp["window_to"] = newest
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Log == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	kind := r.URL.Query().Get("kind")
	items := h.cfg.Log.Recent(0)
	if kind != "" {
		filtered := make([]checkinlog.Record, 0, len(items))
		for _, rec := range items {
			if rec.Kind == kind {
				filtered = append(filtered, rec)
			}
		}
		items = filtered
	}
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *Handler) taskStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.task.snapshot())
}

// ---------------------------------------------------------------------------
// 后台任务槽（同一时刻只允许一个全量任务）
// ---------------------------------------------------------------------------

type taskSlot struct {
	mu       sync.Mutex
	running  bool
	kind     string
	started  time.Time
	finished time.Time
	results  []scheduler.CheckinResult
	errMsg   string
}

// start 尝试占用任务槽并同步执行 fn（在调用方 goroutine 外另起一个）。
// 已有任务在跑时返回 false，调用方应回 409 而不是排队——排队会让界面误以为立刻执行了。
func (t *taskSlot) start(kind string, fn func() []scheduler.CheckinResult) bool {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return false
	}
	t.running = true
	t.kind = kind
	t.started = time.Now()
	t.finished = time.Time{}
	t.results = nil
	t.errMsg = ""
	t.mu.Unlock()

	go func() {
		var results []scheduler.CheckinResult
		var errMsg string
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					errMsg = fmt.Sprintf("panic: %v", rec)
				}
			}()
			results = fn()
		}()
		t.mu.Lock()
		t.running = false
		t.finished = time.Now()
		t.results = results
		t.errMsg = errMsg
		t.mu.Unlock()
	}()
	return true
}

func (t *taskSlot) snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{
		"running": t.running,
		"kind":    t.kind,
		"results": t.results,
	}
	if !t.started.IsZero() {
		out["started_at"] = t.started
	}
	if t.running {
		out["elapsed_sec"] = int64(time.Since(t.started).Seconds())
	} else if !t.finished.IsZero() {
		out["finished_at"] = t.finished
		out["duration_sec"] = int64(t.finished.Sub(t.started).Seconds())
		out["ok"] = sumOK(t.results)
		out["fail"] = sumStatus(t.results, checkinlog.StatusFail)
		out["already"] = sumStatus(t.results, checkinlog.StatusAlready)
		if t.errMsg != "" {
			out["error"] = t.errMsg
		}
	}
	return out
}

func sumOK(rs []scheduler.CheckinResult) int {
	n := 0
	for _, r := range rs {
		if r.Status == checkinlog.StatusOK {
			n++
		}
	}
	return n
}

func sumStatus(rs []scheduler.CheckinResult, want string) int {
	n := 0
	for _, r := range rs {
		if r.Status == want {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
