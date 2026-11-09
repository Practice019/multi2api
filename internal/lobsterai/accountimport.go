package lobsterai

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// accountimport.go lobsterai 自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么这么短
//
// "导入"恰好是 `ParseCredential` + `MarshalAuthFile` 的复合 ——
// 两个函数本包早已有（`LoadDir` 与续期落盘都在用），判据只有一份。
//
// # 用户粘什么
//
// 发请求与续期真正要用的字段（都是 `Auth` 上已持久化的）：
//
//	access_token     Bearer
//	refresh_token    续期用
//	expires_at       字符串形态（毫秒或秒都接受，见 ExpiresAtMS）
//	uid / user_id    账号主键与续期请求体
//	uuid             安装 UUID —— **续期时必须原样回传**，丢了续期会被拒
//	first/latest_keyfrom  续期请求体要用的时间戳（原值回传，不取当前时刻）
//
// ⚠ `uuid` 与两个 `keyfrom` 是这份凭证里最容易漏的字段（它们不在上游的
// 响应里、而是客户端侧生成的）。`ParseCredential` 已负责读全，
// 这里只要不去动它就够 —— 这正是"复用解析器、不手写映射"的价值。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘贴的 JSON 转成可落盘的 lobsterai 凭证。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	// T-330：分割 / 汇总 / 逐条失败语义都在 gateway.ImportCredentialsGeneric 里；
	// 本上游只提供「怎么把一条粘贴内容解析成凭证」这一件事。
	return gateway.ImportCredentialsGeneric(pasted, func(item string) (gateway.ImportedCredential, error) {
		a, perr := ParseCredential([]byte(item))
		if perr != nil {
			return gateway.ImportedCredential{}, perr
		}
		uid := strings.TrimSpace(a.UID)
		if uid == "" {
			return gateway.ImportedCredential{}, errNoImportUID
		}
		b, merr := MarshalAuthFile(a)
		if merr != nil {
			return gateway.ImportedCredential{}, merr
		}
		return gateway.ImportedCredential{
			FileName: FileName(a),
			Raw:      b,
			UID:      uid,
			Nickname: a.Nickname,
		}, nil
	})
}

// errNoImportUID 解析出了凭证，但拿不到账号主键。
var errNoImportUID = errors.New("lobsterai: 无法确定账号标识（uid 为空且无法从 token 派生）")
