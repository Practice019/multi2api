// refresh.go MiniMax 的凭证续期（gateway.CredentialRefresher）。
//
// # ⚠ 这个文件是补一个**真实的用户报障**
//
// 用户报：控制台的 Token 列显示「已过期 10/10 17:21」，账号一直不能用 ——
// 而它本应自动续期。
//
// 根因不是"续期写错了"，而是**根本没人实现续期**：
//
//	RefreshSkewExt         "多早该刷"  ← 我实现了（返回 5 分钟窗口）
//	CredentialRefresher    "怎么刷"    ← **我没实现**
//
// 而这两者是**必须成对**的（见 gateway/refresh_skew.go 的标题：「两者必须成对」）。
//
// 后果比"没实现续期"更糟 —— 是**声明了却兑现不了**：
//
//  1. 出站循环问 needsRefreshVia → 上游说"该刷了" → 返回 true
//  2. 核心调 refreshCredential → ExtOf[CredentialRefresher] **失败**
//  3. 语义被解释成「该上游的凭证不需要刷新」→ **静默跳过**
//  4. token 就这么过期着，每次都走到 401
//
// 第 3 步那条语义本身是对的（纯 API Key 的上游确实不需要刷新），
// 但它把"我承诺了要刷却没实现"和"我的凭证本来就不用刷"混成了同一件事。
// 所以**漏实现 CredentialRefresher 不会报任何错** —— 这正是它的隐蔽之处。
//
// 修法不是"把 RefreshSkew 也删掉"（那只是把主动续期降级成被动 401 重试），
// 而是**把承诺兑现**：这里实现真正的 OAuth refresh_token 换取。
package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// refreshTimeout 续期请求的超时。
//
// 比普通业务请求短：它在出站路径上（用户正等着对话），不能久等。
const refreshTimeout = 15 * time.Second

// refreshToken 用 refresh_token 换一份新的 access_token。
//
// # 协议（照抄参照项目 `minimax-oauth.ts` 的 refreshMinimaxCredential）
//
//	POST {accountHost}/oauth2/token
//	Content-Type: application/x-www-form-urlencoded
//
//	grant_type=refresh_token
//	refresh_token=<旧值>
//	client_id=mcode-public
//	scope=agent.default
//	audience=agent-backend
//
// # ⚠ refresh_token 的轮换：两种都要兼容
//
// `parseTokenGrant(m, 旧值)` 把旧 refresh_token 作为**回退**：
//
//	服务端下发新的 → 采用新的（轮换型）
//	服务端不下发   → 沿用旧的（非轮换型）
//
// 不能写死任一边：硬要求新值会让非轮换型上游每次续期都失败；
// 硬沿用旧值会让轮换型上游在第一次续期后就作废。
//
// # 返回值
//
// 返回**新的一份 Auth**（未落盘）。调用方负责原地写回 + 落盘 ——
// 与 raccoon / cline 的分工一致。
func (c *Client) refreshToken(ctx context.Context, a *Auth) (*Auth, error) {
	if a == nil || !a.Refreshable() {
		return nil, fmt.Errorf("minimax: 凭证缺少 refresh_token，无法续期（需重新登录）")
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	form := url.Values{}
	form.Set("grant_type", refreshGrant)
	form.Set("refresh_token", a.RefreshToken)
	form.Set("client_id", oauthClientID)
	form.Set("scope", oauthScope)
	form.Set("audience", oauthAudience)

	raw, status, err := c.postForm(ctx, c.accountBase+pathToken, form)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	if status != 200 {
		// ⚠ 终态判据（照抄参照项目 minimax-auth.ts）：
		// `invalid_grant` / `expired` / `revoked` 说明 refresh_token 已死 ——
		// 反复重试只会每次请求多付一次失败往返。调用方据此禁用账号。
		er := firstNonEmpty(str(m["error"]), fmt.Sprintf("HTTP %d", status))
		if isTerminalRefreshError(er) {
			return nil, fmt.Errorf("minimax: refresh_token 已失效（%s），需重新登录该账号", er)
		}
		return nil, fmt.Errorf("minimax: 续期失败：%s（%s）", er, snippet(raw))
	}
	next, perr := parseTokenGrant(m, a.RefreshToken)
	if perr != nil {
		return nil, fmt.Errorf("minimax: 续期响应无法解析：%w", perr)
	}
	// 昵称/账号 id 服务端不会重发 —— 从旧凭证带过来，否则账号池里会突然变成匿名。
	next.Nickname = a.Nickname
	if next.AccountID == "" {
		next.AccountID = a.AccountID
	}
	return next, nil
}

// isTerminalRefreshError refresh_token 是否已**终态失效**。
//
// 判据照抄参照项目（正则 `/invalid_grant|expired|revoked/i`）。
//
// ⚠ 用子串匹配而不是等值：上游的 error 文案不统一
// （`invalid_grant` / `refresh_token_expired` / `token revoked` 都见过）。
func isTerminalRefreshError(errText string) bool {
	low := strings.ToLower(errText)
	for _, kw := range []string{"invalid_grant", "expired", "revoked"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

// ---- gateway.CredentialRefresher ----

// RefreshCredential 续期一份凭证（**原地更新 + 落盘**）。
//
// # 为什么必须原地更新字段，而不是换一份新 Auth
//
// 账号池里存的是**同一个 `*Auth` 指针**（见 cmd/server 的
// syncMiniMaxAccounts → SyncToDirWithSecrets）。若这里返回一份新对象
// 而不写回原对象，池里那份会**永远停在作废的旧 token 上** ——
// 表现为"续期明明成功了，下一个请求还是 401"，极难查。
//
// 所以逐字段写回原对象（与 raccoon / cline 同一手法）。
//
// # 落盘失败不让续期失败
//
// 内存里的凭证已经可用，本次对话应当继续；落盘失败只影响下次启动
// （会用旧的、可能已过期的 token，那时再续期即可）。
// 为了一个落盘错误让用户这次对话失败是本末倒置。
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return fmt.Errorf("minimax: 凭证类型不对（期望 *minimax.Auth，实际 %T）", cred.Secret)
	}
	if !a.Refreshable() {
		// 没有 refresh_token ⇒ 只能重新登录。这不是"不用刷"，是**刷不了** ——
		// 所以返回错误让核心记一次失败（连续失败达上限会禁用账号，
		// 提示用户重新登录），而不是静默跳过。
		return fmt.Errorf("minimax: 凭证缺少 refresh_token，需重新登录该账号")
	}
	next, err := p.client.refreshToken(context.Background(), a)
	if err != nil {
		return err
	}
	// 逐字段写回（原地）—— 见方法注释里"为什么不能换新对象"。
	if next.AccessToken != "" {
		a.AccessToken = next.AccessToken
	}
	if next.RefreshToken != "" {
		a.RefreshToken = next.RefreshToken
	}
	if next.TokenType != "" {
		a.TokenType = next.TokenType
	}
	if next.ExpiresAt != "" {
		a.ExpiresAt = next.ExpiresAt
	}
	// IssuedAt 也要写回 —— 端到端实测发现漏了它：续期后凭证变成
	// "有过期时刻但没发证时刻"，寿命推不出来 ⇒ RefreshSkew 只能走
	// 10 分钟固定兜底，用户期望的"过半就续"在第一次续期后就失效了。
	if next.IssuedAt != "" {
		a.IssuedAt = next.IssuedAt
	}
	if next.Scope != "" {
		a.Scope = next.Scope
	}
	if next.AccountID != "" {
		a.AccountID = next.AccountID
	}
	// 昵称保持（服务端不重发）。
	if err := saveInPlace(a); err != nil {
		// 只记日志，不让续期失败 —— 理由见方法注释。
		logf("minimax: 续期成功但落盘失败 uid=%s（内存里已可用）：%v", a.UID(), err)
	}
	return nil
}
