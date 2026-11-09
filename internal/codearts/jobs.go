// jobs.go 把 CodeArts 的后台任务注册给核心调度器（gateway.JobExt 实现）。
//
// # 为什么续期是"上游的事"
//
// CodeArts 的 STS 凭证只有约 2 小时寿命，且 refresh_token 是**消费型**的
// （用一次即作废，实测 STS5.1806 "the refresh token has been used"）。
// 什么时候续、以多长周期扫、失败怎么退避 —— 全部是 CodeArts 专属知识，
// 核心不该知道。核心只通过 gateway.Job 拿到"一个名字、一个间隔、一个 Run"。
//
// 加第二个上游时，核心的 scheduler 一行都不用改。
//
// # 为什么这里是一个自己写的薄循环
//
// 改造前有一层「适配旧 server.Backend 接口」的投影（凭证双向转换那套），
// 续期调度绑死在那层上。新的 Provider 路径不需要投影 —— 它直接持有 `*Auth`。
//
// 阶段 1 评审发现那层已经**完全无引用**，连同它的调度器一起删掉了
// （backend.go + scheduler.go，共 490 行）。删除前用
// "删掉后 go build + go test 仍通过"证明了它确实是死代码。
//
// 续期的**并发约束**（refresh_token 是一次性的，用一次即作废）由
// `Auth.refreshMu` 保证（见 client.RefreshToken），不在调度器里。
// 所以这里只需要一个薄循环：扫凭证 → 挑将过期的 → 串行续期。
package codearts

import (
	"context"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// JobRefresh 后台续期任务的稳定标识（用于调度器日志与状态展示）。
const JobRefresh = "codearts-refresh"

// JobWelfare 每日福利自动领取（签到语义）任务的稳定标识。
//
// # 为什么要有它（用户要求"所有上游都自动签到"）
//
// 每日签到时点（9/21）的槽位只跑 workbuddy；trae 有自己的 trae-checkin；
// codearts 此前**只能手动点「签到」按钮** —— 三个有签到语义的上游里
// 唯独它不自动。这里补一个幂等的后台任务：每 welfareInterval 扫一次，
// ClaimAllWelfare 内部已跳过 claimable=false（今日已领）的活动，
// 领不到就记 skip，不重复消耗。
const JobWelfare = "codearts-welfare"

// defaultRefreshSkew 后台扫描时判定"将过期"的窗口。
//
// 与请求路径的 refreshSkew 一致（3 分钟）：窗口必须显著小于 2 小时的凭证寿命，
// 否则会出现"刚判定为新鲜、发出去已过期"的窗口。
const defaultRefreshSkew = refreshSkew

// Jobs 返回本上游要注册的定时任务（gateway.JobExt）。
//
// refreshInterval <= 0 时返回**空切片** —— "不注册任务"是合法状态，
// 核心对空切片不做任何事（与"没有平台特殊任务"的上游行为一致）。
//
// ⚠ 这里刻意**不做**成 Due 门控的任务：续期是"到点就看一眼谁快过期"，
// 而"谁快过期"本身就要扫一遍凭证（很便宜，纯本地时间比较），
// 用 Due 再扫一遍没有收益。固定间隔即可。
func (p *Provider) Jobs() []gateway.Job {
	jobs := make([]gateway.Job, 0, 2)
	if p.refreshInterval > 0 {
		jobs = append(jobs, gateway.Job{
			Name:     JobRefresh,
			Interval: p.refreshInterval,
			// ★ T-292：**加到期判据** —— 判据是"寿命过半"，而扫描粒度是 refreshInterval（默认 30m）。
			//   只靠 Interval 会出现：某个号刚过半点，而下一轮扫描在 29 分钟后 ⇒
			//   用户看到"已经剩不到一半了却一直没续"，最坏要等满一个 interval（实测用户报的就是这个）。
			//   给了 Due 之后核心每 jobTickInterval（30s）问一次，过半 30 秒内就能续上；
			//   Due 内部同时保留"距上次扫描超过 interval 就强制扫一轮"的旧节奏语义（见 refreshDue）。
			//
			//   ⚠ 核心的语义是：**有 Due 就不再叠加 Interval 门槛**（见 scheduler/job.go 的 jobDue），
			//     所以"每 interval 至少扫一次"这件必须在 Due 里自己表达 —— 这就是 refreshDue 的两个分支。
			Due: p.refreshDue,
			Run: p.runRefresh,
		})
	}
	if p.welfareEnabled && p.welfareInterval > 0 {
		jobs = append(jobs, gateway.Job{
			Name:     JobWelfare,
			Interval: p.welfareInterval,
			Run:      p.runWelfare,
		})
	}
	return jobs
}

// refreshDue 后台续期任务的到期判据（★ T-292）。
//
// 两个分支，任一为真即该跑：
//
//	① 距上次扫描已达 refreshInterval（旧的"每 30 分钟扫一轮"节奏，保留为兜底）
//	② **已有账号寿命过半** ⇒ 立刻跑（30s 粒度内），不再干等下一个 interval
//
// 为什么需要 ①：核心在"任务有 Due"时**不再叠加 Interval 门槛**，
// 若只留 ②，"一个号都还没过半"时任务永远不跑 —— 那样一旦某个号的文件在
// 两轮之间被换成快过期的凭证，就只能等它真的过半才被发现（兜底也丢了）。
//
// 为什么 ② 必须与 runRefresh 的挑号条件同源：否则会出现
// "Due 说该跑、runRefresh 一个都不挑" ⇒ 每 30 秒空转一次（日志被刷、上游被打）。
// 所以两边都只走 accountsPastSkew 这一个判据函数。
//
// `now` 由核心传入（scheduler 用它判到期），本函数不做时间取反等隐式操作。
func (p *Provider) refreshDue(now time.Time) bool {
	if p.refreshInterval <= 0 {
		return false // 未开启后台续期（不该走到这里，Jobs() 已按同一条件注册）
	}
	// ① 兜底节奏：从未扫过 ⇒ 立刻跑（保持"启动即首扫"的既有语义）；
	//    或距上次扫描已满一个 interval。
	if last := p.lastRefreshScanNS.Load(); last == 0 || !now.Before(time.Unix(0, last).Add(p.refreshInterval)) {
		return true
	}
	// ② 有账号已过半 ⇒ 该续了。
	need, _ := p.accountsPastSkew(now)
	return len(need) > 0
}

// accountsPastSkew 挑出"此刻就该续期"的账号 —— **runRefresh 与 refreshDue 共用的唯一判据**。
//
// 三个跳过条件（与改造前 runRefresh 里的三个 continue 逐条对应，不新增不减少）：
//
//	· 没有 refresh_token ⇒ 续不了（只能重新登录）
//	· 正在失败退避中 ⇒ 不重试（凭证永久失效时等用户重新登录，见 holdRefresh）
//	· 剩余寿命还大于本上游的续期窗口（≈ 寿命的一半）⇒ 还不到时候
//
// 返回 (nil, err) 时调用方自行决定：runRefresh 把错误抛给调度器记日志；
// refreshDue 把它当成"这轮不跑"（判据失败不该让任务每 30 秒报一次错）。
func (p *Provider) accountsPastSkew(now time.Time) ([]*Auth, error) {
	list, err := p.localAccounts()
	if err != nil {
		return nil, err
	}
	var need []*Auth
	for _, a := range list {
		if a == nil {
			continue
		}
		if a.RefreshToken == "" {
			continue
		}
		if p.onRefreshHold(a) {
			continue
		}
		skew, ok := refreshSkewOf(a)
		if !ok {
			continue
		}
		// 剩余 ≤ skew ⟺ now + skew ≥ 到期时刻（与 core/refreshratio.go 的判据同形）。
		if now.Add(skew).Before(time.Unix(a.ExpiresAt, 0)) {
			continue // 还没过半：本轮不刷
		}
		need = append(need, a)
	}
	return need, nil
}

// runWelfare 一趟福利自动领取（签到语义）：逐账号领取可领福利并记录历史。
func (p *Provider) runWelfare(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	list, err := p.localAccounts()
	if err != nil {
		return err
	}
	for _, a := range list {
		if a == nil || ctx.Err() != nil {
			continue
		}
		if _, _, err := p.claimWelfareFor(a, "sched"); err != nil {
			log.Printf("codearts: 福利自动领取失败 uid=%s: %v", a.UID, err)
			continue
		}
	}
	return nil
}

// runRefresh 一趟后台续期：扫凭证目录，对将过期的账号串行续期。
//
// # 为什么串行
//
// 并发没有收益（每账号一次网络往返），却会让日志交错、
// 也更难判断哪个失败对应哪个号。
//
// # 为什么单个账号失败不停轮
//
// 一个号的问题不应影响其它号，也不该让调度器退出 ——
// 续期失败会在下一轮重试，且请求路径还有一次惰性续期的机会。
func (p *Provider) runRefresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// ★ T-292：先记下"这一轮扫描发生了" —— refreshDue 的兜底分支靠它算"距上次扫描多久"。
	//   记在入口而不是结尾：即使下面中途 return，也说明"这一轮跑过了"，
	//   否则 refreshDue 会立刻再判一次"该跑"，形成 30 秒级空转。
	p.lastRefreshScanNS.Store(time.Now().UnixNano())

	// ★ T-292：挑号判据统一到 accountsPastSkew（**同一个函数**也被 refreshDue 用来判到期）
	need, err := p.accountsPastSkew(time.Now())
	if err != nil {
		return err
	}
	if len(need) == 0 {
		return nil
	}

	// ⚠ 文案随判据改（2026-11-01）：这里不再是"全量续"，而是"已过半寿命的那批"。
	//   留着旧字会让日志与行为相反 —— 本仓在意"日志说实话"（实测：新构建跑出
	//   这行时，读的人会以为还在无条件全量刷）。
	log.Printf("codearts: 后台续期开始，%d 个账号（已过半寿命：剩余 ≤ 寿命×0.5）", len(need))
	for _, a := range need {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.client.RefreshToken(a); err != nil {
			log.Printf("codearts: 后台续期失败 (uid=%s): %v", a.UID, err)
			p.holdRefresh(a, err)
			// 通知装配层（pool.NoteRefreshFailure）：连续失败达上限自动禁用，
			// 死 token 账号在账号池里可见「需重新登录」。
			if p.onRefreshFailure != nil {
				p.onRefreshFailure(a.UID)
			}
			continue
		}
		p.clearRefreshHold(a.UID)
		if p.onRefreshSuccess != nil {
			p.onRefreshSuccess(a.UID)
		}
		log.Printf("codearts: 后台续期成功 (uid=%s)，新过期 %s",
			a.UID, time.Unix(a.ExpiresAt, 0).Format(time.RFC3339))
	}
	return nil
}

// holdRefresh 续期失败后挂退避。
//
// 永久性凭证错误（refresh token 已被服务端消费 / 无 token / 缺 DPoP 私钥）
// → 停 24 小时等用户重新登录，否则每轮扫描都打一次注定失败的上游请求
// （实测 STS5.1806 死 token 每 60s 一次）。
// 其余错误（网络/5xx 抖动）→ **指数退避 + 抖动**（借鉴 LiteLLM）：
// 2m → 4m → 8m → … 封顶 30m，每次失败翻倍并带 ±20% 随机抖动 ——
// 多账号同时抖动失败时不会在同一时刻集体重试（惊群）。
// 凭证文件被外部更新（重新登录写盘）时由 onRefreshHold 提前解除。
func (p *Provider) holdRefresh(a *Auth, err error) {
	now := time.Now()
	p.refreshHoldMu.Lock()
	defer p.refreshHoldMu.Unlock()
	e := p.refreshHold[a.UID]
	e.fails++

	hold := refreshBackoff(e.fails)
	if isPermanentRefreshError(err) {
		hold = 24 * time.Hour
		log.Printf("codearts: 凭证已失效（%v）—— 已暂停该账号自动续期，请重新登录（页内「＋添加账号」或 cmd/login）；凭证文件更新后自动恢复", err)
	}
	e.until = now.Add(hold)
	e.holdAt = now
	p.refreshHold[a.UID] = e
}

// refreshBackoff 指数退避 + 抖动：base 2m 起，每多一次失败翻倍，封顶 30m，
// 再乘 (0.8, 1.2) 的随机抖动。借鉴 LiteLLM 的 exponential backoff + jitter。
func refreshBackoff(fails int) time.Duration {
	const (
		base = 2 * time.Minute
		cap  = 30 * time.Minute
	)
	d := base
	for i := 1; i < fails && d < cap; i++ {
		d *= 2
	}
	if d > cap {
		d = cap
	}
	// ±20% 抖动（math/rand 全局锁开销可忽略：退避是低频路径）
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

// onRefreshHold 该账号是否处于退避期。
//
// 特判"外部重新登录"：凭证文件（a.FilePath）的修改时间晚于进入退避的时刻
// （holdAt）→ 说明用户已重新登录写入了新凭证，提前解除退避，下一轮恢复自动续期。
func (p *Provider) onRefreshHold(a *Auth) bool {
	p.refreshHoldMu.Lock()
	defer p.refreshHoldMu.Unlock()
	e, ok := p.refreshHold[a.UID]
	if !ok {
		return false
	}
	if time.Now().Before(e.until) {
		if a.FilePath != "" {
			if fi, err := os.Stat(a.FilePath); err == nil && fi.ModTime().After(e.holdAt) {
				delete(p.refreshHold, a.UID)
				return false
			}
		}
		return true
	}
	delete(p.refreshHold, a.UID)
	return false
}

// clearRefreshHold 续期成功（或重试窗口过期）时清除退避。
func (p *Provider) clearRefreshHold(uid string) {
	p.refreshHoldMu.Lock()
	delete(p.refreshHold, uid)
	p.refreshHoldMu.Unlock()
}

// isPermanentRefreshError 凭证**永久失效**（必须重新登录才能恢复）的错误特征。
//
// 与之相对的是一次性抖动（网络、5xx）—— 那些只挂短退避，下轮重试。
func isPermanentRefreshError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "has been used") || // STS5.1806：refresh token 已被服务端消费
		strings.Contains(s, "无 refresh_token") ||
		strings.Contains(s, "缺 DPoP") ||
		strings.Contains(s, "需重新登录")
}

// 编译期断言：Provider 实现 JobExt。
//
// 与 provider.go 里的断言分开写：那条保证"能被发现为上游"（arch_test 的判据），
// 这条保证"任务确实注册得上去"。两条都失败会编译不过。
var _ gateway.JobExt = (*Provider)(nil)
