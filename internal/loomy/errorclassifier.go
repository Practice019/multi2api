// errorclassifier.go Loomy 的错误分类与额度恢复排程。
//
// # 为什么每个上游都要有自己的分类器
//
// 见 gateway.ErrorClassifier 的包注释：曾经核心拿 workbuddy 的错误码表去判
// 所有上游的响应体，两个方向同时出错 —— 额度漏判（静默把额度烧到底）
// 与反向误伤（一个健康账号被一个碰巧出现的裸数字永久禁用）。
//
// 所以"我的错误长什么样"是**上游的事实**，必须由上游自己报。
//
// # Loomy 的错误形态（手册第 9/10 节）
//
//	{"code":"100002","desc":"缺少 token"}        ← 注意：HTTP **200**
//	登录已失效，请重新登录                        ← session 无效
//	You have insufficient credits to make ...     ← 积分耗尽
//	Model "xxx" is not supported on this endpoint ← 模型名不对
//	该模型暂未开放                                ← 404，生图模型被下架
//
// ⚠ 第一条和第二条印证了手册第 3 节的坑：**鉴权失败的响应是 200**。
// 所以本分类器**不能**只看状态码 —— 那会把"鉴权头用错了"和"session 没了"
// 都当成成功。正文判据必须先跑。
package loomy

import (
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 正文判据。全部按手册实录的**原文**匹配（大小写不敏感）。
//
// 判据要"只有自己真正认识才返回硬分类"（见 gateway.ErrorClassifier 的实现约束）：
// 拿不准时返回 ErrKindNone 比猜一个类别更安全。所以下面每一条都对应
// 手册里一句**实测见过**的原文，没有一条是"看起来像"。
const (
	// markerNoToken 鉴权头缺失/用错（HTTP 200 + code 100002）。
	markerNoToken = "缺少 token"
	// markerSessionDead session 无效 → 只能人工重登。
	markerSessionDead = "登录已失效"
	// markerNoCredits 积分耗尽（英文原文，手册第 9 节第 3 条）。
	markerNoCredits = "insufficient credits"
	// markerModelUnsupported 模型名不对（自造 ID 时的原文）。
	markerModelUnsupported = "is not supported on this endpoint"
	// markerModelClosed 生图模型被上游下架（404「该模型暂未开放」）。
	markerModelClosed = "该模型暂未开放"
)

// Classify 按 (状态码, 响应体) 判定错误类别。
//
// 顺序是硬约束：**先正文、后状态码**。
//
// 理由是 Loomy 的鉴权错误是 200 —— 先看状态码的话，`登录已失效` 会被
// 归成 ErrKindNone（"200 = 成功"），于是失效的号既不冷却也不禁用，
// 每轮都重新选中它、每轮都白跑一次往返。这正是分类器存在的意义。
func (p *Provider) Classify(status int, body string) gateway.ErrorKind {
	low := strings.ToLower(body)

	switch {
	case strings.Contains(body, markerSessionDead):
		// # 为什么这里必须是 SessionDead（永久禁用），而不是像 codearts 那样"只换号"
		//
		// codearts 的凭证失效映到 ErrKindAuth（换号不罚），因为它**能自动续期**
		// （refresh_token + 后台任务）。Loomy 没有任何续期手段：
		// 没有 refresh token、没有 TTL 字段、客户端也不刷新
		//（证据见 credential.go 的包注释）。
		//
		// 所以"这个号的 session 没了"是一件**只能由人来做**的事 ——
		// 继续把它留在轮换池里，只会让每一次请求都多消耗一个必然失败的往返，
		// 而且用户永远看不到"有个号需要重登"这个真正要做的事。
		// 这正是 ErrKindSessionDead 的语义（见 gateway 的常量注释）。
		return gateway.ErrKindSessionDead

	case strings.Contains(body, markerNoCredits):
		// 积分耗尽：日额度 5000 按自然日自动重置（手册第 4.3 节），
		// 配合 ResetAt 给出恢复时刻。
		return gateway.ErrKindHardCredit

	case strings.Contains(body, markerNoToken):
		// ⚠ 这一条是**我们自己**的缺陷，不是账号的问题：
		// 它只会在"该发 token: 的请求发了 Bearer"或反之时出现（见 client.go）。
		// 所以绝不能据此惩罚账号 —— 归到 ErrKindClient（只换号不罚）。
		return gateway.ErrKindClient

	case strings.Contains(body, markerModelClosed):
		// 404「该模型暂未开放」：上游自己下架的模型（两个生图模型）。
		// 与"模型名写错"不同 —— 名字是对的，是上游不开。
		return gateway.ErrKindNotFound

	case strings.Contains(low, markerModelUnsupported):
		// 模型名不对。同样是客户端问题，不该记在账号头上。
		return gateway.ErrKindClient
	}

	// 正文没有认识的东西 → 按状态码走通用兜底。
	//
	// 刻意**不**在这里发明关键词：兜底只映射 RFC 语义（402/429/404/5xx/4xx），
	// 那是唯一跨上游真的有共识的东西。
	return gateway.DefaultErrorKind(status, body)
}

// ResetAt 返回额度耗尽的号什么时候能再用。
//
// # 为什么 loomy **有**资格回答这个问题
//
// 与 codearts 不同（它的配额按滚动窗口算，不归它管，所以显式返回 ok=false）：
// Loomy 的日额度是**服务端按自然日重置**的，手册第 4.3 节给了一组流水实证：
//
//	09-11 09:38 dailyBalance=4950 → 09-12 03:04 dailyBalance=5000（跨天自动补满）
//	09-12 → 09-14 整天未启动客户端，再打开仍为满额 5000
//
// 中间没有任何领取动作，源码里也没有 claimDaily/dailyRefresh 之类的东西。
// 所以"下一个自然日是恢复时刻"是**有证据的**，不是猜的。
//
// # 边界取哪天：自然日边界在哪个时区？手册没有钉死
//
// 这是本函数唯一不确定的地方，如实说明：
//
//	· 上游是讯飞（中国），按 CST（+08:00）算自然日是合理推断；
//	· 手册那组流水的跨天观测点是 09-12 03:04 **UTC**（= 11:04 CST），
//	  它证明"跨天会补满"，但没有把边界精确到某个时刻。
//
// 取"下一个 CST 00:00"就够保守：真实边界若在 CST 00:00，我们正好；
// 若更早（例如 UTC 00:00 = CST 08:00），我们只是多闲置几小时 ——
// 方向是安全的（宁可晚一点恢复，也不要反复撞"积分耗尽"）。
//
// 与 prompt.Gate 的 nextMidnightCST 同口径：用 FixedZone 而不是 now.Location()，
// 因为宿主时区是不确定的（Docker 默认 UTC、Windows 本地时区），
// 而这里的语义是"跟随上游的自然日"，与宿主无关。
func (p *Provider) ResetAt(cred gateway.Credential) (time.Time, bool) {
	// 本上游的恢复排程与具体凭证无关（所有账号共用同一套按日额度），
	// 但接口要求收凭证 —— 保持与其它两个同类扩展点同形状。
	_ = cred
	return nextMidnightCST(time.Now()), true
}

// cstZone 上游使用的时区（固定 +08:00，与宿主 TZ 解耦）。
var cstZone = time.FixedZone("CST", 8*60*60)

// nextMidnightCST 返回 now 之后最近的 CST 00:00。
//
// 边界语义（与 internal/prompt 的同名逻辑一致）：
//
//	23:59:59 → 几秒后的次日 00:00
//	00:00:00 → **次日** 00:00（刚过零点，下一个零点是明天）
func nextMidnightCST(now time.Time) time.Time {
	y, m, d := now.In(cstZone).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cstZone)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
