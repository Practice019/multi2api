package gateway

import (
	"encoding/json"
	"errors"
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
// 两种形状都认（与 workbuddy / loomy 既有导入同一判据）：
//
//	[{...}, {...}]   数组（多条）
//	{...}            单个对象
//
// 返回**原文切片**而不是解析后的结构：上游的凭证字段各不相同，只有它自己
// 知道怎么解（这正是 `AccountImportExt` 存在的理由）。
//
// ⚠ 空输入 / 空数组 / 非 JSON 都返回**带原因的 error** —— 由调用方转成
// 界面提示。静默返回空切片会让用户看到"导入了 0 个"而不知道哪里错了。
func SplitAccountImportItems(pasted string) ([]string, error) {
	trim := strings.TrimSpace(pasted)
	if trim == "" {
		return nil, errors.New("请求体为空 —— 请粘贴账号 JSON（单个对象，或 [ ] 包起来的数组）")
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
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, string(it))
		}
		return out, nil
	default:
		// 先确认它至少是个 JSON 对象 —— 这样"粘了半截文本"能在**入口**
		// 就被拒，而不是落到上游的 ParseCredential 里报一句形状错误。
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trim), &probe); err != nil {
			return nil, errors.New("不是合法的 JSON 对象: " + err.Error())
		}
		return []string{trim}, nil
	}
}
