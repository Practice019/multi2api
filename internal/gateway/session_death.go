package gateway

// SessionDeathPolicyExt 上游自报「我的会话失效能不能自愈」。
//
// # 为什么这件事必须由上游回答
//
// 同一个分类（ErrKindSessionDead）在两家的含义**完全不同**：
//
//	workbuddy  会话死了可以自动续期（有 refresh token + 后台续期任务），
//	           所以一次 12153 很可能是**瞬时抖动**（网络闪断、刷新竞态）——
//	           核心因此给了「连续 3 次才禁用」的门控（Pool.NoteSessionDead）。
//	loomy      **没有任何续期手段**：凭证只有一个 session，没有 refresh token、
//	           没有 TTL 字段、客户端也不刷新（见 internal/loomy/credential.go）。
//	           它的「登录已失效」是**终态** —— 留在池里只会让每次选中都多消耗一个
//	           必然失败的往返，而用户永远看不到"有个号需要重登"这件事。
//
// 把这条差异写在核心，就等于让核心替所有上游决定"你的失效能不能自愈" ——
// 而那正是本仓反复禁止的事（核心不得依赖任何具体上游的事实）。
//
// # 机制
//
// 与其它扩展点一致：上游只实现自己有的，核心用类型断言发现。
// **没有实现的上游**保持既有行为（连续 N 次才禁用）⇒ 纯增量、可选，
// 不改变任何既有部署的行为。
type SessionDeathPolicyExt interface {
	// SessionDeathIsPermanent 报告：这个上游的凭证一旦报「会话失效」，
	// 是否**不可能自动恢复**（只能人工重登）。
	//
	// 返回 true   ⇒ 核心一次即禁用（Pool.Disable），不等计数门控。
	// 返回 false  ⇒ 走既有的「连续 N 次」门控。
	//
	// 必须纯函数：不发网络请求、不改状态（它在请求路径上被调用）。
	SessionDeathIsPermanent() bool
}
