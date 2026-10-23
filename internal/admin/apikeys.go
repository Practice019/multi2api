// apikeys.go —— /admin/apikeys 管理端点（对标 new-api 令牌管理，单机简化版）。
//
// # 为什么端点长在核心 admin 而不是上游
//
// API key 是**网关自身的调用方凭据**，与任何上游无关 —— 就像 /admin/settings
// 一样是通用能力。放在核心段（register 在 mountUpstreamRoutes 之前），
// 上游即便声明同名路由也无法覆盖。
package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"workbuddy2api/internal/account"
	"workbuddy2api/internal/apikey"
)

// apiKeysStore 本包持有的存储（Config.APIKeys 注入，nil = 未启用）。
func (h *Handler) apiKeysStore() *apikey.Store {
	return h.cfg.APIKeys
}

// registerAPIKeys 注册 /admin/apikeys 系列端点（New 里调用）。
//
// # 未启用时**不注册**路由（而不是注册后 501）
//
// 与 Settings/Log 同一条判据：没注入依赖就不该出现会失败的按钮。
// 前端按 manifest 的 admin_routes 渲染操作入口 —— 未注册 = 面板不渲染操作区。
func (h *Handler) registerAPIKeys() {
	h.register("GET /admin/apikeys", h.apiKeysList)
	h.register("POST /admin/apikeys", h.apiKeysCreate)
	h.register("POST /admin/apikeys/toggle", h.apiKeysToggle)
	h.register("POST /admin/apikeys/delete", h.apiKeysDelete)
	h.register("POST /admin/apikeys/reset", h.apiKeysReset)
	h.register("POST /admin/apikeys/update", h.apiKeysUpdate)
	h.register("POST /admin/apikeys/rotate-admin", h.apiKeysRotateAdmin)
	h.register("POST /admin/apikeys/rotate-api-key", h.apiKeysRotateAPIKey)
}

// apiKeysList GET /admin/apikeys —— 全部 key 的掩码视图 + 管理钥匙掩码。
func (h *Handler) apiKeysList(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []apikey.View{}, "enabled": false})
		return
	}
	// 两把钥匙的掩码分别回传（它们**互相独立**）：
	//
	//	admin_key   对话接口的 api_key（不限额，给调用方）
	//	admin_token 管理台门禁令牌（进控制台）
	//
	// ⚠ 为什么必须都回：前端要能分别显示"我用的是哪一把、轮换的是哪一把"。
	// 改造前只有 admin_key，而那时它同时是门禁 —— 一个字段承担两个语义，
	// 正是用户报的"门禁 token 不能和 api 一样"。
	// T8：**按身份取列表** —— 成员只看得到自己名下的 key。
	//
	// 判据住在存储层（apikey.ListFor）：它先判身份再看归属，空 owner + 非管理员
	// 直接回空表。见那里的长注释 —— 那个"空 owner 也是空串"的坑是本次最危险的
	// 一处（写反了就把管理员的 key 整列表端给成员）。
	p := h.principalFrom(r)
	keys := s.ListFor(p.UserID, p.IsAdmin())

	// 两把**管理**钥匙的掩码只回给管理员。
	//
	// ⚠ 成员连掩码都不该看到：掩码带可辨认前缀（用于"我现在用的是哪一把"），
	// 而 admin_token 是管理台门禁凭据 —— 让成员知道它存在、长什么样没有任何
	// 好处（他不能用，也不该用）。
	adminMask, adminTokenMask := "", ""
	// ⚠ 2026-10-20 用户口径：**登录 = 唯一门槛**；登录之后控制台要照旧"一切正常" ——
	// 包括看得到、可复制完整凭据，以及模型目录直接用这把 api_key 自动取回来
	//（不该再让用户在顶栏手粘一次）。
	//
	// 做法是在**会话鉴权之后**回吐完整值，而不是"把凭据注回页面 HTML"：
	//   · 本请求要过 consoleAdminAuth ⇒ 未登录/错令牌拿不到（401）；
	//   · 页面 HTML 里不留凭据 ⇒ 截图、另存为、浏览器缓存、扩展都带不出去。
	// 两者对"登录之后"的体验完全一样（打开就能用、点一下就能复制/显示），
	// 差别只在未授权时拿不拿得到 —— 那正是门禁该管的事。
	adminKeyFull, adminTokenFull := "", ""
	if p.IsAdmin() {
		if h.cfg.APIKey != "" {
			adminMask = apikey.MaskID(h.cfg.APIKey)
			adminKeyFull = h.cfg.APIKey
		}
		if t := h.currentAdminToken(); t != "" {
			adminTokenMask = apikey.MaskID(t)
			adminTokenFull = t
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":        keys,
		"enabled":     true,
		"admin_key":   adminMask,
		"admin_token": adminTokenMask,
		// 完整值（会话鉴权之后才可能走到这里 ⇒ 未登录拿不到）。
		// 前端用 `admin_key_full` 自动填顶栏那个「对话密钥」⇒ 模型目录/对话测试
		// 登录后开箱可用；`admin_token_full` 供仪表盘"显示/复制"管理令牌。
		"admin_key_full":   adminKeyFull,
		"admin_token_full": adminTokenFull,
		// ⚠ 2026-10-19 用户新口径：`"member": !p.IsAdmin()` 字段**已删除**。
		//
		// 它的唯一用途是让前端把面板标题换成「我的 API Key」（成员看到的是
		// 过滤后的子集，不点明会让人以为"别人的 key 都不见了"）。单账号口径
		// 下没有成员会话 ⇒ 这个标志**恒为 false**，留着就是一条永远为假的
		// 角色判据，下一个人会据此以为成员还有身份。
		//
		// 载荷的其余字段一个都不动（keys / enabled / admin_key / admin_token）：
		// 删掉"成员的标记"**不等于**少给数据 —— 对唯一的身份（管理员）来说
		// 这三条掩码与整份 key 列表照旧。
		//
		// 前端如果还在读 `json.member`，一律得到 undefined（falsy）⇒ 走
		// "管理员面板"那条渲染路径，正是它应有的样子；前端文件由另一个窗口
		// 清理（本窗口只写 *.go）。
	})
}

// apiKeysCreate POST /admin/apikeys —— body {"name":"...","limit":0,"rpm":0,"models":["..."]}。
// 返回完整 key（**仅这一次**返回明文，前端展示后不再可得）。
//
// models 省略 / null / [] 都等于**不限制模型**（见 apikey.Key.Models 的三态说明）。
func (h *Handler) apiKeysCreate(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Limit  int64    `json:"limit"`
		RPM    int      `json:"rpm"`
		Models []string `json:"models"`
		// ModelMap 模型对外名映射（对外名 → 内部名）。
		// 省略 / null ⇒ 不做映射（与 models 的三态一致）。
		ModelMap map[string]string `json:"model_map"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	// T8：新 key 的归属 = 当前身份。
	//   · 成员建的 ⇒ 归他（此后他只能看见/管这一把）
	//   · 管理员建的 ⇒ owner="" （存量语义：空 = 管理员所有）
	//   · 用 admin_token 的脚本建的 ⇒ UserID 为空 ⇒ 同样落进"管理员所有"
	k, err := s.CreateOwned(h.principalFrom(r).UserID, body.Name, body.Limit, body.RPM, body.Models, body.ModelMap)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         k.ID,
		"masked":     apikey.MaskID(k.ID),
		"full":       k.ID,
		"name":       k.Name,
		"enabled":    k.Enabled,
		"limit":      k.Limit,
		"rpm":        k.RPM,
		"models":     k.Models,
		"model_map":  k.ModelMap,
		"created_at": k.CreatedAt,
	})
}

// apiKeysToggle POST /admin/apikeys/toggle —— body {"id":"...","enabled":bool}。
// principalFrom 取**已过门禁**请求的身份（供 handler 判断归属用）。
//
// # 为什么取不到时回零值而不是拒绝
//
// 门禁（admin.go 的 ServeHTTP）已经保证"到这里的人有身份"；能走到 handler 的
// 只有两条路：管理员，或成员白名单里的端点。所以这里的 `!ok` 是"不该发生"的
// 情况（例如将来有人把 handler 挂到门禁之外）。
//
// 零值 = `IsAdmin()==false` + `UserID==""`，而 apikey 的归属判据对
// "空 owner + 非管理员"是**空列表 / 不能管**（见 apikey.ListFor 的注释）
// ⇒ 恰好是**最严**的那一档。所以这里不必再写一次拒绝：包一层只会掩盖门禁
// 真正失效的事实。
func (h *Handler) principalFrom(r *http.Request) account.Principal {
	p, ok := h.principalOf(r)
	if !ok {
		return account.Principal{}
	}
	return p
}

// requireKeyAccess 管某把 key 之前的越权检查；false = 已经写过响应。
//
// 口径：管理员管全部；成员只管自己名下的（`apikey.Store.CanManage`，与
// `ListFor` 同一套判据的另一种形态 —— 能看见才谈得上管）。
//
// ⚠ 不区分"key 不存在"与"不是你的"：都回 403。区分开就给了成员一个探测器
// ——"这个 id 存在吗？"（拿 id 枚举 + 响应差异）。他不需要知道别人的 id 是否有效。
func (h *Handler) requireKeyAccess(w http.ResponseWriter, r *http.Request, id string) bool {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return false
	}
	p := h.principalFrom(r)
	if s.CanManage(p.UserID, p.IsAdmin(), id) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "这把 API key 不属于当前账号（成员只能管理自己创建的 key）。",
		"code":  "forbidden_role",
		"scope": "apikey",
	})
	return false
}

func (h *Handler) apiKeysToggle(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if !h.requireKeyAccess(w, r, body.ID) {
		return
	}
	on, err := s.Toggle(body.ID, body.Enabled)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": apikey.MaskID(body.ID), "enabled": on})
}

// apiKeysDelete POST /admin/apikeys/delete —— body {"id":"..."}。
func (h *Handler) apiKeysDelete(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if !h.requireKeyAccess(w, r, body.ID) {
		return
	}
	if err := s.Delete(body.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// apiKeysReset POST /admin/apikeys/reset —— body {"id":"..."}，重置已用额度/失败数。
func (h *Handler) apiKeysReset(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if !h.requireKeyAccess(w, r, body.ID) {
		return
	}
	if err := s.Reset(body.ID); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

// apiKeysUpdate POST /admin/apikeys/update —— body {"id":"...","limit":0,"rpm":0,"models":[...]}。
// 改配额/限速/模型白名单。
//
// # 每个字段都是三态（**字段缺失 = 不改这一项**）
//
//	limit / rpm 用**指针**：缺失 / null → 不改；给 0 → 设为不限；给 N → 设为 N
//	models 用切片：缺失 / null → 不改；给 `[]` → 清空（恢复不限制）；给非空 → 设为白名单
//
// ⚠ 前端有「改配额」与「改模型」两个独立动作，各自只带自己那几个字段。
// 把"缺失"当成"设为 0 / 清空"会让**用户没动过的字段被悄悄改掉** ——
// 改一次模型把额度清零，或改一次配额把模型限制解开。两者界面上都看不出来。
func (h *Handler) apiKeysUpdate(w http.ResponseWriter, r *http.Request) {
	s := h.apiKeysStore()
	if s == nil {
		writeError(w, http.StatusNotImplemented, "API key 管理未启用")
		return
	}
	var body struct {
		ID     string   `json:"id"`
		Limit  *int64   `json:"limit"`
		RPM    *int     `json:"rpm"`
		Models []string `json:"models"`
		// Groups 这把 key 可用的分组 ID 列表（PLAN-groups P2/Q2：多选）。
		//
		// 三态与 Models 一致：nil（字段缺失）⇒ 不改；[] ⇒ 清空（= 不限制）；
		// 非空 ⇒ 整体替换。
		//
		// ⚠ 存 ID 不存名字：分组可以改名，存名字等于改名即断链。
		Groups []string `json:"groups"`
		// ModelMap 模型对外名映射（对外名 → 内部名）。
		// nil（字段缺失）⇒ 不改；{} ⇒ 清空映射；非空 ⇒ 整体替换。
		ModelMap map[string]string `json:"model_map"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if !h.requireKeyAccess(w, r, body.ID) {
		return
	}
	if err := s.UpdateWithGroups(body.ID, body.Limit, body.RPM, body.Models, body.Groups, body.ModelMap); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

// writeStoreError 把 apikey.Store 的哨兵错误映射成 HTTP。
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apikey.ErrUnknown):
		writeError(w, http.StatusNotFound, "API key 不存在")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// apiKeysRotateAdmin POST /admin/apikeys/rotate-admin —— 轮换管理钥匙。
//
// 生成新 key → 写回 config.json 的 api_key 字段（ConfigPath）→ 通过
// OnAPIKeyRotated 通知装配层更新 handler 内存（立即生效，无需重启）→
// apiKeysRotateAdmin 轮换**管理令牌**（admin_token）。
//
// # 与 apiKeysRotateAPIKey 的分工（用户要求：门禁 token 与 api 必须分开）
//
//	本端点          → 换 admin_token（管理台门禁）。换完**旧令牌立即作废**。
//	apiKey 那个端点 → 换 api_key（对话接口凭据）。
//
// 两者互不影响 —— 这正是"两把钥匙"的意义：轮换对话 key 不该把管理台锁掉，
// 反之亦然。
//
// ⚠ 改造前这个端点换的是 **api_key**（名字叫 rotate-admin 却动对话凭据），
// 而门禁也认 api_key —— 于是"轮换管理钥匙"实际是"换掉对话 key 并顺带
// 改变门禁"，两者纠缠在一起。现在它只管门禁。
//
// # 与 apiKeysRotateAPIKey 的分工（用户要求：门禁 token 与 api 必须分开）
//
//	本端点          → 换 admin_token（管理台门禁）。换完**旧令牌立即作废**。
//	apiKey 那个端点 → 换 api_key（对话接口凭据）。
//
// 两者互不影响 —— 这正是"两把钥匙"的意义：轮换对话 key 不该把管理台锁掉，
// 反之亦然。
//
// # 无令牌时的行为（不生成、而是引导配置）
//
// 部署可能根本没配 admin_token。此时**不应该**
// 偷偷生成一把并写盘 —— 那会改变"本部署有没有远程管理面"这个安全事实，
// 而该事实应当由人在 config 里显式写定。所以这里明确报错并给出做法。
//
// ⚠ 注意：没配 admin_token 不等于"没有管理面" —— 控制台账号（浏览器会话）
// 是另一条独立的路（见 admin.go 的 principalOf）。本端点管的是**令牌**这把机器钥匙。
func (h *Handler) apiKeysRotateAdmin(w http.ResponseWriter, r *http.Request) {
	// ★ 硬判管理员（T8）。为什么在门禁之外再判一次？
	//
	// 因为这两个端点的回执里带**完整的明文新凭据**（新 admin_token / 新 api_key），
	// 而"轮换"是**改整个部署的钥匙**：越权一次即全失守。
	//
	// ⚠ 2026-10-19 单账号口径：门禁侧那张成员白名单已整张删掉（admin.go 的
	// canAccess）—— 非管理员身份在 account 那层就被拒。这条二次判据因此更该
	// 留着：它是这类端点上唯一还能独立失败的那层（门禁写错、端点被挪到别处
	// 挂载，任何一处都够）。三行，代价可忽略。
	if !h.principalFrom(r).IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "轮换管理凭据仅管理员可用。",
			"code":  "forbidden_role",
			"scope": "admin",
		})
		return
	}
	if h.cfg.ConfigPath == "" {
		writeError(w, http.StatusBadRequest, "本部署不支持写回 config.json（未接线 ConfigPath），无法轮换")
		return
	}
	if strings.TrimSpace(h.currentAdminToken()) == "" {
		writeError(w, http.StatusBadRequest,
			"当前未配置 admin_token。请先在 config.json 里加一个 `\"admin_token\": \"<足够长的随机值>\"` "+
				"再重启；那之后本端点就能免重启地轮换它了。"+
				"（不自动生成：'本部署有没有远程管理面'该由人在配置里写定，不由一次点击决定。）")
		return
	}
	newKey := apikey.NewAdminKey()
	if err := setConfigField(h.cfg.ConfigPath, "admin_token", newKey); err != nil {
		writeError(w, http.StatusInternalServerError, "写回 config.json 失败: "+err.Error()+"（新令牌未生效）")
		return
	}
	// 内存立即生效（必须**跨包**同步，否则就是本次修的"要重启才生效"）。
	h.SetAdminToken(newKey)
	if h.cfg.OnAdminTokenRotated != nil {
		h.cfg.OnAdminTokenRotated(newKey)
	}
	log.Printf("admin: 管理令牌已轮换（写入 %s，立即生效，无需重启）", h.cfg.ConfigPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"full":   newKey,
		"masked": apikey.MaskID(newKey),
		"path":   h.cfg.ConfigPath,
		"note": "旧管理令牌已立即失效。请用新令牌重新登录控制台" +
			"（**本机也要**凭据：会话或 admin_token 二选一；轮换后旧的那份不再放行）。",
	})
}

// apiKeysRotateAPIKey 轮换**对话接口的 api_key**（与门禁令牌无关）。
func (h *Handler) apiKeysRotateAPIKey(w http.ResponseWriter, r *http.Request) {
	// ★ 硬判管理员（T8）。为什么在门禁之外再判一次？
	//
	// 因为这两个端点的回执里带**完整的明文新凭据**（新 admin_token / 新 api_key），
	// 而"轮换"是**改整个部署的钥匙**：越权一次即全失守。
	//
	// ⚠ 2026-10-19 单账号口径：门禁侧那张成员白名单已整张删掉（admin.go 的
	// canAccess）—— 非管理员身份在 account 那层就被拒。这条二次判据因此更该
	// 留着：它是这类端点上唯一还能独立失败的那层（门禁写错、端点被挪到别处
	// 挂载，任何一处都够）。三行，代价可忽略。
	if !h.principalFrom(r).IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "轮换管理凭据仅管理员可用。",
			"code":  "forbidden_role",
			"scope": "admin",
		})
		return
	}
	if h.cfg.APIKey == "" {
		writeError(w, http.StatusBadRequest, "当前未配置 api_key（config 里没有），无需轮换")
		return
	}
	if h.cfg.ConfigPath == "" {
		writeError(w, http.StatusBadRequest, "本部署不支持写回 config.json（未接线 ConfigPath），无法轮换")
		return
	}
	newKey := apikey.NewAdminKey()
	if err := setConfigField(h.cfg.ConfigPath, "api_key", newKey); err != nil {
		writeError(w, http.StatusInternalServerError, "写回 config.json 失败: "+err.Error()+"（新 key 未生效）")
		return
	}
	if h.cfg.OnAPIKeyRotated != nil {
		h.cfg.OnAPIKeyRotated(newKey)
	}
	log.Printf("admin: api_key 已轮换（写入 %s，立即生效）", h.cfg.ConfigPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"full":   newKey,
		"masked": apikey.MaskID(newKey),
		"path":   h.cfg.ConfigPath,
		"note":   "这是**对话接口**的凭据。管理台门禁用的是 admin_token，两者独立。",
	})
}

// setConfigField 读 config.json → 只改指定顶层字段的值 → 原子写回。
//
// 这是 setConfigAPIKey 的泛化：字段名参数化，其余逐字节保留的语义完全一致。
// 抽出来的理由是**现在有两个字段要改**（api_key 与 admin_token），
// 而它们在同一个文件里、必须是同一套"只动那一处值"的写法。
func setConfigField(path, field, newValue string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取: %w", err)
	}
	if out, ok := replaceJSONStringValue(raw, field, newValue); ok {
		return writeFileAtomic(path, out)
	}
	// 回落：文件里没有该字段 —— 那说明它还没被写过。
	//
	// ⚠ 这里**不能**用"整体重新序列化"（那会丢注释/顺序，正是用户报的
	// 「配置太乱」）。改成**插入**一行：在顶层对象的开括号之后插入。
	out, ok := insertTopLevelString(raw, field, newValue)
	if !ok {
		return fmt.Errorf("config.json 里定位不到顶层对象的插入点（文件畸形？）")
	}
	log.Printf("admin: config.json 原本没有 %s 字段，已插入（其余内容逐字节保留）", field)
	return writeFileAtomic(path, out)
}

// insertTopLevelString 在顶层 JSON 对象的第一个 `{` 之后插入 `"field": "value",`。
//
// # 为什么是"插入"而不是"解析后重写"
//
// 解析后重写会丢注释与字段顺序（见 setConfigField 的注释）——
// 而本仓的 config.json 现在**支持并鼓励**写注释（用户要求）。
// 插入只影响那一行的位置，其余逐字节保留。
//
// 缩进取 2 空格（与 config.example.json 及各部署的既有风格一致）。
func insertTopLevelString(raw []byte, field, value string) ([]byte, bool) {
	// 找第一个非空白、非注释字符里的 `{`（顶层对象开括号）。
	clean := stripCommentsForRewrite(raw)
	open := -1
	for i := 0; i < len(clean); i++ {
		switch clean[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			open = i
		}
		break
	}
	if open < 0 {
		return nil, false
	}
	enc, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	// 插入文本：换行 + 2 空格缩进 + "field": "value",
	insert := "\n  \"" + field + "\": " + string(enc) + ","
	out := make([]byte, 0, len(raw)+len(insert))
	out = append(out, raw[:open+1]...)
	out = append(out, insert...)
	out = append(out, raw[open+1:]...)
	return out, true
}

// setConfigAPIKey 保留旧名（既有调用方与测试用它），语义 = setConfigField("api_key")。
//
// # ⚠ 改造前它把整个 config 重新序列化，这是用户那份配置"乱"的根源
//
// 旧实现：
//
//	var cfg map[string]any
//	json.Unmarshal(raw, &cfg)
//	cfg["api_key"] = newKey
//	out, _ := json.MarshalIndent(cfg, "", "  ")   // ← 整个文件重写
//
// 后果三条（用户实测看到的"太乱了"）：
//
//	① **注释全丢** —— 用户手写的说明一个都不剩
//	② **顺序全变** —— map 序列化按 key 排序，且嵌套层级与手写习惯无关，
//	   于是"轮换一次钥匙，配置文件的段落顺序就变了"
//	③ **空行/缩进风格全变** —— 包括那个夹在段中间的空行
//
// 而轮换管理钥匙**只需要改一个字符串**。没有任何理由重写整个文件。
//
// # 现在的做法：字节级替换那一处的值
//
// 定位 `"api_key"` 这个键、找到它后面的字符串字面量、只替换那一段。
// 其余**逐字节原样保留** —— 注释、顺序、空行、缩进全部不动。
func setConfigAPIKey(path, newKey string) error {
	return setConfigField(path, "api_key", newKey)
}

// replaceJSONStringValue 把 `"key": "..."` 里的值替换成 newValue，
// 其余字节**原样保留**。成功返回 (新内容, true)。
//
// # 为什么手写扫描而不是用正则
//
// 要跳过的情形太多，正则写不对：字符串内部的 `"api_key"`（例如某个说明
// 字段刚好含这几个字）、转义引号、注释里的同名键。这些的失败形态都是
// **改错地方** —— 而改错一个配置文件的后果比不轮换钥匙重得多。
//
// 扫描规则（与 jsonc.go 的字符串状态机同源）：
//
//	跳过字符串、行注释、块注释
//	只在**顶层**找 `"key"`（深度 1 的键）—— 嵌套对象里的同名键不动
//	键后面必须是可选的空白 + 冒号 + 可选的空白 + 双引号字符串
func replaceJSONStringValue(raw []byte, key, newValue string) ([]byte, bool) {
	keyLit := []byte(`"` + key + `"`)
	depth := 0
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			// 字符串：跳到结束（含转义处理）。
			j := i + 1
			for j < len(raw) {
				if raw[j] == '\\' {
					j += 2
					continue
				}
				if raw[j] == '"' {
					break
				}
				j++
			}
			if j >= len(raw) {
				return nil, false // 未闭合
			}
			// 是顶层键名？且紧接着是冒号？
			if depth == 1 && bytes.Equal(raw[i:j+1], keyLit) {
				k := j + 1
				for k < len(raw) && (raw[k] == ' ' || raw[k] == '\t' || raw[k] == '\n' || raw[k] == '\r') {
					k++
				}
				if k < len(raw) && raw[k] == ':' {
					k++
					for k < len(raw) && (raw[k] == ' ' || raw[k] == '\t' || raw[k] == '\n' || raw[k] == '\r') {
						k++
					}
					if k < len(raw) && raw[k] == '"' {
						// 找到值的起止。
						v := k + 1
						for v < len(raw) {
							if raw[v] == '\\' {
								v += 2
								continue
							}
							if raw[v] == '"' {
								break
							}
							v++
						}
						if v >= len(raw) {
							return nil, false
						}
						enc, err := json.Marshal(newValue)
						if err != nil {
							return nil, false
						}
						out := make([]byte, 0, len(raw)-(v-k-1)+len(enc))
						out = append(out, raw[:k]...)
						out = append(out, enc...)
						out = append(out, raw[v+1:]...)
						return out, true
					}
				}
			}
			i = j
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '/':
			// 注释：跳过（保持与 jsonc.go 一致 —— 注释里的 "api_key" 不是键）。
			if i+1 < len(raw) && raw[i+1] == '/' {
				for i < len(raw) && raw[i] != '\n' {
					i++
				}
			} else if i+1 < len(raw) && raw[i+1] == '*' {
				i += 2
				for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
					i++
				}
				i++
			}
		}
	}
	return nil, false
}

// stripCommentsForRewrite 只给"没有 api_key 可替换、必须整体重写"的回落路径用。
//
// 与 jsonc.go 的 stripJSONComments 是**同一件事**，但那个在 main 包、
// 这个在 admin 包（admin 不能 import cmd/server —— 方向反了）。
//
// ⚠ 两处实现同一套状态机是**重复**，这里如实记下取舍：抽公共包要新建
// 一个只放 40 行状态机的包，而它的唯一调用方就是这两处；相比之下
// "两处各有一份"的风险是它们会分叉。分叉的症状是"配置文件写法在
// 启动解析和轮换写回两条路径上被理解得不一样"—— 概率低但难查。
// 若将来出现第三处调用，就应该抽出去（那时收益才盖过成本）。
func stripCommentsForRewrite(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	inStr, inLine, inBlock := false, false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case inLine:
			if c == '\n' || c == '\r' {
				inLine = false
				continue
			}
			out[i] = ' '
		case inBlock:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				inBlock = false
				continue
			}
			if c != '\n' && c != '\r' {
				out[i] = ' '
			}
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		default:
			switch {
			case c == '"':
				inStr = true
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				out[i], out[i+1] = ' ', ' '
				i++
				inLine = true
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				out[i], out[i+1] = ' ', ' '
				i++
				inBlock = true
			}
		}
	}
	return out
}

// writeFileAtomic 临时文件 + rename，避免写到一半被中断留下半个配置。
//
// 权限保持 0600（配置里有管理钥匙）—— 与旧实现一致。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换: %w", err)
	}
	return nil
}
