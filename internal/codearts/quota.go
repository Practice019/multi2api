package codearts

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// 模型额度状态探测。
//
// 为什么需要：benefit 通道（免费额度）会**独立于积分**耗尽，
// 而订阅接口只报积分，看不出免费额度的余量。
// 表现是：四个默认通道模型正常，三个 benefit 模型全部
// `InferHub.4291.200 insufficient quota`，但账号本身完全健康。
//
// 症状极具误导性 —— 用户看到"积分还剩 5995"会以为额度没耗，
// 于是反复排查参数/账号/网络。因此把额度状态**探测出来并标记**，
// 让 /v1/models 直接告诉客户端"这个模型当前不可用"，一眼就能换模型。
//
// 识别依据（实测的完整错误体）：
//
//	HTTP 200 + data:{"error_code":"InferHub.4291.200",
//	                 "error_msg":"insufficient quota",
//	                 "details":[{"error_msg":"modelId: glm-5.3-flash"}, ...]}
//
// 注意它**不是** HTTP 错误码，而是塞在 SSE 流里的业务错误。

// QuotaState 是单模型的额度状态。
type QuotaState struct {
	// Exhausted 为 true 表示探测确认额度不足（当前不可用）。
	Exhausted bool `json:"exhausted"`
	// ExhaustedUntil 该标记的失效时刻（**下一个本地日零点**）。
	//
	// # 为什么必须有到期时间（这是"每日额度"这个事实的落点）
	//
	// benefit 通道是**每日**免费额度，耗尽后次日重置。只写
	// `Exhausted=true` 而没有到期时间会引出两个后果：
	//
	//  1. 模型被**永久**当成不可用（其实第二天就回来了）；
	//  2. 更糟的是死锁 —— 一旦据此把模型从 /v1/models 摘掉，
	//     就再也没有请求去触发 ClearQuota，"越藏越久"。
	//
	// 把"每日"写进数据里，自愈就是**必然**的，不依赖任何后台任务。
	// 零值表示"无到期时间"（旧数据/测试构造），此时按永不过期处理。
	ExhaustedUntil time.Time `json:"exhausted_until,omitempty"`
	// Reason 是人类可读的原因（如 "insufficient quota"）。
	Reason string `json:"reason,omitempty"`
	// CheckedAt 最近一次探测时间。
	CheckedAt time.Time `json:"checked_at"`
}

// nextLocalMidnight 返回下一个本地日零点。
//
// 用本地时区而不是 UTC：上游的"每日"是按用户所在时区的自然日重置的，
// 用 UTC 会在错误的时间点提前恢复（东八区会提前 8 小时）。
func nextLocalMidnight() time.Time {
	now := time.Now()
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
}

// quotaProbe 探测结果缓存。
//
// 为什么不每次请求都探：探测本身要发一次真实上游调用（消耗额度、
// 占并发名额），只能低频做。这里用 TTL 缓存，且只在管理台读取时触发。
type quotaProbe struct {
	mu     sync.Mutex
	states map[string]QuotaState
	// inFlight 防止并发重复探测同一个模型。
	inFlight map[string]bool
	ttl      time.Duration
}

var quotaCache = &quotaProbe{
	states:   map[string]QuotaState{},
	inFlight: map[string]bool{},
	ttl:      10 * time.Minute,
}

// QuotaTTL 返回探测结果的有效期。
func QuotaTTL() time.Duration {
	quotaCache.mu.Lock()
	defer quotaCache.mu.Unlock()
	return quotaCache.ttl
}

// SetQuotaTTL 调整探测有效期（测试用）。
func SetQuotaTTL(d time.Duration) {
	quotaCache.mu.Lock()
	quotaCache.ttl = d
	quotaCache.mu.Unlock()
}

// QuotaStates 返回当前缓存的额度状态（不发探测）。
//
// stale=false 表示条目仍在 TTL 内，可直接展示；
// stale=true 表示结果可能过期，调用方应触发 ProbeQuota 刷新。
func QuotaStates() (states map[string]QuotaState, stale bool) {
	quotaCache.mu.Lock()
	defer quotaCache.mu.Unlock()

	out := make(map[string]QuotaState, len(quotaCache.states))
	stale = true
	now := time.Now()
	for k, v := range quotaCache.states {
		out[k] = v
		if now.Sub(v.CheckedAt) <= quotaCache.ttl {
			stale = false
		}
	}
	return out, stale
}

// ProbeQuota 对指定模型发一次最小请求，判断额度是否耗尽。
//
// 只在成功识别为"额度不足"时标记 Exhausted；其他任何错误（网络、
// 并发上限、参数问题）都**不**标记 —— 那些是暂时性或无关的，
// 误标会让用户以为模型不可用而错过它。
func (c *Client) ProbeQuota(a *Auth, model string, benefit bool) QuotaState {
	quotaCache.mu.Lock()
	if s, ok := quotaCache.states[model]; ok && time.Since(s.CheckedAt) < quotaCache.ttl {
		quotaCache.mu.Unlock()
		return s
	}
	if quotaCache.inFlight[model] {
		// 已有探测在跑，返回上次结果（可能是零值）避免重复消耗额度
		s := quotaCache.states[model]
		quotaCache.mu.Unlock()
		return s
	}
	quotaCache.inFlight[model] = true
	quotaCache.mu.Unlock()

	defer func() {
		quotaCache.mu.Lock()
		delete(quotaCache.inFlight, model)
		quotaCache.mu.Unlock()
	}()

	st := QuotaState{CheckedAt: time.Now()}

	var extra map[string]string
	if benefit {
		extra = map[string]string{"maas_type": "benefit"}
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		"stream":     true,
		"max_tokens": 1, // 只要一个 token：探测不该明显消耗额度
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = ctx

	rc, status, rb, err := c.ChatStreamWith(a, body, extra)
	if err != nil {
		// 传输层错误不标记：与额度无关
		st.Reason = ""
		quotaCache.mu.Lock()
		quotaCache.states[model] = st
		quotaCache.mu.Unlock()
		return st
	}

	var raw []byte
	if status >= 400 {
		raw = rb
	} else {
		raw, _ = io.ReadAll(io.LimitReader(rc, 4096))
		rc.Close()
	}

	if reason, ok := DetectQuotaExhausted(string(raw)); ok {
		st.Exhausted = true
		// 与 MarkQuotaExhausted 同款到期语义：探测出来的额度不足同样是
		// **每日**额度，次日应自动恢复可用。
		st.ExhaustedUntil = nextLocalMidnight()
		st.Reason = reason
		log.Printf("codearts: 模型 %s 额度不足（%s，%s 后自动恢复）",
			model, reason, time.Until(st.ExhaustedUntil).Round(time.Minute))
	}

	quotaCache.mu.Lock()
	quotaCache.states[model] = st
	quotaCache.mu.Unlock()
	return st
}

// detectQuotaExhausted 判断一份响应体是否是"额度不足"。
//
// 识别两种形态：
//  1. InferHub.4291.200 + insufficient quota（benefit 免费额度耗尽）
//  2. 余额/积分类硬错误（复用 Classify 的 hardMarkers 口径）
//
// 导出给测试与 handler 复用（handler 在真实请求失败时也可就地标记，
// 这样用户不需要等下一次探测）。
func DetectQuotaExhausted(body string) (reason string, ok bool) {
	lower := strings.ToLower(body)

	// benefit 免费额度耗尽：这只在流内业务错误里出现
	if strings.Contains(body, "InferHub.4291") ||
		strings.Contains(lower, "insufficient quota") ||
		strings.Contains(body, "额度不足") {
		return "insufficient quota（免费额度已耗尽）", true
	}
	if strings.Contains(lower, "insufficient balance") ||
		strings.Contains(body, "余额不足") {
		return "insufficient balance（余额不足）", true
	}
	return "", false
}

// MarkQuotaExhausted 就地把某模型标记为额度不足。
//
// 用途：正常请求路径撞到额度错误时立刻标记，省掉一次探测往返，
// 也让管理台在不额外发请求的前提下立刻反映真实状态。
//
// 标记带**到期时间**（下一个本地日零点）：benefit 是每日额度，
// 到期即自动失效，不依赖任何后台任务清理（见 QuotaState.ExhaustedUntil）。
func MarkQuotaExhausted(model, reason string) {
	if model == "" {
		return
	}
	quotaCache.mu.Lock()
	quotaCache.states[model] = QuotaState{
		Exhausted:      true,
		ExhaustedUntil: nextLocalMidnight(),
		Reason:         reason,
		CheckedAt:      time.Now(),
	}
	quotaCache.mu.Unlock()
}

// IsQuotaExhausted 报告某模型**当前**是否处于额度耗尽状态。
//
// 与直接读 QuotaStates()[model].Exhausted 的区别：本函数会处理**日切过期** ——
// 超过 ExhaustedUntil 的标记视为已失效（每日额度已重置），
// 调用方据此就能自愈，不需要任何显式清理。
//
// 为什么把过期判断放在读取侧而不是靠后台定时删除：
// 读取侧判断是**无状态**的，不会因为进程重启/后台任务漏跑而失效；
// 定时删除则会留下"任务没跑 → 模型一直是隐藏的"这种静默故障。
func IsQuotaExhausted(model string) bool {
	quotaCache.mu.Lock()
	defer quotaCache.mu.Unlock()
	s, ok := quotaCache.states[model]
	if !ok || !s.Exhausted {
		return false
	}
	if !s.ExhaustedUntil.IsZero() && !time.Now().Before(s.ExhaustedUntil) {
		// 已过到期时刻：按每日重置处理，标记自然失效。
		return false
	}
	return true
}

// ClearQuota 清除某模型的额度标记（成功调用后调用）。
//
// 为什么成功要清除：额度可能是周期性恢复的（如每月重置），
// 一旦某次调用成功就说明它现在可用，旧标记必须失效。
func ClearQuota(model string) {
	quotaCache.mu.Lock()
	defer quotaCache.mu.Unlock()
	if s, ok := quotaCache.states[model]; ok && s.Exhausted {
		delete(quotaCache.states, model)
		log.Printf("codearts: 模型 %s 额度已恢复，清除标记", model)
	}
}

// ProbeAllQuota 逐个探测所有已注册模型的额度状态。
//
// 串行执行：账号有并发上限（3），并发探测会互相挤掉名额，
// 反而让探测结果失真（拿到"并发上限"而不是真实额度状态）。
func (c *Client) ProbeAllQuota(a *Auth) map[string]QuotaState {
	out := map[string]QuotaState{}
	for _, m := range knownModels {
		// ⚠ 不按 Verified 过滤：Verified 表示"该模型在本账号上确实存在"，
		// 与"此刻额度是否耗尽"是**两件不同的事**。额度是按日恢复的临时状态，
		// 用 Verified 过滤会让"今天额度用完 → 明天不再探测 → 永远发现不了它已恢复"。
		// 探测本身以最小请求（max_tokens=1）做，成本可忽略。
		out[m.ID] = c.ProbeQuota(a, m.ID, m.Channel == ChannelBenefit)
		// 留间隔：探测本身占一个会话名额，太密会撞并发上限
		time.Sleep(500 * time.Millisecond)
	}
	return out
}

// quotaTrackingBody 包装上游响应流，在**真实请求路径**上就地维护模型额度状态。
//
// # 为什么必须在这里（这是文档写了、却一直没接的那条线）
//
// quota.go 的包注释说这套机制的目的是"让 /v1/models 直接告诉客户端
// 这个模型当前不可用"，DetectQuotaExhausted 的注释也说"导出给测试与
// handler 复用（handler 在真实请求失败时也可就地标记，这样用户不需要等
// 下一次探测）"。但改造前**没有任何生产调用方**：
//
//	MarkQuotaExhausted  只被测试调用
//	Provider.Models()   从不读 quotaCache
//
// 于是"额度耗尽"只有人工点管理台的「额度探测」才会被发现 ——
// 而用户遇到的是"三个 benefit 模型恒失败，但界面显示积分还剩很多"，
// 完全无从判断该换模型。
//
// # 为什么不做在 errorclassifier.go
//
// 那条接缝是**纯函数**契约（见 gateway.ErrorClassifier 的实现约束：
// 不发网络、不改共享状态），且它的签名里没有"是哪个模型"。
//
// # 为什么放在本包
//
// 本包同时握有**模型名**（请求体）与**响应流**，且不需要核心认识 codearts
// 的任何细节（架构判据 3）。
type quotaTrackingBody struct {
	io.ReadCloser
	model string
	// buf 累积流的前若干字节用于识别；达到上限后不再累积。
	buf []byte
	// done 表示已得出结论（标记或清除），后续不再扫描。
	done bool
}

// quotaScanLimit 是扫描窗口大小。
//
// 额度错误是**唯一**一帧（实测整段响应只有那一个 error 信封，几百字节），
// 而正常回复的第一帧就带 "choices"。所以 8KB 远超所需，
// 又不至于在长回复上累积内存。
const quotaScanLimit = 8 << 10

func (b *quotaTrackingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && !b.done {
		if room := quotaScanLimit - len(b.buf); room > 0 {
			take := n
			if take > room {
				take = room
			}
			b.buf = append(b.buf, p[:take]...)
		}
		body := string(b.buf)

		if reason, ok := DetectQuotaExhausted(body); ok {
			// 额度耗尽：就地标记（带日切到期），后续 /v1/models 立刻反映。
			MarkQuotaExhausted(b.model, reason)
			log.Printf("codearts: 请求路径撞到额度耗尽，标记模型 %s 不可用（%s）", b.model, reason)
			b.done = true
		} else if strings.Contains(body, `"choices"`) {
			// 已经出现正常数据帧 → 本次调用与额度无关，清除旧标记。
			// 这就是**自愈点**：额度次日恢复后，第一条成功请求即解除隐藏。
			ClearQuota(b.model)
			b.done = true
		}
	}
	return n, err
}
