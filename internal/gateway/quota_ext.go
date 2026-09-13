// 扩展点：上游自报"我的额度怎么取"。
//
// # 为什么单开一个文件，而不是写进 extension.go
//
// extension.go 是**扩展点集合**（AdminExt / JobExt / LoginFlow / AuthDirExt /
// CredentialLoader 五个都挤在里面）。再加一个会让它更难读；
// 而且 T2/T3 两个任务都要加扩展点，分文件可以让并行改动**零冲突**。
// 后续新扩展点也按"一个扩展点一个文件"走。
//
// # 为什么需要这个扩展点（实测的根因）
//
// 改造前 `/admin/accounts` 的 `quota` / `credits` **只有 workbuddy 有值**，
// 因为只有它有一条把额度写回池的路径：
//
//	POST /admin/credits/refresh  →  workbuddy.RefreshCredits
//	                             →  client.UserResource（真上游）
//	                             →  pool.SetCredits(uid, remain)
//
// 而 `pool.SetQuota` / `pool.FromPerModel` / `QuotaKindPerModel`
// 早就为 codearts 铺好了路（见 pool/quota.go 的注释），
// **却从来没有任何调用点** —— 一条铺了没通车的路。
//
// 结果 codearts 的额度落到 `restoreQuota` 的兜底分支上，
// 被造成 `{kind:credits, has_data:true, remaining:0}` ——
// **一个把"从没查过"伪装成"查过了，确实是 0"的假数据**（实测确认，
// 见 data/state.json 的 `01a08fe0...`）。
//
// # 职责边界：核心只负责"把结果写回池"
//
// 这个接口**不解释**额度怎么算：
//
//	workbuddy → 积分（单值）
//	codearts  → 积分套餐（CreditRemain，浮点）
//
// 上游自己知道该打哪个端点、怎么解析、取不到时算不算"有数据"。
// 核心拿到的就是一个已经成型的 QuotaView，执行 SetQuota 即可。
//
// # 与 AdminExt 的关系：**不是**替代，是补充
//
// `/admin/models/quota-probe`（codearts 的按模型探测）与
// `/admin/credits/refresh`（workbuddy 的余额刷新）**都标着 CapQuotaProbe**，
// 但它们语义不同（前者是"哪个模型当前不可用"，后者是"账号还剩多少"），
// 且**都不写回池**。所以不能拿 AdminExt 冒充这条路径。
package gateway

// QuotaExt 上游自报"账号额度怎么取"。
//
// 可选实现：不实现的上游，其账号在 /admin/accounts 上额度显示为**未知**（`—`），
// 而不是 0。这是刻意的 —— 见 QuotaResult 的注释。
//
// 实现者必须满足：
//   - 不得 panic（契约要求，与 Provider 一致）
//   - 取不到额度时返回 `HasData=false`，**不要**返回一个 0 值充数
//   - uid 不存在时返回 ok=false（而不是 error）
type QuotaExt interface {
	// RefreshQuota 取某个账号的当前额度。
	//
	// 返回 (额度视图, ok)：
	//
	//	ok=false         账号不存在 / 未接线 —— 调用方据此回 404 或跳过
	//	ok=true, 有数据  Quota.HasData=true，调用方写回池
	//	ok=true, 取不到  Quota.HasData=false，**调用方不写池**
	//
	// # 为什么"取不到"要单列一种返回，而不是返回 error
	//
	// 因为调用方对两者的处理**不同**：error 要记日志报给用户，
	// 而"上游没告诉我额度"是**正常情况**（上游限流、接口临时 5xx、
	// 或该账号本来就没有额度概念）—— 它不该刷错误日志，
	// 但**必须**让界面显示成未知，而不是沿用上一次的旧值或填 0。
	//
	// 用 (QuotaView, ok) 两值不够表达三态，所以把"有没有数据"放在
	// QuotaView.HasData 里 —— 它是 pool 已有的字段，语义正好一致。
	RefreshQuota(uid string) (QuotaView, bool)
}

// QuotaView 上游报给核心的额度视图。
//
// # 为什么**不**直接复用 pool.QuotaView
//
// 因为 `internal/pool` 不得被 gateway 依赖，反之亦然 ——
// gateway 是"上游与核心之间的唯一接缝"，它必须保持**零核心依赖**，
// 否则加一个上游就要连带改 pool（判据 1 会破）。
//
// 转换由 cmd/server 的适配层做（与 SettingsExts 同一手法：
// 上游类型 ≠ 核心类型，方法集精确匹配，必须显式转换）。
//
// 字段与 pool.QuotaView 一一对应，转换是**逐字段直译**、无信息损失。
type QuotaView struct {
	// Kind 额度形态。取值见 QuotaKind* 常量。
	Kind string `json:"kind,omitempty"`

	// Remaining 单值额度（Kind == credits 时有效）。
	//
	// ⚠ 仅在 HasData 为 true 时才有意义。
	Remaining int64 `json:"remaining,omitempty"`

	// ByModel 按模型额度（Kind == per_model 时有效）。
	ByModel map[string]int64 `json:"by_model,omitempty"`

	// HasData 上游是否**真的**给出了额度数据。
	//
	// # 这个字段是整个扩展点的关键
	//
	// 它把三件事分开：
	//
	//	HasData=false              → 没查到（新账号 / 上游不报）→ 界面显示 `—`
	//	HasData=true, Remaining=0  → 查到了，确实是 0        → 界面显示 `0`
	//	HasData=true, Remaining>0  → 查到了，还有额度        → 界面显示具体数
	//
	// 混为一谈会让"没查过"显示成一个看确定的 0，
	// 用户会以为"这个号没额度了"而误删账号 —— 这正是本次要修的坑。
	HasData bool `json:"has_data,omitempty"`
}

// 额度形态常量（与 pool.QuotaKind* 取值逐字一致）。
//
// 为什么在两边各定义一份而不是共享：gateway 不能依赖 pool（同 QuotaView 的理由）。
// 两份字面量由测试钉住一致性（见 quota_ext_test.go），
// 漂移会立刻变红而不是等界面上显示错。
const (
	// QuotaKindCredits 单值额度（workbuddy 积分 / codearts 积分套餐）。
	QuotaKindCredits = "credits"
	// QuotaKindPerModel 按模型额度。
	QuotaKindPerModel = "per_model"
	// QuotaKindUnlimited 无上限（包月/试用期账号）。
	QuotaKindUnlimited = "unlimited"
)

// UnknownQuota 返回"未知额度"的规范表示。
//
// 上游取不到额度时应当**用它**，而不是 `QuotaView{}` 之外自造形态 ——
// 统一出口才能保证 `HasData=false` 这条不变量不被漏掉。
func UnknownQuota() QuotaView { return QuotaView{HasData: false} }

// CreditsQuota 从单值构造额度视图。
//
// # 为什么要这个构造函数（而不是让上游自己写结构体字面量）
//
// 核心要区分"查到 0"与"没查到"。让每个上游手写
// `QuotaView{Kind:"credits", Remaining:0, HasData:true}` 极易漏掉 HasData，
// 漏掉就退化成未知 —— 界面显示 `—` 而实际上真的没额度了。
// 构造函数把这个不变量收在一处。
//
// nil 安全：调用方传 `*Subscription` 之类的指针时不必先判空
// （上游常见写法：`sub, err := Fetch(); if err != nil { return UnknownQuota(), true }`）。
func CreditsQuota(remaining int64) QuotaView {
	return QuotaView{Kind: QuotaKindCredits, Remaining: remaining, HasData: true}
}

// PerModelQuota 从按模型额度构造（codearts 的按模型探测用）。
//
// 空表视为**未知**而不是"零额度"：一个没有任何模型条目的 per_model
// 不携带任何信息，把它当成可信的 0 会让账号被静默降权
// （pool 侧同一条判据见 restoreQuota 的 hasContent）。
func PerModelQuota(byModel map[string]int64) QuotaView {
	if len(byModel) == 0 {
		return UnknownQuota()
	}
	return QuotaView{Kind: QuotaKindPerModel, ByModel: byModel, HasData: true}
}
