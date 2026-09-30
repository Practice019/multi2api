package trae

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// accountimport.go trae 自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么这么短
//
// "导入"恰好是 `Parse` + `MarshalAuthFile` 的复合 ——
// 两个函数本包早已有（`LoadDir` 与续期落盘都在用），判据只有一份。
//
// ⚠ 本包的解析器叫 `Parse`（不是其它上游的 `ParseCredential`）——
// 调用时按本包的名字写，不要去"统一命名"（那是与本任务无关的重构）。
//
// # 用户粘什么
//
// 发请求与续期真正要用的字段（都是 `Auth` 上已持久化的）：
//
//	AccessToken       Cloud-IDE-JWT 头
//	RefreshToken      每次 ExchangeToken 轮换（**消费型**）
//	ExpiresAt         accessToken 过期时刻（Unix 秒）
//	RefreshExpiresAt  refreshToken 自身过期时刻（0 = 未知）
//	ClientID          **必须与签发这份 refreshToken 的那个一致**
//	Domain / ApiHost  "trae.cn" / "https://api.trae.com.cn"
//	MachineID / DeviceID  x-machine-id / x-device-id
//
// ⚠ `ClientID` 是最容易漏的一个：硬编码一个值去刷所有账号，对"从桌面客户端
// 导入"的账号会刷新失败 → 到期 → 账号表现为"突然过期"。`Parse` 已负责读它，
// 这里只要不动它就够 —— 这正是"复用解析器、不手写映射"的价值。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘贴的 JSON 转成可落盘的 trae 凭证。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	items, err := gateway.SplitAccountImportItems(pasted)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.ImportedCredential, 0, len(items))
	for _, raw := range items {
		a, perr := Parse([]byte(raw))
		if perr != nil {
			return nil, perr
		}
		uid := strings.TrimSpace(a.UID)
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
var errNoImportUID = errors.New("trae: 无法确定账号标识（uid 为空）")
