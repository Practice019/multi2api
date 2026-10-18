package admin

// credentialdir.go —— 「某个上游的凭证目录在哪」这个判据**只写一份**。
//
// # 为什么必须是一份（而不是三处各写一遍）
//
// 同一个问题（"dir 是哪个目录"）被三条路径问过：
//
//	导入    POST /admin/accounts/import      → 写文件到哪
//	重载    POST /admin/accounts/reload      → 扫哪个目录
//	删除    DELETE /admin/accounts/{uid}     → 兜底找凭证文件（本次新加）
//
// 判据写三遍的代价不是"不好看"，而是**将来只改对一处**，于是：
// 导入写进了 A 目录、重载只扫 B 目录 ⇒ 界面报"导入成功"但账号不出现。
// 这类缺陷在本项目出现过（见 codearts 的目录拼接注释），
// 所以这里刻意把顺序写死成唯一的一份实现。
//
// # 判据顺序（每个调用方都按它）
//
//	① 上游自报的 `AuthDirExt.AuthDir()`  —— 目录是**上游的事实**，核心不猜
//	② 否则回落 `<凭证根>/<provider>`      —— 凭证根 = AuthsBase，其次 AuthDir，最后 "auths"
//
// ⚠ 与 `reloadProviderInto` 的自报分支**逐字同源**：那边也是"先问 AuthDirExt"。
//   所以对实现了该扩展点的上游（loomy / trae / zcode / minimax …），
//   三条路径拿到的目录是同一个值。

import (
	"path/filepath"
	"strings"

	"workbuddy2api/internal/gateway"
)

// credentialDirFor 取该上游的凭证目录。
func (h *Handler) credentialDirFor(pid string) string {
	base := strings.TrimSpace(h.cfg.AuthsBase)
	if base == "" {
		base = strings.TrimSpace(h.cfg.AuthDir)
	}
	if base == "" {
		base = "auths"
	}
	if p, ok := h.providerByID(pid); ok {
		if ax, ok := gateway.ExtOf[gateway.AuthDirExt](p); ok {
			if d := strings.TrimSpace(ax.AuthDir()); d != "" {
				return d
			}
		}
	}
	return filepath.Join(base, pid)
}
