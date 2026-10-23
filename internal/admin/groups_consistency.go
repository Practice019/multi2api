// groups_consistency.go 分组引用的一致性检查与一键清理（PLAN-groups.md P5）。
//
// # 要解决的两种"陈旧引用"
//
//	① 悬空分组：key.Groups 里有一个**不存在**的分组 ID
//	   后果：那把 key 走"受限且并集为空"⇒ 调用全 403，而错误信息只说
//	         "分组里没有任何模型" —— 用户得自己猜到是删过分组
//
//	② 陈旧模型：分组里列着一个**目录里已不存在**的模型
//	   后果：它永远匹配不上任何请求（无害，但界面上那一条是死配置，
//	         而"分组里有 5 个模型"的计数比实际能用的多）
//
// # ⚠ 为什么不自动修
//
// 目录会**抖动**：上游拉取失败、凭证过期时目录会短暂为空（本仓已在
// apikey.PruneModels 的注释里记过这个坑）。自动按目录清理 ⇒ 一次抖动把用户
// 精心配的分组内容全删了。所以清理只走显式端点（POST /admin/groups/prune），
// 而**检查**可以随时跑（它只读、不改）。
//
// 同理：悬空分组理论上可以自动清（它不依赖目录，只依赖分组表这个确定事实），
// 但仍然保留显式触发 —— 与"删分组时级联清"配合起来已经覆盖了正常路径，
// 这个端点只用于收拾历史残留（比如 P5 之前删掉的分组）。
package admin

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"workbuddy2api/internal/groupstore"
)

// groupDanglingRef 一把 key 指向了不存在的分组。
type groupDanglingRef struct {
	KeyID   string `json:"key_id"`
	KeyName string `json:"key_name"`
	// Masked 掩码后的 key（完整 key 不出现在一致性报告里 —— 它会被前端缓存/截图/贴到工单）。
	Masked  string `json:"masked"`
	GroupID string `json:"group_id"`
	// Risky true = 这把 key 的**全部**分组都悬空 ⇒ 它当前一个模型都不能用。
	//
	// 单独给一个布尔而不是让前端自己数：前端拿到 [悬空1, 悬空2] 与
	// [悬空1, 正常分组] 的判断逻辑不同，而"全部悬空"才是需要紧急处理的那类。
	Risky bool `json:"risky"`
}

// groupStaleModel 分组里列着一个目录里已不存在的模型。
type groupStaleModel struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
	Model     string `json:"model"`
}

// groupConsistency 一致性报告。字段名直接给前端用，不做二次映射。
type groupConsistency struct {
	Enabled bool `json:"enabled"`

	// DanglingRefs 悬空的 (key, 分组) 引用对。
	DanglingRefs []groupDanglingRef `json:"dangling_refs"`
	// DanglingCount 悬空引用**去重后的分组 ID 数**（前端显示"有 N 个分组已不存在"）。
	DanglingGroupIDs []string `json:"dangling_group_ids"`

	// StaleModels 分组里的陈旧模型。
	StaleModels []groupStaleModel `json:"stale_models"`
	// StaleCheckable false = 这次没拿到模型目录（未注入 / 取失败）⇒ StaleModels 不可信。
	//
	// ⚠ 必须显式给这个位，而不是"目录为空就当没有陈旧模型"。
	// 两者在前端看起来都是 stale_models=[]，但含义相反：
	//   checkable=true  真的都还在
	//   checkable=false 没查到，别信这个空
	StaleCheckable bool `json:"stale_checkable"`
	// CatalogSize 本次比对用的目录规模（0 且 checkable=false 就是"没拿到"）。
	CatalogSize int `json:"catalog_size"`
	// CatalogIncomplete 本次**没能检查**的来源（上游 id 等）。
	//
	// ⚠ 非空时 stale_models 依然可信（已查的部分是准的），但结论要打折：
	// 某个模型可能是从没查成的那个来源来的。界面上应显示"部分来源未就绪"，
	// 而不是让用户以为"列表是完整的"。
	CatalogIncomplete []string `json:"catalog_incomplete,omitempty"`

	// OK 两项都干净（且目录确实查过）。
	OK bool `json:"ok"`
}

// catalogFunc 取当前对外模型目录（内部名）。
//
// nil = 未接线 ⇒ 陈旧模型这一项不可检查（StaleCheckable=false）。
// 用函数值而不是直接调某个包：admin 不该认识"目录从哪来"（它可能是上游 +
// 渠道并集，那是 server 侧的事）。
type catalogFunc func() ModelCatalogSnapshot

// ModelCatalogSnapshot「当前对外模型目录」的一份快照（由装配层投影）。
//
// ⚠ Unavailable 非空只说明快照不完整，**不能**据此判定某个模型已下线 ——
// 它可能恰好来自没查成的那个来源。界面上要区分这两件事。
type ModelCatalogSnapshot struct {
	Names       []string
	Unavailable []string
}

// buildGroupConsistency 算一致性报告（只读：不改任何存储）。
//
// groups 为 nil ⇒ 分组功能未启用，返回 enabled=false 的空报告。
// catalog 为 nil ⇒ 陈旧模型这一项不可检查（StaleCheckable=false）。
func (h *Handler) buildGroupConsistency(groups *groupstore.Store, catalog catalogFunc) groupConsistency {
	rep := groupConsistency{
		Enabled: groups != nil,
		// 空切片而非 nil：前端不必为 null 单独写分支（本仓一贯口径）。
		DanglingRefs:     []groupDanglingRef{},
		DanglingGroupIDs: []string{},
		StaleModels:      []groupStaleModel{},
	}
	if groups == nil {
		return rep
	}

	known := map[string]bool{}
	for _, g := range groups.List() {
		known[g.ID] = true
	}

	// ---- ① 悬空引用 ----
	//
	// 直接用 apikey.View：它已经带 Groups 与 Masked（P2 加字段时同步改过 List）。
	// 不另开一套"只取分组"的接口 —— 视图的字段必须与 List 同源，
	// 否则又会出现"存了但读不出来"这一类问题（本仓在 ModelMap 上踩过）。
	if ks := h.apiKeysStore(); ks != nil {
		danglingIDs := map[string]bool{}
		for _, v := range ks.List() {
			if len(v.Groups) == 0 {
				continue // 不限制的 key：没有引用可悬空
			}
			var missing []string
			for _, gid := range v.Groups {
				if !known[gid] {
					missing = append(missing, gid)
				}
			}
			if len(missing) == 0 {
				continue
			}
			// ⚠ 全部悬空才是"这把 key 现在一个模型都用不了"（P3 的拦截会拒掉一切）。
			//   部分悬空只是少了一些模型，严重性不同 —— 前端要能分开显示。
			risky := len(missing) == len(v.Groups)
			for _, gid := range missing {
				danglingIDs[gid] = true
				rep.DanglingRefs = append(rep.DanglingRefs, groupDanglingRef{
					KeyID: v.ID, KeyName: v.Name, Masked: v.Masked,
					GroupID: gid, Risky: risky,
				})
			}
		}
		for gid := range danglingIDs {
			rep.DanglingGroupIDs = append(rep.DanglingGroupIDs, gid)
		}
		sort.Strings(rep.DanglingGroupIDs)
		sort.Slice(rep.DanglingRefs, func(i, j int) bool {
			if rep.DanglingRefs[i].KeyID != rep.DanglingRefs[j].KeyID {
				return rep.DanglingRefs[i].KeyID < rep.DanglingRefs[j].KeyID
			}
			return rep.DanglingRefs[i].GroupID < rep.DanglingRefs[j].GroupID
		})
	}

	// ---- ② 陈旧模型 ----
	if catalog != nil {
		snap := catalog()
		rep.CatalogSize = len(snap.Names)
		rep.CatalogIncomplete = snap.Unavailable

		// ⚠ 目录为空 ⇒ **不能**判定任何模型陈旧。
		//
		// 那是把「没查成」当成「不存在」，结论是「你分组里全是废名字，清空吧」——
		// 灾难性的建议。宁可不报，让它显示成「未就绪」。
		if len(snap.Names) == 0 {
			rep.StaleCheckable = false
		} else {
			rep.StaleCheckable = true
			exist := make(map[string]bool, len(snap.Names))
			for _, n := range snap.Names {
				if n != "" {
					exist[n] = true
				}
			}
			for _, g := range groups.List() {
				for _, m := range g.Models {
					if exist[m] {
						continue
					}
					rep.StaleModels = append(rep.StaleModels, groupStaleModel{
						GroupID: g.ID, GroupName: g.Name, Model: m,
					})
				}
			}
			sort.Slice(rep.StaleModels, func(i, j int) bool {
				if rep.StaleModels[i].GroupID != rep.StaleModels[j].GroupID {
					return rep.StaleModels[i].GroupID < rep.StaleModels[j].GroupID
				}
				return rep.StaleModels[i].Model < rep.StaleModels[j].Model
			})
		}
	}

	// ---- ③ 总结论 ----
	//
	// ⚠ "OK" 要保守：目录没查过就**不能说**"分组内容都是干净的"。
	rep.OK = len(rep.DanglingRefs) == 0 && (!rep.StaleCheckable || len(rep.StaleModels) == 0)
	return rep
}

// groupsConsistency GET /admin/groups/consistency
//
// 只读。可以随时跑 —— 它不改任何东西，所以不会因为目录抖动误伤配置。
func (h *Handler) groupsConsistency(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.buildGroupConsistency(h.groupsStore(), h.groupCatalogFunc()))
}

// groupsPrune POST /admin/groups/prune —— 清掉**悬空的分组引用**。
//
// # 只清悬空，不清陈旧模型（这是刻意的）
//
// 悬空依赖的是"分组表里有没有这个 ID"—— 那是**确定事实**，不可能因为
// 上游抖动而误判。陈旧模型依赖的是模型目录，而目录会短暂为空（凭证过期、
// 上游拉取失败）⇒ 自动按目录清会把用户精心挑的分组内容一次删光。
//
// 所以本端点只做前者；后者要人看过报告再手工改（界面上逐条删）。
func (h *Handler) groupsPrune(w http.ResponseWriter, r *http.Request) {
	gs := h.groupsStore()
	if gs == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "分组功能未启用"})
		return
	}
	ks := h.apiKeysStore()
	if ks == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "API key 存储未启用，没有引用可清理"})
		return
	}

	// 先把悬空的分组 ID 算出来（幂等：没有悬空时下面会什么都不做）。
	rep := h.buildGroupConsistency(gs, nil)
	if len(rep.DanglingGroupIDs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pruned": false, "reason": "没有悬空的分组引用"})
		return
	}

	res, err := ks.PruneGroups(rep.DanglingGroupIDs)
	if err != nil {
		// 清理失败如实回报。**不**回滚：分组引用已经改了一半时回滚会制造
		// "清了一半又回来"的形态，比"清了一部分、报告哪一部分"更难排查。
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := map[string]any{
		"ok":           true,
		"pruned":       true,
		"keys_touched": res.KeysTouched,
		"groups_gone":  res.GroupsGone,
	}
	// ⚠ 权限放大必须显式告诉调用方（见 apikey.PruneResult.KeysUnrestricted 的注释）。
	//   这不是"顺便带一个统计"，而是这次操作最需要用户知晓的后果。
	if len(res.KeysUnrestricted) > 0 {
		out["keys_unrestricted"] = res.KeysUnrestricted
		out["warning"] = "下列 key 的分组引用被清空，它们现在**不受模型限制**了（可用全部模型）：" +
			joinLimited(res.KeysUnrestricted, 5)
	}
	writeJSON(w, http.StatusOK, out)
}

// joinLimited 用中文顿号连接，超过 n 个就加「…（共 N 个）」。
//
// 与 server 包的同名函数是**各留一份**（两边包内私有）：返回给前端的文案
// 在本包内自洽即可，为一个五行的格式化函数在核心之间拉依赖不划算。
// （channelstore / admin 的 writeFileAtomic 也是同样的取舍。）
func joinLimited(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, "、")
	}
	return strings.Join(list[:n], "、") + "…（共 " + strconv.Itoa(len(list)) + " 个）"
}
