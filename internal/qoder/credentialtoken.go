package qoder

import "workbuddy2api/internal/gateway"

// credentialtoken.go —— 本上游自报「这份凭证有可用的 access token 吗」。
//
// # 为什么需要它（用户报的：Token 列一直是 `—`）
//
// 界面「Token」列的判据是 `has_token`，而它原来只读**账号池投影**
//（`Pool.AuthByUID()` 返回的核心 `*auth.Auth`）。对本上游而言投影里
// 那个字段**永远是空的**，因为字段名对不上：
//
//	核心 auth.Parse 读 `auth.accessToken`（驼峰）
//	本上游落盘写的过期/令牌字段名与它不同
//
// 于是：
//
//	has_token = false    ← 投影里读不到
//	（但凭证文件里的 access_token 完全正常）
//
// 界面把「我们没在投影里读到 token」说成了「这个号没有 token」——
// 而真相是**信息并不缺失，只是它在 secret 里**。
//
// # 这正是 gateway.CredentialTokenExt 存在的理由
//
// 该扩展点的文档（credential_token.go 的文件头）记录的就是这个失败形态 ——
// 当年 workbuddy-intl 登录成功后 token 列恒为 `—`，根因完全相同
//（非默认上游的条目进池时只带 {UID, Nickname} 的裸投影，真凭证走
// `Pool.SecretOf` 的不透明通道）。当时的结论是：
//
//	让上游自己回答「我的凭证有 token 吗」，核心不猜投影里的字段。
//
// 本上游属于同一类（凭证是不透明的私有结构），所以按同一条修。
//
// 编译期断言。
var _ gateway.CredentialTokenExt = (*Provider)(nil)

// HasToken 报告这份凭证是否带 access token（只读、不发网络）。
//
// ⚠ 读的是**活凭证**（`cred.Secret` 里那份 `*Auth`），不是池投影 ——
// 投影是启动快照且字段名与上游落盘不一致，越读越错。
func (p *Provider) HasToken(cred gateway.Credential) bool {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return false
	}
	return a.AccessToken != ""
}
