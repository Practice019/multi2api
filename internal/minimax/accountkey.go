// accountkey.go 账号身份的"证据式"判重。
//
// # 为什么需要这个文件
//
// 用户报：同一个 MiniMax 账号登录两次，账号池里出现**两条**。
//
// 根因是我把 uid 定义成 access_token 的哈希 —— 而 MiniMax 的令牌
// **每次登录都是新发的**（实测两条凭证：60 字符随机串、0 个点、
// 除前缀外无一相同）。主键建在会变的值上，判重自然失效。
//
// # 上游确实没有账号身份（三条路都实测过）
//
//	① 令牌解不出：不是 JWT，sub 无从谈起（参照项目同样拿不到）
//	② 端点拿不到：/mavis/api/v1/{user,me,account,user/info,usage}、
//	   /minimax-cloud/api/v1/{user/info,account/info}、
//	   account.minimax.cn/{oauth2/userinfo,userinfo} **全部 404**
//	③ 能用的端点里没有：credit/details 与 signin/status 的真实完整
//	   响应里没有任何 user_id / uid / sub / account 字段
//
// # 所以退而求其次：用账号作用域的实时数据当指纹
//
// 实测同一个账号的两条不同令牌打积分端点，返回**逐字段一致**：
//
//	granted_at_ms=1791613598478 expire_at_ms=1794153600000 credit_type=2
//
// 两条不同令牌的指纹相同 ⇒ 极大概率同一账号。
//
// ⚠ **这是证据，不是身份**。所以有两条硬约束：
//
//  1. **指纹为空 ⇒ 绝不合并**。新号可能一条积分包都没有，
//     两个"零积分的不同账号"会有同样的空指纹 —— 若允许合并，
//     就是把两个真实账号并成一个，比重复更糟（少一个号、丢一份额度）。
//  2. **判不准就按新增处理**（保持今天的行为），并在回执里说清楚。
//     宁可留一条重复让用户删，也不静默覆盖别人的凭证 ——
//     覆盖是**不可逆**的（旧 refresh_token 被替换后无法找回）。
package minimax

import (
	"context"
	"sort"
	"strings"
	"time"
)

// fingerprint 一份凭证的账号指纹（积分桶的稳定标识）。
//
// 取值：**所有桶**的 (credit_type, granted_at_ms, expire_at_ms, granted_amount)
// 四元组排序后拼接。**不含** remaining/consumed —— 那两个数会随消费变化，
// 同账号先后两次探测若跨了一次消费就会不一致，导致判重漏判。
//
// 返回空串 = 取不到指纹（无桶、请求失败、形状不对）⇒ 调用方必须放弃合并。
func (c *Client) fingerprint(ctx context.Context, a *Auth) string {
	if a == nil || !a.Usable() {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	top, err := c.creditDetails(ctx, a)
	if err != nil {
		return ""
	}
	list, ok := top["details"].([]any)
	if !ok || len(list) == 0 {
		// 空明细 ⇒ 指纹为空。这是**故意**的：见文件头的硬约束 1。
		return ""
	}
	rows := make([]string, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ct, _ := toFloat(m["credit_type"])
		ga, _ := toFloat(m["granted_at_ms"])
		ex, _ := toFloat(m["expire_at_ms"])
		granted := strings.TrimSpace(str(m["granted_amount"]))
		// 缺任一标识字段就跳过这条 —— 拼出来的指纹必须只由**确定**的信息组成。
		if ga <= 0 || ex <= 0 || granted == "" {
			continue
		}
		rows = append(rows, rateKey(int(ct), int64(ga), int64(ex), granted))
	}
	if len(rows) == 0 {
		return ""
	}
	sort.Strings(rows)
	return strings.Join(rows, "|")
}

// rateKey 单个积分桶的身份四元组（拼成一段字符串）。
func rateKey(creditType int, grantedAt, expireAt int64, granted string) string {
	return string(rune('a'+creditType%26)) + ":" +
		dec(grantedAt) + ":" + dec(expireAt) + ":" + granted
}

// dec 把整数转十进制字符串（避免为了这一件事引 strconv 到调用点）。
func dec(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// dedupIdentity 用指纹查这份凭证是否**已经**在池里。
//
// 命中时返回那条已有账号的 uid ⇒ 调用方把新令牌**写进它的文件**
// （而不是新增一条）。
//
// # 代价，如实标注
//
// 最坏情况要多打 **N+1 次只读请求**（N = 现有账号数）：新凭证一次 + 每个
// 已有凭证一次。登录是低频操作，这个代价换"不再制造重复账号"是划算的；
// 但它确实存在，所以只在登录落盘前做一次，不进任何每请求路径。
//
// 已有账号的指纹会在本轮内缓存（同一份凭证不重复探测）。
func (p *Provider) dedupIdentity(a *Auth) (string, bool) {
	if a == nil || !a.Usable() {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mine := p.client.fingerprint(ctx, a)
	if mine == "" {
		// 判不准 ⇒ 不合并。保持"新增"这个可逆的、信息更少的行为。
		return "", false
	}
	for _, uid := range p.knownUIDs() {
		if uid == "" || uid == a.UID() {
			continue
		}
		existing := p.cred(uid)
		if existing == nil || !existing.Usable() {
			continue
		}
		// 自己的指纹不必再算（uid 相同就是同一条）。
		theirs := p.client.fingerprint(ctx, existing)
		if theirs == "" {
			continue // 对方取不到指纹 ⇒ 无法判断，跳过（**不算命中**）
		}
		if theirs == mine {
			return uid, true
		}
	}
	return "", false
}
