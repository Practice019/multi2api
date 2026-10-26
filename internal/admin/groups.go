// groups.go 管理端「分组」CRUD（PLAN-groups.md P1）。
//
// # 分组是什么（与本仓既有概念的关系）
//
// 分组是一批**内部模型名**的具名集合（见 internal/groupstore 的包注释）：
//
//	分组 { m-a, m-b }  ← 只回答「允许哪些模型」
//	       ↓ 由哪条渠道服务，是 channelstore 能力索引的事
//
// 所以这里的接口既不收渠道 id，也不做渠道校验 —— 分组里写一个暂时没有渠道
// 能服务的模型是**合法**的（那是"允许范围"，不是"保证可用"，见 PLAN 3.4）。
//
// # 三条与 channels.go 对齐的约定
//
//  1. **未注入就不注册路由**：没配分组存储的部署不该出现一个点了会 503 的按钮。
//  2. **update 的字段三态**：name/description/models 都是可选的，缺省 = 不改；
//     显式给 [] = 清空模型清单。用指针收参数就是为了分清"没传"与"传了空"。
//  3. **错误按哨兵映射状态码**：重名 → 409、不存在 → 404。
//     一律回 400 的话，前端只能把「名字被占了」当成普通失败，用户不知道该改什么。
package admin

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// groupsStore 取分组存储；nil = 未启用该功能。
//
// 接线位置（已完成，改动时别漏）：
//
//	① admin.Config.Groups *groupstore.Store
//	② admin.New 里独立一段：if h.cfg.Groups != nil { h.registerGroups() }
//	   ⚠ 必须是**独立**的 if，不能并进 APIKeys / Channels 两处 —— 分组与它们是
//	     三件独立的事，共用门会让「只配了分组」的部署静默看不到这个功能
//	     （路由 404，而前端按 manifest 只会觉得功能不存在，没有任何报错线索）。
//	③ cmd/server/main.go：groupstore.New(filepath.Join(authsRoot, "groups.json")) 并注入。
//
// 未接线时本函数返回 nil ⇒ 列表回 enabled=false、其余端点 404，不影响既有功能。
func (h *Handler) groupsStore() *groupstore.Store { return h.cfg.Groups }

// groupCatalogFunc 取"当前对外模型目录"（内部名），供一致性检查判断分组内容是否陈旧。
//
// TODO(接线，由主脑完成)：
//
//	admin.Config 加一个字段：ModelNames func() []string
//	本函数体改成：return catalogFunc(catalogFunc(h.cfg.ModelNames))
//	cmd/server/main.go 注入一个取当前目录的函数（内部名，含上游 + 渠道并集）。
//
// 未接线时返回 nil ⇒ 一致性报告里 StaleCheckable=false（前端据此显示
// "未检查"而不是"全部干净"）。这条降级是刻意的：**空** 与 **没查** 必须能分开，
// 否则界面会把"没查过"显示成"一切正常"。
func (h *Handler) groupCatalogFunc() catalogFunc {
	if h.cfg.ModelNames == nil {
		return nil
	}
	return catalogFunc(h.cfg.ModelNames)
}

// registerGroups 注册 /admin/groups 系列端点（接线后在 New 里调用）。
func (h *Handler) registerGroups() {
	h.register("GET /admin/groups", h.groupsList)
	h.register("POST /admin/groups", h.groupsCreate)
	h.register("POST /admin/groups/update", h.groupsUpdate)
	h.register("POST /admin/groups/delete", h.groupsDelete)
	// 引用完整性（P5）：检查只读、清理显式触发。
	//
	// ⚠ 清理**不自动跑**：陈旧模型那项依赖模型目录，而目录会因上游抖动短暂为空，
	// 自动清理会把用户配好的分组内容一次删光（apikey.PruneModels 的注释里记过同一坑）。
	h.register("GET /admin/groups/consistency", h.groupsConsistency)
	// T85：引用反查（只读）—— "我改这个分组会影响谁"。
	//
	// ⚠ 挂在同一个 registerGroups 里 ⇒ 自动继承 groups.go:15 那条约定：
	//   没注入分组存储的部署**不注册**这个路由（404 而不是 503），前端按 manifest
	//   就不会渲染入口。它不写库、不触发级联，所以不挂审计写动作。
	h.register("GET /admin/groups/usage", h.groupsUsage)
	// 编辑弹窗的候选模型（与一致性检查同一份来源，避免两边判断不一致）。
	h.register("GET /admin/groups/catalog", h.groupCatalog)
	h.register("POST /admin/groups/prune", h.groupsPrune)
}

// groupView 是对外的分组视图。
//
// ModelCount 单独给：前端表格要显示"这个分组里有几个模型"，让它在不展开
// models 数组时也能直接渲染（列表接口一次可能回几十条，前端不该为了计数再算一遍）。
type groupView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Models      []string `json:"models"`
	ModelCount  int      `json:"model_count"`
	// ModelMap 对外名 → 内部名（别名跟着分组走，见 groupstore.Group.ModelMap）。
	ModelMap  map[string][]string `json:"model_map,omitempty"`
	CreatedAt string              `json:"created_at"`

	// UsedByCount 引用这个分组的**条目总数**（API key + 渠道 + 令牌）。
	//
	// ⚠ T85 新增的只读字段，既有字段一个没动。它回答用户最痛的那个问题
	//「我改这个分组会影响谁」—— 列表页每行都要显示，所以它必须来自**一次聚合**
	//（见 groupRefIndex：三处各查一遍，不在每行里各查三次）。
	UsedByCount int `json:"used_by_count"`
}

func groupViewOf(g *groupstore.Group) groupView {
	models := g.Models
	if models == nil {
		// 落盘/内存里都保证是 []，这里再兜一层：前端拿到 null 会多出一条分支。
		models = []string{}
	}
	return groupView{
		ID: g.ID, Name: g.Name, Description: g.Description,
		Models: models, ModelCount: len(models), ModelMap: g.ModelMap, CreatedAt: g.CreatedAt,
	}
}

// groupsList GET /admin/groups
func (h *Handler) groupsList(w http.ResponseWriter, r *http.Request) {
	s := h.groupsStore()
	if s == nil {
		// 与 channels 同款：未启用回空集 + enabled=false（前端据此不渲染操作区）
		writeJSON(w, http.StatusOK, map[string]any{"groups": []groupView{}, "enabled": false})
		return
	}
	list := s.List()
	// T85：一次聚合出全部分组的引用数（3 次查询 + 内存归并），不是每行 3 次查询。
	// 规模前提：key / 渠道 / 令牌都是**数十量级**（渠道 3~20、key 个位数、令牌数十），
	// 所以「全量拉回内存再归并」比在 SQL 里做三张表的 JOIN/子查询更简单也更快；
	// 若将来这些表涨到万级，这里要换成 SQL 聚合（接口形状不用改）。
	idx := h.groupRefIndex(list)
	out := make([]groupView, 0, len(list))
	for _, g := range list {
		v := groupViewOf(g)
		v.UsedByCount = idx.countOf(g.ID)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "enabled": true})
}

type groupCreateReq struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Models      []string `json:"models"`
	// ModelMap 对外名 → 内部名。**别名跟着分组走**（用户要求：模型在分组里定义，
	// 那"它对外叫什么"也该在这里定义）。
	ModelMap map[string][]string `json:"model_map"`
}

// groupsCreate POST /admin/groups
func (h *Handler) groupsCreate(w http.ResponseWriter, r *http.Request) {
	s := h.groupsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "分组功能未启用"})
		return
	}
	var req groupCreateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	// 名字必填在存储层也校验（那是唯一的事实来源）；这里先拦一道只是为了
	// 给出更贴近表单的中文提示，而不是 "groupstore: 分组名不能为空"。
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name 必填"})
		return
	}
	g, err := s.Create(req.Name, req.Description, req.Models, req.ModelMap)
	if err != nil {
		writeGroupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "created": true, "group": groupViewOf(g)})
}

// groupUpdateReq 改分组的入参。三个可选字段都是**指针**：
//
//	nil        → 不改
//	指向 ""    → 存储层报错（分组名不能为空），描述可以清成空串
//	指向 []    → 清空模型清单
//
// 用指针而不是"空值代表不改"：models 的空切片是**合法输入**（用户就是要清空），
// 若把它当成"没传"，前端就永远没法把分组里的模型清掉。
type groupUpdateReq struct {
	ID          string    `json:"id"`
	Name        *string   `json:"name"`
	Description *string   `json:"description"`
	Models      *[]string `json:"models"`
	// ModelMap 三态：不传/null = 不改；{} = 清空；非空 = 整体替换。
	ModelMap *map[string][]string `json:"model_map"`
}

// groupsUpdate POST /admin/groups/update
func (h *Handler) groupsUpdate(w http.ResponseWriter, r *http.Request) {
	s := h.groupsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "分组功能未启用"})
		return
	}
	var req groupUpdateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	// *[]string → []string 的转换：nil 指针保持 nil（= 不改），
	// 非 nil 指针即使指向空切片也照样传出（= 清空）。
	//
	// ⚠ JSON 里写 "models": null 与**不写**等价（encoding/json 把 null 解到指针
	// 就是置 nil），所以"清空"必须写 []。这条不写清楚，前端会以为 null 能清空，
	// 提交后界面提示成功、模型却还在。
	var models []string
	if req.Models != nil {
		models = *req.Models
	}
	// model_map 三态：指针 nil（不传/null）= 不改；指向空 map = 清空。
	//
	// ⚠ 与 models 同一个坑：JSON 里写 "model_map": null 等价于**不写**，
	// 要清空必须写 {}。
	var mm map[string][]string
	if req.ModelMap != nil {
		mm = *req.ModelMap
	}
	var g *groupstore.Group
	var err error
	if req.ModelMap != nil {
		g, err = s.Update(req.ID, req.Name, req.Description, models, mm)
	} else {
		g, err = s.Update(req.ID, req.Name, req.Description, models)
	}
	if err != nil {
		writeGroupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "group": groupViewOf(g)})
}

// groupsDelete POST /admin/groups/delete  {"id": "..."}
func (h *Handler) groupsDelete(w http.ResponseWriter, r *http.Request) {
	s := h.groupsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "分组功能未启用"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	// 先记下这个分组叫什么（删完就查不到了），日志与响应里要用。
	name := req.ID
	if g, ok := s.Get(req.ID); ok && g != nil {
		name = g.Name
	}

	if err := s.Delete(req.ID); err != nil {
		writeGroupErr(w, err)
		return
	}

	// ── 级联清理（P5）：删分组 ⇒ 所有 key 里指向它的引用一起消失 ──
	//
	// 与删渠道时清 key 里的模型引用（apikey.PruneModels）是同一套口径：
	// 用户删了一个东西，指向它的残留就该一起消失。
	//
	// 不清理的后果是可控但不友好：P2/P3 的判定链把"受限且并集为空"处理成
	// **拒绝**（安全），于是那些 key 调用全 403，而错误只说"分组里没有任何模型"。
	out := map[string]any{"ok": true, "id": req.ID, "name": name}
	if ks := h.apiKeysStore(); ks != nil {
		res, perr := ks.PruneGroups([]string{req.ID})
		if perr != nil {
			// ⚠ 清理失败**不**回滚分组删除：分组已经删掉了，回滚会让用户看到
			//   "删了又回来"。如实报告哪一步没做干净即可（与 channelsDelete 同处理）。
			log.Printf("admin: 分组 %s（%s）已删除，但清理 API key 引用失败: %v", req.ID, name, perr)
			out["prune_error"] = perr.Error()
		} else if res.KeysTouched > 0 {
			log.Printf("admin: 分组 %s（%s）已删除，连带清理 %d 把 API key 的分组引用", req.ID, name, res.KeysTouched)
			out["pruned_keys"] = res.KeysTouched
			// ⚠ 权限放大必须显式告诉调用方 —— 这是这次操作最需要用户知晓的后果。
			if len(res.KeysUnrestricted) > 0 {
				out["keys_unrestricted"] = res.KeysUnrestricted
				out["warning"] = "下列 key 的分组引用被清空，它们现在**不受模型限制**了（可用全部模型）：" +
					joinLimited(res.KeysUnrestricted, 5)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// writeGroupErr 把 groupstore 的错误哨兵映射成状态码。
//
// 为什么要分开：前端拿到 409 会提示"这个名字已经被占用，换个名字"，
// 拿到 404 会刷新列表 —— 两者都给 400 的话，用户只能看到"操作失败"。
func writeGroupErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, groupstore.ErrNameExists):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case errors.Is(err, groupstore.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
}

// ── T85：分组引用反查（只读聚合）────────────────────────────────────────────
//
// 用户最痛的问题：「我改这个分组会影响谁」。本轮补的就是这个**只读**反查。
//
// # 判据（对应契约，但有一处实现细节必须说明）
//
//	keys     internal/apikey 的 Key.Groups 含这个分组
//	channels store.Channel 的 GroupsOf()（Group 列逗号拆分，空 ⇒ default 兜底）含这个分组
//	tokens   store.Token 的 Group 编码串解出的 Groups ∪ AutoGroups 含这个分组
//
//
// ⚠ 类别之间有**重叠**，必须去重：Wave B2 起 apikey 的真源就是 tokens 表
// （apikey/sql.go 文件头写明"持久化：SQL（tokens 表）"），所以同一把凭据会同时出现在
// apikey.List 与 store.ListTokens 里。这里按 key 串去重：**先按 keys 计入，令牌维度
// 跳过已在 keys 里的那批**。否则同一把 key 被算两次。
// ⚠ **实现细节（与契约字面不同，已向主脑报备）**：契约说"含该分组**名**"，但代码里
// 三处存的东西**不是同一个命名空间**：
//
//	apikey.Groups  存的是分组 **ID**（形如 g-1a2b3c —— groups_consistency.go 就是拿
//	               g.ID 与它比对的）
//	token.Group    存的是 hyxapi 风格的**名**（NormalizeTokenGroups("") 兜底成 "default"，
//	               那是名字不是 ID）
//	channels.Group 存的是**名**（"default" / 用户填的组名）
//
// 所以匹配一律按「**ID 或名字**任一命中」，并在收集阶段把两种写法**归一化成组 ID**
// （见下面的 canonical），这样：
//
//	① 不管某个部署/前端写的是 ID 还是名字，反查都答得对；
//	② 同一条记录同时写了某组的 ID 与名字时只算**一次**（不会把 used_by_count 算成 2）。
//
// 若主脑要求严格只按名字匹配，改成只看 nameIndex 即可 —— 但那样 apikey 那半在真实
// 数据里恒为 0 条（它们存的是 ID）。

// groupRefKey 一条 API key 引用。
//
// ⚠ id 用**掩码**：只读聚合报告里不该出现明文 key —— 与 groups_consistency.go 的
// KeyID/Masked 同一口径（那里连内部结构都只把 Masked 投影进报告），
// 也与令牌列表一律走 MaskedTokens 同一条纪律。
type groupRefKey struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// groupRefChannel 一条渠道引用。
type groupRefChannel struct {
	// ID 管理端可寻址 id（= channels.external_id，与渠道列表里的 id 同源）。
	ID   string `json:"id"`
	Name string `json:"name"`
	// ExternalID 库列原值（今天与 ID 同一个值；分开给是因为历史行可能两样都空）。
	ExternalID string `json:"external_id"`
}

// groupRefToken 一条令牌引用（id 是令牌主键，与 /admin/tokens 列表同口径）。
type groupRefToken struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// groupRefTokenScanCap 令牌扫描的硬上限（分页要有头）。
//
// 为什么要有：这是只读反查，不能因为库里令牌异常多就把管理端拖住。
// 20 × 200 = 4000 条，远超本仓实际量级（数十）。被截断时**必须**在响应里说明
// （token_scan_truncated）——"少报了"与"没人引用"不能让用户分不出来。
const groupRefTokenScanCap = 4000

// listAllTokenRows 分页拉全量 tokens 行（store.ListTokens 的 limit 上限是 200）。
//
// 与 apikey/sql.go 的 listAllTokenRows 同一粒度：那边也是按 200 翻页直到取不满一页。
// 上限 groupRefTokenScanCap 是为了让只读反查有个头（超限时响应里带 token_scan_truncated）。
func listAllTokenRows() ([]store.Token, error) {
	const page = 200
	all := make([]store.Token, 0, page)
	for offset := 0; offset < groupRefTokenScanCap; offset += page {
		batch, err := store.ListTokens(offset, page)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < page {
			return all, nil
		}
	}
	return all, nil
}

// groupRefIndex 一次聚合的结果：按**分组 ID**归并的三类引用 + 计数。
type groupRefIndex struct {
	counts   map[string]int
	keys     map[string][]groupRefKey
	channels map[string][]groupRefChannel
	tokens   map[string][]groupRefToken
	// tokenScanTruncated 令牌维度是否因为超过扫描上限而被截断。
	tokenScanTruncated bool
}

// countOf 取某个分组的引用条目数（未知 ID ⇒ 0）。
func (i groupRefIndex) countOf(groupID string) int { return i.counts[groupID] }

// groupRefIndex 收集全部分组的三类引用（**每类各查一遍**，然后内存归并）。
//
// 为什么是"各查一遍"而不是逐分组查询：列表页要给每个分组算 used_by_count，
// 若逐分组去查 key/渠道/令牌，就是 N+1（分组数 × 3 次全表扫）。这里固定 3 次查询，
// 与分组数无关。规模前提见 groupsList 的注释（数十量级）。
func (h *Handler) groupRefIndex(groups []*groupstore.Group) groupRefIndex {
	idx := groupRefIndex{
		counts:   map[string]int{},
		keys:     map[string][]groupRefKey{},
		channels: map[string][]groupRefChannel{},
		tokens:   map[string][]groupRefToken{},
	}
	byID := make(map[string]bool, len(groups))
	byName := make(map[string]string, len(groups))
	for _, g := range groups {
		if g == nil {
			continue
		}
		byID[g.ID] = true
		if _, dup := byName[g.Name]; !dup {
			byName[g.Name] = g.ID
		}
	}
	// canonical 把一条记录写下的若干分组标识归一化成去重后的**分组 ID** 列表。
	// 不认识的标识（组已删的悬空引用）不归属任何分组 —— 那是 P5 一致性端点的事。
	canonical := func(refs []string) []string {
		out := make([]string, 0, len(refs))
		seen := map[string]bool{}
		for _, raw := range refs {
			s := strings.TrimSpace(raw)
			if s == "" {
				continue
			}
			id := s
			if !byID[id] {
				mapped, ok := byName[s]
				if !ok {
					continue
				}
				id = mapped
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
		return out
	}

	// ① + ③ 凭据（keys / tokens）—— **同一张表**。
	//
	// ⚠ 这是本轮最重要的一个代码事实：Wave B2 起 API key 与 console token 是**同一批行**
	// （apikey/sql.go 的 reloadIndexFromSQL 把 tokens 表的**每一行**都装进 key 索引，
	// 而 /admin/tokens 也列同一批行）。所以契约里的 keys 与 tokens 不是两个不相交的集合，
	// 直接各扫一遍必然重复计数（同一把凭据既在 keys 又在 tokens ⇒ total 翻倍）。
	//
	// 判据用**归属**这个真实列：
	//
	//	user_id == 0  ⇒ keys   （管理端凭据：/admin/apikeys 建的 key，以及用管理令牌建的令牌；
	//	                        apikey 的 keyToToken 从不写 user_id）
	//	user_id >  0  ⇒ tokens （某个账号自己的令牌，/admin/tokens 按 user_id 归属）
	//
	// 于是两个类**互斥**、加起来正好是全部凭据 ⇒ total 不重复。
	// 分组判据与 apikey 那边逐字一致：apikey 的 Groups ↔ 本行的 group 列（encodeGroups/decodeGroups
	// 成对），所以读行上的 Group 与读 apikey 的内存态是同一个值。
	if store.DB != nil {
		rows, err := listAllTokenRows()
		if err != nil {
			log.Printf("admin: 分组引用反查读凭据失败（keys/tokens 两维按空处理）: %v", err)
		} else {
			for i := range rows {
				tok := rows[i]
				refs := append([]string{}, tok.GetGroups()...)
				if auto, aerr := tok.GetAutoGroups(); aerr == nil {
					refs = append(refs, auto...)
				}
				owner := tok.UserID
				for _, id := range canonical(refs) {
					idx.counts[id]++
					if owner == 0 {
						// id 用**掩码**：只读聚合报告里不该出现明文凭据。
						idx.keys[id] = append(idx.keys[id], groupRefKey{ID: tok.MaskedKey(), Name: tok.Name})
					} else {
						idx.tokens[id] = append(idx.tokens[id], groupRefToken{ID: tok.ID, Name: tok.Name})
					}
				}
			}
		}
	} else if ks := h.apiKeysStore(); ks != nil {
		// 降级：没有 SQL 的部署（apikey 还在 JSON 模式）读不到 user_id，只能把全部凭据
		// 归到 keys 一类。如实记一行日志，别让「少了 tokens 一类」看起来像「没人引用」。
		log.Printf("admin: 分组引用反查无 SQL 凭据源（tokens 一维按空处理，keys 取 apikey 内存态）")
		for _, v := range ks.List() {
			for _, id := range canonical(v.Groups) {
				idx.counts[id]++
				idx.keys[id] = append(idx.keys[id], groupRefKey{ID: v.Masked, Name: v.Name})
			}
		}
	}

	// ② 渠道：一次 ListChannels()；空分组按 default 兜底（与能力行同口径）。
	if store.DB != nil {
		rows, err := store.ListChannels()
		if err != nil {
			// 读失败**不**编造结果：宁可这一维少报（并在日志里留痕），
			// 也不要让「读不到」看起来像「没人引用」。
			log.Printf("admin: 分组引用反查读渠道失败（该维度按空处理）: %v", err)
		} else {
			for i := range rows {
				ch := &rows[i]
				gs := ch.GroupsOf()
				if len(gs) == 0 {
					gs = []string{store.AbilityDefaultGroup}
				}
				for _, id := range canonical(gs) {
					idx.counts[id]++
					idx.channels[id] = append(idx.channels[id], groupRefChannel{
						ID: ch.ExternalID, Name: ch.Name, ExternalID: ch.ExternalID,
					})
				}
			}
		}
	}
	return idx
}

// groupsUsage GET /admin/groups/usage?id=<分组名或 ID> —— 谁引用了这个分组（只读）。
//
// 契约（T85/T86 冻结）：
//
//	{"group":"<名>","keys":[{id,name}],"channels":[{id,name,external_id}],
//	 "tokens":[{id,name}],"total":N}
//
// 边界：
//
//	分组不存在          ⇒ **404**（用空数组冒充成功会让用户永远以为"没人引用"）
//	存在但无人引用      ⇒ 200 + total:0 + 三个空数组（**合法**结果，与 404 可区分）
//
// 它**不写库、不触发级联**：点开看一眼不该改变任何状态。
//
// # ⚠⚠ 两条**必须知道**的事实（主脑 2026-10-27 裁决维持本实现，故写死在这里）
//
//  1. **keys 与 tokens 不是两个不相交的来源，而是同一张 tokens 表按 user_id 分区**：
//     Wave B2 起 apikey 的真源就是 tokens 表，且 apikey/sql.go 的 reloadIndexFromSQL
//     把该表的**每一行**都装进 key 索引；/admin/tokens 也列同一批行。所以
//     user_id == 0 ⇒ keys（管理端建的凭据：/admin/apikeys 的 key、用管理令牌建的令牌），
//     user_id > 0 ⇒ tokens（账号自己的令牌）。
//     ⚠ 看到"同一把凭据既能出现在 keys 又能出现在 tokens"**不要**当 bug 去改：
//     那是**分区前**的样子（两边各扫一遍 ⇒ total 翻倍）。改回去 total 就变了。
//  2. **分组的命名空间不统一**（本仓真实不一致）：apikey.Groups 存分组 **ID**（g-xxxx），
//     而 token.Group 与 channels.Group 存分组 **名**（NormalizeTokenGroups("") 兜底成
//     "default" —— 那是名字）。所以匹配口径是「**ID 或名字**任一命中」，并且在收集阶段
//     归一化成组 ID（见 groupRefIndex 的 canonical）。**不要**为了"统一"改成只按名字：
//     那样 apikey 那半在真实数据里恒为 0 条，是静默失效。
func (h *Handler) groupsUsage(w http.ResponseWriter, r *http.Request) {
	s := h.groupsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "分组功能未启用"})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("id"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填（分组名或分组 ID）"})
		return
	}
	// 先按 ID 找（拿得到 ID 时最精确），再按名字找（契约里的入参形态）。
	g, ok := s.Get(q)
	if !ok || g == nil {
		g, ok = s.GetByName(q)
	}
	if !ok || g == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "分组不存在: " + q,
			"hint":  "按分组名与分组 ID 都没找到；要么名字写错，要么它已经被删掉",
		})
		return
	}

	idx := h.groupRefIndex(s.List())
	// 三个数组一律给**空切片**而不是 nil：前端不必为 null 多写一条分支（本仓一贯口径）。
	keys := idx.keys[g.ID]
	if keys == nil {
		keys = []groupRefKey{}
	}
	chans := idx.channels[g.ID]
	if chans == nil {
		chans = []groupRefChannel{}
	}
	toks := idx.tokens[g.ID]
	if toks == nil {
		toks = []groupRefToken{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group": g.Name,
		// keys 与 tokens 是**同一张 tokens 表按 user_id 的两个分区**（0 ⇒ keys，>0 ⇒ tokens），
		// 不是两个独立来源；两者互斥，所以 total 不重复计数（详见本函数上方第 1 条）。
		"keys": keys,
		// 渠道按 Group 列（分组**名**）匹配，并以 ID 或名字任一命中（命名空间不一致，见上方第 2 条）。
		"channels": chans,
		"tokens":   toks,
		"total":    len(keys) + len(chans) + len(toks),
		// 令牌被扫描上限截断时如实说明（见 groupRefTokenScanCap）。
		"token_scan_truncated": idx.tokenScanTruncated,
	})
}
