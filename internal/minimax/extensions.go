// extensions.go MiniMax 实现的全部 gateway 扩展点。
//
// 清单（与 zcode 对齐，缺一条就是缺失一个功能）：
//
//	AuthDirExt / DisplayNameExt / AccountColumnsExt   账号池呈现
//	CredentialTokenExt / ExpiryExt / LifetimeExt / RefreshSkewExt  凭证生命周期
//	CredentialLoader / CredentialSecretLoader          按上游重扫凭证
//	QuotaExt / HealthProbeExt / SoftRateExt            额度与健康
//	AdminExt / AccountImportExt / DailyActionExt       运维端点与签到
//	LoginFlow（在 login.go） / NoticeExt
//
// ⚠ 每一个都是**类型断言**发现的：方法名差一个字母就静默失效，
// 而编译能过、界面不报错。所以 interfaces_compile_test.go 里有一份
// 编译期断言把这整张清单钉住。
package minimax

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// ---- 账号池呈现 ----

// AccountColumns 账号池列集（有序的**规范列 id**）。
//
// # 报哪几列
//
//	provider  "上游"   多上游部署下靠它区分
//	nickname  "昵称"   本上游没有真实昵称（见 Auth.UID 的注释），
//	                   展示的是 token 短哈希 —— 但那一列仍有意义
//	quota     "额度"   积分余额，这一列**有真实数据**
//	checkin   "今日签到" 本上游有每日签到（DailyActionExt 也声明了它）
//	token_expiry "Token 到期" 凭证有过期时刻（expires_at），能看到还剩多久
//	success   "成功"   核心计数器，**每个上游都必须有**（用户明确要求）
//
// 刻意**不报**：
//
//	token  那一列读 `has_token`/`token_expire_sec`，而本上游的
//	       access_token **不是 JWT**、没有可读的 exp —— 用 token_expiry 代替
//	       （它有 expires_at 这个明确字段），与 codearts / loomy 同一判据
//	welfare           本上游没有福利领取
//	breaker / in_flight 用户明确要求账号池不显示这两列
func (p *Provider) AccountColumns() []string {
	return []string{
		gateway.AccountColProvider,
		gateway.AccountColNickname,
		gateway.AccountColQuota,
		gateway.AccountColCheckin,
		gateway.AccountColTokenExpiry,
		gateway.AccountColSuccess,
		gateway.AccountColOps,
	}
}

// ---- 凭证生命周期 ----

// HasToken 该凭证是否有可用令牌。
func (p *Provider) HasToken(cred gateway.Credential) bool {
	a, ok := cred.Secret.(*Auth)
	return ok && a != nil && a.Usable()
}

// TokenExpiry 报凭证过期时刻（**毫秒**）。
//
// ⚠ 单位是毫秒（本仓约定），不是秒 —— 传秒会让界面显示"1970 年"。
// 不知道时返回 ok=false（界面显示 `—`），**不是** 0（0 表示"真的已过期"）。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return 0, false
	}
	ms := a.ExpiresAtMS()
	if ms <= 0 {
		// "不知道"与"是 0"必须分开 —— 本仓的三态纪律。
		return 0, false
	}
	return ms, true
}

// NeverExpires 本上游的凭证**不会**永不过期。
//
// 它有明确的 `expires_at`（由 `expires_in` 算出），所以恒 false。
// 与 loomy / raccoon 那边"设计上就没有 TTL"是不同的形态。
func (p *Provider) NeverExpires(cred gateway.Credential) bool { return false }

// RefreshSkew 续期提前量。
//
// ⚠ 与 zcode 同一条理由：token 走到最后一刻才续期，会有
// "刚好在过期瞬间发请求"的窗口，那一次必然 401 —— 而 401 在本上游的
// 处置是"标记需重登"（因为 refresh_token 可能已失效），代价很高。
// 提前 5 分钟避开这个窗口。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil || !a.Refreshable() {
		return 0, false
	}
	return 5 * time.Minute, true
}

// ---- 按上游重扫凭证 ----

// LoadCredentials 读取 dir 下全部凭证（只投影 uid/nickname）。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.Credential{
			Provider: providerID,
			UID:      a.UID(),
			Nickname: a.DisplayName(),
			FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret。
//
// ⚠ 必须与 LoadCredentials **读同一份对象**（同一次 LoadDir 的结果）——
// 各扫一次会让"续期改了池里那份"与"磁盘那份"分叉。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: providerID,
				UID:      a.UID(),
				Nickname: a.DisplayName(),
				FilePath: a.FilePath,
			},
			Secret: a,
		})
	}
	return out, nil
}

// effectiveDir 解析生效的凭证目录（空 = 用本实例配置的那个）。
func (p *Provider) effectiveDir(dir string) string {
	if s := strings.TrimSpace(dir); s != "" {
		return s
	}
	if p != nil {
		return p.authDir
	}
	return ""
}

// ---- 额度 ----

// RefreshQuota 取积分余额。
//
// # 三态语义（本仓 QuotaView 的约定）
//
//	HasData=false         没查到 → 界面 `—`
//	HasData=true, 0       查到了确实是 0 → 界面 `0`
//	HasData=true, N       有余额
//
// ⚠ **余额取 `Σ details[].remaining_amount`，不是 `total_count`**。
// 参照项目修过的真实缺陷：`total_count` 是 `details[]` 的**记录条数**。
// 它俩在"余额为 0"时**偶然相等**（都是 0），所以任何只看 0 的断言都是
// 同义反复 —— 必须构造"一条 800 的记录"这种让两者分叉的样本
// （见 quota_test.go 的那条用例）。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	a := p.cred(uid)
	if a == nil || !a.Usable() {
		return gateway.QuotaView{HasData: false}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	top, err := p.client.creditDetails(ctx, a)
	if err != nil {
		// 端点失败**不等于余额为 0** —— 如实报"没查到"。
		logf("minimax: 额度查询失败 uid=%s: %v", uid, err)
		return gateway.QuotaView{HasData: false}, false
	}
	total, ok := parseCreditBalance(top)
	if !ok {
		return gateway.QuotaView{HasData: false}, false
	}
	return gateway.QuotaView{Kind: "credits", Remaining: int64(total), HasData: true}, true
}

// parseCreditBalance 从积分明细响应里算余额。
//
// # 判据链（每条都有参照项目的依据）
//
//	details 是数组         → Σ remaining_amount（字符串！用 looseAmount）
//	details 缺失/非数组     → 余额 **0**（有效结果，不是失败）
//	details 与 total_count 都缺 → 失败（ok=false）
//
// ⚠ `remaining_amount` 实测是**字符串**（`"800.00"`），而同一个响应里
// `total_count` 是裸数字 —— 上游同一份 JSON 混用两种类型。
//
// ⚠ 空串必须**先挡掉**：`Number(”)` 是 0，直接用会把"缺字段"读成
// "0 积分"。`looseAmount` 对空串返回 nil，这里的 `continue` 就是那条防线。
func parseCreditBalance(top map[string]any) (float64, bool) {
	if top == nil {
		return 0, false
	}
	raw, hasDetails := top["details"]
	if !hasDetails {
		// `details` 与 `total_count` 都没有 ⇒ 这不像一个余额响应。
		if _, hasCount := top["total_count"]; !hasCount {
			return 0, false
		}
		// 有 total_count 但没 details ⇒ **余额 = 0**（有效结果）。
		return 0, true
	}
	list, ok := raw.([]any)
	if !ok {
		// `details` 存在但不是数组 ⇒ 形状不对，判失败（不猜）。
		return 0, false
	}
	var sum float64
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		amt := looseAmount(m["remaining_amount"])
		if amt == nil {
			// 空串/非数字 ⇒ 这条不计，**不编造 0**。
			continue
		}
		sum += *amt
	}
	return roundCredits(sum), true
}

// ---- 健康与限流 ----

// ProbeHealth 探测凭证是否健康。
//
// 用"拉一次积分明细"当探针：它是最便宜的鉴权端点（**只读**、不消耗额度、
// 不改任何状态），而 401/403 明确说明凭证坏了。
//
// ⚠ 402 不算不健康 —— 那是"余额不足"，账号本身是好的
// （本仓在 zcode 上踩过：把额度耗尽记成不健康会让整批号被停用）。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := p.authOf(cred)
	if err != nil {
		return err
	}
	_, err = p.client.creditDetails(ctx, a)
	return err
}

// SoftRateReset 解析上游给的"将在 … 重置"时刻。
//
// 本上游的限流信息**不在 body 的结构化字段里**（参照项目没用任何
// 429 专项解析），所以这里恒返回 ok=false —— 让核心走既有的
// 账号级软冷却（softRate 基数 + 指数退避）。
//
// ⚠ 返回 false 是**有意义**的答复（"我不知道具体时刻"），
// 不是"没实现"：核心据此用保守退避，而不是瞎猜一个时刻。
func (p *Provider) SoftRateReset(status int, body string) (time.Time, bool) {
	return time.Time{}, false
}

// ---- 导入 ----

// ImportCredentials 把一段用户粘贴的 JSON 转成可落盘的凭证。
//
// 走共享的 `gateway.SplitAccountImportItems` —— 它认三种形状：
// 单对象 / `[ ]` 数组 / **多个独立对象连在一起**（用户一次粘 N 个文件）。
//
// ⚠ 裸 token（`mmoat_…`）也认：用户从别处复制 token 时不会特意包 JSON。
// 判据与 zcode 的裸 Key 同款 —— 只在"不是 JSON"时才走这条兜底。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	text := strings.TrimSpace(pasted)
	if text == "" {
		return nil, fmt.Errorf("minimax: 粘贴内容为空")
	}
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		items, err := gateway.SplitAccountImportItems(text)
		if err != nil {
			return nil, fmt.Errorf("minimax: %w", err)
		}
		out := make([]gateway.ImportedCredential, 0, len(items))
		var errs []string
		for i, it := range items {
			// 多条时**跳过坏条目**、只要求至少一条成功
			//（本仓既有契约：不能因一条坏的全盘失败）。
			ic, ierr := p.importOneJSON(it)
			if ierr != nil {
				errs = append(errs, fmt.Sprintf("第 %d 条: %v", i+1, ierr))
				continue
			}
			out = append(out, ic)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("minimax: 没有解析出任何可用凭证：%s", strings.Join(errs, "; "))
		}
		return out, nil
	}
	// 非 JSON：按"每行一个 token"处理（容忍 `Bearer ` 与引号）。
	var out []gateway.ImportedCredential
	var errs []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ic, err := p.importOneToken(line)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		out = append(out, ic)
	}
	if len(out) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("minimax: 没有解析出任何可用凭证：%s", strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("minimax: 没有解析出任何可用凭证")
	}
	return out, nil
}

// importOneJSON 解析一条 JSON 对象。
func (p *Provider) importOneJSON(text string) (gateway.ImportedCredential, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return gateway.ImportedCredential{}, fmt.Errorf("minimax: JSON 解析失败: %w", err)
	}
	a, err := authFromMap(m)
	if err != nil {
		return gateway.ImportedCredential{}, err
	}
	return p.credentialFor(a)
}

// importOneToken 解析一行裸 token。
func (p *Provider) importOneToken(line string) (gateway.ImportedCredential, error) {
	tok := strings.TrimSpace(line)
	tok = strings.TrimPrefix(tok, "Bearer ")
	tok = strings.Trim(tok, `"'`)
	if tok == "" {
		return gateway.ImportedCredential{}, fmt.Errorf("minimax: 空 token")
	}
	return p.credentialFor(&Auth{AccessToken: tok, TokenType: "Bearer"})
}

// authFromMap 从 JSON 对象里构造 Auth（容忍嵌套 `auth` 层与 camelCase）。
//
// ⚠ 容忍 camelCase 是有据的：客户端登录态文件用的是
// `accessToken` / `refreshToken` / `expiresAtMs`（见参照项目
// `minimax-credential.ts`），而本插件的形状是 snake_case。
// 用户很可能把客户端那份直接粘进来 —— 不认 camelCase 会让他
// 以为"导入坏了"，而我们其实只是没读那个键。
func authFromMap(m map[string]any) (*Auth, error) {
	if inner, ok := m["auth"].(map[string]any); ok {
		m = inner
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok2 := v.(string); ok2 && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
				// 数字也接受（expiresAtMs 是毫秒**数字**）。
				if f, ok2 := toFloat(v); ok2 && f > 0 {
					return fmt.Sprintf("%d", int64(f))
				}
			}
		}
		return ""
	}
	a := &Auth{
		AccessToken:  str("access_token", "accessToken", "token"),
		RefreshToken: str("refresh_token", "refreshToken"),
		TokenType:    str("token_type", "tokenType"),
		ExpiresAt:    str("expires_at", "expiresAt", "expiresAtMs", "expires_at_ms"),
		Scope:        str("scope"),
		AccountID:    str("account_id", "accountId"),
		Nickname:     str("nickname", "name", "label"),
	}
	if !a.Usable() {
		return nil, fmt.Errorf("minimax: 缺少 access_token")
	}
	if a.TokenType == "" {
		a.TokenType = "Bearer"
	}
	return a, nil
}

// credentialFor 把 Auth 变成可落盘的 ImportedCredential。
func (p *Provider) credentialFor(a *Auth) (gateway.ImportedCredential, error) {
	if !a.Usable() {
		return gateway.ImportedCredential{}, fmt.Errorf("minimax: 凭证不可用（缺 access_token）")
	}
	uid := a.UID()
	if uid == "" {
		return gateway.ImportedCredential{}, fmt.Errorf("minimax: 无法确定账号身份")
	}
	raw, err := json.MarshalIndent(authFile{A: a}, "", "  ")
	if err != nil {
		return gateway.ImportedCredential{}, err
	}
	return gateway.ImportedCredential{
		FileName: FileName(a),
		Raw:      raw,
		UID:      uid,
		Nickname: a.DisplayName(),
	}, nil
}

// ---- 签到 ----

// DailyActions 报本上游的每日动作。
//
// 只报 `checkin`（每日签到）—— 本上游**没有**保活/福利/旅行那些概念，
// 报了就是"假按钮"（点下去必然失败）。
//
// OneURL / AllURL 都指向**本包自己的**管理端点（在 admin.go 里注册）：
//
//	/admin/minimax/checkin       单账号（body `{"uid":"…"}`）
//	/admin/minimax/checkin-all   本上游全部账号
//
// ⚠ 作用域是**本上游**，不是整个账号池 —— 改造前的前端把「全部签到」
// 写成对整池调用 workbuddy 挂的那条路由，在两个上游的部署里语义是模糊的。
func (p *Provider) DailyActions() []gateway.DailyAction {
	return []gateway.DailyAction{
		{
			ID:     "checkin",
			Label:  "签到",
			Title:  "领取当天的签到积分（幂等：今天领过会提示「今日已签到」）",
			OneURL: "/admin/minimax/checkin",
			AllURL: "/admin/minimax/checkin-all",
		},
	}
}

// CheckinAll 对本上游全部账号执行一次签到。
//
// # 为什么返回逐账号结果而不是一个数字
//
// 顶部按钮要一次触发多个上游。只回一个总数，某个上游整片失败时
// 用户看到的是"成功了 N 个"，无法判断是哪个上游没动。
func (p *Provider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	rep := gateway.DailyCheckinReport{Provider: providerID}
	uids := p.knownUIDs()
	if len(uids) == 0 {
		// 0 个账号**不是失败**（空切片 + 空 Error）。
		return rep
	}
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			rep.Error = "上下文取消（已中止）"
			return rep
		}
		rep.Results = append(rep.Results, p.checkinOne(ctx, uid))
	}
	return rep
}

// checkinOne 单账号签到（单账号端点与全量端点共用）。
func (p *Provider) checkinOne(ctx context.Context, uid string) gateway.DailyCheckinResult {
	a := p.cred(uid)
	if a == nil || !a.Usable() {
		return gateway.DailyCheckinResult{UID: uid, Status: "skip", Detail: "凭证不可用"}
	}
	data, err := p.client.signinClaim(ctx, a)
	if err != nil {
		return gateway.DailyCheckinResult{UID: uid, Status: "fail", Detail: err.Error()}
	}
	res, rerr := signinClaimResult(data)
	if rerr != nil {
		return gateway.DailyCheckinResult{UID: uid, Status: "fail", Detail: rerr.Error()}
	}
	return gateway.DailyCheckinResult{UID: uid, Status: res.Status, Detail: res.Detail}
}

// signinClaimOutcome 签到领取的结果（本包内部的中间形态）。
type signinClaimOutcome struct {
	Status string
	Detail string
	Credit int64
}

// signinClaimResult 把签到领取的响应翻成结果。
//
// ⚠ 幂等判据是 **`claim_result`**，不是 HTTP 状态码（重复领取同样 200）：
//
//	1 = 真领到          → ok
//	2 = 已领过          → already（**必须**与"领到"区分：
//	                      报成 ok 会让用户以为积分又加了一次）
//	其他/缺失/类型不对   → **失败**（绝不虚报成功）
//
// 参照项目原话：`claim_result` 缺失/null/越界/字符串时一律判 failed ——
// 虚报会让用户以为 +了积分，实际 +0。
func signinClaimResult(data map[string]any) (signinClaimOutcome, error) {
	if data == nil {
		return signinClaimOutcome{}, fmt.Errorf("minimax: 签到响应为空")
	}
	// 严格：字符串 "1" 也算无效（参照项目明确要求）。
	n, ok := strictInt(data["claim_result"])
	if !ok {
		return signinClaimOutcome{}, fmt.Errorf("minimax: 签到响应缺少有效的 claim_result")
	}
	switch n {
	case claimResultClaimed:
		credit := int64(0)
		// ⚠ `points` 是**总数**，`bonus_points` **含在其中**，不得相加
		//（参照项目实测：points 800 / bonus_points 400 时客户端显示 800，
		//  相加会让展示金额虚高一倍 —— 用户亲自纠正过）。
		if f, ok2 := toFloat(data["points"]); ok2 {
			credit = int64(roundCredits(f))
		}
		return signinClaimOutcome{
			Status: "ok",
			Detail: fmt.Sprintf("签到成功，+%d 积分", credit),
			Credit: credit,
		}, nil
	case claimResultAlreadyClaimed:
		return signinClaimOutcome{Status: "already", Detail: "今日已签到"}, nil
	default:
		return signinClaimOutcome{}, fmt.Errorf("minimax: 签到返回未知的 claim_result=%v", n)
	}
}

// ---- 内部辅助 ----

// Notice 界面上挂在本上游分组旁的一句提醒。
//
// # 为什么要提醒（不是客套话）
//
// 本上游的协议事实全部来自开源参照项目的**源码逐行提取**
// （`deepseek-harness-codearts/src/minimax*.ts`），而本机**没有** MiniMax
// 凭证（客户端登录态文件 `~/.minimax/auth/.../auth.json` 也不在这台机器上），
// 所以：
//
//	已验证：OAuth 设备码握手形状、目录/积分/签到的解析、SSE 转换、
//	        错误分类 —— 全部用 hermetic 假上游覆盖
//	未验证：**真实对话**（推理端点一次都没打过真上游）
//
// 与 zcode 那条提醒同款口径：只说"未经真实对话验证"，
// **不写"不稳"**这类没有依据的话 —— 已实测的部分是稳的。
func (p *Provider) Notice() string {
	return "测试中：未经真实对话验证（协议抄自开源参照实现）"
}

// knownUIDs 本实例已知的 uid 列表。
//
// ⚠ 优先用核心注入的枚举器 —— 生产上 `p.creds` 是空的（凭证来自池子）。
// 只读本地会让管理端点返回 `200 + {"accounts":[]}`，看起来像"没有账号"。
func (p *Provider) knownUIDs() []string {
	p.mu.RLock()
	enum := p.uidEnumerator
	local := make([]string, 0, len(p.creds))
	for uid := range p.creds {
		local = append(local, uid)
	}
	p.mu.RUnlock()

	if enum != nil {
		if list := enum(); len(list) > 0 {
			sort.Strings(list)
			return list
		}
	}
	sort.Strings(local)
	return local
}
