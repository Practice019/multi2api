// Package logbuf 进程内请求日志环形缓冲。
//
// 动机：网关原本只把「每请求一行表格日志」printf 到 stdout，进程内没有任何留存，
// 控制台无法回看。这里把同一份数据同时写进定长环形缓冲，供 /admin/logs 增量拉取。
//
// 设计约束：写路径必须极轻（每个 chat 请求都会走），因此只做一次 mutex 保护的
// 定长切片写入，不做 IO、不做 JSON 序列化；序列化交给读侧。
package logbuf

import (
	"fmt"
	"sync"
	"time"
)

// DefaultCapacity 默认保留条数。2000 条足以覆盖最近几小时的排障窗口。
const DefaultCapacity = 2000

// Entry 一条请求日志（字段刻意扁平，便于直接 JSON 出去）。
//
// 关于 usage 派生字段（Credit/ThinkTokens/CacheHitTokens/CacheMissTokens）：
//   - 它们全部来自上游 usage 对象，缺失/不可解析时一律写 0，**不使用 -1 哨兵**。
//     只有 Tokens 保留 -1=「usage 缺失」的语义，新字段靠「Tokens<0 ⇒ 全为 0」
//     间接表达缺失，避免每个字段各自引入一套哨兵值。
//   - 历史行（写入本结构体新字段之前落盘的 ~1900 行）没有这些 JSON 键，
//     反序列化后即为 Go 零值 0，读侧聚合无需做兼容分支。
//   - 这些字段是按请求累加的量（credit 是消费额度、其余是 token 计数），
//     永远不会是负数；解析层已保证非负。
type Entry struct {
	// OutTokens 纯**输出** token（2026-10-20 加）：tok/s 的正确分子。
	//
	// ⚠ 为什么不复用 Tokens：Tokens 是 total（输入+输出），拿它算 tok/s 会把输入
	// 当成生成量。实测一条 2408ms 的请求显示 15759 t/s（37948/2.4s）——
	// 数学没错，物理不可能。两个语义必须分开存。
	// ⚠ 0 = 上游没给 completion_tokens 或旧版本没记录（历史行全为 0），
	// 前端据此显示 "—"，绝不拿它算速度（那会把"没有数据"变成"速度很快"）。
	OutTokens int       `json:"out_tokens,omitempty"`
	Seq       int64     `json:"seq"`
	At        time.Time `json:"at"`
	Model     string    `json:"model"`
	Mode      string    `json:"mode"`   // stream | sync
	Status    int       `json:"status"` // 对客户端返回的状态码
	UID       string    `json:"uid"`    // 完整 uid，展示层自行截断
	// Provider 本次请求走的上游标识（与 gateway.Provider.ID() 对应）。
	//
	// 加它只花一个字段：统计按上游分组（/admin/stats 的 by_provider）是
	// one-api 社区的实证教训（"先把观测建好"）。不带这一维时，
	// 两个上游的调用次数与消耗混在一个数里，面板无法归因。
	//
	// 向后兼容：历史日志行没有这个键，反序列化得空串，
	// 读侧按"未标注"处理即可，不需要兼容分支。
	Provider string `json:"provider,omitempty"`
	TTFBMS   int64  `json:"ttfb_ms"`
	Tokens   int    `json:"tokens"` // -1 表示 usage 缺失
	TotalMS  int64  `json:"total_ms"`

	Credit          float64 `json:"credit"`            // 本次请求实际消耗的额度
	ThinkTokens     int     `json:"think_tokens"`      // 推理（思维链）token 数
	CacheHitTokens  int     `json:"cache_hit_tokens"`  // 命中缓存的 prompt token
	CacheMissTokens int     `json:"cache_miss_tokens"` // 未命中缓存的 prompt token
	// Decision 本请求的**重试/换号判决留痕**，形如 `reason:source`（空 = 成功，不出现在日志里）。
	//
	// ⚠ 抄自 new-api 的 `PolicyDecision{Action, Reason, Source}`（service/request_policy.go:19，
	//   前端类型见 web/src/features/system-settings/request-policies/api.ts:7）——
	//   它把每次"重试 / 不重试"的判决**连同原因**记进请求日志。
	//
	// ⚠ 我们的取舍（为什么只存 `reason:source` 两个而不是三个字段）：
	//   · `Action`（retry / stop）**已经由 `Status` 体现**了 —— 重试成功就是 200、
	//     预算用尽就是 502/503 ⇒ 再存一份是冗余；
	//   · 而 `reason` 与 `source` 是**行为里看不出来的**（为什么重试、是谁决定的）
	//     ⇒ 那才是排障时唯一需要而当前缺失的信息。
	// ⚠ 加它的直接动机：2026-10-18 排 503 时，日志里只有"503"这一个事实，
	//   我为此**反复猜了四轮**（额度 / 容量 / 重试策略全都猜错）——
	//   有这一列的话，第一轮就能看到"upstream_5xx"还是"pool_unavailable"。
	Decision string `json:"decision,omitempty"`
	// PreMs 从"请求进入网关"到"真正发出上游请求"之间花掉的毫秒（2026-10-18 加）。
	//
	// ⚠ 为什么必须单独量它（用户原话："根本就不是号级速度的问题，而是后端代码调用有 bug"）：
	// `ttfb_ms` 含**两段** —— 我们这边（选号 / 取凭证 / 可能刷凭证 / 转换请求体）的耗时，
	// 加上上游首包耗时。**不拆开就无法回答"15 秒的首字里我们占了多少"**。
	//
	// ⚠ 判读方式（这是加它的全部目的）：
	//	pre_ms 很小（几十毫秒）   ⇒ 慢在上游，不是网关的 bug
	//	pre_ms 很大（几百毫秒以上）⇒ 网关自己花了时间，那才是该修的地方
	// ⚠ 对"每次新会话的第一条"尤其要看这一列：它的 prompt 最短、本该最快。
	PreMs int64 `json:"pre_ms,omitempty"`
	// ClientIP / ClientUA 请求来源（2026-10-18 加）。
	//
	// ⚠ 加它的唯一原因：实测发现有 6256 条非流式请求（98% 用 loomy/DeepSeek-V4.1-Flash）
	// **不经过 new-api**（那边 24 小时只见过 1 条非流式）⇒ 说明有**另一个客户端直连网关**。
	// 而日志里没有来源信息 ⇒ 只能靠模式猜"是谁在非流式调用"。加这两列之后**一次就能点名**。
	//
	// ⚠ 为什么这件事重要：非流式请求必须等整段生成完才返回（实测 11~134 秒），
	// 而流式的首字只要数百毫秒 ⇒ "第一条很慢"的体感很可能就是它的。
	// 但要改掉它，先得知道是谁 —— 这就是这两列存在的理由。
	ClientIP string `json:"client_ip,omitempty"`
	ClientUA string `json:"client_ua,omitempty"`
	// Channel / UpstreamModel 这条请求走的是哪条**渠道**、以及被改写成哪个上游模型名（2026-10-20 加）。
	//
	// ⚠ 加它的直接动机（用户原话："而且这个有问题我用的是csg呢不是LOOMY"）：
	//   他发的是渠道公开名 csg/deepseek-v4.1-flash-1017，但日志里只有 model 与 provider 两列，
	//   而 provider 是账号池路径的概念（provider=loomy）—— 于是**无法证明请求到底走了渠道
	//   还是回落到了账号池**。他看到的那些 loomy 记录其实是另一个 Python 客户端（client_ua
	//   写着 AsyncOpenAI）的流量，他自己的 csg 请求在这份日志里一条都没有
	//   —— 因为渠道分支当时**根本没写日志**。
	//
	// ⚠ 这两列是排障的关键分界：同样一个 200，走渠道和走账号池是两条完全不同的路
	//   （不同的 key、不同的计费口径、不同的上游），日志里必须能分开。
	//   空 = 本次没走渠道（账号池路径，或渠道未命中而回落）。
	Channel       string `json:"channel,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
}

// Ring 定长环形缓冲。零值不可用，必须经 New 构造。
type Ring struct {
	mu    sync.Mutex
	buf   []Entry
	next  int
	full  bool
	total int64 // 累计写入条数，同时充当递增序列号

	// sink 可选的落盘目标（nil = 只留在内存）。写盘刻意放在锁外：
	// 文件 IO 不能拖着所有请求一起等。
	sink *Sink
}

// New 构造容量为 capacity 的环形缓冲；capacity<=0 时取 DefaultCapacity。
func New(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Ring{buf: make([]Entry, capacity)}
}

// SetSink 挂上落盘目标（可在启动后调用）。
func (r *Ring) SetSink(s *Sink) {
	r.mu.Lock()
	r.sink = s
	r.mu.Unlock()
}

// Sink 返回当前落盘目标（可能为 nil）。
func (r *Ring) Sink() *Sink {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sink
}

// SeedSeq 用落盘日志里的最大 seq 播种计数器，让序号跨重启继续增长。
//
// 为什么需要它：Push 会用 Ring 自己的 total 覆盖 Entry.Seq，而 total 是进程级的、
// 重启归零。不播种的话，同一个落盘文件里会出现重复 seq ——
// 「序号依次变大」就只在单次进程生命周期内成立。
//
// 语义约束：
//   - n<=0 视为「无历史」，保持从 1 开始（首次启动的常见情形）；
//   - n 小于当前计数时不回退（取较大者），否则会与已有记录重号；
//   - 负数一律拒绝。
//
// 必须在开始 Push 之前调用；运行中调用只会把计数往前推，不会重排已有条目。
func (r *Ring) SeedSeq(n int64) error {
	if n < 0 {
		return fmt.Errorf("logbuf: SeedSeq 不接受负数（%d）", n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.total {
		r.total = n
	}
	return nil
}

// Push 写入一条；返回分配的递增序列号。
func (r *Ring) Push(e Entry) int64 {
	r.mu.Lock()
	r.total++
	e.Seq = r.total
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	sink := r.sink
	r.mu.Unlock()

	if sink != nil {
		sink.Append(e)
	}
	return e.Seq
}

// Snapshot 返回 seq > sinceSeq 的全部条目（按时间正序），以及当前最大 seq。
// sinceSeq<=0 时返回缓冲内全部。读侧轮询用 sinceSeq 做增量游标。
func (r *Ring) Snapshot(sinceSeq int64) ([]Entry, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	max := r.total
	if sinceSeq >= max {
		return nil, max
	}
	// 已滚出缓冲的最老 seq 下界：满了就是 total-cap+1，没满就是 1。
	lowest := int64(1)
	if r.full {
		lowest = r.total - int64(len(r.buf)) + 1
	}
	if sinceSeq < lowest-1 {
		sinceSeq = lowest - 1
	}

	n := int(max - sinceSeq)
	out := make([]Entry, 0, n)
	// 已写入的条目总数为 written = min(total, cap)，逻辑索引 0 对应 seq = total-written+1。
	written := r.total
	if int64(len(r.buf)) < written {
		written = int64(len(r.buf))
	}
	start := r.next - int(written)
	if start < 0 {
		start += len(r.buf)
	}
	for i := 0; i < int(written); i++ {
		e := r.buf[(start+i)%len(r.buf)]
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, max
}

// Len 返回当前缓冲内的条目数。
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return len(r.buf)
	}
	return r.next
}

// Cap 返回容量。
func (r *Ring) Cap() int { return len(r.buf) }
