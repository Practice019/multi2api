package phanthywork

import "workbuddy2api/internal/gateway"

// credentialext.go —— 本包对两个"凭证自述"扩展点的**编译期断言**与设计说明。
//
// # 为什么这两个扩展点在别的上游是"用户报的 bug"，在这里是"默认就该有"
//
// 两个扩展点（CredentialExpiryExt / CredentialTokenExt）都是因为同一个失败形态
// 才被引进 gateway 的：
//
//	账号池投影只给 {UID, Nickname, FilePath}，真凭证走不透明 secret 通道
//	⇒ 界面「Token」列读投影读不到 → 显示 `—`
//	⇒ 用户读成"这个号没有 token"，而真相是"我们没在投影里读到它"
//
// 对本上游，这个失败形态是**必然**的（不是概率事件）：PhanthyWork 的凭证是
// 嵌套结构 + 两条独立的过期来源（毫秒字段与 JWT 的 exp），任何"按投影读字段"
// 的实现都会落空。
//
// 所以本包从第一天就实现它们 —— 而不是等用户报"Token 列是 `—`"。
//
// # 实现的约束（两个方法都必须满足）
//
//	只读、不发网络：它们是账号列表渲染路径上的调用（每账号一次）
//	不知道就返回 ok=false / false：**绝不编造** 0（那会渲染成"1970 已过期"）
//
// 方法体在 provider.go（TokenExpiry / HasToken），编译期断言在 login.go 顶部
// 的那一组 var _ = … 里。
var (
	_ gateway.CredentialExpiryExt = (*Provider)(nil)
	_ gateway.CredentialTokenExt  = (*Provider)(nil)
)
