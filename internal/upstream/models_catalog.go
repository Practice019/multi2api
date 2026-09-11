// models_catalog.go 上游模型目录（GET /v3/config）接口封装。
//
// 与 client.go 里的 FetchModels（/console/enterprises/personal/models）**不是一回事**，
// 两个端点必须同时保留：
//
//	/console/enterprises/personal/models  → 带 isDefault / reasoning.supportedEfforts，
//	                                        是「有哪些模型可用」的来源（FetchModels 用）；
//	/v3/config                            → 每个模型带 credits 系数串（如 "x0.51 credits"），
//	                                        是「同样的 token 花多少倍价钱」的唯一来源。
//
// 实测两边的 model id 集合**不相同**、字段也**不重叠**，谁也替代不了谁。
// 本文件的职责是把 credits 系数拿下来并解析成 float64，供成本归因使用。
//
// 关键事实（实测，改动前请勿凭直觉重写）：
//   - base 是 chatBase（https://copilot.tencent.com）；
//   - 必需头：Authorization: Bearer <token>、Accept: application/json、
//     User-Agent: CLI/2.63.2 CodeBuddy/2.63.2。缺它们上游直接拒（401/403）。
//     本包用 BillingHeaders —— 它恰好设了前两个，User-Agent 由本文件单独补；
//   - data.models 是对象数组，credits 是**字符串** "x<小数> credits"，不是数字；
//   - data.agents / data.productFeatures 同时存在，但本任务不需要，故意不解析。
package upstream

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"workbuddy2api/internal/auth"
)

// modelsCatalogPath 模型目录端点路径（挂在 chatBase 之后）。
const modelsCatalogPath = "/v3/config"

// creditsPrefix / creditsSuffix 是 credits 字符串的字面构成："x0.51 credits"。
// 解析走「剥前后缀再 strconv.ParseFloat」而不是正则，省一次编译与一次分配。
const (
	creditsPrefix = "x"
	creditsSuffix = "credits"
)

// ModelCatalogEntry 模型目录条目。CreditsRaw 是上游原样字符串，Multiplier 是解析后的数值。
//
// 为什么两个都留：Multiplier 是给成本计算用的，CreditsRaw 是排查用 ——
// 当上游换了格式（例如 "x0.51x credits"）导致 Multiplier 变 0 时，
// 日志里能看到原文，否则只能看到一排 0 猜原因。
type ModelCatalogEntry struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Vendor         string   `json:"vendor"`
	Tags           []string `json:"tags"`
	CreditsRaw     string   `json:"credits"`
	Multiplier     float64  `json:"-"`
	MaxInputTokens int64    `json:"maxInputTokens"`

	SupportsToolCall  bool `json:"supportsToolCall"`
	SupportsImages    bool `json:"supportsImages"`
	SupportsReasoning bool `json:"supportsReasoning"`
}

// ParseCreditsMultiplier 把上游的 credits 系数串解析成 float64。
//
// 接受的形态是 "x<小数> credits"（实测样本："x0.00 credits" / "x0.51 credits" /
// "x2.00 credits" / "x5.00 credits"）。宽容度（都来自真实数据的不确定性）：
//   - 前后空白任意多；
//   - "credits" 后缀可缺（上游若改名不至于全线归零）；
//   - 大小写不敏感（"X0.51 CREDITS"）；
//   - 小数点写作 ".5" 或 "5." 也认（strconv 的原生行为）。
//
// 一律返回 0（而不是错误）的情形：空串、只有 "x"、没有 "x" 前缀、
// 前后缀之间有杂质、ParseFloat 失败、负数、NaN/Inf。
//
// 「返回 0 而不是负数」是刻意的：调用方按 `if m > 0` 判断系数可用性，
// 让坏数据退化成「没有该模型的信息」，而不是「倒贴钱」。
// 本函数是纯函数：不读全局、不写全局、任何输入都不 panic。
func ParseCreditsMultiplier(s string) float64 {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0
	}
	// 前缀：必须有 "x"/"X"，且后面还得有内容（裸 "x" 走 ParseFloat 也是失败，这里提前挡掉）。
	if len(t) < 2 || (t[0] != 'x' && t[0] != 'X') {
		return 0
	}
	t = t[1:]

	// 后缀：大小写不敏感地剥掉尾部 "credits"（及其后空白）。
	t = strings.TrimSpace(t)
	if len(t) >= len(creditsSuffix) &&
		strings.EqualFold(t[len(t)-len(creditsSuffix):], creditsSuffix) {
		t = strings.TrimSpace(t[:len(t)-len(creditsSuffix)])
	}
	if t == "" {
		return 0
	}

	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0
	}
	// 负数 / NaN / Inf 一律归零：上游出现这些值属于格式异常，不是有效成本系数。
	//
	// `v == 0` 单独判一次是为了挡 **负零**："x-0" 会被 ParseFloat 解析成 -0，
	// 而 IEEE-754 规定 -0.0 < 0 为 **false**，所以只判 `v < 0` 会让它漏过去。
	// -0 虽然在任何 `> 0` 判断下都等价于 0，但 `math.Signbit(-0)` 为真，
	// 输出到 JSON 里是 `-0`，看着像 bug 也容易让下游的符号判断出错。
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v == 0 {
		return 0
	}
	return v
}

// ModelCatalog 一份模型目录快照。
//
// 用命名类型（而不是裸 []ModelCatalogEntry）是为了后续能加字段
// （例如 fetchedAt / productFeatures）而不破坏调用方签名。
type ModelCatalog struct {
	Models []ModelCatalogEntry
}

// MultiplierTable 返回 id → 系数的映射，便于按模型名直接查成本系数。
//
// 只收 Multiplier > 0 的条目：系数为 0 意味着「免费/未知」，
// 让调用方用 map 的 comma-ok 区分「没这个模型」和「这个模型 0 系数」会更容易出错，
// 需要原始列表时直接用 ModelCatalog.Models。
//
// 空结果一律返回 **nil**（统一契约，评审指出过不一致）：
// 曾经用 `len(mc.Models) == 0` 判空，于是「没有模型」返回 nil、
// 「有模型但系数全是 0」返回非 nil 空 map —— 两个同样"空"的结果对
// `if tbl == nil` 含义不同，调用方会写出只在其中一种情况下正确的分支。
// 现在按**结果**判空：收完若一个都没进，返回 nil。
func (mc *ModelCatalog) MultiplierTable() map[string]float64 {
	if mc == nil {
		return nil
	}
	out := make(map[string]float64, len(mc.Models))
	for _, m := range mc.Models {
		if m.ID != "" && m.Multiplier > 0 {
			out[m.ID] = m.Multiplier
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Multiplier 查询单个模型的成本系数；不存在或系数为 0 时返回 (0, false)。
//
// 线扫 O(n)：真实目录只有 29 个模型，且调用点在每次请求里至多几次，
// 不值得为它维护一份缓存表（那会引入「缓存与 Models 不同步」的新失败模式）。
// 需要批量查时用 MultiplierTable 一次建表。
func (mc *ModelCatalog) Multiplier(modelID string) (float64, bool) {
	if mc == nil {
		return 0, false
	}
	for _, m := range mc.Models {
		if m.ID == modelID {
			if m.Multiplier <= 0 {
				return 0, false
			}
			return m.Multiplier, true
		}
	}
	return 0, false
}

// catalogData /v3/config 的 data 段（**已由 doJSON 解掉外层信封**）。
//
// ⚠ 这里**不能**再套一层 `{"data":{...}}`：doJSON 已经把 `env.Data` 返回回来了
// （见 client.go 的 `return env.Data, nil`）。写成嵌套结构会得到 0 个模型且不报错 ——
// 实测踩过：真机上 models=0，而所有「路径/请求头」断言都还是通过的，
// 只有「解析出 4 个模型」这条才发现。
//
// 只声明本任务要用的字段：data.agents 与 data.productFeatures 也在响应里，
// 但解析它们纯属浪费，且会让「上游改了 agents 结构」变成一个假失败。
type catalogData struct {
	Models []ModelCatalogEntry `json:"models"`
}

// FetchModelCatalog 拉取并解析上游模型目录（GET /v3/config）。
//
// 签名与 GrowthTasks / GrowthStreak 保持一致的风格：单个 *auth.Auth 入参，
// 返回指针 + error。失败时返回 (nil, err)，**调用方应忽略错误继续主流程** ——
// 模型目录是锦上添花，拿不到不该影响任何一次对话。
//
// 实测必需头（缺了上游直接拒），这里三件套齐上：
//
//	Authorization: Bearer <accessToken>   ← BillingHeaders
//	Accept: application/json              ← BillingHeaders
//	User-Agent: CLI/2.63.2 CodeBuddy/2.63.2 ← 本函数单独补（BillingHeaders 不设 UA）
func (c *Client) FetchModelCatalog(a *auth.Auth) (*ModelCatalog, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+modelsCatalogPath, nil)
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	req.Header.Set("User-Agent", clientUA)

	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	// doJSON 返回的已经是 data 段，这里直接解 Models —— 见 catalogData 的注释。
	//
	// 空 data 要当作「空目录」而不是错误：上游在「this 账号没有可见模型」之类的
	// 情况下会回 `{"code":0,"msg":"OK"}`（没有 data 字段），此时 doJSON 返回的是
	// 长度为 0 的 RawMessage，直接 Unmarshal 会报 "unexpected end of JSON input"。
	// 这与本包其它接口（如 GrowthClaim）的「缺 data 按无事发生」是同一套语义。
	var cd catalogData
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cd); err != nil {
			return nil, err
		}
	}
	out := &ModelCatalog{Models: make([]ModelCatalogEntry, 0, len(cd.Models))}
	for _, m := range cd.Models {
		// 每次 Fetch 都重算系数，不信任上游可能（但实测没有）直接给数字的字段。
		m.Multiplier = ParseCreditsMultiplier(m.CreditsRaw)
		out.Models = append(out.Models, m)
	}
	return out, nil
}

// SortByMultiplierDesc 按成本系数从高到低就地排序，同系数按 id 升序保证结果稳定。
//
// 稳定性不是洁癖：成本报表要能跨多次拉取 diff，
// 同系数模型每次顺序不同会让报表看起来「变了」。
//
// 入参要求：Models 里每个条目的 Multiplier 应为**有限值**。
// 正常路径由 ParseCreditsMultiplier 保证（它拒绝 NaN/Inf/负数）；
// 但字段是导出的，外部若塞进 NaN，下面的 less 仍做了兜底（见 less 的注释），
// 只是那种情况下排序结果不再有确定语义。本方法就地修改、不加锁：
// 调用方若把同一份 *ModelCatalog 共享给多个 goroutine，需要自己串行化。
func (mc *ModelCatalog) SortByMultiplierDesc() {
	if mc == nil {
		return
	}
	ms := mc.Models
	// 手写插入排序：29 个元素，且不引 sort 包之外的任何东西也不值得。
	for i := 1; i < len(ms); i++ {
		cur := ms[i]
		j := i - 1
		for j >= 0 && less(cur, ms[j]) {
			ms[j+1] = ms[j]
			j--
		}
		ms[j+1] = cur
	}
}

// less 定义「排在前面」：系数大的在前，系数相同的 id 小的在前。
//
// NaN 兜底（评审指出 less 在 NaN 下不是严格弱序：`NaN != x` 为真且
// `NaN > x` 为假，会同时满足 less(a,b) 与 less(b,a)，排序结果不确定）：
// 这里显式把 NaN 当作**最小**，使 less 成为全序。
// 正常路径永远构造不出 NaN（ParseCreditsMultiplier 拒绝它），
// 这层兜底是给「导出字段被外部写入 NaN」留的安全网 ——
// 宁可排到末尾，也不要产生不确定顺序或 panic。
func less(a, b ModelCatalogEntry) bool {
	aNaN, bNaN := math.IsNaN(a.Multiplier), math.IsNaN(b.Multiplier)
	if aNaN || bNaN {
		if aNaN && bNaN {
			return a.ID < b.ID // 两个都是 NaN：仍按 id 定序
		}
		return bNaN // 只有 b 是 NaN → a 排前
	}
	if a.Multiplier != b.Multiplier {
		return a.Multiplier > b.Multiplier
	}
	return a.ID < b.ID
}
