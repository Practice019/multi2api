// provider_alias.go 把上游 ID 换成对外代号，避免泄露后端身份。
//
// # 用户的要求
//
// 「owned_by 我不希望让别人知道上游」
//
// # 实际泄露的不止 owned_by —— 一共 5 处
//
// 用远端网关实测，客户端从公开接口能看到的上游身份：
//
//	1) /v1/models 的 owned_by        -> workbuddy / workbuddy-intl / csg / loomy
//	2) /v1/models 的 id 前缀          -> workbuddy-intl/deepseek-v4.1-flash
//	3) chat 响应的 model 字段          -> monkeycode-basic/deepseek-flash
//	4) chat 响应的 id 前缀             -> chatcmpl-... / 0217927502...
//	5) chat 响应的 usage 私有字段      -> points_consumed（loomy 独有）
//
// 3)4)5) 来自上游原始响应，是透传的产物。它们同样是指纹：
// 每档各调一次，就能按响应格式把 6 条通道分出组来。
//
// # 两个不同的东西，别混
//
//	upstream ID    内部路由键（workbuddy-intl）。必须真实，否则路由不了。
//	alias          对外代号（channel-a）。只出现在响应里。
//
// 映射是单向的：内部 -> 对外。请求侧不需要反解 —— 客户端发来的
// 已经是 alias，或者走 model_map 在更早的地方换成内部名。
package server

import "strings"

// aliasFor 返回某上游的对外代号。
//
// 未配置映射时原样返回上游 ID —— 这是刻意的向后兼容：
// 既有部署（不关心泄露）行为逐字节不变。
func (h *Handler) aliasFor(upstream string) string {
	id := strings.TrimSpace(upstream)
	if id == "" {
		return ""
	}
	if h.cfg.ProviderAlias == nil {
		return id
	}
	if alias, ok := h.cfg.ProviderAlias[id]; ok && strings.TrimSpace(alias) != "" {
		return strings.TrimSpace(alias)
	}
	// 配了映射表但漏了某个上游：不能回落成真名。
	//
	// 那会让「我已经隐藏了」这个假设在新增上游时静默失效 ——
	// 而失效的表现是别人的域名开始出现在响应里，很难发现。
	// 用固定前缀 + 稳定短哈希兜底：既中性，又不会每次启动都变
	//（变了的话客户端的模型索引会全部失效）。
	return "channel-" + shortAliasHash(id)
}

// shortAliasHash 给未配映射的上游生成稳定短代号。
//
// 用 FNV-1a 而不是 crypto 哈希：这里只需要稳定且够短，
// 不需要抗碰撞 —— 而 FNV 在 6 字符下足够区分几十个上游。
func shortAliasHash(s string) string {
	const hexdigits = "0123456789abcdef"
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	b := make([]byte, 6)
	for i := 0; i < 6; i++ {
		b[i] = hexdigits[h&0xf]
		h >>= 4
	}
	return string(b)
}
