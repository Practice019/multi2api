// accountkey.go 账号身份的"证据式"判重。
//
// # 为什么需要这个文件
//
// 用户报：同一个 MiniMax 账号登录两次，账号池里出现**两条**。
// 根因是 uid 被定义成 access_token 的哈希 —— 而 MiniMax 的令牌
// **每次登录都是新发的**（实测两条凭证 60 字符随机串、0 个点、
// 除前缀外无一相同）。主键建在会变的值上，判重自然失效。
//
// # 上游确实没有账号身份（三条路都实测过）
//
//	① 令牌解不出：不是 JWT，没有 sub（参照项目同样拿不到 —— 它的
//	   account_id 就是从 JWT sub 解的，对真实凭据恒 undefined）
//	② 端点拿不到：实测 9 个候选全 404 ——
//	   /mavis/api/v1/{user,me,account,user/info,usage}、
//	   /minimax-cloud/api/v1/{user/info,account/info}、
//	   account.minimax.cn/{oauth2/userinfo,userinfo}
//	③ 能用的端点里没有：credit/details 与 signin/status 的真实完整响应
//	   全文没有任何 user_id / uid / sub / account 字段
//
// # 所以退而求其次：账号作用域的实时数据当指纹
//
// 同一个号的两条不同令牌打积分端点，返回逐字段一致的桶标识：
//
//	granted_at_ms=1791613598478 expire_at_ms=1794153600000 credit_type=2
//
// ⚠ 这是**证据，不是身份**。三条硬约束：
//
//  1. 指纹为空 ⇒ 绝不合并（两个"零积分的不同账号"会有相同的空指纹，
//     合并就是把两个真账号并成一个、覆盖掉一份凭证 —— 覆盖不可逆）
//  2. 判不准 ⇒ 保持新增（重复用户能删；吞掉一个号用户不会知道）
//  3. 只在登录落盘前做一次，不进任何每请求路径
//
// # ⚠ 第二次报障逼出来的第三条尝试（先续期再取指纹）
//
// 判重上线后用户仍然看到两条。查下去是设计漏洞：
//
//	旧凭证 21:41:17 过期 → 用户 21:47 重登 →
//	判重要拿**已有凭证**去取指纹，而它的令牌已过期 →
//	积分端点回 401（实测）→ 取不到 → 按约束 2「不算命中」→ 又新增一条
//
// 也就是说：**恰恰在最需要判重的场景（旧号已过期、用户正是因此重登），
// 原实现注定失败。** 修法是取不到就先把已有凭证续期再取 ——
// 于是这一步顺带修好了用户同一句报障里的"凭据没有刷新"。
package minimax

import (
	"context"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// fingerprint 一份凭证的账号指纹（积分桶的稳定标识）。
//
// 取值：**所有桶**的 (credit_type, granted_at_ms, expire_at_ms, granted_amount)
// 四元组排序后拼接。**不含 remaining/consumed** —— 那两个数随消费变化，
// 同账号先后两次探测若跨了一次消费就会不一致 ⇒ 判重漏判，
// 而且这种漏判**看起来像随机**（这条由变异验证确认有判别力）。
//
// 空串 = 取不到（无桶 / 请求失败 / 形状不对）⇒ 调用方必须放弃合并。
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
		return "" // 空明细 ⇒ 空指纹。故意的，见文件头约束 1。
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
		// 缺任一标识字段就跳过 —— 拼出来的指纹只能由**确定**的信息组成。
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

// accountKey 取一份凭证的账号指纹，**必要时先把它续期**。
//
// 三级尝试（每步都有实测依据）：
//
//	① 命中缓存 AccountKey   —— 免费，且旧令牌已死时仍可用
//	② 用现有令牌打积分端点   —— 新鲜凭证走这条
//	③ 续期后再打一次         —— 已过期凭证走这条
//
// ③ 是第二次报障的直接修法。它的副作用是**好的**：走到 ③ 时旧凭证被
// 顺手续上，于是"旧凭据没被刷新"一起解决。
//
// 返回空串 = 三级都拿不到 ⇒ 调用方放弃合并。
func (p *Provider) accountKey(a *Auth) string {
	if a == nil || !a.Usable() {
		return ""
	}
	if s := strings.TrimSpace(a.AccountKey); s != "" {
		return s
	}
	ctx := context.Background()
	if fp := p.client.fingerprint(ctx, a); fp != "" {
		p.rememberKey(a, fp)
		return fp
	}
	if !a.Refreshable() {
		return ""
	}
	// 先续期再取（原地更新 + 落盘，见 RefreshCredential）。
	if err := p.RefreshCredential(gateway.Credential{
		Provider: providerID, UID: a.UID(), Secret: a,
	}); err != nil {
		logf("minimax: 判重前续期已有凭证失败 uid=%s：%v（无法判断是否同一账号，按新增处理）", a.UID(), err)
		return ""
	}
	fp := p.client.fingerprint(ctx, a)
	if fp != "" {
		p.rememberKey(a, fp)
		logf("minimax: 判重时发现已有凭证已过期 ⇒ 已续期并取到指纹 uid=%s", a.UID())
	}
	return fp
}

// rememberKey 把指纹缓存进凭证并落盘。
//
// 落盘失败不报错 —— 缓存只是加速与"令牌死后仍可比对"的兜底，
// 丢了下次重新取一次即可。
func (p *Provider) rememberKey(a *Auth, fp string) {
	if a == nil || fp == "" || a.AccountKey == fp {
		return
	}
	a.AccountKey = fp
	if err := saveInPlace(a); err != nil {
		logf("minimax: 账号指纹缓存落盘失败 uid=%s：%v（不影响本次判重）", a.UID(), err)
	}
}

// dedupIdentity 用指纹查这份凭证是否**已经**在池里，命中返回那条的 uid。
//
// 代价如实标注：最坏多打 N+1 次只读请求，外加对已过期凭证的若干次续期
// （N = 现有账号数）。只在登录落盘前做一次；有 AccountKey 缓存后，
// 同一账号的第二次比对是免费的。
func (p *Provider) dedupIdentity(a *Auth) (string, bool) {
	if a == nil || !a.Usable() {
		return "", false
	}
	mine := p.accountKey(a)
	if mine == "" {
		return "", false // 判不准 ⇒ 不合并（约束 1/2）
	}
	for _, uid := range p.knownUIDs() {
		if uid == "" || uid == a.UID() {
			continue
		}
		existing := p.cred(uid)
		if existing == nil || !existing.Usable() {
			continue
		}
		theirs := p.accountKey(existing)
		if theirs == "" {
			continue // 无法判断 ⇒ **不算命中**（宁留重复，不吞别人的号）
		}
		if theirs == mine {
			return uid, true
		}
	}
	return "", false
}

// rateKey 单个积分桶的身份四元组。
func rateKey(creditType int, grantedAt, expireAt int64, granted string) string {
	return string(rune('a'+creditType%26)) + ":" +
		dec(grantedAt) + ":" + dec(expireAt) + ":" + granted
}

// dec 非负整数转十进制字符串。
//
// 不引 strconv/fmt：入参恒为非负（调用点已判 >0），手写这十行更容易确认
// 没有错误路径，也不会因格式化分支引入意外输出。
func dec(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
