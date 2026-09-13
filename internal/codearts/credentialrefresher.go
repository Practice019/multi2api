package codearts

// credentialrefresher.go —— codearts 通过 `gateway.CredentialRefresher` 自报
// 「我的凭证怎么刷新」。
//
// # 为什么必须有它（这是本次修复的核心之一）
//
// 出站循环原先恒用 `cfg.Upstream`（workbuddy 的 upstream.Client）刷新凭证。
// codearts 的账号被选出来之后，被**workbuddy 的客户端**拿去 RefreshToken：
//
//	workbuddy 的实现读的是 auth.Auth.RefreshToken（它认识的字段）
//	而池子里存的是一份 **codearts.Auth**（不同的结构体，不同类型的断言）
//	→ "no refreshToken"
//	→ 上层把这个 codearts 账号标记失败并冷却
//	→ 503 no_healthy_account
//
// 实测：一次 codearts/glm-5.3-flash 请求就让 codearts 账号 err_total +3，
// 而 workbuddy 三个号的 last_err 纹丝不动。
//
// # 为什么刷新不能放在核心
//
// codearts 的续期与 workbuddy **并发语义都不同**：
//
//	refresh_token 是**一次性消费**的
//	必须带 DPoP 证明（要用凭证里那把 ES256 私钥现场签名）
//	必须全程持 Auth.refreshMu，且锁要在**读 refresh_token 之前**拿到
//
// 任何一条写不进「一段通用核心代码」。所以它属于上游。

import (
	"errors"
	"fmt"
	"log"

	"workbuddy2api/internal/gateway"
)

// 编译期断言（与 gateway.Provider / gateway.AdminExt / gateway.CredentialLoader 并列）。
var _ gateway.CredentialRefresher = (*Provider)(nil)

// RefreshCredential 续期一份 codearts 凭证。
//
// # 这里只做「断言 + 转发 + 落盘」，为什么
//
// `Client.RefreshToken(a *Auth)` 已经把 codearts 续期的全部复杂度关在里面：
//
//	持 refreshMu（防 refresh_token 双消费）
//	检查 refresh_token / DPoP 私钥，缺任一返回「需重新登录」类的错误
//	DPoP 签名 → 换取新的 AK/SK/SecurityToken
//	**原地更新** a 的字段（核心持有的就是同一个对象，所以不需要回写池子）
//
// 本方法与 workbuddy 那一侧同构：核心把组装好的凭证交回来，上游自己认识它。
//
// # 为什么错误要包一层
//
// 核心的失败分支会 `errors.As(err, &ue)` 找 `*upstream.Error` 判断
// ErrSessionDead。codearts 的 `Client.RefreshToken` 返回的是 `fmt.Errorf`
// 包装的普通 error —— 它**不是** upstream.Error，所以核心会走
// 「NoteError（喂熔断计数）+ 换号」这条路径。
//
// 这是刻意的：codearts 的「需重新登录」与 workbuddy 的 session dead
// **不是同一件事**（前者可以靠后台续期任务恢复，后者必须人工重登），
// 用同一个分类会让一个 codearts 的临时凭证问题把号永久禁用。
// 这里显式包一层 %w 是为了保留错误链（日志里能看到 refresh_failed 的原文），
// 而**不**把它伪装成 upstream.Error。
//
// # 落盘语义（与 workbuddy 的差异，刻意保留）
//
// workbuddy 的 RefreshCredential 里落盘失败只 log（凭证在内存里已可用）。
// 这里**不落盘** —— 与改造前完全一致：codearts 的续期由它自己的
// 后台任务（jobs.go 的 RefreshExpiring）负责持久化，请求路径上的刷新
// 只更新内存，下一次后台任务会把新凭证写回。请求路径加一次写盘会让
// 每次续期都产生一次磁盘 IO，而它并不改变任何可观测行为。
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if p.client == nil {
		return errors.New("codearts: 上游客户端未接（无法续期）")
	}
	if err := p.client.RefreshToken(a); err != nil {
		log.Printf("codearts: 凭证续期失败 uid=%s: %v", a.UID, err)
		return fmt.Errorf("codearts: 续期凭证: %w", err)
	}
	return nil
}
