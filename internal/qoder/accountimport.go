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
	// T-331：分割 / 汇总 / 逐条失败语义共用；下面只保留 qoder 特有的两步：
	//   ① 必须在 ParseCredential **之前**从原文读出 product_id（见 explicitProductID 的注释）
	//   ② 导入时把 ProductID 归一到本实例的 productID
	return gateway.ImportCredentialsGeneric(pasted, func(item string) (gateway.ImportedCredential, error) {
		explicit := explicitProductID(item)
		a, perr := ParseCredential([]byte(item))
		if perr != nil {
			return gateway.ImportedCredential{}, perr
		}
		if explicit != "" && explicit != p.productID {
			return gateway.ImportedCredential{}, errWrongProduct(p.productID, explicit)
		}
		a.ProductID = p.productID
		uid := strings.TrimSpace(a.UIDValue())
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
