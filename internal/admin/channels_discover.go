// channels_discover.go 「模型自动发现」：POST /admin/channels/discover
//
// # 它解决什么
//
// Key 型渠道的模型列表要么手抄、要么去上游问。手抄必然抄错（上游改名/加模型时更明显），
// 而"问上游"这件事**只有上游自己知道** —— 所以这里不是让管理台认识某个上游，
// 而是经注入的 `cfg.ChannelProvider`（类型取自 gateway 契约）拿一个 Provider 来问。
//
// ⚠ 为什么不在这里 import 任何具体上游包：见 admin.go 里那段注释（当年的 Upstream 字段
// 就是因为这个被删的）。管理台一旦认识某个具体上游，就会长出"只对某一个上游成立的行为"。
//
// # 三种入参形态（对应三种真实用法）
//
//	{"id":"ch-x"}                      ⇒ 用已存渠道的 BaseURL + 第一个启用 key 去问（最常见：编辑时点"自动发现"）
//	{"base_url":"...","key":"..."}     ⇒ **还没保存**就先试一次（新建表单里"先看它有什么再决定"）
//	{"id":"ch-x","save":true}          ⇒ 问完把结果**写回该渠道的 models**（一键回填）
//
// ⚠ save 的语义刻意是"覆盖 models"而不是"合并"：合并会让上游删掉的模型永远赖在列表里，
// 而那个模型的请求会一直失败 —— 覆盖才反映上游当前的事实。想保留手工项就别用 save。
package admin

import (
	"encoding/json"
	"net/http"
	"sort"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
)

type discoverReq struct {
	ID      string `json:"id"`
	BaseURL string `json:"base_url"`
	Key     string `json:"key"`
	Save    bool   `json:"save"`
}

func (h *Handler) channelsDiscover(w http.ResponseWriter, r *http.Request) {
	s := h.channelsStore()
	if s == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道功能未启用"})
		return
	}
	if h.cfg.ChannelProvider == nil {
		// 接线问题，不是用户输入问题 ⇒ 说清楚，别让用户去猜"是不是我的 BaseURL 写错了"
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "服务端未注入 ChannelProvider（组合根漏接线），无法向上游查询模型"})
		return
	}
	var req discoverReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}

	var ch *channelstore.Channel
	if req.ID != "" {
		got, ok := s.Get(req.ID)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "渠道不存在: " + req.ID})
			return
		}
		ch = got
	}
	baseURL, key := req.BaseURL, req.Key
	if ch != nil {
		if baseURL == "" {
			baseURL = ch.BaseURL
		}
		if key == "" {
			key = firstEnabledKey(ch)
		}
	}
	if baseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "base_url 必填（或给一个已存在的渠道 id）"})
		return
	}
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "没有可用的 key：该渠道没有启用状态的 key，且请求里也没带 key"})
		return
	}

	id, name := "discover", "discover"
	if ch != nil {
		id, name = ch.ID, ch.Name
	}
	p := h.cfg.ChannelProvider(id, baseURL)
	if p == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ChannelProvider 工厂返回了 nil"})
		return
	}
	cred := gateway.Credential{
		Provider: id, UID: id, Nickname: name,
		// 与转发路径同一个跨上游最小公共形状（核心不知道"哪种上游怎么读 key"）
		Secret: map[string]any{"api_key": key},
	}
	ms, err := p.Models(r.Context(), cred)
	if err != nil {
		// ⚠ 错误里**不得**出现 key 明文（provider 已经保证了这一点，这里再声明一次意图）
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "向上游查询模型失败: " + err.Error(),
			"hint":  "检查 Base URL 是否指向 OpenAI 兼容端点（通常以 /v1 结尾）与 key 是否有效",
		})
		return
	}
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids) // 稳定输出：前端每次点"自动发现"看到的顺序一致

	saved := false
	if req.Save && ch != nil && len(ids) > 0 {
		updated := *ch
		updated.Models = ids
		if err := s.Upsert(&updated); err != nil {
			writeJSON(w, http.StatusInternalServerError,
				map[string]any{"error": "已查到模型但写回失败: " + err.Error(), "models": ids})
			return
		}
		saved = true
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"models": ids,
		"count":  len(ids),
		"saved":  saved,
		// 空列表要能被区分：是真的上游没有模型，还是它回了个空数组
		"note": map[bool]string{true: "上游返回了 0 个模型（检查 Base URL 是否指向 /v1）",
			false: "ok"}[len(ids) == 0],
	})
}

// firstEnabledKey 取该渠道第一个**启用**状态的 key。
//
// ⚠ 不取停用的：停用 key 去问上游只会拿到 401，然后用户会以为"自动发现功能坏了"。
func firstEnabledKey(ch *channelstore.Channel) string {
	for _, k := range ch.Keys {
		if k.Status == channelstore.StatusEnabled && k.Key != "" {
			return k.Key
		}
	}
	return ""
}
