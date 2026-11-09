package codar

// accountimport.go —— 手工导入凭证（T-322）。
//
// # 为什么需要它（用户原话：不希望依赖任何外部应用，必须自包含）
//
// Codar 的**首次**令牌必须有来源。四条路的取舍：
//
//	① 读桌面端 ~/.codar/config.json —— **依赖外部应用**，用户明确否掉
//	② 手机+短信登录 —— 需要极验类人机验证（4 个字段由前端 JS 采集，后端强校验）⇒ 代码层过不去
//	③ 微信扫码登录 —— 已实测可用（T-322：POST /v1/wechat/login/qrcode 返回 scene+qr_url，
//	   发码**不需要** captcha）⇒ 是最理想的自动化入口，待实现 LoginFlow
//	④ ★ 手工粘贴 —— 零依赖，人只操作一次；与 codearts 的 ManualLogin 同构
//
// 本文件实现 ④ 作为**保底**：只要有一段合法 JWT（或整个配置 JSON），就能把账号加进来。
//
// # 支持的粘贴形态（都实测过来源）
//
//	· 裸 JWT：eyJhbGciOiJIUzI1…            （Authorization: Bearer 后面那一长串）
//	· 桌面端 config.json 全文               （provider.speedai.token —— 兼容老路径）
//	· 本仓自己的落盘格式                    （{"auth":{...},"account":{...}}，可直接再导入）
//	· 多段（每行一个 / JSON 数组）          —— 交给 core 的分割器处理

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"workbuddy2api/internal/gateway"
)

var errNoImportToken = errors.New("codar: 没有可导入的令牌（需要 JWT 或含 token 的 JSON）")

// ParseCredential 把一段文本解析成 *Auth。
//
// 依次尝试：本仓落盘格式 → 桌面端 config → 纯 JWT。
// 解析不出令牌就报错（**不**返回半成品：半成品进池会变成幽灵号）。
func ParseCredential(raw []byte) (*Auth, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return nil, errNoImportToken
	}

	// ① 本仓格式（或任何带 auth.token 的 JSON）
	var sf storedFile
	if err := json.Unmarshal([]byte(s), &sf); err == nil {
		if a := sf.normalize(); a.Token != "" {
			if a.ExpiresAt == 0 {
				a.ExpiresAt = jwtExpiry(a.Token)
			}
			a.fillUID()
			return a, nil
		}
	}

	// ② 桌面端 config（provider.speedai.token）
	if strings.Contains(s, "provider") {
		var dc desktopConfig
		if err := json.Unmarshal([]byte(s), &dc); err == nil {
			tok, user := "", ""
			if dc.Provider.Speedai != nil {
				tok, user = dc.Provider.Speedai.Token, dc.Provider.Speedai.Username
			}
			if tok == "" {
				for _, p := range dc.Provider.Profiles {
					if p.Speedai != nil && p.Speedai.Token != "" {
						tok, user = p.Speedai.Token, p.Speedai.Username
						break
					}
				}
			}
			if tok != "" {
				a := &Auth{Token: tok, Username: user, ExpiresAt: jwtExpiry(tok)}
				a.fillUID()
				return a, nil
			}
		}
	}

	// ③ 裸 JWT（三段点分 base64url）
	if strings.Count(s, ".") == 2 && strings.HasPrefix(s, "eyJ") && !strings.ContainsAny(s, " \n\t{}\"") {
		a := &Auth{Token: s, ExpiresAt: jwtExpiry(s)}
		if c, ok := jwtClaims(s); ok {
			if u, _ := c["username"].(string); u != "" {
				a.Username = u
			}
		}
		a.fillUID()
		return a, nil
	}

	return nil, errNoImportToken
}

// ImportCredentials 实现 gateway.AccountImportExt：把粘贴的文本变成可落盘的凭证。
//
// 与 codearts（internal/codearts/accountimport.go:43）同构：
//
//	core 的分割器切多段 → 逐段解析 → 每段组装 {FileName, Raw, UID, Nickname} 交给 core 落盘。
//
// ⚠ 文件名沿用本仓惯例 `codar-<uid>.json`（与 LoadDir 的前缀约束一致 —— 前缀不匹配就永远不会被读回来）。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	// ★ 顺序很重要（T-322 实测）：**先整体试解析**。
	// 裸 JWT 不是 JSON，而 core 的分割器要求合法 JSON 对象 ⇒ 先分割会直接把它拒掉。
	//
	// T-331：逐条循环改用 gateway.MapImportItems（它只负责"逐条 + 失败即整体失败"），
	// 分割前的这一步与"单段回退"都留在本上游 —— 它们是 codar 特有的语义。
	whole := strings.TrimSpace(pasted)
	items := []string{}
	if _, err := ParseCredential([]byte(whole)); err == nil {
		items = []string{whole}
	} else {
		split, err := gateway.SplitAccountImportItems(pasted)
		if err != nil {
			return nil, err
		}
		items = split
	}
	if len(items) == 0 {
		items = []string{pasted}
	}
	// idx 让报错里的段号与原来逐字一致（MapImportItems 按顺序逐条调用）。
	idx := 0
	return gateway.MapImportItems(items, func(item string) (gateway.ImportedCredential, error) {
		idx++
		a, perr := ParseCredential([]byte(item))
		if perr != nil {
			if a2, e2 := ParseCredential([]byte(whole)); e2 == nil && len(items) == 1 {
				a = a2
			} else {
				return gateway.ImportedCredential{}, fmt.Errorf("codar: 第 %d 段解析失败: %w", idx, perr)
			}
		}
		if a == nil || a.Token == "" {
			return gateway.ImportedCredential{}, errNoImportToken
		}
		a.fillUID()
		if a.ExpiresAt == 0 {
			a.ExpiresAt = jwtExpiry(a.Token)
		}
		if a.Nickname == "" {
			a.Nickname = a.UID
		}
		blob, merr := json.MarshalIndent(map[string]any{
			"auth":    map[string]any{"token": a.Token, "username": a.Username, "expiresAt": a.ExpiresAt},
			"account": map[string]any{"uid": a.UID, "nickname": a.Nickname},
		}, "", "  ")
		if merr != nil {
			return gateway.ImportedCredential{}, merr
		}
		return gateway.ImportedCredential{
			FileName: "codar-" + safeName(a.UID) + ".json",
			Raw:      blob,
			UID:      a.UID,
			Nickname: a.Nickname,
		}, nil
	})
}
