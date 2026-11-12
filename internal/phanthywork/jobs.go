// jobs.go PhanthyWork 的后台主动续期（gateway.JobExt）。
//
// # 为什么必须有它（这是本仓一条**跨上游不变量**，不是可选优化）
//
// 核心的续期只有两条触发路径：
//
//	① 出站请求时 handler 判「该刷了」才刷
//	② 后台定时任务（上游通过 JobExt 自报的 Jobs()）
//
// 本包实现了 ① 所需的两件东西（CredentialRefresher + RefreshSkewExt），但没有 ②。
// 缺 ② 的后果是**静默**的：没人拿这个号的发请求时，token 会静静过期，
// 而 refresh_token 明明还能用；界面上「Token」列一直显示已过期。
//
// 这条守卫的出处是三次用户报障（cline → raccoon → minimax），形态逐字相同。
// 本包被登记进 cmd/server 的 allUpstreams() 之后，立刻被同一条守卫接管：
// 守卫在正常工作，不是回归。完整判据见 cmd/server/jobs_pairing_test.go。
//
// # 与坑⑤（refreshToken 轮换 ⇒ 与桌面端互踢）的张力，以及本包的裁决
//
// 报告 §3⑤ 与 §7.1 的两条对策是「同账号 refresh 必须串行」+「能只读就只读」。
// 后台续期天然是"主动刷"，所以这里把两条对策都落到底：
//
//	对策一「串行」  → 复用包内 refreshGate（同账号 in-flight 合并 + 成功回放 60s），
//	                  见 RefreshCredential；本文件**不**另写并发控制
//	对策二「只读」  → 每一轮都**先过 RefreshSkew** 才真刷（不满足就零请求）；
//	                  并且**启动时不一次性刷所有账号**（见 runRefresh）
//
// 换句话说：本文件把"后台任务"这件事压到**最小必要频率**，
// 而不是"既然要了就刷勤一点"。
package phanthywork

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：Provider 必须实现后台任务扩展点。
//
// 缺了它不会编译失败，而是**静默退化**成"只在被人用到时续期"——
// 也就是那三次报障的形态。断言在这里，是为了让"哪天有人删掉 Jobs()"
// 立刻编译不过，而不是等用户发现 token 过期。
var _ gateway.JobExt = (*Provider)(nil)

// JobRefresh 后台续期任务的注册名（出现在控制台的定时任务列表里）。
const JobRefresh = "phanthywork-refresh"

// defaultRefreshScanInterval 后台扫描间隔。
//
// # 取值依据（这一条必须算出来，不能"随手取个 30 分钟"）
//
// 本上游 access_token 的寿命是 AccessTokenTTL（**实测 1800 秒**，报告 §3.3：
// JWT 的 exp-iat = 1800，三处独立印证）。RefreshSkew 复用 gateway 的统一策略
// 「剩余 ≤ 寿命的一半」（见 refreshratio.go），于是这一份凭证的**该刷点**是
// 签发后 900 秒（剩 900 秒）。
//
// 扫描间隔必须**显著小于** 900 秒，否则会出现这条链最糟的形态：
//
//	间隔 30 分钟 = 1800 秒 > 该刷点 900 秒 ⇒ 一个刚在两次扫描之间变"该刷"的
//	账号，要等整整 30 分钟才被看到 —— 而它的 token 在 900 秒后就过期了。
//	结果：后台任务照样注册了、也照样在跑，但观察到的永远是"已过期"。
//
// 取 10 分钟的理由：
//
//	最坏延迟 600 秒 < 该刷点 900 秒（AccessTokenTTL/2）⇒ 任何账号在它过期之前**必被扫到一次**
//	余量 ≥ 300 秒 ⇒ 失败也还有一次重试机会（重试余量正是那三次报障的关键）
//	比这更密没有收益（寿命是分钟级，且扫描本身只是内存里过一遍）
//
// ⚠ 与"能只读就只读"不冲突：间隔只决定**多久看一眼**，
// 真正决定"要不要打上游"的是 needsRefresh（过 skew 才刷）。
// 一个空闲部署每 10 分钟只看一眼内存，每 ~15 分钟才真刷一次。
const defaultRefreshScanInterval = 10 * time.Minute

// refreshSpacing 两次真实刷新之间的最小间隔（**串行的落地**）。
//
// 账号多时，一轮里可能有多个账号同时该刷。串行 + 间隔让上游看到的是
// "分散的几次请求"，而不是"同一毫秒里的 N 个请求"（后者在服务端看来就是
// 一次设备风控信号）。
//
// 抽成变量是为了测试能把它压成 0（否则一条 3 账号的用例要白等 2 秒）。
var refreshSpacing = time.Second

// firstRunRefreshLimit 首轮最多刷几个账号。
//
// # 为什么首轮要限额（这条是"别形成刷新风暴"的落点）
//
// refreshToken 是**轮换**的：每刷一次，旧的那个立刻作废。
// 如果进程一启动就把池子里所有账号刷一遍，后果有两层：
//
//	① 与官方桌面端共用账号时 —— **启动即把所有人的桌面端踢下线**
//	   （一个用户装了 20 个号，一次启动就轮换 20 个 refreshToken）
//	② 服务端看到的是同一秒内的 N 个 token 请求 —— 那正是风控想看的形状
//
// 所以首轮只刷**最急的那一个**（按剩余寿命排序的第一个）。
// 其余账号由后续轮次逐步消化 —— 每轮 10 分钟，扫描间隔本身就把它们摊开了。
const firstRunRefreshLimit = 1

// refreshInterval 本实例的扫描间隔（<=0 表示不注册后台任务）。
//
// 判据：没有凭证目录 = 没有账号要刷，起任务只是空转。
// 与 minimax / raccoon / cline 同一口径（守卫两个方向都守）。
func (p *Provider) refreshInterval() time.Duration {
	if p == nil || strings.TrimSpace(p.authDir) == "" {
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
		Name:     JobRefresh,
		Interval: iv,
		Run:      p.runRefresh,
	}}
}

// runRefresh 扫描账号，把临近过期的刷掉。
//
// # ⚠ 必须刷**池里持有的那个对象**，不能刷磁盘副本
//
// 池子持有的是启动时放进 `secrets[uid]` 的那个 `*Auth`，而 LoadDir 每次都从
// 磁盘**新建**对象。刷后者只会更新磁盘，池里那份纹丝不动 ——
// 而界面读的是池（TokenExpiry 拿到的 cred.Secret 就是池里那个指针），
// 于是出现"落盘已续期、界面仍显示已过期"。
//
// 所以这里全部走 `p.cred(uid)`：它**优先**问注入的凭证源（生产路径：池里的
// 活 secret），未接线时才回落本实例的 creds（测试路径）。这一步是必需的，
// 不是优化。
//
// # 本轮规模控制的三个闸
//
//	首轮限额       —— firstRunRefreshLimit（见它的注释：轮换凭据不许启动即风暴）
//	串行 + 间隔    —— refreshSpacing
//	过 skew 才真刷 —— needsRefresh（复用 RefreshSkew，"能只读就只读"的落点）
func (p *Provider) runRefresh(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || strings.TrimSpace(p.authDir) == "" {
		return nil
	}
	list := p.refreshCandidates()
	if len(list) == 0 {
		return nil
	}
	// 首轮只刷最急的 1 个 —— 见 firstRunRefreshLimit 的注释。
	limit := 0 // 0 = 不限
	p.mu.Lock()
	if !p.jobsRanOnce {
		p.jobsRanOnce = true
		limit = firstRunRefreshLimit
	}
	p.mu.Unlock()

	now := time.Now()
	var refreshed, skipped, failed int
	for _, a := range list {
		if a == nil {
			continue
		}
		// 每一轮都查一次取消：账号多时这一轮可能跨好几分钟，
		// 关停时不该继续打完剩下的上游请求。
		if err := ctx.Err(); err != nil {
			return err
		}
		// 没有 refreshToken 的号**刷不了**（不是"刷失败"）：如实跳过。
		//
		// 与 minimax 的 jobs.go 同一个取舍：这条过滤的真实收益是"不让日志被
		// 刷屏"（每个刷不了的号每轮都会打一条永远失败的日志）；不保留它也不会
		// 造成状态差异，因为 RefreshCredential 在没有 refreshToken 时会提前返回。
		if strings.TrimSpace(a.OAuth.RefreshToken) == "" {
			skipped++
			continue
		}
		// ★ 「能只读就只读」的落点：不过 skew 就**零请求**。
		if !p.needsRefresh(a, now) {
			skipped++
			continue
		}
		if limit > 0 && refreshed >= limit {
			skipped++
			continue
		}
		// 两次真实刷新之间留间隔（首个不等待）。
		if refreshed > 0 && refreshSpacing > 0 {
			select {
			case <-time.After(refreshSpacing):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		// ⚠ RefreshCredential 里已经复用了刷新闸（同账号串行、防 TOCTOU），
		// 这里**不**再加一层锁 —— 锁的粒度是上游的事实，加两层只会更难推理。
		// 记下刷新前的令牌：RefreshCredential 对"其实不用刷"的情况也返回 nil
		// （它是幂等的），所以**只有令牌真的变了才算 refreshed** ——
		// 否则日志会写"成功 1"而实际上什么都没发生（我第一版就是这样，
		// 被 TestRunRefreshExactlyOnceWhenSkewMet 当场抓住：日志与请求计数对不上）。
		beforeAT, beforeRT := a.OAuth.AccessToken, a.OAuth.RefreshToken
		if err := p.RefreshCredential(gateway.Credential{
			Provider: providerID, UID: a.UID(), Secret: a,
		}); err != nil {
			failed++
			// 终态（refresh_token 也失效）与可重试分开报 —— 用户才知道该不该重新登录。
			// 混成一条会让他反复重试一个不可能成功的动作。
			if IsRefreshExpired(err) || IsRegistrationConflict(err) {
				log.Printf("phanthywork: uid=%s 凭据已失效，无法自动续期 —— 请重新登录/导入", gateway.ShortUID(a.UID()))
			} else {
				log.Printf("phanthywork: uid=%s 后台续期失败（可重试）：%v", gateway.ShortUID(a.UID()), err)
			}
			continue
		}
		if a.OAuth.AccessToken == beforeAT && a.OAuth.RefreshToken == beforeRT {
			// 进去之后发现"其实还不用刷"：如实计跳过，不冒充成功。
			skipped++
			continue
		}
		refreshed++
	}
	// 只在真有动作时打一行汇总：空闲部署（全都还新鲜）不该每 10 分钟刷屏。
	if refreshed > 0 || failed > 0 {
		log.Printf("phanthywork: 后台续期 成功 %d / 失败 %d（跳过 %d）", refreshed, failed, skipped)
	}
	return nil
}

// needsRefresh 这份凭证现在该不该续期。
//
// # 判据必须与**出站路径**同源（这是关键，不是细节）
//
// 出站路径走 needsRefreshVia → RefreshSkew。如果这里另写一个判据
// （例如固定"剩 5 分钟"），就会出现两种后果之一：
//
//	比出站更严 → 后台永远不刷，等于没加这个任务（那三次报障原样复发）
//	比出站更松 → 后台刷得比出站勤，白白多打上游请求（而本上游每刷一次就
//	             轮换一次 refreshToken ⇒ 多刷 = 多一次与桌面端互踢的机会）
//
// 所以直接复用 RefreshSkew：它已经是"这一份凭证多早算该刷"的**唯一**答案。
func (p *Provider) needsRefresh(a *Auth, now time.Time) bool {
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}
	skew, ok := p.RefreshSkew(cred)
	if !ok {
		// ok=false = "我提供不了窗口信息"（例如令牌不是 JWT、读不到 iat/exp）。
		// 用扫描间隔兜底 —— 与出站路径的兜底窗口同一个量级，且**绝不会更密**。
		skew = defaultRefreshScanInterval
	}
	if skew <= 0 {
		// (0, true) = 明确声明"不需要提前刷"。尊重它 —— 那是上游的属性。
		return false
	}
	exp, hasExp := a.ExpiresAt()
	if !hasExp || exp <= 0 {
		// 不知道过期时刻就**不能**因此就刷（那会变成每轮都刷一次，
		// 因为"未知"永远成立）。跳过，交给出站路径的 401 处置。
		return false
	}
	return exp*1000-now.UnixMilli() <= skew.Milliseconds()
}

// refreshCandidates 列出本轮要检查的凭证（**按剩余寿命升序**：最急的排前面）。
//
// 顺序：先用注入的 uid 枚举（生产路径：核心知道池里有哪些号），拿不到时回落
// 本实例的 creds（测试路径）。反过来会让生产永远扫不到东西。
//
// ⚠ 排序不是"好看"：首轮限额取的是**列表第一个**，所以"谁最急"必须由这里
// 定下来，而不是碰运气（map 遍历顺序是随机的）。
func (p *Provider) refreshCandidates() []*Auth {
	p.mu.RLock()
	enum := p.uidEnumerator
	local := make([]*Auth, 0, len(p.creds))
	for _, a := range p.creds {
		local = append(local, a)
	}
	p.mu.RUnlock()

	var out []*Auth
	if enum != nil {
		uids := enum()
		out = make([]*Auth, 0, len(uids))
		for _, uid := range uids {
			// ⚠ 走 p.cred(uid) 而不是本地 map —— 见 runRefresh 里
			// "必须刷池里那个对象"那段。
			if a := p.cred(uid); a != nil {
				out = append(out, a)
			}
		}
	}
	if len(out) == 0 {
		out = local
	}
	// 按剩余寿命升序（拿不到过期时刻的排最后 —— 它们本来也不会被刷）。
	sort.SliceStable(out, func(i, j int) bool {
		ei, oki := out[i].ExpiresAt()
		ej, okj := out[j].ExpiresAt()
		if !oki {
			return false
		}
		if !okj {
			return true
		}
		return ei < ej
	})
	return out
}
