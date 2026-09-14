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
// （"续期写到另一个对象上"）。
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
// （`AccountView.TokenExpireSec *int64`）。
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

// CredentialLifetimeExt 是 CredentialExpiryExt 的**可选加强版**：
// 上游自报「这份凭证是不是**设计上就不会过期**」。
//
// # 为什么 (at, ok) 表达不了这件事（这是用户实测报出来的缺口）
//
// `TokenExpiry` 的值域只有"某个时刻"与"不知道"两种。但现实里有**第三态**：
//
//	workbuddy  accessToken 有 remind（约 60 天）        → 有到期时刻
//	codearts   STS 约 2 小时一轮                        → 有到期时刻
//	loomy      session **服务端持久化登录态，无 TTL**    → 既不是"某时刻"，
//	                                                      也不是"我们不知道"
//
// 对 loomy，`TokenExpiry` 只能回 ok=false → 界面显示 `—`
// （"该上游没有提供凭证过期时间"）。那句提示本身没错，但它把
// **"我们没读到一个时刻"** 说成了 **"信息缺失"** —— 而真相是
// **信息并不缺失，结论就是"不会过期"**。
//
// 用户看到 loomy 那格永远是一个 `—`，自然读成"这功能没做"（实测如此）。
//
// # 为什么单开一个接口，而不是给 TokenExpiry 加一种返回值
//
// 给已有接口加语义（比如约定 at==0 && ok==true 表示"永久"）会同时做两件坏事：
//
//  1. 与现有契约**相反** —— CredentialExpiryExt 的注释白纸黑字写着
//     "ok=true, at<=0 → 与 ok=false 同义"，而 codearts 正是靠这条
//     把"上游没给 exp"安全地处理成未知。改语义会让那条路径变成"永久"。
//  2. 强制**全部**实现者理解这个新哨兵：漏改一处就把"未知"说成"永久"，
//     而"永久"是个**强断言**，说错了会误导用户不去换号。
//
// 所以按本仓库既有手法（`CredentialSecretLoader` 之于 `CredentialLoader`）：
// **另开一个可选接口**，只有真的想表达三态的上游才实现它。
// 不实现 = 行为与之前**逐字节相同**（未知仍渲染 `—`）。
//
// # 判断顺序（调用方必须遵守）
//
//	TokenExpiry 给出 at>0        → 有到期时刻，**以它为准**（本接口不被问到）
//	否则问本接口 → true          → 「永久」—— 不是未知
//	否则                        → 未知 → `—`
//
// ⛔ 绝不允许两个来源同时为真：一个既有到期时刻又"永久"的凭证不存在，
//
//	先问时刻是为了让"上游同时实现了两个接口"时有一个确定的答案。
type CredentialLifetimeExt interface {
	// NeverExpires 报告这份凭证是否**没有过期时间**（设计如此）。
	//
	//	true  → 该上游的凭证不绑时间：失效只可能来自主动登出、改密、
	//	        服务端清理或风控，不会"到点自动失效"
	//	false → 有到期时间、或实现方不确定 —— 一律按未知处理
	//
	// 实现应当**只读**、不得 panic、不得发网络请求（与 TokenExpiry 同一条）。
	// 拿不准时必须返回 false：把"不确定"报成"永久"会让用户以为
	// 手里的号永远可用，而它下一条请求就可能失效。
	NeverExpires(cred Credential) bool
}
