// jobs.go —— minimax 的后台主动续期。
//
// # 为什么必须有它（用户报障：「MiniMax 的账号没有自动续期」）
//
// ⚠ 这与 raccoon / cline 修之前**逐字同一种病**，而 minimax 是**第三个**中的招的。
// 前两次的完整记录见 internal/raccoon/jobs.go 与 internal/cline/jobs.go ——
// 这里只讲 minimax 自己那几处不同。
//
// # 根因：核心的续期只有两条触发路径，minimax 一条都不占
//
//	① 出站请求时   handler 的 needsRefreshVia 判"该刷了"才刷
//	② 后台定时任务 上游通过 gateway.JobExt 自报的 Jobs()
//
// minimax **满足** `gateway.CredentialRefresher`（refresh.go 的
// RefreshCredential 签名正确、原地写回、也落了盘）、也**满足**
// `gateway.RefreshSkewExt`（extensions.go 正确报了"已用寿命 ≥50% 该刷"）——
// 但**没有 Jobs()**。
//
// 于是它只有"有人拿 minimax 号发对话请求"时才会顺手续期。没人用时静静过期 ——
// 界面上 Token 列显示"已过期"，而 refresh_token 明明还能用。
//
// # 为什么这个上游特别容易漏（值得单独记）
//
// 其余上游都有**开源参照实现**可以逐行照抄（见 DELIVERY.md：那个 TypeScript
// 项目 231 文件），而它的 `src/` 里**没有 minimax** —— 本上游是照官方桌面端
// 的 asar 与实测推出来的自研实现。没有参照时，"照着别人的结构补齐一个 Jobs()"
// 这件事就没有样本可抄，于是漏了。
//
// 这也解释了为什么本文件是照 raccoon 写的（那个上游有参照、且已经踩过同一个坑）。
//
// # 教训（与前两次完全相同，所以第三次更要写下来）
//
// 一个上游"具备续期能力"与"会去续期"是**两件事**：
// 前者是方法签名，后者是**注册了后台任务**。缺后者时**没有任何报错**，
// 只是悄悄过期 —— 而这正是最容易被漏掉、也最难自查的一类空缺。
//
// 对照（本次修完后）：workbuddy / codearts / cline / lobsterai / qoder /
// raccoon / trae / **minimax** 都有后台续期任务，不会停在"已过期"。
// zcode 是唯一合法的例外：JWT 通道**上游根本没有 refresh 接口**（它用
// RefreshSkew 回 (0,true) 明确声明"不需要提前刷"，见该方法的注释）。
package minimax

import (
	"context"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：Provider 实现后台任务扩展点。
//
// 缺了它不会编译失败，而是**静默退化**成"只在被人用到时续期"——
// 也就是本次报障的形态。断言在这里，是为了让"哪天有人删掉 Jobs()"
// 立刻编译不过，而不是等用户发现 token 过期。
var _ gateway.JobExt = (*Provider)(nil)

// JobRefresh 后台续期任务的注册名（出现在控制台的定时任务列表里）。
const JobRefresh = "minimax-refresh"

// defaultRefreshScanInterval 后台扫描间隔。
//
// # 取值依据（minimax 的寿命特点）
//
// minimax 的 access_token 寿命由服务端下发（参照项目的实测结论：
// `expires_in` 因环境而异），而 RefreshSkew 的判据是**寿命的一半**
// （extensions.go 里那条：下限 2m、上限 30m）。
//
// 取 10 分钟一轮的理由：
//
//	寿命 1h  → 该刷点是剩 30m  → 最坏延迟 10m，还有 20m 余量给重试
//	寿命 10m → 该刷点是剩 5m（下限 2m 夹紧后的实际值）→ 一轮就能覆盖
//
// 比这更密没必要（寿命是分钟~小时级，且扫描本身要读一遍凭证目录），
// 更疏会吃掉重试余量 —— 而**重试余量恰恰是这三次报障的关键**：
// token 过期几小时都没被碰过，说明"只在被人用到时才刷"完全不够。
const defaultRefreshScanInterval = 10 * time.Minute

// refreshInterval 本实例的扫描间隔（<=0 表示不注册后台任务）。
//
// 判据：没有凭证目录 = 没有账号要刷，起任务只是空转。
// 与 raccoon / cline 同一口径。
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
// 这是本仓栽过多次的坑（cline 的 jobs.go 有完整记录）：池子持有的是
// 启动时放进 `secrets[uid]` 的那个 `*Auth`，而 `LoadDir` 每次都从磁盘
// **新建**对象。刷后者只会更新磁盘，池里那份纹丝不动 ——
// 而界面读的是池（`TokenExpiry` 拿到的 `cred.Secret` 就是池里那个指针），
// 于是出现"落盘已续期、界面仍显示已过期"。
//
// 所以这里全部走 `p.cred(uid)` —— 它**优先**问注入的凭证源
// （生产路径：池里的活 secret），未接线时才回落本实例的 creds（测试路径）。
// 这一步是必需的，不是优化。
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
		// 刷不了的（没有 refresh_token）如实跳过。
		//
		// # ⚠ 这条过滤的真实成本（我第一版把它写大了，这里改正）
		//
		// 我原先写的是"不过滤就会反复发上游请求、失败还会累计禁用计数"。
		// **两条都不对**，查证如下：
		//
		//	· `RefreshCredential` 在 `!a.Refreshable()` 时**提前返回**，
		//	  根本不发 HTTP（见 refresh.go 那个 early return）——所以没有
		//	  "反复打上游"这回事
		//	· 禁用计数 `NoteRefreshFailure` **只在请求路径**被调用
		//	  （internal/server/handler.go:2165/2181，唯一入口是该文件的
		//	  refreshCredential）——后台任务里 per-account 失败只进本函数
		//	  的局部 `failed`，**不会**记到池子上，更不会禁用账号
		//
		// 所以这条 continue 的真实收益是**小**的：
		//
		//	① 省掉每轮每号一次纯 CPU 的方法调用（可忽略）
		//	② **不让日志被刷屏** —— 没有它，每个刷不了的号每 10 分钟
		//	   都会打一条"后台续期失败（可重试）"，永久如此，而它
		//	   永远不会成功（那种"永远会失败的重试"最消耗排查者的注意力）
		//	③ 让**「刷不了」与「刷失败」在代码里就是两件事** ——
		//	   前者跳过、后者计数并报错。语义分开比合并好，哪怕当前
		//	   合并也不会造成状态差异
		//
		// 保留这条过滤是按 ②③ 的理由，不是按 ①。把理由写准很重要：
		// 一条被夸大的注释会让人以为它拦住了大问题，从而在别处放走真的。
		if !a.Refreshable() {
			skipped++
			continue
		}
		if !p.needsRefresh(a, now) {
			skipped++
			continue
		}
		if err := p.RefreshCredential(gateway.Credential{
			Provider: providerID, UID: a.UID(), Secret: a,
		}); err != nil {
			failed++
			// 终态（refresh_token 也失效）与可重试分开报 —— 用户才知道
			// 该不该去重新登录。混成一条会让他反复重试一个不可能成功的动作。
			if isTerminalRefreshError(err.Error()) {
				log.Printf("minimax: uid=%s refresh_token 已失效，无法自动续期 —— 请重新登录/导入",
					a.DisplayUID())
			} else {
				log.Printf("minimax: uid=%s 后台续期失败（可重试）：%v", a.DisplayUID(), err)
			}
			continue
		}
		refreshed++
	}
	// 只在真有动作时打一行汇总：空闲部署（全都还新鲜）不该每 10 分钟刷屏。
	// `skipped` 不打进日志 —— 它每轮都等于账号数，是纯粹的噪音。
	if refreshed > 0 || failed > 0 {
		log.Printf("minimax: 后台续期 成功 %d / 失败 %d（跳过 %d）", refreshed, failed, skipped)
	}
	return nil
}

// needsRefresh 这份凭证现在该不该续期。
//
// # 判据必须与**出站路径**同源（这是关键，不是细节）
//
// 出站路径走 `needsRefreshVia` → `RefreshSkew`（"寿命的一半"，夹在 2m~30m）。
// 如果这里另写一个判据（例如固定"剩 5 分钟"），就会出现两种后果之一：
//
//	比出站更严 → 后台永远不刷，等于没加这个任务（本次报障原样复发）
//	比出站更松 → 后台刷得比出站勤，白白多打上游请求
//
// 所以直接复用 `RefreshSkew`：它已经是"这一份凭证多早算该刷"的**唯一**答案。
// 这也是本仓反复出现的形状（jobs 复用 RefreshSkew，见 raccoon / qoder 的注释）。
func (p *Provider) needsRefresh(a *Auth, now time.Time) bool {
	skew, ok := p.RefreshSkew(gateway.Credential{
		Provider: providerID, UID: a.UID(), Secret: a,
	})
	if !ok {
		// RefreshSkew 回 ok=false = "我提供不了窗口信息"（例如寿命未知）。
		// 那时用一个保守的固定窗口兜底 —— 与 RefreshSkew 内部
		// "寿命未知 ⇒ 10 分钟"那条保持同一个量级。
		skew = defaultRefreshScanInterval
	}
	if skew <= 0 {
		// (0, true) = 明确声明"不需要提前刷"。尊重它 —— 那是上游的属性。
		return false
	}
	exp := a.ExpiresAtMS()
	if exp <= 0 {
		// 不知道过期时刻：**不能**因此就刷（那会变成每轮都刷一次，
		// 因为"未知"永远成立）。跳过，交给出站路径的 401 处置。
		return false
	}
	return exp-now.UnixMilli() <= skew.Milliseconds()
}

// refreshCandidates 列出本轮要检查的凭证。
//
// 顺序：先用注入的 uid 枚举（生产路径：核心知道池里有哪些号），
// 拿不到时回落本实例的 creds（测试路径）。**反过来会让生产永远扫不到东西** ——
// 本包的 creds 在生产里是空的（见 Provider 结构体那段注释：
// "p.creds 只有测试会填"）。
func (p *Provider) refreshCandidates() []*Auth {
	p.mu.RLock()
	enum := p.uidEnumerator
	p.mu.RUnlock()

	var uids []string
	if enum != nil {
		uids = enum()
	}
	out := make([]*Auth, 0, len(uids))
	if len(uids) > 0 {
		for _, uid := range uids {
			// ⚠ 走 p.cred(uid) 而不是只查本地 map —— 见 runRefresh 里
			// "必须刷池里那个对象"那段。
			if a := p.cred(uid); a != nil {
				out = append(out, a)
			}
		}
		return out
	}
	// 回落：本实例自己维护的 creds（仅测试会走到）。
	p.mu.RLock()
	for _, a := range p.creds {
		if a != nil {
			out = append(out, a)
		}
	}
	p.mu.RUnlock()
	return out
}
