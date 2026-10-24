// reasoning_normalize.go 出站前归一化 reasoning_effort。
//
// # 用户实测的缺陷 1
//
// /v1/models 声明六模型都支持 [off low high max]，而实际：
//
//	003 workbuddy      reasoning_effort=off  -> 上游 400
//	004 workbuddy-intl reasoning_effort=off  -> 上游 400
//	002 loomy          reasoning_effort=off  -> 200
//	005 monkeycode     reasoning_effort=off  -> 200
//
// 而传 none（不在声明列表里）反而 200 且无推理输出。
// 也就是说 workbuddy 系把 off 当非法值，真正表达关闭的是省略这个参数。
//
// # 400 的副作用比错误本身更坏
//
// 实测：每次 off 失败都让 2 个账号进冷却，于是**后续正常请求也跟着 503**。
// 一个参数取值错误污染整个账号池 —— 这是必须在出口层拦住的理由。
package server

import (
	"encoding/json"
)

// offValues 语义为「关闭推理」的取值。
//
// off 是我们对外声明的那个；none 是部分上游实际接受的。
// 两者对客户端都表示同一件事，出站时统一处理。
var offValues = map[string]bool{
	"off":  true,
	"none": true,
}

// normalizeReasoningEffortForUpstream 把 off/none 转成「省略该字段」。
//
// 为什么是删除而不是改成某个值：

//	实测 workbuddy 系不传 reasoning_effort 时**完全不思考**（thinking 为 0），
//	而 none 也不在它接受的字面量里。删除是唯一在所有上游都安全表达。
//	loomy / monkeycode 接受 off，但删除对它们同样等价于关闭 ——
//	所以统一删除，不按上游分支。
//
// 返回 changed=false 表示 body 无需改动（**不重新编码**）。
//
// 不重新编码很重要：既有客户端不带 reasoning_effort 时，
// body 必须逐字节原样转发（与改造前一致）。
func normalizeReasoningEffortForUpstream(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	changed := false
	for _, key := range []string{"reasoning_effort", "reasoningEffort"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		s, _ := v.(string)
		if offValues[s] {
			delete(obj, key)
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}
