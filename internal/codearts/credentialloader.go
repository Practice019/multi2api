package codearts

// credentialloader.go —— 让 codearts 通过 `gateway.CredentialLoader` 自报
// "我的凭证怎么读"，核心按上游重载 auths 时就不会再用错解析器。
//
// # 为什么需要（实测踩出来的真 bug）
//
// 核心原来用 `auth.LoadDirCompat` 扫描 —— 那是 **workbuddy 的解析器**，
// Glob 前缀写死 `workbuddy*.json`。拿去扫 codearts 目录 → 一个都读不到，
// 反而把 `auths/` 根下遗留的 workbuddy 旧文件当成了结果。
//
// 实测：`LoadDirCompat("auths", "codearts")` 返回 3 条，全是 workbuddy 的；
// 而 `auths/codearts/` 里那个真的 codearts 凭证被完全忽略。
// 随后核心还对 codearts 域 `SyncToDirFor` 了那 3 个 workbuddy 账号 —— 跨域污染。

import (
	"log"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：Provider 满足新扩展点。
var _ gateway.CredentialLoader = (*Provider)(nil)

// LoadCredentials 读取 `dir` 下全部 codearts 凭证。
//
// 复用包内 `LoadDir`（前缀 `codearts*.json`），把 `*Auth` 投影成
// `gateway.Credential` —— 核心只要 **uid / nickname**（账号池主键与展示名），
// **不解释凭证内容**。
//
// 为什么返回 `[]gateway.Credential` 而不是 `[]*Auth`：
// 核心不该知道 `codearts.Auth` 这个类型（那是本包的事实）。
// `Secret` 留空 —— 按上游重载的落盘路径由 `syncCodeartsAccounts` 负责
// （它需要完整的 `*Auth` 做 secret），这里只要投影后的身份。
//
// 单个文件解析失败由 `LoadDir` 跳过并记日志（S1 的修复），
// **不是错误**；目录为空也不是错误。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		if a == nil || a.UID == "" {
			continue
		}
		out = append(out, gateway.Credential{
			Provider: ProviderID,
			UID:      a.UID,
			Nickname: a.Nickname,
			FilePath: a.FilePath,
		})
	}
	log.Printf("codearts: 从 %s 读到 %d 个凭证（LoadCredentials）", dir, len(out))
	return out, nil
}

// 编译期断言：Provider 也满足"带 secret"的可选加强版（评审 R2）。
var _ gateway.CredentialSecretLoader = (*Provider)(nil)

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret。
//
// # 为什么必须优先走注入的访问器（p.accounts）
//
// 装配层把 `p.accounts` 接到凭证 **store** 上，而 store 里那个 `*Auth`
// 就是池 secret / 后台任务 / 管理端点共享的**同一个对象**。
//
// 这里若自己 `LoadDir` 造一份新的，就会把 007 修掉的那个 503 根因重新引入：
//
//	对象级 refreshMu 跨对象无效 → 一次性 refresh_token 被消费两次
//	→ 池里那份永远停在已作废的旧值上 → no_healthy_account
//
// 所以"secret 从哪来"必须由**上游自己**回答，而且答案必须是 store。
// 只有没注入访问器时（单测、未接线的装配）才回落 LoadDir ——
// 那种形态下不存在第二个所有者。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	var list []*Auth
	var err error
	if p.accounts != nil {
		// ⚠ 这条分支下 `dir` 参数**被忽略**：权威目录是 store 自己的那个
		//（它由装配时的 cfg.CodeartsAuthDir 决定）。两者在正常装配下是同一个值，
		// 但日志必须说清真正用了哪个 —— 否则"我点了重载但目录不对"会变成
		// 一条查不出的线索。所以这里单独打一行，而不是复用下面那句。
		list = p.accounts()
		log.Printf("codearts: store 给出 %d 个凭证（LoadCredentialsWithSecrets，含 secret；"+
			"传入的 dir=%s 被 store 自己的目录覆盖）", len(list), dir)
		return buildCredentialSecrets(list), nil
	}
	list, err = LoadDir(dir)
	if err != nil {
		return nil, err
	}
	log.Printf("codearts: 从 %s 读到 %d 个凭证（LoadCredentialsWithSecrets，含 secret）", dir, len(list))
	return buildCredentialSecrets(list), nil
}

// buildCredentialSecrets 把 *Auth 投影成「身份 + 不透明 secret」。
//
// 抽出来只为让上面两条分支共用同一套投影规则 —— 规则若有两份实现，
// 迟早会出现"store 那条多带/少带一个字段"的分叉。
func buildCredentialSecrets(list []*Auth) []gateway.CredentialSecret {
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		if a == nil || a.UID == "" {
			continue
		}
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{Provider: ProviderID, UID: a.UID, Nickname: a.Nickname, FilePath: a.FilePath},
			// 不透明值：核心只搬运，不解释。
			Secret: a,
		})
	}
	return out
}
