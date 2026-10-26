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
	"strconv"
	"strings"

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

// ── B3：多 Key 的批量动作 ──────────────────────────────────────────────────

// channelsKeysSummary GET /admin/channels/keys/summary?id=ch-x
//
// 「查状态」的汇总视图：几把启用、几把停用、当前选择模式。
// 只给计数与掩码，不给明文（明文仍只有 keys?reveal=1 那一个显式入口）。
func (h *Handler) channelsKeysSummary(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	views, err := s.KeysView(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	enabled, disabled := 0, 0
	for _, v := range views {
		if v.Status == channelstore.StatusEnabled {
			enabled++
		} else {
			disabled++
		}
	}
	mode := ""
	if ch, ok := s.Get(id); ok && ch != nil {
		mode = ch.ChannelInfo.KeySelectionMode
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "key_count": len(views), "enabled": enabled, "disabled": disabled,
		"key_selection_mode": mode, "keys": views,
	})
}

// channelsKeysStatusAll POST /admin/channels/keys/status-all  {"id":"ch-x","status":1|2}
//
// 全启 / 全禁。逐个下标调 SetKeyStatus（**每次它自己持 per-channel 锁**）——
// 刻意不在外层再持一把锁：那一层已经持了，外面再持就是自锁（sync.Mutex 不可重入）。
//
// 代价写清楚：不是单个事务，中途失败会留下"改了一半"的状态；返回的 changed 就是
// 已经改掉的条数，调用方据此知道发生了什么（重试是幂等的：再跑一次会把剩下的改完）。
func (h *Handler) channelsKeysStatusAll(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID     string `json:"id"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	// 与单把操作同一口径：只允许 1/2（3 是系统写的自动停用）。
	if req.Status != channelstore.StatusEnabled && req.Status != channelstore.StatusManuallyDisabled {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "status 只能是 1(启用)/2(手动停用)；3(自动停用)由系统写入",
		})
		return
	}
	views, err := s.KeysView(req.ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	reason := ""
	if req.Status == channelstore.StatusManuallyDisabled {
		reason = channelstore.ReasonManual
	}
	changed := 0
	for i := range views {
		if views[i].Status == req.Status {
			continue
		}
		if err := s.SetKeyStatus(req.ID, i, req.Status, reason); err != nil {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": "已改 " + strconv.Itoa(changed) + " 把后失败: " + err.Error(), "changed": changed})
			return
		}
		changed++
	}
	after, _ := s.KeysView(req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": changed, "key_count": len(views), "keys": after})
}

// channelsKeysDeleteDisabled POST /admin/channels/keys/delete-disabled  {"id":"ch-x"}
//
// 删掉所有**已停用**的 key（保留启用中的）。
//
// ⚠ 必须**从大到小**删：RemoveKey 按下标删并把后面的前移，正序删会漏掉一半
// （删完 0，原本的 1 变成 0，而循环已经走到 1 了）。这个 off-by-one 在
// "停用的 key 恰好相邻"时才暴露，而那时用户已经点了几十次 —— 如实写在代码里。
func (h *Handler) channelsKeysDeleteDisabled(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	views, err := s.KeysView(req.ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	var idxs []int
	for i := range views {
		if views[i].Status != channelstore.StatusEnabled {
			idxs = append(idxs, i)
		}
	}
	removed := 0
	for i := len(idxs) - 1; i >= 0; i-- {
		if err := s.RemoveKey(req.ID, idxs[i]); err != nil {
			writeJSON(w, http.StatusBadRequest,
				map[string]any{"error": "已删 " + strconv.Itoa(removed) + " 把后失败: " + err.Error(), "removed": removed})
			return
		}
		removed++
	}
	after, _ := s.KeysView(req.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "key_count": len(after), "keys": after})
}

// channelsKeysMode POST /admin/channels/keys/mode  {"id":"ch-x","mode":"random"|"round_robin"}
//
// 多 Key 的选择模式（hyxapi 的 multi_key_mode）。空串 = 保持原值 —— 与
// POST /admin/channels 里 key_selection_mode 的口径一致（那边也是空 = 不改）。
func (h *Handler) channelsKeysMode(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode != "random" && mode != "round_robin" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "mode 只能是 random / round_robin"})
		return
	}
	if err := s.SetKeySelectionMode(req.ID, mode); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key_selection_mode": mode})
}
