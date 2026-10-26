package admin

// tokens.go —— 令牌（API Key）管理端点（Wave B4）。
//
// # 它和 /admin/apikeys 是什么关系
//
// /admin/apikeys（apikeys.go）是**JSON 时代**留下的那组端点，语义比 hyxapi 的令牌体系弱：
// 没有"创建数量"、没有有序分组优先级、没有自动分组策略、没有跨组重试、没有自查询量。
// 本文件按用户截图里「创建令牌」抽屉的字段把语义补齐；两者**共用同一张表**
// （internal/store 的 tokens；A2/B2 已把 apikey 的存储换成 SQL 真源），
// 所以不存在"两份真相"—— 只是同一行的两个视图。
//
// # 硬约束（写之前先读，别越界）
//
//	· 本文件**只调用** internal/store 与 internal/apikey 的导出 API，不改它们的实现。
//	· 明文 Key **只**在两条端点出现：POST /admin/tokens/{id}/key 与 POST /admin/tokens/batch/keys。
//	  其它任何响应一律走 store.TokenView（Masked()）。
//	· 路由注册在 admin.go 的 New() 里（一行 h.registerTokens()，主脑已授权只加这一行）。
//
// # 为什么 count>1 是本仓自己定的语义
//
// hyxapi 后端没有 count 字段（前端抽屉里有"创建数量"，但提交时被前端展开成多次调用）。
// 我们把它做成**后端原生**：等价于连续建 count 条，且为了给后缀留位置，
// 名称字节上限从 50 降到 43，第 2 条起追加 "-" + 6 位随机短码。
// 这样"一次批量建 50 条"不再需要 50 次 HTTP 往返，也不会出现"建到第 7 条失败，
// 前 6 条已经在库里但前端不知道"的黑箱 —— 部分成功必须如实报告（见 createdTokens）。

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/authlimit"
	"workbuddy2api/internal/store"
)

const (
	// tokenNameMaxBytes 单个令牌名称的字节上限（与 hyxapi 的 50 对齐）。
	tokenNameMaxBytes = 50
	// tokenBatchNameMaxBytes count>1 时的上限：50 - len("-") - 6 位短码 = 43。
	// 留出后缀空间，否则"名字本来就 50 字节"的那批会在第 2 条起静默截断。
	tokenBatchNameMaxBytes = 43
	// tokenCreateMaxCount 一次最多建多少条（与截图里的 1..1000 对齐）。
	tokenCreateMaxCount = 1000
	// tokenDeleteMaxCount 一次批量删除的上限。
	tokenDeleteMaxCount = 1000
	// tokenRevealBatchMax 批量取明文的上限（比删除小得多：这是敏感面）。
	tokenRevealBatchMax = 100
	// tokenAutoGroupMaxCount auto 模式下可选分组的条数上限。
	//
	// hyxapi 把这个上限放在全局配置里（auto_groups 的 max_count）。本仓现在没有那个配置项，
	// 所以**这里定一个常量并写在注释里**（而不是悄悄不限）：20 已经远超实际分组数
	// （本仓 13 个上游 + default），设它是为了挡住"提交一千个分组"这种把选择器撑爆的输入。
	tokenAutoGroupMaxCount = 20
	// tokenDefaultPageSize / tokenMaxPageSize 列表分页。
	tokenDefaultPageSize = 50
	tokenMaxPageSize     = 200
	// tokenRevealWindow / tokenRevealMaxPerWindow 明文端点的限流窗口与额度。
	//
	// 30 次/分钟：管理员手动"看一眼 key"足够用（真要看 100 条就走批量端点，
	// 批量端点自己还有 100 的上限），但脚本化地遍历整张表会在几十秒内被挡住。
	tokenRevealWindow       = time.Minute
	tokenRevealMaxPerWindow = 30
)

// tokenRevealLimiter 明文端点的限流器（进程内滑动窗口）。
//
// # 为什么用 authlimit 而不是自己写一个
//
// internal/authlimit 是本仓**既有**的限速工具（登录/注册在用）：滑动窗口、空 key 拒绝、
// 参数没给对时 fail-closed。敏感端点的限流最怕"静默不生效"，那套语义正好是我们要的。
//
// ⚠ 它是**进程内**的：多实例部署时每个实例各算各的（本仓是单进程网关，与 authlimit 的
// 既有定位一致）。要全局限流得换 Redis 版，那是另一个任务，不在这里假装做到。
var tokenRevealLimiter = authlimit.NewLimiter(tokenRevealWindow, tokenRevealMaxPerWindow)

// registerTokens 注册 /admin/tokens 系列端点（admin.go 的 New 里调用一次）。
//
// # 未启用令牌存储时**不注册**路由（而不是注册后回 503）
//
// 与 APIKeys/Channels/Groups 同一条约定：没注入依赖就不该出现会失败的按钮 ——
// 前端按 manifest 的 admin_routes 渲染操作入口，未注册 = 面板不渲染操作区。
// 判据比那三条宽：SQL 真源（store.DB）与老视图（cfg.APIKeys）任一可用即可。
func (h *Handler) registerTokens() {
	if h.cfg.APIKeys == nil && store.DB == nil {
		return
	}
	h.register("GET /admin/tokens", h.tokensList)
	h.register("GET /admin/tokens/{id}", h.tokensDetail)
	h.register("GET /admin/tokens/{id}/usage", h.tokensUsage)
	h.register("POST /admin/tokens", h.tokensCreate)
	h.register("POST /admin/tokens/update", h.tokensUpdate)
	h.register("POST /admin/tokens/delete", h.tokensDelete)
	// ── 敏感：只有这两条回明文 ──
	h.register("POST /admin/tokens/{id}/key", h.tokensRevealKey)
	h.register("POST /admin/tokens/batch/keys", h.tokensRevealKeys)
}

// ── 请求体 ────────────────────────────────────────────────────────────────

// flexStrings 既能吃 ["a","b"] 也能吃 "a,b"。
//
// 为什么需要它：截图里的"模型限制"是多选（前端可能发数组），而 hyxapi 的请求体里
// 同一个字段是逗号串；我们的前端历史上两种都发过。**接受两种形态**比"约定一种、
// 另一种静默变空"安全 —— 后者会让用户以为"限制了模型"，其实限制没生效。
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(raw []byte) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		*f = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []string
		if err := json.Unmarshal(raw, &arr); err != nil {
			return err
		}
		*f = arr
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err != nil {
		return err
	}
	*f = []string{one}
	return nil
}

// tokenCreateRequest 创建令牌的请求体（字段名与前端截图一致，下划线风格）。
type tokenCreateRequest struct {
	Name string `json:"name"`
	// Count 创建数量（1..1000）。缺省 1；hyxapi 后端没有这个字段，语义见包注释。
	Count int `json:"count"`
	// Automatic true = 自动分组（按可用分组轮询），false = 有序分组（Groups 顺序即优先级）。
	Automatic bool `json:"automatic"`
	// Groups 有序分组：**顺序就是优先级**（前端用可拖拽的 SortableMultiSelect 排的）。
	Groups []string `json:"groups"`
	// AutoGroups 自动分组的候选集合（仅 Automatic 且 AutoGroupMode == "custom" 时有效）。
	AutoGroups []string `json:"auto_groups"`
	// AutoGroupMode "inherit"（继承全局配置）或 "custom"（用 AutoGroups）。
	AutoGroupMode   string `json:"auto_group_mode"`
	CrossGroupRetry bool   `json:"cross_group_retry"`
	// GroupSwitchStatusCodes 触发换组的 HTTP 状态码；空 = store 的默认集合。
	GroupSwitchStatusCodes []int `json:"group_switch_status_codes"`
	GroupSwitchRetryCount  int   `json:"group_switch_retry_count"`
	UnlimitedQuota         bool  `json:"unlimited_quota"`
	RemainQuota            int   `json:"remain_quota"`
	// AllowUserQuotaQuery 允许此令牌查询账户余额（决定自查询量响应里带不带 user_quota）。
	AllowUserQuotaQuery bool `json:"allow_user_quota_query"`
	// ExpiredTime Unix 秒；**-1 = 永不**（与 hyxapi 一致）。缺省 0 会被归一成 -1。
	ExpiredTime        int64       `json:"expired_time"`
	ModelLimitsEnabled bool        `json:"model_limits_enabled"`
	ModelLimits        flexStrings `json:"model_limits"`
	// AllowIps 一行一个 IP（hyxapi 同款：换行分隔，不是逗号）。
	AllowIps string `json:"allow_ips"`
	UserID   int    `json:"user_id"`
}

// tokenUpdateRequest 更新令牌的请求体：全字段可改 + status_only 快车道。
type tokenUpdateRequest struct {
	ID int `json:"id"`
	// StatusOnly true 时只改状态（列表里的启用/禁用开关走这条路）。
	StatusOnly bool `json:"status_only"`
	Status     int  `json:"status"`

	tokenCreateRequest
}

// syncAuthIndex 把刚写过的令牌同步进**网关鉴权索引**（T81）。
//
// # 为什么每个写路径都必须调它（这是一次真实事故）
//
// 本文件的 CRUD **直写 tokens 表**（store.CreateToken / SaveToken /
// UpdateTokenStatus / DeleteToken），而网关的鉴权只问 apikey.Store.Validate ——
// 那个索引只在它接管 SQL 时全量重建一次。漏调的后果是：
//
//	/admin/tokens 建的令牌在**列表/详情/明文**上都好端端的，
//	打 /v1/chat/completions 却一律 401 invalid_api_key ——
//	CRUD 面全绿、只有真打一次才暴露（重启后又会"自己好"，因为重建了索引）。
//
// 参数是**行 id**：apikey.Store.SyncTokenByID 自己回读库里的权威行 —— 行在就建/更新，
// 行不在（含软删）就摘掉。所以新建/改名/改状态/改额度/删除/批量删都只需一句
// "我动过这些 id"，不必各自维护一份要同步的字段清单。
//
// ⚠ 写路径**新增**时必须跟着加一句，否则又会出现"CRUD 能写、网关不认"。
// 这条不变量有 Go 测试钉住：tokens_authsync_test.go 的 TestAdminCreatedTokenAuthenticatesOnGateway。
func (h *Handler) syncAuthIndex(ids ...int) {
	keys := h.apiKeysStore()
	if keys == nil || len(ids) == 0 {
		// 未注入 apikey.Store（如 handler 级单测）= 没有网关鉴权这一层，无索引可同步。
		return
	}
	if err := keys.SyncTokensByID(ids); err != nil {
		// 索引没跟上 ≠ 本次写入失败（数据已落库）。记日志：下一次鉴权会走
		// "未命中回库"兜底，所以这只是可观测性问题，不是数据问题。
		log.Printf("admin: 令牌写入后同步鉴权索引失败（下次鉴权会回库兜底）: %v", err)
	}
}

// ── 端点实现 ──────────────────────────────────────────────────────────────

// tokensList GET /admin/tokens?p=&page_size=&keyword=
//
// 返回掩码列表 + 总数。keyword 匹配 name / owner_id / key（key 用掩码匹配没意义，
// 但管理员可能粘贴了一整串 —— ParseTokenKey 会先剥前缀再匹配，见下）。
func (h *Handler) tokensList(w http.ResponseWriter, r *http.Request) {
	if h.cfg.APIKeys == nil && store.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "令牌存储未启用")
		return
	}
	page := atoiDefault(r.URL.Query().Get("p"), 1)
	if page < 1 {
		page = 1
	}
	size := atoiDefault(r.URL.Query().Get("page_size"), tokenDefaultPageSize)
	if size < 1 || size > tokenMaxPageSize {
		size = tokenDefaultPageSize
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))

	items, total, err := listTokenPage(keyword, (page-1)*size, size)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取令牌列表失败")
		return
	}
	// ⚠ 出口只有一个：MaskedTokens（任何绕过它的写法都可能把明文漏出去）。
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     store.MaskedTokens(items),
		"total":     total,
		"page":      page,
		"page_size": size,
	})
}

// tokensDetail GET /admin/tokens/{id} —— 掩码 + 全字段（分组解码成数组）。
func (h *Handler) tokensDetail(w http.ResponseWriter, r *http.Request) {
	tok, ok := h.tokenByPathID(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, tok.Masked())
}

// tokensUsage GET /admin/tokens/{id}/usage —— 自查询量（对标 hyxapi 的 /api/usage/token）。
//
// # 为什么 user_quota 是"有条件字段"
//
// 这把 key 可能发给第三方。默认不让它看见账户余额（AllowUserQuotaQuery 默认 false）；
// 管理员显式打开后才有 user_quota 段。所以这里用 map 而不是结构体 —— 结构体会把
// user_quota 序列化成 0，前端无法区分"余额是 0"和"没权限看"。
func (h *Handler) tokensUsage(w http.ResponseWriter, r *http.Request) {
	tok, ok := h.tokenByPathID(w, r)
	if !ok {
		return
	}
	expires := tok.ExpiredTime
	if expires < 0 {
		expires = 0 // -1 = 永不 ⇒ 对外统一成 0（hyxapi 同款映射）
	}
	out := map[string]any{
		"name":            tok.Name,
		"total_granted":   tok.RemainQuota + tok.UsedQuota,
		"total_used":      tok.UsedQuota,
		"total_available": tok.RemainQuota,
		"unlimited_quota": tok.UnlimitedQuota,
		"model_limits":    tok.GetModelLimits(),
		"expires_at":      expires,
	}
	if tok.AllowUserQuotaQuery {
		out["user_quota"] = tok.RemainQuota
	}
	writeJSON(w, http.StatusOK, out)
}

// tokensCreate POST /admin/tokens —— 建 1..1000 条。
func (h *Handler) tokensCreate(w http.ResponseWriter, r *http.Request) {
	var req tokenCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Count = normalizeCount(req.Count)
	if msg := validateTokenCreate(&req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	result, err := createTokens(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建令牌失败")
		return
	}
	// 新建的每一条都要进网关鉴权索引（否则 CRUD 能写、/v1/* 401，见 syncAuthIndex 注释）。
	createdIDs := make([]int, 0, len(result.Items))
	for _, it := range result.Items {
		if id, ok := it["id"].(int); ok {
			createdIDs = append(createdIDs, id)
		}
	}
	h.syncAuthIndex(createdIDs...)
	// 部分成功也用 200：请求本身没毛病，是其中某几条撞了唯一约束之类的运行期问题。
	// 用 4xx/5xx 会让前端把**已经建好的那些**当成"全失败"，那才是真的误导；
	// 前端据 result.partial / result.failed 决定怎么提示。
	writeJSON(w, http.StatusOK, result)
}

// tokensUpdate POST /admin/tokens/update —— 全字段可改；status_only 只切状态。
func (h *Handler) tokensUpdate(w http.ResponseWriter, r *http.Request) {
	var req tokenUpdateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID <= 0 {
		writeError(w, http.StatusBadRequest, "缺少 id")
		return
	}
	tok, err := store.GetTokenByID(req.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "令牌不存在")
		return
	}
	if req.StatusOnly {
		if req.Status != store.TokenStatusEnabled && req.Status != store.TokenStatusDisabled {
			writeError(w, http.StatusBadRequest, "status 只能是 1（启用）或 2（禁用）")
			return
		}
		if err := store.UpdateTokenStatus(req.ID, req.Status); err != nil {
			writeError(w, http.StatusInternalServerError, "更新状态失败")
			return
		}
		h.syncAuthIndex(req.ID) // 状态变更必须**立刻**影响鉴权
		tok.Status = req.Status
		writeJSON(w, http.StatusOK, tok.Masked())
		return
	}
	if msg := applyTokenUpdate(tok, &req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// ⚠ 分组归一化只走 store 的唯一入口（NormalizeGroupPolicy），不在这里自己拼：
	// 它同时负责 Group 编码串、AutoGroups、状态码集合三者的互相校验与排序。
	tok.NormalizeGroupPolicy()
	if err := store.SaveToken(tok); err != nil {
		writeError(w, http.StatusInternalServerError, "保存令牌失败")
		return
	}
	h.syncAuthIndex(tok.ID)
	writeJSON(w, http.StatusOK, tok.Masked())
}

// tokensDelete POST /admin/tokens/delete —— 单条或批量（上限 1000）。
func (h *Handler) tokensDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID  int   `json:"id"`
		IDs []int `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ids := req.IDs
	if req.ID > 0 {
		ids = append([]int{req.ID}, ids...)
	}
	ids = dedupeInts(ids)
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "缺少要删除的 id")
		return
	}
	if len(ids) > tokenDeleteMaxCount {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("一次最多删除 %d 条（收到 %d 条）", tokenDeleteMaxCount, len(ids)))
		return
	}
	deleted := 0
	deletedIDs := make([]int, 0, len(ids))
	for _, id := range ids {
		n, err := store.DeleteToken(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "删除令牌失败")
			return
		}
		deleted += int(n)
		if n > 0 {
			deletedIDs = append(deletedIDs, id)
		}
	}
	// 软删的行不会自动离开鉴权索引 ⇒ 删除必须同步，否则被删的令牌还能用。
	h.syncAuthIndex(deletedIDs...)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "requested": len(ids)})
}

// tokensRevealKey POST /admin/tokens/{id}/key —— **敏感**：返回单条明文。
func (h *Handler) tokensRevealKey(w http.ResponseWriter, r *http.Request) {
	if !h.allowTokenReveal(w, r) {
		return
	}
	tok, ok := h.tokenByPathID(w, r)
	if !ok {
		return
	}
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"id": tok.ID, "name": tok.Name, "key": tok.FullKey()})
}

// tokensRevealKeys POST /admin/tokens/batch/keys —— **敏感**：批量返回明文（上限 100）。
func (h *Handler) tokensRevealKeys(w http.ResponseWriter, r *http.Request) {
	if !h.allowTokenReveal(w, r) {
		return
	}
	var req struct {
		IDs []int `json:"ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ids := dedupeInts(req.IDs)
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "缺少要取明的 id")
		return
	}
	if len(ids) > tokenRevealBatchMax {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("一次最多取 %d 条明文（收到 %d 条）", tokenRevealBatchMax, len(ids)))
		return
	}
	type item struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	items := make([]item, 0, len(ids))
	missing := make([]int, 0)
	for _, id := range ids {
		// 同 tokenByPathID：不存在与已软删都是 (nil, nil)，都要算 missing。
		tok, err := store.GetTokenByID(id)
		if err != nil || tok == nil {
			missing = append(missing, id)
			continue
		}
		items = append(items, item{ID: tok.ID, Name: tok.Name, Key: tok.FullKey()})
	}
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "missing": missing})
}

// ── 辅助 ──────────────────────────────────────────────────────────────────

// allowTokenReveal 敏感端点的三道闸之一：限流。
//
// # 为什么 key 用"来源主机 + 端点"而不是只用一个全局桶
//
// 只用一个全局桶，一个刷子就能把管理员自己挡在门外（authlimit 的注释里点名了这个反模式）。
// 按来源分流后，攻击者只能花掉自己那一份。
//
// ⚠ 取不到来源（RemoteAddr 空）时 authlimit 会**拒绝**（fail-closed）——
// 这正是我们要的：宁可多一次 429，也不要给一条免检通道。所以这里不去"兜底造一个 key"。
func (h *Handler) allowTokenReveal(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ok, retryAfter := tokenRevealLimiter.Allow(host+"|"+r.URL.Path, time.Now())
	if ok {
		return true
	}
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "取明文过于频繁，请稍后再试")
	return false
}

// noStore 禁止任何一层缓存明文。
//
// no-store 而不是 no-cache：no-cache 允许"存下来但每次校验"，中间代理仍可能落盘；
// 明文 key 一旦进了代理缓存，就再也不受我们的 no-store 约束了。
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// tokenByPathID 解析 {id} 并取令牌；失败时已写好响应，调用方直接 return。
func (h *Handler) tokenByPathID(w http.ResponseWriter, r *http.Request) (*store.Token, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id 不合法")
		return nil, false
	}
	tok, err := store.GetTokenByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取令牌失败")
		return nil, false
	}
	// ⚠⚠ 必须判 nil：store.GetTokenByID 对"不存在 / **已软删**"两种情况都回 **(nil, nil)**
	// （它内部把 gorm.ErrRecordNotFound 吞成 nil 了，见 store/token.go:501-503）。
	// 少这一判的后果是 `200 null`：删掉的令牌取详情仍然"成功"，前端拿到 null 会
	// 静默渲染成空表单 —— 用户以为还在、再点保存就是新建一条。这条是测试抓出来的真缺陷。
	if tok == nil {
		writeError(w, http.StatusNotFound, "令牌不存在")
		return nil, false
	}
	return tok, true
}

// decodeJSON 读请求体；失败时写好 400 并返回 false。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON（或含未知字段）: "+err.Error())
		return false
	}
	return true
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func dedupeInts(in []int) []int {
	seen := make(map[int]bool, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if v <= 0 || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// normalizeCount 缺省 1，并把超范围的值原样留着交给校验报错（不要在这里静默夹紧：
// "我填了 5000，结果它悄悄建了 1000 条" 比直接报错难查得多）。
func normalizeCount(n int) int {
	if n == 0 {
		return 1
	}
	return n
}

// validateTokenCreate 逐条校验（与 hyxapi 对齐的那几条 + count 语义）。
func validateTokenCreate(req *tokenCreateRequest) string {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return "名称不能为空"
	}
	limit := tokenNameMaxBytes
	if req.Count > 1 {
		limit = tokenBatchNameMaxBytes
	}
	if len(name) > limit {
		return fmt.Sprintf("名称最长 %d 字节（当前 %d）", limit, len(name))
	}
	if req.Count < 1 || req.Count > tokenCreateMaxCount {
		return fmt.Sprintf("创建数量必须在 1..%d 之间", tokenCreateMaxCount)
	}
	if req.Automatic {
		if req.AutoGroupMode == "custom" {
			groups := normList(req.AutoGroups)
			if len(groups) == 0 {
				return "自定义自动分组至少要选 1 个分组"
			}
			if len(groups) > tokenAutoGroupMaxCount {
				return fmt.Sprintf("自动分组最多 %d 个（当前 %d）", tokenAutoGroupMaxCount, len(groups))
			}
		}
	} else if len(normList(req.Groups)) == 0 {
		return "有序分组模式下至少要选 1 个分组"
	}
	if req.GroupSwitchRetryCount < 0 {
		return "换组重试次数不能为负"
	}
	if msg := validateIPList(req.AllowIps); msg != "" {
		return msg
	}
	return ""
}

// validateIPList 一行一个 IP；允许 IPv4/IPv6 与 CIDR。
//
// # 为什么必须逐行校验而不是"存下来就好"
//
// 这个字段是**访问控制**：写错一个字符（比如把 10.0.0.0/8 写成 10.0.0.0/8 里混了空格）
// 会让白名单静默失效或者误放行。宁可 400 让用户改，也不要替他猜。
func validateIPList(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" {
			continue
		}
		if net.ParseIP(entry) != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(entry); err == nil {
			continue
		}
		return fmt.Sprintf("IP 白名单里这一行不是合法 IP 或网段：%s", entry)
	}
	return ""
}

// applyTokenUpdate 把请求里的字段覆盖到令牌上（返回错误文案，空串 = 通过）。
func applyTokenUpdate(tok *store.Token, req *tokenUpdateRequest) string {
	if strings.TrimSpace(req.Name) != "" {
		name := strings.TrimSpace(req.Name)
		if len(name) > tokenNameMaxBytes {
			return fmt.Sprintf("名称最长 %d 字节（当前 %d）", tokenNameMaxBytes, len(name))
		}
		tok.Name = name
	}
	if req.Status != 0 {
		if req.Status != store.TokenStatusEnabled && req.Status != store.TokenStatusDisabled {
			return "status 只能是 1（启用）或 2（禁用）"
		}
		tok.Status = req.Status
	}
	// 分组：Automatic 决定用 Groups 还是 AutoGroups，两者都写进 Group 编码串交给唯一入口归一。
	if req.Automatic {
		groups := normList(req.AutoGroups)
		if req.AutoGroupMode == "custom" {
			if len(groups) == 0 {
				return "自定义自动分组至少要选 1 个分组"
			}
			if len(groups) > tokenAutoGroupMaxCount {
				return fmt.Sprintf("自动分组最多 %d 个（当前 %d）", tokenAutoGroupMaxCount, len(groups))
			}
			if err := tok.SetAutoGroups(groups); err != nil {
				return "自动分组编码失败"
			}
			tok.Group = store.EncodeTokenGroups([]string{})
		} else {
			// inherit：清掉自定义快照，回到"跟全局配置走"。
			if err := tok.SetAutoGroups(nil); err != nil {
				return "自动分组编码失败"
			}
			tok.Group = store.EncodeTokenGroups([]string{})
		}
	} else if g := normList(req.Groups); len(g) > 0 {
		tok.Group = store.EncodeTokenGroups(g)
	}
	if req.GroupSwitchRetryCount > 0 || req.CrossGroupRetry {
		n := req.GroupSwitchRetryCount
		if n > 10 {
			n = 10 // 夹 0..10（与截图里的数字框一致）
		}
		if n < 0 {
			n = 0
		}
		tok.GroupSwitchRetryCount = n
	}
	tok.CrossGroupRetry = req.CrossGroupRetry
	if len(req.GroupSwitchStatusCodes) > 0 {
		tok.GroupSwitchCodes = store.NormalizeGroupSwitchStatusCodes(req.GroupSwitchStatusCodes)
	}
	tok.UnlimitedQuota = req.UnlimitedQuota
	if req.UnlimitedQuota {
		tok.RemainQuota = 0 // 不限额度时强制 0（hyxapi 同款）
	} else if req.RemainQuota > 0 {
		tok.RemainQuota = req.RemainQuota
	}
	tok.AllowUserQuotaQuery = req.AllowUserQuotaQuery
	if req.ExpiredTime != 0 {
		tok.ExpiredTime = req.ExpiredTime
	}
	if tok.ExpiredTime == 0 {
		tok.ExpiredTime = -1 // 0 视为"没填" ⇒ 永不（与 hyxapi 的 -1 一致）
	}
	tok.ModelLimitsEnabled = req.ModelLimitsEnabled
	if req.ModelLimits != nil {
		tok.ModelLimits = strings.Join(normList(req.ModelLimits), ",")
	}
	if strings.TrimSpace(req.AllowIps) != "" || req.AllowIps == "" {
		if msg := validateIPList(req.AllowIps); msg != "" {
			return msg
		}
		ips := strings.TrimSpace(req.AllowIps)
		tok.AllowIps = &ips
	}
	return ""
}

// createResult 创建结果：**部分成功必须可判定**（前端据此提示，不要靠猜）。
type createResult struct {
	Requested int              `json:"requested"`
	Created   int              `json:"created"`
	Failed    int              `json:"failed"`
	Partial   bool             `json:"partial"`
	Items     []map[string]any `json:"items"`
	Failures  []map[string]any `json:"failures"`
}

// createTokens 建 count 条；**部分成功如实报告**（不回滚已建的）。
func createTokens(req tokenCreateRequest) (*createResult, error) {
	base := strings.TrimSpace(req.Name)
	items := make([]map[string]any, 0, req.Count)
	failures := make([]map[string]any, 0)

	for i := 0; i < req.Count; i++ {
		name := base
		if i > 0 {
			suffix, err := shortTokenSuffix()
			if err != nil {
				failures = append(failures, map[string]any{"index": i + 1, "name": name, "error": "生成后缀失败"})
				continue
			}
			name = base + "-" + suffix
		}
		tok := &store.Token{
			Name:                  name,
			UserID:                req.UserID,
			Status:                store.TokenStatusEnabled,
			ExpiredTime:           req.ExpiredTime,
			RemainQuota:           req.RemainQuota,
			UnlimitedQuota:        req.UnlimitedQuota,
			ModelLimits:           strings.Join(normList(req.ModelLimits), ","),
			AllowUserQuotaQuery:   req.AllowUserQuotaQuery,
			Group:                 store.EncodeTokenGroups(normList(req.Groups)),
			CrossGroupRetry:       req.CrossGroupRetry,
			GroupSwitchRetryCount: clampRetry(req.GroupSwitchRetryCount),
		}
		if tok.ExpiredTime == 0 {
			tok.ExpiredTime = -1
		}
		if tok.UnlimitedQuota {
			tok.RemainQuota = 0
		}
		tok.ModelLimitsEnabled = req.ModelLimitsEnabled
		if req.AllowIps != "" {
			ips := req.AllowIps
			tok.AllowIps = &ips
		}
		if req.Automatic && req.AutoGroupMode == "custom" {
			if err := tok.SetAutoGroups(normList(req.AutoGroups)); err != nil {
				failures = append(failures, map[string]any{"index": i + 1, "name": name, "error": "自动分组编码失败"})
				continue
			}
		}
		if len(req.GroupSwitchStatusCodes) > 0 {
			tok.GroupSwitchCodes = store.NormalizeGroupSwitchStatusCodes(req.GroupSwitchStatusCodes)
		}
		tok.NormalizeGroupPolicy()
		if err := store.CreateToken(tok); err != nil {
			// ⚠ 不回滚：已经建好的那些是用户要的东西，删掉它们才是更坏的结果。
			failures = append(failures, map[string]any{"index": i + 1, "name": name, "error": err.Error()})
			continue
		}
		items = append(items, map[string]any{"id": tok.ID, "name": tok.Name, "key": tok.MaskedKey()})
	}
	return &createResult{
		Requested: req.Count,
		Created:   len(items),
		Failed:    len(failures),
		Partial:   len(failures) > 0 && len(items) > 0,
		Items:     items,
		Failures:  failures,
	}, nil
}

func clampRetry(n int) int {
	if n < 0 {
		return 0
	}
	if n > 10 {
		return 10
	}
	return n
}

// shortTokenSuffix 6 位随机短码（复用 store 的凭据级随机源，不自己引 math/rand）。
//
// 不做"库里查重"：48 位随机串里取 6 位，撞了就是 1/56e9；为这个概率加一次查询不划算。
// 真撞了也只会被 uniqueIndex 拦下（Key 唯一），表现为那一条 failures —— 会被如实报出来。
func shortTokenSuffix() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 6)
	for i := range out {
		out[i] = alphabet[int(buf[i%4])%len(alphabet)]
	}
	return string(out), nil
}

func normList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		// 逗号串与数组形态都要能过（flexStrings 已统一成数组，但这里的入参也可能是
		// 单元素里塞了逗号串 —— 例如前端把多选序列化成了 "a,b"）。
		for _, part := range strings.Split(raw, ",") {
			v := strings.TrimSpace(part)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// listTokenPage 列表 + 关键字搜索。
//
// # 为什么关键字搜索直接落 SQL 而不是"取一页再过滤"
//
// "取一页再过滤"会得到**假结果**：过滤后这一页可能只剩 2 条，而总数仍是全表 ——
// 前端会显示"共 500 条，每页 2 条"。要么分页是错的，要么计数是错的，两者都不能接受。
//
// ⚠ 这里直接用 store.DB（导出全局）跑查询：store 目前没有"带关键字的分页接口"。
// 建议后续给 store 加 ListTokensPage(query) —— 那时这个函数应改成调用它，
// 别让 SQL 散在 admin 层（已在回报里提为改进项）。
func listTokenPage(keyword string, offset, limit int) ([]store.Token, int64, error) {
	db := store.DB
	if db == nil {
		// 没有 SQL 真源时退化成全量列举（老 JSON 路径仍要能用）。
		items, err := store.ListTokens(offset, limit)
		if err != nil {
			return nil, 0, err
		}
		total, err := store.CountTokens()
		return items, total, err
	}
	q := db.Model(&store.Token{})
	if keyword != "" {
		like := "%" + keyword + "%"
		// key 也可能被整串粘进来（sk-xxx）：ParseTokenKey 先剥前缀，按库内 Key 匹配。
		parsed := store.ParseTokenKey(keyword)
		q = q.Where("name LIKE ? OR owner_id LIKE ? OR key LIKE ?", like, like, "%"+parsed+"%")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []store.Token
	if err := q.Order("id DESC").Offset(offset).Limit(limit).Find(&items).Error; err != nil {
		return nil, 0, err
	}
	return items, total, nil
}
