// extensions.go MiMo 实现的全部可选扩展点（集中在本文件，trae 同构）。
//
// # 事故档案 —— RefreshSkew 为什么恒 (0, true)（评审报告 §6-1，务必读）
//
// needsRefreshVia（internal/server/handler.go）的分支语义：
//
//	has=false → **回落核心兜底窗口 10m**，不是"不刷"！
//	has=true && skew<=0 → 真正关闭预检
//
// 账号池投影只带 {UID, Nickname}（cmd/server/mimocreds.go，同 traecreds.go）
// ⇒ auth.Auth.NeedsRefresh 对 ExpiresAt=0 **恒真** ⇒ 返回 (0,false) 等于没改：
// 每个对话请求仍会预检续期、消费一次性 refreshToken（TRAE"账号突然过期"的
// 根因形状）。**唯一有效的关闭 = (0, true)**。本包恒如此，别改。
//
// # 本上游实现/不实现清单（理由见报告 §4.2 表）
//
//	实现：Provider / AdminExt / JobExt / LoginFlow / AuthDirExt / CredentialLoader(+Secret)
//	      CredentialRefresher（仅 oauth/free 有实义，sk 无害 no-op）/ RefreshSkewExt
//	      CredentialExpiryExt / CredentialLifetimeExt / AccountColumnsExt / ErrorClassifier
//	      SoftRateExt / HealthProbeExt
//	不实现：DailyActionExt（MiMo 无签到/领取端点，三源一致 0 发现）
//	        ResetPolicyExt / QuotaExt / ModelMultiplierExt / CredentialTokenExt
//	        （无官方数据源 —— "上游没有就不造"，loomy 文体）
package mimo

import (
	"context"
	"log"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言群：Provider 实现它声称的全部扩展点。
var (
	_ gateway.LoginFlow              = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.CredentialRefresher    = (*Provider)(nil)
	_ gateway.RefreshSkewExt         = (*Provider)(nil)
	_ gateway.CredentialExpiryExt    = (*Provider)(nil)
	_ gateway.CredentialLifetimeExt  = (*Provider)(nil)
	_ gateway.AccountColumnsExt      = (*Provider)(nil)
	_ gateway.ErrorClassifier        = (*Provider)(nil)
	_ gateway.SoftRateExt            = (*Provider)(nil)
	_ gateway.HealthProbeExt         = (*Provider)(nil)
	_ gateway.JobExt                 = (*Provider)(nil)
	_ gateway.AdminExt               = (*Provider)(nil)
	_ gateway.ManualLoginExt         = (*Provider)(nil)
)

// ── 手动登录完成（服务器部署逃生路径）──────────────────────────────────

// ManualLogin 自报手动粘贴完成端点（gateway.ManualLoginExt）。
//
// 场景：网关跑在 Linux 服务器上，OAuth 回调地址是"浏览器那台机器"的
// 127.0.0.1 —— 收不到。manual 模式（mimo.oauth_redirect_mode=manual）下
// 授权完成页会展示密文回跳 URL，用户复制过来粘贴即可。
func (p *Provider) ManualLogin() (string, string) {
	return completePath, "浏览器授权完成后：若回跳页面打不开或提示连接被拒绝，请复制浏览器地址栏的完整回跳 URL（含 u=…），粘贴到这里完成添加。"
}

// ── 凭证目录与加载 ──────────────────────────────────────────────────────

// AuthDir 本上游凭证落盘目录。
func (p *Provider) AuthDir() string { return p.authDir }

// LoadCredentials / LoadCredentialsWithSecrets 同源（同一个 LoadDir）。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.Credential{
			Provider: providerID, UID: a.UID, Nickname: a.Nickname,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同一次扫描，附带 Secret。
// ⚠ "同源"是硬要求：secret 缺失的账号被选中时取不到真凭证（R2 教训）。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{Provider: providerID, UID: a.UID, Nickname: a.Nickname, FilePath: a.FilePath},
			Secret:     a,
		})
	}
	return out, nil
}

func (p *Provider) effectiveDir(dir string) string {
	if dir != "" {
		return dir
	}
	return p.authDir
}

// localAccounts 扫本上游凭证目录。
func (p *Provider) localAccounts() ([]*Auth, error) { return LoadDir(p.authDir) }

// ── 续期 ────────────────────────────────────────────────────────────────

// RefreshCredential 续期一份凭证。
//
// 三态语义（loomy 判据的反面 —— MiMo 的 oauth/free 轨**有**可刷东西）：
//   - 纯 sk：返回 nil（**无害 no-op，不报错**）—— 预检已由 (0,true) 关闭，
//     这个分支只会被后台 Job/手动路径意外触达，报错反而制造"续期失败"噪音；
//   - oauth：持写锁整段刷新 + 落盘（失败不覆写，refreshToken 带回才替换）；
//   - free：作废旧票重 bootstrap（幂等）。
func (p *Provider) RefreshCredential(cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if !a.Renewable() {
		return nil // 纯 sk：没有可刷的材料
	}
	return p.renewForChat(context.Background(), a)
}

// RefreshSkew 恒 (0, true)：显式声明"不需要预检续期"。见文件头事故档案。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	return 0, true
}

// TokenExpiry 凭证过期时刻（sk → ok=false，**别造 0**：0 会被读成"1970 年过期"）。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return 0, false
	}
	if a.ExpiresAt > 0 {
		return a.ExpiresAt, true
	}
	return 0, false
}

// NeverExpires "这份凭证设计上不过期"。
//
// 判据：paid+api 且没有可刷材料（纯 sk）→ true —— UI 显示「长期有效」，
// 而不是把"没读到过期时刻"渲染成 `—` 让人以为功能缺失（credential_expiry
// 三态教训）。oauth/free 轨有到期时刻，轮不到这里。
func (p *Provider) NeverExpires(cred gateway.Credential) bool {
	a, err := authOf(cred)
	if err != nil || a == nil {
		return false
	}
	return a.Channel == ChannelPaid && a.Type == TypeAPI
}

// ── 账号池列 ────────────────────────────────────────────────────────────

// AccountColumns 本上游在账号池的列集（首列统一为"上游"，核心加）。
// 无签到列（没有签到）；quota 列等有真实观测源再进（现在进来只会恒 `—`）。
func (p *Provider) AccountColumns() []string {
	return []string{
		gateway.AccountColProvider,
		gateway.AccountColNickname,
		gateway.AccountColUID,
		gateway.AccountColStatus,
		gateway.AccountColTokenExpiry,
		gateway.AccountColSuccess,
		gateway.AccountColOps,
	}
}

// ── 健康探测 ────────────────────────────────────────────────────────────

// ProbeHealth 用 GET {base}/models 探测（便宜、真实带鉴权；401 即不健康）。
// free 轨没有 /models：探测=重 bootstrap 能否拿到票（唯一可用手段，报告 §2.2）。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.Channel == ChannelFree {
		_, err := p.client.Bootstrap(ctx, a.Fingerprint)
		return err
	}
	_, err = p.client.FetchModels(ctx, a)
	return err
}

// ── 软限流恢复时刻 ──────────────────────────────────────────────────────
//
// 契约签名只有 (status, body) —— 响应头（Retry-After / X-RateLimit-Reset）
// 到不了这里，所以 body 形态优先，其余用短兜底。
// ⚠ 兜底默认 5min：两参考项目 1h vs 5min 分歧取短者 —— 本仓有主动健康检查
// （pool-health-check）兜底提前回血，短冷却的最坏代价只是多几次探测。

// softRateFallback 拿不到时刻时的短兜底。
const softRateFallback = 5 * time.Minute

// SoftRateReset 解析恢复时刻（gateway.SoftRateExt）。
func (p *Provider) SoftRateReset(status int, body string) (time.Time, bool) {
	return ParseSoftRateReset(status, body, time.Now())
}

// ParseSoftRateReset 纯函数版（测试直接钉）。
func ParseSoftRateReset(status int, body string, now time.Time) (time.Time, bool) {
	if status != 429 && !strings.Contains(strings.ToLower(body), "rate limit") {
		return now.Add(softRateFallback), true
	}
	if t, ok := parseTryAgainIn(body, now); ok {
		return t, true
	}
	return now.Add(softRateFallback), true
}

// parseTryAgainIn 抽 "Try again in 1h 2m 30s" 样式（MiMo2API 正则的无依赖版）。
func parseTryAgainIn(body string, now time.Time) (time.Time, bool) {
	lower := strings.ToLower(body)
	i := strings.Index(lower, "try again in ")
	if i < 0 {
		return time.Time{}, false
	}
	rest := lower[i+len("try again in "):]
	var h, m, sec int64
	for _, unit := range []struct {
		ch byte
		to *int64
	}{{'h', &h}, {'m', &m}, {'s', &sec}} {
		k := strings.IndexByte(rest, unit.ch)
		if k <= 0 {
			continue
		}
		var digits strings.Builder
		for _, c := range strings.TrimSpace(rest[:k]) {
			if c < '0' || c > '9' {
				break
			}
			digits.WriteRune(c)
		}
		if digits.Len() == 0 {
			continue
		}
		var n int64
		for _, c := range digits.String() {
			n = n*10 + int64(c-'0')
		}
		*unit.to = n
		rest = rest[k+1:]
	}
	total := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second
	if total <= 0 {
		return time.Time{}, false
	}
	return now.Add(total), true
}

// ── 定时任务 ────────────────────────────────────────────────────────────

// JobRefresh 后台续期任务标识。
const JobRefresh = "mimo-refresh"

// Jobs oauth/free 轨的后台窗口化续期。
//
// 与 trae runRefresh 同构：30 分钟扫描、**只刷临近过期的可刷凭证**（sk 跳过），
// refreshToken 已死 → 预警 + 失败计数（界面可见「需重新登录」，不再静默）。
func (p *Provider) Jobs() []gateway.Job {
	if p.refreshInterval <= 0 {
		return nil
	}
	return []gateway.Job{{
		Name:     JobRefresh,
		Interval: p.refreshInterval,
		Run:      p.runRefresh,
	}}
}

// oauthRefreshSkew oauth 轨的后台主动刷新窗口（读活 secret，不是池投影）。
const oauthRefreshSkew = 15 * time.Minute

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
		if a == nil || !a.Renewable() {
			continue // 纯 sk：无刷可刷（这条 continue 是本包与"每请求全刷"事故的距离）
		}
		if a.TokenDead() {
			log.Printf("mimo: uid=%s refreshToken 已过期（refreshExpiresAt=%d），"+
				"无法自动续期 —— 请重新登录/导入", shortUID(a.UID), a.RefreshExpiresAt)
			if p.onRefreshFailure != nil {
				p.onRefreshFailure(a.UID)
			}
			continue
		}
		if !a.NeedsRefresh(oauthRefreshSkew) {
			continue
		}
		need = append(need, a)
	}
	if len(need) == 0 {
		return nil
	}
	log.Printf("mimo: 后台续期开始，%d 个可刷凭证临近过期（%v 窗口）", len(need), oauthRefreshSkew)
	for _, a := range need {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := p.RefreshCredential(gateway.Credential{Provider: providerID, UID: a.UID, Secret: a}); err != nil {
			log.Printf("mimo: 后台续期失败 uid=%s: %v", shortUID(a.UID), err)
			if p.onRefreshFailure != nil {
				p.onRefreshFailure(a.UID)
			}
			continue
		}
		if p.onRefreshSuccess != nil {
			p.onRefreshSuccess(a.UID)
		}
	}
	return nil
}
