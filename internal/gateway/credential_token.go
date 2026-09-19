package gateway

// credential_token.go —— 「这份凭证有可用的 access token 吗」这个问题的归属。
//
// # 为什么必须有它（用户实测：workbuddy-intl 登录成功后 token 列恒为 `—`）
//
// 账号列表的 `has_token` 原来只读 `Pool.AuthByUID()` —— 那是**核心的通用投影**
// （`*auth.Auth`，见 internal/admin/admin.go 的 accountViews）。
//
// 但按上游分子目录之后，管理台的两条入池路径（`accountsReload` 与
// `pollViaFlow`）交给池子的都是**裸投影**（只有 UID/Nickname），
// 真凭证走池的不透明 secret 通道（`Pool.SecretOf`）：
//
//	auths = append(auths, &auth.Auth{UID: c.UID, Nickname: c.Nickname})
//
// 于是非默认上游（第二个实例）的账号在投影里**永远没有 token**：
//
//	workbuddy-intl: has_token=false   ← 实测（secret 里明明有 accessToken）
//	workbuddy:      has_token=true    ← 默认上游，投影里就带着 token
//
// 界面把「我们没在投影里读到 token」说成了「这个号没有 token」——
// 而真相是**信息并不缺失，只是它在 secret 里**。
// 用户据此以为登录失败，实际上凭证完全正常。
//
// # 为什么不直接把 secret 塞进投影
//
// 那会违反「核心不得读 Secret」（见 Credential 的注释）：核心一旦知道
// `*auth.Auth` 长什么样，"加新上游核心零改动"这条判据立刻失效。
//
// # 为什么不是"让入池路径带上完整 *auth.Auth"
//
// 治标不治本，而且会造出第二份凭证对象：`accountsReload` 传入的 `a` 是
// 从磁盘重扫的**新对象**，与 secret 里那份（续期会原地更新它）必然分叉 ——
// 那正是本项目反复吃过的"续期写到另一个对象上"的形态。
//
// 所以按本仓库既有手法（`CredentialExpiryExt` 之于到期时刻）：
// **另开一个可选接口，问拥有这份凭证的上游**。不实现 = 行为与之前逐字节相同。
type CredentialTokenExt interface {
	// HasToken 报告这份凭证是否带有**可用的** access token。
	//
	// 实现应当**只读**、不发网络请求：它是账号列表渲染路径上的调用，
	// 每个账号一次。需要探测才知道的上游应当返回 false（界面显示 `—`）。
	HasToken(cred Credential) bool
}
