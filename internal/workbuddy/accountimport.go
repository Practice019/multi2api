package workbuddy

import (
	"encoding/json"
	"errors"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// accountimport.go workbuddy 自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么本包此前已有导入端点，还要再接这个扩展点
//
// `internal/workbuddy/import.go` 里的 `AdminHandler.handleImport` 早就实现了
// 批量导入（走自己的 `POST /admin/import`）。但核心本轮为**所有上游**统一了
// 一个入口 `POST /admin/accounts/import`，而那个入口的判据是
// `gateway.AccountImportExt`。
//
// 只声明 `CapImport` 而不接这个扩展点 → 界面渲染出「批量导入」按钮、
// 点下去回 501（**假按钮**）。这条不一致是**守卫测试当场抓到的**
//（`gateway.TestCapImportRequiresAccountImportExt` 的判据 + 各上游契约测试）。
//
// # 为什么不把 import.go 的逻辑搬过来（避免两份实现漂移）
//
// 这里直接复用 import.go 那套：`importItem`（含中文键/英文别名）、
// `fillAliases`、`decodeJWTClaims`、`importSafeUID`、`importChannelDefaults`。
// 搬一份的话迟早漂移 —— 而"导入产出的凭证与登录产出的不同形"
// 会让账号池出现无法解释的差异。
var _ gateway.AccountImportExt = (*Provider)(nil)

// errMissingAccessToken 一条输入里没有鉴权材料。
//
// 与 import.go 里那句文案**逐字相同**：同一个失败在两条入口上说同一句话，
// 用户排查时不会以为是两个不同的问题。
var errMissingAccessToken = errors.New("缺少 accessToken（它是唯一鉴权材料）")

// ImportCredentials 把用户粘贴的 JSON 转成可落盘的 workbuddy 凭证。
//
// 落盘走 `auth.Auth` 的嵌套形（与页内 OAuth 登录**逐字段同形**），
// 但这里**不落盘** —— 只产出"文件名 + 内容"，由核心决定写到哪
//（上游不认识 AuthDir 的权威值，与其它上游同一条约束）。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	items, err := gateway.SplitAccountImportItems(pasted)
	if err != nil {
		return nil, err
	}
	instanceID := ""
	if p != nil {
		instanceID = p.ID()
	}
	out := make([]gateway.ImportedCredential, 0, len(items))
	for _, raw := range items {
		var it importItem
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			return nil, err
		}
		// 与旧端点同一条：导出工具的键名变体补到主字段上
		//（中文键优先，别名只填空的槽位）。
		var m map[string]any
		if json.Unmarshal([]byte(raw), &m) == nil {
			fillAliases(&it, m)
		}
		c, ierr := buildImportedCredential(instanceID, it)
		if ierr != nil {
			return nil, ierr
		}
		out = append(out, c)
	}
	return out, nil
}

// buildImportedCredential 构造一条可落盘的凭证（**不写盘**）。
//
// 与 `importOne` 的关系：那边负责"构造 + SaveAtomic"，这里只做构造 ——
// 落盘位置由核心给。两者共用 `importChannelDefaults` 与同一套字段取舍，
// 所以不会漂移；**唯一**的差别就是落点由谁决定。
func buildImportedCredential(instanceID string, it importItem) (gateway.ImportedCredential, error) {
	sess := strings.TrimSpace(it.AccessToken)
	if sess == "" {
		return gateway.ImportedCredential{}, errMissingAccessToken
	}
	claims, _ := decodeJWTClaims(sess)

	uid, err := importSafeUID(firstNonEmptyStr(strings.TrimSpace(it.UID), claims.Sub))
	if err != nil {
		return gateway.ImportedCredential{}, err
	}
	expiresAt := it.ExpiresAt
	if expiresAt <= 0 {
		expiresAt = claims.Exp
	}
	nickname := firstNonEmptyStr(strings.TrimSpace(it.Username), claims.PreferredUsername, claims.Username, uid)

	channel, domain := importChannelDefaults(instanceID)
	// FilePath 只用于满足 MarshalNested 的"必须知道落点"这条防御
	//（它会拒绝 FilePath 为空的凭证，与 SaveAtomic 同一道检查）。
	// 真正的落点由核心拼 —— 这里的值**不会被采用**。
	a := &auth.Auth{
		AccessToken:  sess,
		RefreshToken: strings.TrimSpace(it.RefreshToken),
		ExpiresAt:    expiresAt,
		Domain:       domain,
		Channel:      channel,
		UID:          uid,
		Nickname:     nickname,
		// ⚠ 必须带上（用户实测点出来的）：`MarshalNested` 会把它一起写回，
		// 不给值就等于用空串覆盖用户已有的设备令牌 —— 而那是**每账号一个**
		// 的风控头，丢了之后回落链只剩全局值，表现为"导入后这个号开始被风控"。
		DeviceToken: strings.TrimSpace(it.DeviceToken),
		FilePath:    "workbuddy-" + uid + ".json",
	}
	raw, merr := a.MarshalNested()
	if merr != nil {
		return gateway.ImportedCredential{}, merr
	}
	return gateway.ImportedCredential{
		FileName: "workbuddy-" + uid + ".json",
		Raw:      raw,
		UID:      uid,
		Nickname: nickname,
	}, nil
}
