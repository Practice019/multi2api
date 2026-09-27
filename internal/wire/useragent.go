// useragent.go 出站 UA 的**按模型族分档**选择。
//
// # 为什么需要它（参照项目有、我们此前没有）
//
// 参照项目的 BuddyProduct 有 `userAgentByModelFamily` 表，注释写明了理由：
//
//	腾讯后台的「使用端」列按出站 UA 归因，故该值必须**含对应产品品牌字样**
//	（`WorkBuddy/...` 或 `CodeBuddyIDE/...`），否则账单显示为 `-`。
//
// 而国际版与国内版**共用同一后端协议、模型池分属不同产品线**：
//
//	国际版独有模型线（gpt- / gemini- / claude-） → 国际版客户端形态
//	国内系模型（glm- / hy / kimi- / minimax-）    → 国内客户端形态
//
// 实测同一账号下，走 `gpt-*` 与走 `glm-*` 时官方客户端形态并不一致，
// 后台按 UA 归因的「使用端」也随之不同。**仅用一个全局 UA 无法让两类
// 模型都归因正确** —— 账单里会有一类显示成 `-`。
//
// # 两个形态的逐字取值（参照 product.ts:70-71）
//
//	国际版  WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2
//	国内版  WorkBuddy/5.5.2 WorkBuddy/5.5.2    CLI/5.5.2
//
// ⚠ 差异只在**第二段**：`WorkBuddy AI` vs `WorkBuddy`。第一段与第三段相同。
// 这看起来像"笔误/重复"，但它是对官方客户端出站形态的**逐字复刻** ——
// 参照注释专门警告过："看起来重复所以要合并"是一个很容易犯的错。
package wire

import (
	"bytes"
	"encoding/json"
	"strings"
)

// UAModelFamilyRule 一条按模型族覆写 UA 的规则（先命中先返回）。
type UAModelFamilyRule struct {
	// Match 模型名前缀（如 "gpt-" / "glm-"）。
	Match string
	// UA 命中时使用的 UA。
	UA string
}

// PickUA 按模型名从分档表里选 UA；无命中时返回 fallback。
//
// # 判据是"前缀匹配 + 先命中先返回"（照搬参照实现）
//
// 顺序有意义：参照的表里 `hy` 排在 `hy3-x` 之类之前不构成问题，
// 但若将来加一条更宽的前缀（如 `g`），它的位置就会决定结果 ——
// 所以这里**保序**，不排序、不去重。
//
// ⚠ `model` 为空时直接返回 fallback：那是"不知道这次是什么模型"的情形
//（如非 chat 路径），此时不该猜一个分档。
func PickUA(rules []UAModelFamilyRule, model, fallback string) string {
	if model == "" {
		return fallback
	}
	for _, r := range rules {
		if r.Match == "" {
			continue
		}
		if strings.HasPrefix(model, r.Match) {
			return r.UA
		}
	}
	return fallback
}

// ModelOf 从请求体里取顶层 `model` 字段（取不到返回空串）。
//
// # 为什么放在这里而不是各上游各写一份
//
// "从 OpenAI 兼容请求体里读 model"是**线协议层**的常识，与是哪个上游无关。
// 已有两处需要它（upstream 的 UA 分档、qoderwasm 的模型 key），
// 各写一份必然漂移。
//
// # 为什么用流式解析而不是完整 Unmarshal，也不是字符串搜索
//
//   - **完整 Unmarshal**：请求体可能有几百 KB（含 base64 图片），
//     建整棵消息树只为读一个字符串，代价与收益不成比例。
//   - **字符串搜索**：`"model"` 完全可能出现在消息正文里
//     （用户问"model 字段是什么意思"），搜到的是错的。
//
// 所以用 `json.Decoder` 做**流式**解析：只读顶层键，遇到第一个 `model`
// 就停 —— 既不建树，也不会把正文里的同名字段当顶层。
func ModelOf(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "" // 顶层不是对象（数组/标量）→ 不是我们认的请求体
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := keyTok.(string)
		if !ok {
			return ""
		}
		if key == "model" {
			valTok, err := dec.Token()
			if err != nil {
				return ""
			}
			s, _ := valTok.(string)
			return s
		}
		// 跳过该键的值（嵌套容器也一并跳过）。
		if err := skipValue(dec); err != nil {
			return ""
		}
	}
	return ""
}

// skipValue 跳过一个 JSON 值（标量消费一次，容器读到配对的闭合符）。
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // 标量：已消费
	}
	if d != '{' && d != '[' {
		return nil
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if dd, ok := tok.(json.Delim); ok {
			switch dd {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
