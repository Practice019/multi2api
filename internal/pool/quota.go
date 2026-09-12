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

// Effective 把额度折算成**一个标量**，仅供选号权重使用。
//
// 折算规则（都有测试）：
//
//	未探测（HasData=false）→ 0        （不假装有额度，也不假装没有）
//	unlimited             → 很大值    （排最前）
//	credits               → Remaining
//	per_model             → **最大值**，不是求和
//
// 为什么 per_model 取最大值而不是求和：一次调用只用一个模型，
// 求和会把"10 个模型各 100"算成 1000，夸大实际可用量，
// 让这类账号在选号时被不合理地偏袒。
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

// IsUnknown 尚未探测过额度。
func (q QuotaView) IsUnknown() bool { return !q.HasData }

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
// 一旦任何一次 SetQuota/SetCredits 被调用，状态就自然升级成新格式。
func restoreQuota(saved QuotaView, legacyCredits int64) QuotaView {
	if saved.HasData || saved.Kind != "" {
		return saved // 新格式，直接采用
	}
	// 旧格式：用 Credits 造一个单值视图
	return QuotaView{Kind: QuotaKindCredits, Remaining: legacyCredits, HasData: true}
}
