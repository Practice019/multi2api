// armor_detect.go 破甲**检测**层：判断一份出站提示词里有没有破甲痕迹。
//
// # 它解决什么（用户原话）
//
//	「禁用破甲 = 只拦截（让他自己去修改）」
//
// 也就是说：这一档**不注入**，而是对选定范围内的请求先看一眼提示词，
// 命中就**不发出**，直接回一句可读的错误。
//
// # 三条纪律（都是用户拍板的）
//
//	① 本地判据：零上游调用、零额外请求、热路径毫秒级 ⇒ 只用**字面量子串**匹配。
//	   不做正则：用户可填的正则进热路径等于开一个 ReDoS 面（见 design-notes 方案取舍 D）。
//	② 不管内容合规：色情/违法一律不检 —— 只认「破甲」这件事。
//	③ 不剥离、不改写：命中就拒绝，改写成什么由调用方自己决定（用户原话"让他自己去修改"）。
package prompt

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// defaultArmorPatterns 内置模式集 —— **只留高特征句**（2026-11-13 收窄，验收方点名）。
//
// # 为什么默认只留高特征句（误杀 vs 漏检，代价不对称）
//
// 漏检的代价是「这一条没被拦下」，而它本来就有别的手段兜：观察模式先量命中率、
// 用户自己往模式库里加词、拦截那档本来就要用户手动打开。
// 误杀的代价是**正常流量被拒**（禁用态直接 4xx）—— 而泛化词（单独一个「越狱」/「破甲」）
// 在正常提问里高频出现：
//
//	「什么是越狱」        ← 正常提问，却被「越狱」命中
//	「破甲是什么意思」      ← 同上
//	「developer mode 在 Chrome 里怎么开」 ← 工程语境里的同名短语
//
// 更要紧的是：这类噪音**连观察模式也救不了** —— 命中计数与日志一旦被正常流量淹没，
// 用户就没法用它校准（本来「先观察再拦截」的全部价值就是那个读数）。
// 所以默认集宁窄勿宽：宁可漏，不可吵。
//
// # 想加宽怎么办（不要往这里塞）
//
// 在设置页的「检测模式（追加）」里加（配置键 prompt_armor_patterns）—— 那些词是用户
// 自己看过、确认过的。常见示例（**故意不在默认集里**）：
//
//	越狱 / 破甲 / 越狱提示词 / 解除限制 / 不受任何限制 / 无限制模式 /
//	开发者模式 / developer mode / jailbreak / dan mode / no restrictions apply /
//	ignore previous instructions
//
// # 默认集里留下的两类
//
//	① 本仓破甲正文**自己的独有短语**（armor-payload.md 逐字副本里的句子与幂等标记）——
//	   它出现在别处，基本只能说明对方用的就是这份破甲；
//	② 明确指向「改写人格 / 绕过拒绝」的短语（如「越狱模式」/「破甲模式」）——
//	   它们本身就是那个动作的名字，不是这件事的泛称。
//
// ⚠ 全部小写（匹配时文本也小写，见 DetectArmor）—— 每次请求逐条 ToLower 是纯浪费。
// ⚠ 断言钉在 armor_detect_test.go 的 TestDefaultArmorPatternsRejectNormalQuestions：
//	「正常问句『什么是越狱』不得命中内置集」（收窄前它是红的）。
var defaultArmorPatterns = []string{
	// ① 本仓破甲正文自己的独有短语（armor-payload.md 逐字副本）
	//    幂等标记（带前缀 ⇒ 几乎不可能自然出现）
	"wb2api:armor",
	//    载荷里独有的人设/模式/指令句
	"little code sauce",
	"unrestricted sandbox",
	"clr_all_context",
	"blind devotion",
	// ② 明确指向「改写人格 / 绕过拒绝」这个**动作**的短语（各语言各留一个说法）
	"破甲模式",
	"越狱模式",
	"jailbreak mode",
	"you are now dan",
	"ignore all previous instructions",
}

// ArmorPatterns 解析出本次生效的模式集。
//
//	PatternsOnly=false（默认）  内置集 + 追加条目
//	PatternsOnly=true           只用追加条目（用户要"完全自己定"时能关掉默认集）
//
// 条目一律 trim + 小写；空条目丢掉 —— 一个空串会让匹配永远命中（把整个网关拦死）。
func ArmorPatterns(cfg ArmorConfig) []string {
	out := make([]string, 0, len(defaultArmorPatterns)+len(cfg.Patterns))
	if !cfg.PatternsOnly {
		out = append(out, defaultArmorPatterns...)
	}
	for _, p := range cfg.Patterns {
		if v := normalizePattern(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// normalizePattern 归一化一条模式：去首尾空白 + 折内部空白 + 小写。
//
// 折空白是为了让 "ignore  all   previous" 这种手写排版也能命中（配置是人工填的）。
func normalizePattern(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Detection 一次命中的**证据**。
//
// 为什么要 Pattern 与 Excerpt 两个字段：日志要能回答两件事 ——
// 「命中了哪条模式」（校准模式库）与「原文长什么样」（判断误杀）。
type Detection struct {
	// Pattern 命中的那一条模式（已归一化：小写 + 空白折叠）。
	Pattern string `json:"pattern"`
	// Excerpt 命中位置的上下文（归一化后的文本，前后各 40 字节，按 UTF-8 边界裁）。
	Excerpt string `json:"excerpt"`
}

// DetectArmor 在文本里找工作模式。
//
// 判据是**字面量子串**（不含正则）：这条路径在每个请求上跑，而用户可填的正则
// 等于开一个 ReDoS 面（见文件头纪律①）。
//
// 归一化对两端都做（小写 + 连续空白折成一个空格）：
//
//	文本  "You are Little   Code\nSauce"  ⇒ "you are little code sauce"
//	模式  "Little   Code Sauce"           ⇒ "little code sauce"
//
// 不做归一化的后果实测形态是「配的模式明明在提示词里，却没命中」——
// 而它看起来像"功能没生效"，排查成本极高。
//
// ⚠ 空文本 / 空模式集一律不命中：空模式集若"恒真"，启用观察模式会直接把网关拦死。
func DetectArmor(text string, patterns []string) (Detection, bool) {
	if strings.TrimSpace(text) == "" || len(patterns) == 0 {
		return Detection{}, false
	}
	norm := collapseSpaces(strings.ToLower(text))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		i := strings.Index(norm, p)
		if i < 0 {
			continue
		}
		return Detection{Pattern: p, Excerpt: excerptAround(norm, i, len(p))}, true
	}
	return Detection{}, false
}

// collapseSpaces 把连续空白折成一个空格（无连续空白时**原样返回，零分配**）。
func collapseSpaces(s string) string {
	if !strings.ContainsAny(s, "\t\n\r\f\v") && !strings.Contains(s, "  ") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			if !space {
				b.WriteByte(' ')
				space = true
			}
		default:
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// excerptAround 取命中位置两侧的上下文。
//
// ⚠ 按 UTF-8 边界裁：中文提示词按字节切会把一个汉字切成两半，产生 \ufffd 乱码
// （本仓已踩过这个坑，见 CHANGELOG 的「UTF-8 截断」条目）。
func excerptAround(s string, at, n int) string {
	const pad = 40
	start, end := at-pad, at+n+pad
	if start < 0 {
		start = 0
	}
	if end > len(s) {
		end = len(s)
	}
	for start > 0 && start < len(s) && !utf8.RuneStart(s[start]) {
		start--
	}
	for end > 0 && end < len(s) && !utf8.RuneStart(s[end]) {
		end--
	}
	return strings.TrimSpace(s[start:end])
}

// ArmorBodyText 从请求体里取出**提示词文本**（检测的输入）。
//
// # 为什么不是直接扫整个 body 的原始字节
//
// 可解释性：扫原始 JSON 会把字段名也当成命中（例如某个模型名里恰好含某个词），
// 日志里给出一个"用户根本没写过的位置"，校准误差率时会被这类噪音带偏。
//
// 口径（与 ApplyArmorToBody / SystemText 同一族，都是"取 messages 里的文本"）：
//
//	messages[].content       字符串，或分片数组里 type=text 的分片
//	顶层 prompt / input / system   非 messages 形状的调用方（老接口与简化调用）
//
// 返回的第二个值 structured 报告"这份文本是不是按结构化口径取出来的"：
// JSON 解析失败时返回 (原始字节, false) —— **文本仍要交出来**，
// 否则一个非法 JSON 的 body 就能绕过检测（那正是最该被看见的一种请求）。
func ArmorBodyText(body []byte) (string, bool) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return string(body), false
	}
	parts := make([]string, 0, 8)
	if msgs, ok := obj["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			parts = appendContentText(parts, mm["content"])
		}
		return strings.Join(parts, "\n"), true
	}
	for _, k := range []string{"prompt", "input", "system"} {
		if s, ok := obj[k].(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n"), true
}

// appendContentText 取一条 message 的文本内容（字符串 / 分片数组里的 text 分片）。
func appendContentText(parts []string, c any) []string {
	switch v := c.(type) {
	case string:
		return append(parts, v)
	case []any:
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["type"].(string); t != "text" {
				continue
			}
			if s, ok := pm["text"].(string); ok {
				parts = append(parts, s)
			}
		}
	}
	return parts
}
