package account

import (
	"errors"
	"net/http"
)

// autherr.go —— "这条会话现在是什么状态"到 HTTP 的**唯一**一份映射。
//
// # 为什么抽出来
//
// 回答这个问题的端点现在有两个：`GET /admin/auth/session`（前端启动时问一次）
// 与 `POST /admin/auth/password`（改口令时先确认自己还在线）。两处各写一份的
// 后果不是编译错误，而是**漂移**：同一个失效原因在一个端点回 session_expired、
// 在另一个回 auth_required，前端于是对同一种状态有两条不同提示。
//
// # 几个分支的意图（抄 new-api 的会话校验语义）
//
//	ErrSessionNotFound  Cookie 我们不认识（伪造/已清理）⇒ 401 auth_required
//	                    ⚠ 刻意与 ErrSessionExpired 分开：凭证根本没签发过，
//	                    不该提示"已过期"（那会让用户以为是自己的问题）
//	ErrSessionExpired   过了绝对有效期 ⇒ 401 session_expired
//	ErrSessionRevoked   被撤销，或**账号 AuthVersion 变了**（改口令/被下线）
//	                    ⇒ 401 session_revoked，文案点明原因，用户才知道该重登
//	ErrDisabled         账号被管理员禁用 ⇒ 403（不是 401：重登也没用）
func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSessionExpired):
		writeError(w, http.StatusUnauthorized, "session_expired", "登录已过期，请重新登录")
	case errors.Is(err, ErrSessionRevoked):
		writeError(w, http.StatusUnauthorized, "session_revoked",
			"登录已失效（口令变更或被下线），请重新登录")
	case errors.Is(err, ErrDisabled):
		writeError(w, http.StatusForbidden, "disabled", "账号已被禁用，请联系管理员")
	default:
		// 含 ErrSessionNotFound：Cookie 是我们不认识的（伪造/已清理）
		writeError(w, http.StatusUnauthorized, "auth_required", "请先登录")
	}
}
