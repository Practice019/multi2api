// Package authlimit 登录/注册的反滥用工具箱：滑窗限速、本地人机校验、匿名
// 端点请求体上限 —— 三层各自独立，调用方装哪层就装哪层。
//
// # 抄自哪里（new-api）
//
// 出处：viking://resources/repos/QuantumNous/new-api（下同）
//
//	关键路径限速  middleware/rate-limit.go 的 CriticalRateLimit()；参数在
//	              common/constants.go:230-232：CriticalRateLimitNum = 20、
//	              CriticalRateLimitDuration = 20 * 60（20 次 / 20 分钟）
//	挂在哪        router/api-router.go:79-81 —— /register 与 /login 都是
//	              CriticalRateLimit + anonymousRequestBodyLimit + TurnstileCheck
//	              三层叠着上，本包就是这三层的对应物
//	人机校验      middleware/turnstile-check.go：那里是 POST 到 Cloudflare
//	              siteverify。本仓是单机自用网关，没有 Turnstile 的 site key /
//	              secret key，也不该为了一个注册页引外部依赖，所以换成
//	              零依赖的自绘 SVG 算术题（见 captcha.go）
//	体上限        middleware/request_body_limit.go 的 AnonymousRequestBodyLimit()
//
// # 为什么单独一个包（而不是塞进 internal/account）
//
// 这三层不是"人"或"登录态"的语义（internal/account 管的是这两样）。混在
// 一起会让"改口令"和"防爆破"的判据互相纠缠，也很难在不引 bcrypt 与落盘的
// 条件下单测。本包零依赖：只用标准库，全部状态在内存里。
package authlimit

import (
	"strings"
	"sync"
	"time"
)

// Limiter 滑动窗口限速器：窗口 window 内最多放行 max 次。
//
// # 为什么是滑动窗口而不是固定窗口
//
// new-api 的内存限速器与 Redis 版都是**固定窗口**
// （middleware/rate-limit.go 的 inMemoryRateLimiter 与 redisFixedWindowScript），
// 它自己的注释就写着后果："traffic at a window boundary can burst up to twice
// the configured limit"。这里是登录/注册的闸门，边界突刺会把有效速率直接翻倍，
// 不值得为省几个 time.Time 留这个口子。固定窗口唯一的好处是 Redis 里一条 Lua
// 就能原子化，而我们是单机进程内，一把锁就够。
//
// ⚠ 只增不减的表会一直长（每个打过来的 IP 一行），所以有 Prune；调用方应当按
// 固定节拍（比如每 5 分钟）调一次 —— 见 Prune 的注释。
type Limiter struct {
	window time.Duration
	max    int

	mu sync.Mutex
	// hits：key → 最近 window 内的放行时刻，天然按时间升序（只 append）。
	hits map[string][]time.Time
}

// NewLimiter 造一个限速器。window <= 0 或 max <= 0 时它**拒绝一切**（fail-closed），
// 理由见 Allow。
func NewLimiter(window time.Duration, max int) *Limiter {
	return &Limiter{
		window: window,
		max:    max,
		hits:   make(map[string][]time.Time),
	}
}

// Allow 判断这次请求能否放行；放行时**顺带记一次**，拒绝时给出还要等多久。
//
// 返回值：
//
//	ok          true = 放行，false = 拒绝
//	retryAfter  拒绝时才有意义：从 now 起还要等多久，最旧的那次放行才滑出窗口。
//	            调用方按 HTTP 语义向上取整成整数秒填进 Retry-After
//	            （new-api 的 writeRateLimited 也是这么做的）。放行时返回 0。
//
// # 空 key 一律拒绝（这是本文件里最要紧的一条取舍）
//
// 取不到来源（IP 空、用户名空）时有两条看似"温和"的路，两条都是错的：
//
//	① 退化成共用窗口（把 key 归一到某个常量）：一个刷子会把**所有人**
//	   一起挡在门外 —— 攻击者拿到了一个把正常用户全踢下线的开关。
//	② 空 key 就放行：等于给攻击者一条免检通道，只要设法让 key 为空
//	   （伪造出取不到 IP 的请求形态）就能无限刷。
//
// 所以空 key（含纯空白）一律拒绝。取不到来源时宁可不放行，也不要把这一层
// 静默降级成"没装"。此时 retryAfter 给**窗口长度**这个保守上界 —— 空 key
// 没有"最旧的一次"可算，而 new-api 的内存限速器同样只能给保守上界
// （见 writeRateLimited 的注释："callers without a TTL pass the full window
// duration as a conservative upper bound"）。
//
// # 参数没给对时同样拒绝
//
// window <= 0 或 max <= 0 一律拒绝，而不是当成"不限"。这与本仓 apikey 的
// `rpm == 0 表示不限速`（internal/apikey/apikey.go）不同，但那里的 0 是**用户
// 在界面上显式要的东西**；这里的 0 只会来自装配时漏传参数，而"漏传 = 不限"
// 意味着这一层悄悄不生效、没有任何告警。拒绝是立刻看得见的故障（人人都 429，
// 报到我们这里），静默放行则是被刷了一夜才发现。
func (l *Limiter) Allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	if l == nil {
		return false, 0
	}
	key = strings.TrimSpace(key)

	l.mu.Lock()
	defer l.mu.Unlock()
	if key == "" || l.max <= 0 || l.window <= 0 {
		return false, l.window
	}

	hits := l.trimLocked(key, now)
	if len(hits) >= l.max {
		// 最旧那一次滑出窗口后窗口内就只剩 max-1 次，下一次必然放行。
		retry := hits[0].Add(l.window).Sub(now)
		if retry < 0 {
			retry = 0
		}
		// ⚠ 被拒的这一**不**记录：否则"失败尝试"会把窗口一直往后推，
		// 一个持续刷的客户端就永远等不到恢复。
		return false, retry
	}
	l.hits[key] = append(hits, now)
	return true, 0
}

// Reset 清掉一个 key 的窗口（改口令成功、管理员解除封禁之后调）。
//
// 空 key 是 no-op：Allow 从不给空 key 记账，没什么可清的。
func (l *Limiter) Reset(key string) {
	if l == nil {
		return
	}
	key = strings.TrimSpace(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

// Prune 清掉所有"窗口内一次放行都没有"的 key，返回清掉几个。
//
// ⚠ 这不是可选优化而是必需项：hits 的键来自外部输入（每个打过来的 IP 一行），
// 不清理就是个只涨不缩的泄漏面，慢速扫段能把它撑到内存耗尽。清的理由是"这个
// key 现在已经等于没有记录了"——它的语义没变，只是表变短。
//
// 不清整表的理由：那些**还在窗口内**的 key 正是限速的依据，清掉等于把正在
// 被限速的刷子当场放行。
func (l *Limiter) Prune(now time.Time) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	for key := range l.hits {
		if len(l.trimLocked(key, now)) == 0 {
			removed++
		}
	}
	return removed
}

// Count 窗口内该 key 的放行次数（不记账，只看）。
//
// 顺带把窗口外的时刻清掉 —— 反正锁已经拿着了，多做的这一步零成本，还省得
// 探针式查询把过期数据一直养着。所以它既是观察也是清理。
func (l *Limiter) Count(key string, now time.Time) int {
	if l == nil {
		return 0
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.trimLocked(key, now))
}

// trimLocked 丢掉窗口外的时刻并返回窗口内的部分；全空了就把这个 key 整条删掉
// （于是 Prune 只要看返回值是不是空即可）。调用方必须持锁。
//
// 写法与 internal/apikey 的 rateOKLocked 同款：借底层数组原地过滤，
// 高频 key 上零分配。
//
// 边界口径：正好落在 now-window 上的那次放行**算窗口外**（用 After 而不是
// !Before），于是"窗口滑过后恢复"在 t+window 这一瞬间就成立，调用方不必猜
// 半个窗口的误差。
func (l *Limiter) trimLocked(key string, now time.Time) []time.Time {
	kept := l.hits[key][:0]
	cutoff := now.Add(-l.window)
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}
