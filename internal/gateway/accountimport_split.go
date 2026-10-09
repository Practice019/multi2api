package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// SplitAccountImportItems 把用户粘贴的一段文本拆成**逐条**的 JSON 原文。
//
// # 为什么放在 gateway 而不是让每个上游各写一遍
//
// "认数组还是单个对象"是**用户输入的形态**，与任何上游的凭证结构无关 ——
// 8 个上游各抄一遍这段 switch 只会让其中几处慢慢写歪（本项目反复吃这个亏：
// 同一个判据抄多份，只有一份被改对）。
//
// 三种形状都认：
//
//	[{...}, {...}]   数组（多条）
//	{...}            单个对象
//	{...}{...}       多个独立对象连在一起 ← 用户实测提的需求
//	{...},{...}      同上（逗号分隔）
//
// 返回**原文切片**而不是解析后的结构：上游的凭证字段各不相同，只有它自己
// 知道怎么解（这正是 `AccountImportExt` 存在的理由）。
//
// ⚠ 空输入 / 空数组 / 非 JSON 都返回**带原因的 error** —— 由调用方转成
// 界面提示。静默返回空切片会让用户看到"导入了 0 个"而不知道哪里错了。
func SplitAccountImportItems(pasted string) ([]string, error) {
	trim := strings.TrimSpace(pasted)
	if trim == "" {
		return nil, errors.New(
			"请求体为空 —— 请粘贴账号 JSON（单个对象、[ ] 数组，或把多个对象依次粘进来）")
	}
	switch trim[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal([]byte(trim), &items); err != nil {
			return nil, errors.New("不是合法的 JSON 数组: " + err.Error())
		}
		if len(items) == 0 {
			return nil, errors.New("数组里没有任何账号")
		}
		return rawItemsToStrings(items), nil
	default:
		// 先确认它至少是个 JSON 对象 —— 这样"粘了半截文本"能在**入口**
		// 就被拒，而不是落到上游的 ParseCredential 里报一句形状错误。
		var probe map[string]json.RawMessage
		objErr := json.Unmarshal([]byte(trim), &probe)
		if objErr == nil {
			return []string{trim}, nil
		}
		// 不是单个对象 —— 可能是**多个独立对象连在一起**（见下面那段）。
		if items, ok := splitConcatenatedObjects(trim); ok {
			return items, nil
		}
		// 都不是：把**原始**的解析错误如实报出去（此时它才是真正的原因）。
		return nil, errors.New("不是合法的 JSON 对象: " + objErr.Error())
	}
}

// splitConcatenatedObjects 把"多个独立 JSON 对象连在一起"拆成逐条原文。
//
// # 为什么需要它（用户实测提的需求）
//
// 用户原话："我通常导入的时候会导入多个独立的 JSON 文件，那这个时候它就不行了。
// 所以希望能够多加一个功能，就是如果它是多个独立的 JSON 文件，它可以自动
// 组合成一个完整的大的 JSON 文件，然后再导入。"
//
// 也就是他把 N 个文件的内容依次粘进同一个输入框，得到形如
//
//	{"auth":{…}}
//	{"auth":{…}}
//	{"auth":{…}}
//
// 的文本。而 `[{…},{…}]` 那种**本来就带方括号**的形状一直是支持的 ——
// 差的恰好就是这一种（用户手上是 N 个**独立文件**，不是一个大数组）。
//
// # 判据刻意保守：只在"确实拆得出多个对象"时动手
//
// 上游的输入不都是 JSON（zcode 认裸 API Key、`zai:` 前缀、`Bearer ` 前缀）。
// 所以这里**不**做"先包一层 [ ] 再看"那种宽松改写 —— 那会把
// `sk-abcdef` 变成 `["sk-abcdef"]`，让裸 Key 导入**静默改变含义**。
// 两种形态都要求起点是 `{`（或本身就是合法数组），裸 Key 根本进不来。
//
// # ⚠ 尾部残渣必须拒绝，不能"能读几条算几条"
//
// 流式读很容易写成"读到一个算一个、读不动就停" —— 那会让
// **2 个完好对象 + 1 个粘坏的** 变成"导入 2 个、第 3 个静默消失"。
// 本项目的判据是"静默丢数据比明确失败糟得多"，所以这里要求
// 输入被**完整消费**（只有 io.EOF 才是正常结束）。
func splitConcatenatedObjects(trim string) ([]string, bool) {
	// ① 逗号分隔：套上方括号后应当是**合法数组**（json.Valid 顺带保证没有残渣）。
	if strings.HasPrefix(trim, "{") {
		if wrapped := "[" + trim + "]"; json.Valid([]byte(wrapped)) {
			var items []json.RawMessage
			if err := json.Unmarshal([]byte(wrapped), &items); err == nil && len(items) > 1 {
				return rawItemsToStrings(items), true
			}
		}
	}
	// ② 空白分隔（每行一个文件的内容）：流式逐个读。
	dec := json.NewDecoder(strings.NewReader(trim))
	var items []json.RawMessage
	for {
		var v json.RawMessage
		err := dec.Decode(&v)
		if err == io.EOF {
			break // 正常结束：输入被完整消费
		}
		if err != nil {
			// 有残渣 / 语法错误 → 不猜（宁可让调用方报原始错误）。
			return nil, false
		}
		items = append(items, v)
	}
	if len(items) > 1 {
		return rawItemsToStrings(items), true
	}
	return nil, false
}

// rawItemsToStrings 把 RawMessage 切片转成字符串切片。
func rawItemsToStrings(items []json.RawMessage) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, string(it))
	}
	return out
}
