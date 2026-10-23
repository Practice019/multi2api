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
	ModelMap   map[string]string `json:"model_map,omitempty"`
	CreatedAt   string   `json:"created_at"`
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
	out := make([]groupView, 0, len(list))
	for _, g := range list {
		out = append(out, groupViewOf(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "enabled": true})
}

// groupCreateReq 建分组的入参。models 是内部模型名清单（归一化由 groupstore 做）。
type groupCreateReq struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Models      []string `json:"models"`
	// ModelMap 对外名 → 内部名。**别名跟着分组走**（用户要求：模型在分组里定义，
	// 那"它对外叫什么"也该在这里定义）。
	ModelMap map[string]string `json:"model_map"`
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
	ModelMap *map[string]string `json:"model_map"`
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
	var mm map[string]string
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
