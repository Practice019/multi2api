// credential.go 的扩展点部分：CredentialLoader / CredentialSecretLoader。
//
// # 为什么必须有（实测踩到）
//
// 不实现时「重载 auths」对这个上游**静默无效**：
//
//	POST /admin/accounts/reload {"provider":"monkeycode"}
//	→ {"provider":"workbuddy","dir":"auths\workbuddy","scanned":6}
//
// 返回 200 且带着**另一个上游**的目录与条数 —— 看起来"重载成功"，
// 实际一个 monkeycode 账号都没扫到。这是最坏的一类失败：
// 成功得可疑，而不是失败得刺眼。
//
// # 两个必须同源
//
// `CredentialLoader` 与 `CredentialSecretLoader` 必须解析同一批文件、
// 给出同一批 uid。分叉的后果是本仓踩过的：
//
//	池里多出一个"没有 secret"的账号 → 选中它时取不到 Auth → 必失败
//
// 所以下面两个实现共用 `LoadDir` 与 `UID()`。
package monkeycode

import (
	"workbuddy2api/internal/gateway"
)

// 编译期断言。
var _ gateway.CredentialLoader = (*Provider)(nil)
var _ gateway.CredentialSecretLoader = (*Provider)(nil)

// LoadCredentials 扫描目录，返回「核心投影」（不含 secret）。
//
// 目录由 AuthDir() 提供 —— 读与写必须问同一个目录。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.Credential{
			Provider: providerID,
			UID:      a.UID(),
			Nickname: a.DisplayName(),
			// ⚠ 刻意**不填** ExpiresAt：投影是启动快照，
//			// 而 session 有 30 天硬过期且无续期接口 —— 抄进来只会是个过期的假值。
			FilePath: a.FilePath,
		})
	}
	return out, nil
}
// LoadCredentialsWithSecrets 与上面同源，但把活 secret 一起给出。
//
// 没有它，池里那些账号"没有凭证" —— 被选中时取不到 api_key/signing_secret，
// 对话必然 401。
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
				UID:      a.UID(),
				Nickname: a.DisplayName(),
				FilePath: a.FilePath,
			},
			Secret: a,
		})
	}
	return out, nil
}
