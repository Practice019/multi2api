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
// # ⚠ T2 订正：上面关于 codearts 的那一行**是错的**（实测推翻）
//
// 原文说 codearts 是"按模型配额"，暗示 per_model 才是它的正确形态。
// 用真实凭证实测（2026-09-13）后确认：
//
//	codearts 有数字的额度只有**订阅级积分**：
//	    FetchSubscription.CreditRemain = 7474.04（total 7500, used 25.96）
//	    —— 账号整体还剩多少，**就是一个标量**，int64 完全装得下。
//
//	而"按模型探测"那条路（QuotaState）**根本没有数值字段**：
//	    type QuotaState struct {
//	        Exhausted bool        // ← 只有"耗尽没耗尽"
//	        Reason    string
//	        CheckedAt time.Time
//	    }
//	    它回答的是"这个模型现在能不能用"（benefit 免费额度会独立于积分耗尽），
//	    不是"还剩多少"。**把布尔当额度数值用是不可能的。**
//
// **结论：quota 需要用 QuotaView 而不是裸 int64，理由是对的；
// 但引用 codearts 作为依据是错的** —— 真正的理由是"额度形态不止一种"
//（unlimited / 未来可能出现真正按模型的上游），而不是 codearts 本身。
//
// 保留 QuotaView 不变，但 **FromPerModel 目前没有上游提供数据源**：
// 它是为"将来某个真的按模型报额度的上游"预留的形态。
// 接新上游时请**先实测上游到底给不给数字**，不要照着上面那行过时注释直接接。
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

// FromPerModel 从按模型额度构造。
//
// ⚠ **目前没有上游会调用它**（T2 实测订正，见文件头）：
// codearts 的"按模型探测"只返回布尔（够不够），不返回数值，
// 所以它的额度走的是 FromCredits（订阅级积分）。
//
// 保留它是为**将来某个真的按模型报额度**的上游预留的形态 ——
// 那时 ByModel 里才会有真数据，EffectiveFor(model) 也才真正有用。
// 接新上游前请先实测上游给不给数字，不要照着过时注释直接接。
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
//
// # ⚠ T2 修的第二个缺陷：向上升级时**凭空造出一个 0**
//
// 上面那条回落有个**没有被识别的副作用**：对于旧状态文件里根本没有额度信息的
// 账号（`saved` 是零值、`legacyCredits` 也是 0），回落会造出
//
//	{Kind: credits, Remaining: 0, HasData: true}
//
// 而按本文件开头对 HasData 的定义，这**恰好**是那句话：
//
//	HasData=true + Remaining=0 → 查过了，确实没额度
//
// 真相却是**从没查过**。注意这不是"设计遗漏"，而是
// **实现违反了自己声明的契约** —— HasData 的注释早就写明
// "必须与 Remaining==0 区分开"，而这里恰恰没有区分。
//
// 实测后果（本仓 data/state.json 里的真实数据）：
//
//	"01a08fe0...": {"quota":{"kind":"credits","has_data":true},"credits":0}
//	                ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^ remaining 被
//	                omitempty 吃掉 = 0，用户看到的就是"codearts 额度是 0"
//
// 用户会把 0 读成"这个号没额度了"而去删账号 —— 这正是本次要修的坑。
//
// 修法：**只有真的曾经有过额度信息**（`legacyCredits != 0`）才回落到
// Credits 单值；否则如实返回**未知**（HasData=false），界面显示 `—`。
//
// # 为什么这条对 workbuddy 零影响（有测试钉死）
//
// workbuddy 的账号走 `SetCredits` → `FromCredits` 写入，
// 那份 quota 的 `hasContent()` 恒为 true（Kind==credits 就是一例），
// **根本进不到兜底分支**。只有"从来没有过真数据"的账号才会走到这里。
//
// 见 TestRestoreQuota_WorkbuddyRealDataNotDowngraded 与
// TestRestoreQuota_UnknownStaysUnknown。
func restoreQuota(saved QuotaView, legacyCredits int64) QuotaView {
	if saved.hasContent() {
		return saved // 有内容，直接采用
	}
	// 旧格式，或"自称 per_model 但表是空的" —— 用 Credits 兜底。
	//
	// ⚠ 但**只在 legacyCredits 真的带信息时**才兜底。
	// legacyCredits==0 意味着"这个账号从来没有被写过额度"，
	// 此时造一个 has_data:true, remaining:0 就是**把未知伪装成确定的零**。
	if legacyCredits != 0 {
		return QuotaView{Kind: QuotaKindCredits, Remaining: legacyCredits, HasData: true}
	}
	// 从没查过 → 如实报告未知（界面显示 `—`，不是 0）。
	return QuotaView{HasData: false}
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
