package codearts

// resetpolicy.go —— codearts 对 `gateway.ResetPolicyExt` 的回答。
//
// # 为什么 codearts 的回答是 ok=false
//
// 出口层在 ErrHardCredit 上要问"这个号什么时候能再用"。改造前它无从按上游问
// （回调没有参数），于是拿到的是**装配层注入的那一个**时刻 ——
// workbuddy 的"次日 04:00"。
//
// 而 04:00 是 workbuddy 的**签到恢复**前置时刻（签到时点 09:00/21:00）。
// codearts 没有签到，也没有任何"每天某个整点恢复额度"的排程：
//
//	它不声明 CapCheckin（见 provider.go 的 Caps 注释）
//	它的每日动作是「领取福利」（welfare），与"额度重置"不是同一件事
//	它的配额是按窗口滚动的服务端行为，不产生一个本地可算的时刻
//
// 把一个语义完全不同的时刻强加上去，两个方向都是错的：
//
//	codearts 明明已恢复却还冷到次日凌晨 → 白白闲置近 24h
//	按 04:00 解冻而实际未恢复          → 又撞一次硬错误
//
// 所以这里如实回答 **ok=false**：「我没有这个信息」。
// 出口层据此回落到自己的通用保守值（now+1h）——
// 那正是"通用保守值"存在的意义：明确的兜底，而不是替上游编一个排程。
//
// # 为什么是"不实现"的另一种写法
//
// 完全可以**不**实现本方法，让 `gateway.ExtOf[gateway.ResetPolicyExt]` 落空 ——
// 效果与 ok=false 相同（装配层两条路径都返回 ok=false）。
//
// 之所以显式写出来，与 workbuddy 必须显式实现 ErrorClassifier 是同一个理由：
// **让"codearts 没有额度恢复排程"这件事在它自己的包里有据可查**。
// 缺一个文件只能靠"翻遍本包找不到"来推断，而那是审计脚本要猜的东西；
// 一个带注释的显式回答是一份可读的事实声明。
//
// ⚠ 本方法**不返回任何时刻**：即便返回 now+1h 也是错的 ——
// 通用兜底是**核心**的取舍（它知道全局的保守程度），不是上游的事实。
// 上游把自己的猜测伪装成事实，就回到了本扩展点要修的那类错误。

import (
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言（与 RefreshSkew / CredentialRefresher / ErrorClassifier 并列）。
var _ gateway.ResetPolicyExt = (*Provider)(nil)

// ResetAt 如实报告 codearts **没有**额度恢复排程。
//
// 返回值恒为 (time.Time{}, false)：
//
//	until 零值 —— 调用方**不得**读它（ok=false 时无意义）
//	ok    false —— "我没有这个信息"，核心回落通用保守值 now+1h
//
// ⚠ 这是一个**稳定**的回答，不是"暂时不知道"：
// 只要 codearts 仍然不声明 CapCheckin（没有每日签到/额度重置端点），
// 它就该是这个值。哪天它有了真正的配额窗口重置时刻，在**这里**返回它。
func (p *Provider) ResetAt(_ gateway.Credential) (time.Time, bool) {
	return time.Time{}, false
}
