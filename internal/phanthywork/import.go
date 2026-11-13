// import.go 批量粘贴导入（gateway.AccountImportExt + CapImport）。
//
// # 用户粘什么（**四种形态**都认 —— 这正是本上游的特殊之处）
//
//	① 网关自己落的嵌套形 {"auth":{…},"account":{…}}（从别的部署拷过来的文件内容）
//	② **官方客户端的**凭据文件 {"claudeAiOauth":{accessToken,refreshToken,expiresAt}}
//	   —— 报告 auth §3.1 实测它落在 %USERPROFILE%/.phanthy-code/.credentials.json，
//	      用户最可能的动作就是把那个文件整段粘进来/拷进来
//	③ 本包扁平形 {"oauth":{…},"device":{…}}（手写最省事）
//	④ **顶层驼峰**形（2026-11-14 T-914，用户实测粘的那种）：
//	   {"platform":"phanthy","userid":"cmwc…","phone":"150…","account":"+86 150****3411",
//	    "accessToken":"eyJ…","refreshToken":"ZnGR…","expiresAt":1794545067558}
//	   —— 与②同一套驼峰字段名，区别是**没有 claudeAiOauth 根键**，且多了 `userid`/`phone`/`account`。
//	      ⚠ 它的 `userid` 就是 uid 的来源（②的兜底路径读不到它 ⇒ 用户那次"导不进去"）。
//
// 四种形态的判定全在 ParseCredential 里（**唯一判据**），本文件只做搬运 ——
// 手写第二份字段映射是"看起来成功、实际字段错位"的高发区。
// ⚠ 冲突时**按旧形态解释**（①→②→③→④ 的先后就是优先级；见 parseOfficialShape 里 wrapped 那段）。
//
// # 多份怎么分隔
//
// 用 gateway.SplitAccountImportItems（**共享骨架**）：它认数组、认一行一个对象的
// 拼接、认单对象。不自己写分隔逻辑 —— 那正是被抽成共享函数的东西。
package phanthywork

import (
	"fmt"
	"strings"

	"workbuddy2api/internal/gateway"
)

// 编译期断言。
var _ gateway.AccountImportExt = (*Provider)(nil)

// errNoImportUID 解析出了凭证，但拿不到账号主键。
var errNoImportUID = cryptoErr("无法确定账号标识（JWT 的 sub 与 account_scope 都取不到）")

// ImportCredentials 把用户粘贴的内容转成可落盘的凭证。
//
// # 失败语义：任一条坏 ⇒ 整批失败，但错误里**逐条列出是哪几份、为什么**
//
// 契约（gateway.AccountImportExt）要求"解析失败 → 返回带原因的错误"，
// 而共享骨架 ImportCredentialsGeneric 的语义是"任一条坏即整批失败"
// （它明确不做 skipBad：不引入当前无人使用的开关）。
//
// 但"整批失败"只说了一句"有一条坏了"，用户看不出**哪一条**。所以本实现自己
// 走一遍逐条循环：仍然整批失败（语义不变），但把每一份的原因与序号都收进错误里。
// 于是用户看到的是：
//
//	phanthywork: 导入失败：1/3 份有问题 —— 第 2 份: 凭证缺少 access_token（唯一鉴权材料）
//
// ⚠ 成功时不做"部分成功"：一份都没问题的场景下 len(结果) == 份数；
// 有任何一份有问题就一份都不落盘（调用方拿到 error → 不会写任何文件）。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	items, err := gateway.SplitAccountImportItems(pasted)
	if err != nil {
		// 切分失败 = 这段文本根本不是"一个或多个 JSON 对象"。
		// 共享骨架给的是原始 JSON 错误（"invalid character '{' after top-level value"）——
		// 它对定位有用，但对**用户**不够：他不知道自己在哪一步错了。
		// 所以这里补一层产品上下文，同时**保留原错误**（%w）供排查。
		return nil, fmt.Errorf("phanthywork: 粘贴内容无法按 JSON 对象切分"+
			"（每份应当是一个完整的 JSON 对象；支持数组、一行一个、或单对象）: %w", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("phanthywork: 没有可导入的内容（粘贴的是空的？）")
	}
	out := make([]gateway.ImportedCredential, 0, len(items))
	failed := make([]string, 0, 2)
	for i, item := range items {
		cred, perr := importOneCredential(item)
		if perr != nil {
			failed = append(failed, fmt.Sprintf("第 %d 份: %v", i+1, perr))
			continue
		}
		out = append(out, cred)
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("phanthywork: 导入失败：%d/%d 份有问题 —— %s",
			len(failed), len(items), strings.Join(failed, "；"))
	}
	return out, nil
}

// importOneCredential 解析一份粘贴内容并组装成可落盘的条目。
//
// 三步：ParseCredential（唯一判据）→ 取 uid（**空 uid 的凭证进池只会变成幽灵号**）
// → MarshalAuthFile（与页内登录产的凭证**逐字节同形**，下游不必区分来源）。
func importOneCredential(item string) (gateway.ImportedCredential, error) {
	a, err := ParseCredential([]byte(item))
	if err != nil {
		return gateway.ImportedCredential{}, err
	}
	uid := strings.TrimSpace(a.UID())
	if uid == "" {
		return gateway.ImportedCredential{}, errNoImportUID
	}
	name, raw, err := a.MarshalAuthFile()
	if err != nil {
		return gateway.ImportedCredential{}, err
	}
	if name == "" || len(raw) == 0 {
		return gateway.ImportedCredential{}, cryptoErr("序列化结果为空（不是可落盘的凭证）")
	}
	return gateway.ImportedCredential{
		FileName: name,
		Raw:      raw,
		UID:      uid,
		Nickname: a.Nickname(),
	}, nil
}
