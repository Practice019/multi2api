package qoder

import (
	"context"
	"errors"
	"log"
	"time"

	"workbuddy2api/internal/gateway"
)

// jobs_refresh.go —— qoder / qodercn 的后台主动续期。
//
// # 为什么必须有它（用户报障：「没有 Token」）
//
// 界面「Token」列的权威是 `Auth.ExpiresAt`，而它**只能从续期响应拿到**：
//
//	GET/POST /api/v1/deviceToken/refresh
//	  → {"device_token":…, "expires_at":"2026-10-30T06:56:55Z", …}
//
// access_token 自己是 `dt-` 前缀的**不透明串**（27 字符，不是 JWT），
// 解不出 `exp`；凭证结构上也没有独立的过期字段。所以"不续期"→
// **永远不知道什么时候过期** → 界面恒显示 `—`。
//
// 而本上游此前只有签到任务（见 checkin 侧的 Jobs），**没有续期任务**：
// `RefreshCredential` 只有"有人发对话请求"时才会被出站循环顺带调一次。
// 没人用 qoder 发请求 → 从不续期 → Token 列永远是空的（用户报障的形态）。
//
// # 与 cline 那次是同一模式
//
// cline 是"有 RefreshCredential 但没 Jobs()"，qoder 是"有 Jobs() 但只有签到"。
// 两者都属于"能力实现了、驱动路径没接上"。
//
// # ⚠ 刷新的对象必须是**池里那个**（cline 那轮踩过的分叉）
//
// 用 `p.creds(uid)`（装配层注入的活 secret 访问器），不是 `LoadDir` 的
// 磁盘副本 —— 否则会出现"落盘已续期、界面仍显示旧值"。
// 详见 internal/cline/jobs.go 的注释，那边记了实测数据。
var _ gateway.JobExt = (*Provider)(nil)

// refreshScanInterval 续期扫描间隔。
//
// 取 30 分钟：与签到同一节奏（本仓"所有上游统一节奏"的既有取舍）。
// qoder 的 access_token 寿命约 **30 天**（实测 `expires_at` 与 `created_at`
// 相差整月），所以 30 分钟一轮绰绰有余；密的收益为零。
//
// 触发窗口由 `RefreshSkew` 决定（寿命的 50% = 约 15 天）——
// 也就是说正常情况下几乎每轮都是"跳过"，只有真的接近过期才会刷。
// 这不浪费：一轮的开销只是读一次本地 token 解出 iat/exp，没有网络。
const refreshScanInterval = 30 * time.Minute

// refreshJobName 本任务的稳定名。
//
// ⚠ **必须带上产品 id**：qoder 与 qodercn 是**同类型的两个实例**，
// 都注册同一个名字时调度器会判重并**静默跳过后者** ——
//
//	scheduler: 任务名 qoder-refresh 重复（上游 qoder 与 qodercn），已跳过后者
//
// 实测症状：中国版的 token 永不被续期 —— 而用户报「没有 Token」的
// 那个号**恰恰是中国版**。日志里只有一行"重复"，不报错、不重试。
func (p *Provider) refreshJobName() string {
	return p.productID + "-refresh"
}

// Jobs 注册后台任务：签到（见 checkin.go）+ **续期**（本文件）。
//
// 两个任务是两件事，所以合并在这里返回 —— 上游的 `Jobs()` 只有一处，
// 分两个方法返回会漏掉一个（那正是本轮缺陷的形态）。
func (p *Provider) Jobs() []gateway.Job {
	var jobs []gateway.Job
	if p != nil && p.checkin != nil && p.checkin.AutoEnabled() {
		jobs = append(jobs, gateway.Job{
			Name:     p.checkin.Descriptor().ID,
			Interval: p.checkin.Interval(),
			Run:      p.checkin.RunAll,
		})
	}
	if p != nil && p.authDir != "" {
		jobs = append(jobs, gateway.Job{
			Name:     p.refreshJobName(),
			Interval: refreshScanInterval,
			Run:      p.runRefresh,
		})
	}
	return jobs
}

// runRefresh 扫描凭证，把临近过期的续期（并因此拿到/更新过期时刻）。
//
// ⚠ 逐个错误只记日志、不中断整轮：一个号的 refresh_token 真失效了，
// 不该让其它号失去续期机会。
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
		p.writeBackQuota(ctx, a)

		// 额度写回：**每轮都做**，且**不受下面的 continue 影响**。
		//
		// # 为什么与续期完全解耦（用户报「额度是 0」）
		//
		// `gateway.QuotaExt.RefreshQuota` 只在有人点「刷新本上游额度」时
		// 被核心调用 —— **没有**定时任务扫它。所以重启后额度列一直是空的：
		// 界面上那个 0 不是"余额 0"，而是"我们还没问过上游"。
		//
		// ⚠ **位置很要紧**：我第一版把它放在下面那串 continue 之后，
		// 于是"没有 refresh_token 的号"直接跳过了额度写回 —— 那种号
		// 额度永远不会更新（而它们的 token 恰恰不会过期，最需要看余额）。
		// 额度与 token 是**两件独立的事**，判据不该共用。
		// 不可续期（没有 refresh_token）：刷不了，如实跳过（不空转）。
		if !a.Renewable() {
			skipped++
			continue
		}
		// 「该刷吗」复用 RefreshSkew 的同一判据（寿命比例），
		// 保证后台与请求路径不会给出不同答案。
		//
		// ⚠ **过期时刻未知（ExpiresAtMS()==0）时也要刷一次**：
		// 这正是用户报障的情形 —— 旧凭证没存 `expires_at`，而那个值
		// **只能**从续期响应拿到。若不刷，界面永远显示 `—`。
		// 所以"未知"在这里要当成"值得刷一次去问清楚"，而不是"不刷"。
		if !p.shouldRefresh(a, now) {
			skipped++
			continue
		}
		if err := p.RefreshCredential(gateway.Credential{
			Provider: p.productID, UID: a.UIDValue(), Secret: a,
		}); err != nil {
			failed++
			if errors.Is(err, ErrRefreshExpired) {
				log.Printf("%s: uid=%s refresh_token 已失效，无法自动续期 —— 请重新登录/导入",
					p.productID, shortUID(a.UIDValue()))
			} else {
				log.Printf("%s: uid=%s 后台续期失败（可重试）：%v",
					p.productID, shortUID(a.UIDValue()), err)
			}
			// ⚠ 续期失败**不**影响额度（额度端点只需要 access_token），
			// 所以那条写回在循环开头就已经做过了。
			continue
		}
		refreshed++
	}
	if refreshed > 0 || failed > 0 {
		log.Printf("%s: 后台续期完成 —— 续期 %d，跳过 %d，失败 %d",
			p.productID, refreshed, skipped, failed)
	}
	return nil
}

// writeBackQuota 查一次额度并写回账号池。
//
// # 为什么必须有它（用户报「额度是 0」）
//
// `gateway.QuotaExt.RefreshQuota` 只在有人点「刷新本上游额度」时被核心调用
// —— 全仓没有定时任务扫它（只有 workbuddy 在签到/旅行路径里顺带回写）。
// 于是**重启后额度列一直是空的**：界面上那个 0 不是"余额 0"，
// 而是"我们还没问过上游"。
//
// # 为什么放在续期任务里而不是新起一个任务
//
// 续期与额度打的是同一个 host（`openapi.qoder.sh` / `...com.cn`），
// 已有的一轮扫描顺手多一次 GET 即可；另起任务要再配一次间隔与
// 去重（而且任务名又要按产品区分 —— 上一轮刚在那上面栽过）。
//
// ⚠ 失败**只记日志**：额度查不到不该影响续期（两者是不同的接口）。
// 也**不写 0** —— `RefreshQuota` 查不到时返回 `HasData=false`，
// 那种情况不写池（界面保持"未知"而不是"0"）。
func (p *Provider) writeBackQuota(ctx context.Context, a *Auth) {
	if p == nil || p.quotaSink == nil || a == nil {
		return
	}
	uid := a.UIDValue()
	if uid == "" {
		return
	}
	qv, ok := p.RefreshQuota(uid)
	if !ok {
		// 本包服务不了这个 uid（不该发生 —— 候选是从自己的凭证里挑的）。
		return
	}
	// ⚠ 只有真拿到数据才写。`HasData=false` 是"上游没给出可用余额"
	//（凭据过期 / 限流 / 活动未开启），写进去会让界面显示 0 ——
	// 比"未知"更糟，因为它看起来像"额度被用光了"。
	if !qv.HasData {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	p.quotaSink(uid, qv)
}

// SetQuotaSink 注入额度写回通道（装配层调用）。
//
// 与 SetCredentialSource 同形：上游不认识账号池的类型，
// 由装配层把 `pool.SetQuota` 适配进来。
func (p *Provider) SetQuotaSink(fn func(uid string, q gateway.QuotaView)) {
	if p == nil {
		return
	}
	p.quotaSink = fn
}

// shouldRefresh 报告这份凭证现在该不该续期。
//
// # 两条判据（顺序不能反）
//
//  1. **过期时刻未知** → 刷。这是用户报障的情形：旧凭证没有 `expires_at`，
//     而那个值只能从续期响应拿到。不刷就永远不知道 → 界面恒显示 `—`。
//     ⚠ 这条与 `RefreshSkew` 的"不返回 (0,true)"并不矛盾：那里说的是
//     **请求路径**的预检窗口（每次出站请求都问一次，恒真就变成"每请求都续期"）；
//     这里是后台每 30 分钟一轮，刷一次之后 `expires_at` 就有值了，
//     下一轮自然落回正常判据 —— 不会退化成每轮都刷。
//  2. 否则按**寿命比例**（与请求路径同一个 `RefreshSkew`）判断。
func (p *Provider) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil {
		return false
	}
	if a.ExpiresAtMS() <= 0 {
		return true
	}
	skew, ok := gateway.RefreshSkewFromToken(a.AccessToken)
	if !ok {
		// token 不是 JWT（qoder 的 `dt-` 串就是这种）：没有比例窗口可比，
		// 但有绝对过期时刻 —— 用它按"距过期不足一轮扫描间隔"判断。
		skew = refreshScanInterval
	}
	ms := a.ExpiresAtMS()
	return ms > 0 && ms <= now.Add(skew).UnixMilli()
}

// refreshCandidates 返回本轮要检查的凭证（**池里的活对象**优先）。
//
// # ⚠ 必须按 productID 过滤（实测抓到的）
//
// qoder 与 qodercn 共用同一个凭证目录（`auths/qoder/`），靠文件里的
// `product_id` 区分。两个实例都会跑本任务 —— 若各自读**全部**凭证：
//
//	qoder  实例拿到 CN 的号 → 用国际版端点发 CN 的 refresh_token → 401
//	qodercn 实例拿到 INTL 的号 → 反之亦然
//
// 表现是"一半的号续期必然失败"，日志写着 `refresh_token 已失效`
// —— 而那个 refresh_token 在**对的**端点上完全可用（实测 200）。
// 极易被误读成"用户的号真失效了"，让人去重新登录。
//
// 所以用 `LoadDirFor(dir, p.productID)`。
//
// # 池里那份优先
//
// 见文件头：刷磁盘副本会导致"落盘已续期、界面仍显示旧值"。
// `creds` 未接线（纯单测）时退回磁盘 —— 那种情况下没有池，也就没有分叉。
func (p *Provider) refreshCandidates() ([]*Auth, error) {
	disk, err := LoadDirFor(p.authDir, p.productID)
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
		uid := d.UIDValue()
		if uid == "" {
			continue
		}
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
