// adminpoints.go loomy 的两个**专属标签页**背后的端点：新手任务 与 邀请码。
//
// # 为什么它们住在 loomy 里，而不是账号池里
//
// 用户的要求原文：「不要去改账号池的 ui 了，账号池保持简单统一就行，
// 多的直接写 loomy 专属标签页里面去」。
//
// 架构上正好也是这么分的：
//
//	账号池（accountRow）    只画**每个上游都一样**的那几列：上游/昵称/UID/额度/状态/操作
//	上游专属标签页          承载"只有这个上游才有的整块功能"
//
// 通用面板的生成是**数据驱动**的（见 webui.html 的 buildProviderPanels）：
// 上游声明一个能力位 + 提供一条**非 hidden 的 GET 路由**，导航里就会出现
// 一个标签页，内容由该能力位对应的渲染器填。所以这里加两条 GET
// （tasks / invite）就够了 —— 前端**不认识 loomy 这个名字**。
//
// # 账号从哪来（本包不需要核心给）
//
// 凭证目录就是 `p.authDir`，格式是本包自己的 `loomy*.json`。
// 所以"遍历本上游所有账号"是自足的（见 authByUID 的同款理由）。
// 这正是"加新上游核心零改动"那条判据在**管理端点**上的体现。
package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 本轮新增的路径。
const (
	tasksPath      = "/admin/loomy/tasks"
	tasksAutoPath  = "/admin/loomy/tasks/auto"
	tasksAllPath   = "/admin/loomy/tasks/auto-all"
	invitePath     = "/admin/loomy/invite"
	inviteBindPath = "/admin/loomy/invite/bind"
)

// accountRef 一个 loomy 账号在管理端点里的投影。
type accountRef struct {
	UID      string
	Nickname string
	Session  string
}

// accounts 列出本上游凭证目录里的全部账号。
//
// 读不到就是空列表（**不是错误**）：面板显示"还没有账号"比报 500 有用。
func (p *Provider) accounts() []accountRef {
	list, err := LoadDir(p.authDir)
	if err != nil {
		return nil
	}
	out := make([]accountRef, 0, len(list))
	for _, a := range list {
		if a.UID == "" || a.Session == "" {
			continue
		}
		out = append(out, accountRef{UID: a.UID, Nickname: a.Nickname, Session: a.Session})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// restrictToUID 支持 `?uid=` 只查一个账号（面板逐行取数用）。
//
// # 为什么需要它（用户要求：先加载网页，再按需加载里面的数据）
//
// 面板一次查 35 个账号，上游单次 0.2~23.7 秒 ⇒ 整张表空着转 20 秒以上，
// 用户的原话是「网页还是加载不出来」。现在前端先渲染表格骨架，再**逐行**
// 请求（每行一次 `?uid=`）—— 第一行约 1 秒就出现，慢账号只拖慢它自己。
//
// ⚠ 找不到该 uid 时返回 **nil**（不是全部）：把"这个号不在池里"当成
// "查全部"会让前端那一行显示成别人的数据 —— 一个静默的串号。
func restrictToUID(accts []accountRef, uid string) []accountRef {
	if uid == "" {
		return accts
	}
	for _, x := range accts {
		if x.UID == uid {
			return []accountRef{x}
		}
	}
	return nil
}

// panelFillConcurrency 「分块补数」（`?uids=`）这一条路的并发上限。
//
// # ⚠ 为什么它比 loomyPanelConcurrency 更小
//
// 共享层（webui.html 的 lazyFill）一次只发**一个块**，于是后端这条路的
// 并发度就是**全局**在途账号数：每个账号 2 次上游请求 ⇒ 4 个账号并行
// = 8 条在途上游请求。取 6 会到 12 条，正好压在实测出 502 的那条线上
//（见 loomyPanelConcurrency 的说明）。取 4 = 留一半余量。
//
// # 它和"前端逐行取数"的区别（回归的根因）
//
// 旧模型是**前端**发 6 条并发 HTTP，每条只查 1 个账号 ⇒ 上游 12 条在途
// ⇒ 502 ⇒ 整列「查询失败」。现在并发上限只有一个地方（这里），
// 前端再也不可能把它乘掉。
// ⚠⚠ 用户要求把并发提到 25（2026-10-18：「性能太差了 提高并发数25个」）。
//
// 权衡如实记在这里：本文件与 `points.go` 的注释都记着**实测「12 条并发打上游就出 502」**，
// 而每个账号要 2 次上游请求 ⇒ 25 个账号并行 = **50 条在途**，是那条线的 4 倍。
// ⇒ 如果面板又开始整列「查询失败」，**把本常量与 `points.go` 的 loomyPanelConcurrency
//    一起退回 4**（退回即可恢复"8 条在途"的安全区），并把 `webui.html` 的
//    `LAZY_CHUNK` 一并退回（块大小与这里配套）。
//
// ⚠ 另注：块大小（`LAZY_CHUNK`）与本常量要**配套**改 —— 块里只有 6 个 uid 时，
// 本常量提到 25 也用不上（一块最多 6 个账号并行）。
const panelFillConcurrency = 25

// parseUIDList 解析 `?uids=a,b,c`。
func parseUIDList(q string) []string {
	if strings.TrimSpace(q) == "" {
		return nil
	}
	raw := strings.Split(q, ",")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// restrictToUIDs 支持 `?uids=a,b,c` 一次取**一小块**账号。
//
// 与 restrictToUID 同一条原则：**不认识的名字不进结果**（绝不退化成"查全部"，
// 那会把别人的数据画进这一行）。缺的那几个由调用方补成 error 行 —— 见
// fillMissing。
func restrictToUIDs(accts []accountRef, uids []string) []accountRef {
	want := make(map[string]bool, len(uids))
	for _, u := range uids {
		want[u] = true
	}
	out := make([]accountRef, 0, len(uids))
	for _, a := range accts {
		if want[a.UID] {
			out = append(out, a)
		}
	}
	return out
}

// fillMissing 给"请求了但池子里没有"的 uid 各补一行 error。
//
// ⚠ 为什么必须补：前端按块替换行，块里少一个 uid 而它什么都不画，那一行就会
// **永远停在「正在读取…」** —— 表现得像卡住。补成 ok=false 才是实话：
// 「这个号不在凭证目录里」。
func fillMissing[T any](uids []string, got []T, uidOf func(T) string, mk func(string) T) []T {
	have := make(map[string]bool, len(got))
	for _, v := range got {
		have[uidOf(v)] = true
	}
	out := got
	for _, u := range uids {
		if !have[u] {
			out = append(out, mk(u))
		}
	}
	return out
}

// panelPageSize 两个面板每页固定行数（用户要求写死 25）。
//
// # 为什么固定、而不是可配
//
// 分页在这里的目的不是排版，而是**把每次打开页面的上游查询量限住**：
// 25 个账号 × 2 次请求，而不是"全部账号"（会随账号数一直涨）。
const panelPageSize = 25

// panelPage 解析 ?page=（1 基）并返回合法区间 [lo, hi)。
//
// 越界**钳到合法范围**（而不是报错）：翻页按钮与手工改 URL 都可能给出越界值，
// 而"回到最后一页"比"报错"更符合用户预期。
func panelPage(q string, total int) (page, pages, lo, hi int) {
	pages = (total + panelPageSize - 1) / panelPageSize
	if pages < 1 {
		pages = 1
	}
	fmt.Sscanf(q, "%d", &page)
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	lo = (page - 1) * panelPageSize
	hi = lo + panelPageSize
	if hi > total {
		hi = total
	}
	return page, pages, lo, hi
}

// panelRowCache 面板行的**服务端缓存**（用户要求：一打开先显示缓存数据）。
//
// # ⚠ 为什么缓存放服务端，而不是只放前端
//
// 用户要的是"一打开页面显示是缓存数据 + 一句正在更新"。缓存若只在前端，
// 那么**换浏览器 / 清过缓存之后第一次打开**仍然是空白等 20 秒 —— 而那正是
// 「网页加载不出来」的原始形态。放服务端则任何客户端第一眼就有数据。
//
// ⚠ 它只按 uid 存"最近一次成功的结果"，**不做失效判定**：面板是展示性的，
// "这条多旧"由前端显示的「正在更新」与逐行刷新来表达，不必在这里猜 TTL。
type panelRowCache[T any] struct {
	mu sync.RWMutex
	m  map[string]T
	at time.Time
	// onChange 每次写入后回调（2026-10-18 加）—— 用于**落盘**。
	//
	// # 为什么必须落盘（用户 2026-10-18 的原话）
	//
	//	"是不是本地文件里面有这些账号的额度等信息，前端只需要直接加载缓存就行了啊？
	//	 还需要等待几分钟？一毫秒就行了啊"
	//
	// ⚠ 他说得对：`state.json` 是落盘的 ⇒ "账号池"的额度**一毫秒出**；
	//   而这两个面板缓存**只是内存** ⇒ ⚠ **进程一重启就空** ⇒
	//   只能重新查上游（每号 2 次请求）⇒ **"要等几分钟"的真正来源**。
	// ⇒ 落盘之后：启动即读盘 ⇒ 前端**一毫秒**见到上次的数据，后台再慢慢刷新 ✓
	//
	// ⚠ 用回调而不是让本类型自己知道路径：它对"存哪、怎么存"一无所知（泛型缓存不该管磁盘）。
	onChange func()
}

func newPanelRowCache[T any]() *panelRowCache[T] {
	return &panelRowCache[T]{m: map[string]T{}}
}

func (c *panelRowCache[T]) get(uid string) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[uid]
	if !ok {
		return zero, false
	}
	return v, true
}

func (c *panelRowCache[T]) set(uid string, v T) {
	if c == nil || uid == "" {
		return
	}
	c.mu.Lock()
	c.m[uid] = v
	c.at = time.Now()
	cb := c.onChange
	c.mu.Unlock()
	// ⚠⚠ **回调必须在锁外调用**：
	//   落盘那条路（throttledSavePanels → savePanelsSnapshot）会去 `RLock` 同一把锁，
	//   而 Go 的 RWMutex **不允许同一个 goroutine 递归取读锁** ⇒ 在锁内调就是死锁。
	// ⚠ 这也是"捕获回调解引用、解锁后再调"这个写法的唯一理由。
	if cb != nil {
		cb()
	}
}

// updatedAt 最近一次写入的时刻（前端据此显示「更新于 …」）。
func (c *panelRowCache[T]) updatedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.at
}

// panelsFileName 面板缓存的落盘文件名（放在 authDir 下 —— 那是**网关自己**的目录，
	// 不是本机 Loomy 客户端的 clientDataDir）。
	const panelsFileName = "panels.json"

	// panelsSnapshot 两个面板缓存的落盘形态。
	//
	// ⚠ 为什么整份存而不是逐条存：它只有几十条（44 个号 × 2 个面板），
	//   整份序列化是最简单的正确选择 —— 逐条 append 会让"重启后读到半份"成为可能。
	type panelsSnapshot struct {
		Tasks   map[string]accountTasksView  `json:"tasks"`
		Invite  map[string]accountInviteView `json:"invite"`
		SavedAt time.Time                    `json:"saved_at"`
	}

	// loadPanelsSnapshot 启动时读一次落盘的面板缓存。
	//
	// ⚠ 文件不存在、读不动、JSON 坏了 —— **一律当空**（不是错误）：
	//   它只是缓存，缺了顶多"第一次打开要查上游"，而**报错会让整个面板起不来**。
	func (p *Provider) loadPanelsSnapshot() panelsSnapshot {
		var s panelsSnapshot
		if p == nil || p.authDir == "" {
			return s
		}
		raw, err := os.ReadFile(filepath.Join(p.authDir, panelsFileName))
		if err != nil {
			return s
		}
		_ = json.Unmarshal(raw, &s)
		return s
	}

	// savePanelsSnapshot 把两个面板缓存落盘（原子写：先写 .tmp 再 rename）。
	//
	// ⚠ 原子写是必须的：非原子写在写到一半时崩溃会留下**半份 JSON**，
	//   下次启动读它解析失败 ⇒ 整份缓存白丢（而且静默）。
	func (p *Provider) savePanelsSnapshot() {
		if p == nil || p.authDir == "" {
			return
		}
		s := panelsSnapshot{Tasks: map[string]accountTasksView{}, Invite: map[string]accountInviteView{}, SavedAt: time.Now()}
		if c := p.panelTasks; c != nil {
			c.mu.RLock()
			for k, v := range c.m {
				s.Tasks[k] = v
			}
			c.mu.RUnlock()
		}
		if c := p.panelInvite; c != nil {
			c.mu.RLock()
			for k, v := range c.m {
				s.Invite[k] = v
			}
			c.mu.RUnlock()
		}
		raw, err := json.Marshal(s)
		if err != nil {
			return
		}
		dst := filepath.Join(p.authDir, panelsFileName)
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return
		}
		_ = os.Rename(tmp, dst)
	}

	// throttledSavePanels 存盘的**节流**入口（每次补一个号都会写缓存 ⇒ 不能每次都落盘）。
	//
	// ⚠ 5 秒最多存一次：一轮 sweep 最多补 8 个号 ⇒ 不节流就是 8 次整份序列化写盘，
	//   而缓存本来就不要求强一致（丢了顶多重查）。
	// ⚠ 用单独一把锁而不是复用 panelMu：store 是 IO，不该把面板读写路径拖住。
	func (p *Provider) throttledSavePanels() {
		if p == nil {
			return
		}
		p.saveMu.Lock()
		defer p.saveMu.Unlock()
		if time.Since(p.lastSaveAt) < 5*time.Second {
			return
		}
		p.lastSaveAt = time.Now()
		go p.savePanelsSnapshot()
	}

	// tasksPanel / invitePanel 两个面板的行缓存（惰性初始化）。
//
// ⚠ 惰性而不是在构造函数里建：Provider 有多个构造入口（New / NewWithConfig /
// 测试夹具），漏一个就会出现"这个入口下缓存是 nil" —— 而 nil 接收者上的
// get/set 是安全的（已经在上面处理），所以宁可惰性 + 空安全。
func (p *Provider) tasksPanel() *panelRowCache[accountTasksView] {
	p.panelMu.Lock()
	defer p.panelMu.Unlock()
	if p.panelTasks == nil {
		p.panelTasks = newPanelRowCache[accountTasksView]()
		// ⚠⚠ **启动即读盘**（AGENTS 第 9 条：本地文件缓存 ⇒ 前端读缓存 ⇒ 毫秒级）。
		//
		// 顺序必须是"先灌盘、后挂 onChange"：
		// 灌盘走的也是 set()，那时若已挂上回调就会**在读盘过程中触发一串写盘**（纯浪费）。
		for uid, v := range p.loadPanelsSnapshot().Tasks {
			p.panelTasks.set(uid, v)
		}
		// ⚠ 落盘挂在 onChange 上（见 panelRowCache.onChange）：补一个号就（节流地）存一次。
		p.panelTasks.onChange = p.throttledSavePanels
	}
	return p.panelTasks
}

func (p *Provider) invitePanel() *panelRowCache[accountInviteView] {
	p.panelMu.Lock()
	defer p.panelMu.Unlock()
	if p.panelInvite == nil {
		p.panelInvite = newPanelRowCache[accountInviteView]()
		// ⚠ 与 tasksPanel 同一处理：先灌盘、后挂 onChange（理由见那里）。
		for uid, v := range p.loadPanelsSnapshot().Invite {
			p.panelInvite.set(uid, v)
		}
		p.panelInvite.onChange = p.throttledSavePanels
	}
	return p.panelInvite
}

// ─ 新手任务 ───────────────────────────────────────────────────────────

// accountTasksView 一个账号的任务状态。
type accountTasksView struct {
	UID      string          `json:"uid"`
	Nickname string          `json:"nickname"`
	OK       bool            `json:"ok"`
	Tasks    map[string]bool `json:"tasks,omitempty"`
	Done     int             `json:"done"`
	Total    int             `json:"total"`
	Earned   int64           `json:"earned"`
	Max      int64           `json:"max"`
	Balance  int64           `json:"balance"`
	// BalanceKnown **缓存里有没有 balance 这个键**（2026-10-18 加）。
	//
	// ⚠ 与 LocalPoints.BalanceKnown 同源：int64 的零值**分不清**"确实是 0"
	//   与"这一次没读到" ⇒ 前端只能把两者都画成 `0`，
	//   ⚠ 而一个一直在成功发请求的号被显示成"额度 0"，用户会怀疑账号废了（实测报过）。
	// ⚠ 三处结构（本结构 / accountTaskRunResult / accountInviteView）**必须都带上**它，
	//   否则"运行任务后回填那一行"会把已知的值降级成未知。
	BalanceKnown bool `json:"balance_known"`
	Pending  []string        `json:"pending,omitempty"`
	Error    string          `json:"error,omitempty"`
	// Stale 这一行是**缓存占位**（还没读到），不是查询结果。
	//
	// ⚠ 与 OK=false 必须分开：OK=false + Error 是"查失败了"，
	// 而 Stale=true 是"这一次还没查"（缓存未命中）。两者在前端是
	// 两种显示（「查询失败」vs「正在读取…」），混起来会让用户以为坏号。
	Stale bool `json:"stale,omitempty"`
}

// handleTasksList GET /admin/loomy/tasks —— 每个账号的任务进度。
func (p *Provider) handleTasksList(w http.ResponseWriter, r *http.Request) {
	all := p.accounts()
	totalN := len(all)
	q := r.URL.Query()

	// ── `?uids=a,b,c` 一次取**一小块**（webui 共享层的分块懒加载用）──
	//
	// ⚠ 与下面 `?uid=` 的唯一区别是**粒度**。保留 `?uid=` 是为了不破坏老调用方；
	// 但前端已经改成"一个块一条 HTTP" —— 逐行取数（前端自己叠 6 并发）
	// 正是整列「查询失败」的成因，见 panelFillConcurrency。
	if uids := parseUIDList(q.Get("uids")); len(uids) > 0 {
		one := restrictToUIDs(all, uids)
		out := make([]accountTasksView, len(one))
		fanOutConcurrent(len(one), panelFillConcurrency, func(i int) {
			out[i] = p.tasksViewFor(one[i])
		})
		out = fillMissing(uids, out,
			func(v accountTasksView) string { return v.UID },
			func(u string) accountTasksView {
				v := accountTasksView{UID: u, Total: len(TaskDefs), Max: taskMaxPoints(),
					Error: "这个账号不在凭证目录里"}
				for _, a := range all {
					if a.UID == u {
						v.Nickname = maskDisplay(a.Nickname)
						break
					}
				}
				return v
			})
		var earned, mx int64
		for _, v := range out {
			earned += v.Earned
			mx += v.Max
		}
		writeLoomyJSON(w, http.StatusOK, map[string]any{
			"defs": TaskDefs, "accounts": out,
			"page": 1, "pages": 1, "total": len(out), "size": panelPageSize,
			"updated_at": p.tasksPanel().updatedAt(),
			"summary": map[string]any{"accounts": len(out), "earned": earned,
				"max": mx, "pending": countPending(out)},
		})
		return
	}

	// ── `?uid=` 只查这一个（老调用方；前端已不再用）并写进服务端缓存 ──
	if uid := strings.TrimSpace(q.Get("uid")); uid != "" {
		one := restrictToUID(all, uid)
		out := make([]accountTasksView, len(one))
		fanOutConcurrent(len(one), loomyPanelConcurrency, func(i int) {
			out[i] = p.tasksViewFor(one[i])
		})
		var earned, mx int64
		for _, v := range out {
			earned += v.Earned
			mx += v.Max
		}
		writeLoomyJSON(w, http.StatusOK, map[string]any{
			"defs": TaskDefs, "accounts": out,
			"page": 1, "pages": 1, "total": len(out), "size": panelPageSize,
			"updated_at": p.tasksPanel().updatedAt(),
			"summary": map[string]any{"accounts": len(out), "earned": earned,
				"max": mx, "pending": countPending(out)},
		})
		return
	}

	// ── `?page=` **只读缓存、零上游请求** ──
	//
	// 这是用户设计的核心：打开页面先看到"上一轮的数据"，而不是空表等 20 秒。
	// 没缓存的行带 stale=true，前端据此显示「正在读取…」并随后逐行刷新。
	if q.Get("page") != "" {
		page, pages, lo, hi := panelPage(q.Get("page"), totalN)
		out := p.tasksCachedPage(all[lo:hi])
		var earned, mx int64
		for _, v := range out {
			if v.OK {
				earned += v.Earned
				mx += v.Max
			}
		}
		writeLoomyJSON(w, http.StatusOK, map[string]any{
			"defs": TaskDefs, "accounts": out,
			"page": page, "pages": pages, "total": totalN, "size": panelPageSize,
			"from_cache": true, "updated_at": p.tasksPanel().updatedAt(),
			"summary": map[string]any{"accounts": len(out), "earned": earned,
				"max": mx, "pending": countPending(out)},
		})
		return
	}

	// ── 都不带：与改造前逐字节相同（全量查 + 顺手写缓存）──
	//
	// ⚠ 特意保留这条：后端先上、前端分页后跟，不存在"后端改了前端没跟上"
	// 那种半截状态 —— 面板不会因为分页这次改动而坏掉。
	out := make([]accountTasksView, totalN)
	fanOutConcurrent(totalN, loomyPanelConcurrency, func(i int) {
		out[i] = p.tasksViewFor(all[i])
	})
	var totalEarned, totalMax int64
	for _, v := range out {
		totalEarned += v.Earned
		totalMax += v.Max
	}
	writeLoomyJSON(w, http.StatusOK, map[string]any{
		"defs":     TaskDefs,
		"accounts": out,
		"page":     1, "pages": 1, "total": totalN, "size": panelPageSize,
		"updated_at": p.tasksPanel().updatedAt(),
		"summary": map[string]any{
			"accounts": len(out),
			"earned":   totalEarned,
			"max":      totalMax,
			"pending":  countPending(out),
		},
	})
}

// tasksViewFor 查一个账号的任务与余额，并写进服务端缓存。
//
// ⚠ **只有成功的结果才写缓存**：把一次失败的查询缓存起来，会让"上次失败"
// 在下一次打开页面时被当成**已确认的事实**显示出来（而它可能只是抖了一下）。
func (p *Provider) tasksViewFor(a accountRef) accountTasksView {
	v := accountTasksView{UID: a.UID, Nickname: maskDisplay(a.Nickname),
		Total: len(TaskDefs), Max: taskMaxPoints()}
	// ⚠ **每个账号一份独立预算**（见 withPointsTimeout 的事故记录）。
	withPointsTimeout(func(ctx context.Context) {
		tasks, err := p.Tasks(ctx, a.Session)
		if err != nil {
			v.Error = err.Error()
			return
		}
		v.OK = true
		v.Tasks = tasks
		for _, d := range TaskDefs {
			if tasks[d.Key] {
				v.Done++
				v.Earned += d.Points
			} else {
				v.Pending = append(v.Pending, d.Key)
			}
		}
		if bal, _, berr := p.Balance(ctx, a.Session); berr == nil {
			v.Balance = bal
			// ⚠ 查到了 ⇒ 这一维是"已知"（见 accountTasksView.BalanceKnown）。
			v.BalanceKnown = true
		}
	})
	if v.OK {
		p.tasksPanel().set(a.UID, v)
	}
	return v
}

// panelWarmupInterval 面板补数的扫描间隔；panelWarmupBatch 每轮最多补几个号。
//
// ⚠ **为什么必须有这一对**（用户 2026-10-18 的诉求："打开页面毫秒加载整个页面数据"）：
//
// 前端打开面板时只请求**当页**，而 `?page=` 只从**服务端缓存**取 ——
// 没进缓存的行回 `stale: true`，前端据此画「正在读取…」。
// ⚠ 而"让那些行进缓存"的唯一途径是**真的去查一次上游**（`tasksViewFor` / `inviteViewFor`）。
// ⇒ 若补数只靠前端触发，就永远有洞：**没翻到的页不补、关掉页面就停**
//   （实测：第 2 页那 19 个号一次都没被请求过，于是永远停在「正在读取…」）。
// ⇒ 搬到服务端后，**无论前端何时打开、开在第几页，缓存都是满的**。
//
// ⚠ 限量与顺序都是必须的：补数要打上游（每号 2 次），而实测 **12 条并发就 502**
//   ⇒ 每轮只补 panelWarmupBatch 个、且**顺序**跑（下面那个 for 是串行的）。
//   ⚠ 与 freePoolSweepBatch 同一取舍：账号多时每个号的复查周期自动拉长。
const (
	panelWarmupInterval = 60 * time.Second
	panelWarmupBatch    = 8
)

// StartPanelWarmup 起一个后台循环，**持续**把没进缓存的面板行补上。
//
// ⚠ 形状**照抄** StartFreePoolSweep（`sync.Once` + recover + Ticker）——
//   本仓已有的后台任务都是这个形态，不新造。
// ⚠ 除了等第一个 tick，**启动时先立刻跑两轮**：冷启动后用户往往马上打开页面，
//   等 60 秒才补一轮太晚（而这两轮各限 8 个，代价可控）。
func (p *Provider) StartPanelWarmup() {
	if p == nil {
		return
	}
	p.warmOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("loomy: 面板预热 panic（已忽略）: %v", r)
				}
			}()
			// ⚠ 启动时先补两轮（后面交给 ticker）
			for i := 0; i < 2; i++ {
				p.sweepPanels()
			}
			t := time.NewTicker(panelWarmupInterval)
			defer t.Stop()
			for range t.C {
				p.sweepPanels()
			}
		}()
	})
}

// sweepPanels 跑一轮：把"还没进缓存"的号补几个。返回本轮实际补了几个。
//
// ⚠ 返回值为测试而留（生产不关心"这轮补了几个"）——与 sweepFreePool 同一约定。
// ⚠ tasks 与 invite **各补各的**：一条号可能 tasks 有缓存而 invite 没有，
//   只判其中一个会让另一半永远空着（那正是"新手任务好了、邀请码还是正在读取"的形态）。
func (p *Provider) sweepPanels() int {
	accts := p.accounts()
	if len(accts) == 0 {
		log.Printf("loomy: 面板预热一轮 —— **credentials 目录里读不到账号**（authDir=%q）⇒ 什么都没做", p.authDir)
		return 0
	}
	// ⚠ 先数清"还差多少"，再补 —— 这两行是**排障的入口**：
	//   没有它们，"预热有没有在动、为什么没补上"就只能靠猜（今天已经猜了很久）。
	todoT, todoI := 0, 0
	for _, a := range accts {
		if _, ok := p.tasksPanel().get(a.UID); !ok {
			todoT++
		}
		if _, ok := p.invitePanel().get(a.UID); !ok {
			todoI++
		}
	}
	n := 0
	var firstErr string
	for _, a := range accts {
		if n >= panelWarmupBatch {
			break
		}
		if _, ok := p.tasksPanel().get(a.UID); !ok {
			// ⚠ 这一步会打上游并**写进缓存**（`tasksViewFor` 末尾的 `if v.OK { set }`）
			// ⚠ 而**失败时它不写缓存**（那是对的：不把"没读到"固化成数据）——
			//   但也意味着失败会被**静默跳过**，所以下面把首条错误打出来。
			if v := p.tasksViewFor(a); !v.OK && firstErr == "" {
				firstErr = v.Error
			}
			n++
			continue
		}
		if _, ok := p.invitePanel().get(a.UID); !ok {
			if v := p.inviteViewFor(a); !v.OK && firstErr == "" {
				firstErr = v.Error
			}
			n++
			continue
		}
	}
	log.Printf("loomy: 面板预热一轮 —— 待补 tasks=%d invite=%d（共 %d 个号）· 本轮补 %d 个 · 首个错误=%q",
		todoT, todoI, len(accts), n, firstErr)
	return n
}

// tasksCachedPage 从服务端缓存取一页。**零上游请求。**
//
// 没缓存的行给一个 Stale 占位（uid/昵称是本地就有的）—— 前端据此渲染
// 「正在读取…」并随后逐行 `?uid=` 刷新。这样"打开页面"永远是瞬时的。
func (p *Provider) tasksCachedPage(accts []accountRef) []accountTasksView {
	out := make([]accountTasksView, len(accts))
	c := p.tasksPanel()
	for i, a := range accts {
		if v, ok := c.get(a.UID); ok {
			out[i] = v
			continue
		}
		out[i] = accountTasksView{UID: a.UID, Nickname: maskDisplay(a.Nickname),
			Total: len(TaskDefs), Max: taskMaxPoints(), Stale: true}
	}
	return out
}

// taskMaxPoints 全部任务的总分。
func taskMaxPoints() int64 {
	var n int64
	for _, d := range TaskDefs {
		n += d.Points
	}
	return n
}

// countPending 还有多少个未完成任务（合计）。
func countPending(list []accountTasksView) int {
	n := 0
	for _, v := range list {
		n += len(v.Pending)
	}
	return n
}

// taskRunResult 一次"一键完成"的结果。
type taskRunResult struct {
	UID       string   `json:"uid"`
	Nickname  string   `json:"nickname,omitempty"`
	Completed []string `json:"completed,omitempty"`
	Already   []string `json:"already,omitempty"`
	Failed    []string `json:"failed,omitempty"`
	Balance   int64    `json:"balance"`
	// BalanceKnown 见 accountTasksView.BalanceKnown（三处结构必须都带上）。
	BalanceKnown bool `json:"balance_known"`
	Error     string   `json:"error,omitempty"`
}

// runTasks 对一份凭证把还没完成的任务逐个点掉。
//
// # 为什么逐个 POST 而不是"一条全量端点"
//
// 上游只有 `POST /onboarding/tasks/complete {key}` 这一个粒度。
// 所以"一键完成"在**我们**这侧就是循环 —— 这也是它必须显示进度的原因
// （8 个任务 = 8 次往返，上游慢的时候会等几秒）。
//
// 已完成的会产生 `alreadyCompleted=true`，那不是失败（幂等），单列一栏。
func (p *Provider) runTasks(ctx context.Context, a accountRef) taskRunResult {
	res := taskRunResult{UID: a.UID, Nickname: maskDisplay(a.Nickname)}

	tasks, err := p.Tasks(ctx, a.Session)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	done := make(map[string]bool, len(tasks))
	for k, v := range tasks {
		done[k] = v
	}
	for _, d := range TaskDefs {
		if done[d.Key] {
			res.Already = append(res.Already, d.Key)
			continue
		}
		already, bal, cerr := p.CompleteTask(ctx, a.Session, d.Key)
		if cerr != nil {
			res.Failed = append(res.Failed, d.Key+": "+cerr.Error())
			continue
		}
		if already {
			// 上游说这次是重复点（并发/上一轮已经入账）——照记 already。
			res.Already = append(res.Already, d.Key)
		} else {
			res.Completed = append(res.Completed, d.Key)
		}
		// 余额取**最后一次**成功回执里的值（它每次都会带回最新余额）。
		if bal > 0 {
			res.Balance = bal
		// ⚠ 查到了 ⇒ 已知（见 accountTasksView.BalanceKnown）。
		res.BalanceKnown = true
		}
	}
	if res.Balance == 0 {
		if bal, _, berr := p.Balance(ctx, a.Session); berr == nil {
			res.Balance = bal
		// ⚠ 查到了 ⇒ 已知（见 accountTasksView.BalanceKnown）。
		res.BalanceKnown = true
		}
	}
	return res
}

// handleTasksAuto POST /admin/loomy/tasks/auto —— 一键完成**一个**账号。
func (p *Provider) handleTasksAuto(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := strings.TrimSpace(req.UID)
	if uid == "" {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "缺少 uid"})
		return
	}
	var target *accountRef
	for _, a := range p.accounts() {
		if a.UID == uid {
			cp := a
			target = &cp
			break
		}
	}
	if target == nil {
		writeLoomyJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有这个 loomy 账号: " + uid})
		return
	}
	ctx, cancel := pointsCtx()
	defer cancel()
	res := p.runTasks(ctx, *target)
	writeLoomyJSON(w, http.StatusOK, map[string]any{"ok": res.Error == "", "result": res})
}

// handleTasksAutoAll POST /admin/loomy/tasks/auto-all —— 一键完成**全部** loomy 账号。
func (p *Provider) handleTasksAutoAll(w http.ResponseWriter, r *http.Request) {
	accts := p.accounts()
	// `?uid=` = 只查这一个（面板逐行取数；见 restrictToUID）。
	accts = restrictToUID(accts, strings.TrimSpace(r.URL.Query().Get("uid")))
	results := make([]taskRunResult, len(accts))
	// 写操作更要限流：对同一个上游打太快会被限流，而每个账号要跑 8 个任务。
	fanOutConcurrent(len(accts), loomyPanelConcurrency, func(i int) {
		a := accts[i]
		// ⚠ 每个账号一份独立预算（见 withPointsTimeout 的事故记录）。
		// 这条更危险：共用预算会让**后面的账号一个任务都跑不成**，
		// 而界面上只显示"失败"，看不出是超时。
		withPointsTimeout(func(ctx context.Context) { results[i] = p.runTasks(ctx, a) })
	})
	var completed, failed int
	for _, res := range results {
		completed += len(res.Completed)
		failed += len(res.Failed)
	}
	writeLoomyJSON(w, http.StatusOK, map[string]any{
		"ok":        failed == 0,
		"results":   results,
		"accounts":  len(accts),
		"completed": completed,
		"failed":    failed,
	})
}

// ── 邀请码 ──────────────────────────────────────────────────────────────

// accountInviteView 一个账号的邀请码状态。
type accountInviteView struct {
	UID       string       `json:"uid"`
	Nickname  string       `json:"nickname"`
	OK        bool         `json:"ok"`
	Activated bool         `json:"activated"`
	Applied   string       `json:"applied,omitempty"`
	Balance   int64        `json:"balance"`
	// BalanceKnown 见 accountTasksView.BalanceKnown（三处结构必须都带上）。
	BalanceKnown bool `json:"balance_known"`
	Codes     []InviteCode `json:"codes,omitempty"`
	Error     string       `json:"error,omitempty"`
	// Stale 见 accountTasksView.Stale 的说明。
	Stale bool `json:"stale,omitempty"`
}

// handleInviteList GET /admin/loomy/invite —— 每个账号的激活状态 + 自己生成的码。
func (p *Provider) handleInviteList(w http.ResponseWriter, r *http.Request) {
	all := p.accounts()
	totalN := len(all)
	q := r.URL.Query()

	// `?uids=` 一次取一小块（见 handleTasksList 里的同款说明）。
	if uids := parseUIDList(q.Get("uids")); len(uids) > 0 {
		one := restrictToUIDs(all, uids)
		out := make([]accountInviteView, len(one))
		fanOutConcurrent(len(one), panelFillConcurrency, func(i int) {
			out[i] = p.inviteViewFor(one[i])
		})
		out = fillMissing(uids, out,
			func(v accountInviteView) string { return v.UID },
			func(u string) accountInviteView {
				v := accountInviteView{UID: u, Error: "这个账号不在凭证目录里"}
				for _, a := range all {
					if a.UID == u {
						v.Nickname = maskDisplay(a.Nickname)
						break
					}
				}
				return v
			})
		writeLoomyJSON(w, http.StatusOK, map[string]any{
			"accounts": out,
			"page":     1, "pages": 1, "total": len(out), "size": panelPageSize,
			"updated_at": p.invitePanel().updatedAt(),
			"summary":    inviteSummary(out),
		})
		return
	}

	if uid := strings.TrimSpace(q.Get("uid")); uid != "" {
		one := restrictToUID(all, uid)
		out := make([]accountInviteView, len(one))
		fanOutConcurrent(len(one), loomyPanelConcurrency, func(i int) {
			out[i] = p.inviteViewFor(one[i])
		})
		writeLoomyJSON(w, http.StatusOK, map[string]any{
			"accounts": out,
			"page":     1, "pages": 1, "total": len(out), "size": panelPageSize,
			"updated_at": p.invitePanel().updatedAt(),
			"summary":    inviteSummary(out),
		})
		return
	}

	if q.Get("page") != "" {
		page, pages, lo, hi := panelPage(q.Get("page"), totalN)
		out := p.inviteCachedPage(all[lo:hi])
		writeLoomyJSON(w, http.StatusOK, map[string]any{
			"accounts": out,
			"page":     page, "pages": pages, "total": totalN, "size": panelPageSize,
			"from_cache": true, "updated_at": p.invitePanel().updatedAt(),
			"summary": inviteSummary(out),
		})
		return
	}

	out := make([]accountInviteView, totalN)
	fanOutConcurrent(totalN, loomyPanelConcurrency, func(i int) {
		out[i] = p.inviteViewFor(all[i])
	})
	writeLoomyJSON(w, http.StatusOK, map[string]any{
		"accounts": out,
		"page":     1, "pages": 1, "total": totalN, "size": panelPageSize,
		"updated_at": p.invitePanel().updatedAt(),
		"summary":    inviteSummary(out),
	})
}

// inviteViewFor 查一个账号的激活状态与邀请码，并写进服务端缓存。
func (p *Provider) inviteViewFor(a accountRef) accountInviteView {
	v := accountInviteView{UID: a.UID, Nickname: maskDisplay(a.Nickname)}
	// ⚠ **每个账号一份独立预算**（见 withPointsTimeout 的事故记录）。
	withPointsTimeout(func(ctx context.Context) {
		act, applied, err := p.Activation(ctx, a.Session)
		if err != nil {
			v.Error = err.Error()
			return
		}
		v.OK = true
		v.Activated = act
		v.Applied = applied
		// 自己的码是**附加信息**：查不到不影响激活状态那一行（所以错误只丢进 Error）。
		codes, cerr := p.InvitationCodes(ctx, a.Session)
		if cerr == nil && len(codes) == 0 {
			// ⚠ 上游没给码但查询是成功的 → 大概率是"从没做过首登初始化"
			//（客户端每次登录后会自动调 first-login，而导入/添加的号跳过了这步）。
			// 实测：初始化后 invitation-codes 立刻返回 5 个码、并补发注册奖励。
			// 服务端有 alreadyProcessed 幂等保护，重复调用不会重复发奖 ——
			// 所以这里**自动补一次**，让"我生成的码"不用用户手动点任何东西就出现。
			if ferr := p.FirstLogin(ctx, a.Session); ferr == nil {
				codes, cerr = p.InvitationCodes(ctx, a.Session)
			} else {
				cerr = ferr
			}
		}
		if cerr == nil {
			v.Codes = codes
		} else if v.Error == "" {
			v.Error = "邀请码列表查询失败：" + cerr.Error()
		}
		if bal, _, berr := p.Balance(ctx, a.Session); berr == nil {
			v.Balance = bal
			// ⚠ 查到了 ⇒ 这一维是"已知"（见 accountTasksView.BalanceKnown）。
			v.BalanceKnown = true
		}
	})
	if v.OK {
		p.invitePanel().set(a.UID, v)
	}
	return v
}

// inviteCachedPage 从服务端缓存取一页（零上游请求）。见 tasksCachedPage。
func (p *Provider) inviteCachedPage(accts []accountRef) []accountInviteView {
	out := make([]accountInviteView, len(accts))
	c := p.invitePanel()
	for i, a := range accts {
		if v, ok := c.get(a.UID); ok {
			out[i] = v
			continue
		}
		out[i] = accountInviteView{UID: a.UID, Nickname: maskDisplay(a.Nickname), Stale: true}
	}
	return out
}

// inviteSummary 汇总（激活/未绑定数）。
//
// ⚠ Stale 占位行**不计入**分母：它们还没读到，算进"未绑定"会让界面
// 在刷新的那几秒里显示一个假的"未绑定 N"（而那个 N 会随后跳变）。
func inviteSummary(out []accountInviteView) map[string]any {
	known, activated := 0, 0
	for _, v := range out {
		if v.Stale {
			continue
		}
		known++
		if v.Activated {
			activated++
		}
	}
	return map[string]any{
		"accounts":  known,
		"activated": activated,
		"pending":   known - activated,
	}
}

// handleInviteBind POST /admin/loomy/invite/bind —— 给某个账号绑定邀请码。
//
// 回执带**绑定后重新查询**的激活状态：绑定是一次性的（码 maxUses=1），
// 前端据此立刻把那一行刷成"已激活"，不必等下一次列表刷新。
func (p *Provider) handleInviteBind(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID  string `json:"uid"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	uid := strings.TrimSpace(req.UID)
	code := strings.TrimSpace(req.Code)
	// ⚠ 全程日志（用户报"绑定失败：未知原因，且没有日志"）。
	// 每个分支都落一条，从"请求到达"起 —— 若复现后连到达行都没有，
	// 说明请求根本没进这个 handler（前端路径/页面缓存问题），那本身就是答案。
	log.Printf("loomy: 绑定邀请码请求到达 uid=%s code=%s", uid, code)
	if uid == "" || code == "" {
		log.Printf("loomy: 绑定邀请码被拒（uid/code 为空）uid=%q code=%q", uid, code)
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "uid 与 code 都不能为空"})
		return
	}
	if len(code) < 4 || len(code) > 32 {
		// 实测邀请码 6 位，但不设死 —— 上游才是权威，长度怪异的码交给上游
		// 判定（200003 邀请码不可用），而不是在这里因为"不是 6 位"就白拒。
		log.Printf("loomy: 绑定邀请码被拒（长度异常）uid=%s code=%s len=%d", uid, code, len(code))
		writeLoomyJSON(w, http.StatusBadRequest,
			map[string]any{"ok": false, "error": "邀请码长度异常（应为 6 位，实测形如 E3HRN8）"})
		return
	}
	var target *accountRef
	for _, a := range p.accounts() {
		if a.UID == uid {
			cp := a
			target = &cp
			break
		}
	}
	if target == nil {
		log.Printf("loomy: 绑定邀请码被拒（账号不存在）uid=%s code=%s", uid, code)
		writeLoomyJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "没有这个 loomy 账号: " + uid})
		return
	}
	ctx, cancel := pointsCtx()
	defer cancel()

	if err := p.BindInvite(ctx, target.Session, code); err != nil {
		// 上游错误码原文很晦涩（实测：自绑=100001"请求参数错误"、已用/失效码=200003
		// "邀请码不可用"），用户根本看不出"码为什么不行"。这里翻译成人话，
		// 并把**原文**落日志 —— 排查时能看真实错误码，界面上不甩晦涩报错。
		translated := translateBindError(err)
		log.Printf("loomy: 绑定邀请码失败 uid=%s code=%s 原文=%v", shortUID(uid), code, err)
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": translated})
		return
	}
	log.Printf("loomy: 绑定邀请码上游成功 uid=%s code=%s（开始复查激活状态）", shortUID(uid), code)
	act, applied, err := p.Activation(ctx, target.Session)
	bal, _, _ := p.Balance(ctx, target.Session)
	resp := map[string]any{
		"ok":        err == nil,
		"uid":       uid,
		"activated": act,
		"applied":   applied,
		"balance":   bal,
	}
	if err != nil {
		resp["error"] = "绑定已提交，但复查激活状态失败：" + err.Error()
		log.Printf("loomy: 绑定已提交但复查失败 uid=%s code=%s 复查错误=%v", shortUID(uid), code, err)
	}
	log.Printf("loomy: 绑定邀请码结果 uid=%s code=%s ok=%v activated=%v applied=%q balance=%d",
		shortUID(uid), code, resp["ok"], act, applied, bal)
	writeLoomyJSON(w, http.StatusOK, resp)
}

// inviteCodeLen 实测邀请码长度（见 handleInviteBind）。
const inviteCodeLen = 6

// translateBindError 把绑定邀请码的上游错误翻译成人话。
//
// 实测错误码（2026-09-15）：
//
//	100001  请求参数错误  —— 自绑（拿自己账号生成的码绑自己）时返回
//	200002  邀请码不存在  —— 抄错/多复制字符/码格式不对
//	200003  邀请码不可用  —— 已用（maxUses=1 被消费过）/ 失效
//
// 三个原文都让人看不出"码为什么不行"，用户会以为是 bug。这里按码翻译，
// 未命中时保留原文并补一句通用提示（也不让界面光秃秃一行晦涩报错）。
func translateBindError(err error) string {
	if err == nil {
		return "未知错误"
	}
	msg := err.Error()
	hint := "（请确认用的是**其他账号**「我生成的码」里 active 的码、且该码还没被用过）"
	switch {
	case strings.Contains(msg, "100001"):
		return "不能绑定自己账号生成的邀请码 —— " + hint
	case strings.Contains(msg, "200002"):
		return "邀请码不存在：可能抄错了字符或多复制了内容 —— " + hint
	case strings.Contains(msg, "200003"):
		return "邀请码不可用：已被使用或已失效 —— " + hint
	default:
		return msg + " —— " + hint
	}
}
