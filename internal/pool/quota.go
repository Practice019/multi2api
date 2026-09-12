package pool

// 额度视图：把"账号还剩多少可用量"从上游专属语义里抽象出来。
//
// # 为什么需要它
//
// 改造前 `Status.Credits int64` 与 `SetCredits(uid, int64)` 隐含假设
// **额度就是一个整数**。这在第二个上游上就不成立了：
//
//	workbuddy → 积分（单值，一个 int64 够）
//	codearts  → 按模型配额（ProbeQuota(model)，一个 int64 装不下）
//
// 若继续用 int64，codearts 的额度只能被强行求和或取第一个模型，
// 两种都是错的。
//
// # 设计原则：pool 不解释额度语义
//
// pool 只做两件事：
//  1. 存下上游给的额度视图（原样保存，不理解含义）
//  2. 把它折算成一个**标量**用于选号权重（Effective）
//
// "这个账号能不能用"由**上游**回答（ReenableIfUsable 的 usable 参数），
// pool 不写"余额 > 0 就能用"这种上游专属规则 ——
// codearts 的账号可能某个模型配额用尽但整体仍可用。
type QuotaView struct {
	// Kind 额度形态。取值见 QuotaKind* 常量。
	Kind string `json:"kind,omitempty"`

	// Remaining 单值额度（Kind == credits 时有效）。
	Remaining int64 `json:"remaining,omitempty"`

	// ByModel 按模型额度（Kind == per_model 时有效）。
	// key 是上游模型名（不含 provider 前缀）。
	ByModel map[string]int64 `json:"by_model,omitempty"`

	// HasData 是否已从上游探测过。
	//
	// **必须与 Remaining==0 区分开**：
	//   HasData=false → 还没查过（新账号），不该被当成"没额度"
	//   HasData=true + Remaining=0 → 查过了，确实没额度
	// 混为一谈会让新账号在选号时被永久降权。
	HasData bool `json:"has_data,omitempty"`
}

// 额度形态常量。
const (
	// QuotaKindCredits 单值额度（workbuddy 的积分）。
	QuotaKindCredits = "credits"
	// QuotaKindPerModel 按模型额度（codearts）。
	QuotaKindPerModel = "per_model"
	// QuotaKindUnlimited 无上限（包月/试用期账号）。
	QuotaKindUnlimited = "unlimited"
)

// unlimitedWeight 无上限账号折算出的标量权重。
//
// 取一个"足够大但不会溢出"的值：它只需要在选号时排到最前，
// 参与的是 float64 归一化（除以候选集最大值），过大的绝对值没有意义。
const unlimitedWeight int64 = 1 << 40

// Effective 把额度折算成**一个标量**。
//
// ⚠ 对 `per_model` 而言这个标量是**近似值**，只适合"该账号整体还有多少量"的
// 展示与粗粒度选号。真正的选号应当用 `EffectiveFor(请求的模型)`。
//
// 折算规则（都有测试）：
//
//	未探测（HasData=false）→ 0
//	unlimited             → 很大值（排最前）
//	credits               → Remaining
//	per_model             → 各模型额度的**最大值**
//
// 为什么 per_model 用最大值：它是"这个账号最多还能跑多少"的上界，
// 用于展示是合理的。**但它对选号是错的** —— 见 EffectiveFor 的说明。
func (q QuotaView) Effective() int64 {
	if !q.HasData {
		return 0
	}
	switch q.Kind {
	case QuotaKindUnlimited:
		return unlimitedWeight
	case QuotaKindPerModel:
		var max int64
		for _, v := range q.ByModel {
			if v > max {
				max = v
			}
		}
		return max
	default: // credits 及未知形态
		return q.Remaining
	}
}

// EffectiveFor 返回该账号**针对某个模型**的可用额度。
//
// # 为什么必须有这个（评审指出的一个真实缺陷）
//
// 用 Effective()（取所有模型的最大值）来选号是**反的**：
//
//	账号 X = {gpt-5.5: 1000, other: 0}      → Effective = 1000
//	账号 Y = {gpt-5.5: 0,    other: 999999} → Effective = 999999
//
// 请求 gpt-5.5 时，Y 对**该模型**零额度，却因为 other 的高额度而排在 X 前面。
// 结果是选到一个跑不了的账号。
//
// 原注释里写的"一次调用只用一个模型"恰恰**支持**按模型取值，
// 而不是取最大值 —— 那是自相矛盾的，已纠正。
//
// 语义：
//   - credits / unlimited → 与 Effective() 相同（它们不分模型）
//   - per_model           → 该模型的值；该模型不在表里视为 0（不是"未知"）
//   - 未探测              → 0
func (q QuotaView) EffectiveFor(model string) int64 {
	if !q.HasData {
		return 0
	}
	switch q.Kind {
	case QuotaKindPerModel:
		return q.ByModel[model]
	default:
		return q.Effective()
	}
}

// IsUnknown 尚未探测过额度。
func (q QuotaView) IsUnknown() bool { return !q.HasData }

// Clone 深拷贝额度视图。
//
// # 为什么必须深拷贝（评审指出的一个真实缺陷）
//
// `ByModel` 是 map（引用类型）。若 `Status()`/`List()` 直接把内部 map 递出去，
// 调用方改一下返回值就**改到了池子的内部状态**，而且绕过了锁。
// 实测：`sts[0].Quota.ByModel["m"] = 424242` 能改到池内。
//
// 有回归测试守着（TestReviewerF7_QuotaMapMustBeCopied）。
func (q QuotaView) Clone() QuotaView {
	out := q
	if len(q.ByModel) > 0 {
		m := make(map[string]int64, len(q.ByModel))
		for k, v := range q.ByModel {
			m[k] = v
		}
		out.ByModel = m
	}
	return out
}

// FromCredits 从单值构造（workbuddy 的调用方用这个，最省事）。
func FromCredits(remaining int64) QuotaView {
	return QuotaView{Kind: QuotaKindCredits, Remaining: remaining, HasData: true}
}

// FromPerModel 从按模型额度构造（codearts 的调用方用这个）。
func FromPerModel(byModel map[string]int64) QuotaView {
	return QuotaView{Kind: QuotaKindPerModel, ByModel: byModel, HasData: true}
}

// restoreQuota 从落盘状态恢复额度视图。
//
// 处理**升级兼容**：旧状态文件只有 credits 字段，没有 quota。
// 此时反序列化得到的 Quota 是零值（HasData=false），
// 若直接采用会让所有账号变成"未探测"状态 —— 选号权重全归零。
// 因此回落到 Credits。
//
// 为什么不写迁移脚本：这个回落本身就是迁移，且不需要停机。
//
// # 评审指出的一个次生缺陷（已修）
//
// 早先的实现是 `if saved.HasData || saved.Kind != "" { return saved }` ——
// 于是 `{"kind":"per_model","has_data":true}`（ByModel 为空）会被原样采用，
// 得到一个 **Effective=0 但 IsUnknown()=false** 的"可信的零"，
// 即使落盘同时有 credits=5000 可用。账号被静默降权。
//
// 现在：额度视图**内容为空**时一律回落到 Credits，
// 不管它自称什么 Kind。
func restoreQuota(saved QuotaView, legacyCredits int64) QuotaView {
	if saved.hasContent() {
		return saved // 有内容，直接采用
	}
	// 旧格式，或"自称 per_model 但表是空的" —— 都用 Credits 兜底
	return QuotaView{Kind: QuotaKindCredits, Remaining: legacyCredits, HasData: true}
}

// hasContent 报告这个额度视图是否真的带了数据。
//
// 用来把"自称有数据"与"真的有数据"区分开：
// `{Kind: per_model, HasData: true, ByModel: nil}` 是前者。
func (q QuotaView) hasContent() bool {
	if !q.HasData {
		return false
	}
	switch q.Kind {
	case QuotaKindPerModel:
		return len(q.ByModel) > 0
	case QuotaKindUnlimited:
		return true
	case QuotaKindCredits:
		return true // Remaining 可能是合法的 0
	default:
		// 未知 Kind：只有在真带了数的情况下才算有内容
		return q.Remaining != 0 || len(q.ByModel) > 0
	}
}
