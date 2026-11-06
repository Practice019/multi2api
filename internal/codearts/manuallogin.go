package codearts

// manuallogin.go —— 服务器部署下的「手动粘贴回跳地址」逃生路径（gateway.ManualLoginExt）。
//
// # 抄的样板
//
// 形态抄 `internal/monkeycode/login.go` 的 ManualLogin + handleManualComplete：
// 端点挂在 AdminRoutes() 上、逻辑挂在 `*Provider` 上、并带编译期断言。
//
// ⚠ **没有**抄 `internal/cline`：那边是 `var _ gateway.LoginFlow = (*loginFlow)(nil)`
// 的"访问器形态" —— 正是 `gateway/contract_test.go` 记录的实测事故形态
// （测试走访问器全绿，生产走 `ExtOf` 类型断言必然失败）。方法集匹配是 Go 类型
// 断言的硬要求：**访问器的返回值不参与方法集匹配**。
//
// # 为什么需要它（用户实测场景）
//
// 页内 OAuth 的回调是 `http://127.0.0.1:<port>/oauth/callback`（oauth.go:223）——
// 它指向的是**浏览器所在机器**的回环地址。网关跑在服务器上时，用户浏览器的回调
// 永远打不进服务器端口：授权走完了、地址栏里也带着 code，就是进不了账号池。
//
// 官方 CLI 自己有手动模式（平台 code 页展示密文 code，用户复制粘贴进终端），
// 本文件把它对齐到控制台：**前端渲染粘贴框 → POST 本端点 {state, code}**。
//
// # 换证链**完全复用**，没有第二套
//
// 本地回调服务器收到 code 时做的唯一一件事是
// `s.deliver(callbackResult{Code: code})`（oauth.go:234-246）。本文件做的
// **也只有这一件事**：把用户粘贴来的 code 投进**同一个** `*Session`。
// 之后 `Poll → Wait → Exchange → credentialFrom` 全部是既有代码，一行不复制。
//
// 为什么这样就够：
//   - `Session.resultCh` 是**带缓冲(1)** 的 channel，`deliver` 又由 `once` 守卫
//     ⇒ 「先粘贴、后轮询」与「先轮询、后粘贴」**两种顺序都成立**
//     （轮询那侧由 login.go 的 `once.Do` 起后台 goroutine 阻塞在 `Wait()` 上）。
//   - `deliver` 的 `once` 语义 == 本地回调的语义：**第一个到达的 code 生效**，
//     重复粘贴被忽略（与"回调只该来一次"一致）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：扩展点与登录流**必须挂在 *Provider 上**。
//
// ⚠ 少了 ManualLoginExt 这一条：核心的 `fillManualLogin` 用
// `gateway.ExtOf[gateway.ManualLoginExt](p)` 做类型断言（admin/schedule.go:340），
// 断言不匹配**不报任何错**，表现为 manifest 下发 `login.manual = null`
// ⇒ 界面上没有「粘贴回跳 URL 完成」的输入框，也没有「添加账号」按钮。
// 断言把这种"静默失效"提前到编译期。
var _ gateway.ManualLoginExt = (*Provider)(nil)

// ⚠ LoginFlow 这条也是**独立**要钉的（codearts 原来只钉了 `(*loginFlow)`，
// 那是内部类型；核心问的是 `*Provider`）。见 login.go:114 的说明。
var _ gateway.LoginFlow = (*Provider)(nil)

// manualCompletePath 粘贴完成端点。
//
// 与 monkeycode 的 `/admin/monkeycode/login/complete` 同构（路径里带上游 id，
// 因为核心是**按路径**把请求转给对应上游的 AdminRoutes 的）。
const manualCompletePath = "/admin/codearts/login/complete"

// manualLoginInstructions 给用户看的操作说明（中文，与本仓惯例一致）。
const manualLoginInstructions = "在本机浏览器里打开授权页并完成登录。" +
	"登录成功后浏览器会跳到 127.0.0.1 的回调地址；" +
	"如果页面提示「无法访问 / 连接被拒绝」（网关在服务器上时是正常的），" +
	"就把**地址栏里那一整条地址**复制下来粘到下面 —— " +
	"形如 http://127.0.0.1:端口/oauth/callback?code=…&state=… 。" +
	"也可以只粘 code（此时按界面上这次登录的 state 完成）。"

// ManualLogin 自报「粘贴回跳地址完成」的端点与指引（gateway.ManualLoginExt）。
//
// 只声明"有没有这条逃生路径"，不碰任何状态 —— 真正的投递在 handleManualComplete。
func (p *Provider) ManualLogin() (string, string) {
	return manualCompletePath, manualLoginInstructions
}

// extractPasted 从用户粘贴的内容里取出 (state, code)。
//
// 支持四种形态（用户实际会粘什么，就都收什么）：
//
//	① 完整回跳地址  http://127.0.0.1:8080/oauth/callback?code=abc&state=xyz
//	② 裸 query 串   code=abc&state=xyz   （也可以以 ? 开头）
//	③ 参数在 fragment 的变体  http://…/callback#code=abc&state=xyz
//	④ 只粘 code     abc                 （state 回落到界面给的 stateHint）
//
// 任何形态都**不 panic**：取不到就返回可读错误，由 handler 转成 400。
func extractPasted(pasted, stateHint string) (state, code string, err error) {
	raw := strings.TrimSpace(pasted)
	if raw == "" {
		return "", "", errors.New("没有粘贴任何内容")
	}
	// 「像地址/查询串」的判据（有其一即当带参数的串处理）：
	//   · 有 scheme（`://`）· 以 `/` 或 `?` 开头 · 含 `?` 或 `#` · 含 `=` 分隔符
	// 反过来，只有**不含任何这些标记**的串才当"裸 code"。
	//
	// ⚠ 这条判据的第一版写错了：只查了 `code=`，于是
	// `http://…/callback?state=X`（有地址、但**没有** code）会掉进"裸 code"分支，
	// 把整条 URL 当 code 投出去 —— 那种"看起来成功"的错最难查。
	addrish := strings.Contains(raw, "://") ||
		strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "?") ||
		strings.ContainsAny(raw, "?#") ||
		strings.Contains(raw, "=")
	if !addrish {
		// 裸 code：state 只能来自界面。
		return strings.TrimSpace(stateHint), raw, nil
	}
	// 定位 query 起点：优先 `?`，其次 `#`（fragment 变体），都没有就用整串。
	q := raw
	if j := strings.IndexAny(raw, "?#"); j >= 0 {
		q = raw[j+1:]
	}
	q = strings.TrimPrefix(q, "?")
	// 丢掉 fragment 尾巴（若 query 在前、fragment 在后）
	if j := strings.IndexByte(q, '#'); j >= 0 {
		q = q[:j]
	}
	values, perr := url.ParseQuery(q)
	if perr != nil {
		return "", "", fmt.Errorf("粘贴的地址解析失败：%v", perr)
	}
	c := strings.TrimSpace(values.Get("code"))
	if c == "" {
		return "", "", errors.New("粘贴的地址里没有 code 参数 —— 请确认复制的是**授权完成后的回跳地址**")
	}
	s := strings.TrimSpace(values.Get("state"))
	if s == "" {
		s = strings.TrimSpace(stateHint)
	}
	return s, c, nil
}

// completeManual 把粘贴来的 code 投进 state 对应的会话。
//
// ★ 这是本功能的全部：**复用本地回调那条投递路径**（`Session.deliver`），
// 不自己换证、不自己落盘。凭证由既有的 Poll/Exchange 链路产出。
func (f *loginFlow) completeManual(state, code string) error {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if state == "" {
		return errors.New("缺少 state：请把整个回跳地址粘回来，或重新发起一次登录")
	}
	if code == "" {
		return errors.New("没有 code：请把整个回跳地址粘回来")
	}
	f.mu.Lock()
	sess, ok := f.sessions[state]
	f.mu.Unlock()
	if !ok || sess == nil {
		// 复用既有错误语义：state 不存在 = 已兑换 / 已过期 / 从未发起。
		return errStateGone
	}
	real, ok := sessionFor(sess.oauth)
	if !ok {
		// 测试替身路径（见 login.go 的 sessionFor 注释）：替身不参与真实投递。
		return errors.New("授权会话不可投递（测试替身）")
	}
	// ★ 与本地回调**同一条**投递路径。首个到达者生效（once 语义）。
	real.deliver(callbackResult{Code: code})
	return nil
}

// handleManualComplete 粘贴完成端点：POST {state, code} → 投递 → 既有链路换证。
//
// 响应语义与 monkeycode 的同名端点保持一致（`{"ok":…}` / `{"error":…}`），
// 但**不在这里同步换证**：codearts 的换证是**轮询式**的（核心按 state 反复 Poll），
// 所以这里只回"已收到"，凭证由下一次 Poll 取走。
func (p *Provider) handleManualComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "只接受 POST"})
		return
	}
	var body struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}
	// 限长，避免有人往这里灌大 body（与 admin.go 的既有写法一致：解码失败即 400）。
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}

	state, code, err := extractPasted(body.Code, body.State)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false,
			"error": "没有 state：请把**整个回跳地址**粘回来（里面自带 state），" +
				"或先在界面上重新发起一次登录再粘 code",
		})
		return
	}

	// 取**同一个**缓存实例 —— 绝不能新建（新建 = sessions 各是一份空 map，
	// 见 login.go:127-131 记的实测 bug）。
	f, ok := p.LoginFlow()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "本部署未配置登录流程"})
		return
	}
	lf, ok := f.(*loginFlow)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "登录流程类型异常"})
		return
	}
	if err := lf.completeManual(state, code); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"state":   state,
		"message": "已收到授权码，正在换取凭证；界面会在下次轮询时自动完成。",
	})
}
