package qoder

import (
	"encoding/json"
	"errors"
	"strings"

	"workbuddy2api/internal/gateway"
)

// accountimport.go qoder / qodercn 自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么这么短
//
// "导入"恰好是 `ParseCredential` + `MarshalAuthFile` 的复合 ——
// 两个函数本包早已有（`LoadDirFor` 与续期落盘都在用），判据只有一份。
//
// # 用户粘什么
//
// 发请求与续期真正要用的字段：
//
//	access_token    `dt-` 前缀的不透明串（**不是 JWT**，解不出 exp）
//	refresh_token   `drt-` 前缀
//	expires_at      绝对毫秒时刻 —— 「Token」列与"要不要续期"的**唯一**权威
//	machine_id      续期请求体必须带它
//	product_id      **决定这份凭证归哪个实例**（见下）
//
// # ⚠ 两个实例共用一个凭证目录，所以 product_id 是**必需**的
//
// qoder 与 qodercn 都把凭证写进同一目录（靠文件里的 `product_id` 区分，
// 见 Config.AuthDir 的注释）。导入时若不校验它：
//
//	qodercn 实例导入一份 product_id=qoder 的凭证
//	  → 那份凭证会被写进 CN 的目录、但池子里标成 CN
//	  → 之后它用 CN 的端点发国际版的 token → 401
//
// 所以这里**按实例过滤**：不是本实例产品的凭证直接拒绝，并说清原因。
// 这与 `LoadDirFor` 的判据同源（那边也是按 productID 过滤）。
var _ gateway.AccountImportExt = (*Provider)(nil)

// ImportCredentials 把用户粘贴的 JSON 转成可落盘的 qoder 凭证。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	items, err := gateway.SplitAccountImportItems(pasted)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.ImportedCredential, 0, len(items))
	for _, raw := range items {
		// ⚠⚠ 必须在 ParseCredential **之前**读出原文里的 product_id。
		//
		// # 为什么不能直接看 ParseCredential 的结果（实测的缺陷）
		//
		// `ParseCredential` 内部会调 `normalize()`，而那里有一句
		//
		//	if a.ProductID == "" { a.ProductID = providerID }
		//
		// `providerID` 是**包级常量 "qoder"**（国际版）—— 它拿不到实例的产品。
		// 于是"没写 product_id"被解析器**提前填成了国际版**，我下面那条
		// "空则归本实例"的分支永远走不到：
		//
		//	国际版实例：空 → 填 "qoder" → 恰好等于本实例 → 放行（侥幸对）
		//	中国版实例：空 → 填 "qoder" → ≠ "qodercn" → **被拒**
		//
		// 而用户看到的是「该凭证属于 qoder，不能在 qodercn 上导入」——
		// 他根本没写 qoder、也没有国际版的号。那条错误把所有归因
		// 都指向了错误的方向（这正是本项目最忌讳的：失败要指向真相）。
		//
		// 所以判据取**原文**（用户真正写下的东西），而不是解析器的兜底值。
		explicit := explicitProductID(raw)

		a, perr := ParseCredential([]byte(raw))
		if perr != nil {
			return nil, perr
		}
		// 显式写了别的产品才拒；没写（或写成本实例）一律归本实例 ——
		// 用户既然在 qodercn 的卡片上点了导入，意图就是 qodercn。
		if explicit != "" && explicit != p.productID {
			return nil, errWrongProduct(p.productID, explicit)
		}
		a.ProductID = p.productID

		uid := strings.TrimSpace(a.UIDValue())
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

// explicitProductID 从**用户粘贴的原文**里读出 product_id（没写返回空串）。
//
// 认两种位置：嵌套形的 `auth.product_id`（本包落盘用的）与顶层的
// `product_id`（手工抄的凭证更可能这么写）。
//
// ⚠ 只读这一个字段、不做完整解析：目的是"拿到用户的原始意图"，
// 而不是替代 `ParseCredential`（凭证的其余字段仍由它负责，
// 那样校验与落盘才有单一权威）。
func explicitProductID(raw string) string {
	var doc struct {
		ProductID string `json:"product_id"`
		Auth      struct {
			ProductID string `json:"product_id"`
		} `json:"auth"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmpty(doc.Auth.ProductID, doc.ProductID))
}

// errWrongProduct 这份凭证属于另一个产品（qoder ↔ qodercn）。
//
// # 为什么必须**明确拒绝**而不是"顺手改掉 product_id"
//
// 两个产品的 authBase / openApiBase / clientId **完全不同**（见 qoder.go 的
// CN 配置注释）。把一份国际版凭证的 product_id 改成 qodercn 之后：
//
//	续期请求会打 CN 的端点、但带的是国际版的 refresh_token → 401
//	而界面显示"凭证已失效，请重新登录" —— 把"导错产品"说成了"凭证坏了"
//
// 那是误导性的失败。宁可当场拒，并说清"这份是国际版的，请在国际版卡片上导入"。
func errWrongProduct(want, got string) error {
	return errors.New("该凭证属于 " + got + "，不能在 " + want + " 上导入" +
		"（两个产品的授权站点与客户端 id 不同，改标签会让续期必然失败）—— " +
		"请在 " + got + " 的卡片上导入它")
}

// errNoImportUID 解析出了凭证，但拿不到账号主键。
var errNoImportUID = errors.New("qoder: 无法确定账号标识（uid 为空且无法从 token 派生）")
