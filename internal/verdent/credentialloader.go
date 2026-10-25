// credentialloader.go 扩展点：CredentialLoader / CredentialSecretLoader。
//
// # 为什么必须有（这是全仓共同踩过的坑）
//
// 不实现时「重载 auths」对这个上游**静默无效**：接口会回落到核心的默认解析器
// （那是 workbuddy 的），于是返回 200 且带着**另一个上游**的目录与条数 ——
// 看起来"重载成功"，实际一个 verdent 账号都没扫到。
// 这是最坏的一类失败：成功得可疑，而不是失败得刺眼。
//
// # 两个必须同源
//
// CredentialLoader 与 CredentialSecretLoader 必须解析同一批文件、给出同一批 uid。
// 分叉的后果（本仓踩过）：池里多出一个"没有 secret"的账号 → 选中它时取不到 Auth
// → 对话必然失败。所以下面两个实现共用同一个 LoadDir 与 UID()。
package verdent

import (
	"workbuddy2api/internal/gateway"
)

// LoadCredentials 扫描目录，返回「核心投影」（不含 secret）。
//
// 目录由 AuthDir() 提供 —— 读与写必须问同一个目录。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	if dir == "" {
		dir = p.AuthDir()
	}
	list, err := LoadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.Credential{
			Provider: ProviderID,
			UID:      a.UID(),
			Nickname: a.DisplayName(),
			// ★ 这一条必须填：JetBrains 风格的 JWT 是**有明确 exp** 的，
			// 与 monkeycode 那种"30 天硬过期但无声明"不同 —— 界面上
			// 「Token」列靠它显示过期状态。不填的话那一列永远是「不提供」。
			ExpiresAt: a.TokenExpiredAt(),
			FilePath:  a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与上面同源，但把活 secret 一起给出。
//
// 没有它，池里那些账号"没有凭证" —— 被选中时取不到 token，对话必然 401。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	if dir == "" {
		dir = p.AuthDir()
	}
	list, err := LoadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider:  ProviderID,
				UID:       a.UID(),
				Nickname:  a.DisplayName(),
				ExpiresAt: a.TokenExpiredAt(),
				FilePath:  a.FilePath,
			},
			Secret: a,
		})
	}
	return out, nil
}
