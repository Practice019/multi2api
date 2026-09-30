package raccoon

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// accountimport.go raccoon 自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么这么短
//
// "导入"恰好是 `ParseCredential` + `MarshalAuthFile` 的复合 ——
// 两个函数本包早已有（`LoadDir` 与续期落盘都在用），判据只有一份。
//
// # 用户粘什么
//
// `ParseCredential` 认嵌套形与扁平形，发请求真正要用的字段是：
//
//	access_token     JWT（Bearer）
//	refresh_token    续期用
//	expires_at       毫秒时间戳的**字符串**（可空 → 回落解 JWT exp）
//	office_identity  X-Org-Code 头（`personal` 或组织码；空串也照发）
//	device_id        X-Client-Device-ID（32 位 hex）
//
// 所以用户把 `raccoon-*.json` 的内容整段粘进来即可。
//
// ⚠ 与本上游的**扫码登录**是两条独立入口：扫码产出新凭证，
// 导入是"用户手上已经有一份"。两者最终走同一个 `MarshalAuthFile`，
// 落盘形态一致，下游不必区分。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘贴的 JSON 转成可落盘的 raccoon 凭证。
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
var errNoImportUID = errors.New("raccoon: 无法确定账号标识（user_id 与 access_token 都取不到）")
