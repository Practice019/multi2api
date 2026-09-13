package workbuddy

// credentialloader.go —— workbuddy 通过 `gateway.CredentialLoader` 自报
// "我的凭证怎么读"。
//
// # 为什么需要（与 codearts 同一个 bug）
//
// 按上游重载 auths 时，核心原来**写死**用 `auth.LoadDirCompat` ——
// 那恰好是 workbuddy 的格式，所以 **workbuddy 自己那条路径"碰巧是对的"**。
//
// 但"碰巧对"不是判据：核心写死 workbuddy 的解析器，等于**核心知道
// workbuddy 的凭证格式**，而 codearts 那条路径就因此彻底坏了
//（实测：重载 codearts 扫到的是 workbuddy 的凭证）。
//
// 所以两个上游都实现本扩展点，核心只按 `ExtOf[CredentialLoader]` 分派、
// **不硬编码任何上游的格式**。这也让"加新上游核心零改动"这条判据重新成立。

import (
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// 编译期断言（与 AuthDirExt 那条并列）。
var _ gateway.CredentialLoader = (*Provider)(nil)

// LoadCredentials 读取 `dir` 下全部 workbuddy 凭证。
//
// `dir` 由核心传入 —— 它是 `AuthDirExt.AuthDir()` 报的那个目录
// （本包返回 `cfg.AuthDir`，可能是空串 → 核心回落默认目录）。
//
// ⚠ 用 `auth.LoadDirCompat(dir, ProviderID)` 而不是 `auth.LoadDir(dir)`：
// 迁移期凭证可能还在**父目录**根下（`auths/workbuddy-*.json` 与
// `auths/workbuddy/workbuddy-*.json` 并存），只读子目录会看到
// "账号池突然空了"。`LoadDirCompat` 子目录优先、按 uid 去重。
//
// 返回投影后的 `gateway.Credential`（只要 uid / nickname）——
// 核心不解释凭证内容。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	if dir == "" {
		dir = p.cfg.AuthDir
	}
	// 兼容读的父目录：`dir` 通常是 `auths/workbuddy`，根是 `auths`。
	base := dir
	if parent := parentDir(dir); parent != "" {
		base = parent
	}
	list, err := auth.LoadDirCompat(base, ProviderID)
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
	return out, nil
}

// parentDir 取父目录；无父目录时返回空串。
//
// 单独写出来是因为 `dir` 可能是相对路径 `auths/workbuddy` ——
// `filepath.Dir("auths/workbuddy")` = `auths`，正是兼容读要的 base。
// 而 `filepath.Dir("workbuddy")` = `.`，那会把 base 退化成当前目录，
// 所以用 `Dir` 之后还要判断它是否等于原值/Dot。
func parentDir(dir string) string {
	p := dir
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			if i == 0 {
				return ""
			}
			return p[:i]
		}
	}
	return ""
}
