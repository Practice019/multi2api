// quota_ext.go Loomy 实现 gateway.QuotaExt —— "我的额度怎么取"。
//
// # 额度在哪（用户报"额度没做"之后的实测结论）
//
// **不在模型代理端点上。** 实测打了 17 条候选路径（/points、/points/summary、
// /user/points、/balance、/quota、/account/points…），`loomyad.xunfei.cn/api/v1`
// 上一律返回 404；换成业务域 `loomy.xunfei.cn` 也全是 404 或 Next.js 的 404 页。
//
// 原因是**请求根本不经过渲染层**：客户端渲染层调的是
// `window.electronAPI.points.queryRecordsV2(...)` —— 那是 Electron 的 IPC，
// 真正的 HTTP 请求由**主进程**发出，URL 在主进程 bundle 里，渲染层看不到。
//
// 而客户端把结果**明文缓存**在本机 Local Storage 的 `loomy-points-summary` 键下
// （实测值：`{"balance":8000,"dailyBalance":5000,"updatedAt":"2026-09-14T08:15:28.627Z"}`）。
// 那就是本实现的数据源。
//
// # 为什么展示 `balance`（总余额）而不是 `dailyBalance`（每日额度）
//
// 客户端有两个并行的账本（手册第 4.3 节实测）：
//
//	balance      总余额   —— **不随自然日重置**（邀请/活动/充值累积）
//	dailyBalance 每日额度 —— 服务端按自然日自动重置为 5000
//
// 这一列只能装一个数（`gateway.QuotaView` 是单值），选 `balance` 的理由：
//
//  1. **语义跨天稳定**。`balance` 只有消耗与发放两种变化，昨天的值今天仍然
//     是"还剩多少"的（一个上界）。而 `dailyBalance` 在跨日那一刻**语义就变了**
//     （服务端已重置，缓存里却还是昨天的数）—— 用一个会突然改变含义的字段
//     填充同一列，会让"这个数字是什么意思"变成要看日期才知道的事。
//  2. **它才是硬上限**。总余额耗尽时请求会拿到 `insufficient credits`
//     （手册第 9 节：日额度会自动回血，但 `balance` 不会）。
//     而日额度只限制"一天花多少"。
//  3. 账号池的选号权重用的也是这个数（`pool.QuotaView.Effective()`），
//     它要的是"这个号还剩多少"的整体判断。
//
// `dailyBalance` 没有被丢掉：诊断端点（adminroute.go 的 client-store）
// 把两个账本一起下发，跨日与否也在那里说明。
//
// # 必须说清的一条局限（不许藏）
//
// 这是**客户端缓存**：客户端在跑的时候它会被刷新，而**网关自己消耗的积分
// 不会写回它**（网关不在客户端进程里，没人去更新那个键）。
// 所以本值应当读成"截至 `updatedAt` 的余额"，而不是"此刻的余额"。
// 诊断端点会连 `updatedAt` 一起给出，让这个偏差**可查**。
package loomy

import (
	"log"
	"strings"

	"workbuddy2api/internal/gateway"
)

// RefreshQuota 取该账号的当前额度（gateway.QuotaExt）。
//
// 返回 (额度视图, ok)：
//
//	ok=false           uid 为空（调用方传错了）—— 调用方跳过
//	ok=true + 有数据   本机缓存里有**这个 uid** 的余额 → 写回池
//	ok=true + 取不到   HasData=false → 调用方**不写池**，界面显示 `—`
//
// # 为什么"取不到"返回 HasData=false 而不是 0
//
// 0 会被读成"这个号没额度了"（用户可能会据此删号），而"读不到"是另一件事：
// 网关可能部署在另一台机器上、客户端可能没在跑、缓存可能属于别的账号。
// 契约把这两件事分得很清楚（见 gateway.QuotaView.HasData 的注释）。
//
// # 为什么失败只记日志、不返回 error
//
// 额度刷新是**展示性**操作。它失败不该让 /admin/accounts 报错，
// 也不该刷错误日志把真正的故障淹掉。与 codearts 同一条判据。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	if strings.TrimSpace(uid) == "" {
		return gateway.UnknownQuota(), false
	}

	// ── 首选：**直接问积分网关**（本轮改的就是这一步）──
	//
	// 上一轮这里只读本机客户端缓存，于是有两个天然缺陷：
	//
	//	① 只有"本机登录的那一个账号"有值，池里其它 loomy 号恒为 `—`
	//	② 值只在客户端运行时才更新，**网关自己的消耗不会写回**那个缓存
	//
	// 而 `GET /api/v1/points/records` 是**按 session 逐个账号查**的，
	// 上面两条同时消失。这才是"额度查询"应有的样子。
	if a := p.authByUID(uid); a != nil {
		ctx, cancel := pointsCtx()
		bal, _, err := p.Balance(ctx, a.Session)
		cancel()
		if err == nil {
			return gateway.CreditsQuota(clampNonNegative(bal)), true
		}
		// 查失败（网络/上游 5xx/session 失效）→ 回落到本机缓存，
		// 而不是直接报未知。缓存至少对"本机那一个号"是可信的历史值。
		log.Printf("loomy: 积分网关查额度失败 uid=%s: %v（回落到本机缓存）",
			shortUID(uid), err)
	}

	// ── 回落：本机客户端缓存（仅对"本机登录的那个账号"有效）──
	dir := p.effectiveClientDir()
	if dir == "" {
		// 本机没有客户端数据目录（网关跑在服务器上、或客户端还没装）。
		// 这是**正常部署形态**，不是故障 —— 只记一次信息性日志。
		log.Printf("loomy: 额度未知 —— 积分网关查不到，且本机没有 Loomy 客户端数据目录")
		return gateway.UnknownQuota(), true
	}

	sess, _, err := ReadLocalAuth(dir)
	if err != nil {
		log.Printf("loomy: 额度未知 —— 读取客户端登录态失败 dir=%s: %v", dir, err)
		return gateway.UnknownQuota(), true
	}
	if sess == nil {
		log.Printf("loomy: 额度未知 —— 客户端数据目录 %s 里没有 loomy-auth-session"+
			"（客户端未登录？）", dir)
		return gateway.UnknownQuota(), true
	}
	// ⚠ 缓存是**本机登录的那个账号**的，不是"每账号一份"。
	//
	// 若把本机缓存的余额贴到池里另一个 loomy 账号上，会得到一个
	// **看起来完全合理的错数**（比"显示 —"糟得多）：用户会照着它
	// 决定用哪个号、甚至据此删号。
	//
	// 所以 uid 不匹配时**明确回答未知**。这不是保守，是唯一正确的答案。
	if sess.UID != uid {
		log.Printf("loomy: 额度未知 —— 本机客户端登录的是 %s，而被问的是 %s"+
			"（缓存只对本机登录的那个账号有效）", shortUID(sess.UID), shortUID(uid))
		return gateway.UnknownQuota(), true
	}

	pts, err := ReadLocalPoints(dir)
	if err != nil {
		log.Printf("loomy: 额度未知 —— 读取客户端积分缓存失败 dir=%s: %v", dir, err)
		return gateway.UnknownQuota(), true
	}
	if pts == nil {
		// 登录态在、但积分摘要在：客户端还没把摘要写下来（刚登录/从未打开设置页）。
		log.Printf("loomy: 额度未知 —— 客户端未缓存积分摘要（uid=%s）", shortUID(uid))
		return gateway.UnknownQuota(), true
	}
	return gateway.CreditsQuota(clampNonNegative(pts.Balance)), true
}

// clampNonNegative 负数（理论上的透支）clamp 到 0。
//
// 负额度在展示与选号权重上都没有意义（负数权重会被当成"比没有记录还差"）。
// 与 codearts 同一条处理。
func clampNonNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// authByUID 在本上游的凭证目录里按 uid 找一份凭证。
//
// # 为什么本包能自己找到（不需要核心给）
//
// 凭证目录就是 `p.authDir`（AuthDirExt 报的那个），格式也是本包自己的
// `loomy*.json` —— 所以"按 uid 取 session"是本包**完全自足**的一件事，
// 不必让核心为它加一个"把凭证交给上游"的扩展点。
//
// 找不到返回 nil：那不是错误（该 uid 不属于本上游），调用方据此回落。
func (p *Provider) authByUID(uid string) *Auth {
	if p == nil || strings.TrimSpace(uid) == "" {
		return nil
	}
	list, err := LoadDir(p.authDir)
	if err != nil {
		return nil
	}
	for _, a := range list {
		if a.UID == uid {
			return a
		}
	}
	return nil
}

// 编译期断言：Provider 实现额度扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"界面上额度一直空着"。
var _ gateway.QuotaExt = (*Provider)(nil)
