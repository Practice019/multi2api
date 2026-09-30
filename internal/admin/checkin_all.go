// checkin_all.go 核心的**跨上游**全量签到：POST /admin/accounts/checkin/all。
//
// # 为什么需要这条核心端点（用户本轮要求）
//
//	"这些（全部签到（lobsterai）/（qoder）/（qodercn）/（trae））放到上游的卡片顶部
//	 不要放到和账号池水平的位置；账号池水平的位置只放一个全部签到，
//	 是签到所有的上游，也就是触发所有上游的全部签到"
//
// 于是「全部签到」分成两级：
//
//	分组头（上游卡片顶部）  打该上游自己的 AllURL      → 只管自己那几个号
//	账号池顶部（唯一的那个）打本端点                    → 触发**所有**上游
//
// 第二级必须是核心端点：它要遍历注册表，而"认识有哪些上游"这件事
// 只有核心能做（上游之间互不可见，架构铁律）。
//
// # 为什么不复用 DailyAction.AllURL 由前端逐个打
//
// 那会让一次用户点击变成 N 个并发请求，N 个 toast、N 个进度条互相覆盖
// （webui.html 里有一段注释专门记这个坑：「后结束的那个会把先结束/
// 仍在进行的内容擦掉」）。而且前端要认识"哪些上游有签到" —— 那是把
// 上游清单搬进渲染层。核心一次收口，进度走既有的 /admin/task 通道。
package admin

import (
	"context"
	"log"
	"net/http"
	"time"

	"workbuddy2api/internal/gateway"
)

// checkinAllTaskKind /admin/task 里显示的任务名。
//
// 与 workbuddy 全量签到用的 "checkin" 区分开：那是**一个**上游的签到，
// 本任务是**所有**上游。名字混同会让用户在进度条上分不清自己在看哪个。
const checkinAllTaskKind = "checkin-all"

// checkinAllTimeout 整趟的上限。
//
// 它是**兜底**而不是预期耗时：每个上游内部已有自己的超时（trae 2 分钟、
// 共享驱动按账号逐个走）。设上限的理由是任务槽同一时刻只放一个任务 ——
// 一个卡死的上游会让后面的入口全部 409，而没有任何东西去解它。
const checkinAllTimeout = 15 * time.Minute

// taskSlotStarter 任务槽的**可选**扩展：能启动后台任务。
//
// # 为什么是可选接口而不是改宽 TaskSlot
//
// TaskSlot 是 /admin/task 的只读契约（Snapshot）。把 Start 塞进去会逼
// 每一个只做只读的测试替身去实现一个它根本不用的方法。
// 可选接口 + 类型断言是本仓库既有模式（见 JobStatusView 的注释）。
//
// ⚠ 生产装配必须让它成立：cmd/server 注入的 adminTaskSlotAdapter
// 包着进程内唯一那个 workbuddy 任务槽（它带 Start）。
type taskSlotStarter interface {
	Start(kind string, fn func() []map[string]any) bool
}

// accountsCheckinAll POST /admin/accounts/checkin/all —— 触发**所有**上游的全量签到。
//
// # 回执
//
//	202 {started:true, providers:[…]}  已启动，后台执行（进度看 /admin/task）
//	409                                已有全量任务在跑（与改造前逐字一致：不排队）
//	501                                任务槽未接线（装配层没注入 —— 要刺眼，不要静默同步跑）
//
// providers 是**本次真正会跑**的上游清单（实现了 DailyCheckinExt 的那些）。
// 它必须在启动时就回给前端：任务完成后用户看到的只有"成功/已签到/失败"
// 三个计数，无从判断"到底动了哪几个上游"—— 而那正是本按钮的全部语义
// （"触发所有上游"里"所有"指的是哪些，得说得出来）。
func (h *Handler) accountsCheckinAll(w http.ResponseWriter, r *http.Request) {
	providers := h.checkinProviders()
	if len(providers) == 0 {
		// 一个都没有时不假装成功：那要么是没启用任何上游（装配问题），
		// 要么是"签到"这个扩展点谁都没实现（接线问题）。两种都要看得见。
		writeError(w, http.StatusNotImplemented, "没有任何上游实现了全量签到（gateway.DailyCheckinExt）")
		return
	}
	slot, ok := h.cfg.TaskSlot.(taskSlotStarter)
	if !ok || slot == nil {
		writeError(w, http.StatusNotImplemented, "后台任务槽未接线，无法执行跨上游签到")
		return
	}
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.ID())
	}
	if !slot.Start(checkinAllTaskKind, func() []map[string]any {
		return h.runCheckinAll()
	}) {
		writeError(w, http.StatusConflict, "已有任务在执行中，请等它结束")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started":   true,
		"providers": names,
		"count":     len(names),
	})
}

// checkinProviders 注册表里**真的会签到**的上游（按注册顺序）。
//
// # 判据为什么是"有没有自报签到动作"，而不只是"有没有实现扩展点"
//
// 实测（隔离实例 7871）：`workbuddy` 与 `workbuddy-intl` 是**同一个
// Provider 类型**的两个实例，两者都实现了 DailyCheckinExt。但海外版
// （DisableGrowthTravel）**没有签到玩法** —— 它的路由里没有
// /admin/checkin，DailyActions 也不报签到动作，CheckinAll 如实回空。
//
// 只看"有没有实现扩展点"会把它列进 providers 清单，于是用户点完
// 「全部签到」看到 `providers: [workbuddy, workbuddy-intl]`，
// 以为两个上游都签了 —— 而其中一个**一个号都没动**。
// 这正是本按钮最危险的谎报形态（回执说做了、实际没做）。
//
// 所以判据加上"它自报了至少一个 batch 动作"：那是"这个实例真的有
// 可签到的动作"的**上游自报事实**，与前端渲染卡片头按钮用的是同一条
// （dailyAllActionsHTML 的判据）。两处同源，不会出现"卡片上没按钮、
// 回执里却说你签了"。
//
// # 为什么"没实现"不算错误
//
// 一个纯 API Key 上游（没有账号生命周期）本来就没有签到 ——
// 不实现这个扩展点是**它的事实**，不是缺陷。把它们报成 skipped 会让
// 用户以为"有几个上游漏了"，那是谎报；这里干脆不列出它们。
func (h *Handler) checkinProviders() []gateway.Provider {
	if h.cfg.Registry == nil {
		return nil
	}
	out := make([]gateway.Provider, 0)
	for _, p := range h.cfg.Registry.All() {
		ext, ok := gateway.ExtOf[gateway.DailyCheckinExt](p)
		if !ok || ext == nil {
			continue
		}
		// 它自己说"我有没有可签到的动作"。没有就不列 —— 列了就是谎报。
		if !hasBulkDailyAction(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// hasBulkDailyAction 该上游是否自报了至少一个**有全量端点**的每日动作。
//
// 判据与前端渲染卡片头「全部签到」按钮的那条**逐字同源**
// （webui.html 的 dailyAllActionsHTML：`batch && all_url`）：
// 界面上有按钮 ⟺ 回执里列它。两处不一致就会出现
// "卡片上没按钮、但顶部签到说签过它"这种无法解释的状态。
func hasBulkDailyAction(p gateway.Provider) bool {
	dae, ok := gateway.ExtOf[gateway.DailyActionExt](p)
	if !ok || dae == nil {
		return false
	}
	for _, a := range gateway.SanitizeDailyActions(dae.DailyActions()) {
		if a.Batch && a.AllURL != "" {
			return true
		}
	}
	return false
}

// runCheckinAll 逐个上游跑它的全量签到，汇总成任务槽要的形状。
//
// # 为什么串行
//
// ① 账号池顶部的按钮是一次人工点击，并发打 N 个上游只是为了快几秒，
//
//	却会让进度条与日志交错；② 任务槽本来就只允许一个全量任务，
//	串行是它的自然语义。
//
// # 为什么每个上游单独 recover
//
// 上游的签到会打真实网络。一个上游 panic 不该让后面几个拿不到结果 ——
// 更不该带崩网关（workbuddy 的任务槽对整趟 fn 也有 recover，但那只在
// 最外层，拦下之后**后面几个上游就不跑了**）。
func (h *Handler) runCheckinAll() []map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), checkinAllTimeout)
	defer cancel()

	out := make([]map[string]any, 0)
	for _, p := range h.checkinProviders() {
		if ctx.Err() != nil {
			log.Printf("admin: 跨上游签到在跑到 %s 之前超时/取消，后续上游未执行", p.ID())
			break
		}
		rep := checkinOneProvider(ctx, p)
		// 整趟失败（列账号失败等）也要留一条可见结果 ——
		// 否则"这个上游一个号都没跑"在三个计数里完全隐形。
		if rep.Error != "" {
			log.Printf("admin: 跨上游签到 %s 整趟失败: %s", p.ID(), rep.Error)
			out = append(out, map[string]any{
				"uid":    "(" + p.ID() + ")",
				"status": "fail",
				"detail": rep.Error,
			})
		}
		for _, res := range rep.Results {
			out = append(out, map[string]any{
				"uid":    res.UID,
				"status": res.Status,
				"detail": res.Detail,
			})
		}
	}
	return out
}

// checkinOneProvider 跑一个上游的全量签到，并 panic 兜底。
//
// # Provider 为什么由**这里**填
//
// 让上游自己报名字等于让它复述自己的 ID —— 两处一旦不一致（改名、
// 复制粘贴），界面上的归属就错了。这里遍历时本来就持有 p.ID()。
func checkinOneProvider(ctx context.Context, p gateway.Provider) (rep gateway.DailyCheckinReport) {
	ext, ok := gateway.ExtOf[gateway.DailyCheckinExt](p)
	if !ok || ext == nil {
		return gateway.DailyCheckinReport{Provider: p.ID()}
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("admin: 跨上游签到 %s panic: %v", p.ID(), rec)
			rep = gateway.DailyCheckinReport{
				Provider: p.ID(),
				Error:    "执行 panic（已兜住，其它上游继续）",
			}
		}
	}()
	rep = ext.CheckinAll(ctx)
	rep.Provider = p.ID()
	return rep
}
