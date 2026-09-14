// softrate.go 429 code=6004（**模型级**限流）的识别与重置时刻解析。
//
// # 为什么需要它（借鉴 workbuddy2api-panel）
//
// 上游的 429 有两种完全不同的含义：
//
//	账号级限流  该账号此刻被限 → 只能等冷却
//	模型级限流  该**模型**的使用量超限（code 6004，msg 带「将在 … 重置」）
//	            → 账号是健康的，换一个模型立刻可用
//
// 改造前两者被当成同一件事（都按账号级软冷却处理），后果是：
//
//	账号因 gpt-5.5 被限流 → 请求 glm-5.2 也选不到它
//	→ 明明还有可用的模型额度，却表现为"这个号废了"
//
// 单账号部署下这更严重：整个网关对该账号的所有请求都被拒，
// 而用户只是想换个模型。
//
// # 收窄的两个点（不是"推翻重做"）
//
//	① **冷却截止**：上游明说"将在 <时刻> 重置"时，直接取那个墙钟时刻，
//	   不再用"基数 × 指数退避"去猜 —— 重置时间已是权威答案。
//	② **豁免范围**：记录触发模型，换模型请求时该账号视为可用。
//
// 无「将在 … 重置」文案的 6004，以及一切非 6004 的软限流，
// **完全退回改造前的行为**（softRate 基数 + 指数退避 + 封顶）。
package upstream

import (
	"regexp"
	"strings"
	"time"
)

// modelRateLimitCode 明确指向"模型级 429 限流"的业务 code。
//
// 上游用它表达"该模型的使用量超限"，而不是账号整体被限流。
const modelRateLimitCode = "6004"

// softRateResetLoc 上游 429 文案里重置时间的固定时区（UTC+8）。
//
// ⚠ 固定 +08:00 而不是 now.Location()：上游的文案就是按这个时区写的，
// 与容器/宿主机时区**无关**。用本地时区解释会让解析出来的时刻
// 在 UTC 容器里整整偏 8 小时 —— 冷却要么提前结束（继续撞 429），
// 要么多冷 8 小时（账号白白闲置）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc 暴露重置时间的固定时区（供测试构造/断言同一口径）。
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// softRateResetRe 匹配「将在 … 重置」，捕获中间的时间串。
//
// 用 (.+?) 非贪婪：文案后面可能还有别的内容，贪婪匹配会把它们一起吞进来
// 导致 time.Parse 失败（那会让"能解析的文案"退化成"永远退回指数退避"）。
var softRateResetRe = regexp.MustCompile(`将在 (.+?) 重置`)

// softRateTimeLayout 上游重置时间的格式（无时区后缀；时区固定 UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// modelRateLimitRe 匹配体里的 `"code":6004`。
//
// # 容差
//
// JSON 里可能写成 `"code":6004` / `"code": 6004` / `"code":"6004"`
// —— 上游不同端点的序列化不一致，只认一种会漏掉一部分。
//
// # ⚠ 数字边界是必需的（不能用 `"?6004"?`）
//
// 朴素写法 `"?` + 6004 + `"?`（两侧引号都可选）会把 **`"code":60040`**
// 也匹配上 —— `"?` 可以为空，于是 `6004` 前缀匹配 `60040`。
// 其他以 6004 开头的业务码（60040/60041/…）会被误判成模型级限流，
// 后果是账号被错误豁免 + 冷却被收窄，继续撞真实限流。
//
// 所以数字分支必须带边界 `[^0-9]` 或串尾。Go 的 RE2 不支持 lookahead，
// 因此用显式的"非数字或结尾"替代 `(?!\d)`。
//
// 这个 bug 是本文件的测试抓出来的（TestIsModelRateLimit 的 `60040` 用例）。
var modelRateLimitRe = regexp.MustCompile(`"code"\s*:\s*(?:"6004"|6004(?:[^0-9]|$))`)

// IsModelRateLimit 报告 body 是否明确指向模型级限流（业务 code 6004）。
func IsModelRateLimit(body string) bool {
	return modelRateLimitRe.MatchString(body)
}

// ParseSoftRateReset 从 429 body 解析「将在 … 重置」时刻。
//
// 成功返回**墙钟时刻**（按 UTC+8 解释），失败返回零值 + false。
//
// # 为什么先判 IsModelRateLimit
//
// 非模型级限流（非 6004）即使带"重置"字样也不返回 —— 那种文案
// （如通用限流提示）的重置时间**没有冷却语义**，解析出来反而会
// 错误地把冷却收窄（把一个本该账号级冷却的问题当成模型级豁免掉）。
func ParseSoftRateReset(body string) (time.Time, bool) {
	if !IsModelRateLimit(body) {
		return time.Time{}, false
	}
	m := softRateResetRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	// 文案有时带 " UTC+8" 后缀（时区已固定，后缀去掉再解析）。
	ts = strings.TrimSuffix(ts, " UTC+8")
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
