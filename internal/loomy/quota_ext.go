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
	"context"
	"errors"
	"log"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/logx"
)

// quotaFailLogInterval 同一 uid 的失败日志最小间隔（2026-11-02 加）。
//
// ⚠ 为什么需要它（真机事故）：Loomy 有 8 个账号 session 失效时，刷新路径会
// 反复撞同一批账号。没有限流时实测刷到 **1187 行/秒 ≈ 900 MB/小时**，
// 8 小时写掉 8.9 GB 日志并把系统盘顶到 80%、造成持续 I/O 阻塞。
//
// 真正的根治是让退避生效（见 RefreshQuota 失败分支里的 noteFail 调用），
// 这里限流是**第二道闸** —— 防的是"将来又出现别的高频失败路径"。
const quotaFailLogInterval = 5 * time.Minute

// quotaFailLogAt 记录每个 uid 上次打失败日志的时刻。
var quotaFailLogAt sync.Map // uid -> time.Time

// upstreamCodeRe 匹配 pointsCall 抛出的**业务码失败**原文里的 6 位码。
//
// 形态固定（见 pointsCall）：`loomy: 积分接口 /points/records?… 返回 100002: 登录已失效，请重新登录`
var upstreamCodeRe = regexp.MustCompile(`返回 (\d{6}):`)

// quotaFailReason 把一条额度查询 error 归成"给用户看的一档原因"。
//
// # 为什么要分档（用户原话："这个查询失败，其实有可能是网络的原因"）
//
// 改造前所有失败都打成同一句"积分网关查额度失败"，于是**网络抖动**与
// **账号被停用**在日志里长得一模一样 —— 用户会去删一个其实只是网络抖了一下的健康号。
//
// # 判据只认三类（够用，不多加）
//
//	· 网络/超时：net.Error 或 context.DeadlineExceeded
//	· 上游拒绝：error 里含 "返回 " + 6 位业务码（pointsCall 的固定形态）
//	· 其余（回执不是 JSON / 缺 balance 字段 / 其它）：归"回执异常"
func quotaFailReason(err error) (reason string, hint string) {
	if err == nil {
		return "未知", ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "超时", "网络慢或上游无响应，稍后会自动重试"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return "网络失败", "连不上上游，稍后会自动重试"
	}
	if m := upstreamCodeRe.FindStringSubmatch(err.Error()); m != nil {
		return "上游拒绝", "业务码 " + m[1] + "：见同行的上游原文"
	}
	return "回执异常", "上游响应不符合预期"
}

// trimLoomyPrefix 去掉 error 里重复的 `loomy: ` 前缀。
//
// ⚠ 为什么需要它：本包的 error 大多由 `fmt.Errorf("loomy: …")` 造出来，
// 而日志本身又带了 `loomy` 前缀 ⇒ 打出来是 `loomy 额度查询失败 …: loomy: 调用积分接口失败…`
// —— 前缀重复两遍。这里循环 trim（因为可能被包了多层）。
func trimLoomyPrefix(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	for strings.HasPrefix(s, "loomy: ") {
		s = strings.TrimPrefix(s, "loomy: ")
	}
	return s
}

// cacheMiss 统计"本机缓存不覆盖该 uid"的次数（L188 那条日志的替代）。
//
// # 为什么不再每个号打一条
//
// 「本机缓存只对本机登录的那一个账号有效」是**设计内的正常情况**，不是异常。
// 116 个号 × 每 5 分钟刷新 = 每天最多 3 万条纯噪音（用户抱怨"可读性不好"的正是这类）。
// ⇒ 改成每 quotaFailLogInterval 汇总一行，说清"有多少号只能靠积分网关查"。
var cacheMiss struct {
	mu   sync.Mutex
	n    int
	last time.Time
}

// noteCacheMiss 记一次缓存不覆盖，每 quotaFailLogInterval 最多汇总打一行。
func noteCacheMiss() {
	cacheMiss.mu.Lock()
	cacheMiss.n++
	n := 0
	if cacheMiss.last.IsZero() || time.Since(cacheMiss.last) >= quotaFailLogInterval {
		n = cacheMiss.n
		cacheMiss.n = 0
		cacheMiss.last = time.Now()
	}
	cacheMiss.mu.Unlock()
	if n > 0 {
		log.Printf("loomy 有 %d 个账号的额度只能靠积分网关查（本机缓存只覆盖 1 个号）—— 这是设计内正常情况；"+
			"若紧接着还有「额度查询失败」的日志，那才是要看的", n)
	}
}

// logQuotaFail 打一条失败日志，但同一 uid 在 quotaFailLogInterval 内只打一条。
func logQuotaFail(uid string, err error) {
	now := time.Now()
	if v, ok := quotaFailLogAt.Load(uid); ok {
		if at, ok := v.(time.Time); ok && now.Sub(at) < quotaFailLogInterval {
			return
		}
	}
	quotaFailLogAt.Store(uid, now)
	// ★ 2026-11-09 文案重写（用户抱怨"可读性不好"）：
	//   ① 按**失败类型**分档打印（网络/上游拒绝/回执异常）—— 否则网络抖动与账号停用长得一样
	//   ② 去掉重复的 `loomy: ` 前缀（err 自身带一个、日志前缀又带一个）
	//   ③ **删掉"（回落到本机缓存）"** —— 那是假的：调用方紧接着就返回"额度未知"，
	//      并没有回落成功（真正回落成功的那条路径在下面单独打"已改用本机缓存值"）
	reason, hint := quotaFailReason(err)
	logx.Coalesce(logx.LevelWarn, "loomy", "quota:loomy", "", "额度查询失败 uid=%s（%s）: %s —— %s",
		shortUID(uid), reason, trimLoomyPrefix(err), hint)
}

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
		pts, err := p.PointsSnapshot(ctx, a.Session)
		cancel()
		if err == nil {
			// 查到了 ⇒ 凭据是活的：清掉上一次的失效标记（否则重登后会被一直误判）。
			p.quotaAuth.mark(uid, false)
			bal, daily := pts.Available, pts.Daily
			// 顺手回填候选分档缓存（见 candidate_tier.go）。
			//
			// 这条路径（管理端「刷新额度」/ 启动后的额度刷新）本来就要查一次
			// 余额，结果与分档需要的**完全同一个数** —— 不回填就是白查一次，
			// 而选号路径只能读缓存、不能自己发请求。
			// ⚠ 用 tierOfPoints（看 DailyKnown）而不是 tierOf：dailyBalance
			// 字段缺失时 Daily 是 0，把它当「确实是 0」会把「没读到」判成
			// 「免费额度已用完」（见 points.go 的 Points.DailyKnown）。
			p.tiers.set(uid, tierOfPoints(pts))
			// 同时喂给**免费额度池**：它是选号的权威（见 freepool.go）。
			//
			// ⚠ 必须传 DailyKnown：字段缺失时 daily 是 0，而"没读到"
			// 与"确实是 0"是两件事 —— 把前者记成"已用完"会让一个可能
			// 还有免费额度的号被永久跳过。
			p.pool().noteQuota(uid, pts.Daily, pts.DailyKnown, time.Now())

			q := gateway.CreditsQuota(clampNonNegative(bal))
			// 「免费额度」列：带上**每日赠送**的余额（会按自然日重置的那部分）。
			//
			// ⚠ 只在回执里**真的有** dailyBalance 时才带 —— 否则留零值，
			// 界面显示 `—`（"这个上游不区分 / 这次没读到"）。
			// 把缺失当 0 会渲染成「免费额度已用完」，用户据此以为账号
			// 正在烧永久积分 —— 一个**看起来完全合理但错误**的结论。
			if pts.DailyKnown {
				q = q.WithFree(clampNonNegative(daily))
			}
			return q, true
		}
		// 查失败（网络/上游 5xx/session 失效）→ 回落到本机缓存，
		// 而不是直接报未知。缓存至少对"本机那一个号"是可信的历史值。
		//
		// ★★★ 2026-11-02 修复（真机事故根因）★★★
		// 这里**必须**记一次失败，否则 freePool 的失败退避（freeFailBackoff）
		// 永远不生效：`noteFail` 此前**定义了却从未被任何代码调用**，
		// 于是 f.failAt 永远是空的 ⇒ dueForRecheck 的退避判断恒为假 ⇒
		// 失败账号每拍都被判定"该重查" ⇒ drainRecheck 的无界 for 循环永不退出。
		// 实测后果：wb2api 烧 90% 单核 CPU、日志 1187 行/秒（约 900 MB/小时）。
		// 详见 docs/progress-log.md 2026-11-02 条目。
		p.pool().noteFail(uid, time.Now())
		logQuotaFail(uid, err)
		// 把"这次失败是不是凭据失效"记下来，交给 gateway.QuotaAuthFailureExt
		// 暴露给核心 —— 核心据此决定要不要摘号（判据留在本包，见 quota_auth.go）。
		// ⚠ 只有**凭据类**才记 true：网络/5xx/限流什么都不做，那会把好号踢下线。
		p.quotaAuth.mark(uid, quotaErrIsSessionDead(err))
	}

	// ── 回落：本机客户端缓存（仅对"本机登录的那个账号"有效）──
	dir := p.effectiveClientDir()
	if dir == "" {
		// 本机没有客户端数据目录（网关跑在服务器上、或客户端还没装）。
		// 这是**正常部署形态**，不是故障 —— 只记一次信息性日志。
		logx.Debugf("loomy", "额度未知 —— 积分网关查不到，且本机没有 Loomy 客户端数据目录")
		return gateway.UnknownQuota(), true
	}

	sess, _, err := ReadLocalAuth(dir)
	if err != nil {
		logx.Debugf("loomy", "额度未知 —— 读取客户端登录态失败 dir=%s: %v", dir, err)
		return gateway.UnknownQuota(), true
	}
	if sess == nil {
		logx.Debugf("loomy", "额度未知 —— 客户端数据目录 %s 里没有 loomy-auth-session"+
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
		// ★ 2026-11-09 文案重写（原来这里每个号每轮都打一条，且文案用了「被问的」这种
		//   自造词、也没说清"为什么查不到" —— 用户不知道存在本地客户端）。
		//   现在：**不在每个号上打**，只计数；由 noteCacheMiss 每 5 分钟汇总一行。
		//   理由：缓存只覆盖本机登录的那一个号，这是**设计内正常情况**，不是异常。
		noteCacheMiss()
		return gateway.UnknownQuota(), true
	}

	pts, err := ReadLocalPoints(dir)
	if err != nil {
		logx.Debugf("loomy", "额度未知 —— 读取客户端积分缓存失败 dir=%s: %v", dir, err)
		return gateway.UnknownQuota(), true
	}
	if pts == nil {
		// 登录态在、但积分摘要在：客户端还没把摘要写下来（刚登录/从未打开设置页）。
		logx.Debugf("loomy", "额度未知 —— 客户端未缓存积分摘要（uid=%s）", shortUID(uid))
		return gateway.UnknownQuota(), true
	}
	q := gateway.CreditsQuota(clampNonNegative(pts.Balance))
	// 本机缓存里的 dailyBalance 与积分网关那侧**同一个语义**，一并带上。
	// ⚠ 同样只在键真的出现过时才带（DailyKnown）—— 实测缓存里存在
	// 只写 balance 的部分更新，那时这一列必须是 `—`。
	if pts.DailyKnown {
		q = q.WithFree(clampNonNegative(pts.DailyBalance))
	}
	// ★ 2026-11-09：**真正**回落成功时才说"已改用本机缓存值"。
	//   旧文案在失败路径上就写"回落到本机缓存"，而那条路径紧接着返回"额度未知" ——
	//   等于对用户撒谎（用户会以为额度还有效）。这里才是回落真的生效的地方。
	log.Printf("loomy 积分网关查不到，已改用本机缓存值 uid=%s（缓存更新于客户端上次写入，非此刻）",
		shortUID(uid))
	return q, true
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
