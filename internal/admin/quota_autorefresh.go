package admin

import (
	"log"
	"sort"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/pool"
)

// 本文件：**额度自动刷新**。设计见 docs/cache-refresh-design.md 第 2 节（定时兜底）。
//
// # 它解决什么（用户 2026-10-18 的原话）
//
//	"我们的要求不是说我们调用一次调用一个账号一次请求，它会返回出额度吗？
//	 然后直接写回本地的磁盘文件缓存，前端就读取这个缓存就行了呀。
//	 但为什么会出现前端显示是过时的文件、过时的数据？"
//
// 查证结论（三层，都记在 panelwarmup.go 的同一处注释里）：
//  1. **chat 回执里没有余额字段** ⇒ "每次调用写回真实余额"做不到。
//  2. **也不能拿过期值做减法**（`gateway/quota_consumed.go` 记着那次"多扣"的教训，
//     而那个数会喂给**选号权重**）。
//  3. 所以真实余额只有两条来源：用户点「刷新额度」／`StartFreePoolSweep`（只查池子成员）
//     ⇒ ⚠ **界面那个值 = "上次刷新的值" = 过时**。
//
// ⇒ 唯一的正解就是**让刷新自动发生**：本文件那个循环。
// ⚠ 它必须在这里（admin 层）而不是 loomy 层：余额的落账口是核心的
//   `pool.Pool.SetQuota`，而 loomy 拿不到它（依赖方向不对 —— 试过了，编译期就断）。

// quotaAutoRefreshEvery 自动刷新的间隔。
//
// ⚠ 它取 loomy 的 `TTLQuota`（3 分钟）同一个值，理由是那个值的**消费者**：
// 额度是**选号权重**的输入，选号又决定"下一个请求发给谁"。3 分钟足够跟上
// "按分钟级使用"的节奏，而它把请求量压在 `44 号 / 3 分钟 ≈ 15 次/分钟`。
//
// ⚠ 不要调成"每次调用都刷"：那会把请求量变成跟调用量线性相关（实测口径
//
//	见设计文档第 4 节：100 调用/分 ⇒ 100 请求/分，是这里的 5 倍以上），
//	而收益只是"提前 3 分钟知道"。
const quotaAutoRefreshEvery = 3 * time.Minute

// 首轮**延迟**：错开启动瞬间的其它预热（面板预热、free pool 填充）。
//
// ⚠ 不 await、不抢跑：启动流程一毫秒都不该为它多等（与 admin.go 里那条
// "不 await" 的预热同一个约束）。
const quotaAutoRefreshFirstDelay = 20 * time.Second

// 自适应降速的**上限**。⚠ 上游不稳时最长让路 30 分钟，而不是无限退让 ——
// 无限退让的后果是"上游恢复了、额度却再也不更新"，而这件事**没有别的信号**
// （这条循环是唯一在刷额度的地方）。
const quotaAutoRefreshMaxEvery = 30 * time.Minute

// 连续多少轮"失败率过半"就退让一档。
//
// ⚠ 判据用**比例**而不是绝对数：实测 76 个号里失败 1 个（2026-10-18 的真实数据）
// 是正常抖动；76 个里失败 40 个才说明上游/网络出问题了 —— 两者必须区分得开，
// 否则一次偶发抖动就会让额度刷新**主动变慢**（那是在制造用户报的那个现象）。
const quotaAutoRefreshFailRun = 2

// ── 额度更新策略（2026-11-02 · T-029 定稿）─────────────────────────────
//
// # 用户原话（设计依据）
//
//	"如果说回值里面是可以计算出来的，我们就计算。然后可以计算的时候，
//	 那它们有可能会出现偏差…所以我们可以再给它加一个定时的更新，但这个定时
//	 可以延得很长，比如说一天去定时更新一下，或者说就不更新了，除非自己前端手动更新。"
//
// ⇒ 于是刷新分成**两种模式**（本文件的中心判据）：
//
//	① 有回执（上游声明了 UsageTapSpec/UsageConsumed）：余额**算得出来** ⇒ 默认不刷，
//	   只在"24 小时"或"成功调用增量 ≥200 次"之一到点时**校正**一次。
//	② 没回执：只能刷，按活跃度分层（热 10m / 温 30m / 冷 6h）。
//
// # ⚠ 有回执的也不能真的"永不刷"
//
// `gateway/quota_consumed.go` 把偏差描述成"**有界**且自愈" —— 而那个"界"的前提正是
// 周期性地有一次 `RefreshQuota` 当新基准（那次查询会清空上游侧的待上报累计）。
// 永不刷 ⇒ 偏差**无界**。这就是下面两条闸门存在的理由。
//
// # ⚠ 判据用 `LastUsed` 而不是 `LastSuccess`（分层那一支）
//
// 一次调用**无论成功失败都消耗额度**（见 orderQuotaRefreshTargets 的注释），
// 所以"最近被选中"才是"额度可能已经变了"的信号。
//
// # ⚠ 与手工「刷新额度」的分工
//
// `RefreshAllQuotasNow`（手工/排障）**仍然全量刷** —— 用户点那个按钮的意思就是
// "我现在要看全部"。分层/校正只作用在**定时循环**那条路上（`RefreshDueQuotasNow`）。
const (
	// ① 有回执的上游：靠算 ⇒ 只做"校正"。
	//
	// quotaReconcileEvery 多久**至少**校正一次（用户给的量级："比如一天"）。
	quotaReconcileEvery = 24 * time.Hour
	// quotaReconcileCallCount 成功调用增量达到它就校正一次。
	//
	// # ⚠ **这是代理指标**（别当精算）
	//
	// 真值 = "自上次查询以来上游侧的待上报累计"，它住在**各上游内部**
	//（`quota_consumed.go:22-33`：由上游自己累计、`RefreshQuota` 成功时清空），
	// 核心**没有访问器**。所以这里用 `pool.Status.SuccessCount` 的**增量**当代理：
	//
	//	语义差异：SuccessCount 增量 ≠ 消耗量（一次成功调用可能消耗 0），
	//	         所以 200 只是"大概够久、该校正了"的**粗颗粒**触发，不是精算。
	//	将来上游若把累加器暴露出来，只改 RefreshDueQuotasNow 里取 callsSince 那两行。
	quotaReconcileCallCount = 200

	// ② 没回执的上游：分层窗口与间隔。
	//
	// ⚠ 比第一版分层（3m/15m/60m）更宽 —— 用户明确说"3 分钟没必要"。
	quotaHotWindow  = 10 * time.Minute
	quotaWarmWindow = 60 * time.Minute
	quotaHotEvery   = 10 * time.Minute
	quotaWarmEvery  = 30 * time.Minute
	quotaColdEvery  = 6 * time.Hour

	// ③ 死号跳过：连续这么多轮"刷完还是拿不到数据"⇒ 视为刷不动、暂停刷新。
	//
	// ⚠ 判据是 **HasData==false**（拿不到数据），**不是**"数值没变"：
	//   有数据但数值不变是**正常的**（那个号真的没消耗），跳掉它面板就再也不更新了。
	quotaStaleRounds = 3
)

// quotaTapProviders 有回执（余额算得出来）的上游。
//
// # ⚠ 这是 T-029 裁决的 **(b) 方案：admin 侧单一出处名单**
//
// 理想判据是"该上游声明了 UsageTapSpec 且 Mode=UsageConsumed" —— 但 `UsageTapSpec`
// 是上游**局部构造**的（loomy `provider.go:400`、qoder `consumed.go:74`），gateway
// 没有访问器、`ExtOf[T]` 也取不到，所以要动 gateway + 2 个上游包才能查。
// 裁决：不值得（3 处改动换一个查表可得的事实）⇒ 名单。
//
// ⚠ **将来若 (a) 落地，只改这个函数体**（所有调用点都不用动）。
// ⚠ 名单与实现漂移由 `TestQuotaTapProvidersMatchSource` 守着（去源码里找
//
//	`UsageTapSpec{` + `UsageConsumed`）—— 多写/少写都会红。
//
// ⚠ `qodercn` 与 `qoder` 共用同一份 `consumed.go` 的 tap，所以两个 id 都要在名单里。
func quotaTapProviders() map[string]bool {
	return map[string]bool{
		"loomy":   true, // provider.go  UsageTapSpec{Key:"points_consumed", Mode:UsageConsumed}
		"qoder":   true, // consumed.go  UsageTapSpec{Key:"credits",         Mode:UsageConsumed}
		"qodercn": true, // 同上（同一个 consumed.go，两个 provider id）
	}
}

// isTapBacked 该上游的余额能不能**算**出来。名单外一律当"不支持回执"。
func isTapBacked(provider string) bool { return quotaTapProviders()[provider] }

// quotaTierState 进程内的"上次刷到它是什么时候"。
//
// # ⚠ 为什么放内存而不是往 pool.Auth 加字段
//
// ① 它是**刷新器的内部账本**，不是账号属性：账号属性会被序列化进状态文件、
//
//	被别的路径读到，而这个时刻只对"这条循环"有意义。
//
// ② 丢了没有后果：重启后所有 lastQuotaAt 归零 ⇒ 第一轮全刷一次
//
//	（正是"面板不该是空的"所需要的），之后重新分层。
//
// ③ 加字段要动 `internal/pool` —— 那是**所有上游的选号入口**的公共结构；
//
//	为一个"丢了也没事"的时间戳去动它，风险收益不划算（本次刻意不动 pool）。
//
// ⚠ 并发：后台循环 + 手工端点都会读写，用一把小锁。键是 uid。
type quotaTierState struct {
	mu      sync.Mutex
	lastAt  map[string]time.Time
	callsAt map[string]int64
	usedAt  map[string]time.Time // 上次刷新时该号的 LastUsed（选号侧判分层用）
	stale   map[string]int       // uid → 连续"刷完仍拿不到数据"的轮数
	dead    map[string]bool      // uid → 已暂停刷新
	prov    map[string]string
}

var quotaTier = &quotaTierState{
	lastAt: map[string]time.Time{}, callsAt: map[string]int64{}, usedAt: map[string]time.Time{},
	stale: map[string]int{}, dead: map[string]bool{}, prov: map[string]string{},
}

// usedSeen 上次刷新时该 uid 的 LastUsed（选号侧那个"严重过期"判据要用它分层）。
func (s *quotaTierState) usedSeen(uid string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usedAt[uid]
}

// lastSeen 上次刷这个 uid 的时刻（零值 = 从没刷过 ⇒ 首轮必刷）。
func (s *quotaTierState) lastSeen(uid string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAt[uid]
}

// callsSeen 上次刷这个 uid 时的成功调用数（算"增量"用，见 quotaReconcileCallCount）。
func (s *quotaTierState) callsSeen(uid string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callsAt[uid]
}

// isDead 这个 uid 是否已暂停刷新（死号跳过）。
func (s *quotaTierState) isDead(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead[uid]
}

// markSeen 记下"刚刚刷过这批 uid"（含成功调用数快照）。
//
// ⚠ **成功与失败都记**：失败也要等它那一档过去再重试，
//
//	否则一个死号会被每一拍都撞一次（那正是"每 3 分钟白烧一次"的另一半）。
//
// ⚠ 这里**不再**记 `LastUsed`（曾计划过）：选号侧的陈旧判据改成了
// `func(uid string, lastUsed time.Time) bool`，那个 lastUsed 由 **pool 自己传**
// （`e.lastUsed` 就在它手上）—— 于是 admin 不必抄一份、也就不必 import auth。
// 少一个字段、少一处同步点。
func (s *quotaTierState) markSeen(uids []string, calls map[string]int64, used map[string]time.Time, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, uid := range uids {
		s.lastAt[uid] = now
		if calls != nil {
			s.callsAt[uid] = calls[uid]
		}
		if used != nil {
			s.usedAt[uid] = used[uid]
		}
	}
}

// provSeen 该 uid 记过的 provider（noteRound 写的）。
func (s *quotaTierState) provSeen(uid string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prov[uid]
}

// StalenessChecker 产出"这个号的额度已经**严重**过期、不该拿它做选号决策"的判据。
//
// 签名是 `func(uid string, lastUsed time.Time) bool` —— **不接收 `*auth.Auth`**：
//
//	① admin 因此**不需要 import internal/auth**（少一条依赖边，也就不会有循环依赖的风险）；
//	② pool 那边 `e.lastUsed` 本来就在手上，直接传过来最省事（不必让 admin 抄一份账）。
//
// # 为什么不是 `dueForRefresh` 的取反（裁决里点出的关键差别）
//
//	dueForRefresh=true ⇒ **该刷了**（数据稍旧没关系，照旧能用）
//	stale             ⇒ **不能用**（严重过期，拿它决策会出错）
//
// 两者不是一回事：刚过期一分钟的号仍然可用。所以这里加了**容忍倍数 2**：
//
//	没回执：热 2×10m=20m ｜ 温 2×30m=60m ｜ 冷 2×6h=12h
//	有回执：2×24h=48h
//
// # 为什么要容忍而不是"一过期就排除"
//
// 排除有代价（候选变少 ⇒ 更容易走到回退/503）。只有当数据**严重**过期时，
// "基于错数决策"的危害才超过"少一个候选"的代价；而有回执的上游余额本来就是
// **算**出来的（允许在 24h 内飘），把 48h 当"严重"与那个设计对齐。
//
// ⚠ 返回的闭包会在 pool 的**选号热路径**上被调用 ⇒ 只做 map 查表 + 时间比较，无 IO。
// ⚠ 三条保护在 pool 侧（默认 nil / 全排除回退 / 只告警一次）——本函数不承担它们。
// ⚠ **默认不接线**（见 cmd/server 的装配点；按裁决"先上线不启用"）。
func StalenessChecker() func(uid string, lastUsed time.Time) bool {
	return func(uid string, lastUsed time.Time) bool {
		// ⚠ now 在这里取（而不是由调用方传）：这个闭包是选号热路径上调用的，
		//   “现在”就该是调用的那一刻。
		now := time.Now()
		if uid == "" {
			return false // 判不出来就不排除（保守方向：宁可给号）
		}
		last := quotaTier.lastSeen(uid)
		if last.IsZero() {
			return false // 从没刷过 ⇒ 不排除（"还没账"的首轮状态不该让池子空）
		}
		var every time.Duration
		if isTapBacked(quotaTier.provSeen(uid)) {
			every = quotaReconcileEvery
		} else {
			switch quotaTierOf(lastUsed, now) {
			case "hot":
				every = quotaHotEvery
			case "warm":
				every = quotaWarmEvery
			default:
				every = quotaColdEvery
			}
		}
		return now.Sub(last) >= 2*every // ★ 容忍倍数 2：只有"严重过期"才排除
	}
}

// noteRound 记"这一轮刷完之后，这个号到底有没有拿到数据"，返回状态是否变化。
//
// # 为什么用 HasData 当判据（T-029 裁决）
//
// `RefreshQuota` 不返回失败原因（只有 `(QuotaView, bool)`），所以"按错误文本分类"
// 做不到。但那三个刷不动的号（codearts 401 / lobsterai 未登录 / qoder 未给出余额）
// 有一个**共同特征**：`HasData==false`（拿不到数据）。
// ⇒ 不需要原因也能达到目的，且零 gateway 改动。
//
// ⚠ 判据是 `HasData==false`，**不是**"数值没变"：有数据但数值不变的号是正常的
// （真的没消耗），跳掉它面板就再也不更新了。
func (s *quotaTierState) noteRound(uid, provider string, hasData bool) (stateChanged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prov[uid] = provider
	if hasData {
		changed := s.dead[uid] || s.stale[uid] != 0
		delete(s.stale, uid)
		delete(s.dead, uid)
		return changed // 任何一次拿到数据 ⇒ 立刻恢复刷新
	}
	s.stale[uid]++
	if !s.dead[uid] && s.stale[uid] >= quotaStaleRounds {
		s.dead[uid] = true
		return true
	}
	return false
}

// deadList 当前被暂停的 uid → provider（供"状态变化时打一行"用）。
func (s *quotaTierState) deadList() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.dead))
	for uid := range s.dead {
		out[uid] = s.prov[uid]
	}
	return out
}

// resetQuotaTierStateForTest 清空账本（**测试专用**；生产路径不调用它）。
//
// ⚠ 必须有它：账本是包级变量（见上面的理由②），没有它测试之间会互相污染 ——
//
//	而"上一轮刷过"会静默地把下一轮变成空刷，症状正是"测试偶发通过"。
func resetQuotaTierStateForTest() {
	quotaTier.mu.Lock()
	defer quotaTier.mu.Unlock()
	quotaTier.lastAt = map[string]time.Time{}
	quotaTier.callsAt = map[string]int64{}
	quotaTier.usedAt = map[string]time.Time{}
	quotaTier.stale = map[string]int{}
	quotaTier.dead = map[string]bool{}
	quotaTier.prov = map[string]string{}
}

// quotaTierOf 这个账号属于哪一层（只用于"没回执"那一支；供日志与测试可读性）。
func quotaTierOf(lastUsed, now time.Time) string {
	idle := now.Sub(lastUsed)
	switch {
	case !lastUsed.IsZero() && idle < quotaHotWindow:
		return "hot"
	case !lastUsed.IsZero() && idle < quotaWarmWindow:
		return "warm"
	default:
		return "cold" // 含"从没被用过"（零值）：它的额度不会自己变
	}
}

// dueForRefresh 这个账号这一拍该不该刷（**纯函数** ⇒ 全部组合能零网络钉住）。
//
// 签名里的两种模式：
//
//	tapBacked=true  （有回执，余额靠算）：24 小时**或**成功调用增量 ≥200 次，先到者刷
//	tapBacked=false （没回执，只能刷）  ：热 10m / 温 30m / 冷 6h
//
// # 为什么抽成纯函数
//
// 错的方向有两个，而**两个都不报错**：
//
//	刷太勤 → 请求量白烧（用户这次报的问题）
//	刷太懒 → 有回执的号偏差**无界**（算出来的余额越飘越远，界面看着还挺合理）
//
// 而循环体带 `time.Sleep`，决策留在里面就只能靠"等"来测。
func dueForRefresh(tapBacked bool, lastUsed, lastQuotaAt time.Time, callsSince int64, now time.Time) bool {
	// 从没刷过 ⇒ 必刷。
	//
	// ⚠ 这条不能省：否则"刚启动 ⇒ 所有号都没账 ⇒ 一天/六小时不刷"，
	//   面板会空白很久 —— 那正是用户最初报的"前端显示过时数据"。
	if lastQuotaAt.IsZero() {
		return true
	}
	if tapBacked {
		return now.Sub(lastQuotaAt) >= quotaReconcileEvery || callsSince >= quotaReconcileCallCount
	}
	var every time.Duration
	switch quotaTierOf(lastUsed, now) {
	case "hot":
		every = quotaHotEvery
	case "warm":
		every = quotaWarmEvery
	default:
		every = quotaColdEvery
	}
	return now.Sub(lastQuotaAt) >= every
}

// orderQuotaRefreshTargets 把待刷账号按"最近被用过"排前面，**原地**改 list 的顺序。
//
// # 为什么是"排序"而不是"只刷最近用过的"
//
//	只刷用过的 → 冷号的额度**永远旧**，而选号可能挑到它 ⇒ 基于错数决策 ✗
//	排序       → **还是刷全部**（请求量不变），只是把"最可能已经变了"的排前面 ✓
//
// ⚠ 为什么"最近被选中"就等于"额度一定变了"：一次调用无论成功失败都会消耗额度
// （所以用 `LastUsed` 而不是 `LastSuccessTime` —— 失败的调用同样花钱）。
// ⚠ 这个次序只在**上游偶发失败**时才看得出来（先刷到的是最该刷的那批），
//
//	而那是常态不是异常 —— 实测一轮 76 个号里失败 1 个。
//
// ⚠ 用 `SliceStable`：`List()` 本身按 uid 有序，稳定排序能让"从没被用过"那些
//
//	保持原有次序（它们全部是零值，比较结果相同）⇒ 行为可预测。
//
// # 为什么抽成独立函数
//
// ⚠ 我第一版把它内联在 `RefreshAllQuotasNow` 里，然后发现**变异测试抓不到它** ——
// 把 `After` 改成 `Before`（也就是"每轮先刷从没用过的号"）时，**没有任何测试会红**。
// 而那个错误的症状是：真正该刷的号被挤到一轮的后面，上游偶发失败时它们
// 就一直是旧值 —— **不报错、不崩，只是数据慢慢变旧**，正是本仓最难发现的那类 bug。
func orderQuotaRefreshTargets(list []pool.Status) {
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].LastUsed.After(list[j].LastUsed)
	})
}

// quotaBackoffStep 自适应降速的**决策本身**，抽成纯函数。
//
// ⚠ 为什么不把它留在循环里：循环体带 `time.Sleep`，测它要真的等上一分钟；
//
//	而"退让的判据"是这条循环里**最容易错、错了又最不容易发现**的一段 ——
//	判错的方向有两个（该退不退 ⇒ 猛撞上游；不该退却退 ⇒ 额度悄悄变慢），
//	两个都不会报错。抽出来才能把每个方向都钉住。
//
// 语义：
//   - 这一轮正常（bad=false）⇒ 连续计数归零、间隔**立刻**回到基准
//     （退让是为了**让路**，不是为了惩罚上游；上游一好就该全速刷）
//   - 连续 `quotaAutoRefreshFailRun` 轮不正常 ⇒ 间隔翻倍，封顶 `quotaAutoRefreshMaxEvery`
//     ⚠ 封顶的意义：无限退让会让"上游恢复了、额度却再也不更新"变成一个**没有任何信号**
//     的状态（这条循环是唯一在刷额度的地方）。
func quotaBackoffStep(bad bool, badRuns int, every time.Duration) (int, time.Duration) {
	if !bad {
		return 0, quotaAutoRefreshEvery
	}
	badRuns++
	if badRuns >= quotaAutoRefreshFailRun && every < quotaAutoRefreshMaxEvery {
		every *= 2
		if every > quotaAutoRefreshMaxEvery {
			every = quotaAutoRefreshMaxEvery
		}
	}
	return badRuns, every
}

// StartQuotaAutoRefresh 起后台循环，按 `quotaAutoRefreshEvery` 刷**全部账号**的额度。
//
// ⚠ 幂等（`sync.Once`）：重复调用不会起第二个循环。
// ⚠ 不 await：立刻返回。
// ⚠ 每一轮都自带 `recover` ⇒ 一轮崩了不影响下一轮，更不影响进程。
func (h *Handler) StartQuotaAutoRefresh() {
	if h == nil || h.cfg.Pool == nil {
		return
	}
	h.quotaAutoRefreshOnce.Do(func() {
		log.Printf("loomy: 额度自动刷新循环已启动 —— 每 %v 一轮，首轮 %v 后",
			quotaAutoRefreshEvery, quotaAutoRefreshFirstDelay)
		go func() {
			// ⚠ 这一层的 recover 是整个 goroutine 的兜底：一旦漏出去，
			//   循环就**永久停摆**了，而症状是"额度又变回旧的" —— 靠日志才能发现。
			defer func() {
				if r := recover(); r != nil {
					log.Printf("loomy: ⚠ 额度自动刷新循环**退出**了（panic=%v）—— 额度会停止自动更新", r)
				}
			}()
			time.Sleep(quotaAutoRefreshFirstDelay)
			// ⚠ **自适应降速**（设计第 5 节"全局预算"的最后一道闸门）。
			//
			// 判据是**失败比例**而不是绝对数：实测 76 个号里失败 1 个是正常抖动
			//（那一天的上游就是这样），而 76 个里失败 40 个才是"上游/网络出问题了"。
			// ⚠ 连续 `quotaAutoRefreshFailRun` 轮高失败 ⇒ 间隔翻倍，封顶
			//   `quotaAutoRefreshMaxEvery`；任一**轮正常** ⇒ 立刻恢复原间隔。
			//
			// ⚠ 为什么"立刻恢复"而不是"慢慢恢复"：退让是为了**让路**，不是为了惩罚上游。
			//   上游一好就该全速刷（额度是选号权重，慢了直接影响选号）。
			// ⚠ 为什么封顶 30 分钟：无限退让会让"上游恢复了、额度却再也不更新"
			//   变成一个**没有任何信号**的状态（这条循环是唯一在刷它的东西）。
			// ⚠ 起点是**节拍**而不是 `quotaAutoRefreshEvery`（2026-11-02 改）：
			//   分层之后"每一轮刷全部"不再成立，节拍只决定"多久检查一次谁到期"。
			//   节拍取 1 分钟 ⇒ 热号（3 分钟一层）最多晚 1 分钟刷到。
			every := quotaAutoRefreshEvery
			badRuns := 0
			for {
				res, due, total := func() (r quotaRefresher, due, total int) {
					defer func() {
						if rec := recover(); rec != nil {
							log.Printf("loomy: 额度自动刷新**这一轮** panic 已被隔离：%v", rec)
						}
					}()
					// ⚠⚠ 分层：只刷**已到期**的那批（见 quotaHotEvery 那段注释）。
					//   手工端点的 `RefreshAllQuotasNow` 仍然全量 —— 用户点按钮就是要全量。
					return h.RefreshDueQuotasNow(time.Now())
				}()

				// 失败比例 > 一半视为"这一轮不正常"（分母含跳过/未知：
				// 它们也是"没刷到"的账号，只是原因不同）。
				//
				// ⚠ 空拍（这一拍没有任何账号到期）⇒ considered=0 ⇒ bad=false ⇒
				//   连续计数归零。这是对的：**没发请求就不该判定上游不好**。
				considered := res.Updated + res.Unknown + res.Failed
				bad := considered > 0 && res.Failed*2 > considered

				prev := every
				badRuns, every = quotaBackoffStep(bad, badRuns, every)
				if every != prev {
					if every > prev {
						log.Printf("loomy: ⚠ 额度刷新连续 %d 轮失败偏多（本拍失败 %d/%d，到期 %d/%d）⇒ 间隔退让到 %v",
							badRuns, res.Failed, considered, due, total, every)
					} else {
						log.Printf("loomy: 额度刷新恢复正常 ⇒ 间隔回到 %v", every)
					}
				}
				time.Sleep(every)
			}
		}()
	})
}

// RefreshDueQuotasNow 按分层刷新**已到期**的那批账号（定时循环的唯一入口）。
//
// 返回 (统计, 到期数, 账号总数) —— 后两个数是为了让调用方/测试看得见"这一拍
// 为什么便宜"：改前每拍都是 total，改后每拍通常只有个位数。
//
// ⚠ 空拍（没有任何账号到期）**不打日志、不发请求** —— 这是这次改动的收益本体：
//
//	老实现每 3 分钟必然打一行"账号 102 · 更新 97 …"，而其中绝大多数号
//	余额根本没变。
func (h *Handler) RefreshDueQuotasNow(now time.Time) (res quotaRefresher, due, total int) {
	if h == nil || h.cfg.Pool == nil {
		return quotaRefresher{}, 0, 0
	}
	list := h.cfg.Pool.List()
	total = len(list)
	// 到期批**内部**仍然把"最近用过的"排前面（上游偶发失败时，最该刷的先刷到）。
	orderQuotaRefreshTargets(list)

	dueUIDs := make([]string, 0, len(list))
	calls := make(map[string]int64, len(list))
	provs := make(map[string]string, len(list))
	used := make(map[string]time.Time, len(list))
	for _, st := range list {
		if st.UID == "" {
			continue
		}
		// ★ 死号跳过（T-029 ③）：连续 3 轮刷完仍拿不到数据 ⇒ 不再发请求。
		//
		// ⚠ 它在**算到期之前**就 continue，所以这些号**不计入** due/considered ——
		//   这正是"跳过的号不该影响失败率"（否则 3 个死号就能把 102 个号的
		//   失败率抬到降速阈值附近）。
		if quotaTier.isDead(st.UID) {
			continue
		}
		prov := st.Provider
		if prov == "" {
			prov = h.cfg.DefaultProvider
		}
		provs[st.UID] = prov
		used[st.UID] = st.LastUsed
		// ⚠ 代理指标：拿不到 Status 时 callsSince 记 0 ⇒ 只剩 24h 那条闸门兜底。
		if snapshot, ok := h.cfg.Pool.Status(st.UID); ok {
			calls[st.UID] = snapshot.SuccessCount
		}
		callsSince := calls[st.UID] - quotaTier.callsSeen(st.UID)
		if dueForRefresh(isTapBacked(prov), st.LastUsed, quotaTier.lastSeen(st.UID), callsSince, now) {
			dueUIDs = append(dueUIDs, st.UID)
		}
	}
	due = len(dueUIDs)
	if due == 0 {
		return quotaRefresher{}, 0, total
	}

	res = h.refreshQuotas(dueUIDs)
	// ⚠ 无论成败都记：失败也要等它那一档过去再重试（否则死号被每拍都撞）。
	quotaTier.markSeen(dueUIDs, calls, used, now)
	// ★ 死号判定：刷完之后**仍然拿不到数据**（HasData==false）⇒ 记一轮。
	changed := h.noteQuotaRound(dueUIDs, provs)
	// ⚠ 一行里必须同时有"到期/总数"与四个计数：
	//   只看 `更新 3` 会以为循环坏了；看到 `到期 3/102` 才知道那是**分层生效**。
	log.Printf("loomy: 额度自动刷新一拍（分层/校正）—— 到期 %d/%d · 更新 %d · 未知 %d · 失败 %d · 跳过 %d · 上游 %v",
		due, total, res.Updated, res.Unknown, res.Failed, res.Skipped, res.Providers)
	if changed {
		h.logQuotaPausedOnce()
	}
	return res, due, total
}

// noteQuotaRound 把"这一轮刷完有没有拿到数据"记进账本，返回"暂停集是否变化"。
//
// # 为什么要单独一步（而不是塞进 refreshQuotas）
//
// `refreshQuotas` 只回**聚合**计数（updated/unknown/failed），拿不到逐 uid 的
// `HasData`。而 pool 的 `Status` 恰好带着它（`Quota.HasData`）—— 所以这里
// 用"刷完之后回读池子"的方式拿到逐号事实，**不需要动 gateway/上游**（T-029 裁决）。
func (h *Handler) noteQuotaRound(uids []string, provs map[string]string) (changed bool) {
	if h == nil || h.cfg.Pool == nil {
		return false
	}
	for _, uid := range uids {
		hasData := false
		if st, ok := h.cfg.Pool.Status(uid); ok {
			hasData = st.Quota.HasData
		}
		if quotaTier.noteRound(uid, provs[uid], hasData) {
			changed = true
		}
	}
	return changed
}

// logQuotaPausedOnce 只有在"暂停集发生变化"时才打一行（不要每拍刷屏）。
//
// ⚠ 列的是 provider + uid 前缀（**不打完整 uid**：日志会被贴进 issue/聊天）。
func (h *Handler) logQuotaPausedOnce() {
	dead := quotaTier.deadList()
	if len(dead) == 0 {
		log.Printf("loomy: 额度刷新：暂停集已清空（凭据恢复或有数据了）⇒ 全部账号恢复刷新")
		return
	}
	parts := make([]string, 0, len(dead))
	for uid, prov := range dead {
		if len(uid) > 8 {
			uid = uid[:8]
		}
		if prov == "" {
			prov = "?"
		}
		parts = append(parts, prov+"/"+uid)
	}
	sort.Strings(parts)
	log.Printf("loomy: 额度刷新：%d 个账号刷不到数据，暂停刷新（恢复后自动继续）：%s",
		len(parts), strings.Join(parts, ", "))
}

// RefreshAllQuotasNow 立刻刷一轮全部账号的额度，并把结果打出来。
//
// ⚠ 分开导出是为了让"定时"这一层薄到不需要测试：循环只负责节拍，
//
//	真正的动作是这个函数，它可以直接被单测与排障调用。
//
// ⚠ 全量传 uid 是**安全**的：`refreshQuotas` 内部按**账号自己**的 provider 找
//
//	额度扩展点，没实现 `QuotaExt` 的上游走"跳过"分支（计入 `Skipped`，
//	不是错误）—— 所以这里不需要先筛出"哪个上游支持额度"。
func (h *Handler) RefreshAllQuotasNow() quotaRefresher {
	if h == nil || h.cfg.Pool == nil {
		return quotaRefresher{}
	}
	list := h.cfg.Pool.List()
	// ⚠⚠ **事件驱动**（设计第 2 节第 ③ 条，也是目标里的第 ④ 项）：
	// 把"最近被用过"的账号排前面 —— 而这是**排序**，不是"只刷用过的"。
	orderQuotaRefreshTargets(list)
	uids := make([]string, 0, len(list))
	for _, st := range list {
		if st.UID != "" {
			uids = append(uids, st.UID)
		}
	}
	if len(uids) == 0 {
		return quotaRefresher{}
	}

	res := h.refreshQuotas(uids)
	// ⚠ 手工全量刷也要记账（2026-11-02）：否则用户点完「刷新额度」，
	//   分层循环会在下一拍把刚刷过的那批**再刷一遍**（白花一遍请求）。
	calls := make(map[string]int64, len(uids))
	provs := make(map[string]string, len(uids))
	used := make(map[string]time.Time, len(uids))
	for _, uid := range uids {
		if snapshot, ok := h.cfg.Pool.Status(uid); ok {
			calls[uid] = snapshot.SuccessCount
		}
		if snapshot, ok := h.cfg.Pool.Status(uid); ok {
			provs[uid] = snapshot.Provider
			used[uid] = snapshot.LastUsed
		}
	}
	quotaTier.markSeen(uids, calls, used, time.Now())
	// ⚠ 手工刷也要走死号记账：**手工刷成功是死号恢复的唯一人工途径** ——
	//   用户点了「刷新额度」并且这次拿到数据了，那个号必须立刻恢复刷新
	//   （否则他会看到"我手动刷出来了，可自动刷新还是不管它"）。
	if h.noteQuotaRound(uids, provs) {
		h.logQuotaPausedOnce()
	}
	// ⚠⚠ 这四个数是**分开**计的（`refreshQuotas` 的注释里有理由），所以日志也分开打：
	//   把它们合成一个"成功数"会让"某个上游整片被跳过"（Skipped）彻底不可见 ——
	//   而那正是本仓最需要人介入的状态。
	log.Printf("loomy: 额度自动刷新一轮（手工全量）—— 账号 %d · 更新 %d · 未知 %d · 失败 %d · 跳过 %d · 上游 %v",
		len(uids), res.Updated, res.Unknown, res.Failed, res.Skipped, res.Providers)
	return res
}
