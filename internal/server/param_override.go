// param_override.go 渠道级请求体参数覆盖（PLAN 的 T6 / 差距 G5）。
//
// # 为什么需要它
//
// 改造前，"某个渠道要改一下出站参数"只有一条路：往 handler 里加特例。
// 本仓已经走过一次：reasoning_normalize.go 的 off→删除，就是这样一个特例 ——
// 它把字段名（reasoning_effort / reasoningEffort）、取值（off/none）、
// 动作（删除）三件事**写死在一个函数里**。
//
// 换一个诉求（某渠道必须补 top_k、某上游不认 temperature、某渠道强制
// service_tier）就得再往出口层加一段 if，而出口层是**所有渠道**共用的
// 路径 —— 每加一条，风险评估面就扩大一次。
//
// 这类"每渠道一点小差异"的需求会**持续**出现。new-api 的结论是把它做成
// 渠道表上的 overrides.param 字段（前端可编辑、按渠道生效），而不是代码里的
// 特例。本文件做同一件事：渠道声明一组规则，出站前按顺序应用到 JSON body 上。
//
// # 与 new-api 的关系：抄语义，砍范围
//
// 蓝本：new-api 的 relay/common/override.go。它的 ParamOperation 支持
// 十几种 mode（set/delete/move/copy/prepend/append/trim_prefix/…/
// set_header/return_error），并带条件匹配（path + mode + value + invert）。
//
// 本文件只保留**能表达真实需求的最小集合**：
//
//	set      强制改写或新增某个字段
//	delete   强制删除某个字段（上游不认它就必须删）
//	default  仅当客户端**没传**时才设（改默认值，不覆盖客户端的显式选择）
//
// 外加一组**合取条件**（When）：本仓现成的 reasoning 规则需要它 ——
// "off/none 时才删" 用 delete + in 条件表达，而不是"把该字段无论如何删掉"。
// 没有条件，这条内置规则就退化不了（会把 low/high 也删掉，等于丢用户的意图）。
//
// 砍掉的每一项都是刻意的，不是没做完：
//
//	move / copy     本仓没有"同一个值必须同时出现在两个键上"的场景，
//	                而它们让"路径指向哪里"变成两维（from/to），审计更难读；
//	set_header      头部覆盖是另一层（handler 直接管 header），混进来会让
//	                本文件横跨请求体与请求头两个抽象，失败原子性也没法统一；
//	return_error    拦截请求是**另一个机制**（请求校验），语义上不属于"参数覆盖"；
//	正则类          regex_replace 让配置文件具备不可静态审计的能力，
//	                等真有案例再说，且到时要单独 review。
//
// 砍掉的每一条都能加回来 —— 前提是**先有一个真实案例**。
//
// # 三条硬约束（本文件的设计核心）
//
// ① **失败必须原子**：任何一条规则应用失败，都返回**原始 body**，
//
//	而不是"前面几条已经改好的 body"。
//
//	为什么：半改的 body 是**没有任何人设计过**的东西。比如规则表是
//	「删掉 reasoning_effort、再把 thinking.budget_tokens 设成 0」，
//	第二条失败时，半改的 body 既丢了客户端明确要的推理强度、
//	又没拿到关闭的效果 —— 上游行为无法预测，且报错信息会指向
//	"上游 400"，没人能从这个 400 倒推出"是我们的覆盖规则冲突"。
//	相比之下"完全不改 = 客户端原样发给上游"，行为至少是**可解释的**：
//	覆盖没生效，上游按客户端原本的请求回答。
//
//	规则本身非法（未知 op、空路径、set 缺 value）同理：解析期就报问题，
//	该条规则不进规则表，而不是在出站路径上"尽力而为"。
//
// ② **没有实际改动就不重新编码**：Applied 为空时返回的 body 与入参
//
//	**同一份字节**（连一次 json.Marshal 都不做）。
//
//	本仓已有这条纪律（见 provider_router.go 的 rewriteModel、
//	reasoning_normalize.go 的 changed 返回值）：重新序列化会改变键顺序、
//	把 0.50 写成 0.5、丢掉重复键的原始形态。对"没配覆盖规则的渠道"
//	（绝大多数）请求必须逐字节原样转发，否则每次升级都在给所有客户端
//	制造无谓的差异面。
//
// ③ **未触碰的数字必须保住字面量**：解码用 UseNumber（见 paramDecodeObject）。
//
//	用默认的 map[string]any 解码会把所有数字变成 float64，而 body 里
//	常见的 seed / 长整型 id 超过 2^53 后会**静默变值**
//	（实测形态：seed=9007199254740993 → 9007199254740992）。
//	覆盖规则只改一个字段，却把另一个字段改了值 —— 这是最坏的一类 bug：
//	它只在"恰好配了覆盖规则 + 恰好带大整数"时出现。
//	json.Number 保留原始字面量，重新编码时原样写回。
//
// # 已知边界（写在这里，免得后来者当成 bug 去"修"）
//
//  1. **键顺序会变**：真正改动时会重新 Marshal，而 Go 的 map 编码按 key 排序。
//     上游不关心键顺序；需要我们关心的是**没改动时**的字节一致（见②）。
//  2. **路径分隔符没有转义**：`.` 恒为层级分隔符，所以键名里本身带点的
//     字段（如 `a.b`）无法被寻址。new-api 用 sjson 的转义支持它，
//     但那套转义（\\. \\* \\?）本身就是一个需要文档的语法；
//     本仓没有任何带点键名的上游字段，先不引入。
//  3. **不支持在数组中间插入元素**：只支持按下标寻址/删除已有元素。
//     越界下标在 set 时是错误（否则会造出一个带空洞的数组），
//     在 delete 时是 no-op（删除本就不存在的东西是幂等的）。
//  4. **null 视为"已存在"**：`{"temperature": null}` 在 default 规则眼里
//     算客户端"传了"（与 new-api 的 gjson.Exists 口径一致）。
//     JSON null 是一个显式选择，把它当"没传"会**改写客户端的意图**。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ParamOp 规则的动词。取值是**配置契约**（渠道表里写的就是这几个词）：
// 只加不改，改一个词等于让所有已保存的渠道配置静默失效。
type ParamOp string

const (
	// ParamOpSet 强制设为某值（客户端传了别的也改）。
	//
	// 用途：上游只认某个取值、或渠道要求固定行为（强制 stream_options.include_usage）。
	ParamOpSet ParamOp = "set"

	// ParamOpDelete 强制删除该字段。
	//
	// 用途：客户端会带、但该上游明确不接受（或接受后行为很坏）的字段。
	// 删除是**幂等**的：字段本来就不存在时不算失败、也不算改动。
	ParamOpDelete ParamOp = "delete"

	// ParamOpDefault 仅在字段**不存在**时设值。
	//
	// # 为什么不叫 "set_if_missing" 之外的名字
	//
	// 这是三者里唯一容易和 set 混淆的，所以名字取"默认值"这个业务词：
	// 运维的心智是"这条渠道的默认 temperature 是 1"，
	// 而不是"我要覆盖客户端" —— 前者天然包含"客户端说了算"。
	//
	// ⚠ 存在性按**键是否出现**判定（null 也算出现，见文件头边界 4）：
	// 判空值会让 null（显式关闭）被默认值覆盖，那是在改客户端的意图。
	ParamOpDefault ParamOp = "default"
)

// 条件比较方式。同样是配置契约（只加不改）。
const (
	// ParamCondEq 值相等（按 JSON 语义比较：数字按数值比，字符串区分大小写）。
	ParamCondEq = "eq"
	// ParamCondNe 值不相等。
	ParamCondNe = "ne"
	// ParamCondIn 值命中给定数组之一（reasoning off/none 那条规则用的就是它）。
	ParamCondIn = "in"
	// ParamCondExists 路径存在（不关心值）。
	ParamCondExists = "exists"
	// ParamCondNotExists 路径不存在。
	ParamCondNotExists = "not_exists"
)

// ParamCondition 一条条件。多条之间是 **AND**（全部成立才应用规则）。
//
// # 为什么只支持 AND，不照抄 new-api 的 Logic=AND/OR
//
// 规则表的读者是运维，不是程序员。OR 让"这条规则什么时候生效"变成
// 需要分组计算的东西，而实际需求（"off/none 时删除"、"只在流式请求上补字段"）
// 全是 AND。真需要 OR 时写两条规则即可 —— 规则的表达力够，且**可读**。
type ParamCondition struct {
	// Path 要比较的路径（与规则 Path 同一套语法）。
	Path string
	// Mode 比较方式，空值等价于 eq。
	Mode string
	// Value 比较目标（eq/ne/in 用；in 要求是 []any）。
	Value any
}

// ParamRule 一条覆盖规则。
type ParamRule struct {
	// Op 动词（set / delete / default）。
	Op ParamOp
	// Path 字段路径，"a.b.c" 形式；数组用数字下标（"messages.0.content"）。
	Path string
	// Value set/default 的目标值。nil 表示 JSON null（显式写 null 是合法操作）。
	//
	// ⚠ 从配置解析时（ParseParamOverrides），set/default **必须**带 value 键，
	// 否则解析期就报"缺 value" —— 因为 `{"op":"set","path":"x"}` 更像漏写，
	// 而不像"我要把 x 设成 null"。显式写 `"value": null` 才会解析成 nil。
	Value any
	// When 可选条件，全部成立才应用本规则。
	When []ParamCondition
}

// Describe 把规则渲染成一行人类可读文本（进日志 / 审计 / 报错）。
//
// # 为什么需要它（而不是只在出错时 fmt.Sprintf(rule)）
//
// 规则的实际效果从配置里**看不出来**：default 与 set 写起来只差一个词，
// 但生效结果完全不同（一个尊重客户端、一个覆盖客户端）。
// 出站日志里记一行 "default temperature = 1" 才能解释"为什么这次请求
// 带了 temperature" —— 否则只能靠人回去翻渠道配置。
func (r ParamRule) Describe() string {
	op := string(r.Op)
	if op == "" {
		op = "?"
	}
	switch r.Op {
	case ParamOpDelete:
		return "delete " + r.Path
	case ParamOpSet, ParamOpDefault:
		return op + " " + r.Path + " = " + paramRenderValue(r.Value)
	default:
		// 未知 op 也要能打印（解析期报问题时要带上原始意图）。
		return op + " " + r.Path
	}
}

// ParamRuleProblem 一条规则的问题说明。
//
// # 为什么返回"问题列表"而不是 fail-fast 的 error
//
// 渠道配置是人手写的（或前端表单拼的），一次可能有 3 条写错。fail-fast 只会
// 让人改一条、再保存、再发现下一条 —— 三步。列表一次说清，且**坏规则不进
// 规则表、好规则照常生效**（配置是独立规则，没有"全有全无"的必要；
// 而应用中失败必须是原子的，见文件头①）。
type ParamRuleProblem struct {
	// Index 规则在表里的下标（-1 表示整体性问题，如 body 不是 JSON 对象）。
	Index int
	// Rule 规则的人类可读形态（Describe，或配置里的原始键）。
	Rule string
	// Reason 为什么不行。
	Reason string
}

// String 形如 `规则 #2（set a..b）：路径 "a..b" 第 2 段为空`。
func (p ParamRuleProblem) String() string {
	where := p.Rule
	if p.Index >= 0 {
		where = fmt.Sprintf("规则 #%d（%s）", p.Index, p.Rule)
	} else if where == "" {
		// index<0 且没有规则标签 ⇒ 整体性问题（body 不是 JSON 对象等）。
		where = "整体"
	}
	return where + "：" + p.Reason
}

// Error 让问题可直接当 error 用（便于 errors.As / 日志打印）。
func (p ParamRuleProblem) Error() string { return p.String() }

// ParamOverrides 一个渠道的规则表。
//
// 用切片而不是 map：**顺序是语义**（"先删字段再设值"与反过来结果不同，
// 比如先 default 再 delete 就一定是空的）。map 的随机迭代顺序会让
// 同一份配置每次跑出不同结果。
type ParamOverrides []ParamRule

// Apply 等价于 ApplyParamOverrides(body, o)，便于调用方持有 ParamOverrides。
func (o ParamOverrides) Apply(body []byte) ParamApplyResult {
	return ApplyParamOverrides(body, o)
}

// Empty 规则表为空（调用方据此**完全不碰** body）。
func (o ParamOverrides) Empty() bool { return len(o) == 0 }

// ParamApplyResult 一次应用的结果。
//
// # 为什么用一个结果结构体而不是 (body, error)
//
// 失败时调用方需要的**不只是** error：它还要知道"改了几条 / 哪条失败"，
// 因为这决定它怎么做（记日志 / 告警 / 忽略）。而成功时它需要 Applied
// 来解释出站 body 为什么和客户端发的不一样。
//
// ⚠ 契约：Problems 非空时，Body **必然**是调用方传进来的那一份原 body
// （见文件头①），Changed 必为 false，Applied 必为空。
type ParamApplyResult struct {
	// Body 应用后的 body；没有任何改动时与原 body 是同一份字节。
	Body []byte
	// Changed 是否有实际改动（false 时 Body 与原 body 字节一致）。
	Changed bool
	// Applied 实际**改变了 body** 的规则（Describe 形态，按应用顺序）。
	// 没有改动的规则（删除不存在的字段、set 成同一个值）不记 ——
	// 日志要回答的是"这次请求被改了什么"，不是"配置里有几条规则"。
	Applied []string
	// Problems 失败原因。非空 ⇒ Body 未被修改。
	Problems []ParamRuleProblem
}

// Err 把 Problems 汇总成一个 error（无问题时返回 nil）。
func (r ParamApplyResult) Err() error {
	if len(r.Problems) == 0 {
		return nil
	}
	parts := make([]string, 0, len(r.Problems))
	for _, p := range r.Problems {
		parts = append(parts, p.String())
	}
	return fmt.Errorf("参数覆盖未生效（原 body 保持不变）：%s", strings.Join(parts, "；"))
}

// ApplyParamOverrides 按顺序把规则应用到 JSON body 上。
//
// 纯函数：不读配置、不改入参、不写日志。调用方（出站前的那一层）决定
// 哪些渠道用哪张表、失败后是记日志还是返回错误 —— 这样本函数能被
// 表驱动测试穷举，而"要不要因为覆盖失败而拒请求"是产品决策，不该埋在这里。
func ApplyParamOverrides(body []byte, rules []ParamRule) ParamApplyResult {
	res := ParamApplyResult{Body: body}
	if len(rules) == 0 || len(body) == 0 {
		return res
	}

	root, err := paramDecodeObject(body)
	if err != nil {
		// body 不是 JSON 对象：可能是二进制透传（图片 part）、或客户端发了怪东西。
		// 这不是"规则写错了"，是"这条请求不适用规则"。返回原 body 并说明。
		res.Problems = []ParamRuleProblem{{Index: -1, Rule: "全部规则", Reason: err.Error()}}
		return res
	}

	var node any = root
	applied := make([]string, 0, len(rules))
	for i, rule := range rules {
		next, changed, err := paramApplyRule(node, rule)
		if err != nil {
			// 原子失败：丢弃半成品 node，交出原始 body。
			res.Applied = nil
			res.Problems = []ParamRuleProblem{{Index: i, Rule: rule.Describe(), Reason: err.Error()}}
			return res
		}
		if !changed {
			continue
		}
		node = next
		applied = append(applied, rule.Describe())
	}

	if len(applied) == 0 {
		// 全是 no-op：不重新编码（见文件头②）。
		return res
	}

	out, err := json.Marshal(node)
	if err != nil {
		// 走到这里说明规则值里有 json.Marshal 不支持的东西
		// （例如调用方在 Go 里手搓规则时塞了 chan/func）。同样原子失败。
		res.Problems = []ParamRuleProblem{{Index: -1, Rule: "全部规则", Reason: "结果无法序列化成 JSON：" + err.Error()}}
		return res
	}
	res.Body = out
	res.Changed = true
	res.Applied = applied
	return res
}

// paramApplyRule 应用一条规则，返回（可能是新的）节点与"是否真的改了"。
func paramApplyRule(node any, rule ParamRule) (any, bool, error) {
	segs, err := paramParsePath(rule.Path)
	if err != nil {
		return node, false, err
	}
	if len(rule.When) > 0 {
		ok, err := paramConditionsHold(node, rule.When)
		if err != nil {
			return node, false, err
		}
		if !ok {
			return node, false, nil
		}
	}
	switch rule.Op {
	case ParamOpSet:
		return paramSetPath(node, segs, rule.Value)
	case ParamOpDefault:
		// "仅当客户端没传"：存在性判定必须在写之前（写之后恒存在）。
		if _, exists := paramGetPath(node, segs); exists {
			return node, false, nil
		}
		return paramSetPath(node, segs, rule.Value)
	case ParamOpDelete:
		return paramDeletePath(node, segs)
	default:
		// 解析期已拦住未知 op；手搓规则（或从别处反序列化）仍可能到这。
		return node, false, fmt.Errorf("未知的操作 %q（只支持 set / delete / default）", rule.Op)
	}
}

// paramSetPath 在 segs 指定的位置写入 value，返回（新节点, 是否改动）。
//
// # 为什么中间节点不存在时"自动建对象"而不是报错
//
// set a.b.c 的意图显然包含"a.b 不存在就建出来"，否则跨三层以上的规则
// 永远写不成。JSON 里对象是唯一合理的容器（建数组需要猜下标）。
//
// # 为什么中间节点是**标量**时反而报错
//
// 反例是真实存在的：客户端可能发 `{"metadata": "abc"}`，而规则写
// `set metadata.trace_id x`。此时唯一"能成功"的做法是把 "abc" 静默替换成
// 对象 —— 客户端原本的数据就此消失，而且上游只会看到一份它没预期的 body。
// 报错（→ 原子失败 → 原 body 发给上游）虽然少了覆盖，但没有破坏任何数据。
//
// ⚠ 中间节点是 **null** 时按"不存在"处理（建对象）：JSON null 通常意味着
// "这个键没有内容"，而不是"这里有个叫 null 的值"，建对象符合直觉。
func paramSetPath(node any, segs []string, value any) (any, bool, error) {
	head := segs[0]
	switch cur := node.(type) {
	case map[string]any:
		if len(segs) == 1 {
			if old, ok := cur[head]; ok && paramValueEqual(old, value) {
				return cur, false, nil // 已经是目标值 ⇒ 不算改动（保住字节一致）
			}
			cur[head] = value
			return cur, true, nil
		}
		child, ok := cur[head]
		if !ok || child == nil {
			child = map[string]any{}
		}
		next, changed, err := paramSetPath(child, segs[1:], value)
		if err != nil {
			return cur, false, err
		}
		if changed {
			cur[head] = next
		}
		return cur, changed, nil

	case []any:
		idx, err := paramSliceIndex(head, len(cur))
		if err != nil {
			return cur, false, err
		}
		if len(segs) == 1 {
			if paramValueEqual(cur[idx], value) {
				return cur, false, nil
			}
			cur[idx] = value
			return cur, true, nil
		}
		child := cur[idx]
		if child == nil {
			// 与 map 分支同一口径：null 视为"没有内容"，建对象。
			child = map[string]any{}
		}
		next, changed, err := paramSetPath(child, segs[1:], value)
		if err != nil {
			return cur, false, err
		}
		if changed {
			cur[idx] = next
		}
		return cur, changed, nil

	default:
		return node, false, fmt.Errorf("路径前段 %q 命中了%s类型的已有值，不是对象/数组："+
			"覆盖会把它静默替换成结构体（丢掉客户端原值），因此拒绝", head, paramTypeName(node))
	}
}

// paramDeletePath 删除 segs 指定的字段，返回（新节点, 是否改动, 错误）。
//
// # 为什么删除是"永不失败"的
//
// 删除的语义是"保证它不在"。字段本来不存在时，这个保证**已经成立** ——
// 报错只会让一条正确的规则去拖垮整张规则表（原子失败会把所有覆盖都废掉）。
// 数组越界、路径穿过标量，同理都是 no-op。
//
// ⚠ 数组元素删除用 full slice expression（cur[:idx:idx]），避免
// append 复用底层数组时把**相邻节点的数据**改掉：本函数是在共享的
// 解码树上原地改，一个 alias bug 会表现为"删了 A 数组的元素，B 数组跟着少了"。
func paramDeletePath(node any, segs []string) (any, bool, error) {
	head := segs[0]
	switch cur := node.(type) {
	case map[string]any:
		child, ok := cur[head]
		if !ok {
			return cur, false, nil
		}
		if len(segs) == 1 {
			delete(cur, head)
			return cur, true, nil
		}
		next, changed, err := paramDeletePath(child, segs[1:])
		if err != nil {
			return cur, false, err
		}
		if changed {
			cur[head] = next
		}
		return cur, changed, nil

	case []any:
		// ⚠ 语法错误（不是数字 / 负数）必须报上去：那是**规则写错了**，
		// 与"这个元素不存在"是两件事。只把"越界"当 no-op —— 越界时
		// "保证它不在"这个目标已经成立，报错只会让一条无害规则拖垮整张表。
		idx, err := paramParseIndex(head)
		if err != nil {
			return cur, false, err
		}
		if idx >= len(cur) {
			return cur, false, nil
		}
		if len(segs) == 1 {
			return append(cur[:idx:idx], cur[idx+1:]...), true, nil
		}
		next, changed, err := paramDeletePath(cur[idx], segs[1:])
		if err != nil {
			return cur, false, err
		}
		if changed {
			cur[idx] = next
		}
		return cur, changed, nil

	default:
		// nil 或标量：没有更深的层级可删。
		return node, false, nil
	}
}

// paramGetPath 读取路径上的值；exists=false 表示路径不存在（或不合法）。
//
// 与 paramDeletePath 同一条取向：读不到不是错误。调用方是 default 的
// 存在性判定与条件求值，它们对"不存在"都有明确语义（设值 / 条件不成立）。
func paramGetPath(node any, segs []string) (any, bool) {
	for _, seg := range segs {
		switch cur := node.(type) {
		case map[string]any:
			v, ok := cur[seg]
			if !ok {
				return nil, false
			}
			node = v
		case []any:
			idx, err := paramSliceIndex(seg, len(cur))
			if err != nil {
				return nil, false
			}
			node = cur[idx]
		default:
			return nil, false
		}
	}
	return node, true
}

// paramConditionsHold 全部条件成立 ⇒ true。
func paramConditionsHold(node any, conds []ParamCondition) (bool, error) {
	for i, cond := range conds {
		segs, err := paramParsePath(cond.Path)
		if err != nil {
			return false, fmt.Errorf("第 %d 个条件：%w", i+1, err)
		}
		val, exists := paramGetPath(node, segs)
		ok, err := paramConditionMatch(cond, val, exists)
		if err != nil {
			return false, fmt.Errorf("第 %d 个条件：%w", i+1, err)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// paramConditionMatch 单个条件的判定。
func paramConditionMatch(cond ParamCondition, val any, exists bool) (bool, error) {
	mode := cond.Mode
	if mode == "" {
		mode = ParamCondEq
	}
	switch mode {
	case ParamCondExists:
		return exists, nil
	case ParamCondNotExists:
		return !exists, nil
	}

	// ⚠ 路径不存在时，除 not_exists 外的条件一律**不成立**。
	//
	// 反例：把"不存在"当成零值参与比较，会让 eq "" / eq 0 在字段缺失时命中，
	// 于是 `when reasoning_effort eq "off"` 会在客户端**根本没传**时也触发删除 ——
	// 看起来无害（删一个不存在的字段），但它掩盖了真正的 bug：
	// 条件该命中而没命中时，人会以为是"条件写错了"而不是"路径拼错了"。
	// 缺失要能被 not_exists 显式表达，而不是被 eq 顺带匹配。
	if !exists {
		return false, nil
	}

	switch mode {
	case ParamCondEq:
		return paramValueEqual(val, cond.Value), nil
	case ParamCondNe:
		return !paramValueEqual(val, cond.Value), nil
	case ParamCondIn:
		list, ok := cond.Value.([]any)
		if !ok {
			return false, fmt.Errorf("mode=in 的 value 必须是数组，得到 %s", paramTypeName(cond.Value))
		}
		for _, item := range list {
			if paramValueEqual(val, item) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("未知的比较方式 %q（支持 eq/ne/in/exists/not_exists）", mode)
	}
}

// paramValueEqual 按 JSON 语义比较两个值。
//
// # 为什么不能直接 reflect.DeepEqual
//
// 三个来源的值形状不一致：配置解析出 json.Number、Go 手搓规则用 int、
// 测试里常写 float64。DeepEqual(json.Number("1"), float64(1)) 是 false，
// 于是"set 成同一个值"会被判成改动（破坏字节一致性），
// "条件 eq 1"也会静默不成立 —— 而这两种失效都**没有任何报错**。
//
// 所以先归一化（数字一律 float64、容器递归），再比较。
// 代价是超过 2^53 的整数比较会失真 —— 但那种值只可能出现在 body 的
// 未触碰字段里（UseNumber 保住了字面量），规则里的值不会那么大。
func paramValueEqual(a, b any) bool {
	return reflect.DeepEqual(paramNormalizeValue(a), paramNormalizeValue(b))
}

// paramNormalizeValue 把值归一到可比较的形态（见 paramValueEqual）。
func paramNormalizeValue(v any) any {
	switch t := v.(type) {
	case json.Number:
		// 能转成 float64 就当数字比（"0.70" 与 0.7 是同一个值）；
		// 转不了（比如超长整数）就按字面量比，至少不会误判成"相等"。
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint:
		return float64(t)
	case uint64:
		return float64(t)
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = paramNormalizeValue(t[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = paramNormalizeValue(item)
		}
		return out
	default:
		return v
	}
}

// paramDecodeObject 把 body 解码成 JSON 对象树。
//
// ⚠ UseNumber 是硬要求，理由见文件头③（seed / 长整型 id 会静默变值）。
func paramDecodeObject(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON：%v", err)
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("请求体顶层是%s，不是 JSON 对象：参数覆盖只能作用于对象", paramTypeName(root))
	}
	return obj, nil
}

// paramParsePath 把 "a.b.0.c" 切成路径段。
//
// 空段（"a..b" / "a." / ".a"）一律报错而不是**猜**：猜"跳过空段"会让
// 一个多打了点的配置静默生效在别的路径上 —— 而覆盖规则写错位置，
// 表现是"上游行为变了但没报错"，最短的排障路径是让它**在解析期就炸**。
func paramParsePath(path string) ([]string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, fmt.Errorf("路径为空")
	}
	segs := strings.Split(trimmed, ".")
	for i, seg := range segs {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			return nil, fmt.Errorf("路径 %q 第 %d 段为空（多打了点，或首尾有点）", path, i+1)
		}
		segs[i] = seg
	}
	return segs, nil
}

// paramParseIndex 只做**语法**判定：必须是 >= 0 的十进制整数。
//
// # 为什么负数在这里就拒（而不是像 new-api 那样解析成"倒数第 N 个"）
//
// new-api 的 processNegativeIndex 支持 -1 = 最后一个元素。本仓不实现它，
// 但**必须承认它的存在**：运维写过 new-api 的规则，很可能写下 `stop.-1`。
// 如果我们按"路径不存在"静默放过，结果是**配了但永远不生效**，
// 而日志一切正常 —— 这是最难查的一类配置错误。
// 所以负数在语法层就拒（→ 规则报问题 → 人能看见），
// 而不是在运行期悄悄变成 no-op。
func paramParseIndex(seg string) (int, error) {
	idx, err := strconv.Atoi(seg)
	if err != nil {
		return 0, fmt.Errorf("段 %q 不是数组下标（该位置是数组）", seg)
	}
	if idx < 0 {
		return 0, fmt.Errorf("不支持负数下标 %d（new-api 里 -1 表示倒数第一个，本仓未实现）", idx)
	}
	return idx, nil
}

// paramSliceIndex 解析下标并检查范围（set 用：越界必须报错，不能造空洞）。
func paramSliceIndex(seg string, length int) (int, error) {
	idx, err := paramParseIndex(seg)
	if err != nil {
		return 0, err
	}
	if idx >= length {
		return 0, fmt.Errorf("数组下标 %d 越界（长度 %d）：覆盖不会自动补空元素", idx, length)
	}
	return idx, nil
}

// paramTypeName 把值翻译成中文类型名（用于报错，读者是运维不是编译器）。
func paramTypeName(v any) string {
	switch v.(type) {
	case nil:
		return " null "
	case bool:
		return "布尔值"
	case string:
		return "字符串"
	case json.Number, float64, float32, int, int32, int64, uint, uint64:
		return "数字"
	case map[string]any:
		return "对象"
	case []any:
		return "数组"
	default:
		return fmt.Sprintf(" %T ", v)
	}
}

// paramRenderValue 把规则值渲染成简短文本（Describe 用）。
func paramRenderValue(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	// 超长值（例如规则里塞了整段提示词）截断：日志里一行看不完没有价值。
	// ⚠ 名字不用 max：Go 1.21+ 的内建函数叫 max，遮蔽它会让后来者
	// 以为在用内建（而且 gofmt/vet 都不会提醒）。
	const maxRender = 120
	if len(b) > maxRender {
		return string(b[:maxRender]) + "…"
	}
	return string(b)
}

// ParseParamOverrides 把渠道配置（channelstore.Overrides.Param，JSON 解码后的
// map）编译成规则表。
//
// 两种写法都接受，因为它们的用途不同：
//
//	正式形态（有顺序、能表达 delete/default/条件）：
//	  {"operations":[{"op":"default","path":"temperature","value":1},
//	                 {"op":"delete","path":"reasoning_effort",
//	                  "when":{"path":"reasoning_effort","mode":"in","value":["off","none"]}}]}
//
//	简写形态（只表达 set，键就是路径）：
//	  {"temperature":1, "stream_options.include_usage":true}
//
// 简写形态沿用 new-api legacy override 的口径（键 = sjson 路径，值 = set 目标），
// 目的是让"只改一个默认值"这种最常见需求不必写一个对象数组。
//
// ⚠ 简写形态的参数是 map（无序），这里**按 key 排序**后再建规则：
// 否则同一份配置每次解析出不同顺序，日志里的 Applied 抖动，测试也无法断言。
// 真需要顺序时用正式形态 —— 顺序是语义（见 ParamOverrides 的注释）。
//
// ⚠ 两种写法**互斥**：出现 "operations" 键就只按正式形态解析，其余键被忽略。
// 混写（既写 operations 又写 {"temperature":1}）不报错但会静默丢掉简写部分 ——
// 这里刻意选择"明确优先"而不是"合并"：合并要回答"谁先谁后"，
// 而顺序是语义（见 ParamOverrides），一个没写在配置里的顺序不该由实现来决定。
// 若真要合并，请把简写项也写成 operations 里的一条。
//
// 返回的规则表**不含**有问题的规则（问题在第二个返回值里）。调用方应当把
// 问题记进日志（配置错误不该静默），但**不该拒绝请求** —— 一条写错的规则
// 不应该让整个渠道不可用。
func ParseParamOverrides(raw map[string]any) (ParamOverrides, []ParamRuleProblem) {
	if len(raw) == 0 {
		return nil, nil
	}
	if opsRaw, ok := raw["operations"]; ok {
		return parseParamRuleList(opsRaw)
	}

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(ParamOverrides, 0, len(keys))
	var problems []ParamRuleProblem
	for i, key := range keys {
		if _, err := paramParsePath(key); err != nil {
			problems = append(problems, ParamRuleProblem{Index: i, Rule: key, Reason: err.Error()})
			continue
		}
		out = append(out, ParamRule{Op: ParamOpSet, Path: key, Value: raw[key]})
	}
	return out, problems
}

// parseParamRuleList 解析正式形态的 operations 数组。
func parseParamRuleList(opsRaw any) (ParamOverrides, []ParamRuleProblem) {
	if opsRaw == nil {
		return nil, nil
	}
	list, ok := opsRaw.([]any)
	if !ok {
		if typed, ok2 := opsRaw.([]map[string]any); ok2 {
			list = make([]any, len(typed))
			for i := range typed {
				list[i] = typed[i]
			}
		} else {
			return nil, []ParamRuleProblem{{
				Index:  -1,
				Rule:   "operations",
				Reason: fmt.Sprintf("operations 必须是规则数组，得到 %s", paramTypeName(opsRaw)),
			}}
		}
	}

	out := make(ParamOverrides, 0, len(list))
	var problems []ParamRuleProblem
	for i, item := range list {
		raw, ok := item.(map[string]any)
		if !ok {
			problems = append(problems, ParamRuleProblem{
				Index:  i,
				Rule:   fmt.Sprintf("%v", item),
				Reason: fmt.Sprintf("规则必须是对象，得到 %s", paramTypeName(item)),
			})
			continue
		}
		rule, problem := parseParamRule(i, raw)
		if problem != nil {
			problems = append(problems, *problem)
			continue
		}
		out = append(out, rule)
	}
	return out, problems
}

// parseParamRule 解析单条规则；problem 非 nil 表示这条不能进规则表。
func parseParamRule(idx int, raw map[string]any) (ParamRule, *ParamRuleProblem) {
	opText := paramStringField(raw, "op")
	if opText == "" {
		// 兼容 new-api 的字段名（它的 ParamOperation 用 "mode"）。
		// 成本两行，收益是照抄来的配置不用改写 —— 也包括文档/教程里的样例。
		opText = paramStringField(raw, "mode")
	}
	rule := ParamRule{
		Op:   ParamOp(strings.ToLower(opText)),
		Path: paramStringField(raw, "path"),
	}

	fail := func(reason string) (ParamRule, *ParamRuleProblem) {
		return ParamRule{}, &ParamRuleProblem{Index: idx, Rule: rule.Describe(), Reason: reason}
	}

	switch rule.Op {
	case ParamOpSet, ParamOpDefault, ParamOpDelete:
	default:
		return fail(fmt.Sprintf("未知的操作 %q（只支持 set / delete / default）", opText))
	}

	if _, err := paramParsePath(rule.Path); err != nil {
		return fail(err.Error())
	}

	if rule.Op != ParamOpDelete {
		value, hasValue := raw["value"]
		if !hasValue {
			// 键存在性判定（而不是判 nil）：JSON 的 "value": null 与"没写 value"
			// 在 map 里是两种形态，前者是合法的"设成 null"，后者是漏写。
			return fail(fmt.Sprintf("%s 缺少 value 字段（要把值设成 null 请显式写 \"value\": null）", rule.Op))
		}
		rule.Value = value
	}

	if whenRaw, hasWhen := raw["when"]; hasWhen && whenRaw != nil {
		conds, err := parseParamConditions(whenRaw)
		if err != nil {
			return fail(err.Error())
		}
		rule.When = conds
	}

	return rule, nil
}

// parseParamConditions 解析 when（单个条件对象，或条件数组 = AND）。
func parseParamConditions(raw any) ([]ParamCondition, error) {
	if list, ok := raw.([]any); ok {
		out := make([]ParamCondition, 0, len(list))
		for i, item := range list {
			cond, err := parseParamCondition(item)
			if err != nil {
				return nil, fmt.Errorf("第 %d 个条件：%w", i+1, err)
			}
			out = append(out, cond)
		}
		return out, nil
	}
	cond, err := parseParamCondition(raw)
	if err != nil {
		return nil, err
	}
	return []ParamCondition{cond}, nil
}

// parseParamCondition 解析单个条件。
func parseParamCondition(raw any) (ParamCondition, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return ParamCondition{}, fmt.Errorf("条件必须是对象（形如 {\"path\":\"x\",\"mode\":\"eq\",\"value\":\"y\"}），得到 %s", paramTypeName(raw))
	}
	cond := ParamCondition{
		Path: paramStringField(m, "path"),
		Mode: strings.ToLower(paramStringField(m, "mode")),
	}
	if cond.Mode == "" {
		cond.Mode = ParamCondEq
	}
	if _, err := paramParsePath(cond.Path); err != nil {
		return ParamCondition{}, err
	}

	switch cond.Mode {
	case ParamCondExists, ParamCondNotExists:
		// 只看存在性，不需要 value。
		return cond, nil
	case ParamCondEq, ParamCondNe:
		value, hasValue := m["value"]
		if !hasValue {
			return ParamCondition{}, fmt.Errorf("mode=%s 需要 value 字段", cond.Mode)
		}
		cond.Value = value
	case ParamCondIn:
		value, hasValue := m["value"]
		if !hasValue {
			return ParamCondition{}, fmt.Errorf("mode=in 需要 value 字段（数组）")
		}
		list, ok := value.([]any)
		if !ok {
			return ParamCondition{}, fmt.Errorf("mode=in 的 value 必须是数组，得到 %s", paramTypeName(value))
		}
		cond.Value = list
	default:
		return ParamCondition{}, fmt.Errorf("未知的比较方式 %q（支持 eq/ne/in/exists/not_exists）", cond.Mode)
	}
	return cond, nil
}

// paramStringField 宽容地取字符串字段（非字符串按空串处理）。
func paramStringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// ReasoningOffOverrideRules 把本仓现有的"reasoning_effort off → 删除"特例
// 表达成一组覆盖规则。
//
// # 为什么这条内置规则放在本文件
//
// reasoning_normalize.go 现在的实现就是这个语义，但写死成函数。等集成层
// 把出站路径切到 ParamOverrides 之后，那个函数应当**退化成一条内置规则**
// （PLAN §4 风险表："T6 落地后，归一化退化为 override 的默认规则"）。
// 这里先把那条规则以数据形态写出来，让"接口能不能表达它"成为**可编译、
// 可测试**的事实，而不是一句口头承诺（见 TestParamOverrideReasoningParity）。
//
// ⚠ 两个键名都覆盖：本仓客户端两种写法都出现过（reasoning_effort 与
// reasoningEffort），只删一个会让另一种写法继续打出 400。
//
// ⚠ 条件是 in ["off","none"] 而不是无条件删除：low/high/max 是各上游
// 都认的合法值，删掉它们等于把用户"要更强推理"的意图丢了。
func ReasoningOffOverrideRules() ParamOverrides {
	paths := []string{"reasoning_effort", "reasoningEffort"}
	rules := make(ParamOverrides, 0, len(paths))
	for _, path := range paths {
		rules = append(rules, ParamRule{
			Op:   ParamOpDelete,
			Path: path,
			When: []ParamCondition{{
				Path:  path,
				Mode:  ParamCondIn,
				Value: []any{"off", "none"},
			}},
		})
	}
	return rules
}
