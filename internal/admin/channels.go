// channels.go 管理端「渠道」CRUD（对标 new-api 的渠道设置）。
//
// # 三件事按仓库既有约定做
//
//  1. **未启用就不注册路由**（与 /admin/apikeys 同一条判据）：没注入 Channels 存储时
//     路由不存在 ⇒ 前端按 manifest 的 admin_routes 不渲染操作区，而不是渲染出一个点了会 501 的按钮。
//  2. **key 列表只回掩码**：列表接口回 `sk-abc…xyz` 形状（复用 apikey.MaskID，一个事实来源），
//     要复制明文走**显式**的 /admin/channels/reveal —— 那是一个"我知道我在做什么"的动作，
//     而不是"顺手把明文塞进列表"。列表接口是轮询的，明文塞进去等于每次轮询都把 key 发一遍。
//  3. **保存前校验映射环**：modelmap.Validate 在这里拦下 A→B→A。
//     环在请求时才炸的话，用户看到的是一个莫名其妙的上游 400，排查方向直接跑偏。
package admin

import (
	"log"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/modelmap"
)

// channelsStore 取渠道存储；nil = 未启用该功能。
func (h *Handler) channelsStore() *channelstore.Store { return h.cfg.Channels }

// registerChannels 注册 /admin/channels 系列端点（New 里调用）。
func (h *Handler) registerChannels() {
	h.register("GET /admin/channels", h.channelsList)
	h.register("POST /admin/channels/discover", h.channelsDiscover) // 「模型自动发现」：编辑时一键回填上游模型名
	h.register("POST /admin/channels", h.channelsUpsert)
	h.register("POST /admin/channels/delete", h.channelsDelete)
	h.register("POST /admin/channels/status", h.channelsStatus)
	h.register("POST /admin/channels/reveal", h.channelsReveal)
	// 多 key 运维（对标 new-api 的「Key 管理」）：加/删/启停都按**下标**动，不动别人。
	// ⚠ 不能只靠 POST /admin/channels 整体覆盖 —— 表单不回显明文，覆盖会把前端不知道的 key 抹掉。
	h.register("GET /admin/channels/keys", h.channelsKeysList)
	h.register("POST /admin/channels/keys/add", h.channelsKeysAdd)
	h.register("POST /admin/channels/keys/status", h.channelsKeysStatus)
	h.register("POST /admin/channels/keys/delete", h.channelsKeysDelete)
}

// channelView 是对外的渠道视图：**key 只出掩码**（明文走 reveal）。
type channelView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         int               `json:"type"`
	BaseURL      string            `json:"base_url"`
	KeyMasks     []string          `json:"key_masks"`
	KeyCount     int               `json:"key_count"`
	Models       []string          `json:"models"`
	ModelMapping map[string]string `json:"model_mapping"`
	Group        string            `json:"group"`
	Priority     int               `json:"priority"`
	Weight       int               `json:"weight"`
	Status       int               `json:"status"`
	Enabled      bool              `json:"enabled"`
}

func viewOf(ch *channelstore.Channel, models []string) channelView {
	v := channelView{
		ID: ch.ID, Name: ch.Name, Type: ch.Type, BaseURL: ch.BaseURL,
		Models: models, ModelMapping: ch.ModelMap, Group: ch.Group,
		Priority: ch.Priority, Weight: ch.Weight, Status: ch.Status,
		Enabled:  ch.Status == channelstore.StatusEnabled,
		KeyMasks: make([]string, 0, len(ch.Keys)), KeyCount: len(ch.Keys),
	}
	for _, k := range ch.Keys {
		v.KeyMasks = append(v.KeyMasks, apikey.MaskID(k.Key))
	}
	return v
}

// channelsList GET /admin/channels
func (h *Handler) channelsList(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		// 与 apikeys 同款：未启用就回空集 + enabled=false（前端据此不渲染操作区）
		writeJSON(w, http.StatusOK, map[string]any{"channels": []channelView{}, "enabled": false})
		return
	}
	list := s.List()
	out := make([]channelView, 0, len(list))
	for _, ch := range list {
		out = append(out, viewOf(ch, ch.Models))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channels": out,
		"enabled":  true,
		// 能力索引的规模（"对外可服务的 (渠道,模型) 对数"）—— 一眼看出渠道是否真被展开成能力
		"ability_count": len(s.Abilities()),
	})
}

// channelReq 是保存渠道的入参。keys 用 []string 而不是结构化 []ChannelKey：
// 前端表单填的就是"一行一个 key"，每 key 状态由后端补成 Enabled（要停某个 key 走 status 接口，
// 那是运维动作、不是新建表单的事 —— 表单里塞每 key 状态只会让人填错）。
type channelReq struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         int               `json:"type"`
	BaseURL      string            `json:"base_url"`
	Keys         []string          `json:"keys"`
	Models       []string          `json:"models"`
	ModelMapping map[string]string `json:"model_mapping"`
	Group        string            `json:"group"`
	Priority     int               `json:"priority"`
	Weight       int               `json:"weight"`
	Status       int               `json:"status"`
	// KeySelectionMode 多 key 的选法（""/"random"/"round_robin"）。空 = 保持原值（新建时 = 随机）。
	KeySelectionMode string `json:"key_selection_mode"`
}

// channelsUpsert POST /admin/channels —— 新建（无 id）或更新（带 id）。
func (h *Handler) channelsUpsert(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req channelReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name 必填"})
		return
	}
	if req.BaseURL == "" && strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "base_url 必填（Key 型渠道只填 Base URL + Key）"})
		return
	}
	// 映射**保存前**校验：环在请求时才炸会让用户看到莫名其妙的上游 400
	if err := modelmap.Mapping(req.ModelMapping).Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "模型映射非法: " + err.Error()})
		return
	}

	ch := &channelstore.Channel{
		ID: req.ID, Name: req.Name, Type: req.Type, BaseURL: strings.TrimRight(req.BaseURL, "/"),
		Models: req.Models, ModelMap: req.ModelMapping, Group: req.Group,
		Priority: req.Priority, Weight: req.Weight, Status: req.Status,
	}
	if ch.Status == 0 {
		ch.Status = channelstore.StatusEnabled
	}
	created := false
	if ch.ID == "" {
		ch.ID = newChannelID()
		created = true
	} else if old, ok := s.Get(ch.ID); ok {
		// 更新：保留原 key（表单不回显明文 ⇒ 不回传 keys 就表示"不改 key"）
		// 以及原 ChannelInfo（key 选择模式是既有设置，不能因为这次没传就被清掉）。
		if len(req.Keys) == 0 {
			ch.Keys = old.Keys
		}
		ch.ChannelInfo = old.ChannelInfo
	}
	// 追加 key，**按内容去重**：同一把 key 填两遍会让轮询连续选中同一个条目
	// （界面上看是"轮询没生效"，实际是数据里有两份相同的 key）。
	seen := make(map[string]bool, len(ch.Keys)+len(req.Keys))
	for _, k := range ch.Keys {
		seen[k.Key] = true
	}
	for _, raw := range req.Keys {
		k := strings.TrimSpace(raw)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		ch.Keys = append(ch.Keys, channelstore.ChannelKey{Key: k, Status: channelstore.StatusEnabled})
	}
	// 选择模式：空 = 保持原值；显式给了才改（并且只认两个合法值，
	// 因为第三个值会让 PickKeyIdx 静默落回随机 —— 用户以为设了轮询其实没设）。
	if req.KeySelectionMode != "" {
		switch req.KeySelectionMode {
		case "random", "round_robin":
			ch.ChannelInfo.KeySelectionMode = req.KeySelectionMode
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "key_selection_mode 只能是 random / round_robin（空 = 保持原值）",
			})
			return
		}
	}
	ch.ChannelInfo.MultiKey = len(ch.Keys) > 1
	if err := s.Upsert(ch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "created": created, "channel": viewOf(ch, ch.Models)})
}

// channelsDelete POST /admin/channels/delete  {"id": "..."}
func (h *Handler) channelsDelete(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	// 先记下这条渠道**声明过哪些模型** —— 删除之后 ch 就查不到了，
	// 而这些名字正是要清理的引用。
	//
	// 为什么用「渠道声明的模型」而不是「目录快照」：渠道声明的名字是
	// 删除这一刻的确切事实；目录快照可能因为上游抖动而暂时为空，
	// 拿它做清理会误删用户精心挑的白名单（见 apikey.PruneModels 的注释）。
	var declared []string
	var channelName string
	if ch, ok := s.Get(req.ID); ok && ch != nil {
		declared = append(declared, ch.Models...)
		// 渠道的 model_mapping 值也是内部名，同样要清。
		for _, internal := range ch.ModelMap {
			declared = append(declared, internal)
		}
		channelName = ch.Name
	}

	if err := s.Delete(req.ID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}

	// ── 级联清理：删渠道 ⇒ 该渠道的模型在 API key 里也不该再出现 ──
	//
	// 用户的原话：「删除之后有关这个渠道的都不要了，语义不就是这样么」。
	// 对 —— 留着一个指向已不存在模型的别名，比没有更糟：用户看到它、
	// 复制它发请求，得到的却是 unknown_provider。
	out := map[string]any{"ok": true}
	if ks := h.apiKeysStore(); ks != nil && len(declared) > 0 {
		if res, perr := ks.PruneModels(declared); perr != nil {
			// 清理失败**不**回滚渠道删除：渠道已经删掉了，回滚会让用户
			// 看到"删了又回来"。如实报告哪一步没做干净即可。
			log.Printf("admin: 渠道 %s 已删除，但清理 API key 引用失败: %v", channelName, perr)
			out["prune_error"] = perr.Error()
		} else if res.KeysTouched > 0 {
			log.Printf("admin: 渠道 %s 已删除，连带清理 %d 把 API key 里的 %d 个模型引用: %v",
				channelName, res.KeysTouched, len(res.ModelsGone), res.ModelsGone)
			out["pruned_keys"] = res.KeysTouched
			out["pruned_models"] = res.ModelsGone
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// channelsStatus POST /admin/channels/status  {"id": "...", "status": 1|2|3}
func (h *Handler) channelsStatus(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID     string `json:"id"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	switch req.Status {
	case channelstore.StatusEnabled, channelstore.StatusManuallyDisabled, channelstore.StatusAutoDisabled:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status 只能是 1(启用)/2(手动停用)/3(自动停用)"})
		return
	}
	if err := s.SetStatus(req.ID, req.Status); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// channelsReveal POST /admin/channels/reveal  {"id": "..."} —— 回**明文** key，供复制。
//
// 为什么单独一个端点：列表接口是轮询的（前端每次刷新都会打），把明文塞进列表等于
// 每次轮询都把全部渠道的 key 发一遍。而这个端点只在用户主动点「显示/复制」时调用。
// 它已经不要求额外口令 —— 会话门禁就是门槛（与"登录后凭据可看可复制"的既有口径一致）。
func (h *Handler) channelsReveal(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 必填"})
		return
	}
	ch, ok := s.Get(req.ID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道不存在: " + req.ID})
		return
	}
	keys := make([]string, 0, len(ch.Keys))
	for _, k := range ch.Keys {
		keys = append(keys, k.Key)
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": ch.ID, "keys": keys})
}

// newChannelID 造渠道 id。用 crypto/rand 而不是自增序号：序号在"删一条再建一条"时会重号，
// 而那些 id 会被能力索引与日志引用 ⇒ 重号会让历史日志指向另一条渠道。
func newChannelID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ch-fallback"
	}
	return "ch-" + hex.EncodeToString(b[:])
}
