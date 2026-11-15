// dryrun.go 存量权限的 **dry-run 比较器**（U-02）。
//
// # 它回答什么问题
//
// 「现在把某个令牌切到 v2 解释，它能用的模型集合会怎么变？有没有变宽的？」
//
// 这是迁移前的**硬门禁**：计划 §5 U-02 的验收写着
// 「输出非秘密差异清单，区分 unrestricted、restricted-empty 和确定模型集合；
// 　**无法解析／歧义项阻塞迁移**」，风险栏写着「**不扩大权限**」。
//
// # 为什么必须单独造一个比较器，而不是"切完看现象"
//
// 权限变化**没有运行期信号**：
//
//	· 变宽 —— 请求成功，只是这个 token 突然能用它本来不该用的模型
//	· 变窄 —— 请求 403，用户以为模型下线了
//
// 两种都不会在日志里说"这是迁移导致的"。所以只能**事前**逐条算出来。
//
// # 旧/新两套语义的差别（F-09 那个坑）
//
//	旧（覆盖）  白名单**空** = 不限；启用与否只看 ModelLimitsEnabled
//	新（交集）  组候选 ∩ 白名单；**启用且空集合 = 拒绝全部**
//
// ⇒ 最危险的一种迁移是「旧白名单空（不限）→ 新解释空集合（全拒）」的反面：
//
//	旧解释里"没配白名单"是不限，若新解释把它当成"什么都不给"，
//	那是变窄（安全但用户会炸）；反过来如果新解释把它当成"全放"，
//	那是**变宽**，必须阻塞。
package channelstore

import (
	"sort"

	"workbuddy2api/internal/groupstore"
)

// 权限集合的三种形态（计划 §5 U-02 验收里的三个词，逐字对应）。
const (
	// PermUnrestricted 不限：任何模型都可用。
	PermUnrestricted = "unrestricted"
	// PermRestrictedEmpty 受限但集合为空 ⇒ 一个模型都不能用。
	PermRestrictedEmpty = "restricted_empty"
	// PermModels 受限且给出确定集合。
	PermModels = "models"
)

// PermissionSet 一个令牌"能用哪些模型"的**分类**表示。
//
// 刻意不用 []string + 一个 bool：那个写法在调用点总有人把"空切片"
// 读成"不限"，而这正是 F-09 要防的错。三种形态是**互斥**的，用 Kind 表达。
type PermissionSet struct {
	Kind   string   `json:"kind"`
	Models []string `json:"models,omitempty"`
}

// NewPermModels 造一个"确定集合"形态（排序去重）。
func NewPermModels(models []string) PermissionSet {
	seen := map[string]bool{}
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = trimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return PermissionSet{Kind: PermRestrictedEmpty}
	}
	return PermissionSet{Kind: PermModels, Models: out}
}

// asSet 把权限集合展开成可比较的 map。unrestricted 返回 nil + true。
func (p PermissionSet) asSet() (map[string]bool, bool) {
	if p.Kind == PermUnrestricted {
		return nil, true
	}
	set := make(map[string]bool, len(p.Models))
	for _, m := range p.Models {
		set[m] = true
	}
	return set, false
}

// DryRunInput 一个令牌的**旧**权限输入（新权限由 ResolvePolicy 现算）。
type DryRunInput struct {
	Name  string
	Owner string
	// Policy 新解释要用的策略（version / 组 / 白名单）。
	Policy RoutingPolicy

	// OldGroupsResolved / OldGroupsModels 旧解释下**分组作用域给出的模型并集**。
	//
	// ⚠ 这个事实住在 apikey 侧（GroupModelsOf 认分组名与 ID 两种引用），
	// channelstore 拿不到，所以由调用方传。空 + Resolved=true 表示"分组作用域为空"
	// （⇒ 旧解释落到白名单）；Resolved=false 表示"解析不了"（⇒ 阻塞）。
	OldGroupsResolved bool
	OldGroupsModels   []string

	// OldLimitEnabled / OldLimits 旧解释的白名单（**只在分组作用域为空时才参与**）。
	OldLimitEnabled bool
	OldLimits       []string
}

// DryRunItem 一个令牌的对比结果。
//
// ⚠ 这里**没有** KeyID 字段，也不会有：它是 Bearer 值本身。
// 报告要能贴进工单，所以只带 Name / Owner。
type DryRunItem struct {
	Name  string `json:"name"`
	Owner string `json:"owner,omitempty"`
	// Applicable false = 这个令牌不吃 v2（policy_version != 2 或没有组）⇒ 无需迁移。
	Applicable bool `json:"applicable"`
	// Old / New 两套解释下的权限形态。
	Old PermissionSet `json:"old"`
	New PermissionSet `json:"new"`
	// Widened 新解释**多放出来**的模型 ⇒ 阻塞（不扩大权限）。
	Widened []string `json:"widened,omitempty"`
	// Narrowed 新解释**少给**的模型 ⇒ 不阻塞，但要在报告里看得见。
	Narrowed []string `json:"narrowed,omitempty"`
	// NarrowedAll 新解释**一个模型都不给**，而旧的给过 ⇒ 这个令牌切过去会**完全失效**。
	//
	// ⚠ 为什么必须单独一个布尔：旧"不限"→新"空集"时，Narrowed 按名字相减是**空数组**
	// （旧的没有名字可减），于是"失去全部"会被报成"什么都没变"。
	// 这是整个比较器里最容易漏的一格，而它的后果最大。
	NarrowedAll bool `json:"narrowed_all,omitempty"`
	// Blocked 非空 = 这条阻塞迁移。取值是给人看的短句。
	Blocked string `json:"blocked,omitempty"`
}

// DryRunReport 整份报告。
type DryRunReport struct {
	Items []DryRunItem `json:"items"`
	// 计数（前端直接显示，不必自己数）。
	Total        int `json:"total"`
	Applicable   int `json:"applicable"`
	Blocked      int `json:"blocked"`
	Widened      int `json:"widened"`
	Narrowed     int `json:"narrowed"`
	Unrestricted int `json:"unrestricted"`
	// CatalogKnownCatalog 报告用的目录是不是**可信全集**。
	//
	// ⚠ false 时整份报告不能当迁移依据：计划 U-02 的风险栏明确写着
	// 「动态目录无法证明完整旧权限时不能用当前快照冒充全集」。
	CatalogKnown bool `json:"catalog_known"`
	// BlockReason 整份报告被阻塞的原因（目录不可信、组表缺失等）。
	BlockReason string `json:"block_reason,omitempty"`
	// Ready 是否可以据此进入下一步（枚举全等且无阻塞）。
	Ready bool `json:"ready"`
}

// DiffPermissionSets 比较两个权限集合，返回 (变宽的, 变窄的)。
//
// 语义：
//
//	旧 unrestricted → 新 Models      变窄（新的少给了）
//	旧 Models       → 新 unrestricted 变宽 ★
//	旧 RestrictedEmpty → 新 wat      变宽（空集什么都没给，给什么都是变宽）★
//	两边都 unrestricted              无差
func DiffPermissionSets(oldSet, newSet PermissionSet) (widened, narrowed []string) {
	oldM, oldUnlimited := oldSet.asSet()
	newM, newUnlimited := newSet.asSet()

	switch {
	case oldUnlimited && newUnlimited:
		return nil, nil
	case oldUnlimited:
		// 旧的什么都能用，新的只给一部分 ⇒ 变窄。
		return nil, newSet.Models
	case newUnlimited:
		// ★ 新的什么都能用，旧的只给一部分 ⇒ 变宽。
		return oldSet.Models, nil
	}

	// 两边都是确定集合（含空集）。
	for m := range newM {
		if !oldM[m] {
			widened = append(widened, m)
		}
	}
	for m := range oldM {
		if !newM[m] {
			narrowed = append(narrowed, m)
		}
	}
	sort.Strings(widened)
	sort.Strings(narrowed)
	return widened, narrowed
}

// ComputeDryRun 算一份存量权限 dry-run 报告。
//
// channels / catalog 由调用方给：本函数**不读库**，只做计算 —— 这样隔离夹具
// 能完全离线地钉住每条分支。gs 为 nil 时所有组都算"解析不了"。
func ComputeDryRun(gs *groupstore.Store, channels []*Channel, catalog []string, inputs []DryRunInput) DryRunReport {
	report := DryRunReport{Items: make([]DryRunItem, 0, len(inputs)), Total: len(inputs)}
	// 目录必须是**已知全集**才可信。空目录 = 不知道有什么模型 ⇒ 整份报告阻塞。
	report.CatalogKnown = len(catalog) > 0
	if !report.CatalogKnown {
		report.BlockReason = "模型目录为空：无法验证旧权限是否完整，不能用空快照冒充全集"
	}
	catalogSet := make(map[string]bool, len(catalog))
	for _, m := range catalog {
		catalogSet[m] = true
	}

	// 别名歧义（判据 4）让**整个模型名空间**变歧义：
	// 一个名字有两个解时，"这把令牌能用哪些模型"就没有确定答案。
	// ⇒ 整份报告阻塞，而不是逐项阻塞（它是目录级问题，不是某个令牌的问题）。
	//
	// ⚠ 与 CatalogKnown 用「||」累积而不是覆盖：两个原因可能同时成立，
	// 只报一个会让人以为修完那个就好了。
	if ambiguous := aliasAmbiguities(channels); len(ambiguous) > 0 {
		names := make([]string, 0, len(ambiguous))
		for _, c := range ambiguous {
			names = append(names, c.Public)
		}
		sort.Strings(names)
		reason := "模型名有歧义（一语两解，无法确定它指谁）：" + joinComma(names)
		if report.BlockReason == "" {
			report.BlockReason = reason
		} else {
			report.BlockReason += "；" + reason
		}
	}

	for _, in := range inputs {
		item := DryRunItem{Name: in.Name, Owner: in.Owner}

		// ── 旧解释：**覆盖**语义（逐条对齐 apikey_models.go）──
		//
		//	① 分组作用域非空 ⇒ 就是它，白名单不参与
		//	② 分组为空      ⇒ 白名单：启用且空 = 一个都不给；启用且非空 = 那个集合
		//	③ 白名单未启用  ⇒ 不限
		switch {
		case !in.OldGroupsResolved:
			item.Old = PermissionSet{Kind: PermRestrictedEmpty}
			item.Blocked = "旧解释的分组作用域无法解析：无法证明旧权限，不能迁移"
		case len(in.OldGroupsModels) > 0:
			item.Old = NewPermModels(in.OldGroupsModels)
		case !in.OldLimitEnabled:
			item.Old = PermissionSet{Kind: PermUnrestricted}
		default:
			// 启用白名单（含列表空的情形）。
			item.Old = NewPermModels(in.OldLimits)
		}

		// ── 新解释：交集语义（ResolvePolicy）──
		policy := in.Policy
		res := ResolvePolicy(gs, channels, policy)

		switch {
		case policy.PolicyVersion != 2 || len(policy.Groups) == 0:
			// 不吃 v2 的令牌：不参与迁移，也不需要比。
			item.Applicable = false
			item.New = item.Old
		case len(res.UnresolvedGroups) > 0:
			// 组不存在 / 组本身不是 v2 ⇒ 无法解析，**阻塞**。
			item.Applicable = true
			item.New = PermissionSet{Kind: PermRestrictedEmpty}
			item.Blocked = "分组无法解析：" + joinComma(res.UnresolvedGroups)
		default:
			item.Applicable = true
			if !policy.ModelLimitsEnabled || len(policy.ModelLimits) == 0 {
				// ⚠ 这里是 F-09 的正面：**新解释下"未启用白名单"= 不收窄**，
				// 而不是"空集合 = 全拒"。启用且空才是全拒。
				if policy.ModelLimitsEnabled {
					item.New = PermissionSet{Kind: PermRestrictedEmpty}
				} else {
					item.New = NewPermModels(res.Models)
				}
			} else {
				// 交集：组候选 ∩ 白名单 —— ResolvePolicy 已经算好了。
				item.New = NewPermModels(res.Models)
			}
		}

		if item.Applicable && item.Blocked == "" {
			item.Widened, item.Narrowed = DiffPermissionSets(item.Old, item.New)
			// ★ 旧给过、新一个都不给 ⇒ 完全失效。按名字相减看不出来，必须单独判。
			if item.Old.Kind != PermRestrictedEmpty && item.New.Kind == PermRestrictedEmpty {
				item.NarrowedAll = true
			}
			// ★ 不扩大权限：只说出**新增**的模型就够阻塞了，
			// 但变宽的成因可能是"旧的是空集"（没列举），报告里要能看出来。
			if len(item.Widened) > 0 {
				item.Blocked = "新解释放出了旧解释没有的模型（不扩大权限）：" + joinComma(item.Widened)
			}
			// 解析出的模型必须都在目录里，否则是歧义（目录与配置不一致）。
			if item.Blocked == "" {
				for _, m := range item.New.Models {
					if !report.CatalogKnown || !catalogSet[m] {
						item.Blocked = "新解释给出的模型不在模型目录里（歧义）：" + m
						break
					}
				}
			}
		}

		// 计数
		if item.Applicable {
			report.Applicable++
		}
		if item.Old.Kind == PermUnrestricted {
			report.Unrestricted++
		}
		if item.Blocked != "" {
			report.Blocked++
		}
		if len(item.Widened) > 0 {
			report.Widened++
		}
		if len(item.Narrowed) > 0 || item.NarrowedAll {
			report.Narrowed++
		}
		report.Items = append(report.Items, item)
	}

	report.Ready = report.CatalogKnown && report.BlockReason == "" && report.Blocked == 0
	return report
}

func joinComma(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += "、"
		}
		out += s
	}
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
