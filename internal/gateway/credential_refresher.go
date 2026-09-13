// credential_refresher.go —— 第五个扩展点：上游自报「怎么刷新我的凭证」。
//
// # 为什么必须有这个扩展点（这是一个实测的真 bug，不是预防性设计）
//
// 与 `CredentialLoader`（读凭证）完全同构的漏网：
//
//	「读凭证」被抽成了扩展点（CredentialLoader），于是核心按上游分派；
//	「刷新凭证」**没有**被抽出来，于是核心只能写死一个上游的 client。
//
// 出站循环（internal/server/handler.go）里只有一份 `cfg.Upstream`
// —— 那是 **workbuddy 的 `upstream.Client`**。它对着**任意上游**选出来的账号
// 调 `RefreshToken`：
//
//	请求 codearts/GLM-5.2
//	  → PickFor("codearts", ...) 选出 codearts 账号（选号是对的）
//	  → cfg.Upstream.RefreshToken(codearts 账号)
//	  → workbuddy 的客户端读它认识的 RefreshToken 字段 → 空
//	  → 返回 "no refreshToken"
//	  → 上层把这个 codearts 账号标记失败并冷却
//	  → 池子里 codearts 的号被逐个烧光
//	  → 503 no_healthy_account: no refreshToken
//
// 实测证据：一次 `codearts/glm-5.3-flash` 请求后，codearts 账号的
// `err_total` 从 3 涨到 6，而 workbuddy 三个号的 `last_err` 始终是
// `0001-01-01T00:00:00Z`（它们的凭证没问题，只是根本轮不到它们）。
//
// 更糟的是它的**归因是反的**：被惩罚的是无辜的 codearts 账号，
// 真正出错的（用错了客户端）那一边没有任何痕迹。
//
// # 为什么不是「核心按 provider ID 分派」
//
// 那会让核心知道每个上游的凭证格式与续期协议 —— 等于把
// 「加新上游核心零改动」这条判据打破。凭证是**上游的事实**：
//
//	workbuddy → OAuth refresh token 换 accessToken（普通 Bearer）
//	codearts  → DPoP 签名的 refresh_token grant，且 refresh_token **一次性消费**，
//	            必须全程持锁串行（见 codearts.Auth.LockRefresh 的注释）
//
// 两者的并发要求都不同（一个可以并发刷，另一个刷两次就是「一次成功一次失败，
// 且失败方可能覆盖掉成功结果」），无法用一段核心代码覆盖。
//
// # 机制（与其它四个扩展点一致）
//
// 上游只实现自己有的，核心用类型断言发现：
//
//	if fr, ok := gateway.ExtOf[gateway.CredentialRefresher](pv); ok {
//	    err := fr.RefreshCredential(cred)
//	}
//
// **没有实现该扩展点的上游**（例如未来某个纯 API Key、不需要续期的上游）
// 对应的是「这个账号不需要刷新」—— 调用方应当跳过刷新、直接用凭证发请求，
// 而不是报错（见 handler.go 里 `ExtOf` 失败分支的注释）。
package gateway

// CredentialRefresher 上游自报「怎么刷新我的凭证」。
//
// 与 CredentialLoader 成对：一个管「从磁盘读出一份凭证」，一个管
// 「把一份**在内存里**的凭证续期」（通常同时把新凭证写回磁盘）。
//
// # 为什么参数是 CallerOwned 的 Credential 而不是「给我一个 uid」
//
// 核心拿不到上游的凭证结构（`Credential.Secret` 是 any，核心**不允许**读）。
// 所以核心只能把「一份组装好的凭证」原样交还给上游，由上游自己断言类型。
// 这与 `Provider.Chat(ctx, cred, body)` 的形状一致 —— 核心只搬不读。
//
// ⚠ **实现必须原地更新 `cred.Secret` 指向的那份凭证**，因为核心持有的
// 就是同一个对象（账号池里的那份）。若实现改为返回一份新凭证，核心
// 就必须把它写回池子 —— 那会让「刷新」这件事在核心侧多出一套状态同步。
type CredentialRefresher interface {
	// RefreshCredential 续期一份凭证。
	//
	// 成功时：`cred.Secret` 指向的凭证已被**原地更新**，
	// 且实现应当尽力把它落盘（落盘失败不应当让整个刷新失败 ——
	// 内存里的凭证已经可用，下次启动才会用到旧的那份）。
	//
	// 失败时：返回 error。调用方（出站循环）会把它当作「这个账号现在用不了」
	// 处理：记一次错误、换下一个号。
	//
	// ⚠ 实现必须自己处理并发：
	//
	//	workbuddy → 持 auth.Auth 的锁（防与 SaveAtomic 并发读到半更新状态）
	//	codearts  → 持 Auth.refreshMu，且必须在**读 refresh_token 之前**拿锁
	//	            （refresh_token 一次性，锁晚一步就会有两次消费）
	//
	// 核心**不**为这里加锁：锁的粒度是上游的事实（codearts 要串行整个
	// 网络往返，workbuddy 只需要保护写回）。
	RefreshCredential(cred Credential) error
}
