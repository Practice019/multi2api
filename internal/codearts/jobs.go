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
			Run:      p.runRefresh,
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
	list, err := p.localAccounts()
	if err != nil {
		return err
	}

	var need []*Auth
	for _, a := range list {
		if a == nil {
			continue
		}
		// 没有 refresh_token 的账号没法续（只能重新登录），跳过。
		if a.RefreshToken == "" {
			continue
		}
		if a.NeedsRefresh(defaultRefreshSkew) {
			need = append(need, a)
		}
	}
	if len(need) == 0 {
		return nil
	}

	log.Printf("codearts: 后台续期开始，%d 个账号临近过期（窗口 %v）", len(need), defaultRefreshSkew)
	for _, a := range need {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.client.RefreshToken(a); err != nil {
			log.Printf("codearts: 后台续期失败 (uid=%s): %v", a.UID, err)
			continue
		}
		log.Printf("codearts: 后台续期成功 (uid=%s)，新过期 %s",
			a.UID, time.Unix(a.ExpiresAt, 0).Format(time.RFC3339))
	}
	return nil
}

// 编译期断言：Provider 实现 JobExt。
//
// 与 provider.go 里的断言分开写：那条保证"能被发现为上游"（arch_test 的判据），
// 这条保证"任务确实注册得上去"。两条都失败会编译不过。
var _ gateway.JobExt = (*Provider)(nil)
