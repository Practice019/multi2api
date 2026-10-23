// latency.go 账号级请求延迟指标：每号最近 N 个**成功**请求的耗时、P50/P95、按 provider 聚合，
// 以及供选号使用的「同一档内优先低延迟」判据。
//
// # 为什么需要它（对标 new-api 的 pkg/perf_metrics，但用途不同）
//
// new-api 的 perf_metrics 记录每个 (模型, 分组, 小时桶) 的 request/success/总延迟，
// **只算均值**（totalLatencyMs / requestCount，见 metrics.go 的 summarize/avg），
// 用于控制台展示成功率与平均延迟 —— 它的渠道选择完全不看延迟
// （只看 priority + 人工 weight，见 model/channel_cache.go）。
//
// 本仓的诉求不同（PLAN-newapi-alignment.md 的 G6）：**选号**要能「同档内优先快的」。
// 所以这里补两件 new-api 没有的东西：
//
//	① P50/P95（而不是均值）：均值会被尾部掩盖 —— 一个号 90% 请求 1 秒、
//	   10% 请求 60 秒，均值 6.9 秒，看起来"中等"；而用户实际感知的是那 10 次
//	   里的每一次。P95 才能表达"这个号的坏日子有多坏"。
//	② 与选号状态同锁（p.mu）的按号存储，供 pick 路径在**持有池锁时**读取 ——
//	   不能是异步刷新的外部指标（那会在选号时读到撕裂的旧值）。
//
// ⚠ 本文件与 pool.go 的 NoteProviderLatency/ProviderLatency 是**两层不同的指标**，
// 不要混用、也不要合并（合并会让两层各自的语义都变糊）：
//
//	NoteProviderLatency   **上游**级、EMA、只有均值、用于"这家整体比那家慢 1.5 倍吗"
//	                      （是否该把流量整批切走 —— SlowerThan 的判据）
//	NoteLatency（本文件） **账号**级、最近 N 个样本的 P50/P95、用于"同一档里先打谁"
//	                      （不改变档位，只改变档内顺序）
//
// 两层都需要：上游级回答"要不要换家"，账号级回答"这家内部先用哪个号"。
// 用 EMA 代替 P 值不行的理由见下方「为什么保留样本而不是 EMA」。
//
// # ⚠ 只采**成功**请求的样本（这是本文件最容易写错的一条）
//
// 失败请求的耗时**不是**这个号的服务质量：一个 400 会在 30ms 内返回，
// 把它当样本会让"参数错误"的号看起来最快，然后被优先选中 —— 与我们要的效果相反。
// 更糟的是它会把**坏号推成快号**，然后每一次选中都失败重试。
// new-api 的 RecordRelayResult 对 Failure 也记延迟（它只是展示用，不参与选号），
// 但我们把延迟当**判据**用，所以口径必须换成"只采成功"。
// 「失败」这件事由熔断器（NoteError）、自动禁用（RecordAutoDisableFailure）、
// 成功权重（weightOf 的 successRate）三条既有的线负责，不需要延迟再表达一次。
//
// # 为什么保留样本而不是 EMA（与 pool.go 的 provTTFB 相反，理由也写在那边）
//
// pool.go 选 EMA 的理由是"中位数要留一整段窗口（内存 + 排序）"—— 对**上游级**
// 成立：上游数量少、只需要一个"快还是慢"的标量。
// 账号级不同：我们要的是**尾部分布**（P95），而 EMA 是单值，天然抹掉尾部；
// 且样本上限可以做到很小（32 个 int64 = 512B/号，46 个号约 23KB）。
// 代价是每次查询要排序 —— 32 个元素的排序在纳秒~微秒级，远小于一次上游往返，
// 而选号本来就要为每个候选算 weightOf（浮点），不缺这点预算。
//
// # 聚合口径（本文件的核心承诺；改动即破坏语义，务必与 latency_test.go 同步改）
//
//  1. **样本**：每次 NoteLatency 记一个 (ms, 时刻)。非正值被丢弃
//     （0 = 没测到；负值 = 时钟回拨/调用方算错）。
//  2. **裁剪**：每号一个环形缓冲，容量 DefaultLatencySamples（默认 32，可配）。
//     满了丢**最旧**的一个 —— 而不是钉住最早 32 个：钉住头会让一个号在变快/变慢
//     之后再也不会更新自己的档位（那个功能等于只工作一次）。
//  3. **窗口**：默认只认最近 DefaultLatencyWindow（10 分钟）内的样本。
//     过期样本在**读取时**被忽略（不搬数据），于是"很久没被选中"的号自动退化为
//     "无样本"（见第 5 条），重新获得流量。这是本机制唯一的自愈路径：
//     被降档的号若永远拿不到流量，旧慢样本就会永远把它按在低档（自锁）。
//     window = 0 表示不按时间衰减（合法值，但不推荐）。
//  4. **P 值**：nearest-rank（最近秩法），rank = ceil(pct/100 × N)，
//     取排序后第 rank 个（1-based），**不插值**。
//     理由：插值会返回一个**从未发生过的延迟**（如 1234.5ms），管理台与
//     评审都无从判断那是真值还是算法产物；nearest-rank 返回的一定是真实观测值，
//     且 N=1 时 P50=P95=那一个样本（插值法在小样本下会出现"P50 比唯一值还小"）。
//     代价：N 小时的 P95 有量化误差（N=32 时 P95 实际落在第 31 个样本上），已够用。
//  5. **足够样本**：分档判据要求该号在窗口内至少有 DefaultLatencyMinSamples（3）个样本；
//     不足视为"无数据"，**不参与**基线计算（但仍会出现在返回值里，按快档处理）。
//     2 个样本的"中位数"就是两者里较小的那个，噪声足以把号来回甩档。
//  6. **样本数**：对外报告的 Samples 是**窗口内**的有效样本数（与 P 值同口径）。
//     显示与决策两个口径就是第二个真相，本仓在这上面已经吃过亏。
//
// # 与选号的配合：「同档内优先低延迟」
//
// 档位（CandidateTierExt / SetCandidateRanker）是**跨档硬隔离**（先烧免费额度），
// 本文件只决定**同一档内**先打谁，绝不改变档位本身。
// 提供两种用法，按"会不会饿死慢号"取舍：
//
//	LatencyTierMap        硬分档（0 快 / 1 普通 / 2 慢）。语义直接，但若调用方
//	                      只取 0 档，慢号就再也拿不到流量 ⇒ 违反用户反复强调的
//	                      "轮询所有账号"（见 weightOf 注释里 11 号实测 10:1 偏斜）。
//	                      仅推荐用于"档内再排序取前几个"这类不会永久排除的场景。
//	LatencyWeightFactors  弱偏好（1.0 / 0.6 / 0.35 的权重乘子）。慢号仍以
//	                      ~1/4 的份额参与抽签，配额会摊平，不会被饿死。
//	                      **接进既有加权选号时请优先用这个。**
//
// 两者共用同一套分档计算（latencyGradesLocked），所以不会出现
// "硬分档说它快、权重因子说它慢"这种两个口径打架的情况。
//
// ⚠ 无样本（或样本不足）的号在分档里按**第 0 档（快）**处理，理由是鸡生蛋：
// 一个刚导入的新号若被排到慢档，就永远拿不到请求、永远没有样本、永远排在慢档。
// 本仓既有代码在同一件事上的取舍是 weightOf 的 "从未用过：满额优先（新号快速摊平）"
// （详见 pool.go 的 w += 1.0 那一行）—— 这里与它保持一致。
// 代价是：新号若真的慢，会被打几次慢请求，凑满 3 个样本后立刻降档。
//
// # ⚠ 状态表为什么挂在包级注册表上（本文件唯一的架构妥协，与 autodisable.go 同源）
//
// 硬约束：本次改动只允许写 latency.go / latency_test.go，不给 Pool/entry 加字段。
// 而延迟样本必须与选号状态**在同一把锁下**读写，否则"pick 看到旧的 P50"
// 与"NoteLatency 写了新的"之间会出现撕裂窗口（表现为档位一会儿对一会儿不对）。
//
// 所以：表的**内容**由 p.mu 保护（加锁纪律与 pool.go 逐字一致：辅助函数带 Locked
// 后缀、调用方持锁），包级 sync.Map 只做 *Pool → 表 的一次性查找。
// 锁序永远是 p.mu → sync.Map（内部锁），没有反向路径，不会死锁。
//
// 代价与 autodisable.go 相同：Pool 被 GC 后注册表里仍留着一个空表
// （惰性创建，只有真记过延迟的池才有），进程内通常只有 1 个池，生产无影响；
// 测试里用 sweepLatencyBook 清理。
//
// # 内存上限（三个方向都要有界，缺一个就能被慢慢吃满）
//
//	每号样本数  环形缓冲容量（默认 32，封顶 MaxLatencySamples）
//	表的条目数  只保留**窗口内有样本**的号（见 pruneLocked），外加
//	            "uid 已不在池里就顺手删"（NoteLatency 的清理分支）
//	样本本身    固定 16 字节（int64 毫秒 + int64 纳秒时刻）
//
// 后两条的必要性：池子支持反复导入/删除账号（SyncToDir 会 delete），
// 而本文件无法改 Remove/SyncToDir 去挂钩清理 —— 只能在写入路径上自愈，
// 并在条目数超过水位时整体清扫一次过期条目。
package pool

import (
	"math"
	"sort"
	"sync"
	"time"
)

const (
	// DefaultLatencySamples 每号保留的样本数（环形缓冲容量）。
	//
	// 取 32 的依据：
	//  1. P95 的最近秩排名是 ceil(0.95×N)。N=32 时 rank=31 ⇒ P95 落在"第 2 慢"
	//     的那个真实样本上：既能反映尾部，又不会被**单个**离群值完全劫持
	//     （N 太小、比如 4，P95 就等于最大值，一次上游抽风就能把号打到慢档）。
	//  2. 32 个样本在窗口 10 分钟下大约覆盖"最近几十次请求"，
	//     对网关的常见流量（个位数 QPS/号）足以翻页，又不会把一两小时前的
	//     速度当成现状。
	//  3. 内存：32 × 16B = 512B/号。46 个号约 23KB —— 可以忽略，
	//     所以不必为省内存换 HDR 直方图（那会引入桶边界误差，而 P 值要的是真值）。
	DefaultLatencySamples = 32

	// MaxLatencySamples 样本数上限（SetLatency 会把超出的配置钳到这里）。
	//
	// 为什么需要硬上限：样本数是**配置项**，一个手滑写成 1000000 的配置
	// 会让每号占 16MB、46 个号 736MB —— 而症状只是"内存慢慢涨"，
	// 没有任何日志线索。上限 512 已远超 P95 稳定估计所需（≥20），
	// 钳制在这里不会伤害任何合理配置。
	MaxLatencySamples = 512

	// DefaultLatencyMinSamples 参与分档所需的最小样本数（窗口内）。
	//
	// 取 3 的依据：2 个样本的"中位数"就是两者里较小的那个 —— 一次冷启动
	// 慢请求会把一个健康号直接打到慢档；3 是能排出真正中位的最小奇数。
	// 与 pool.go 的 provTTFBMinN（20）不同：那边比的是**上游**（样本天然更多、
	// 且整批切流量的代价大，所以要更稳），这边比的是**同一档内的候选**，
	// 3 个样本 + 10 分钟窗口已足够区分"1 秒"与"20 秒"这种量级差。
	DefaultLatencyMinSamples = 3

	// DefaultLatencyWindow 样本的有效期（0 = 不衰减）。
	//
	// 为什么默认 10 分钟：上游速度是**漂移量**（new-api 的桶是 1 小时，
	// 那是为了画图；为了让选号"现在准"，窗口要小得多）。
	// 10 分钟覆盖了网关常见的一轮"轮询一遍所有号"，同时保证
	// 一个号变快之后最多 10 分钟就能重新拿到正常档位。
	DefaultLatencyWindow = 10 * time.Minute

	// latencyFastRatio 相对基线（候选集 P50 的中位数）的倍数阈值。
	//
	// ⚠ 与 pool.go 的 provTTFBSlowRas 取同一个数（1.5）是**有意**的：
	// 两处都在回答"差多少才算真的差"，用同一把尺子可互相印证
	// （那边的 1.5 是按实测 1.81 倍反推出来的，见 provTTFBSlowRas 的注释）。
	// 反过来，若这里取 2.0，则正常的 1.5~1.9 倍差距会被当成"一样快" ——
	// 那正是 provTTFBSlowRas 第一版踩过的坑（对真实数据永不触发）。
	latencyFastRatio = 1.5

	// latencyPruneWater 表的条目数超过这个水位时，清扫一次窗口内无样本的条目。
	//
	// 为什么需要水位而不是每次写都扫：逐次扫是 O(账号数) 的额外开销，
	// 而清理收益只在"长期跑、频繁增删号"时才体现。
	// 水位取 256：远超正常部署的账号数（实测最大 46 个），
	// 触发的唯一场景就是"删过很多号"—— 那正是要清理的场景。
	latencyPruneWater = 256
)

// 分档取值（LatencyTierMap / LatencyWeightFactors 的契约）。
//
// ⚠ 0 是"快"，**不是**"无数据"：无数据的号也归到 0（理由见文件头的"鸡生蛋"）。
// 这样调用方只可能拿到 0/1/2 三个值，不需要为"未知"另写一条分支
// （多一条分支就多一处可能写错的地方）。
const (
	// LatencyTierFast 明显快于该 provider 候选集的典型值（≤ 典型/1.5），
	// 或**样本不足**（视为"给它一次机会"）。
	LatencyTierFast = 0
	// LatencyTierNormal 与典型值同量级（典型/1.5 ~ 典型×1.5）。
	LatencyTierNormal = 1
	// LatencyTierSlow 明显慢于典型值（≥ 典型×1.5）。
	LatencyTierSlow = 2
)

// 分档 → 加权选号的权重乘子（LatencyWeightFactors 的取值）。
//
// 为什么慢档不是 0（排除）：用户反复强调"轮询所有账号"，而 0 权重
// 在 pickWeighted 里等价于永不选中 —— 一个因上游抽风慢了 10 分钟的号
// 会被永久踢出轮询（样本过期后它仍是 0 权重，因为它拿不到新样本）。
// 取 0.35 让慢号保留约 1/4~1/3 的抽签份额：既能明显让位于快号，
// 又能在快号忙/冷却时被选中，从而持续产生新样本（窗口自愈的前提）。
const (
	// LatencyWeightFast 快档（含无样本）不降权。
	LatencyWeightFast = 1.0
	// LatencyWeightNormal 普通档轻微降权。
	LatencyWeightNormal = 0.6
	// LatencyWeightSlow 慢档显著降权，但不归零（保留轮询与自愈）。
	LatencyWeightSlow = 0.35
)

// LatencyStat 单个账号的延迟视图（导出，供管理台 / 诊断 / 测试）。
//
// ⚠ P50/P95/Samples **刻意不带 omitempty**：0 也是事实（"这个号窗口内没有样本"），
// 必须显示成 0 而不是从 JSON 里消失。这是本仓反复的教训
// （见 Status.SuccessCount 的注释：omitempty 让 0 变成空白单元格）。
type LatencyStat struct {
	UID      string `json:"uid"`
	Provider string `json:"provider,omitempty"`

	// P50Ms / P95Ms 窗口内样本的最近秩 P50 / P95（毫秒）。无样本时为 0。
	P50Ms int64 `json:"p50_ms"`
	P95Ms int64 `json:"p95_ms"`

	// Samples 窗口内的有效样本数（与 P50/P95 同口径）。
	Samples int `json:"samples"`

	// LastAt 窗口内最新样本的时刻（零值 = 窗口内无样本）。
	LastAt time.Time `json:"last_at,omitempty"`
}

// latencySample 一个延迟样本。字段用 int64 而不是 time.Duration/time.Time：
// 环形缓冲里每个槽位都会被复制，16 字节的紧凑布局比可读性更重要
// （time.Time 是 24 字节且带单调时钟读数 —— 对"多久以前"没有用处）。
type latencySample struct {
	ms int64
	at int64 // UnixNano
}

// latencyRing 定长环形缓冲：满了覆盖最旧的。
//
// 为什么是环而不是"append + 超长时切掉前一半"：切掉一半会让样本数在
// 32 → 16 → 32 之间锯齿跳变，于是 P95 也跟着跳（第 31 个样本 vs 第 16 个），
// 而档位是**跳变敏感**的（1.5 倍阈值附近来回翻档 = 号被来回甩）。
// 环形缓冲的样本数恒定在容量上，P95 的统计口径稳定。
type latencyRing struct {
	buf  []latencySample // len(buf) == 容量
	head int             // 下一个写入位置
	n    int             // 已写入数（≤ len(buf)）
}

// add 写入一个样本（满则覆盖最旧的）。调用方必须持 p.mu。
func (r *latencyRing) add(s latencySample) {
	if len(r.buf) == 0 {
		return // 防御：容量 0 不应出现（SetLatency 拒绝非正值，创建时用默认值）
	}
	r.buf[r.head] = s
	r.head = (r.head + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

// each 按**从旧到新**遍历有效样本。调用方必须持 p.mu。
func (r *latencyRing) each(fn func(latencySample)) {
	if r == nil || r.n == 0 || len(r.buf) == 0 {
		return
	}
	start := (r.head - r.n + len(r.buf)) % len(r.buf)
	for i := 0; i < r.n; i++ {
		fn(r.buf[(start+i)%len(r.buf)])
	}
}

// values 取窗口内的样本值（**新切片**，调用方可以就地排序）与最新样本时刻（UnixNano）。
//
// window <= 0 表示不衰减（全部样本都算有效）—— 与 SetLatency 的
// "window 0 合法"约定一致（对齐 SetAutoDisable 的 window 语义）。
func (r *latencyRing) values(now time.Time, window time.Duration) (vals []int64, newestAt int64) {
	if r == nil || r.n == 0 {
		return nil, 0
	}
	var cutoff int64
	if window > 0 {
		cutoff = now.Add(-window).UnixNano()
	}
	r.each(func(s latencySample) {
		if cutoff != 0 && s.at < cutoff {
			return // 过期样本：不删，但也不参与本次口径
		}
		vals = append(vals, s.ms)
		if s.at > newestAt {
			newestAt = s.at
		}
	})
	return vals, newestAt
}

// anyFresh 报告环里是否还有窗口内的样本（pruneLocked 用，不分配切片）。
func (r *latencyRing) anyFresh(now time.Time, window time.Duration) bool {
	if r == nil || r.n == 0 {
		return false
	}
	if window <= 0 {
		return true // 不衰减 ⇒ 永远"新鲜"
	}
	cutoff := now.Add(-window).UnixNano()
	fresh := false
	r.each(func(s latencySample) {
		if s.at >= cutoff {
			fresh = true
		}
	})
	return fresh
}

// resize 调整容量：保留**最近**的样本。调用方必须持 p.mu。
func (r *latencyRing) resize(capN int) {
	if capN <= 0 || len(r.buf) == capN {
		return
	}
	kept := make([]latencySample, 0, r.n)
	r.each(func(s latencySample) { kept = append(kept, s) })
	if len(kept) > capN {
		kept = kept[len(kept)-capN:] // 只留最近的
	}
	nr := latencyRing{buf: make([]latencySample, capN), n: len(kept)}
	copy(nr.buf, kept)
	nr.head = len(kept) % capN
	*r = nr
}

// latencyBook 单个 Pool 的延迟配置 + 全量样本表。
//
// 内容由该 Pool 的 mu 保护（加锁纪律与 pool.go 一致：调用方持锁，
// 方法名带 Locked 后缀）。
type latencyBook struct {
	samples int // 每号样本上限
	window  time.Duration
	rings   map[string]*latencyRing
}

// ringLocked 取（或惰性建）某 uid 的环形缓冲。调用方必须持 p.mu。
func (b *latencyBook) ringLocked(uid string) *latencyRing {
	if r, ok := b.rings[uid]; ok {
		return r
	}
	r := &latencyRing{buf: make([]latencySample, b.samples)}
	b.rings[uid] = r
	return r
}

// pruneLocked 丢弃窗口内已无样本的条目。调用方必须持 p.mu。
//
// 只在条目数超过水位时触发（理由见 latencyPruneWater）。
// ⚠ 它**不动**仍在窗口内的条目 —— 那些正是选号要用的数据。
func (b *latencyBook) pruneLocked(now time.Time) {
	for uid, r := range b.rings {
		if !r.anyFresh(now, b.window) {
			delete(b.rings, uid)
		}
	}
}

// latencyBooks *Pool → *latencyBook 的包级注册表。
//
// 只做查找，内容由各自 Pool 的 mu 保护（理由见文件头的"架构妥协"）。
// 键用 *Pool 而不是 uid：两个池里同名 uid 是**两个不同的账号**
// （测试与多实例部署都会出现），混在一起会串样本。
var latencyBooks sync.Map

// latencyBookLocked 取（或创建）本池的延迟表。调用方必须持 p.mu（读锁即可）。
//
// 惰性创建而不是在 New() 里建：New() 在 pool.go 里，本次不改它。
// 用 LoadOrStore 而不是 Load+Store 是防御：即便将来有人在持锁之外调用，
// 也不会丢掉一个已存在的表（那会静默清空所有样本）。
func (p *Pool) latencyBookLocked() *latencyBook {
	if v, ok := latencyBooks.Load(p); ok {
		return v.(*latencyBook)
	}
	b := &latencyBook{
		samples: DefaultLatencySamples,
		window:  DefaultLatencyWindow,
		rings:   map[string]*latencyRing{},
	}
	v, _ := latencyBooks.LoadOrStore(p, b)
	return v.(*latencyBook)
}

// sweepLatencyBook 丢弃本池的延迟表（测试清理用；生产进程内只有一个池，无需调用）。
//
// 为什么允许它存在：注册表是包级强引用，测试会创建成百上千个池，
// 不清理就是稳定的内存泄漏（与 sweepAutoDisableBook 同一条理由）。
func sweepLatencyBook(p *Pool) {
	latencyBooks.Delete(p)
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// SetLatency 注入延迟指标参数（装配层从 config 解析后调用）。
//
// 语义与 SetAutoDisable 对齐：samples 非正值保留原值（用默认），
// 超过 MaxLatencySamples 则钳到上限（防手滑配置把内存吃满，见该常量的注释）；
// window 的判据是 >= 0 —— 0 是合法取值（不按时间衰减），负数与不传等价。
//
// ⚠ 改 samples 时会**立即**把已有的环缩到新容量（保留最近的样本），
// 而不是等旧样本自然被覆盖：否则调小配置后"上限"要过很久才真的生效。
func (p *Pool) SetLatency(samples int, window time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.latencyBookLocked()
	if samples > 0 {
		if samples > MaxLatencySamples {
			samples = MaxLatencySamples
		}
		b.samples = samples
		for _, r := range b.rings {
			r.resize(samples)
		}
	}
	if window >= 0 {
		b.window = window
	}
	// ⚠ 不置 p.dirty：延迟样本是**运行态**，不落盘（理由见 NoteLatency）。
}

// LatencySamples 返回当前生效的每号样本上限（管理台展示 / 配置回读用）。
func (p *Pool) LatencySamples() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.latencyBookLocked().samples
}

// LatencyWindow 返回当前生效的样本有效期（0 = 不衰减）。
func (p *Pool) LatencyWindow() time.Duration {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.latencyBookLocked().window
}

// ---------------------------------------------------------------------------
// 记录
// ---------------------------------------------------------------------------

// NoteLatency 记录一次**成功**请求的端到端延迟（毫秒）。⚠ 只在成功路径上调用。
//
// 调用时机：出口层收到上游完整响应（或流的首字节/结束）后，与 NoteSuccess 并列。
// 失败路径**不要**调用 —— 理由见文件头的"只采成功样本"。
//
// ms <= 0 一律忽略：0 在出口层的含义是"没测到"（非流式、或上游没给时刻），
// 把它喂进来会把一个健康号的中位延迟一路拉到 0、看起来"飞快"
// （pool.go 的 NoteProviderLatency 为同一件事写过同类注释）。
//
// ⚠ 样本**不落盘**（不写 state.json、不置 dirty）。理由同 provState 的 ttfbEMA：
// 延迟是"现在快不快"的运行态，重启后的旧分布不代表现状；
// 而落盘会让每 5s 一次的 flush 多写 N×32 个数字（46 个号约 1500 条），
// 换来一个重启后**不该被信任**的依据。
func (p *Pool) NoteLatency(uid string, ms int64) {
	if ms <= 0 {
		return // 快路径：不进锁（与选号热路径比邻，别为无效样本抢写锁）
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	b := p.latencyBookLocked()
	e, ok := p.byUID[uid]
	if !ok || e == nil {
		// 号已经不在池里（被 Remove / SyncToDir 剔除）：顺手清掉残留样本，
		// 否则"删号 → 重新导入同名 uid"会带着旧的延迟档位上台
		// （与 autodisable.go 的同名清理同源）。
		delete(b.rings, uid)
		return
	}
	b.ringLocked(uid).add(latencySample{ms: ms, at: now.UnixNano()})
	if len(b.rings) > latencyPruneWater {
		b.pruneLocked(now)
	}
}

// ResetLatency 丢弃某账号的全部延迟样本（人工干预调用），返回是否真清掉了东西。
//
// # 什么时候该调
//
//   - 管理台的「恢复 / 清冷却」按钮：人工宣布"这个号没问题"时，若还留着一批
//     把它按在慢档的旧样本，用户会觉得"点了恢复还是不用它"
//     （与 ClearCooldown 注释里"点了清冷却，一分钟后冷两小时"是同一类体验坑）。
//   - 健康探测成功后**不必**清：探测只证明"能通"，不证明"快"。
//     但若探测耗时也经 NoteLatency 采样，那一个样本会与旧样本混在一起；
//     想"从零建立印象"就调它，由调用方取舍。
//
// ⚠ 本方法不清任何**健康**状态（冷却/熔断/禁用）—— 那是 ClearCooldown 的事。
// 延迟只是选号偏好，绝不能拿它当"允许用这个号"的判据。
func (p *Pool) ResetLatency(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.latencyBookLocked()
	if _, ok := b.rings[uid]; !ok {
		return false
	}
	delete(b.rings, uid)
	return true
}

// ---------------------------------------------------------------------------
// 查询（单个账号）
// ---------------------------------------------------------------------------

// LatencyOf 查询单个账号在**窗口内**的 P50/P95/样本数。
//
// ok=false 只表示 uid 不在池里（与 Status / AutoDisableStatus 同源）；
// 在池里但窗口内无样本时返回零值（Samples=0、P50Ms=0、P95Ms=0），ok=true ——
// 管理台要把"0 个样本"显示成 0 而不是空白（见 LatencyStat 的注释）。
//
// ⚠ 不 panic：任何"没有数据"的路径都返回零值。这是本方法的契约
// （选号/管理台侧不该因为一个 nil 解引用而把整个网关带崩）。
func (p *Pool) LatencyOf(uid string) (LatencyStat, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok || e == nil {
		return LatencyStat{}, false
	}
	return p.latencyStatLocked(uid, e, time.Now()), true
}

// latencyStatLocked 组装单账号延迟视图。调用方必须已持有 p.mu（读锁即可）。
func (p *Pool) latencyStatLocked(uid string, e *entry, now time.Time) LatencyStat {
	b := p.latencyBookLocked()
	st := LatencyStat{UID: uid, Provider: p.providerOf(e)}
	vals, newestAt := b.rings[uid].values(now, b.window)
	st.Samples = len(vals)
	if st.Samples == 0 {
		return st // 窗口内无样本：零值（不是 panic，也不是上一轮的旧值）
	}
	// values 返回的是**新切片**，就地排序不会影响环里样本的顺序。
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	st.P50Ms = latencyPercentileSorted(vals, 50)
	st.P95Ms = latencyPercentileSorted(vals, 95)
	st.LastAt = time.Unix(0, newestAt)
	return st
}

// latencyPercentileSorted 在**已升序排序**的样本上取 pct 百分位（nearest-rank）。
//
// 口径见文件头第 4 条：rank = ceil(pct/100 × N)，取第 rank 个（1-based），不插值。
// 边界：空切片返回 0（调用方据此表示"没有数据"），pct<=0 取最小值，pct>=100 取最大值。
//
// 为什么单独抽成纯函数：它是本文件唯一"算错就会静默改变选号偏好"的地方，
// 必须有独立的表驱动测试（不经过池、不经过锁），否则只能靠端到端"感觉不对"。
func latencyPercentileSorted(sorted []int64, pct float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if pct <= 0 {
		return sorted[0]
	}
	if pct >= 100 {
		return sorted[len(sorted)-1]
	}
	rank := int(math.Ceil(pct / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// ---------------------------------------------------------------------------
// 查询（按 provider 维度）
// ---------------------------------------------------------------------------

// LatencyStats 返回**所有**账号的延迟视图（按 provider、再按 P50 升序、
// 无样本者排在该 provider 末尾，最后按 UID 稳定破平）。
//
// 排序口径：管理台要"快的在前"。无样本的号排最后而不是当 0 排最前 ——
// 0 在这里是"没有数据"，不是"零延迟"（混排会让新号看起来最快）。
func (p *Pool) LatencyStats() []LatencyStat {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	out := make([]LatencyStat, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if e == nil {
			continue
		}
		out = append(out, p.latencyStatLocked(uid, e, now))
	}
	sortLatencyStats(out)
	return out
}

// LatencyStatsFor 返回指定上游的账号延迟视图（排序口径同 LatencyStats）。
//
// provider 为空走默认上游（与 ListFor / ProviderLatency 的归一语义一致）。
// ⚠ 该上游一个账号都没有时返回**空切片**而不是 nil：
// JSON 里是 []，避免前端拿到 null（与 ProviderStates 的同一条约定）。
func (p *Pool) LatencyStatsFor(provider string) []LatencyStat {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	want := p.normalizeProvider(provider)
	out := make([]LatencyStat, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if e == nil || p.providerOf(e) != want {
			continue
		}
		out = append(out, p.latencyStatLocked(uid, e, now))
	}
	sortLatencyStats(out)
	return out
}

// sortLatencyStats 稳定排序：provider → 有样本者在前 → P50 升序 → UID。
func sortLatencyStats(stats []LatencyStat) {
	sort.Slice(stats, func(i, j int) bool {
		a, b := stats[i], stats[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if (a.Samples > 0) != (b.Samples > 0) {
			return a.Samples > 0 // 有数据的排前面
		}
		if a.P50Ms != b.P50Ms {
			return a.P50Ms < b.P50Ms
		}
		return a.UID < b.UID
	})
}

// ProviderLatencyPercentiles 返回某上游**全部账号样本合并后**的 P50/P95 与样本数。
//
// ok=false 表示该上游在窗口内一个样本都没有（≠ 该上游不存在：它可能只是没有流量）。
//
// # ⚠ 与 LatencyStatsFor 的口径差异（这是**有意的**，别当成 bug）
//
//	LatencyStatsFor             每号一份 P 值 —— 回答"这个号快不快"（选号用）
//	ProviderLatencyPercentiles  合并后一份 P 值 —— 回答"这家上游现在的延迟分布"
//	                            （运维用：出问题时要能一眼看出这家整体变慢了）
//
// 合并在语义上是**按请求加权**：请求多的号（也就是被选中的号）对结果影响更大。
// 这正是运维要的口径（用户感受到的延迟就是这么分布的）；若要"每号等权"，
// 那得先对每号取 P50 再平均，不在本方法范围内。
func (p *Pool) ProviderLatencyPercentiles(provider string) (p50, p95 int64, n int, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	b := p.latencyBookLocked()
	now := time.Now()
	want := p.normalizeProvider(provider)
	var pooled []int64
	for uid, e := range p.byUID {
		if e == nil || p.providerOf(e) != want {
			continue
		}
		if r := b.rings[uid]; r != nil {
			vals, _ := r.values(now, b.window)
			pooled = append(pooled, vals...)
		}
	}
	if len(pooled) == 0 {
		return 0, 0, 0, false
	}
	sort.Slice(pooled, func(i, j int) bool { return pooled[i] < pooled[j] })
	return latencyPercentileSorted(pooled, 50), latencyPercentileSorted(pooled, 95), len(pooled), true
}

// ---------------------------------------------------------------------------
// 选号配合：同档内优先低延迟
// ---------------------------------------------------------------------------

// LatencyTierMap 为给定候选集计算**相对**延迟分档（0 快 / 1 普通 / 2 慢）。
//
// provider 为空走默认上游；uids 为空表示"该上游的全部账号"。
// 返回 nil 表示**无足够数据、不要据此改变行为**（可比对象不足 2 个），
// 调用方必须原样保持既有选号结果。
//
// # 分档口径
//
//	基线 base = 候选集中"有足够样本"（≥ DefaultLatencyMinSamples）的号的 P50 的
//	           **下中位数**（偶数个取偏小的一侧，见 latencyGradesLocked 里的踩坑记录）
//	0 快    P50 ≤ base / 1.5
//	1 普通  base / 1.5 < P50 < base × 1.5
//	2 慢    P50 ≥ base × 1.5
//	样本不足（< 3 个）或不在池里或在别的上游 → 0（理由见文件头的"鸡生蛋"）
//
// 为什么基线是"候选集的中位数"而不是"最快那个"：
// 用最快者做基线时，一个侥幸的 100ms 样本会把其余全部号打成"慢"，
// 于是 11 个号里 10 个被降档 —— 与用户实测抱怨的 10:1 偏斜同形。
// 中位数对单个离群值不敏感；"明显更快"才需要 base/1.5 的门槛
// （两者都是 1.5 倍的双向门槛，所以分档本质上是"相对中位数的双侧离群判定"）。
//
// # ⚠ 用法（不要把它当全局过滤器）
//
// 它只该用于**同一额度档位内**（CandidateTierExt 之后）的顺序调整。
// 若把它当唯一判据、只取 0 档，慢号在流量充足时就长期拿不到请求；
// 好在窗口过期后它会回到 0 档重新获得流量（自愈），但表现为"每 10 分钟一次试探"。
// 这就是更推荐 LatencyWeightFactors 的原因 —— 它保留轮询，不会出现"只用一两个号"。
//
// ⚠ 加锁：本方法自己取 p.mu 读锁，**不得**在已持锁的路径（如 pickForTier 的写锁）
// 里调用 —— sync.RWMutex 不可重入，会直接死锁。池内调用请用 latencyGradesLocked。
func (p *Pool) LatencyTierMap(provider string, uids []string) map[string]int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	grades, ok := p.latencyGradesLocked(provider, uids, time.Now())
	if !ok {
		return nil
	}
	return grades
}

// LatencyWeightFactors 为给定候选集计算**权重乘子**（1.0 / 0.6 / 0.35）。
//
// 分档与 LatencyTierMap 是同一套计算（同一个函数），只把出口换成乘子：
// 接进既有加权选号时**优先用这个** —— 慢号仍参与抽签，不会被饿死。
// 返回 nil 表示无足够数据（调用方不要调整权重）。
//
// # 怎么接（集成形态）
//
// 在 weightOf 的返回值上乘一级：
//
//	factors := p.LatencyWeightFactors(provider, uids) // 在 pick 之外预取
//	w := weightOf(e) * factors[e.a.UID]              // nil / 缺键 ⇒ 视为 1.0
//
// ⚠ 加锁约束同 LatencyTierMap：本方法自己取读锁，**不能在 pickForTier
// （已持写锁）内部调用**。两条可行路径：
//
//	a) 池外预取：handler 在选号前取一次 factors 传进选号（推荐，无锁风险）；
//	b) 池内直调：在 pickForTier 里改用 p.latencyGradesLocked（调用方已持锁），
//	   再用 LatencyWeightForGrade 换算（pool.go 是同包，能直接调）。
func (p *Pool) LatencyWeightFactors(provider string, uids []string) map[string]float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	grades, ok := p.latencyGradesLocked(provider, uids, time.Now())
	if !ok {
		return nil
	}
	out := make(map[string]float64, len(grades))
	for uid, g := range grades {
		out[uid] = LatencyWeightForGrade(g)
	}
	return out
}

// LatencyWeightForGrade 把分档换算成权重乘子。
//
// 导出的理由：池内（pool.go 的 pickForTier，已持锁）拿到 latencyGradesLocked
// 的结果后需要**同一张**换算表，否则会出现"外面用 0.35、里面用 0.5"的第二套口径。
// 未知分档按"快"（不降权）—— 与"无数据不惩罚"的立场一致。
func LatencyWeightForGrade(grade int) float64 {
	switch grade {
	case LatencyTierSlow:
		return LatencyWeightSlow
	case LatencyTierNormal:
		return LatencyWeightNormal
	default:
		return LatencyWeightFast
	}
}

// latencyGradesLocked 计算候选集内的相对延迟分档（**唯一**的分档实现）。
//
// 调用方必须已持有 p.mu（读锁或写锁均可，本函数只读）。
// 返回值 ok=false 表示数据不足以判档（调用方据此完全保持原行为）。
func (p *Pool) latencyGradesLocked(provider string, uids []string, now time.Time) (map[string]int, bool) {
	want := p.normalizeProvider(provider)
	scope := uids
	if len(scope) == 0 {
		scope = make([]string, 0, len(p.byUID))
		for uid, e := range p.byUID {
			if e != nil && p.providerOf(e) == want {
				scope = append(scope, uid)
			}
		}
		sort.Strings(scope) // 稳定顺序：测试与日志可对照（map 迭代顺序不稳定）
	}

	b := p.latencyBookLocked()
	p50s := make(map[string]int64, len(scope))
	known := make([]int64, 0, len(scope))
	for _, uid := range scope {
		e, ok := p.byUID[uid]
		if !ok || e == nil || p.providerOf(e) != want {
			continue // 别的上游的号 / 已不在池里：不参与本次判档
		}
		r := b.rings[uid]
		if r == nil {
			continue
		}
		vals, _ := r.values(now, b.window)
		if len(vals) < DefaultLatencyMinSamples {
			continue // 样本不足：不进基线（否则一次冷启动就能左右基线）
		}
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		p50 := latencyPercentileSorted(vals, 50)
		p50s[uid] = p50
		known = append(known, p50)
	}
	// 少于 2 个可比对象 ⇒ "相对快慢"无从成立，返回 nil 让调用方保持原行为。
	if len(known) < 2 {
		return nil, false
	}
	sort.Slice(known, func(i, j int) bool { return known[i] < known[j] })
	// base 取**下中位数**（偶数个取偏小的那一侧）。
	//
	// ⚠ 这里踩过一次：第一版取的是上中位数（len/2），理由是"少降档、别误判慢号"。
	// 而 n=2 时上中位数**就等于最大值** ⇒ 慢档门槛变成"要比池里最慢的还慢 1.5 倍"
	// ⇒ **两个号差 90 倍也全部判成普通**，那个功能等于没做（是单测抓出来的：
	// 100ms vs 9000ms 期望慢档，实得普通）。
	// 下中位数在 n=2 时取较快那个，于是"差 1.5 倍以上"立刻能分出来；
	// n≥3 时它与上中位数只差一个名次，几乎不影响口径。
	//
	// 代价是"判慢的门槛偏低" ⇒ 偏多的号被降档。可以接受，因为
	// **延迟只影响优先级，不影响可用性**：被降档的号仍以 0.35 的权重参与抽签
	// （见 LatencyWeightSlow），且窗口过期后会自己回到快档（自愈），
	// 代价远小于"让一个真的慢号持续吃流量"（用户等的是那 18 秒）。
	base := known[(len(known)-1)/2]
	if base <= 0 {
		base = 1 // 防御除零（P50 理论上 > 0，因为 ms<=0 已在 NoteLatency 被丢弃）
	}

	grades := make(map[string]int, len(scope))
	for _, uid := range scope {
		e, ok := p.byUID[uid]
		if !ok || e == nil || p.providerOf(e) != want {
			continue
		}
		p50, has := p50s[uid]
		if !has {
			// 样本不足 / 从没被采样 ⇒ 按"快"处理（鸡生蛋，见文件头）。
			grades[uid] = LatencyTierFast
			continue
		}
		switch {
		case float64(p50) <= float64(base)/latencyFastRatio:
			grades[uid] = LatencyTierFast
		case float64(p50) >= float64(base)*latencyFastRatio:
			grades[uid] = LatencyTierSlow
		default:
			grades[uid] = LatencyTierNormal
		}
	}
	return grades, true
}
