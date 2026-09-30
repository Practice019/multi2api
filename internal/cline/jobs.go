package cline

import (
	"context"
	"errors"
	"log"
	"time"

	"workbuddy2api/internal/gateway"
)

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
// 判据是 `errors.Is` 而不是字符串比较：`ErrRefreshExpired` 在 cline.go /
// client.go 里会被 `fmt.Errorf("...%w", ErrRefreshExpired)` 包装，
// 字符串比较在包装后就失效了。
func isRefreshExpired(err error) bool {
	return err != nil && errors.Is(err, ErrRefreshExpired)
}

// jobs.go —— cline 的后台主动续期。
//
// # 为什么必须有它（用户报障：「Token 已过期，怎么不会自动刷新」）
//
// cline **满足** `gateway.CredentialRefresher`（provider.go 有正确签名的
// `RefreshCredential`），但核心的续期只有**两条**触发路径：
//
//	① 出站请求时   handler.needsRefreshVia 判断"该刷了"才刷
//	② 后台定时任务 上游通过 gateway.JobExt 自报的 Jobs()
//
// 而 cline 此前**一条都不占**：没有 Jobs()，于是只有"有人拿 cline 号发
// 对话请求"时才会顺手续期。没人用 cline 时它的 token 就静静过期 ——
// 界面上「Token」列一直显示"已过期"，而 `refresh_token` 明明还在、
// 明明可以直接换一个新的。
//
// 对比其它上游：workbuddy / codearts / lobsterai / qoder / trae
// 都注册了后台任务（有的是签到，有的是续期），所以从不会停在"已过期"。
//
// # 判据：只刷"可续期且临近过期"的，不做无条件全刷
//
// 与既有上游的 `runRefresh` 同一条取舍（那边注释写得很清楚）：跳过不可续期的
// 凭证，避免退化成"每轮都白刷一次"。cline 的 `refreshSkew.go` 已经
// 把"多早算该刷"表达成**寿命比例**（已用 ≥50%），这里复用它 ——
// 于是判据只有一处，不会与请求路径漂移。
//
// ⚠ 没有它就会重现本仓记过的「每请求都续期」事故的反面：
// 那次是**刷得太勤**（`NeedsRefresh` 对 ExpiresAt=0 恒真），
// 这次是**根本不刷**。两者都源于"判据没有单一权威"，所以这里必须走
// 活凭证上的 `needsRefresh`，而不是池投影的 ExpiresAt。
var _ gateway.JobExt = (*Provider)(nil)

// defaultRefreshScanInterval 后台扫描间隔。
//
// 取 10 分钟：cline 的 access_token 寿命 1 小时，按 RefreshSkew 的
// 50% 比例 → 剩 30 分钟时触发。10 分钟一轮意味着最坏情况下
// 在"该刷"之后 10 分钟内被刷掉，仍留 20 分钟余量给失败重试。
//
// 比这更密没必要（token 寿命是小时级），更疏则会吃掉重试余量。
const defaultRefreshScanInterval = 10 * time.Minute

// refreshInterval 本实例的扫描间隔（<=0 表示不注册后台任务）。
//
// 从 authDir 是否配置推断"这个实例是否值得起任务" —— 与其它上游用
// config 字段的表达略有不同，但这里没有对应配置项，而"没有凭证目录"
// 就等价于"没有账号要刷"，起任务只是空转。
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
		Name:     "cline-refresh",
		Interval: iv,
		Run:      p.runRefresh,
	}}
}

// runRefresh 扫描本地凭证，把临近过期的刷掉。
//
// # ⚠ 必须刷**池里持有的那个对象**，不能刷磁盘副本（实测踩到）
//
// 我第一版走 `LoadDir`（从磁盘新建 `*Auth`）—— 结果是**磁盘更新了、
// 池里的对象没变**：界面读的是池（`Pool.SecretOf`），于是「Token」列
// 一直显示旧值（实测：续期后落盘 `expire_time` 已 +1 小时，
// 界面仍显示 `-3322` 秒）。这正是本仓多个 `refreshskew.go` 注释里
// 反复警告的"续期写到另一个对象上"。
//
// 所以优先用 `p.creds(uid)`（装配层注入的**活 secret** 访问器）——
// 它拿到的就是池里那个指针，`RefreshCredential` 原地更新它，
// 池与界面立刻一致。
//
// 磁盘扫描仍保留作**兜底**：`creds` 未接线（纯单测 / 未入池）时，
// 至少还能把磁盘上的凭证续上（`RefreshCredential` 会落盘）。
// 但那条路径会命中"刷了磁盘、池没变"，所以只在 `creds == nil` 时走。
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
		// 「该刷吗」复用 RefreshSkew 的同一判据（寿命比例），
		// 保证后台与请求路径不会给出不同答案。
		skew, ok := gateway.RefreshSkewFromToken(a.AccessToken)
		if !ok {
			// token 不是 JWT（解不出 iat/exp）：回落到按明确过期时刻判断。
			if !a.needsRefresh(now, defaultRefreshScanInterval) {
				skipped++
				continue
			}
		} else {
			// 有比例窗口：用它算"现在距过期够不够近"。
			//
			// `needsRefresh(now, lead)` 的语义是"距过期不足 lead 就该刷"，
			// 而 skew 正是"寿命的 50%" —— 于是这里等价于
			// "已用寿命 ≥50% 就刷"，与请求路径逐字一致。
			if !a.needsRefresh(now, skew) {
				skipped++
				continue
			}
		}

		if err := p.RefreshCredential(gateway.Credential{
			Provider: providerID, UID: a.UID(), Secret: a,
		}); err != nil {
			failed++
			// 终态（refresh_token 也失效）与可重试分开报，用户才知道
			// 该不该去重新登录。
			if isRefreshExpired(err) {
				log.Printf("cline: uid=%s refresh_token 已失效，无法自动续期 —— 请重新登录/导入",
					shortUID(a.UID()))
			} else {
				log.Printf("cline: uid=%s 后台续期失败（可重试）：%v", shortUID(a.UID()), err)
			}
			continue
		}
		refreshed++
	}
	// 只在真有动作时打日志：每 10 分钟一行"什么都没做"会淹没真正的信号。
	if refreshed > 0 || failed > 0 {
		log.Printf("cline: 后台续期完成 —— 续期 %d，跳过 %d，失败 %d", refreshed, skipped, failed)
	}
	return nil
}

// refreshCandidates 返回本轮要检查的凭证（**池里的活对象**优先）。
//
// # 为什么不能直接返回 LoadDir 的结果（实测的分叉）
//
// 池子持有的是**启动时**放进 `secrets[uid]` 的那个 `*Auth`；而 `LoadDir`
// 每次都从磁盘**新建**对象。刷后者只会更新磁盘，池里那份纹丝不动 ——
// 而界面读的是池，于是"落盘已续期、界面仍显示已过期"（实测形态）。
//
// 所以有 `creds`（装配层注入的活 secret 访问器）时以它为准。
// 它是 `Pool.SecretOf` 的包装，拿到的就是池里那个指针。
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
