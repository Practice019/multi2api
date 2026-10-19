// channels_keys.go 多 key 的运维端点（对标 new-api 的「Key 管理」）。
//
// # 为什么必须有增量端点，不能只靠 POST /admin/channels 整体覆盖
//
// 列表接口只回掩码（明文要走 reveal，那是"我知道我在做什么"的显式动作）。所以编辑一个
// 已有渠道时，前端手里**没有明文 key**；此时用整体覆盖保存，等于把"它不知道的 key"抹掉。
//
// 多 key 的日常操作是"加一把、停一把、删一把" —— 三者都必须能只针对**某一个下标**动。
// 这就是下面四个端点的理由（与 new-api 的 key 管理面板同一套语义）。
package admin

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/channelstore"
)

// channelsKeysList GET /admin/channels/keys?id=ch-xxx
//
// 回该渠道每个 key 的掩码、状态、停用原因与时间。**绝不回明文**。
func (h *Handler) channelsKeysList(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	views, err := s.KeysView(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	ch, _ := s.Get(id)
	mode := ""
	name := ""
	if ch != nil {
		mode = ch.ChannelInfo.KeySelectionMode
		name = ch.Name
	}
	// reveal=1 ⇒ 用**明文**替换掩码。
	//
	// ⚠ 允许明文的前提是"这条路径不是轮询的"：本端点只在用户主动打开 Key 管理窗口时调一次，
	// 而列表端点 /admin/channels 是每 30 秒轮询的（那里永远只给掩码）。
	// 用户的原话是「直接展示全面了」—— 掩码 gk_B…4b2e 认不出是哪一把，而这个窗口的全部用途
	// 就是"让我看清并管理这些 key"。会话门禁（管理员）就是这里的门槛。
	reveal := r.URL.Query().Get("reveal") == "1"
	if reveal && ch != nil {
		for i := range views {
			if i < len(ch.Keys) {
				views[i].Mask = ch.Keys[i].Key
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                 id,
		"name":               name,
		"keys":               views,
		"key_count":          len(views),
		"key_selection_mode": mode,
		"revealed":           reveal,
	})
}

// channelsKeysStatus POST /admin/channels/keys/status  {"id":"ch-x","index":0,"status":1|2}
//
// 手动启用(1)/停用(2)某一个 key。
//
// ⚠ index 用指针：0 是**合法下标**（第一个 key），不能用"==0 表示没传"来判断。
// 这类 off-by-one 在"第一个 key 永远操作不动"时才暴露，而那时用户已经点了几十次。
func (h *Handler) channelsKeysStatus(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID     string `json:"id"`
		Index  *int   `json:"index"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	if req.Index == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "index 必填（从 0 开始）"})
		return
	}
	// 手动操作只允许 1/2：3（自动停用）是系统写的状态，人手动"设成自动停用"没有意义，
	// 而且会让界面上的"自动禁用"标记失去含义。
	if req.Status != channelstore.StatusEnabled && req.Status != channelstore.StatusManuallyDisabled {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "status 只能是 1(启用)/2(手动停用)；3(自动停用)由系统写入",
		})
		return
	}
	reason := channelstore.ReasonManual
	if req.Status == channelstore.StatusEnabled {
		reason = ""
	}
	if err := s.SetKeyStatus(req.ID, *req.Index, req.Status, reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// channelsKeysAdd POST /admin/channels/keys/add  {"id":"ch-x","keys":["sk-a","sk-b"]}
//
// 追加 key（一行一个）。已存在的完全相同 key 会被跳过（重复 key 会让轮询重复打同一把）。
func (h *Handler) channelsKeysAdd(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID   string   `json:"id"`
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	added, err := s.AddKeys(req.ID, req.Keys)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	views, _ := s.KeysView(req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "keys": views})
}

// channelsKeysDelete POST /admin/channels/keys/delete  {"id":"ch-x","index":0}
func (h *Handler) channelsKeysDelete(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID    string `json:"id"`
		Index *int   `json:"index"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	if req.Index == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "index 必填（从 0 开始）"})
		return
	}
	if err := s.RemoveKey(req.ID, *req.Index); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	views, _ := s.KeysView(req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": views})
}
