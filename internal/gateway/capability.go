package gateway

import "strings"

// Capability 一个上游具备的能力集合（位标志）。
//
// 为什么用位标志而不是字符串切片或一堆 bool 方法：
//   - 编译期安全：拼错名字是编译错误，不是运行时不生效
//   - 组合自由：Caps() 可以按上游实际情况任意组合
//   - 加能力只改本文件一处，不改任何上游
//
// 为什么这些能力**不进** Provider 接口：
// 两个上游的账号能力差异过大 —— workbuddy 有成长中心/猫猫旅行，
// codearts 有福利领取/按模型配额。若做成接口方法，每个上游都要写一堆
// "不支持"的空实现，且加第 3 个上游又要改接口。
//
// 正确用法：上游在自己的包内实现这些能力（用各自的小接口），
// 在 Caps() 里如实声明；核心据此决定界面显隐与调度挂载。
type Capability uint32

const (
	// CapChat 具备对话能力。**所有上游都必须声明**（否则这个 Provider 没意义）。
	CapChat Capability = 1 << iota
	// CapModels 支持动态模型目录（Models() 返回非空）。
	CapModels
	// CapCheckin 有签到能力。
	CapCheckin
	// CapGrowth 有成长中心/任务体系（workbuddy 专属）。
	CapGrowth
	// CapTravel 有猫猫旅行（workbuddy 专属）。
	CapTravel
	// CapWelfare 有福利领取（codearts 专属）。
	CapWelfare
	// CapQuotaProbe 支持主动额度探测（而非被动从请求结果推断）。
	CapQuotaProbe
	// CapTasks 有"新手任务"体系（一次性任务，逐个完成即入账积分）。
	//
	// 与 CapGrowth 的区别：growth 是**持续/周期性**的成长中心（有每日进度、
	// 等级、补领）；tasks 是一组**有限的、做完就没有**的引导任务。
	// loomy 的 onboarding 就是后者（8 项共 10000 分）。
	CapTasks
	// CapInvite 有邀请码/兑换码体系（查激活状态、绑定邀请码、查自己生成的码）。
	CapInvite
)

// capNames 能力的可读名。加能力时必须同时加这里 —— 有测试守住这一点。
var capNames = map[Capability]string{
	CapChat:       "chat",
	CapModels:     "models",
	CapCheckin:    "checkin",
	CapGrowth:     "growth",
	CapTravel:     "travel",
	CapWelfare:    "welfare",
	CapQuotaProbe: "quota-probe",
	CapTasks:      "tasks",
	CapInvite:     "invite",
}

// String 返回能力的可读名（单个位）。用于错误信息与前端能力位下发。
func String(c Capability) string {
	if n, ok := capNames[c]; ok {
		return n
	}
	return "unknown"
}

// Names 返回该能力集合里所有已命名的能力（按位从低到高，顺序稳定）。
// 用于前端按能力位渲染入口。
func (c Capability) Names() []string {
	out := make([]string, 0, len(capNames))
	// 按定义顺序遍历，保证输出稳定（map 遍历顺序随机）
	for _, one := range []Capability{
		CapChat, CapModels, CapCheckin, CapGrowth, CapTravel, CapWelfare, CapQuotaProbe,
		CapTasks, CapInvite,
	} {
		if c&one != 0 {
			out = append(out, capNames[one])
		}
	}
	return out
}

// Has 报告是否具备某项能力。
func (c Capability) Has(one Capability) bool { return c&one != 0 }

// AllCapabilities 返回全部已定义的能力，供文档与测试遍历。
func AllCapabilities() []Capability {
	return []Capability{
		CapChat, CapModels, CapCheckin, CapGrowth, CapTravel, CapWelfare, CapQuotaProbe,
		CapTasks, CapInvite,
	}
}

// validProviderID 校验 Provider.ID() 的格式。
//
// 约束成 ^[a-z][a-z0-9-]*$ 的理由：ID 会作为模型名前缀出现在
// "workbuddy/auto" 这种形式里，必须保证不含 '/'、':'、空白等会破坏解析的字符。
func validProviderID(id string) bool {
	if id == "" {
		return false
	}
	if id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for i := 1; i < len(id); i++ {
		ch := id[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			continue
		}
		return false
	}
	return true
}

// SplitModel 把 "provider/model" 拆成两段。
//
// 返回 (providerID, model, true)；不含 '/' 时返回 ("", 整个字符串, false)，
// 由调用方决定用哪个默认 Provider。
//
// 边界（都有测试）：
//   - 空串 → ("", "", false)
//   - "auto" → ("", "auto", false)          裸名，走默认上游
//   - "workbuddy/auto" → ("workbuddy", "auto", true)
//   - "workbuddy/" → ("workbuddy", "", true) 空模型名留给上游报错，不在这里吞掉
//   - "/auto" → ("", "auto", false)          空前缀视为裸名
//   - "a/b/c" → ("a", "b/c", true)           只切第一个 '/'（模型名本身可能含 '/'）
func SplitModel(s string) (providerID, model string, hasPrefix bool) {
	if s == "" {
		return "", "", false
	}
	i := strings.IndexByte(s, '/')
	if i <= 0 {
		// 没有 '/'，或 '/' 在首位（空前缀）→ 视为裸名
		return "", s, false
	}
	return s[:i], s[i+1:], true
}
