// maxoutput.go 从模型名取「该模型的输出上限」，供 Anthropic 转换注入用。
//
// # 为什么需要它
//
// Anthropic 协议的 max_tokens 是必填，而 OpenAI 请求可以不传。填补时
// 需要一个值 —— 这个值应当是**模型真能输出的上限**，而不是随便一个小数。
//
// 用户的诉求：「我不希望去限制模型的输出上限或者其他什么，完全开放」。
//
// # 为什么不学 new-api 维护一张 DefaultMaxTokens 配置表
//
// new-api 那么做是因为它的模型目录里**没有** max_output_tokens（它从
// 各家上游的 /models 响应里拿，多数上游不报）。而本仓的目录本来就带
// 这个字段（/v1/models 里就有 max_output_tokens），再维护第二份数字
// 必然与目录漂移 —— 哪天模型上限变了，两份数据里错的总是配置那份。
//
// 所以这里只做「按模型名查目录」，查不到才用兜底值。
package anthroconv

import (
	"strings"
	"sync"
)

// OutputLimitLookup 按模型名返回输出上限（token）。0 表示未知。
//
// 由装配层注入：它会去问网关的模型目录。
//
// 为什么用函数而不是直接查目录：anthroconv 是**共享协议转换层**，
// 它不认识网关的模型缓存（那是出口层的设施）。用函数注入保住分层。
var OutputLimitLookup func(model string) int

// lookupCache 记住已查过的模型上限。
//
// 转换是**每请求**发生的，而模型上限几乎不变 —— 每请求穿透到目录层
// 是白花钱（目录层可能带锁）。缓存键是模型名，值 0 也缓存
// （表示「查过、确实未知」，避免反复回源）。
var lookupCache sync.Map // string -> int

// SetOutputLimitLookup 装配层注入查询函数（nil = 关闭，走兜底值）。
func SetOutputLimitLookup(fn func(model string) int) {
	OutputLimitLookup = fn
	lookupCache.Range(func(k, _ any) bool { lookupCache.Delete(k); return true })
}

// MaxOutputFor 返回该模型的输出上限（0 = 未知）。
//
// 带缓存：同一次进程生命周期内，一个模型只回源一次。
func MaxOutputFor(model string) int {
	model = strings.TrimSpace(model)
	if model == "" || OutputLimitLookup == nil {
		return 0
	}
	if v, ok := lookupCache.Load(model); ok {
		return v.(int)
	}
	n := OutputLimitLookup(model)
	lookupCache.Store(model, n)
	return n
}

// CacheSize 报告缓存条目数（测试与诊断用）。
func CacheSize() int {
	n := 0
	lookupCache.Range(func(_, _ any) bool { n++; return true })
	return n
}
