package account

import (
	"net/http"
	"strings"
)

// change_password.go —— 自助改口令（`POST /admin/auth/password`，T9）。
//
// # 为什么这个端点必须存在（用户报障 ②）
//
// 改造前控制台的"凭据"就是 config.json 里那把 admin_token，而**改它要重启**
// 才生效 ⇒ 用户的原话是"轮换之后网页加载失败，必须重启"。现在身份走会话，
// 改口令这件事就有了正确的落点：改完**当前这个浏览器不掉线**，其它设备按设计
// 失效。两个"轮换"按钮（admin_token / api_key）早已免重启（写回 config 后
// Handler 每次读**当前**值），缺的就是这一块。
//
// # 三件事，顺序不能换
//
//	① 验旧口令 —— 验不过就 400 bad_current_password。
//	   不验的下场是：拿着别人会话的人可以把账号口令改掉（会话被劫持 ⇒ 永久接管）。
//	② SetPassword —— 它把账号的 AuthVersion +1，于是**所有**旧会话立刻失效
//	   （Lookup 比对版本号）。这正是"改口令要把别的设备踢掉"的意图。
//	③ AdvanceAuthVersion(自己的 sid, 新版本号) —— 只把**执行改动的这条**会话
//	   升到新版本，否则用户改完自己的口令会立刻被自己踢下线。抄的是 new-api
//	   `model/user_session.go` 的 `AdvanceUserSessionAuthVersion`。
//
// ②③ 之间不能"先返回再升级"：升级失败时用户手里那条 cookie 已经失效了，
// 必须把这件事如实报出来（见下面那个 500 分支）。
//
// # 身份从哪来
//
// 本端点挂在 `/admin/auth/` 下（门禁之外，与登录/注册同批），所以**自己**判身份：
// 只认**会话 cookie**，不认 admin_token。
//
// ⚠ 为什么 admin_token 不行：它没有"账号"这个主体（`Principal.UserID` 为空），
// 而改口令是改**某个人**的口令。脚本用令牌换来的是部署级权限，那件事走
// config.json，不走这里。给令牌放行会让"改的是谁的"这个问题消失 —— 于是
// 任何拿得到令牌的人都能改掉管理员的口令。
func (h *HTTP) changePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	// 限速与注册/登录**分开桶**：它是"已登录的人在试**当前**口令"的场景
	// （会话被盗后的爆破面），与匿名注册的滥用不是同一件事。
	if !h.allow(w, r, "password") {
		return
	}

	sid := SIDFromRequest(r)
	if sid == "" {
		writeError(w, http.StatusUnauthorized, "auth_required", "请先登录")
		return
	}
	// Lookup 顺带把"账号被禁用""AuthVersion 变了"都判掉 —— 别在这里重做一遍。
	_, u, err := h.cfg.Sessions.Lookup(sid, h.cfg.Accounts)
	if err != nil {
		writeAuthError(w, err)
		return
	}

	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.CurrentPassword) == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 current_password")
		return
	}
	// 口令规则与注册**同一份**（validatePassword）：换个端点放宽长度限制
	// 就是开了个后门 —— 弱口令可以在注册时被拦，却在改口令时通过。
	if err := validatePassword(body.NewPassword); err != nil {
		writeAccountError(w, err)
		return
	}
	// 新旧相同：不算错误，但要说出来。否则用户会以为"改成功了"，
	// 而他真正想要的效果（把别处在用旧口令的人踢掉）并没有发生。
	if body.NewPassword == body.CurrentPassword {
		writeError(w, http.StatusBadRequest, "same_password", "新口令与当前口令相同，未做改动")
		return
	}

	// ① 验旧口令。⚠ 参数序是 (hash, password) —— 反了会静默恒假。
	if !VerifyPassword(u.PasswordHash, body.CurrentPassword) {
		writeError(w, http.StatusBadRequest, "bad_current_password", "当前口令不正确")
		return
	}

	// ② 改口令：AuthVersion +1 ⇒ 其它设备的会话下次校验即失效。
	next, err := h.cfg.Accounts.SetPassword(u.ID, body.NewPassword)
	if err != nil {
		writeAccountError(w, err)
		return
	}

	// ③ 把自己这条会话就地升到新版本。
	if _, err := h.cfg.Sessions.AdvanceAuthVersion(sid, next); err != nil {
		// 到这里口令**已经改成功**了，只是当前会话没能跟着升级 ⇒ 用户下次请求
		// 会拿到 401 并被前端弹回登录页。必须如实说：报 200 会让用户以为
		// "还在线"，随后无声掉线（那正是本次要修的那类体验问题）。
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{
				"code":    "session_upgrade_failed",
				"message": "口令已改成功，但当前会话未能就地升级，请用新口令重新登录",
			},
			"password_changed": true,
		})
		return
	}

	// 回一个"其它设备几条会失效"的数字供前端提示。
	// 数不准也不影响安全（判据是版本号，不是这个数字），所以只做展示。
	others := 0
	for _, s := range h.cfg.Sessions.ListForUser(u.ID, sid) {
		if !s.Current {
			others++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"revoked_others": others,
		"user":           u.view(),
	})
}
