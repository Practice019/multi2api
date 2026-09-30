package loomy

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// errNoImportUID 解析出了凭证，但拿不到账号主键（uid）。
//
// 单独一个值是为了让调用方能判"这条不可落盘"而不是"整个请求非法" ——
// 没有 uid 的凭证放进池子会变成一个无法按 uid 寻址的幽灵号。
var errNoImportUID = errors.New("loomy: 无法确定账号标识（uid 为空）")

// accountimport.go loomy 自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么这么短
//
// "导入"恰好是 `ParseCredential` + `MarshalAuthFile` 的**复合** ——
// 两个函数本包早已有（`LoadDir` 与落盘路径都在用），且判据只有一份：
//
//	ParseCredential   把磁盘那段 JSON 解成 *Auth
//	MarshalAuthFile   把 *Auth 序列化成磁盘那段 JSON
//
// 所以这里只做"逐条搬运"，不重新写字段映射 —— 手写映射是
// "看起来成功、实际字段错位"的高发区。
//
// ⚠ 既有的 `handleImport`（HTTP 端点）**保留不动**：它走的是
// `importItem` 那套更宽松的字段别名（`userId`/`userid`/`uid` 都认），
// 而那条路径已被前端与测试依赖。本文件只补上**通用入口**要的那一步，
// 两条路最终都落到 `MarshalAuthFile`，落盘形态一致。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘贴的 JSON 转成可落盘的 loomy 凭证。
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
		if strings.TrimSpace(a.UID) == "" {
			return nil, errNoImportUID
		}
		b, merr := MarshalAuthFile(a)
		if merr != nil {
			return nil, merr
		}
		out = append(out, gateway.ImportedCredential{
			FileName: FileName(a),
			Raw:      b,
			UID:      a.UID,
			Nickname: a.Nickname,
		})
	}
	return out, nil
}
