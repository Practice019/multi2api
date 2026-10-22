// models.go 模型表：协议归属 + 兜底目录。
//
// # 这张表为什么必须存在
//
// 上游的模型分属两种协议，**走错就是 404 `路由不存在`**（不是 403）。
// 实测：
//
//	qwen3.8-flash                 openai_responses
//	qwen3.5-plus                  anthropic
//	deepseek-flash                anthropic
//	deepseek-v4-flash             anthropic
//
// 而 `GET /users/models` 的返回里**带 interface_type 字段** —— 那才是权威。
// 所以本表只是**兜底**：远端拿不到目录时用它，避免"新装的号一个模型都没有"。
// 拿到远端目录后，interfaceOf 会优先信远端（见 remoteInterface 缓存）。
package monkeycode

import (
	"strings"
	"sync"
)

// remoteInterfaces 远端目录学到的 模型名 → 协议 映射。
//
// 由 Models() 填充（它每次都会拉远端），Chat() 读它做路由。
// 用 RWMutex 保护：拉目录与对话是并发发生的。
var (
	remoteMu         sync.RWMutex
	remoteInterfaces = map[string]string{}
)

// learnInterfaces 把远端目录里学到的协议归属记下来。
func learnInterfaces(list []ModelEntry) {
	if len(list) == 0 {
		return
	}
	remoteMu.Lock()
	defer remoteMu.Unlock()
	for _, e := range list {
		name := strings.TrimSpace(e.Model)
		iface := strings.TrimSpace(e.InterfaceType)
		if name == "" || iface == "" {
			continue
		}
		remoteInterfaces[name] = iface
	}
}

// interfaceOf 判断某个模型该走哪种协议。
//
// 顺序：① 远端学到的（权威）② 内置兜底表 ③ 猜 openai_responses。
//
// ⚠ 兜底猜 responses 而不是 anthropic：responses 是本上游的**主协议**
// （新模型优先上 responses），而且猜错的 404 会被归成 ErrKindNotFound
// （短冷却、不累计错误），不会禁号。
func interfaceOf(model string) string {
	name := strings.TrimSpace(model)
	if name == "" {
		return ifaceResponses
	}

	remoteMu.RLock()
	if iface, ok := remoteInterfaces[name]; ok {
		remoteMu.RUnlock()
		return normalizeInterface(iface)
	}
	remoteMu.RUnlock()

	for _, e := range fallbackModels() {
		if e.Model == name {
			return normalizeInterface(e.InterfaceType)
		}
	}
	return ifaceResponses
}

// normalizeInterface 归一接口名（远端可能给 `anthropic` / `openai_responses`）。
func normalizeInterface(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "anthropic", "messages":
		return ifaceAnthropic
	case "openai_responses", "responses", "openai-responses":
		return ifaceResponses
	}
	return ifaceResponses
}

// fallbackModels 兜底模型目录。
//
// # 取值的依据
//
// 全部来自国内站 GET /api/v1/users/models?limit=200 的实测结果（45 条）。
// 这里只收 basic 档、
// 且**句法上能确认可用**的那几条 —— 剩下的让远端目录补。
//
// # 为什么不全抄 45 条
//
// ① pro/ultra 对 basic 号一律 403，抄进来等于给用户画假按钮；
// ② 目录会变，抄死的清单必然滞后。兜底只需保证"新装时有东西看得见、能试"。
//
// # context_limit 的取值
//
// 上游目录里每条都带 context_limit / output_limit，这里用实测值；
// 未知的填 0（gateway.ModelInfo 的 0 = 未知，不是"窗口为0"）。
func fallbackModels() []ModelEntry {
	return []ModelEntry{
		{
			Model:         "monkeycode-basic/qwen3.8-flash",
			AccessLevel:   "basic",
			InterfaceType: ifaceResponses,
			ContextLimit:  262144,
			OutputLimit:   32768,
		},
		{
			Model:         "monkeycode-basic/qwen3.5-plus",
			AccessLevel:   "basic",
			InterfaceType: ifaceAnthropic,
			ContextLimit:  131072,
			OutputLimit:   16384,
		},
		{
			Model:         "monkeycode-basic/deepseek-flash",
			AccessLevel:   "basic",
			InterfaceType: ifaceAnthropic,
			ContextLimit:  131072,
			OutputLimit:   16384,
		},
		{
			Model:         "monkeycode-basic/deepseek-v4-flash",
			AccessLevel:   "basic",
			InterfaceType: ifaceAnthropic,
			IsHidden:      true,
			ContextLimit:  131072,
			OutputLimit:   16384,
		},
		{
			Model:         "monkeycode-basic/kimi-k2.5",
			AccessLevel:   "basic",
			InterfaceType: ifaceAnthropic,
			IsHidden:      true,
		},
		{
			Model:         "monkeycode-basic/glm-5.3-flash",
			AccessLevel:   "basic",
			InterfaceType: ifaceAnthropic,
		},
		{
			Model:         "monkeycode-basic/minimax-m2.5",
			AccessLevel:   "basic",
			InterfaceType: ifaceAnthropic,
		},
	}
}
