// extensions.go TRAE 实现的全部可选扩展点。
//
// 按"上游只实现自己有的"原则，这里实现：
//
//	LoginFlow / AuthDirExt / CredentialLoader(+Secret) / CredentialRefresher / RefreshSkewExt
//	CredentialExpiryExt / QuotaExt / AccountColumnsExt / DailyActionExt / JobExt / AdminExt
//
// 刻意**不实现**：
//
//	SoftRateExt    上游没有给出"限流何时解除"，硬冷却即可。
//	ResetPolicyExt 权益不足的恢复时刻由 core 默认处理（本上游不额外声明）。
package trae

import (
	"context"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// shortErr 把错误压缩成一行短句（表格 detail 列用）。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

// 编译期断言：Provider 实现它声称支持的全部扩展点。
var (
	_ gateway.LoginFlow              = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.CredentialRefresher    = (*Provider)(nil)
	_ gateway.RefreshSkewExt         = (*Provider)(nil)
	_ gateway.CredentialExpiryExt    = (*Provider)(nil)
	_ gateway.QuotaExt               = (*Provider)(nil)
	_ gateway.AccountColumnsExt      = (*Provider)(nil)
	_ gateway.DailyActionExt         = (*Provider)(nil)
	_ gateway.JobExt                 = (*Provider)(nil)
	_ gateway.AdminExt               = (*Provider)(nil)
)

// ── 凭证目录与加载 ──────────────────────────────────────────────────────

// AuthDir 本上游的凭证落盘目录（gateway.AuthDirExt）。
func (p *Provider) AuthDir() string { return p.authDir }

// LoadCredentials 读取 dir 下本上游的全部凭证（gateway.CredentialLoader）。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	auths, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(auths))
	for _, a := range auths {
		out = append(out, gateway.Credential{
			Provider: providerID,
			UID:      a.UID,
			Nickname: a.Nickname,
			FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret
// （gateway.CredentialSecretLoader）。"同源"是硬要求：两边都走同一个 LoadDir。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	auths, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(auths))
	for _, a := range auths {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: providerID,
				UID:      a.UID,
				Nickname: a.Nickname,
				FilePath: a.FilePath,
			},
			Secret: a,
		})
	}
	return out, nil
}

// effectiveDir 决定用哪个目录：显式入参 > 装配注入。
func (p *Provider) effectiveDir(dir string) string {
	if dir != "" {
		return dir
	}
	return p.authDir
}

// localAccounts 扫本上游凭证目录。
func (p *Provider) localAccounts() ([]*Auth, error) {
	return LoadDir(p.authDir)
}

// ── 续期 ────────────────────────────────────────────────────────────────

// RefreshCredential 续期一份凭证（gateway.CredentialRefresher）。
//
// 与 codearts 同一条：refreshToken 是消费型的，Client.RefreshToken 内部持
// Auth 写锁串行执行 ExchangeToken；成功后 SaveAtomic 落盘（落盘失败只记日志，
// 内存里的新 token 已可用）。
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if err := p.client.RefreshToken(a); err != nil {
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("trae: 续期成功但落盘失败 uid=%s: %v", shortUID(a.UID), err)
	}
	return nil
}

// refreshSkew 提前续期窗口。
//
// ⚠ 已停用（见下）：TRAE 的 refreshToken 是消费型的，请求路径的预检续期
// 在账号池投影（ExpiresAt=0）下**恒为真** → 每个对话请求都先 ExchangeToken
// 轮换一次，轮换频率比真人客户端高两个量级，是"账号突然过期"的头号嫌疑。
// 该常量仅保留给后台窗口化续期（runRefresh 的 refreshScanSkew）作参照。
const refreshSkew = 10 * time.Minute

// RefreshSkew 返回本上游的提前续期窗口（gateway.RefreshSkewExt）。
//
// 返回 (0, **true**)：显式声明"不需要预检续期，只在 401 后被动续期"。
//
// # ⚠ 为什么 true 而不是 false（上一版修复在这里是失效的）
//
// needsRefreshVia 的分支语义（internal/server/handler.go）：
//
//	has=false → **回落核心兜底窗口**（10m）—— 不是"不刷"！
//	has=true 且 skew<=0 → 真正关闭预检
//
// 而账号池投影（cmd/server/traecreds.go）只带 {UID, Nickname}，ExpiresAt=0，
// auth.Auth.NeedsRefresh 对零值**恒真** —— 于是返回 (0,false) 时预检照旧
// 每个请求触发一次 ExchangeToken，一次性 refreshToken 仍被每请求消费，
// "账号突然过期"的根因形状原封不动。（评审实锤：报告 §6-1。）
// 唯一正确的关闭方式就是 (0,true)：显式"报过窗口"，且窗口为 0。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	return 0, true
}

// TokenExpiry 报告这份凭证的过期时刻（gateway.CredentialExpiryExt）。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil || a.ExpiresAt <= 0 {
		return 0, false
	}
	return a.ExpiresAt, true
}

// ── 额度 ────────────────────────────────────────────────────────────────

// RefreshQuota 取该账号的当前额度（gateway.QuotaExt）。
//
// TRAE 的额度 = 权益包 credits_limit 之和（UserEntUsage）。
// 失败/未找到账号 → HasData=false（界面显示「—」，不填 0）。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	if uid == "" {
		return gateway.UnknownQuota(), false
	}
	a := p.authByUID(uid)
	if a == nil {
		return gateway.UnknownQuota(), true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	remain, err := p.client.UserEntUsage(ctx, a)
	if err != nil {
		log.Printf("trae: 额度刷新失败 uid=%s: %v（显示为未知）", shortUID(uid), err)
		return gateway.UnknownQuota(), true
	}
	if remain < 0 {
		remain = 0
	}
	return gateway.CreditsQuota(remain), true
}

// authByUID 在本上游凭证目录里按 uid 找一份凭证。
func (p *Provider) authByUID(uid string) *Auth {
	if uid == "" {
		return nil
	}
	list, err := LoadDir(p.authDir)
	if err != nil {
		return nil
	}
	for _, a := range list {
		if a != nil && a.UID == uid {
			return a
		}
	}
	return nil
}

// ── 账号池列 ────────────────────────────────────────────────────────────

// AccountColumns 报 trae 在账号池里要显示的列（有序）。
//
// 与 workbuddy 首列一致（上游），含 Token 到期（走 CredentialExpiryExt）
// 与今日签到（走 DailyActionExt）。熔断/在途按用户本轮要求全上游不显示。
func (p *Provider) AccountColumns() []string {
	return []string{
		gateway.AccountColProvider,
		gateway.AccountColNickname,
		gateway.AccountColUID,
		gateway.AccountColQuota,
		gateway.AccountColStatus,
		gateway.AccountColTokenExpiry,
		gateway.AccountColCheckin,
		gateway.AccountColOps,
	}
}

// ── 每日动作 ────────────────────────────────────────────────────────────

// DailyActions 报 trae 的每日动作：签到。
//
// OneURL 单账号签到；AllURL 全量签到（Batch=true → 顶部出现「全部签到」）。
func (p *Provider) DailyActions() []gateway.DailyAction {
	return []gateway.DailyAction{
		{
			ID:     "trae-checkin",
			Label:  "签到",
			Title:  "TRAE SOLO 每日签到领取积分",
			OneURL: "/admin/trae/checkin",
			AllURL: "/admin/trae/checkin/all",
			Batch:  true,
		},
	}
}

// ── 定时任务 ────────────────────────────────────────────────────────────

// JobCheckin / JobRefresh 任务稳定标识。
const (
	JobCheckin = "trae-checkin"
	JobRefresh = "trae-refresh"
)

// Jobs 返回本上游要注册的定时任务（gateway.JobExt）。
func (p *Provider) Jobs() []gateway.Job {
	jobs := make([]gateway.Job, 0, 2)
	if p.checkinEnabled {
		jobs = append(jobs, gateway.Job{
			Name:     JobCheckin,
			Interval: checkinInterval,
			Run:      p.runCheckin,
		})
	}
	if p.refreshInterval > 0 {
		jobs = append(jobs, gateway.Job{
			Name:     JobRefresh,
			Interval: p.refreshInterval,
			Run:      p.runRefresh,
		})
	}
	return jobs
}

// checkinInterval 签到任务的扫描间隔。
//
// 不做"到点才跑"的 Due 门控：每次跑都先查 CheckinStatus，
// 已签就跳过 —— 幂等且跨重启安全（比"每天 9 点整"更稳）。
const checkinInterval = 30 * time.Minute

// recordCheckin 写一条签到历史（「今日签到」列的数据源）。
//
// # 为什么必须有它（用户报的小 bug）
//
// 管理台账号表的「今日签到」列读的是 checkinlog 的 KindCheckin 记录
// （internal/admin/admin.go），不是实时去上游查。此前 trae 签到后不写
// 历史，于是今天已经签到也不显示 —— 这就是"签到按钮点了、列还是空"。
func (p *Provider) recordCheckin(uid, status, detail string, credits int64, trigger string) {
	if p == nil || p.log == nil {
		return
	}
	p.log.Append(checkinlog.Record{
		At:      time.Now(),
		UID:     checkinlog.NormalizeUID(uid),
		Kind:    checkinlog.KindCheckin,
		Status:  status,
		Detail:  detail,
		Credits: credits,
		Trigger: trigger,
	})
}

// runCheckin 一趟签到：扫凭证，逐个查状态、未签则领；结果写历史。
func (p *Provider) runCheckin(ctx context.Context) error {
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
		checkedIn, credits, enable, serr := p.client.CheckinStatus(ctx, a)
		if serr != nil {
			log.Printf("trae: 签到状态查询失败 uid=%s: %v", shortUID(a.UID), serr)
			p.recordCheckin(a.UID, checkinlog.StatusFail, shortErr(serr), 0, "sched")
			continue
		}
		if !enable {
			continue // 上游关闭了签到
		}
		if checkedIn {
			p.recordCheckin(a.UID, checkinlog.StatusAlready, "今天已签到", credits, "sched")
			continue // 今天已签
		}
		if cerr := p.client.CheckinClaim(ctx, a); cerr != nil {
			log.Printf("trae: 签到失败 uid=%s: %v", shortUID(a.UID), cerr)
			p.recordCheckin(a.UID, checkinlog.StatusFail, shortErr(cerr), 0, "sched")
			continue
		}
		log.Printf("trae: 签到成功 uid=%s credits=%d", shortUID(a.UID), credits)
		p.recordCheckin(a.UID, checkinlog.StatusOK, "", credits, "sched")
	}
	return nil
}

// runRefresh 一趟后台续期：扫凭证，对**临近过期**的账号续期（用户要求：
// 所有上游统一 30 分钟粒度扫描，但只刷需要刷的）。
//
// # 为什么不是无条件全量续（修复"账号突然过期"）
//
// refreshToken 是**消费型**的（每次 ExchangeToken 即轮换）。无条件每 30 分钟
// 全量续，等于把 refreshToken 链每 30 分钟转一圈：
//
//   - 与桌面客户端共用同一账号时，桌面端与网关抢一条 refreshToken 链，
//     一边刷新另一边立刻失效 → 网关下次 ExchangeToken 失败 → 到期 401 →
//     计数禁用，界面表现为"账号突然过期"。
//   - 频繁轮换也更容易被上游风控判定为异常。
//
// 改为「距过期 30 分钟才刷」（对齐 trae-local-api 的 isTokenExpiringSoon
// 阈值）：寿命数小时的 token 每天只轮换一两次，而不是 48 次。请求路径的
// needsRefreshVia（10 分钟窗口）仍会在对话前兜底；真正的死 token（refreshToken
// 被消费/过期）走下面的 RefreshTokenDead 分支 —— 直接记失败并让装配层
// 计数禁用（界面显示「需重新登录」），不再反复白刷。
const refreshScanSkew = 30 * time.Minute

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
		if a.RefreshToken == "" {
			continue
		}
		// refreshToken 已死（自身过期）：续期必然失败，直接预警 + 计数，
		// 让 pool 在连续失败后禁用并显示「需重新登录」（界面可见，不再静默）。
		if a.RefreshTokenDead() {
			log.Printf("trae: uid=%s refreshToken 已过期（RefreshExpiresAt=%d），"+
				"无法自动续期 —— 请在控制台重新登录该账号",
				shortUID(a.UID), a.RefreshExpiresAt)
			if p.onRefreshFailure != nil {
				p.onRefreshFailure(a.UID)
			}
			continue
		}
		// 距过期 30 分钟内才刷（NeedsRefresh 判定，含已过期）。
		if !a.NeedsRefresh(refreshScanSkew) {
			continue
		}
		need = append(need, a)
	}
	if len(need) == 0 {
		return nil
	}
	log.Printf("trae: 后台续期开始，%d 个账号临近过期（30 分钟窗口）", len(need))
	for _, a := range need {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := p.client.RefreshToken(a); err != nil {
			log.Printf("trae: 后台续期失败 uid=%s: %v", shortUID(a.UID), err)
			// 通知装配层（pool.NoteRefreshFailure）：连续失败自动禁用，
			// 死 token 账号在账号池里可见「需重新登录」。
			if p.onRefreshFailure != nil {
				p.onRefreshFailure(a.UID)
			}
			continue
		}
		if p.onRefreshSuccess != nil {
			p.onRefreshSuccess(a.UID)
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("trae: 续期成功但落盘失败 uid=%s: %v", shortUID(a.UID), err)
		}
	}
	return nil
}
