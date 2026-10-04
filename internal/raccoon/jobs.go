// jobs.go —— raccoon 的后台主动续期。
//
// # 为什么必须有它（用户报障：「这个 token 续期逻辑有 bug」）
//
// 报障现场（管理台账号行）：
//
//	raccoon  Rac***************1)  7497524…  3082  正常  已过期  643
//	                                                ↑      ↑
//	                                             有额度   但 token 过期
//
// 「正常」与「已过期」同时出现，而下面这条实测把根因指得很清楚：
//
//	access_token   iat/nbf → exp 寿命约 3 小时，**已过期 532 分钟**
//	refresh_token  寿命 30 天，**还剩 29.5 天**（完全可续期）
//
// 也就是说：token 静静过期了 8.9 小时，而一个能救它的 refresh_token
// 一直躺在同一个文件里没人用。
//
// # 根因：核心的续期只有两条触发路径，raccoon 一条都不占
//
//	① 出站请求时   handler 判断"该刷了"才刷
//	② 后台定时任务 上游通过 gateway.JobExt 自报的 Jobs()
//
// raccoon **满足** `gateway.CredentialRefresher`（provider.go 的
// RefreshCredential 签名正确）、也**满足** `gateway.RefreshSkewExt`
//（refreshskew.go 正确报了"已用寿命 ≥50% 该刷"）—— 但**没有 Jobs()**。
//
// 于是它只有"有人拿 raccoon 号发对话请求"时才会顺手续期。没人用它时
// 就静静过期 —— 界面上「Token」列一直显示"已过期"，而 refresh_token
// 明明还能用。
//
// ⚠ 这与 cline 修之前**逐字同一种病**（见 internal/cline/jobs.go 的注释，
// 那里的报障原文是「Token 已过期，怎么不会自动刷新」）。
//
// 教训：一个上游"具备续期能力"与"会去续期"是两件事 ——
// 前者是方法签名，后者是**注册了后台任务**。缺后者时没有任何报错，
// 只是悄悄过期。本文件补的就是这一条。
//
// 对照：workbuddy / codearts / cline / lobsterai / qoder / trae
// 都注册了后台任务，所以从不会停在"已过期"。

package raccoon

import (
	"context"
	"errors"
	"log"
	"time"

	"workbuddy2api/internal/gateway"
)

var _ gateway.JobExt = (*Provider)(nil)

// defaultRefreshScanInterval 后台扫描间隔。
//
// # 取值依据（实测的 token 寿命）
//
// raccoon 的 access_token 寿命约 **3 小时**（实测 iat/nbf → exp），
// 而 RefreshSkew 的判据是"已用寿命 ≥50%" → 剩约 90 分钟时该刷。
//
// 取 10 分钟一轮：最坏情况下在"该刷"之后 10 分钟内被刷掉，
// 仍有约 80 分钟余量给失败重试。
//
// 比这更密没必要（寿命是小时级），更疏会吃掉重试余量 ——
// 而重试余量恰恰是这次报障的关键：token 过期 8.9 小时都没被碰过，
// 说明"只在被人用到时才刷"完全不够。
const defaultRefreshScanInterval = 10 * time.Minute

// refreshInterval 本实例的扫描间隔（<=0 表示不注册后台任务）。
//
// 从 authDir 是否配置推断"这个实例是否值得起任务"：
// 没有凭证目录 = 没有账号要刷，起任务只是空转。
func (p *Provider) refreshInterval() time.Duration {
	if p == nil || p.authDir == "" {
		return 0
	}
	return defaultRefreshScanInterval
}

// Jobs 注册后台续期任务（gateway.JobExt）。
func (p *Provider) Jobs() []gateway.Job {
	iv := p.refreshInterval()
	if iv <= 0 {
		return nil
	}
	return []gateway.Job{{
		Name:     "raccoon-refresh",
		Interval: iv,
		Run:      p.runRefresh,
	}}
}

// runRefresh 扫描本地凭证，把临近过期的刷掉。
//
// # ⚠ 必须刷**池里持有的那个对象**，不能刷磁盘副本
//
// 这是本仓栽过多次的坑（cline 的 jobs.go 有完整记录）：池子持有的是
// 启动时放进 `secrets[uid]` 的那个 `*Auth`，而 `LoadDir` 每次都从磁盘
// **新建**对象。刷后者只会更新磁盘，池里那份纹丝不动 ——
// 而界面读的是池（`TokenExpiry` 拿到的 `cred.Secret` 就是池里那个指针），
// 于是出现"落盘已续期、界面仍显示已过期"。
//
// 所以优先用 `p.creds(uid)`（装配层注入的**活 secret** 访问器）；
// 它未接线（纯单测 / 未入池）时才退磁盘兜底。
func (p *Provider) runRefresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.authDir == "" {
		return nil
	}
	list, err := p.refreshCandidates()
	if err != nil {
		return err
	}
	now := time.Now()
	var refreshed, skipped, failed int
	for _, a := range list {
		if a == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// 不可续期（没有 refresh_token）：刷不了，如实跳过。
		//
		// ⚠ 这条 continue 是"不空转"的关键 —— 没有它，每一轮都会对着
		// 一个永远刷不了的凭证白发一次请求。
		if !a.Renewable() {
			skipped++
			continue
		}
		// 「该刷吗」复用 RefreshSkew 的同一判据（寿命比例）。
		//
		// raccoon 的 access_token 是 JWT，正常路径能拿到比例窗口；
		// 解不出来时回落到按明确过期时刻判断（needsRefresh）。
		if skew, ok := gateway.RefreshSkewFromToken(a.AccessToken); ok {
			// skew 正是"寿命的 50%" —— 于是等价于"已用寿命 ≥50% 就刷"，
			// 与出站请求路径逐字一致。
			if !a.needsRefresh(now, skew) {
				skipped++
				continue
			}
		} else if !a.needsRefresh(now, defaultRefreshScanInterval) {
			skipped++
			continue
		}

		if err := p.RefreshCredential(gateway.Credential{
			Provider: providerID, UID: a.UID(), Secret: a,
		}); err != nil {
			failed++
			// 终态（refresh_token 也失效）与可重试分开报，
			// 用户才知道该不该去重新登录。
			if isRefreshExpired(err) {
				log.Printf("raccoon: uid=%s refresh_token 已失效，无法自动续期 —— 请重新登录/导入",
					shortUID(a.UID()))
			} else {
				log.Printf("raccoon: uid=%s 后台续期失败（可重试）：%v", shortUID(a.UID()), err)
			}
			continue
		}
		refreshed++
	}
	// 只在真有动作时打日志：每 10 分钟一行"什么都没做"会淹没真正的信号。
	if refreshed > 0 || failed > 0 {
		log.Printf("raccoon: 后台续期完成 —— 续期 %d，跳过 %d，失败 %d", refreshed, skipped, failed)
	}
	return nil
}

// refreshCandidates 返回本轮要检查的凭证（**池里的活对象**优先）。
//
// # 为什么不能直接返回 LoadDir 的结果
//
// 见 runRefresh 的注释：那会造成"刷了磁盘、池没变 → 界面仍显示已过期"。
//
// ⚠ 顺序不能反：先磁盘会走进分叉。`creds == nil`（纯单测 / 未入池）
// 时才退磁盘 —— 那种情况下没有池，也就没有分叉问题。
func (p *Provider) refreshCandidates() ([]*Auth, error) {
	disk, err := LoadDir(p.authDir)
	if err != nil {
		return nil, err
	}
	if p.creds == nil {
		return disk, nil
	}
	out := make([]*Auth, 0, len(disk))
	for _, d := range disk {
		if d == nil {
			continue
		}
		uid := d.UID()
		if uid == "" {
			continue
		}
		// 池里有这个 uid 就用池的对象；没有（刚放进目录、池尚未对账）
		// 才用磁盘那份，至少能把它续上。
		if cred, ok := p.creds(uid); ok {
			if live, aerr := authOf(cred); aerr == nil && live != nil {
				out = append(out, live)
				continue
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// isRefreshExpired 报告一个续期错误是不是**终态**（refresh_token 本身失效）。
//
// # 为什么要区分（而不是所有错误一视同仁）
//
// 两者的用户动作完全不同：
//
//	终态   refresh_token 已作废 → 只能重新登录/导入，重试再多次也没用
//	可重试 网络抖动 / 5xx / 429 → 下一轮会自动再试
//
// 把可重试的报成终态会让用户白跑一趟重新登录；把终态报成"稍后重试"
// 则会让 token 永远停在过期状态 —— 正是这次报障的形态。
//
// 判据是 `errors.Is` 而不是字符串比较：`ErrRefreshExpired` 会被
// `fmt.Errorf("...%w", ErrRefreshExpired)` 包装，字符串比较在包装后失效。
func isRefreshExpired(err error) bool {
	return err != nil && errors.Is(err, ErrRefreshExpired)
}
