// provider_router.go 出口层的多上游路由：模型名前缀解析 + 按上游选号 + 目录合并。
//
// # 这个文件为什么存在
//
// 出口层（server）是唯一同时知道"客户端要什么"与"该派给谁"的地方：
// 只有它看得到请求体里的 model 字段。而"有哪些上游"是装配层的事实。
// 两者的接缝就是本文件的 ProviderRouter 接口。
//
// # 与 gateway 的分工
//
//	gateway  定义 Provider 契约与 SplitModel（纯字符串解析，无状态）
//	server   用它们做路由决策（前缀 → 上游 → 选号）
//	cmd/server 提供实现（把 Registry 与各 Provider 接进来）
//
// server 因此**不认识任何具体上游** —— 它只按 ID 问，加第 N 个上游时本文件零改动。
package server

import (
	"context"
	"encoding/json"

	"workbuddy2api/internal/gateway"
)

// ProviderRouter 出口层需要的多上游能力。
//
// 刻意只有四个方法，且**每一个都可失败**（返回 ok=false）：
// 拿不到上游目录是常态（账号没登录/凭证过期），不能让它变成 500。
type ProviderRouter interface {
	// Has 报告该 ID 是否是已注册的上游。
	//
	// 用途：把"未知前缀"与"上游暂时没账号"区分开 ——
	// 前者是客户端写错了，必须回 400 让它立刻知道；
	// 后者是服务端暂时没号，按 503 处理并走轮换。
	Has(id string) bool

	// Default 返回默认上游标识（裸模型名走它）。
	Default() string

	// Models 返回指定上游的模型目录。
	//
	// ok=false 表示这个上游现在给不出目录（没账号、拉取失败、未实现 CapModels），
	// 调用方应当**跳过它**而不是把整个 /v1/models 打成失败。
	Models(ctx context.Context, id string) ([]gateway.ModelInfo, bool)
}

// providerFor 解析请求的 model 字段，返回 (上游ID, 上游侧模型名, 错误)。
//
// # 路由规则（三种输入）
//
//	"workbuddy/auto"  → ("workbuddy", "auto")
//	"codearts/GLM-5.2"→ ("codearts", "GLM-5.2")
//	"auto"            → (默认上游, "auto")   ← 向后兼容，不能回归
//
// # 未知前缀为什么必须是错误而不是"当成裸模型名"
//
// 早先的实现（单上游时代）剥掉前缀就不管了：`codearts/GLM-5.2` 会被
// 当成"模型名叫 GLM-5.2"直接发给默认上游，表现为一个莫名其妙的上游 400。
// 用户看不出是自己写错了前缀。多上游之后这个前缀是**有语义的**，
// 写错必须立刻告知 —— 见 handler 里回 400 的分支。
//
// 注意第三个返回值是 (id, model, ok) 之外的**独立错误**：
// 只有当 hasPrefix 为真且前缀未注册时才非 nil。
func (h *Handler) providerFor(model string) (string, string, error) {
	id, m, hasPrefix := gateway.SplitModel(model)
	if !hasPrefix {
		// 裸模型名：走默认上游。这是既有客户端的路径，行为必须与改造前一致。
		return h.defaultProvider(), m, nil
	}
	if h.cfg.Provider == nil {
		// 单上游模式（未注入路由）：前缀只是记号，剥掉即可 ——
		// 这正是改造前 handler 的行为，保持它以不回归。
		return "", m, nil
	}
	if !h.cfg.Provider.Has(id) {
		return "", "", &unknownProviderError{id: id}
	}
	return id, m, nil
}

// defaultProvider 返回默认上游标识（显式注入优先，否则问路由）。
func (h *Handler) defaultProvider() string {
	if h.cfg.DefaultProvider != "" {
		return h.cfg.DefaultProvider
	}
	if h.cfg.Provider != nil {
		return h.cfg.Provider.Default()
	}
	return ""
}

// unknownProviderError 未知上游前缀。
//
// 做成具名类型而不是 errors.New 的字符串：handler 用它区分"路由错误(400)"
// 与"上游错误(503)"两类截然不同的响应，字符串比对太脆。
type unknownProviderError struct{ id string }

func (e *unknownProviderError) Error() string {
	return "未知的上游前缀 \"" + e.id + "\"：该上游未注册或未启用。" +
		"请检查模型名是否写成 \"provider/model\" 形式；" +
		"不带前缀时走默认上游。"
}

// hasPrefixIn 报告模型名里是否带 provider 前缀（"a/b" 形式，空前缀不算）。
func hasPrefixIn(model string) bool {
	_, _, ok := gateway.SplitModel(model)
	return ok
}

// rewriteModel 把出站请求体里的 model 字段改成裸模型名。
//
// # 为什么必须做
//
// 上游不认识网关的路由前缀。实测：带 "workbuddy/auto" 发过去，
// 上游按"未知模型"拒绝 —— 客户端会拿到一个与它写法相关的 503，
// 而且轮换还会把三个号的额度都白烧一遍。
//
// # 为什么不是简单字符串替换
//
// 用 JSON 解码再编码，保证不会误伤 body 里别处的同名子串
// （messages 内容里完全可能出现 "workbuddy/auto" 这几个字）。
// 代价是重新序列化会丢掉键顺序与空白 —— 因此**只在真的带前缀时**才重写，
// 不带前缀的请求（既有客户端的全部请求）保持原始字节原样转发。
//
// ok=false 表示 body 不是 JSON 对象或 model 字段不存在，
// 调用方应保留原始 body 而不是造一个空 body。
func rewriteModel(body []byte, model string) ([]byte, bool) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false
	}
	if _, ok := obj["model"]; !ok {
		return nil, false
	}
	obj["model"] = model
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// provider_router.go 的目录合并部分 ------------------------------------------

// modelEntry 一个模型在 /v1/models 里的一条记录。
//
// 用 map[string]any 而不是结构体，是为了与既有响应形状**逐字段一致** ——
// 既有的前端测试直接断言这些键名，改成结构体会静默改变 JSON。
type modelEntry = map[string]any

// prefixed 给一条模型记录加 provider 前缀（复制而不是原地改，
// 因为同一条记录可能同时以"带前缀"与"裸"两种形态出现）。
func prefixed(e modelEntry, provider string) modelEntry {
	out := make(modelEntry, len(e))
	for k, v := range e {
		out[k] = v
	}
	out["id"] = provider + "/" + asString(e["id"])
	return out
}

// asString 宽容地把 id 取成字符串（静态表里可能在测试里被替换成别的形态）。
func asString(v any) string {
	s, _ := v.(string)
	return s
}
