// response_scrub.go 按 API Key 的别名回显响应，不暴露上游。
//
// # 用户的要求（两次澄清后的最终口径）
//
// 第一次：「owned_by 我不希望让别人知道上游」
// 第二次：「我的想法是使用 ApiKey 的时候不能暴露，因为我有可能会设置
//          对外的别名。设置了就按照别名来处理，不能暴露别的」
// 第三次：「但是如果是对话密钥（调 /v1 接口，api_key）就不用这个问题，
//          按照原来的上游即可。只是 ApiKey 需要」
//
// 所以规则是**按钥匙身份分**，不是全局开关：
//
//	管理密钥 config.api_key   → 不脱敏，响应保持上游原名（运维/自用）
//	普通 API Key（配了别名）  → 按别名回显，抹掉上游指纹
//	普通 API Key（没配别名）  → 不脱敏（没有别名就无从"按别名处理"）
//
// # 为什么"没配别名就不脱敏"
//
// 脱敏的本质是"用别名替换真名"。没有别名时替换成什么？
// 只能编一个 —— 那会让客户端拿到的 model 与它请求的完全不同，
// 反而制造困惑。所以这条规则是"按别名处理"，不是"一律改名"。
//
// # 抹哪些（配了别名时）
//
//	model            回填客户端请求时用的名字（别名）
//	id               上游前缀是指纹（chatcmpl- / 0217927502...）
//	usage 私有字段    points_consumed / credit（loomy、workbuddy 独有）
//
// 标准 usage 字段（prompt_tokens 等）一律保留 —— 删错会让计费失准，
// 那比泄露更严重。
package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// privateUsageKeys 上游私有的 usage 计费字段（非 OpenAI 标准）。
//
// 只列实测确认存在的那些，不做"猜测式删除"。
var privateUsageKeys = map[string]bool{
	"points_consumed":  true, // loomy
	"credit":           true, // workbuddy 系
	"credits_consumed": true, // 兼容写法
}

// scrubEnabledForRequest 报告本次请求要不要脱敏。
//
// 判据只有一条：**这把钥匙配了别名映射**。
//
// 管理密钥（config.api_key）在鉴权阶段就不写 apikeyCtxKey，
// 所以 modelMapForRequest 返回 nil ⇒ 这里返回 false ⇒ 响应保持原样。
// 这正好就是用户要的「对话密钥不用管」。
func (h *Handler) scrubEnabledForRequest(r *http.Request) bool {
	if h.cfg.APIKeys == nil {
		return false
	}
	m := h.modelMapForRequest(r)
	return len(m) > 0
}

// scrubChatResponse 按别名回显一条 chat 响应。
//
// publicModel 是客户端请求时用的名字（别名）。回填它而不是上游原名：
// 客户端认的是自己发出去的那个名字。
//
// 解析失败时**原样返回** —— 脱敏是增强，不能因为响应畸形就吞掉它
//（畸形响应必须原样到客户端，否则排障时看不到真相）。
func scrubChatResponse(raw []byte, publicModel string) []byte {
	if len(raw) == 0 {
		return raw
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	if !scrubObject(obj, publicModel) {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// scrubObject 就地对一个响应对象脱敏，返回"是否改动过"。
func scrubObject(obj map[string]any, publicModel string) bool {
	changed := false
	// model 字段：回填客户端请求时用的名字。
	if _, ok := obj["model"]; ok && strings.TrimSpace(publicModel) != "" {
		if s, _ := obj["model"].(string); s != publicModel {
			obj["model"] = publicModel
			changed = true
		}
	}
	// id 字段：上游的 id 前缀是强指纹（chatcmpl- / 0217927502...）。
	//
	// 换成网关自己生成的稳定值：保留"这次响应的唯一性"，去掉"哪家上游"。
	// 用原 id 的哈希而不是随机数 —— 同一个 id 映射到同一个值，
	// 客户端做幂等/去重时行为一致。
	if old, ok := obj["id"].(string); ok && old != "" && !strings.HasPrefix(old, "chatcmpl-gw-") {
		obj["id"] = "chatcmpl-gw-" + shortAliasHash(old)
		changed = true
	}
	// usage 私有字段。
	if u, ok := obj["usage"].(map[string]any); ok {
		for k := range privateUsageKeys {
			if _, has := u[k]; has {
				delete(u, k)
				changed = true
			}
		}
	}
	return changed
}

// scrubChatMap 是 scrubChatResponse 的 **map 形态**：非流式出口拿到的已经是
// 解析好的 map，再序列化回去等于白做一次编解码。
//
// 两个入口共用 scrubObject —— 判据只写一份，不会出现"流式脱敏了、
// 非流式漏了"这种分叉（第一版就是只有流式那一路生效）。
func scrubChatMap(obj map[string]any, publicModel string) map[string]any {
	if obj == nil {
		return obj
	}
	scrubObject(obj, publicModel)
	return obj
}