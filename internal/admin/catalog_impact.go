// catalog_impact.go 渠道目录的**删除影响**预览（U-11 验收「删除影响明确」）。
//
// # 为什么单独一个端点而不是塞进渠道详情
//
// 详情页回答"这条渠道现在能服务什么"，而这里回答"删掉它会怎样"。
// 后者是**假设性**的：它必须能在**删除之前**问，而且不能改动任何状态。
// 塞进详情页会让人以为是同一件事。
//
// # 只读
//
// 不写任何东西。算的是 `BuildCatalog` 的当前结果，所以"影响"与
// "目录里实际有什么"是同一份数据 —— 不会出现两套算法互相矛盾。
package admin

import (
	"net/http"
	"strings"

	"workbuddy2api/internal/channelstore"
)

// catalogImpactHandler GET /admin/channels/catalog/impact?channel_id=X
//
// 只读、管理员限定、禁缓存。
func (h *Handler) catalogImpactHandler(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.principalFrom(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("channel_id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少 channel_id")
		return
	}
	// ⚠ 这里**不需要**判 `h.cfg.Channels == nil`：本路由注册在 `registerChannels()`
	// 里，没配渠道存储时整个组都不注册 ⇒ 请求根本到不了这里（404）。
	// 留一段"不可达的 501"等于给下一个人一个假的兜底。
	catalog := channelstore.BuildCatalog(h.cfg.Channels.List())
	writeJSON(w, http.StatusOK, channelstore.CatalogDeletionImpactOf(catalog, id))
}
