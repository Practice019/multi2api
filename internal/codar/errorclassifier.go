package codar

import (
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言（与 provider.go 的断言分开写：这条保证"扩展点确实注册得上去"）。
var (
	_ gateway.Provider            = (*Provider)(nil)
	_ gateway.AuthDirExt          = (*Provider)(nil)
	_ gateway.ErrorClassifier     = (*Provider)(nil)
	_ gateway.RefreshSkewExt      = (*Provider)(nil)
	_ gateway.CredentialExpiryExt = (*Provider)(nil)
)

// 本上游的错误特征串（全部来自实测/源码证据，不猜）。
const (
	markerAccountRestricted = "account_restricted"    // T-310：codar2api 据此立即停派发
	markerForbiddenPage     = "403 forbidden"         // T-310：nginx 拦截页（**出口 IP** 被封）
	markerNotAuthenticated  = "not authenticated"     // 401 的常见文案
	markerMissingModel      = "missing 'model' field" // T-311 实测的 400
	markerInvalidRequest    = "invalid_request_error" // T-311 实测的 error.type
	markerQuota             = "quota"                 // 额度/积分不足（HardCredit 方向）
)

// Classify 把 Codar 的错误映射到本仓的中立词汇。
//
// # 为什么必须区分「出口 IP 被封」与「账号被封」（T-310/T-311 的证据）
//
// 上游对两种拒绝的文案与后果完全不同：
//
//	· nginx 403 拦截页（不含业务 JSON）⇒ **是出口 IP 的问题**，同一个账号换条出口就能通。
//	  若误判成账号问题 → 会把好账号一个个禁用掉（codar2api 专门为它做了"标记出口 IP + 换出口重试"）。
//	· `account_restricted`（业务 JSON）⇒ **是这个账号**被平台限制，换出口也没用，必须人工处理。
//
// 因此：IP 封控 ⇒ ErrKindSoftRate（**短冷却、不累计账号错误**）；
//
//	账号受限 ⇒ ErrKindSessionDead（**永久禁用该号，等人工重新登录/解封**）。
//
// 状态码兜底走 gateway.DefaultErrorKind（保守、只看状态码）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	low := strings.ToLower(body)
	hasJSON := strings.Contains(low, "{") && strings.Contains(low, "\"error\"")

	switch {
	case status == 403 && !hasJSON:
		// nginx/边缘的 403 页面：出口 IP 被封（不是账号）
		if strings.Contains(low, markerForbiddenPage) || strings.Contains(low, "nginx") || strings.TrimSpace(body) == "" {
			return gateway.ErrKindSoftRate
		}
		return gateway.ErrKindServer
	case status == 403 && strings.Contains(low, markerAccountRestricted):
		// 账号级限制：人工介入
		return gateway.ErrKindSessionDead
	case status == 403:
		// 有 JSON 但没有明确标记：按"账号侧拒绝"处理，但不永久禁用（保守）
		return gateway.ErrKindAuth
	case status == 401 && (strings.Contains(low, markerNotAuthenticated) || body == "" || hasJSON):
		// token 不再被接受 ⇒ 需重新登录（重新导入 token 才能恢复）
		return gateway.ErrKindSessionDead
	case status == 429:
		return gateway.ErrKindSoftRate
	case status == 402, status == 400 && strings.Contains(low, markerQuota):
		return gateway.ErrKindHardCredit
	case status == 400 && (strings.Contains(low, markerMissingModel) || strings.Contains(low, markerInvalidRequest)):
		// 确定性客户端错误：换号也不会成功 ⇒ 不换号、直接把这个错误交给调用方
		return gateway.ErrKindFatalClient
	case status == 404:
		return gateway.ErrKindNotFound
	case status >= 500:
		return gateway.ErrKindServer
	}
	return gateway.DefaultErrorKind(status, body)
}

// RefreshSkew 本上游的提前续期窗口。
//
// # 为什么能从 JWT 算
//
// token 是 JWT（T-311 实测 207 字符、含 exp）⇒ 窗口可以按**寿命比例**给：
//
//	有 iat+exp ⇒ 用 gateway.RefreshSkewAtRatio（与全仓"寿命过半"口径一致）
//	只有 exp，且剩余寿命已知 ⇒ 取 min(30min, 剩余/2)
//	解不出来（非 JWT / 无 exp）⇒ ok=false，让核心回落它自己的默认窗口
//
// ⚠ 不返回"永远该刷"（那会烧掉 token）；也不返回 0；
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	a, _ := cred.Secret.(*Auth) // 类型不对时 a=nil，下面的分支会回落 ok=false
	if a == nil || a.Token == "" {
		return 0, false
	}
	c, ok := jwtClaims(a.Token)
	if !ok {
		return 0, false
	}
	exp, _ := c["exp"].(float64)
	if exp <= 0 {
		return 0, false
	}
	if iat, ok2 := c["iat"].(float64); ok2 && iat > 0 && int64(exp) > int64(iat) {
		if skew, ok3 := gateway.RefreshSkewAtRatio(gateway.JWTTimes{
			IssuedAt:  int64(iat),
			ExpiresAt: int64(exp),
		}, gateway.DefaultRefreshRatio); ok3 {
			return skew, true
		}
	}
	remaining := time.Until(time.Unix(int64(exp), 0))
	if remaining <= 0 {
		return 0, false
	}
	half := remaining / 2
	const cap30 = 30 * time.Minute
	if half > cap30 {
		half = cap30
	}
	return half, true
}

// TokenExpiry 实现 gateway.CredentialExpiryExt：把 JWT 的 exp 报给核心（Unix 秒）。
//
// 报它可以省掉"过期了才知道"的一整轮请求（界面上直接显示到期时间）。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, _ := cred.Secret.(*Auth)
	if a == nil || a.Token == "" {
		return 0, false
	}
	if a.ExpiresAt > 0 {
		return a.ExpiresAt, true
	}
	if exp := jwtExpiry(a.Token); exp > 0 {
		return exp, true
	}
	return 0, false
}

// LoadCredentials 实现 gateway.CredentialLoader：把本包目录里的凭证投影成核心能用的形态。
//
// ⚠ 与 LoadCredentialsWithSecrets 必须**同源**（本仓铁律）；这里两者都走 LoadDir。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.Credential{
			Provider: providerID,
			UID:      a.UID,
			Nickname: a.Nickname,
			FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 同上，但把 Secret 一起给（池子要它才能发请求）。
//
// # ⚠⚠ 返回类型必须是 `[]gateway.CredentialSecret`（本轮修的真实 bug）
//
// 本方法此前写成 `([]gateway.Credential, error)` —— 而
// `gateway.CredentialSecretLoader` 要求的是 `([]gateway.CredentialSecret, error)`。
// Go 的接口断言（`ExtOf[CredentialSecretLoader]`）按**方法签名全等**匹配，
// 返回值类型不同 ⇒ 断言失败 ⇒ 装配层以为"这个上游没有 secret 版扫描器" ⇒
// 退回**裸投影**路径（`SyncToDirFor`）⇒ 池子里只有 uid/nickname、**没有 secret**。
//
// 用户可见症状（2026-11-09 实测）：批量导入 codar 两个号 ⇒ 账号列表里能看到、
// 但 `has_token=false`、额度空、`success_count=0`、请求必失败；
// 而管理端点 `/admin/codar/quota` 能正常查到余额（证明凭证文件本身是好的，
// 只是**没进池子的 secret 通道**）。
//
// 这类"签名写错但编译通过"的错，只有接口断言在运行期能发现 —— 所以
// 契约测试之外，`internal/codar/credentialsecret_test.go` 里额外加了一条
// **静态断言**（`var _ gateway.CredentialSecretLoader = (*Provider)(nil)`），
// 让它在编译期就红。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	list, err := LoadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: providerID,
				UID:      a.UID,
				Nickname: a.Nickname,
				FilePath: a.FilePath,
			},
			// 不透明通道：核心只搬不读。
			//
			// ⚠ ExpiresAt **不**放投影里：投影是启动快照，活 secret 续期后
			//   投影不会跟新（与 cmd/server/codarcreds.go 的注释同一条纪律）。
			//   过期权威只走 CredentialExpiryExt。
			Secret: a,
		})
	}
	return out, nil
}
