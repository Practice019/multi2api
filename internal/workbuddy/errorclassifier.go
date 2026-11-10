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
	"net/http"
	"strings"

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
	// ── 唯一一处**有意偏离** upstream.Classify（2026-11-10）────────────────────
	//
	// 403 + **完全空体** ⇒ ErrKindServer，而不是 upstream.Classify 给出的 ErrClient。
	//
	// # 为什么这处必须偏离
	//
	// upstream.Classify 的**每一条**判据都基于响应体里的 marker（hardMarkers /
	// sessionDeadMarkers / contentBlockedMarkers / fatalClientMarkers）—— 空体时全部不命中，
	// 于是一路落到「status >= 400 ⇒ ErrClient」的兜底。而 ErrClient 在 core 的处置是
	// **只换号、不记错**（applyErrorPolicy 的 default 分支，防的是"客户端一次发错参数").
	//
	// 但 403 的语义是 Forbidden —— 在 AI 上游里最常见的原因正是**这个账号被拒绝/被限制**，
	// 那是账号级、不自愈的状态，与"请求参数写错"（400）完全不同类。空体时
	// 请求侧的信息量为零，唯一可用的判据就是状态码本身，而 403 指向的是账号。
	//
	// # 实测证据（2026-11-10，55300 条请求日志）
	//
	// 同一上游、同一天、同为 403，只因响应体不同而落到两类：
	//
	//	codar     403 + {"detail":{"code":"account_restricted",...}} → server → 连续 5 次后摘号 ✅
	//	workbuddy 403 + （空体）                                          → client → **永不摘号** ❌
	//
	// 后果：workbuddy 有 2 个号**连续 8 天 0 成功**，却 cooling=false、disabled=false、
	// breaker_fails=0、last_err 是**零值**（= NoteError 从未被调用过）—— 在管理台上与
	// 一个刚导入的健康号看不出任何区别，于是每次选号都被选中、每次都以 503 收场：
	// 3.5 天里 76 次 503（占该上游 1.1%），每次白打 3 轮上游。
	//
	// # 为什么归 ErrKindServer（而不是新增一个类别）
	//
	// 它同时喂两条线：熔断计数（NoteError）与自动禁用线（RecordAutoDisableFailure），
	// 正是这里缺的两样。新增类别的收益是语义更准，但会牵动 errcode 映射与
	// applyErrorPolicy 两处枚举；在"先让死号能被摘掉"这一步不值得。若将来要区分
	// "账号被拒"与"上游故障"，再单独引入不迟。
	//
	// # 误伤边界（诚实记录）
	//
	// 边缘/WAF 如果对所有账号统一回 403 空体，每个号都会开始累计失败；
	// 缓解是既有的两条：① 阈值是**连续 5 次**（零散失败不触发）；
	// ② 被禁用的号由健康检查循环的 AutoDisabledCandidates() 主动探测，
	//   一旦成功即自动恢复。所以这是"短暂误禁 + 自动恢复"，不是永久损失。
	if status == http.StatusForbidden && strings.TrimSpace(body) == "" {
		return gateway.ErrKindServer
	}
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
	case upstream.ErrContentBlocked:
		// 内容策略拦截 —— core 据此触发**提示词降级重试**，而不是换号。
		//
		// ⚠ 这一条不能落 default：default 是 gateway.ErrKindNone，
		// 而 None 在 core 侧是"只换号不惩罚"。内容问题换号没有意义
		// （每个账号背后是同一套内容策略），换号只会白烧 MaxRotate 次往返，
		// 最后把"内容被拦"误报成"所有账号不可用"。
		return gateway.ErrKindContentBlocked
	case upstream.ErrFatalClient:
		// 请求超出模型上下文窗口 —— 必须显式翻译，**不能**落 default。
		//
		// ⚠ default 是 gateway.ErrKindNone，而 None 在 core 侧是"只换号不惩罚"。
		// 换号对上下文超限毫无用处（每个号收到的 body 一样长）⇒
		// 会白烧 MaxRotate 次往返，最后把"这次请求太长"误报成"所有账号不可用"。
		// 正确处置是**直接结束**（skip-retry），由 core 按这一档实现。
		return gateway.ErrKindFatalClient
	default:
		return gateway.ErrKindNone
	}
}
