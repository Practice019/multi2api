package workbuddy

// credentialrefresher.go —— workbuddy 通过 `gateway.CredentialRefresher` 自报
// 「我的凭证怎么刷新」。
//
// # 为什么需要（与 CredentialLoader 是同一个漏网的两个面）
//
// 出站循环里原来只有一份 `cfg.Upstream`（workbuddy 的 upstream.Client），
// 却对**任意上游**选出来的账号调 `RefreshToken`。对 workbuddy 自己的账号
// 「碰巧是对的」—— 与 CredentialLoader 的注释里那句总结同款：
//
//	核心写死 workbuddy 的实现，等于核心知道 workbuddy 的凭证格式，
//	于是**别的上游那条路径必然坏**。
//
// 实测后果见 gateway.CredentialRefresher 的注释（codearts 的 err_total 被
// 一次正常请求推高 3，号被逐个烧光 → 503 no_healthy_account）。
//
// 补上这个扩展点后，两个上游各自报名，核心按 `ExtOf` 分派、
// **不硬编码任何上游的续期协议**。

import (
	"log"

	"workbuddy2api/internal/gateway"
)

// 编译期断言（与 CredentialLoader / AuthDirExt 两条并列）。
//
// 不实现会在这里编译失败，而不是等到运行时「codearts 的请求莫名其妙 503」。
var _ gateway.CredentialRefresher = (*Provider)(nil)

// RefreshCredential 用 refresh_token 换新的 access token。
//
// # 这里刻意只做「workbuddy 自己那一套」
//
// 直接把 `*auth.Auth` 交给 `upstream.Client.RefreshToken` —— 它已经：
//
//	持 auth.Auth 的锁（防与 SaveAtomic 并发读到半更新状态）
//	缺 refreshToken 时返回明确错误
//	响应缺 expiresIn 时**保留旧过期时间**（避免刷新风暴）
//
// 所以本方法是一次薄转发，与 Provider.Chat 的形状一致：核心把组装好的
// 凭证交给上游，上游自己认识它。
//
// # 落盘失败为什么不让整个刷新失败
//
// 内存里的凭证**已经可用了**（token 已更新），请求可以继续发出去。
// 落盘失败的唯一后果是「下次启动用的是旧 token」—— 那是**下一次启动**的问题，
// 把它升级成「本次请求失败并冷却这个号」是拿一个未来问题换一个当下故障。
// 与改造前 handler 里的取舍一致（那里也只是 log 一行），但记日志这件事必须保留：
// 静默落盘失败会让「重启后 token 失效」变成无法排查的事。
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if err := p.client.RefreshToken(a); err != nil {
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("workbuddy: 凭证刷新成功但落盘失败 uid=%s: %v", a.UID, err)
	}
	return nil
}
