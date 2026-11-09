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

	// UpstreamErr 上游错误的**原文**（已脱敏 + 已截断），仅失败请求出现（2026-11-10 加）。
	//
	// # 为什么必须有这一列（用户原话）
	//
	//   「如果说有什么请求报错了，有问题，不是 200，我觉得需要完整的记录一下具体的原文，
	//     这样我们找问题的时候就可以清晰的知道为什么。」
	//
	// # 与 Decision 的分工（两者互补，缺一不可）
	//
	//	Decision    —— 我做了什么：换了几个号、为什么放弃（`attempts=5 last=codar server 403`）
	//	UpstreamErr —— 上游说了什么：**原文**（含错误码与关键词）
	//
	// 只有 Decision 时，排障只能知道"上游回了 403"，**永远猜不出** 403 的正文是
	// "风控" / "额度不足" / "账号受限" / "边缘 IP 封控" —— 而那四种的处置完全不同
	// （等 / 换号 / 重登 / 换出口）。2026-11-09 的 codar 403 就卡在这里，至今无法定性。
	//
	// # 为什么是脱敏后的
	//
	// 这份会渲染到管理台（网页）。上游原文常带 host / IP / base_url（"后端是谁"的信息），
	// 此前已因泄露被修过一次 ⇒ 与出站同一口径，用 gateway.MaskError。
	// handler.go 里那句注释（"日志里保持原文"）指的正是本字段。
	//
	// # 为什么截断
	//
	// 上游偶尔返回整页 HTML（nginx 502/403 页）。不设上限会把一份响应体整个写进
	// JSONL（单条可达数百 KB）。上限见 UpstreamErrMaxLen，超出时保留**头部**
	// —— 判据关键词（error/restricted/quota/nginx）几乎都在开头。
	UpstreamErr string `json:"upstream_err,omitempty"`
}

// UpstreamErrMaxLen UpstreamErr 的最大字节数（UTF-8 安全截断在 logbuf 侧做）。
//
// 取值理由：4 KiB 足够容纳任何正常的上游错误 JSON（实测最大是 lsrouter 的一条 2.1 KiB
// 校验错误），又能挡住整页 HTML。截断处追加 "…[truncated:N bytes]"，
// 让人一眼看出"这不是全文"。
const UpstreamErrMaxLen = 4096

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

	// sql 可选的落库目标（nil = 不双写）。与 sink 一样在锁外非阻塞入队，
	// 由后台 goroutine 落 logs 表 —— 见 sqlsink.go（有界队列 / 失败隔离 /
	// 丢日志不丢请求）。
	sql *sqlSink
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
	sql := r.sql
	r.mu.Unlock()

	if sink != nil {
		sink.Append(e)
	}
	if sql != nil {
		// 非阻塞：队列满即丢弃并计数（sqlsink.go 第 2 条硬约束）。
		sql.enqueue(e)
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

// TruncateUTF8 把 s 截到最多 max 字节，且**不切断多字节字符**。
//
// 为什么需要它：上游错误体是任意字节流（中文/emoji/整页 HTML），
// 直接 s[:max] 会切出一个非法 UTF-8 序列 —— JSON 编码时会被替换成 U+FFFD，
// 而且日志读回来"看起来像乱码"，排障时会误判成"上游发了乱码"。
//
// 截断时追加 "…[truncated:N bytes]"：必须让人一眼看出"这不是全文"，
// 否则会拿半截正文去搜关键词，得出"上游没给关键词"的错误结论。
func TruncateUTF8(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	// 回退到最后一个完整字符的边界：UTF-8 的续字节形如 10xxxxxx。
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	// 再退一步：若该位置是一个多字节字符的起始字节而字符不完整，也退掉它。
	if cut > 0 && s[cut-1] >= 0xC0 {
		cut--
	}
	if cut <= 0 {
		cut = max
	}
	return s[:cut] + "…[truncated:" + itoa(len(s)-cut) + " bytes]"
}

// itoa 小工具（避免为一个数字引入 strconv 的格式化开销；与仓内既有写法一致）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
