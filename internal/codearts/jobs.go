// jobs.go 把 CodeArts 的后台任务注册给核心调度器（gateway.JobExt 实现）。
//
// # 为什么续期是"上游的事"
//
// CodeArts 的 STS 凭证只有约 30 分钟寿命，且 refresh_token 是**消费型**的
// （用一次即作废，实测 STS5.1806 "the refresh token has been used"）。
// 什么时候续、以多长周期扫、失败怎么退避 —— 全部是 CodeArts 专属知识，
// 核心不该知道。核心只通过 gateway.Job 拿到"一个名字、一个间隔、一个 Run"。
//
// 加第二个上游时，核心的 scheduler 一行都不用改。
//
// # 为什么这里自己写循环而不是复用 RefreshScheduler
//
// 原有的 `RefreshScheduler`（scheduler.go）绑死在 `*Backend` 上，而 `*Backend`
// 是改造前"适配旧 server.Backend 接口"的产物（凭证双向投影那套）。
// 新的 Provider 路径不需要投影 —— 它直接持有 `*Auth`。
//
// 两者共用同一份**并发约束**（refresh_token 一次性），而这个约束由
// `Auth.refreshMu` 保证（见 client.RefreshToken），不在调度器里。
// 所以这里是一个更薄的循环：扫凭证 → 挑将过期的 → 串行续期。
//
// 不复用 RefreshScheduler 而是重写这 30 行，是为了不把 Provider 拖回
// 旧的 Backend 投影模型（那会让"第二上游接入"顺带把旧包袱也搬过来）。
package codearts

import (
	"context"
	"log"
	"time"

	"workbuddy2api/internal/gateway"
)

// JobRefresh 后台续期任务的稳定标识（用于调度器日志与状态展示）。
const JobRefresh = "codearts-refresh"

// defaultRefreshSkew 后台扫描时判定"将过期"的窗口。
//
// 与请求路径的 refreshSkew 一致（3 分钟）：窗口必须显著小于 30 分钟的凭证寿命，
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
	if p.refreshInterval <= 0 {
		return nil
	}
	return []gateway.Job{
		{
			Name:     JobRefresh,
			Interval: p.refreshInterval,
			Run:      p.runRefresh,
		},
	}
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
