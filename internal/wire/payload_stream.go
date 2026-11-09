package wire

import (
	"encoding/json"
	"log"
)

// NormalizeBodyForStreaming 是"轻量规范化层"的共享实现：**强制 stream + 收敛 max_tokens**。
//
// # 实测依据（T-335）
//
// lobsterai 与 raccoon 的 payload.go 各 50 行代码，逐行比过后**只差三处**：
//
//	① 上界常量（lobsterai 200_000 / raccoon 1_000_000）← 这是"抄错就出事"的地方
//	② 日志前缀（"lobsterai:" / "raccoon:"）
//	③ 各自的文档注释（如 lobsterai 的 reasoning_effort 透传说明）
//
// 收敛动作本身逐句相同：非数字 / 0 / 负数 / NaN ⇒ 删掉该字段（让上游用自己的默认值）；
// 超过上界 ⇒ 打日志并改成上界。**注意上界两家不同**，所以它是参数而不是常量。
//
// 背景：上游对超大 max_tokens 直接 4xx，而 DSH 可能注入一个别的 provider 带过来的大值
// （例如从 workbuddy 切过来时的 384000）。
func NormalizeBodyForStreaming(src []byte, maxTokensUpperBound int64, logPrefix string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src // 解析不了就原样返回（不猜）
	}
	obj["stream"] = true // 本层只服务 SSE 上游：stream:false 会让它们 500
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		f, ok := toJSONFloat(v)
		if !ok || f <= 0 || f != f { // 第三个条件滤掉 NaN
			delete(obj, key)
			continue
		}
		if int64(f) > maxTokensUpperBound {
			log.Printf("%s: max_tokens %d -> %d（上游上界）", logPrefix, int64(f), maxTokensUpperBound)
			obj[key] = maxTokensUpperBound
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// toJSONFloat 把 JSON 解出来的数字转成 float64。
func toJSONFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
