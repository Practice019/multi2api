// clientlogin.go 管理台「本地登录」面板的后端：把本机 WorkBuddy 桌面客户端
// 切换到账号池里的任意账号。
//
// 这是本控制台**唯一会改动客户端本机状态**的接口，因此比别的面板多两道约束：
//
//  1. 必须显式传 confirm=true —— 前端弹二次确认，后端再验一次，
//     避免误点、脚本重放或 CSRF 之类的顺手调用把登录态换掉。
//  2. 切换前强制备份、失败即中止（备份逻辑在 clientlogin 包里）——
//     没有回滚能力就不做破坏性操作。
//
// 详细的可逆性设计与文件布局见 internal/clientlogin 包注释。
package admin

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"workbuddy2api/internal/clientlogin"
)

// clientLoginStatus 客户端登录态 + 可切换候选人列表。
func (h *Handler) clientLoginStatus(w http.ResponseWriter, r *http.Request) {
	m := h.cfg.ClientLogin
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "本控制台未启用客户端登录管理")
		return
	}
	st, err := m.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "读取客户端登录态失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// clientLoginAct 是所有客户端登录动作的请求体。
// Confirm 必须是显式的 true：零值 false 代表「没确认」，一律拒绝。
type clientLoginAct struct {
	UID     string `json:"uid"`
	Confirm bool   `json:"confirm"`
}

func decodeClientLoginAct(r *http.Request) clientLoginAct {
	var b clientLoginAct
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&b)
	}
	return b
}

// clientLoginSwitch 把客户端登录态切到指定账号。
func (h *Handler) clientLoginSwitch(w http.ResponseWriter, r *http.Request) {
	m := h.cfg.ClientLogin
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "本控制台未启用客户端登录管理")
		return
	}
	body := decodeClientLoginAct(r)
	if !body.Confirm {
		writeError(w, http.StatusPreconditionRequired, "切换客户端登录态是破坏性操作，需要在界面上二次确认")
		return
	}
	if body.UID == "" {
		writeError(w, http.StatusBadRequest, "缺少 uid")
		return
	}

	res, err := m.Switch(body.UID)
	if err != nil {
		switch {
		case err == clientlogin.ErrSameAccount:
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, clientlogin.ErrClientRunning):
			// 客户端在跑时写盘会被它原地覆盖：这不是故障，是前置条件不满足。
			// 原样返回错误文本（里面已写明要先退出客户端），不加「切换失败」前缀。
			writeError(w, http.StatusConflict, err.Error())
		default:
			// 切换失败的常见原因（找不到凭证、备份失败、快照写不进去）
			// 都属于「当前环境不允许」，用 409 比 500 更贴近语义，前端也好区分。
			writeError(w, http.StatusConflict, "切换失败: "+err.Error())
		}
		return
	}
	log.Printf("admin: client-login switch uid=%s source=%s backup=%s", body.UID, res.Source, res.BackupPath)
	writeJSON(w, http.StatusOK, res)
}

// clientLoginRestore 回滚到上一次切换前的登录态。
func (h *Handler) clientLoginRestore(w http.ResponseWriter, r *http.Request) {
	m := h.cfg.ClientLogin
	if m == nil {
		writeError(w, http.StatusServiceUnavailable, "本控制台未启用客户端登录管理")
		return
	}
	body := decodeClientLoginAct(r)
	if !body.Confirm {
		writeError(w, http.StatusPreconditionRequired, "回滚是破坏性操作，需要在界面上二次确认")
		return
	}
	res, err := m.Restore()
	if err != nil {
		// 无备份 / 备份即当前态都属于「这次回滚没有意义」；
		// 客户端在跑则是前置条件不满足 —— 三种都用 409，且原样给出错误文本。
		if errors.Is(err, clientlogin.ErrAlreadyBackedUp) || errors.Is(err, clientlogin.ErrClientRunning) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusConflict, "回滚失败: "+err.Error())
		return
	}
	log.Printf("admin: client-login restore uid=%s", res.UID)
	writeJSON(w, http.StatusOK, res)
}
