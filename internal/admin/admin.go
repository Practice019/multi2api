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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/clientlogin"
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

	// Business 上游的账号级业务能力（成长/旅行/额度刷新）。
	// nil = 该上游没有这些能力，相关面板降级为报错而非崩。
	//
	// 用消费方接口而不是具体类型：admin **不认识** workbuddy，
	// 加第二个上游时本包零改动（见 upstream_jobs.go）。
	Business UpstreamBusiness

	// ResetModelsCache 清空模型目录缓存（由 server 包注入，避免 admin 反向依赖 server）。
	ResetModelsCache func()
	// ModelCatalog 取模型目录快照（成本系数来源）。nil = 未接线，统计里报 unavailable。
	//
	// 由 cmd/server 注入 server.(*Handler).ModelCatalog —— 它内部按 1h TTL / 5min 失败
	// 负缓存惰性回源上游。这里只当「取数函数」用，缓存与重试策略全部留在 server 包。
	ModelCatalog func() *upstream.ModelCatalog
	// ModelCatalogState 只读的目录缓存状态（ok/stale/unavailable）。nil = 未接线。
	// 与 ModelCatalog 分开注入是刻意的：状态查询**绝不能**触发上游请求，
	// 而取目录会。把两者混成一个函数，就没法在 /admin/stats 里安全地只要状态。
	ModelCatalogState func() ModelCatalogState
	// Settings 设置页的读写契约（由 cmd/server 实现并注入；nil = 关闭设置页）。
	Settings SettingsStore
	// ClientLogin 本机客户端登录态管理（nil = 关闭「本地登录」面板）。
	ClientLogin *clientlogin.Manager
	// BuildTime 进程启动时间，UI 用来算运行时长。
	StartedAt time.Time
}

// Handler 管理台路由（/admin/ 子树）。
type Handler struct {
	cfg  Config
	mux  *http.ServeMux
	task *taskSlot

	// 统计聚合的短缓存：聚合要扫整个落盘文件，而前端按轮询节奏调用，
	// 缓存让「扫盘频率」与「轮询频率」解耦（见 statsCacheTTL）。
	statsMu     sync.Mutex
	statsCache  []logbuf.Entry
	statsSource string
	statsErr    error
	statsAt     time.Time
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

	h.mux.HandleFunc("GET /admin/travel", h.travelList)
	h.mux.HandleFunc("GET /admin/travel/status", h.travelStatus)
	h.mux.HandleFunc("POST /admin/travel/depart", h.travelDepart)
	h.mux.HandleFunc("POST /admin/travel/claim", h.travelClaim)

	h.mux.HandleFunc("GET /admin/growth", h.growthList)
	h.mux.HandleFunc("POST /admin/growth/claim", h.growthClaim)
	h.mux.HandleFunc("POST /admin/growth/accept", h.growthAccept)
	h.mux.HandleFunc("POST /admin/growth/redeem", h.growthRedeem)
	h.mux.HandleFunc("POST /admin/growth/makeup", h.growthMakeup)
	h.mux.HandleFunc("POST /admin/growth/open", h.growthOpen)
	h.mux.HandleFunc("POST /admin/growth/draw", h.growthDraw)
	h.mux.HandleFunc("GET /admin/growth/tasks", h.growthTasks)
	h.mux.HandleFunc("GET /admin/growth/travel/config", h.growthTravelConfig)

	h.mux.HandleFunc("GET /admin/schedule", h.schedule)

	h.mux.HandleFunc("POST /admin/models/refresh", h.modelsRefresh)
	h.mux.HandleFunc("GET /admin/models/preview", h.modelsPreview)

	h.mux.HandleFunc("GET /admin/logs", h.logs)
	h.mux.HandleFunc("GET /admin/logs/history", h.logsHistory)
	h.mux.HandleFunc("GET /admin/stats", h.stats)
	h.mux.HandleFunc("GET /admin/checkin/history", h.history)
	h.mux.HandleFunc("GET /admin/task", h.taskStatus)

	h.mux.HandleFunc("GET /admin/settings", h.settings)
	h.mux.HandleFunc("PUT /admin/settings", h.settingsUpdate)

	// 本地客户端登录态：唯一会改写客户端本机状态的接口，全部要求显式 confirm。
	h.mux.HandleFunc("GET /admin/client-login", h.clientLoginStatus)
	h.mux.HandleFunc("POST /admin/client-login/switch", h.clientLoginSwitch)
	h.mux.HandleFunc("POST /admin/client-login/restore", h.clientLoginRestore)

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
// ModelCatalogState 模型目录缓存的只读状态视图（由 server 包注入的实现填充）。
//
// 三态语义（ok / stale / unavailable）刻意不做成 bool：
// 「有目录但已过 TTL」和「压根没拿到目录」对读报表的人是两件事 ——
// 前者可以继续用（数值可能不准），后者必须显示成缺数据。
type ModelCatalogState struct {
	State     string `json:"state"`      // ok | stale | unavailable
	Models    int    `json:"models"`     // 目录内模型数
	Stale     bool   `json:"stale"`      // 数据存在但已超出 TTL
	Cooldown  bool   `json:"cooldown"`   // 正处于失败负缓存冷却期
	FetchedAt string `json:"fetched_at"` // 最近一次成功拉取时间（RFC3339；无则空）
}

// ModelMultiplier 单模型的成本系数视图（credit 的放大倍数，来自 /v3/config）。
type ModelMultiplier struct {
	Model      string  `json:"model"`
	Multiplier float64 `json:"multiplier"`
	// Calls 该模型在本次聚合窗口内的调用次数（来自 by_model），
	// 让前端能在同一条 chip 上同时说清「调了多少次」和「每次贵多少倍」。
	Calls int `json:"calls"`
}

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
	if h.cfg.Business != nil {
		h.cfg.Business.RunTravelManual()
	}
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
	if h.cfg.Business == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无额度刷新能力")
		return
	}
	if body.UID != "" {
		res, ok := h.cfg.Business.RefreshCredits(body.UID, "manual")
		if !ok {
			writeError(w, http.StatusNotFound, "账号不存在: "+body.UID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"mode": "single", "result": toCheckinResult(res)})
		return
	}
	out := []scheduler.CheckinResult{}
	for _, st := range h.cfg.Pool.List() {
		if res, ok := h.cfg.Business.RefreshCredits(st.UID, "manual"); ok {
			out = append(out, toCheckinResult(res))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mode": "all", "results": out})
}

// toCheckinResult 把上游的额度刷新结果转成调度器通用结果类型。
//
// 转换只在这一处做：上游不必 import scheduler，admin 也不必认识上游的类型。
func toCheckinResult(res CheckinView) scheduler.CheckinResult {
	return scheduler.CheckinResult{
		UID:      res.UID,
		Status:   res.Status,
		Detail:   res.Detail,
		Credits:  res.Credits,
		HasQuota: res.HasQuota,
	}
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

// travelList 账号级旅行列表：直接读守卫维护的内存快照，不发上游请求。
// refresh=1 时强制全量回源一次（对应界面上的「刷新」按钮）。
func (h *Handler) travelList(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无旅行能力")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		b.RefreshTravel(true, false)
	}
	snaps := b.TravelSnapshots()
	// 池里有、但快照还没建起来的账号补一个空行，避免界面缺行让人以为是 bug。
	seen := map[string]bool{}
	for _, s := range snaps {
		seen[s.UID] = true
	}
	for _, st := range h.cfg.Pool.List() {
		if seen[st.UID] {
			continue
		}
		snaps = append(snaps, TravelSnapshot{
			UID: st.UID, Nickname: st.Nickname, Error: "尚未探测（点「刷新」）",
		})
	}
	// 把「自动派送挂在签到时点上」这件事所需的事实一并返回，让界面能显示真实时点
	// 而不是写死一句说明：签到时点可被设置页改，写死就会与实际不一致。
	checkinHours, _ := h.cfg.Scheduler.Hours()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":         snaps,
		"auto_claim":       b.TravelAutoClaimEnabled(),
		"auto_depart":      h.cfg.Scheduler.CheckinEnabled(),
		"checkin_hours":    checkinHours,
		"location_id":      4,
		"watch_interval_s": int64(b.WatchInterval().Seconds()),
	})
}

// 刻意没有 POST /admin/travel/auto：自动领奖的开/关同样只经 PUT /admin/settings，
// 理由与上面的 schedule/toggle 一致。

// travelStatus 单账号详细状态（含猫档案全字段），供界面展开查看。
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

// travelDepart 派猫。uid 为空 = 全部账号（后台任务，含账号间限速）。
func (h *Handler) travelDepart(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无旅行能力")
		return
	}
	body := decodeBody(r)

	if body.UID != "" {
		res := b.TravelDepartFor(body.UID, "manual")
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

	if !h.task.start("travel-depart", func() []scheduler.CheckinResult {
		return h.travelAll(func(uid string) TravelActionResult {
			return b.TravelDepartFor(uid, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// travelClaim 领奖。uid 为空 = 全部账号（后台任务）。
func (h *Handler) travelClaim(w http.ResponseWriter, r *http.Request) {
	b := h.cfg.Business
	if b == nil {
		writeError(w, http.StatusNotImplemented, "当前上游无旅行能力")
		return
	}
	body := decodeBody(r)

	if body.UID != "" {
		res := b.TravelClaimFor(body.UID, "manual")
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

	if !h.task.start("travel-claim", func() []scheduler.CheckinResult {
		return h.travelAll(func(uid string) TravelActionResult {
			return b.TravelClaimFor(uid, "manual")
		})
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"mode": "all", "started": true})
}

// travelAll 对全部非禁用账号跑同一旅行动作，转成 CheckinResult 供任务槽统一呈现。
// 跳过不写入结果列表（否则「全部派猫」会返回一堆 no-op 行），但已经由上游记进历史。
func (h *Handler) travelAll(fn func(uid string) TravelActionResult) []scheduler.CheckinResult {
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
			UID:     res.UID,
			Status:  res.Status,
			Detail:  res.Detail,
			Credits: res.Credits,
		})
	}
	return out
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

// 刻意没有 POST /admin/schedule/toggle：签到/保活的启停只能经 PUT /admin/settings，
// 那条路径会同时写 config.json 并应用运行时值。若另开一个只改内存的开关接口，
// 会出现「开关改了但重启后被 config 覆盖」的两条写路径冲突。

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

// modelsPreview 返回**全部目录模型**的 id 与成本倍率，供前端在模型名称旁标注倍率。
//
// 为什么不把倍率塞进 /v1/models：
//   - 那是**对外**的 OpenAI 兼容端点。客户端按规范解析 `data[]`，
//     塞自定义字段属于污染公共契约（部分客户端会对未知字段告警，甚至有严格模式直接报错）。
//   - 倍率是网关自己的观测信息，只对本地管理界面有意义 —— 该走 admin 面。
//
// 与 /admin/stats 里的 model_multipliers 的分工（两者都保留）：
//   - stats 那份只含**窗口内被调用过**的模型 —— 报表口径，回答"钱花在哪"
//   - 这份是**全部目录模型** —— 选择口径，回答"这个模型多贵"，下拉框需要给所有选项标注
//
// 降级：未接线或目录拿不到时返回**空数组**而不是报错 ——
// 前端拿不到倍率就只显示模型名，不该让整个模型列表渲染失败。
func (h *Handler) modelsPreview(w http.ResponseWriter, r *http.Request) {
	out := []ModelMultiplier{}
	if h.cfg.ModelCatalog != nil {
		if cat := h.cfg.ModelCatalog(); cat != nil {
			for _, m := range cat.Models {
				if m.ID == "" {
					continue
				}
				// 与 stats 那份不同：这里**保留 0 倍率**。
				// x0.00 是"免费"这个有意义的事实，不是"没有数据"。
				out = append(out, ModelMultiplier{Model: m.ID, Multiplier: m.Multiplier})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
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

// logsHistory 从落盘文件读历史请求日志（进程重启后仍可回溯）。
//
// 支持 offset/limit 分页，契约与 /admin/checkin/history 一致：
// 时间倒序（最新在前），返回过滤后总数，前端用同一个分页组件接两个数据源。
func (h *Handler) logsHistory(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil || h.cfg.Ring.Sink() == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}, "enabled": false})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = logbuf.DefaultPageSize
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	items, total, err := h.cfg.Ring.Sink().Page(offset, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取日志文件失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":   items,
		"total":   total,
		"offset":  offset,
		"limit":   limit,
		"enabled": true,
		"file":    h.requestLogStats(),
	})
}

// statsCacheTTL 统计聚合结果的缓存时长。
// 聚合要扫整个落盘文件，而前端是轮询调用；缓存让「扫描频率」与「轮询频率」解耦。
const statsCacheTTL = 5 * time.Second

// stats 汇总请求日志，给出「这个网关到底跑了多少、跑得怎么样」的只读视图。
//
// 数据源优先级：**落盘日志** > 内存环形缓冲。
// 历史实现只读内存缓冲（上限 2000 条），于是重启后立刻显示「暂无请求」，
// 且窗口被限制在「进程启动至今」——那既不是全部历史，也不是用户以为的统计范围。
// 现在改为聚合落盘文件，重启后依然覆盖全部保留期内的历史；
// 落盘不可用（未启用/读失败）时回落到内存缓冲，并在响应里说明当前范围。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Ring == nil {
		writeJSON(w, http.StatusOK, map[string]any{"total": 0, "source": "none"})
		return
	}

	items, source, err := h.statsItems()
	resp := aggregateChatLog(items)
	resp["source"] = source
	resp["capacity"] = h.cfg.Ring.Cap()
	resp["held"] = h.cfg.Ring.Len()
	// aggregated 显式说明「本次聚合了多少条」，与 total 语义不同：
	//   total      窗口内命中某种条件的条数（aggregateChatLog 里就是 len(items)）
	//   aggregated 实际参与聚合的条数
	// 正常情况两者相等。分开暴露是为了让「显示的窗口 ≠ 实际聚合窗口」这类
	// 静默偏差可见 —— 曾经 statsItems 用 Page(0,1<<20) 想取全量却被分页上界
	// 夹到 300，界面仍显示「全部落盘历史，共 300 条」，看不出是截断。
	resp["aggregated"] = len(items)
	if err != nil {
		// 读盘失败时把原因带上，避免用户对着「总数变小」猜原因。
		resp["error"] = "读取落盘日志失败，已回落到内存缓冲: " + err.Error()
	}
	if f := h.requestLogStats(); f != nil {
		resp["file"] = f
		// 自检：聚合条数与文件总条数不一致时明确标注，不静默。
		if n, ok := f["count"].(int); ok && source == "file" && n != len(items) {
			resp["truncated"] = true
			resp["error"] = fmt.Sprintf(
				"聚合窗口不完整：文件 %d 条，本次只聚合了 %d 条", n, len(items))
		}
	}
	// 成本系数与目录状态（B1.2）：放在同一个响应里而不是新开端点。
	//
	// 为什么不新开 /admin/models/catalog：
	//   - 前端渲染「调用统计」时**同时**需要 by_model（调用次数）与 multiplier（每次多贵），
	//     拆成两个端点会让同一次渲染出现「系数到了但次数还没到」的中间态，
	//     还会多一次轮询往返（仪表盘本来就是 5s 轮询）；
	//   - 两个数据源的生命周期不同（日志聚合 5s 缓存 vs 目录 1h TTL），
	//     但**读口径统一**：都是「展示现状」，没有写语义，合并不引入权限/一致性问题；
	//   - 失败降级是局部的：目录拿不到只让 model_catalog.state=unavailable，
	//     其余统计字段照常返回（不会被 5xx 连坐）。
	// 真正需要独立端点的是「刷新目录」这种**写**动作 —— 那已经由 /admin/models/refresh 承担。
	// 顺序很重要：**先**取倍率（可能触发一次惰性回源），**再**读状态。
	// 反过来会让首次响应自相矛盾 —— 倍率已经拿到了，state 却还报 unavailable。
	//
	// ⚠ 已知代价（评审指出，见代码注释中的 HIGH-2）：ModelCatalog() 可能同步阻塞
	// 至多 MaxRotate 次上游请求。若不接受，正确做法是把回源挪到后台 goroutine、
	// 本接口只读缓存 —— 那会改变「首次点击需要等一次拉取」的现行行为，
	// 是产品取舍而非 bug 修复，故留待专门决定（详见 tasks/board.md）。
	resp["model_multipliers"] = h.modelMultipliers(resp)
	resp["model_catalog"] = h.modelCatalogState()
	writeJSON(w, http.StatusOK, resp)
}

// modelCatalogState 取目录缓存状态；未接线或实现返回零值时统一归到 unavailable。
//
// 与 modelMultipliers 分开是因为调用时机不同：状态每次统计都要报（哪怕系数表没取到），
// 而系数表要按 by_model 过滤，且允许整体缺失。
func (h *Handler) modelCatalogState() ModelCatalogState {
	if h.cfg.ModelCatalogState == nil {
		return ModelCatalogState{State: "unavailable"}
	}
	st := h.cfg.ModelCatalogState()
	if st.State == "" {
		// 实现方漏填状态：按最保守的语义处理，不假装有数据。
		st.State = "unavailable"
	}
	return st
}

// modelMultipliers 返回**在本次统计窗口里真的被调用过**的模型的成本系数。
//
// 为什么要按 by_model 过滤，而不是把整个目录倒出来：
// 目录有 30 个模型，而一个网关上通常只跑其中几个；把没调用过的模型也塞进响应，
// 前端就得自己判断「哪些 chip 该显示」——而那正是这份数据存在的理由。
// 过滤后「有 chip = 有调用」，前端不需要第二套判断。
//
// 只输出**窗口里出现过**且**目录里有系数**的模型：报表要回答的是"钱花在哪"，
// 不是"目录里有什么"。
//
// 触发上游的边界（三条），都在 ModelCatalog 内部自限：
//
//  1. 窗口里没有任何 by_model 时直接跳过 —— 没调过模型就没什么可归因的；
//  2. 单测/未接线（nil）时直接返回空切片；
//  3. 其余情况交给 ModelCatalog()，它自带 1h TTL + 5min 失败负缓存，
//     自己决定要不要真的回源。
//
// ⚠ **不要**在这里按 state.State == "unavailable" 提前返回。
// 曾经这么写过，导致功能无法自举 —— 自锁死循环：
//
//	state 在缓存为空时返回 "unavailable"
//	→ 这里提前返回，从不调用 ModelCatalog()
//	→ 缓存永远为空 → state 永远 unavailable
//
// 冷启动时目录**永远不会**被拉取。而当时的测试全把 state 桩成 "ok"，
// 唯一直接调 ModelCatalog() 的 e2e 又绕开了 /admin/stats，所以全绿但功能是死的
// （由独立评审发现，回归测试见 catalog_bootstrap_test.go）。
//
// 正确做法：把"要不要回源"的决策权交给 ModelCatalog 自己 ——
// 它才知道自己是不是在冷却期、缓存是否过期。状态只用于**展示**，不用于决策。
func (h *Handler) modelMultipliers(resp map[string]any) []ModelMultiplier {
	out := []ModelMultiplier{}
	byModel, ok := resp["by_model"].(map[string]int)
	if !ok || len(byModel) == 0 || h.cfg.ModelCatalog == nil {
		return out
	}
	cat := h.cfg.ModelCatalog()
	if cat == nil {
		return out
	}
	// 按模型名排序输出：map 遍历顺序随机，固定顺序让响应可 diff（与目录排序同一动机）。
	names := make([]string, 0, len(byModel))
	for id := range byModel {
		names = append(names, id)
	}
	sort.Strings(names)
	for _, id := range names {
		m, ok := cat.Multiplier(id)
		if !ok {
			// 目录里没有这个模型（或系数为 0）：不输出条目，而不是输出 0 ——
			// 0 系数在前端会显示成「免费」，而真相是「不知道」。
			continue
		}
		out = append(out, ModelMultiplier{Model: id, Multiplier: m, Calls: byModel[id]})
	}
	return out
}

// statsItems 取出用于聚合的条目及其来源标识。
// 带 TTL 缓存：聚合是 O(文件行数)，而调用方是轮询。
func (h *Handler) statsItems() ([]logbuf.Entry, string, error) {
	h.statsMu.Lock()
	if !h.statsAt.IsZero() && time.Since(h.statsAt) < statsCacheTTL {
		items, err := h.statsCache, h.statsErr
		h.statsMu.Unlock()
		if err != nil {
			return items, "memory", err
		}
		return items, h.statsSource, nil
	}
	h.statsMu.Unlock()

	var (
		items  []logbuf.Entry
		source = "memory"
		err    error
	)
	if sink := h.cfg.Ring.Sink(); sink != nil {
		// 用 LoadAll 而不是 Page(0, sinkCountAll)：
		// Page 会按 MaxPageSize 夹紧 limit，而它分页上界是给 HTTP 调用方设的。
		// 曾经这里传 1<<20 想取全量，被静默夹到 300 —— 一份 1855 行的日志
		// 只聚合了最近 300 条，界面上的成功率/平均 TTFB 全是那 300 条的。
		// 内部聚合走旁路，不受分页上界约束（见 logbuf.Sink.LoadAll 的注释）。
		if all, e := sink.LoadAll(); e != nil {
			err = e
		} else if len(all) > 0 {
			items, source = all, "file"
		} else {
			source = "file" // 文件存在但为空：如实说是文件来源，而不是内存
		}
	}
	if source == "memory" {
		items, _ = h.cfg.Ring.Snapshot(0)
	}

	h.statsMu.Lock()
	h.statsCache, h.statsSource, h.statsErr, h.statsAt = items, source, err, time.Now()
	h.statsMu.Unlock()
	return items, source, err
}

// cacheHitRate 缓存命中率 = 命中 / (命中 + 未命中)。
//
// 分母为 0 时返回 (0, false)：调用方据此决定「输出 0」还是「不输出该字段」。
// 之所以必须显式挡掉 0/0，是因为 IEEE-754 下 float64(0)/float64(0) = NaN，
// NaN 经 encoding/json 序列化成 JSON 字面量 `null`（Marshal 对 NaN/Inf 返回
// 不支持值错误，writeJSON 里 `raw, _ :=` 把错误吞掉后会写出一个空 body）——
// 前端拿到 null 再做算术就是 NaN，页面上会出现 "NaN%"。
// 宁可少一个字段，也不要让「没有缓存数据」伪装成一个数值。
func cacheHitRate(hit, miss int64) (float64, bool) {
	den := hit + miss
	if den <= 0 {
		return 0, false
	}
	return float64(hit) / float64(den), true
}

// aggregateChatLog 对条目做纯聚合（无 IO），便于单测直接覆盖算术。
//
// 新增的 usage 派生字段（Credit/ThinkTokens/CacheHitTokens/CacheMissTokens）
// 一律只在 >0 时累加：旧格式行没有这些 JSON 键，反序列化后是 Go 零值，
// 天然贡献 0；-0.0 / 负数这类病态值（理论上解析层已挡掉）也和 0 等价，
// 不会污染其它聚合量（它们各走各的累加器，唯一的交汇点是 cache_hit_rate 的分母，
// 而那里对 <=0 有显式兜底）。
func aggregateChatLog(items []logbuf.Entry) map[string]any {
	byModel := map[string]int{}
	byStatus := map[string]int{}
	byUID := map[string]int{}
	var okN, failN, tokens int64
	var ttfbSum, totalSum int64
	var ttfbN int64
	var credit float64
	var thinkTokens, cacheHitTokens, cacheMissTokens int64
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
		// Credit 用 >0 而不是 !=0：负数/负零属于上游异常数据，
		// 让它进入累加会把「累计消耗」变成负值，比丢弃它更难解释。
		if e.Credit > 0 {
			credit += e.Credit
		}
		if e.ThinkTokens > 0 {
			thinkTokens += int64(e.ThinkTokens)
		}
		if e.CacheHitTokens > 0 {
			cacheHitTokens += int64(e.CacheHitTokens)
		}
		if e.CacheMissTokens > 0 {
			cacheMissTokens += int64(e.CacheMissTokens)
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
		"total":             len(items),
		"ok":                okN,
		"fail":              failN,
		"by_model":          byModel,
		"by_status":         byStatus,
		"by_uid":            byUID,
		"tokens":            tokens,
		"credit_total":      credit,
		"think_tokens":      thinkTokens,
		"cache_hit_tokens":  cacheHitTokens,
		"cache_miss_tokens": cacheMissTokens,
	}
	// cache_hit_rate 只在分母 >0 时输出（空集、全为旧格式行、或只有 miss 也为 0 时都不输出）：
	// 前端据此显示「—」而不是一个 0%，避免把「没有数据」读成「命中率真的是 0」。
	if r, ok := cacheHitRate(cacheHitTokens, cacheMissTokens); ok {
		resp["cache_hit_rate"] = r
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
	return resp
}

// history 任务历史（签到/保活/旅行/积分/成长）。
//
// 支持 offset/limit 分页，契约与 /admin/logs/history 一致：
// 时间倒序（最新在前），total 是**过滤后**总数，前端据此算总页数。
func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Log == nil {
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
	items, total := h.cfg.Log.Page(offset, limit, r.URL.Query().Get("kind"))
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"offset": offset,
		"limit":  limit,
	})
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
