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
//（"从没查过"被伪装成"查过了，确实是 0"）。
//
// 这条路径把那个洞补上：**核心遍历所有上游，问它们各自的额度，写回池。**
package admin

import (
	"log"
	"net/http"
	"sort"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

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
	// Providers 本次真正被问过额度的上游（排序后，供回执/日志）。
	Providers []string
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
	for _, p := range h.cfg.Registry.All() {
		if ext, ok := gateway.ExtOf[gateway.QuotaExt](p); ok {
			exts[p.ID()] = ext
		}
	}

	seen := map[string]bool{}
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
			continue // 该上游不报额度 → 保持未知，不写池
		}
		if !seen[provider] {
			seen[provider] = true
			out.Providers = append(out.Providers, provider)
		}

		qv, ok := h.safeRefreshQuota(ext, uid)
		if !ok {
			out.Failed++
			continue
		}
		if !qv.HasData {
			// 上游如实回答"我不知道" —— **不写池**（写 0 就是造假）。
			out.Unknown++
			continue
		}
		h.cfg.Pool.SetQuota(uid, toPoolQuota(qv))
		out.Updated++
	}

	sort.Strings(out.Providers)
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
//	/admin/accounts/quota/refresh **全上游通用**，只做"取额度 + 写回池"
//
// 两者并存是刻意的：后者不碰解冻（那需要上游回答"能不能用"，是另一件事），
// 所以它对已有路径**零行为改变**，纯粹是新增的只读+写额度入口。
func (h *Handler) accountsQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil {
		writeError(w, http.StatusNotImplemented, "账号池未接线")
		return
	}
	uids := make([]string, 0)
	for _, st := range h.cfg.Pool.List() {
		uids = append(uids, st.UID)
	}
	res := h.refreshQuotas(uids)
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":   res.Updated,
		"unknown":   res.Unknown,
		"failed":    res.Failed,
		"providers": res.Providers,
		// 回执里带上刷新后的账号视图，前端一次调用就能重绘表格。
		"accounts": h.accountViews(),
	})
}
