// refreshratio.go 统一的「按寿命比例提前续期」策略。
//
// # 为什么要统一（用户提出的）
//
// 此前每个上游各自报一个**固定时长**窗口：
//
//	workbuddy  10 分钟
//	codearts   3 分钟
//	cline/raccoon/lobsterai/qoder  没报 → 核心兜底 10 分钟
//
// 固定时长的问题是：**它相对于凭证寿命的意义随寿命而变**。
//
//	1 小时寿命的 cline      10 分钟 = 已用 83% 才续（偏晚）
//	55 天寿命的 workbuddy   10 分钟 = 已用 99.99% 才续（几乎没有提前量）
//	2 小时寿命的 codearts    3 分钟 = 已用 97.5% 才续
//
// 同一句"提前 10 分钟"在这三种上游上差异巨大，而它们共享一个数字纯属巧合。
//
// 用户提出的比例判据一眼就对：**已用寿命 ≥ 50% 就该续期**。
// 它与寿命无关 —— 1 小时的 token 剩 30 分钟续、55 天的 token 剩 27 天续，
// 都是"用掉一半就换新"，语义一致。
//
// # 为什么必须有 iat（签发时刻）
//
// 「已用多少」需要两个点。`exp` 是过期时刻（大家都从 token 里读得到），
// 但**起点必须是 token 的签发时刻 `iat`**，不能用"我们拿到它的时刻"：
//
//	用「拿到时刻」当初点 → 已用时长恒为 0 → 50% **永远算不出来**
//	                    且每个进程重启都会重置，同一个 token 算出不同结论
//
// 签发时刻是 token 自己的属性，只有它能让"已用一半"这个说法成立。
//
// # 拿不到 iat 的上游怎么办
//
// 回落上游自报的固定窗口（或 `ok=false` 交给核心兜底）——
// codearts 就是这种：它的 securityToken 是华为云**不透明串**（非 JWT），
// 只有 expiresAt 没有签发时刻，所以继续用它自己的 3 分钟窗口。
package gateway

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// DefaultRefreshRatio 提前续期的**寿命占比阈值**：已用达到这个比例就续期。
//
// 0.5 = 用掉一半寿命。
const DefaultRefreshRatio = 0.5

// JWTTimes 从 JWT 里读出的两个时刻（Unix 秒；读不到为 0）。
type JWTTimes struct {
	// IssuedAt 签发时刻（`iat`）。0 = token 里没有这个字段。
	IssuedAt int64
	// ExpiresAt 过期时刻（`exp`）。0 = token 里没有这个字段。
	ExpiresAt int64
}

// ParseJWTTimes 读 JWT 的 `iat` / `exp`（不带签名校验 —— 我们只读本地时间戳，
// 不需要信任它）。
//
// # 宽容失败
//
// 入参可能不是 JWT（如 codearts 的 `securityToken` 是华为云不透明串，
// 只有 1 段）。任何解不开的情况都返回零值，**不报错** ——
// 调用方据此回落固定窗口，见文件头。
//
// # 会剥掉 Bearer 前缀
//
// cline 的 token 是 `workos:<jwt>` 形态（前缀**不可剥离**，剥了即 401）。
// 这里只用来读时间戳，所以先剥前缀再解 —— 不影响真正发出去的凭据。
func ParseJWTTimes(token string) JWTTimes {
	t := strings.TrimSpace(token)
	if t == "" {
		return JWTTimes{}
	}
	// `Bearer x` / `workos:x` / 任意 `scheme:x` 形态：取冒号后的部分。
	if i := strings.Index(t, ":"); i >= 0 && i < len(t)-1 {
		t = t[i+1:]
	}
	parts := strings.Split(t, ".")
	if len(parts) < 2 {
		return JWTTimes{} // 不是 JWT（不透明串）
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 宽容：有些实现带 padding。
		if raw, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return JWTTimes{}
		}
	}
	var payload struct {
		Iat float64 `json:"iat"`
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return JWTTimes{}
	}
	return JWTTimes{
		IssuedAt:  numToUnix(payload.Iat),
		ExpiresAt: numToUnix(payload.Exp),
	}
}

// numToUnix 把 JWT 里的时间戳规整成 Unix 秒：拒绝 NaN/Inf/非正，毫秒自动降级。
func numToUnix(v float64) int64 {
	if v <= 0 || v != v { // NaN 自比不相等
		return 0
	}
	n := int64(v)
	// 超过这个量级的只可能是毫秒（1e12 秒 = 公元 33658 年）。
	if n > 1_000_000_000_000 {
		return n / 1000
	}
	return n
}

// RefreshSkewAtRatio 按「已用寿命 ≥ ratio」算出提前续期窗口。
//
// 返回值语义（与 RefreshSkewExt 一致）：
//
//	(skew > 0, true)  距过期还有 skew 或更少时该续期
//	(0, true)         已过阈值，**现在就该续**（对齐"立即触发"的语义）
//	(0, false)        算不出来（缺 iat/exp）→ 调用方回落固定窗口
//
// # 算法
//
//	寿命 = exp - iat
//	阈值时刻 = iat + 寿命×ratio      （= "已用一半"的时刻）
//	窗口 = exp - 阈值时刻            （= 剩这么久时触发）
//
// ⚠ ratio 取 0.5 时窗口 = 寿命的一半 —— 于是"剩一半寿命时续期"。
//
// # 时间基准可注入（now）
//
// 便于测试固定时刻；生产调用传 `time.Now()`。
func RefreshSkewAtRatio(t JWTTimes, ratio float64, now time.Time) (time.Duration, bool) {
	if t.IssuedAt <= 0 || t.ExpiresAt <= 0 {
		return 0, false // 缺一半信息 → 交给调用方回落
	}
	lifetime := t.ExpiresAt - t.IssuedAt
	if lifetime <= 0 {
		// 签发晚于过期（时钟错乱/字段写反）：判不出比例，回落。
		// ⚠ 不返回 (0,true) —— 那会变成"永远该刷"，比不报窗口更糟。
		return 0, false
	}
	if ratio <= 0 || ratio >= 1 {
		ratio = DefaultRefreshRatio
	}
	// 触发时刻 = 签发时刻 + 寿命×ratio
	triggerAt := t.IssuedAt + int64(float64(lifetime)*ratio)
	window := t.ExpiresAt - triggerAt
	if window < 0 {
		window = 0
	}
	// 已经过了触发时刻 → 现在就续（0 表示"立即"）。
	if now.Unix() >= triggerAt {
		return 0, true
	}
	return time.Duration(window) * time.Second, true
}

// RefreshSkewFromToken 一步到位：从 token 串算提前续期窗口。
//
// 供上游一行接入：
//
//	func (p *Provider) RefreshSkew(cred Credential) (time.Duration, bool) {
//	    a, err := authOf(cred)
//	    if err != nil { return 0, false }
//	    return gateway.RefreshSkewFromToken(a.AccessToken)
//	}
func RefreshSkewFromToken(token string) (time.Duration, bool) {
	return RefreshSkewAtRatio(ParseJWTTimes(token), DefaultRefreshRatio, time.Now())
}
