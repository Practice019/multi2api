// migration_dryrun.go U-02 的管理端点：存量权限 dry-run 报告。
//
// # 这个端点回答一个问题
//
// 「现在把存量令牌切到 v2，每个令牌能用的模型会怎么变？有没有变宽的？」
//
// 它是迁移前的**硬门禁**：计划 U-02 的验收要求「无法解析／歧义项**阻塞迁移**」，
// 风险栏要求「**不扩大权限**」。比较逻辑本身在 channelstore.ComputeDryRun，
// 本文件只负责**取事实**（渠道、目录、每个 key 的旧/新策略）然后交给它。
//
// # ⚠ 这个响应里不许出现任何凭据
//
// `apikey.View.ID` 与 `apikey.ApiKey.ID` **都是完整密钥本身**（sk- 开头的 Bearer 值）。
// 本文件把它们当**查找键**用，一个字节都不会进响应：
// 报告项只有 Name / Owner（DryRunItem 结构里根本没有 ID 字段，有判据用反射钉着）。
//
// 报告是要贴进工单 / 发给同事看的，所以这条不是"小心一点"，是结构上不允许。
package admin

import (
	"net/http"

	"workbuddy2api/internal/channelstore"
)

// migrationDryRun GET /admin/migration/dry-run —— 存量权限差异报告（只读，不改任何东西）。
//
// 返回：
//
//	report    channelstore.DryRunReport（逐令牌的旧/新权限与差异）
//	blocked   是否有阻塞（= 不能迁移）
//	（报告内部已带 total / applicable / blocked 等计数）
func (h *Handler) migrationDryRun(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !h.principalFrom(r).IsAdmin() {
		writeError(w, http.StatusForbidden, "admin required")
		return
	}
	ks := h.apiKeysStore()
	if ks == nil {
		writeError(w, http.StatusServiceUnavailable, "API key 存储未启用，没有存量权限可比")
		return
	}
	channels := h.channelsForDryRun()

	// ── 目录：v2 下"当前能对外提供哪些公开名" ──
	//
	// ⚠ 用 BuildCatalog 而不是重新拼一遍：它已经处理了公开名映射与歧义
	// （被拒绝发布的公开名不在 Models 里）。用同一份逻辑，报告与运行时才一致。
	catalog := make([]string, 0)
	for _, m := range channelstore.BuildCatalog(channels).Models {
		catalog = append(catalog, m.ID)
	}

	// ── 逐 key 取事实 ──
	inputs := make([]channelstore.DryRunInput, 0)
	for _, v := range ks.List() {
		// ⚠ v.ID 是**明文稿**，只作查找键，绝不外发。
		version, groups, limited, limits, ok := ks.RoutingPolicyOf(v.ID)
		if !ok {
			continue
		}
		_, oldGroupModels, oldResolved := ks.GroupModelsOf(v.ID)

		inputs = append(inputs, channelstore.DryRunInput{
			Name:  v.Name,
			Owner: v.OwnerID,
			Policy: channelstore.RoutingPolicy{
				PolicyVersion:      version,
				Groups:             groups,
				ModelLimitsEnabled: limited,
				ModelLimits:        limits,
			},
			OldGroupsResolved: oldResolved,
			OldGroupsModels:   oldGroupModels,
			// 旧白名单 = 同一个字段（v1 与 v2 共用 ModelLimits），
			// 差别只在**判定顺序**（旧覆盖 / 新交集）—— 那是比较器的事。
			OldLimitEnabled: limited,
			OldLimits:       limits,
		})
	}

	report := channelstore.ComputeDryRun(h.cfg.Groups, channels, catalog, inputs)
	writeJSON(w, http.StatusOK, map[string]any{
		"report":  report,
		"blocked": !report.Ready,
	})
}

// channelsForDryRun 取渠道列表（没接线时给空切片，让比较器按"目录不可信"阻塞）。
func (h *Handler) channelsForDryRun() []*channelstore.Channel {
	if h.cfg.Channels == nil {
		return nil
	}
	return h.cfg.Channels.List()
}
