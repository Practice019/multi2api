package workbuddy

// errorclassifier.go —— workbuddy 通过 `gateway.ErrorClassifier` 自报
// 「我的错误体怎么分类」。
//
// # 为什么 workbuddy 也必须显式实现它
//
// 与 `RefreshSkewExt` 同一个理由（见 refreshskew.go）：只让 codearts 实现、
// workbuddy 不实现，功能上也能跑 —— 但那样**核心的通用兜底就成了
// workbuddy 的策略**，只是藏在 core 的代码里。
//
// 而 workbuddy 的分类判据里有两条**必须**属于它自己的事实：
//
//	hardMarkers        = ["insufficient credit", "no credit", "quota exceeded", ...]
//	sessionDeadMarkers = ["Offline user session not found", "12153"]
//
// 尤其是 `"12153"` —— 它是一个**裸数字**。把它留在核心，
// 就等于让每个上游的错误体都被这个 codearts 不认识的数字解释：
//
//	codearts 的错误体里只要碰巧含 "12153" → ErrSessionDead → Pool.Disable
//	→ **一个健康的账号被永久禁用**（需人工重登）
//
// 这正是本扩展点要修的第二条危害。把它搬进 workbuddy 的包之后，
// 它对 codearts 的响应体**再也不可见**（分派按上游走，见 handler.go）。
//
// # 为什么不把分类逻辑搬过来重写一份
//
// 因为真正的判据已经写在 `upstream.Classify` 里，且它是 workbuddy 的
// 既有、已测、生产验证过的事实。重写一份就是**发明第三套分类**（任务书
// 明确禁止）。本文件只做一件事：**把 upstream.ErrKind 逐项翻译成
// gateway.ErrorKind**，判据本身仍由 upstream.Classify 独家持有。
//
// 翻译表见 toGatewayKind —— 它是 `upstream.ErrKind` 到 `gateway.ErrorKind`
// 的**逐项直译**，两个枚举的取值顺序刻意对齐（见 gateway.ErrorKind 的注释
// 与 errorclassifier_test.go 的镜像测试）。

import (
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// 编译期断言（与 CredentialRefresher / CredentialLoader / AuthDirExt /
// RefreshSkewExt 并列）。
var _ gateway.ErrorClassifier = (*Provider)(nil)

// Classify 按 HTTP 状态码 + 响应体判定错误类别（gateway.ErrorClassifier）。
//
// # 分派链
//
//	handler.go 出站循环（非 2xx）
//	  → cfg.Provider.Classify(reqProvider, status, body)
//	  → 装配层按 id 取 Provider 实例
//	  → gateway.ExtOf[gateway.ErrorClassifier](pv)   ← 本方法
//	  → upstream.Classify(status, body)              ← 判据的唯一持有者
//	  → toGatewayKind(...)                           ← 类型翻译
//
// # 为什么内部仍然调 upstream.Classify（而不是把判据抄一遍）
//
// 本包是 workbuddy 的适配层，`upstream.Classify` 就是**本上游的分类器**
// （internal/upstream 是 workbuddy 的 HTTP 客户端实现所在）。
// 调用它不引入任何新耦合，也不改变任何判据 —— 只是把结果翻译成
// 跨上游中立的类型，让 core 的 applyErrorPolicy 能吃它。
//
// ⚠ **行为逐字不变**：本方法对任意 (status, body) 的返回值，
// 经 toGatewayKind 翻译后与改造前 core 直接调 upstream.Classify 的结果
// **完全一致**（有测试钉住，见 internal/server 的回归用例）。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	return toGatewayKind(upstream.Classify(status, body))
}

// toGatewayKind 把 upstream.ErrKind 翻译成跨上游中立的 gateway.ErrorKind。
//
// # 为什么是显式 switch 而不是 `gateway.ErrorKind(k)`
//
// 裸类型转换依赖"两个枚举的取值顺序永远一致"这个**隐含**约定 ——
// 任何一边插一个新常量，转换就会**静默错位**（例如 ErrSessionDead 变成
// ErrHardCredit），而那是比崩溃更难查的一类 bug。
//
// 显式列举之后，新增常量时这个 switch 会**落到 default**，
// 由测试（TestToGatewayKindCoversEveryUpstreamKind）立刻报红。
//
// # 翻译表（逐项，无信息损失）
//
//	upstream.ErrNone        → gateway.ErrKindNone
//	upstream.ErrHardCredit  → gateway.ErrKindHardCredit
//	upstream.ErrSoftRate    → gateway.ErrKindSoftRate
//	upstream.ErrSessionDead → gateway.ErrKindSessionDead
//	upstream.ErrNotFound    → gateway.ErrKindNotFound
//	upstream.ErrServer      → gateway.ErrKindServer
//	upstream.ErrClient      → gateway.ErrKindClient
//
// 默认分支返回 ErrKindNone **刻意不给任何上游特有的语义**：
// 一个未知的 ErrKind 只换号不惩罚（保守方向）。
// 把未知值映射成 ErrKindSessionDead 会让一次枚举错位演变成"批量永久禁用"。
func toGatewayKind(k upstream.ErrKind) gateway.ErrorKind {
	switch k {
	case upstream.ErrNone:
		return gateway.ErrKindNone
	case upstream.ErrHardCredit:
		return gateway.ErrKindHardCredit
	case upstream.ErrSoftRate:
		return gateway.ErrKindSoftRate
	case upstream.ErrSessionDead:
		return gateway.ErrKindSessionDead
	case upstream.ErrNotFound:
		return gateway.ErrKindNotFound
	case upstream.ErrServer:
		return gateway.ErrKindServer
	case upstream.ErrClient:
		return gateway.ErrKindClient
	default:
		return gateway.ErrKindNone
	}
}
