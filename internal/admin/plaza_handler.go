// plaza_handler.go 模型广场端点：**所有分组里的对外模型**（用户要求）。
//
// # 与 `/admin/channels/catalog` 的分工
//
//	/admin/channels/catalog  按**渠道**算：这条渠道能服务什么（含快照版本）
//	/admin/channels/plaza    按**分组**算：我们**对外承诺**了哪些模型
//
// 后者是新加的，因为运维实际关心的问题是"客户端能看到哪些模型名"，
// 而那个集合由**分组**决定（`ResolvePolicy` 走分组）。
//
// # 只读
//
// 不写任何东西。广场是"当前配置长什么样"的投影。
package admin

import (
	"net/http"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/groupstore"
)

// channelsPlaza GET /admin/channels/plaza —— 分组里的对外模型全集。
//
// 只读、管理员限定、禁缓存。
func (h *Handler) channelsPlaza(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.principalFrom(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	var groups []*groupstore.Group
	if h.cfg.Groups != nil {
		groups = h.cfg.Groups.List()
	}
	var channels []*channelstore.Channel
	if h.cfg.Channels != nil {
		channels = h.cfg.Channels.List()
	}
	writeJSON(w, http.StatusOK, channelstore.BuildModelPlaza(groups, channels))
}
