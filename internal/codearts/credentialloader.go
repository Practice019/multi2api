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
		})
	}
	log.Printf("codearts: 从 %s 读到 %d 个凭证（LoadCredentials）", dir, len(out))
	return out, nil
}
