// explain.go 渠道选路的**只读诊断**（Explain）。
//
// # 它回答的问题（用户 2026-11-13）
//
//	"一个模型名对应了多个渠道，但是我发现他调用的时候**不会轮询**这些渠道"
//	→ 当天稍后用户直接定了修法：「不要设置上游或者渠道优先级 **全部去掉，全部平等**」。
//
// ⇒ 选路语义已按那条指令改掉（见 route.go 的 Plan：不再按 priority 排序；
//
//	server/channels_forward.go：没有"档"了，全部候选进同一个池）。
//	本文件是**折进来的可判性**：改完之后用户要能**看见**轮询真的均匀了。
//
// ⚠ 只读：不改优先级/权重、不发上游请求、不写库（见 TestExplainIsReadOnly）。
// ⚠ 判据字段驱动（priority / weight / key 状态），**不按上游名分叉**。
package channelroute

import (
	"fmt"
	"strings"

	"workbuddy2api/internal/channelstore"
)

// ExplainView 一份诊断结果（字段名即前后端契约）。
type ExplainView struct {
	PublicModel string `json:"public_model"`
	Group       string `json:"group"`
	// Total 候选总数（同一个池里的条数）。
	Total int `json:"total"`
	// RegularCount 参与常规轮转的条数 —— 2026-11-13 起**恒等于 Total**
	//（单池：不再有"只有第 0 档参与"这回事）。
	RegularCount int `json:"regular_count"`
	// PrioritiesInConfig 配置里仍写着非 0 priority 的候选数（**已不参与选路**，仅提示）。
	PrioritiesInConfig int                `json:"priorities_in_config,omitempty"`
	Candidates         []CandidateExplain `json:"candidates"`
	// Conclusion 一句话结论（界面直接显示）。
	Conclusion string   `json:"conclusion"`
	Notes      []string `json:"notes,omitempty"`
	// Fairness 一轮"各被选中多少次"的可判性结论（由 ApplyCounts 填）。
	Fairness string `json:"fairness,omitempty"`
}

// CandidateExplain 一条候选渠道的可见事实。
type CandidateExplain struct {
	ChannelID     string `json:"channel_id"`
	ChannelName   string `json:"channel_name"`
	UpstreamModel string `json:"upstream_model"`
	// Priority 配置里写着的值 —— ⚠ **已不参与选路**（保留在契约里只为可回退与排障）。
	Priority int `json:"priority"`
	Weight   int `json:"weight"`
	// WeightUsed 权重参与抽签时**实际生效**的值（<=0 视为 1，与 weightedPick 同一口径）。
	WeightUsed   int    `json:"weight_used"`
	KeysTotal    int    `json:"keys_total"`
	KeysUsable   int    `json:"keys_usable"`
	KeySelection string `json:"key_selection_mode,omitempty"`
	// Regular 是否在常规轮转的池里 —— 单池之后**恒为 true**（留着是为了让"平等"可断言）。
	Regular bool `json:"regular"`
	// SkipReason 非空表示"它现在**发不出去**"及其原因（例如 key 全停用）。
	SkipReason string `json:"skip_reason,omitempty"`
	// Picked 统计窗口内被选中多少次（由 ApplyCounts 填；纯展示）。
	Picked int `json:"picked"`
}

// Explain 诊断 publicModel 的路由：有哪些候选、谁在轮、为什么某条轮不到。
//
// 只读：内部只调 `Plan`（纯查表）与读 key 状态；**不发请求、不改任何状态**。
func Explain(s *channelstore.Store, group, publicModel string) (ExplainView, error) {
	cands, err := Plan(s, group, publicModel)
	if err != nil {
		return ExplainView{}, err
	}
	if group == "" {
		group = channelstore.DefaultGroup
	}
	v := ExplainView{PublicModel: publicModel, Group: group, Total: len(cands)}
	for _, c := range cands {
		ce := candidateExplain(c)
		ce.Regular = true // ⚠ 单池：**每条**都参与常规轮转
		v.RegularCount++
		if c.Priority != 0 {
			v.PrioritiesInConfig++
		}
		v.Candidates = append(v.Candidates, ce)
	}
	v.Conclusion, v.Notes = explainConclusion(v)
	return v, nil
}

// ApplyCounts 把"各条被选中多少次"填进诊断，并给出一句**可判性**结论。
//
// ⚠ 纯函数（只读入参、返回新值）：调用方（服务端）从自己的计数里取快照，
//
//	绝不能因为"要看统计"而改动选路状态。
func ApplyCounts(v ExplainView, counts map[string]int) ExplainView {
	total := 0
	for i := range v.Candidates {
		n := counts[v.Candidates[i].ChannelID]
		v.Candidates[i].Picked = n
		total += n
	}
	// 只统计"发得出去"的候选（key 全停用的那类本来就不该被选中）。
	eligible, uncovered, minP, maxP := 0, 0, 1<<30, 0
	for _, c := range v.Candidates {
		if c.SkipReason != "" {
			continue
		}
		eligible++
		if c.Picked == 0 {
			uncovered++
		}
		if c.Picked < minP {
			minP = c.Picked
		}
		if c.Picked > maxP {
			maxP = c.Picked
		}
	}
	switch {
	case total == 0:
		v.Fairness = "还没有采样数据（这一轮没被请求过）"
	case eligible == 0:
		v.Fairness = "没有可用的候选（都缺 key）"
	case uncovered > 0:
		v.Fairness = fmt.Sprintf("**不均匀**：%d/%d 条可用候选一次都没被选中（共采样 %d 次）",
			uncovered, eligible, total)
	case maxP > minP*3:
		v.Fairness = fmt.Sprintf("基本覆盖但偏斜：最多 %d 次 / 最少 %d 次（权重不等时这是预期的）", maxP, minP)
	default:
		v.Fairness = fmt.Sprintf("均匀：%d 条候选都有份额（最多 %d 次 / 最少 %d 次，共 %d 次）",
			eligible, maxP, minP, total)
	}
	return v
}

// candidateExplain 取一条候选的可见事实（key 计数**只读** Keys[]，不碰 round_robin 游标）。
func candidateExplain(c Candidate) CandidateExplain {
	usable, total := 0, 0
	if c.Channel != nil {
		total = len(c.Channel.Keys)
		for _, k := range c.Channel.Keys {
			if k.Status == channelstore.StatusEnabled {
				usable++
			}
		}
	}
	w := c.Weight
	if w <= 0 {
		w = 1 // 与 weightedPick 同一口径
	}
	ce := CandidateExplain{
		ChannelID: c.Channel.ID, ChannelName: c.Channel.Name,
		UpstreamModel: c.UpstreamModel, Priority: c.Priority, Weight: c.Weight,
		WeightUsed: w, KeysTotal: total, KeysUsable: usable,
		KeySelection: c.Channel.ChannelInfo.KeySelectionMode,
	}
	switch {
	case total == 0:
		ce.SkipReason = "这条渠道**没有配置 key** ⇒ 它永远不会被选中"
	case usable == 0:
		ce.SkipReason = fmt.Sprintf("%d 把 key **全部停用** ⇒ 它永远不会被选中（这就是「看起来没参与」的常见成因）", total)
	}
	return ce
}

// explainConclusion 生成"一句话结论"与补充说明。
func explainConclusion(v ExplainView) (string, []string) {
	var notes []string
	noKey := 0
	for _, c := range v.Candidates {
		if c.SkipReason != "" {
			noKey++
		}
	}
	if noKey > 0 {
		notes = append(notes, fmt.Sprintf("其中 %d 条没有可用 key（明细里已标注）", noKey))
	}
	if v.PrioritiesInConfig > 0 {
		notes = append(notes, fmt.Sprintf("配置里仍有 %d 条写着 priority —— **已不参与选路**（2026-11-13 起全部平等）",
			v.PrioritiesInConfig))
	}
	if len(v.Candidates) == 1 {
		notes = append(notes, "只有 1 条候选 ⇒ 没有「轮询」可言（它坏了就是失败）")
	}
	return fmt.Sprintf("全部 %d 条候选同属**一个池** ⇒ 都参与轮询（按 weight 加权随机；默认全 1 即完全平等）",
		v.Total), notes
}

// ExplainText 把诊断渲染成一小段人读文本（CLI/日志用；界面走结构体）。
func ExplainText(v ExplainView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "模型 %s（分组 %s）：候选 %d 条（同一个池）\n", v.PublicModel, v.Group, v.Total)
	for _, c := range v.Candidates {
		fmt.Fprintf(&b, "  · %s（weight=%d，key 可用 %d/%d", c.ChannelID, c.Weight, c.KeysUsable, c.KeysTotal)
		if c.Picked > 0 {
			fmt.Fprintf(&b, "，已选中 %d 次", c.Picked)
		}
		b.WriteString("）")
		if c.SkipReason != "" {
			fmt.Fprintf(&b, " ⚠ %s", c.SkipReason)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "  结论：%s\n", v.Conclusion)
	if v.Fairness != "" {
		fmt.Fprintf(&b, "  均匀性：%s\n", v.Fairness)
	}
	return b.String()
}
