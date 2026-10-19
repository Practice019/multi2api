package account

import (
	"net/http"
	"time"
)

// captcha.go —— 匿名端点的人机校验接缝（T10，抄 new-api 的「注册/登录加一道题」）。
//
// # 为什么是"接缝 + 可选"
//
// 与 `Limiter` 同一理由（见 http.go 的长注释）：实现（本地 SVG 算术题 /
// Cloudflare Turnstile / hCaptcha）各有各的依赖与失效模式，而**端点契约**只有
// 一件："这道题答过没有"。本包不认识 `internal/authlimit`，只认识下面这个接口，
// 于是三件事各自独立可测：
//
//	接线错了   → 端点回 captcha_required 但题发不出来（一眼可见）
//	题算错了   → authlimit 自己的测试红（不牵动端点）
//	端点忘了判 → captcha_test.go 红
//
// nil = 不启用人机校验（默认值）。**不做**"空接口就当通过"那种写法：那会让
// "忘接线"与"故意关掉"这两种状态在代码里长得一样。
type Captcha interface {
	// Issue 发一道新题，返回题号与它的 SVG（自包含、答案不在里面）。
	Issue(now time.Time) (id string, svg string)
	// Verify 校验答案。**一次性**：无论对错该题都作废（否则同一道题可以被
	// 反复试到对，人机校验的意义就没了 —— 那正是 authlimit 里
	// "wrong → still consumed" 那条测试钉住的语义）。
	Verify(id, answer string, now time.Time) bool
}

// PathCaptcha 发题端点。与登录/注册同批挂在门禁之外：**发题本身不需要身份**
// （要身份才能拿题，等于"没登录就不许证明自己不是机器人"）。
const PathCaptcha = "/admin/auth/captcha"

// captchaTTLSeconds 回给前端的建议有效期（秒）。
//
// 这只是**展示**用（前端拿它做倒计时/过期提示），真正的过期判定在实现里
// （`authlimit.Captcha` 自己的 ttl）。两边为什么各有一份：实现是"权威"，
// 这里回的是"给用户看的" —— 前端不该为了显示一个倒计时去反推实现内部参数。
// 所以这个值是**约定**，不是实现读出来的；实现方改 ttl 时要同步这里。
const captchaTTLSeconds = 300

// captchaMaxAnswer 答案长度上限（防"一亿位数字"这种把比较/日志撑爆的输入）。
// 本地题是两位数以内的算术，6 位足够；Turnstile 那种长 token 实现不适用本
// 校验 —— 那种实现应当在 `Verify` 里自己判长度。这里只挡明显异常的输入。
const captchaMaxAnswer = 64

// issueCaptcha GET /admin/auth/captcha —— 发一道题。
//
// 未启用（Captcha == nil）时回 404 `captcha_disabled`：前端据此**隐藏**整块
// 校验区（而不是显示一个空框）。选 404 而不是 403/200+空题：这是"本部署没有
// 这个资源"，语义与"你没权限"、"题是空的"都不同，前端可以照字面处理。
func (h *HTTP) issueCaptcha(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if h.cfg.Captcha == nil {
		writeError(w, http.StatusNotFound, "captcha_disabled", "本部署未启用人机校验")
		return
	}
	// 发题也限速：否则它可以被当成"廉价随机数源"刷（每次都在服务端留一条
	// pending），把内存吃满。桶名与登录/注册分开 —— 正常用户一次登录可能
	// 换一两次题，不该挤掉他真正的登录配额。
	if !h.allow(w, r, "captcha") {
		return
	}
	id, svg := h.cfg.Captcha.Issue(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"id":          id,
		"svg":         svg,
		"ttl_seconds": captchaTTLSeconds,
	})
}

// checkCaptcha 校验请求体里带的人机校验答案；true = 放行。
//
// # 为什么"缺失"与"答错"回**同一个** code
//
// 区分开就给了调用方一个探测口："这个题号存在吗？"（拿 id 枚举 + 响应差异）
// 与"我猜的方向对不对？"。他需要知道的只有一件事：**重新拿题、重新答**。
// 前端据此就在 `captcha_required` 分支里自动换一题（改动后旧题作废，不换
// 就会拿着废题反复失败 —— 这是这个功能最容易做错的一处）。
//
// # 为什么失败也消耗配额
//
// `allow` 已经在调用方那边先跑过了（注册/登录各自一道闸），所以这里不再限速：
// 把"答错"也算一次匿名配额，才能让"刷题"这件事有成本。
func (h *HTTP) checkCaptcha(w http.ResponseWriter, r *http.Request, id, answer string) bool {
	if h.cfg.Captcha == nil {
		return true // 未启用：本函数是 no-op，端点行为与 T3 时完全一致
	}
	if len(id) > 128 || len(answer) > captchaMaxAnswer {
		// 明显异常的输入不送进实现（也顺手挡掉把它当日志炸弹的可能）。
		writeError(w, http.StatusBadRequest, "captcha_required", "请完成人机校验后再试")
		return false
	}
	if h.cfg.Captcha.Verify(id, answer, time.Now()) {
		return true
	}
	// 401/403 都不合适：这不是身份问题，是"这道题现在不能用"。
	writeError(w, http.StatusBadRequest, "captcha_required", "请完成人机校验后再试")
	return false
}
