package cline

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// accountimport.go cline 自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么这么短
//
// "导入"恰好是 `ParseCredential` + `MarshalAuthFile` 的复合 ——
// 两个函数本包早已有（`LoadDir` 与续期落盘都在用），判据只有一份。
// 这里只做逐条搬运，不重写字段映射（手写映射是"看起来成功、实际字段错位"
// 的高发区）。
//
// # 用户粘什么
//
// `ParseCredential` 认两种形状（嵌套形与本包落盘形），而**发请求真正要用的**
// 就是它解析出来的那些字段：
//
//	access_token   （**必须带 `workos:` 前缀**，剥掉即 401）
//	refresh_token  续期用（空 = 不可续期）
//	account_id     余额端点必须用它（usr-… 形态，不是 JWT 的 sub）
//	expire_time    毫秒时间戳（可空，空则回落到解 JWT exp）
//
// 所以用户把 `cline-*.json` 的内容整段粘进来即可。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘贴的 JSON 转成可落盘的 cline 凭证。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	items, err := gateway.SplitAccountImportItems(pasted)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.ImportedCredential, 0, len(items))
	for _, raw := range items {
		a, perr := ParseCredential([]byte(raw))
		if perr != nil {
			return nil, perr
		}
		uid := strings.TrimSpace(a.UID())
		if uid == "" {
			return nil, errNoImportUID
		}
		b, merr := MarshalAuthFile(a)
		if merr != nil {
			return nil, merr
		}
		out = append(out, gateway.ImportedCredential{
			FileName: FileName(a),
			Raw:      b,
			UID:      uid,
			Nickname: a.Nickname,
		})
	}
	return out, nil
}

// errNoImportUID 解析出了凭证，但拿不到账号主键。
var errNoImportUID = errors.New("cline: 无法确定账号标识（access_token 与 account_id 都取不到）")
