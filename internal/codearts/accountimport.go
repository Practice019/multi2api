package codearts

import (
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// errNoImportUID 解析出了凭证，但拿不到账号主键（uid）。
//
// 单独一个值是为了让调用方能判"这条不可落盘"而不是"整个请求非法" ——
// 没有 uid 的凭证放进池子会变成一个无法按 uid 寻址的幽灵号。
var errNoImportUID = errors.New("codearts: 无法确定账号标识（uid 为空）")

// accountimport.go codearts 自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 与本包既有两步的关系
//
//	ParseCredential  已有的：把磁盘那段 JSON 解成 *Auth（嵌套形与扁平形都认）
//	MarshalNested    本轮从 SaveAtomic 里抽出来的：把 *Auth 编回磁盘形态
//
// 导入就是这两步的复合。抽 `MarshalNested` 而不是让导入自己拼 `credFile`：
// 后者是第二份序列化，迟早与本包的落盘路径漂移。
//
// # ⚠ cmd/login 产出的那种凭证直接就能用
//
// 用户从 `cmd/login` 或别处拿到的 codearts 凭证文件，**逐字节**就是
// 本包认的嵌套形 → 粘进来即可导入。这也是"导入"最好的形态：
// 用户手上有什么就粘什么，不需要理解字段。
//
// # ⚠ DPoP 私钥必须一起在
//
// codearts 的续期凭证**不是一个 token**，而是「refresh_token + ES256 私钥」
// 一对。缺私钥时 `RefreshToken` 会直接失败（见 credential.go 的
// HasUsableDPoPKey）。所以粘贴的内容若只有 AK/SK 而没有 dpop 段，
// 那份凭证只能用到 STS 过期（约 2 小时）—— 这里**如实放行**但由界面的
// "凭证续期连续失败"提示暴露，而不是在导入时就拒绝（手工建的凭证
// 从来也是这个形态，导入不该比它更严）。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘贴的 JSON 转成可落盘的 codearts 凭证。
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
		b, merr := a.MarshalNested()
		if merr != nil {
			return nil, merr
		}
		out = append(out, gateway.ImportedCredential{
			// 文件名与 cmd/login 及 core 的落盘惯例一致：`codearts-<uid>.json`。
			FileName: "codearts-" + sanitizeFileName(a.UID) + ".json",
			Raw:      b,
			UID:      a.UID,
			Nickname: a.Nickname,
		})
	}
	return out, nil
}

// sanitizeFileName 把不适合做文件名的字符换掉。
//
// uid 主要来自 AK 或 JWT 的 account_id（都是安全字符），但"手抄一份"
// 的输入不可控 —— 让它带 `/` 进文件名会写到别的目录。
func sanitizeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		default:
			return '_'
		}
	}, s)
}
