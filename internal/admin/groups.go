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
	"net/http"
	"strings"

	"workbuddy2api/internal/groupstore"
)

// groupsStore 取分组存储；nil = 未启用该功能。
//
// TODO(接线，由主脑完成，本文件不改逻辑)：
//
//	① admin.Config 里加一个字段：Groups *groupstore.Store
//	② 本函数体改成：return h.cfg.Groups
//	③ admin.New 里在 registerChannels 那一块旁边加：
//	     if h.cfg.Groups != nil { h.registerGroups() }
//	   ⚠ 必须是**独立**的 if，不能并进 APIKeys / Channels 那两处 ——
//	     分组与它们是三件独立的事，共用门会让"只配了分组"的部署静默看不到这个功能
//	     （路由 404，前端按 manifest 只会觉得功能不存在，没有任何报错线索）。
//	④ cmd/server/main.go 里 groupstore.New(filepath.Join(authsRoot, "groups.json"))
//	   并注入 admin.Config.Groups。
//
// 在那之前本函数恒返回 nil ⇒ 四个端点回 enabled=false / 404，不影响既有功能。
func (h *Handler) groupsStore() *groupstore.Store { return nil }

// registerGroups 注册 /admin/groups 系列端点（接线后在 New 里调用）。
func (h *Handler) registerGroups() {
	h.register("GET /admin/groups", h.groupsList)
	h.register("POST /admin/groups", h.groupsCreate)
	h.register("POST /admin/groups/update", h.groupsUpdate)
	h.register("POST /admin/groups/delete", h.groupsDelete)
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
		Models: models, ModelCount: len(models), CreatedAt: g.CreatedAt,
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
	g, err := s.Create(req.Name, req.Description, req.Models)
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
	var models []string
	if req.Models != nil {
		models = *req.Models
		if models == nil {
			models = []string{} // JSON 里的 null 也当成"清空"，与 [] 同义
		}
	}
	g, err := s.Update(req.ID, req.Name, req.Description, models)
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
	// ⚠ 这里**不做**级联清理（key 里对该分组的引用）。理由：
	//   · 级联要跨到 apikey 存储，admin 现在的依赖里没有它（P5 才接）
	//   · 更要紧的是语义：删分组与清 key 必须是**两次可分别重试**的操作。
	//     塞进一个请求里，一旦清理失败，用户看到的是"删除失败"，但分组其实已经没了。
	if err := s.Delete(req.ID); err != nil {
		writeGroupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
