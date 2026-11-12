// quota_refresh.go 核心把**上游自报的额度**写回账号池。
//
// # 核心在这里只做一件事
//
// **把结果写回池。** 它不解释额度怎么算 —— 打哪个端点、怎么解析、
// 取不到时算不算"有数据"，全部由上游的 gateway.QuotaExt 回答。
// 这样加第 N 个上游时本文件零改动（判据 1）。
//
// # 为什么需要这条路径（T2 的根因）
//
// 改造前 `/admin/accounts` 的 `quota`/`credits` **只有 workbuddy 有值**：
//
//	workbuddy  POST /admin/credits/refresh
//	           → workbuddy.RefreshCredits
//	           → client.UserResource（真上游）
//	           → pool.SetCredits            ← 它自己会把结果写回池
//
// 而 `/admin/accounts` **自己不查任何东西**，它只是把 pool.Status 内嵌进
// AccountView 就返回（见 admin.go 的 accountViews）。所以字段有没有值，
// **完全取决于有没有人调过 SetQuota / SetCredits**。
//
// codearts 一条写回路径都没有 → 它的额度落到 pool.restoreQuota 的兜底分支
// → 被造成 `{kind:credits, has_data:true, remaining:0}` 的**假数据**
// （"从没查过"被伪装成"查过了，确实是 0"）。
//
// 这条路径把那个洞补上：**核心遍历所有上游，问它们各自的额度，写回池。**
package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"sync"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// quotaRefreshConcurrency 额度刷新的并发上限（★ T-436）。
//
// # 为什么是 20（用户 2026-11-09 定）
//
//	「我希望统一设置并发为20。因为我现在感觉特别慢。」
//
// 改造前完全串行：106 个账号 ≈ 106 秒（每个约 1 秒）⇒ 现在 ≈ 6 批 ≈ 6 秒。
//
// # 为什么不能再高
//
// 并发 20 = 同时打上游 20 个请求。loomy 有 45 个号、codar 面板明确写着
// 「上游已有风控记录，请低频使用」⇒ 20 是"明显变快"与"不给上游压力"之间的折中。
//
// ⚠ 三条路径共用 refreshQuotas（手动单上游 / 手动全部 / 后台自动分层刷新）
//
//	⇒ 这个值同时决定了三者的并发上界。
//
// ⚠ 它必须住在**产品文件**里：放测试文件里产品就引用不到（T-435/T-436 各栽过一次）。
const quotaRefreshConcurrency = 20

// quotaRefresher 一次额度刷新的统计（供端点回执与测试断言）。
type quotaRefresher struct {
	// Updated 成功拿到数据并写回池的账号数。
	Updated int
	// Unknown 上游明确回答"我不知道额度"的账号数（界面显示 `—`）。
	//
	// ⚠ 与 Failed 分开计数：这是**正常情况**，不是错误。
	// 混在一起会让"上游不报额度"看起来像故障，从而被误修。
	Unknown int
	// Failed 上游返回 nil 或刷新过程出错（账号解析不了等）的账号数。
	Failed int
	// Skipped 因上游未实现 QuotaExt 而**未被问过额度**的账号数。
	//
	// ⚠ 与 Unknown 区分：
	//   Unknown = "问了，上游说不知道"（正常情况，不该刷错误日志）
	//   Skipped = "压根没问" —— 它意味着某个上游**完全没有额度接入**，
	//            是本文件唯一真正需要人介入的状态。
	//
	// 这个字段存在的理由：本文件曾经对"没实现 QuotaExt 的上游"静默 continue，
	// 于是"某个上游整片账号没被刷到"在日志/回执/界面上**全部不可见**。
	Skipped int
	// Providers 本次真正被问过额度的上游（排序后，供回执/日志）。
	//
	// ⚠ 注意它的语义边界：它只收录**真正问过**的上游。被跳过（未实现
	// QuotaExt）的上游**不会**出现在这里 —— 所以单独看 Providers
	// 无法区分"这个上游只有 0 个账号"和"这个上游整片被跳过"。
	// 后者只能靠 Skipped + 下面那条按 provider 去重的跳过日志来暴露。
	Providers []string

	// ── 下面三个字段只被**手动端点**消费（"刷新本上游额度"按钮那三条路径）──
	//
	// # 为什么把"结论"回传而不是在 refreshQuotas 里直接处置
	//
	// 因为 refreshQuotas 被**三条路径共用**（手动单上游 / 手动全部 / 后台自动分层刷新），
	// 而后台那条是**定时**跑的：把"失败即摘号"接在它上面，一次上游故障就会在几轮之内
	// 把整池号刷成禁用（5 次阈值 × 每 3 分钟一轮 ≈ 15 分钟）。
	// ⇒ 处置只挂在用户明确点了按钮的那条路径上（见 applyQuotaRefreshPolicy）。
	//
	// AuthFailedUIDs 上游报告"这次查询因凭据失效而失败"的账号。
	//
	// ⚠ 它**不是** Failed 的子集：Failed 是"账号解析不了/上游返回 nil"，
	// 而 loomy 的凭据失效走的是 `UnknownQuota(), true`。两者来源不同、处置也不同。
	AuthFailedUIDs []string
	// FreshUIDs 真正拿到数据（HasData=true）的账号 —— 它们此刻**可证明**是活的。
	//
	// ⚠ Unknown（问了但没数据）**不算**：没有证据证明凭据可用，
	// 拿它去解禁会把"查不到"当成"好了"。
	FreshUIDs []string
}

// refreshQuotas 遍历池中所有账号，按各自的上游问额度并写回。
//
// # 为什么要按 provider 分派而不是直接问"当前上游"
//
// 账号池是**多上游共用**的（每个 entry 带 provider 标签）。
// 拿一个"当前上游"去刷全部账号，会把 A 的额度写到 B 的账号上 ——
// 那是比不刷新更糟的错（显示了看起来合理的错数）。
//
// # 没有 QuotaExt 的上游怎么办
//
// **跳过，并且不写池。** 账号保持 `HasData=false` → 界面显示 `—`。
// 这正是用户要的"有什么显示什么，没有的话就不显示" ——
// 不填 0（0 会被误读成"没有额度了"）。
func (h *Handler) refreshQuotas(uids []string) quotaRefresher {
	var out quotaRefresher
	if h.cfg.Pool == nil || h.cfg.Registry == nil {
		return out
	}

	// provider → 该上游的额度扩展点。**只对实现了 QuotaExt 的上游建映射**，
	// 没实现的天然不在表里 → 走"跳过"分支。
	exts := map[string]gateway.QuotaExt{}
	// authExts：实现了 QuotaAuthFailureExt 的上游 —— 它们能回答"这次失败是不是凭据失效"。
	// 没实现的上游不在表里 ⇒ 它的失败**不会被**当成凭据问题（保守方向）。
	authExts := map[string]gateway.QuotaAuthFailureExt{}
	for _, p := range h.cfg.Registry.All() {
		if ext, ok := gateway.ExtOf[gateway.QuotaExt](p); ok {
			exts[p.ID()] = ext
		}
		if ae, ok := gateway.ExtOf[gateway.QuotaAuthFailureExt](p); ok {
			authExts[p.ID()] = ae
		}
	}

	seen := map[string]bool{}
	// loggedSkip 保证"某个上游没实现 QuotaExt"只打**一行**日志。
	//
	// # 为什么必须去重
	//
	// 这个循环是按账号跑的：一个来自没接额度的上游如果有 50 个账号，
	// 不去重就会打 50 行同样的日志 —— 真正的故障（failed 的堆栈）
	// 会被自己的噪音淹没。日志的价值在"有无"，不在"条数"。
	loggedSkip := map[string]bool{}
	// ── 第一阶段（**串行**）：筛出"该真刷"的号 ─────────────────────────────
	//
	// 这一段保持原样：跳过计数、按 provider 去重的日志、Providers 的收集
	// 全都依赖**遍历顺序** ⇒ 放到并发里会让计数与日志顺序变味。
	// ⚠ 这也保证 `out.Skipped` / `out.Providers` 与改造前**逐字一致**。
	type quotaJob struct {
		uid string
		ext gateway.QuotaExt
		// authExt 可能为 nil（该上游不报"失败原因"）。
		// ⚠ 在**筛选阶段**就取好：收尾阶段只该读结论，不该再查注册表。
		authExt gateway.QuotaAuthFailureExt
	}
	var todo []quotaJob
	for _, uid := range uids {
		st, ok := h.cfg.Pool.Status(uid)
		if !ok {
			continue
		}
		// ⚠ 用**账号自己**的 provider 去找上游，不是全局默认上游。
		// 空串按默认上游解释 —— 与 pool 的 providerOf 语义一致
		//（旧状态文件没有 provider 字段，那批账号属于默认上游）。
		provider := st.Provider
		if provider == "" {
			provider = h.cfg.DefaultProvider
		}
		ext, ok := exts[provider]
		if !ok {
			// 该上游不报额度 → 保持未知，不写池（行为不变）。
			//
			// ⚠ 但**必须记下来**：这里曾经是一句光秃秃的 continue。
			// 于是"某个上游整片账号压根没被问过"在日志、回执、界面上
			// 全部不可见 —— 这正是本文件那个 bug 能藏住的直接原因。
			// 计数 + 按 provider 去重的日志，让它一眼可见。
			out.Skipped++
			if !loggedSkip[provider] {
				loggedSkip[provider] = true
				log.Printf("admin: 额度刷新跳过上游 %q（未实现 gateway.QuotaExt）—— 它的额度在界面上将恒为未知", provider)
			}
			continue
		}
		if !seen[provider] {
			seen[provider] = true
			out.Providers = append(out.Providers, provider)
		}
		todo = append(todo, quotaJob{uid: uid, ext: ext, authExt: authExts[provider]})
	}

	// ── 第二阶段（**并发**）：只做"问上游"这一件事 ────────────────────────
	//
	// # 为什么是 worker 池 + 信号量，而不是给每个 uid 起一个 goroutine
	//
	// 106 个号 = 106 个 goroutine 同时打上游 ⇒ 没有上限 ⇒ 那是风控事故。
	// 信号量把"同时在飞"钉在 quotaRefreshConcurrency（20）。
	//
	// # 为什么只并发"取数"，聚合留在串行
	//
	// 循环体里除了发请求，还有 out.Failed/Unknown/Updated 计数、SetQuota 写池 ——
	// 那些都不是并发安全的。把它们留在主 goroutine 里做 ⇒ **无锁、无竞态**，
	// 而且计数结果与串行版**逐字一致**（顺序都没变）。
	//
	// ⚠ panic 隔离仍由 safeRefreshQuota 的 recover 保证：它在 **worker 内**被调用
	//   ⇒ 某个上游违约只让**那一个**号算 failed，其余照常（这正是它存在的理由）。
	type quotaRes struct {
		qv gateway.QuotaView
		ok bool
	}
	results := make([]quotaRes, len(todo)) // 按**下标**写回 ⇒ 每个槽只被一个 goroutine 写
	sem := make(chan struct{}, quotaRefreshConcurrency)
	var wg sync.WaitGroup
	for i := range todo {
		wg.Add(1)
		sem <- struct{}{} // ★ 上限：同时在飞 ≤ 20
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			qv, ok := h.safeRefreshQuota(todo[i].ext, todo[i].uid)
			results[i] = quotaRes{qv: qv, ok: ok}
		}(i)
	}
	wg.Wait()

	// ── 第三阶段（**串行**）：聚合计数 + 写回 ────────────────────────────
	//
	// 与改造前的循环体逐字等价（同样的顺序、同样的四路分支）。
	for i := range todo {
		r := results[i]
		// ⚠ 凭据失效的判定**在下面三个分支之外**先做一次：它可能同时
		// 满足其它分支（loomy 的失效走 Unknown），而它是唯一驱动"摘号"的结论。
		// 只读一次（纯函数，不发请求）。
		if todo[i].authExt != nil && todo[i].authExt.QuotaAuthFailed(todo[i].uid) {
			out.AuthFailedUIDs = append(out.AuthFailedUIDs, todo[i].uid)
		}
		if !r.ok {
			out.Failed++
			continue
		}
		if !r.qv.HasData {
			// 上游如实回答"我不知道" —— **不写池**（写 0 就是造假）。
			out.Unknown++
			continue
		}
		h.cfg.Pool.SetQuota(todo[i].uid, toPoolQuota(r.qv))
		out.Updated++
		out.FreshUIDs = append(out.FreshUIDs, todo[i].uid)
	}

	sort.Strings(out.Providers)
	// 收尾汇总：一行看全四个计数。
	//
	// ⚠ skipped 必须出现在这里。它和 unknown 是**完全不同**的东西：
	// unknown 是上游如实回答"不知道"（正常），skipped 是"我们压根没问"
	// （某个上游没接额度）。只报 unknown 会让后者伪装成前者。
	log.Printf("admin: 额度刷新完成 updated=%d unknown=%d failed=%d skipped=%d providers=%v",
		out.Updated, out.Unknown, out.Failed, out.Skipped, out.Providers)
	return out
}

// safeRefreshQuota 调上游的 RefreshQuota，把 panic 兜住。
//
// # 为什么要 recover
//
// gateway 契约要求实现方不得 panic，但这条路径是**遍历所有上游**跑的：
// 一个上游的 bug 会让整轮刷新中断，**连累其它正常上游的额度也刷不出来**。
// 额度是展示性信息，任何单点异常都只该降级成"这个号未知"。
//
// 同样的手法在 workbuddy.dosageHint 里已有一份（理由相同）。
func (h *Handler) safeRefreshQuota(ext gateway.QuotaExt, uid string) (qv gateway.QuotaView, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("admin: 额度刷新 panic（上游实现违约）uid=%s: %v", uid, r)
			qv, ok = gateway.QuotaView{}, false
		}
	}()
	return ext.RefreshQuota(uid)
}

// toPoolQuota 把上游的额度视图投影成 pool 的类型。
//
// # 为什么需要这一层转换（而不是共用一个类型）
//
// gateway 不得依赖 internal/pool（它是"上游与核心之间的唯一接缝"，
// 必须保持零核心依赖，否则加一个上游就要连带改 pool）。
// 反过来 pool 也不该依赖 gateway。于是两边各声明一份，
// 由**装配层**（这里）逐字段直译 —— 与 SettingsExts 同一手法。
//
// 转换是**无信息损失**的：字段一一对应。
func toPoolQuota(q gateway.QuotaView) pool.QuotaView {
	out := pool.QuotaView{
		Kind:      q.Kind,
		Remaining: q.Remaining,
		HasData:   q.HasData,
		// 「会按周期重置的那部分」（Loomy 的每日赠送）。
		// ⚠ 两个字段**成对**搬：只搬 Free 不搬 FreeKnown 会让
		// "拿不到分项"退化成"分项是 0"，界面显示「免费额度已用完」——
		// 而那正是本列最容易骗人的形态（看起来完全合理）。
		Free:      q.Free,
		FreeKnown: q.FreeKnown,
		// 分项的周期上限（MonkeyCode 的每日 token 上限）。
		// 有了它界面才能显示「剩余 / 上限」；漏搬就只显示孤立的剩余量，
		// 读者看不出"今天总共能跑多少"。0 = 上游没给上限，前端不显示分母。
		FreeLimit: q.FreeLimit,
	}
	if len(q.ByModel) > 0 {
		// 深拷贝：pool 会把这份 map 存进 entry，之后可能被其它 goroutine 读。
		// 直接递上游的 map 会让上游后续改动**绕过池的锁**改到池内状态
		//（pool.QuotaView.Clone 的注释记着这个真实缺陷）。
		m := make(map[string]int64, len(q.ByModel))
		for k, v := range q.ByModel {
			m[k] = v
		}
		out.ByModel = m
	}
	return out
}

// refreshAccountQuotas GET/POST /admin/accounts/quota/refresh —— 刷新额度并写回池。
//
// 语义与 /admin/credits/refresh（workbuddy 专有）的区别：
//
//	/admin/credits/refresh        单上游（workbuddy），且会顺手做"解冻"判断
//	/admin/accounts/quota/refresh **全上游通用**：取额度 + 写回池 + **刷新结论处置**
//
// # 处置是什么（2026-11-10 加，用户要求）
//
// 用户原话："刷新失败了需要把它禁用掉，因为刷新失败有可能是账号过期了；
// 但是有可能禁用的账号刷新成功，就要解除禁用。"
//
//	上游报告"这次失败是**凭据失效**" ⇒ 记一次自动禁用失败（连续 N 次才摘，见 policy 文件）
//	真正拿到数据                     ⇒ 记一次自动禁用成功（只解禁本机制禁掉的号）
//
// ⚠ 判定依据**只能由上游给**：QuotaExt 只回 (QuotaView, ok)，没有错误原文，
// 而 loomy 的失败统一返回 UnknownQuota（与"本机没客户端目录"这类正常情况同形）。
// 所以新增了 gateway.QuotaAuthFailureExt：上游自报"这次失败是不是凭据失效"。
// 没实现它的上游**行为完全不变**（既不摘号也不恢复）。
//
// ⚠ 这条处置**只作用于手动调用**。后台的分层刷新共用 refreshQuotas，但**不**处置 ——
// 定时路径上"失败即摘号"会在一次上游故障后 15 分钟内把整池号刷成禁用。
//
// # 三种作用域（用户报的问题：「行内按钮却刷了全部」）
//
// 用户实测：在账号池里点某一行的「额度」按钮，**所有账号的额度都被刷新了**。
// 原因是本端点以前**不收任何参数**，恒为全池刷新 ——
// 而前端行内按钮与顶部「刷新全部额度」打的是同一个 URL，
// 于是"行内"这个作用域只存在于按钮的视觉位置里，后端根本不知道。
//
// 现在按可选参数分三档：
//
//	（无参数）            全部账号        顶部「刷新全部额度」
//	{"uid":"..."}         只刷这一个账号   行内「额度」按钮
//	{"provider":"..."}    只刷该上游的账号 上游分组标题上的「刷新本上游额度」
//	{"provider":"...","only_disabled":true}
//	                      只探测该上游**已禁用**的账号，且**探测证明可用就解除禁用**
//	                      上游分组上的「探测已禁用账号」按钮（用户 2026-11-12 要求）
//
// 第 4 档与前三档的**动作不同**：前三档只「取额度 + 写回池 + 按证据记账」，
// 第 4 档会**改变账号的启用状态**（解禁）。判据与理由见 applyDisabledProbePolicy。
//
// # 向后兼容
//
// 不带参数时行为与改造前**逐字段相同**（全池刷新）。既有调用方
// （含前端顶部按钮、脚本、curl）一行都不用改。
func (h *Handler) accountsQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil {
		writeError(w, http.StatusNotImplemented, "账号池未接线")
		return
	}
	// 请求体可缺（GET 或空 body 都合法 = 全池刷新）。
	var body struct {
		UID      string `json:"uid"`
		Provider string `json:"provider"`
		// OnlyDisabled 只探测**已禁用**的账号（用户 2026-11-12 要求，见下面的作用域说明）。
		OnlyDisabled bool `json:"only_disabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	// query 参数也认（便于 curl 与脚本按作用域调用，不必构造 JSON）。
	if body.UID == "" {
		body.UID = r.URL.Query().Get("uid")
	}
	if body.Provider == "" {
		body.Provider = r.URL.Query().Get("provider")
	}
	// only_disabled 同样认 query（curl 探测用）。刻意不用 strconv.ParseBool：
	// 它把 "yes" 判成 false，而人写 query 时会写 yes —— 与上面两条的宽松口径一致。
	if !body.OnlyDisabled {
		switch r.URL.Query().Get("only_disabled") {
		case "1", "true", "yes":
			body.OnlyDisabled = true
		}
	}

	all := h.cfg.Pool.List()
	var uids []string
	scope := "all"

	switch {
	case body.UID != "":
		// 单账号：先确认它在池里，否则 404 而不是"刷了 0 个"。
		scope = "account"
		want := body.UID
		for _, st := range all {
			if st.UID == want {
				uids = append(uids, want)
				break
			}
		}
		if len(uids) == 0 {
			writeError(w, http.StatusNotFound, "账号不存在: "+want)
			return
		}
	case body.Provider != "":
		// 按上游：该上游自己在池里的全部账号。
		//
		// ⚠ only_disabled=true 时只取**已禁用**的号（用户 2026-11-12 要求）：
		//   语义是「探测这些被摘掉的号还能不能用，能用就放回池子」。
		//
		//   与整片刷新分开的理由是**效率**（用户原话）：某上游 175 个号里可能只有 3 个是禁用的，
		//   为了探这 3 个去刷 175 个号 = 白打 172 次上游，而且那些正常号本来也不需要证明。
		scope = "provider"
		if body.OnlyDisabled {
			scope = "provider_disabled"
		}
		pid := body.Provider
		for _, st := range all {
			if h.providerOf(st) != pid {
				continue
			}
			if body.OnlyDisabled && !st.Disabled {
				continue
			}
			uids = append(uids, st.UID)
		}
		if len(uids) == 0 {
			// ⚠ 两种「空」必须分开报：
			//   上游一个号都没有       ⇒ 404（配置问题，得去导入）
			//   有号但都不是禁用状态   ⇒ 200 + count=0（正常：没有可探测的号）
			// 只留一个 404 会把后者也说成「该上游在池里没有账号」—— 那是排错方向上的误导。
			if body.OnlyDisabled {
				break // 落到收尾：回 200（前端据此说明「没有已禁用的账号」）
			}
			writeError(w, http.StatusNotFound, "该上游在池里没有账号: "+pid)
			return
		}
	default:
		uids = make([]string, 0, len(all))
		for _, st := range all {
			uids = append(uids, st.UID)
		}
	}

	res := h.refreshQuotas(uids)
	// 用户点按钮 ⇒ 顺手处置。⚠ 只在这条**手动**路径上做（后台分层刷新共用 refreshQuotas
	// 但不处置，见 policy 文件头 —— 定时路径上「失败即摘号」会误杀整池）。
	//
	// ⚠ 两档处置**互斥**，按作用域只跑一档：
	//   provider_disabled（探测禁用号）⇒ 只做「可用则解禁」
	//   其余三档                      ⇒ 做「凭据失效记账 / 刷新成功恢复」
	//
	// 为什么禁用号那档不顺手记失败：这一档的输入集**本来就是禁用号**，
	// 把「凭据失效」再喂一遍自动禁用线不改变任何状态，却可能把这批号标成
	// 「本机制置位」⇒ 与人工禁用的归属混在一起（而归属决定谁能自动解禁）。
	var disabled, recovered, enabled int
	if scope == "provider_disabled" {
		enabled = h.applyDisabledProbePolicy(res)
	} else {
		disabled, recovered = h.applyQuotaRefreshPolicy(res)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":   res.Updated,
		"disabled":  disabled,
		"recovered": recovered,
		// enabled 只对 provider_disabled 档有意义（其余档恒 0）。
		"enabled":   enabled,
		"unknown":   res.Unknown,
		"failed":    res.Failed,
		"skipped":   res.Skipped,
		"providers": res.Providers,
		// Scope 告诉前端"这次刷的是哪一档"，回执文案据此措辞
		//（"已刷新 3 个账号" vs "已刷新全部 12 个账号"）。
		"scope": scope,
		"count": len(uids),
		// 回执里带上刷新后的账号视图，前端一次调用就能重绘表格。
		"accounts": h.accountViews(),
	})
}

// providerOf 取账号所属上游（空则落默认上游，与 accountViews 同一判据）。
func (h *Handler) providerOf(st pool.Status) string {
	if st.Provider != "" {
		return st.Provider
	}
	return h.cfg.DefaultProvider
}
