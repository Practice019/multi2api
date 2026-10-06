// extensions.go 本上游的可选扩展点实现。
//
// # 机制（本仓的架构判据）
//
// 上游**只实现自己有的扩展**，核心用纯类型断言发现：
//
//	if ax, ok := gateway.ExtOf[gateway.AdminExt](p); ok { ... }
//
// 所以本文件里的每个方法都是"可选提供"，**没有的性能就等于没有** ——
// 不需要在一个大接口里塞空实现。
//
// # 本上游提供了什么、刻意没提供什么
//
//	✓ DisplayName          控制台中文名
//	✓ AuthDir              凭证目录（账号池按它扫盘）
//	✓ CredentialTokenExt   有没有令牌（空凭证不并池）
//	✓ CredentialExpiryExt  过期时刻（续期判据靠它）
//	✓ CredentialLifetimeExt API Key 通道**永不过期**
//	✓ RefreshSkewExt       续期提前量（JWT 走到最后一刻才续会撞 401）
//	✓ AccountColumnsExt    账号池多两列（通道 / 平台）
//	✓ AccountImportExt     粘贴 API Key 导入
//	✓ QuotaExt             余额/配额（JWT 通道走 billing，Key 通道走探测）
//	✓ HealthProbeExt       主动健康检查
//	✓ SoftRateExt          **业务码形态的限流**（本上游最要紧的一个）
//	✗ DailyCheckinExt      没有签到端点（额度由订阅周期决定）
//	✗ ImageGenExt          不做生图
//	✗ JobExt               没有需要定期轮询的东西（续期由出口层驱动）
package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// DisplayName 控制台显示名。
func (p *Provider) DisplayName() string { return displayName }

// AuthDir 凭证目录。
func (p *Provider) AuthDir() string { return p.authDir }

// HasToken 该凭证是否有可用令牌。
//
// 为什么要它：账号池并池时用它过滤空凭证。
// 没有这个扩展时核心只能"假定非空"，那会把写坏的凭证文件并进池子，
// 表现为"账号在列表里但每个请求都 401"。
func (p *Provider) HasToken(cred gateway.Credential) bool {
	a, ok := cred.Secret.(*Auth)
	return ok && a != nil && a.Usable()
}

// TokenExpiry 凭证过期时刻（Unix 毫秒）。
//
// ⚠ 本仓的约定是**毫秒**（见 gateway.CredentialExpiryExt 与 raccoon 的实现），
// 而 Auth.ExpiresAt 存的是秒 —— 转换在这里做，不让上游单位泄漏到核心。
//
// ok=false 表示"没有过期信息"。**不能返回 0 表示永不**
// —— 那会让续期判据把 API Key 当成"刚过期"，于是每个请求都去续期。
func (p *Provider) TokenExpiry(cred gateway.Credential) (int64, bool) {
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return 0, false
	}
	if a.ExpiresAt <= 0 {
		return 0, false
	}
	return a.ExpiresAt * 1000, true
}

// NeverExpires API Key 通道的凭证永不过期。
//
// # 为什么这个区分是必要的（本仓踩过的坑）
//
// 出口层判"该不该续期"时，对"没有过期信息"的凭证是**保守处理**
// （按需续期，因为不知道它过期没）。那对 API Key 是**纯浪费** ——
// 它根本没有过期概念，却每个请求都触发一次续期往返。
//
// 声明 NeverExpires 之后，核心就明确知道"这个凭证不用管续期"。
func (p *Provider) NeverExpires(cred gateway.Credential) bool {
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil {
		return false
	}
	// JWT 通道有明确过期时刻 → 需要续期（虽然当前只能重新登录）。
	return !a.UsesJWT()
}

// RefreshSkew 续期提前量。
//
// ok=false 的语义是"该凭证不需要续期"（与 NeverExpires 一致）。
func (p *Provider) RefreshSkew(cred gateway.Credential) (time.Duration, bool) {
	a, ok := cred.Secret.(*Auth)
	if !ok || a == nil || !a.UsesJWT() {
		return 0, false
	}
	return refreshSkew, true
}

// AccountColumns 账号池列集（有序的**规范列 id**）。
//
// # ⚠ 必须回规范词汇表里的 id，不是中文标题
//
// 我第一版回的是 `[]string{"通道", "平台"}` —— 那是**列标题**，不是 id。
// 核心按规范词汇表校验，未登记的 id 被跳过 → 两列都不显示，
// 而这只在运行时日志里有一行警告。
//
// # ⚠ 第二版又报了错的列（Token 永远显示 `—`）
//
// 我原来报了 `token`（"Token 剩余有效期"）。但那个列的判据是
// `has_token` + `token_expire_sec` —— 也就是**上游要提供一个到期时刻**。
//
// 而 zcode 的 JWT **没有 `exp`**（实测解出来只有 iat）：
//
//	{"user_id":"…","token_version":0,"sub":"…","iat":1791258329}
//
// 官方自己的 `resolveJwtExpiration` 对这种 token 也返回 `"unknown"`：
//
//	if (typeof payload.exp !== "number" || …) return { kind: "unknown" }
//
// 也就是说"没有 exp"是**上游的事实**，不是我们没解析出来 ——
// 所以这一列对 zcode 永远只会是 `—`。前端注释里
// （webui.html 的 tokenExpiryCellHTML）把这类情况写得很清楚：
//
//	codearts 的凭证是 AK/SK/STS 三元组、accessToken 恒为空
//	→ 它落到「Token」列会永远是 `—`
//
// 我犯了与那时同一个判断错误：**给一个"没有该字段"的上游报了依赖该字段的列**。
//
// # 现在报哪几列
//
//	provider  "上游"   多上游部署下靠它区分
//	nickname  "昵称"   本上游有昵称（登录拿到 name/email）——
//	                   比 UID 可读得多，而 UID 是 36 位 uuid
//	quota     "额度"   JWT 通道走 billing/current，这一列**有真实数据**
//
// 刻意**不报**：
//
//	token / token_expiry  见上（JWT 无 exp，永远是 `—`）
//	uid                    36 位 uuid 占很宽，昵称更有用；
//	                       完整 uid 在诊断端点里能看
//	checkin / welfare      本上游没有这些概念
//	success/breaker/in_flight  排障计数器，用户在账号池里已经删掉了
func (p *Provider) AccountColumns() []string {
	return []string{
		gateway.AccountColProvider,
		gateway.AccountColNickname,
		gateway.AccountColQuota,
	}
}

// Quota 取额度视图。
//
// # 两条通道的取法不同（实测确认）
//
//	jwt     → GET /api/v1/zcode-plan/billing/**current**?app_version=…
//	          （current 失败再试 /billing/balance —— 抄参照实现的主备顺序）
//	api-key → 没有额度端点，用一次 1-token 探测判断"能不能用"
//
// # ⚠ 我把主备顺序搞反过（用户实测报的 bug）
//
// 我原来直接打 `balance`，而**实测它是 400**：
//
//	GET /billing/balance                      → 400 {"code":3001,"msg":"parameter error"}
//	GET /billing/balance?app_version=3.14.0   → 400 {"code":3001,"msg":"parameter error"}
//	GET /billing/balance?user_id=…            → 400 {"code":3001,"msg":"parameter error"}
//
// 而 `current` 直接就通：
//
//	GET /billing/current?app_version=3.14.0   → 200 {"code":0,"data":{"server_time":…,"plans":[]}}
//
// 参照实现（zcode-proxy quota.go）的原话印证了这个顺序：
//
//	jwt → zcode.z.ai /api/v1/zcode-plan/billing/current（失败再试 /billing/balance）
//
// 两条都要带 `?app_version=` —— 缺了它 current 也能通（实测），
// 但参照实现的头集合与查询串里都有版本，所以照抄以保持一致。
//
// # 为什么 HasData 不能随便置 true
//
// 本仓 QuotaView 的注释把三态分得很清：
//
//	HasData=false        没查到 → 界面显示 `—`
//	HasData=true, 0      查到了确实是 0 → 显示 `0`
//
// 把"查不到"报成 0 会让用户以为额度用尽了（而实际是我们没查到），
// 那会引发一轮无意义的排查。
//
// ⚠ 但"查到了但账号没有套餐"（plans 与 balances 都空）**是**一个真实结论 ——
// 那种情况报 HasData=true + Remaining=0 才对，界面显示 `0` 而非 `—`：
// 用户需要知道"这个号登录成功了，但没有订阅额度"，而不是"我们查不到"。
func (p *Provider) RefreshQuota(uid string) (gateway.QuotaView, bool) {
	a := p.cred(uid)
	if a == nil || !a.Usable() {
		return gateway.QuotaView{HasData: false}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if a.UsesJWT() {
		return p.quotaFromBilling(ctx, a)
	}
	return p.quotaFromProbe(ctx, a)
}

// quotaFromBilling 从 Coding Plan 的计量端点取额度（current → balance 主备）。
func (p *Provider) quotaFromBilling(ctx context.Context, a *Auth) (gateway.QuotaView, bool) {
	// ① 主：billing/current（实测 200）。
	if raw, err := p.client.BillingCurrent(ctx, a); err == nil {
		if view, ok := parseBillingSnapshot(raw); ok {
			return view, true
		}
	}
	// ② 备：billing/balance。
	//
	// ⚠ 实测它对**没有 plan 上下文的调用**返回 400 parameter error ——
	// 所以这条兜底常常失败。这也正是"主路径必须是 current"的原因。
	if raw, err := p.client.BillingBalance(ctx, a); err == nil {
		if view, ok := parseBillingSnapshot(raw); ok {
			return view, true
		}
	}
	// 两条都拿不到 → 如实报"没查到"（不编 0）。
	return gateway.QuotaView{HasData: false}, false
}

// billingSnapshot 计量端点的响应结构（current 与 balance 同形）。
//
// 字段名抄参照实现 `zcode-proxy/quota.go` 的 normalizeBalanceResponse，
// 它同时接受多组别名（`total_units`/`total`/`remaining`）——
// 因为**上游在不同版本里改过字段名**，只认一套会静默取到 0。
//
// ⚠ 数字字段一律用 `any` 而不是 `json.Number`：
// 上游会把用量写成**带引号的字符串**，参照实现为此专门处理
// `"1,234"`（去掉千分位逗号，见它的 jsonNum）。
// 而 `json.Number` 只接**裸数字**，遇到 `"1234"` 会让整个 Unmarshal 失败 ——
// 那是**全盘静默丢失**（连其它字段一起丢掉），比取到 0 更难查。
type billingSnapshot struct {
	Data struct {
		Plans []struct {
			PlanID string `json:"plan_id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			// 到期时刻：参照实现按 ends_at → expires_at 顺序取。
			EndsAt    any `json:"ends_at"`
			ExpiresAt any `json:"expires_at"`
		} `json:"plans"`
		Balances []struct {
			ShowName      string `json:"show_name"`
			Name          string `json:"name"`
			EntitlementID string `json:"entitlement_id"`
			// 主字段名。
			RemainingUnits any `json:"remaining_units"`
			TotalUnits     any `json:"total_units"`
			UsedUnits      any `json:"used_units"`
			// 别名（参照实现容忍的更老/更新字段名）。
			Remaining any    `json:"remaining"`
			Total     any    `json:"total"`
			Used      any    `json:"used"`
			UnitType  string `json:"unit_type"`
			Meter     string `json:"meter"`
		} `json:"balances"`
		// 顶层直给（部分响应不带 balances）。
		TotalUnits     any `json:"total_units"`
		UsedUnits      any `json:"used_units"`
		RemainingUnits any `json:"remaining_units"`
	} `json:"data"`
}

// parseBillingSnapshot 把计量响应转成 QuotaView。
//
// # 三态语义（照参照实现，也是本仓 QuotaView 的约定）
//
//	信封不存在 / code 非 0        → ok=false（没查到 → 界面 `—`）
//	查到了但 plans 与 balances 全空 → HasData=true, Remaining=0（界面 `0`）
//	查到了有额度                  → HasData=true, Remaining=N
//
// 中间那条是本次修复的要点：用户账号登录成功但没有订阅套餐时，
// 上游回的是 `{"plans":[]}`（**实测就是这个形态**）。
// 如果我把它当"没查到"，界面显示 `—`，用户会以为是我们坏了；
// 报 `0` 才是实话："这个号确实没有额度"。
func parseBillingSnapshot(raw []byte) (gateway.QuotaView, bool) {
	// 先判信封：code 非 0 时是业务失败，不是"空额度"。
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return gateway.QuotaView{HasData: false}, false
	}
	if env.Code != 0 {
		return gateway.QuotaView{HasData: false}, false
	}

	var snap billingSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return gateway.QuotaView{HasData: false}, false
	}

	byModel := map[string]int64{}
	var total int64
	for _, b := range snap.Data.Balances {
		// 三值互推：参照实现的做法（缺一个时用另两个算）。
		totalUnits := numOr(b.TotalUnits, b.Total)
		usedUnits := numOr(b.UsedUnits, b.Used)
		remainUnits := numOr(b.RemainingUnits, b.Remaining)
		if remainUnits == nil && totalUnits != nil && usedUnits != nil {
			r := *totalUnits - *usedUnits
			if r < 0 {
				r = 0
			}
			remainUnits = &r
		}
		if remainUnits == nil {
			// 一个数都没有 → 这个桶没信息，跳过（不要造 0）。
			continue
		}
		n := int64(*remainUnits)
		total += n

		name := firstNonEmptyStr(b.ShowName, b.Name, b.EntitlementID, b.UnitType, b.Meter)
		if name == "" {
			name = "额度"
		}
		byModel[name] += n
	}
	if len(byModel) > 0 {
		view := gateway.QuotaView{Kind: "credits", Remaining: total, HasData: true}
		if len(byModel) > 1 {
			view.Kind = "per_model"
			view.ByModel = byModel
		}
		return view, true
	}

	// 没有 balances 桶：看顶层直给的数字（参照实现也认这套）。
	if n := numOr(snap.Data.RemainingUnits); n != nil {
		return gateway.QuotaView{Kind: "credits", Remaining: int64(*n), HasData: true}, true
	}
	if t := numOr(snap.Data.TotalUnits); t != nil {
		used := numOr(snap.Data.UsedUnits)
		remain := *t
		if used != nil {
			remain = *t - *used
			if remain < 0 {
				remain = 0
			}
		}
		return gateway.QuotaView{Kind: "credits", Remaining: int64(remain), HasData: true}, true
	}

	// ⚠ 走到这里说明：信封 code=0（查询成功）但确实没有任何额度数据。
	// 这是**真实结论**（账号没有订阅套餐），不是"查不到"。
	// 报 HasData=true + 0 让界面显示 `0`，而不是会误导的 `—`。
	return gateway.QuotaView{Kind: "credits", Remaining: 0, HasData: true}, true
}

// numOr 取第一个能解成数字的值（对应参照实现的 jsonNum）。
//
// 为什么容忍字符串数字：上游不同版本把用量写成 `"1,234"` 或 `"1234"`。
// 参照实现为此专门去掉千分位逗号；我们照抄这条容忍度 ——
// 只认 JSON 数字会让那些版本静默取到 0。
func numOr(vals ...any) *float64 {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch x := v.(type) {
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				continue
			}
			n := x
			return &n
		case int:
			n := float64(x)
			return &n
		case int64:
			n := float64(x)
			return &n
		case json.Number:
			if f, err := x.Float64(); err == nil {
				return &f
			}
		case string:
			s := strings.ReplaceAll(strings.TrimSpace(x), ",", "")
			if s == "" {
				continue
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return &f
			}
		}
	}
	return nil
}

// firstNonEmptyStr 取第一个非空串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// quotaFromProbe API Key 通道没有额度端点，用探测判断可用性。
//
// 返回 HasData=false —— 因为**我们确实不知道**剩余额度。
// 这里刻意不把"探测通过"伪装成一个数字：那会是个编造的值。
func (p *Provider) quotaFromProbe(ctx context.Context, a *Auth) (gateway.QuotaView, bool) {
	if err := p.ProbeHealth(ctx, gateway.Credential{UID: a.UID, Secret: a}); err != nil {
		return gateway.QuotaView{HasData: false}, false
	}
	return gateway.QuotaView{HasData: false}, true
}

// ProbeHealth 主动健康检查。
//
// 本仓的"自愈机制"靠它提前发现坏号（每 10 分钟探冷却中的账号）。
// 判据刻意宽松：**只要不是鉴权失败就算健康** ——
// 额度耗尽（402/429）说明"这个号是好的，只是这阵子没额度"，
// 把它当不健康会让账号池在额度恢复后仍不敢用它。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := p.authOf(cred)
	if err != nil {
		return err
	}
	body := []byte(`{"model":"GLM-5.3","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`)
	resp, err := p.client.Chat(ctx, a, body, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 不读 body：探测只需要状态码，读它只是浪费带宽（而且可能很大）。
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("zcode: 鉴权失败（HTTP %d）—— 凭证可能已失效", resp.StatusCode)
	}
	return nil
}

// SoftRateReset 识别**业务码形态的限流**。
//
// # 为什么这个扩展点对本上游格外重要
//
// 本上游的失败可能是 **HTTP 200 + body 里的业务码**（见 zcode.go 的说明）。
// 出口层默认只看 HTTP 状态码，于是"风控 3012"会被记成成功 ——
// 那正是本仓修 TRAE 时踩过的同一个坑（"假成功"比失败更糟：
// 失败能被发现，假成功会一直骗到用户自己去核对）。
//
// 这里把业务码翻译成"这是一个限流，何时可重试"，
// 让核心的分类逻辑不必知道本上游的业务码表。
//
// 返回 ok=true 表示"我认得这个失败，它是限流"。
func (p *Provider) SoftRateReset(status int, body string) (time.Time, bool) {
	code := errCode([]byte(body))

	// ① 业务码层：风控 / 并发上限（实测 3012 会持续数分钟）
	switch {
	case isRiskControlCode(code):
		// 风控通常是出口 IP 或请求头形态问题，冷却几分钟让出口有机会换。
		// 5 分钟是**保守取大**：太短会反复撞同一堵墙，每次撞都算一次失败。
		return time.Now().Add(5 * time.Minute), true
	case code == codeConcurrency || code == codeRateLimited:
		// 并发/限流：短暂等待即可（本仓对软限流的约定是秒级）。
		return time.Now().Add(30 * time.Second), true
	case code == codeProviderOverloaded || code == codeNetworkRetry || code == codeServerError:
		return time.Now().Add(20 * time.Second), true
	}

	// ② HTTP 层：即使 body 里没有业务码，529 也是明确的"稍后重试"。
	//
	// 抄自参照实现：`529 is always retryable: the manager gateway answers
	// 529 {"overloaded"} after rotating its own DC proxy, expecting the
	// client to repeat`。
	if status == 529 {
		return time.Now().Add(30 * time.Second), true
	}
	if status == http.StatusTooManyRequests {
		return time.Now().Add(60 * time.Second), true
	}
	return time.Time{}, false
}

// IsTerminal 业务码是否属于"终止，不该重试"（供诊断端点与排障用）。
func (p *Provider) IsTerminal(status int, body string) bool {
	return isTerminalCode(errCode([]byte(body)))
}

// ImportCredentials 解析**粘贴的 API Key**。
//
// 支持的输入形态（尽量宽容 —— 用户会从各种地方复制）：
//
//	纯 Key：`abc123.def456`
//	带前缀：`zai:abc123.def456` / `bigmodel:abc123.def456`
//	JSON：  `{"api_key":"...","coding_plan":true}`
//	多行：  每行一个 Key
//
// # 为什么必须宽容
//
// 本仓其它上游的 import 都是"粘贴一段 JSON"，但 API Key 是**两段点分字符串**，
// 而且是从网页上复制的（常带前后空格、有时带 `Bearer ` 前缀）。
// 严格要求 JSON 会让这个功能基本没法用。
func (p *Provider) ImportCredentials(pasted string) ([]gateway.ImportedCredential, error) {
	text := strings.TrimSpace(pasted)
	if text == "" {
		return nil, fmt.Errorf("zcode: 粘贴内容为空")
	}
	// 整体是 JSON → 按 JSON 解析（可能是对象或数组）。
	if strings.HasPrefix(text, "{") {
		return p.importJSON(text)
	}
	if strings.HasPrefix(text, "[") {
		return p.importJSONArray(text)
	}
	// 否则按"每行一个 Key"处理。
	var out []gateway.ImportedCredential
	var errs []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ic, err := p.importOneKey(line)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		out = append(out, ic)
	}
	if len(out) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("zcode: 没有解析出任何可用凭证：%s", strings.Join(errs, "; "))
		}
		return nil, fmt.Errorf("zcode: 没有解析出任何可用凭证")
	}
	return out, nil
}

// importOneKey 解析一行 Key（可能带 `zai:` / `bigmodel:` 前缀或 `Bearer `）。
func (p *Provider) importOneKey(line string) (gateway.ImportedCredential, error) {
	raw := strings.TrimSpace(line)
	raw = strings.TrimPrefix(raw, "Bearer ")
	raw = strings.Trim(raw, `"'`)

	origin := ""
	codingPlan := false
	// 平台前缀（管理员明确指定平台，避免装配时的探测）。
	for _, tag := range []string{"zai", "bigmodel", "z.ai", "z-ai"} {
		if strings.HasPrefix(strings.ToLower(raw), tag+":") {
			origin = tag
			raw = strings.TrimSpace(raw[len(tag)+1:])
			break
		}
	}
	// Coding Plan 标记（`cp:` 前缀）—— 决定走哪个端点，见 Auth.CodingPlan。
	if strings.HasPrefix(strings.ToLower(raw), "cp:") {
		codingPlan = true
		raw = strings.TrimSpace(raw[3:])
	}
	if raw == "" {
		return gateway.ImportedCredential{}, fmt.Errorf("Key 为空")
	}
	a := &Auth{Kind: CredKindAPIKey, APIKey: raw, CodingPlan: codingPlan}
	if origin != "" {
		a.Origin = normalizeOrigin(origin)
	}
	return p.credentialFor(a)
}

// importJSON 解析单个 JSON 对象。
func (p *Provider) importJSON(text string) ([]gateway.ImportedCredential, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil, fmt.Errorf("zcode: JSON 解析失败: %w", err)
	}
	a, err := authFromMap(m)
	if err != nil {
		return nil, err
	}
	ic, err := p.credentialFor(a)
	if err != nil {
		return nil, err
	}
	return []gateway.ImportedCredential{ic}, nil
}

// importJSONArray 解析 JSON 数组。
func (p *Provider) importJSONArray(text string) ([]gateway.ImportedCredential, error) {
	var arr []map[string]any
	if err := json.Unmarshal([]byte(text), &arr); err != nil {
		return nil, fmt.Errorf("zcode: JSON 数组解析失败: %w", err)
	}
	var out []gateway.ImportedCredential
	var errs []string
	for i, m := range arr {
		a, err := authFromMap(m)
		if err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 条: %v", i+1, err))
			continue
		}
		ic, err := p.credentialFor(a)
		if err != nil {
			errs = append(errs, fmt.Sprintf("第 %d 条: %v", i+1, err))
			continue
		}
		out = append(out, ic)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("zcode: 数组里没有可用凭证：%s", strings.Join(errs, "; "))
	}
	return out, nil
}

// authFromMap 从 JSON 对象里构造 Auth。
func authFromMap(m map[string]any) (*Auth, error) {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	a := &Auth{
		Kind:          str("kind"),
		APIKey:        str("api_key", "apiKey", "key", "apikey"),
		JWT:           str("jwt", "token", "access_token"),
		RefreshToken:  str("refresh_token", "refreshToken"),
		UID:           str("uid", "user_id", "userId"),
		Nickname:      str("nickname", "name", "email"),
		DeviceMid:     str("device_mid", "deviceMid"),
		CaptchaRegion: str("captcha_region", "captchaRegion"),
	}
	if v, ok := m["coding_plan"]; ok {
		if b, ok := v.(bool); ok {
			a.CodingPlan = b
		}
	}
	if v, ok := m["expires_at"]; ok {
		switch t := v.(type) {
		case float64:
			a.ExpiresAt = int64(t)
		case string:
			a.ExpiresAt = parseUnixish(t)
		}
	}
	if origin := str("origin", "platform", "host"); origin != "" {
		a.Origin = normalizeOrigin(origin)
	}
	if !a.Usable() {
		return nil, fmt.Errorf("zcode: 缺少可用令牌（需要 api_key 或 jwt）")
	}
	a.ensureUID()
	return a, nil
}

// parseUnixish 解析"秒或毫秒"的时间戳字符串。
//
// 为什么要两种都认：用户从浏览器里复制的时间戳可能是毫秒。
// 判据是**量级**：大于 1e12 一定是毫秒（1e12 秒 = 公元 33658 年）。
func parseUnixish(s string) int64 {
	s = strings.TrimSpace(s)
	var n int64
	neg := false
	for i, r := range s {
		if i == 0 && r == '-' {
			neg = true
			continue
		}
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	if neg {
		return 0 // 负数时间戳没有意义
	}
	if n > 1_000_000_000_000 {
		return n / 1000
	}
	return n
}

// normalizeOrigin 把平台名归一成 origin URL。
func normalizeOrigin(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bigmodel", "bigmodel.cn", "open.bigmodel.cn", originBigModel:
		return originBigModel
	case "zai", "z.ai", "z-ai", originZAI:
		return originZAI
	}
	return ""
}

// credentialFor 把 Auth 变成一条可落盘的凭证。
func (p *Provider) credentialFor(a *Auth) (gateway.ImportedCredential, error) {
	a.ensureUID()
	if a.Kind == "" {
		a.Kind = CredKindAPIKey
	}
	raw, err := json.MarshalIndent(authFile{A: a}, "", "  ")
	if err != nil {
		return gateway.ImportedCredential{}, err
	}
	return gateway.ImportedCredential{
		FileName: "zcode-" + sanitizeUID(a.UID) + ".json",
		Raw:      raw,
		UID:      a.UID,
		Nickname: a.Nickname,
	}, nil
}
