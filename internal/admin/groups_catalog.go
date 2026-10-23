// groups_catalog.go 给分组编辑弹窗提供「可选的模型名」候选。
//
// # 为什么不能直接用 /v1/models（前端一开始就是这么做的，报错 [object Object]）
//
// /v1/models 是**调用方**端点，要 API key 鉴权。管理台页面拿的是 admin token，
// 两者不是一回事 —— 于是它稳定返回 401，界面显示「读取模型目录失败」。
//
// # 为什么也不用 /admin/models/preview
//
// 那是「倍率预览」，只覆盖走倍率体系的上游（实测 347 条：cline / monkeycode /
// qoder / trae / workbuddy …），**不含渠道**（csg / qiniuyun / senseaudio）。
// 拿它当候选会造出「分组里选得到、实际调不通」—— 而那正是分组要防的事。
//
// # 所以复用 ModelNames（与一致性检查**同一个来源**）
//
// 一致性检查判断「分组里的模型还在不在目录里」，用的就是这个快照。
// 两处共用一份数据，才不会出现「编辑器里选得到 / 检查说它陈旧」这种自相矛盾。
package admin

import (
	"net/http"
)

// groupCatalog GET /admin/groups/catalog
//
// 返回 {models:[...], incomplete:[...], ready:bool}。
//
// ready=false 表示一份都没拿到（目录完全不可用）。此时 models 为空，
// 界面应提示「目录未就绪」并保留手填入口，而不是显示「一个模型都没有」。
func (h *Handler) groupCatalog(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ModelNames == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"models":     []string{},
			"incomplete": []string{}, 
			"ready":      false,
			"error":      "模型目录未接线（装配层未注入 ModelNames）",
		})
		return
	}
	snap := h.cfg.ModelNames()
	models := snap.Names
	if models == nil {
		models = []string{}
	}
	entries := snap.Entries
	if entries == nil {
		// 老装配层没填 Entries 时，按 Name 兜底（owner 留空，前端会切前缀）。
		entries = make([]ModelCatalogEntry, 0, len(models))
		for _, n := range models {
			entries = append(entries, ModelCatalogEntry{ID: n})
		}
	}
	inc := snap.Unavailable
	if inc == nil {
		inc = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":     models,
		"entries":    entries,
		"incomplete": inc,
		"ready":      len(models) > 0,
	})
}
