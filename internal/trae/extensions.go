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
	"time"

	"workbuddy2api/internal/gateway"
)

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
// TRAE 的 accessToken 寿命以小时计（实测 traework2api 的 schedule refresh_hours
// 每天刷 1 次），提前 10 分钟足够覆盖 401 往返与调度抖动。
const refreshSkew = 10 * time.Minute

// RefreshSkew 返回本上游的提前续期窗口（gateway.RefreshSkewExt）。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	_, err := authOf(cred)
	if err != nil {
		return 0, false
	}
	return refreshSkew, true
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

// runCheckin 一趟签到：扫凭证，逐个查状态、未签则领。
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
			continue
		}
		if !enable {
			continue // 上游关闭了签到
		}
		if checkedIn {
			continue // 今天已签
		}
		if cerr := p.client.CheckinClaim(ctx, a); cerr != nil {
			log.Printf("trae: 签到失败 uid=%s: %v", shortUID(a.UID), cerr)
			continue
		}
		log.Printf("trae: 签到成功 uid=%s credits=%d", shortUID(a.UID), credits)
	}
	return nil
}

// runRefresh 一趟后台续期：扫凭证，对将过期的账号串行续期。
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
		if a.NeedsRefresh(refreshSkew) {
			need = append(need, a)
		}
	}
	if len(need) == 0 {
		return nil
	}
	log.Printf("trae: 后台续期开始，%d 个账号临近过期（窗口 %v）", len(need), refreshSkew)
	for _, a := range need {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := p.client.RefreshToken(a); err != nil {
			log.Printf("trae: 后台续期失败 uid=%s: %v", shortUID(a.UID), err)
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("trae: 续期成功但落盘失败 uid=%s: %v", shortUID(a.UID), err)
		}
	}
	return nil
}
