// credential_expiry.go 第七个扩展点：上游自报「这份凭证什么时候过期」。
//
// # 为什么已经有 Credential.ExpiresAt 了还需要它
//
// `gateway.Credential` 里确实有 `ExpiresAt time.Time`（见 provider.go）。但那个
// 字段在**账号池的存储路径上并不承载 codearts 的事实**，原因是：
//
//	池子对 codearts 的存储形态是"不透明 secret"（pool.SecretOf 通道），
//	而投影给核心的 `auth.Auth` 只有 {UID, Nickname} —— ExpiresAt 被丢掉了。
//
// 更关键的是：**即使把 ExpiresAt 补进那份投影，它也会立刻过期**。
// 续期会原地更新 `*codearts.Auth`（secret 那一份），而 `auth.Auth` 投影是
// 启动时建的快照 —— 两者会分叉。这正是本项目上一轮刚修完的那类 bug
//（"续期写到另一个对象上"）。
//
// 所以权威只能是**活的 secret**，而核心不许解释它（判据 3：核心不认识
// 上游凭证结构）。于是问**拥有它的上游**：这就是本扩展点。
//
// # 与 CredentialRefresher 完全同构
//
// 两者都是"把一份不透明的凭证交回给它的上游，让上游自己解释"：
//
//	CredentialRefresher.RefreshCredential(cred)  → 续期
//	CredentialExpiryExt.TokenExpiry(cred)        → 问过期时刻
//
// 核心全程只搬不读，加第八个扩展点仍不改核心。
package gateway

// CredentialExpiryExt 上游自报「这份凭证什么时候过期」。
//
// # 为什么返回 (int64, bool) 而不是 time.Time
//
// 与 `Credential.ExpiresAt`（time.Time，零值表示"没有"）不同，这里用
// **显式的 ok**。理由是零值语义在这个场景里不够用：
//
//	ExpiresAt 为零值 → "没有过期信息"？
//	                  还是"1970 年就过期了"？
//
// 对 codearts 这种凭证文件可能是手写的上游，两者都必须能表达。所以
// ok=false 明确表示"这个上游的凭证没有可读的过期时间"，与"过期时刻恰好是 0"
// 分开。前端据此渲染 `—`（未知），而不是渲染成"已过期"。
//
// # 调用方必须遵守的一条
//
// **`ok && at > 0` 才填字段**，且字段用**指针**表达未知
//（`AccountView.TokenExpireSec *int64`）。
//
// ⛔ 绝不要写 `sec := at - time.Now().Unix()` 再无条件取址 ——
// 那会在 at=0 时造出一个约 -17.9 亿的负值，前端把它渲染成"已过期"，
// 比不显示更糟（用户实测过这个形态）。
type CredentialExpiryExt interface {
	// TokenExpiry 报告这份凭证的过期时刻（Unix 秒）。
	//
	//	ok=true,  at>0  → 到期时刻
	//	ok=true,  at<=0 → 与 ok=false 同义（调用方按未知处理）
	//	ok=false        → 上游不提供过期信息，前端显示 `—`
	//
	// 实现应当**只读**，不要在这里发网络请求：它是账号列表渲染路径上的调用，
	// 每个账号一次。需要探测才能知道过期的上游，应当返回 ok=false。
	TokenExpiry(cred Credential) (at int64, ok bool)
}
