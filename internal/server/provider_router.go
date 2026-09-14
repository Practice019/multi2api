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
	"errors"
	"time"

	"workbuddy2api/internal/gateway"
)

// ProviderRouter 出口层需要的多上游能力。
//
// 刻意只有少数几个方法，且**每一个都可失败**（返回 ok=false）：
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

	// Chat 用**指定上游自己的 Provider** 发一次对话。
	//
	// # 为什么出口层不能自己拿 cfg.Upstream 发（本次修的真 bug）
	//
	// 出站循环原先恒用 `h.cfg.Upstream`（= workbuddy 的 upstream.Client）
	// 发**所有**请求，哪怕选中的是 codearts 的账号、请求的是 codearts 的模型。
	// workbuddy 的客户端把 codearts 的凭证按 workbuddy 的格式解读 ——
	// 必然失败，而且失败得很晚（一次完整的上游往返），还会惩罚那个无辜的账号。
	//
	// 出站层**不可能**自己选对实现：它只认 ID（不认识任何具体上游，见包注释），
	// 而「ID → 能发请求的那个实例」是装配层的事实。
	//
	// 返回形状直接复用 gateway.ChatStream（而不是像 cfg.Upstream.ChatStream
	// 那样返回四元组）：非 2xx 时上游的错误体就在 stream.Body 里，
	// 出口层按需 `io.ReadAll` 后交给 upstream.Classify。
	//
	// ok=false 表示该上游根本没接上（未注册 / 实例缺失）——
	// 出口层应当按「服务端暂时没号」处理（换号/503），而不是当成上游业务错误。
	Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error)

	// Credential 为该上游组装一份**带凭证**的 Credential。
	//
	// # 为什么"组装凭证"必须经过装配层
	//
	// 凭证的秘密部分存在账号池的**不透明 any 通道**里（pool.SecretOf），
	// 只有装配层知道它是哪个上游的什么类型 —— 出口层**不得读 Secret**。
	// 所以出口层只能问"给我一份 uid 对应的、该上游能用的凭证"。
	//
	// # 为什么需要 uid 参数
	//
	// 出站循环已经用 `Pool.PickFor(provider, model)` 选好了号，选号带模型额度
	// 加权与在途名额判断，**不能被绕过**。所以这里按 uid 取，
	// 而不是让装配层再选一次号（那会记录 lastUsed，打乱真实流量的选号分布，
	// 且与已经 Acquire 的名额对不上）。
	//
	// ok=false 表示取不到（账号不在池里 / 没有 secret / 该上游未注册）。
	// ⚠ 与 Models 用的 credentialFor 不同：那条路刻意用「任一个号」，
	// 这条路必须用**调用方指定的那个号**。
	Credential(id, uid string) (gateway.Credential, bool)

	// RefreshCredential 用**该上游自己的**刷新实现续期一份凭证。
	//
	// # 这是本次修复的第二半（A′ 扩展点在出口层的落点）
	//
	// 出站循环原先恒用 `cfg.Upstream.RefreshToken(acct)`（workbuddy 的实现），
	// 对任意上游的账号调用 —— codearts 的账号因此被报 "no refreshToken"
	// 并被标记失败冷却（实测 err_total +3/请求）。
	//
	// 分派本身不在这里做判断：装配层按 `gateway.ExtOf[gateway.CredentialRefresher]`
	// 问上游自己要实现，出口层只说"给这个上游的这个号续期"。
	//
	// ⚠ **没有实现该扩展点的上游不算失败** —— 那表示"它的凭证不需要刷新"。
	// 装配层应当对这种情况返回 ok=false 且 err=nil，出口层据此**跳过刷新**
	// 直接用凭证发请求（Caps 里没有"需要续期"这一位，缺实现就是不需要）。
	//
	// err 只用于诊断日志；出口层不读它做分类（刷新失败一律按"这个号现在
	// 用不了"处理：记错、换号），因为 codearts 的"需重登"与 workbuddy 的
	// session dead 属于**不同**的恢复路径（见各上游实现里的注释）。
	RefreshCredential(ctx context.Context, id string, cred gateway.Credential) (ok bool, err error)

	// Classify 用**该上游自己的**错误分类器判定 (status, body) 的错误类别。
	//
	// # 为什么分类也必须按上游分派（P2 缺口的落点）
	//
	// 出站循环原先在两条上游的响应上**都**调 `upstream.Classify`
	// —— 那是 workbuddy 的分类器，它的判据里有两条上游专有的事实：
	//
	//	hardMarkers        = ["insufficient credit", "no credit", "quota exceeded", ...]
	//	sessionDeadMarkers = ["Offline user session not found", "12153"]
	//
	// 拿它判 codearts 的响应体，两个方向同时出错：
	//
	//	① 额度漏判：codearts 的 "insufficient quota"（InferHub.4291.200）
	//	   不匹配任何一条 hardMarker → ErrNone → 额度耗尽被完全忽略。
	//
	//	② 反向误伤（后果最重）：codearts 的错误体只要含裸数字 "12153"
	//	   → ErrSessionDead → Pool.Disable（**永久禁用**，需人工重登）
	//	   → 一个健康的账号被永久打掉。
	//
	// # 为什么返回值是 gateway.ErrorKind 而不是任何上游的 ErrKind
	//
	// `upstream.ErrKind` 与 `codearts.ErrKind` 是**不同包的不同类型**，
	// core 不可能同时吃两者。所以出口层要的是一个**中立**类型 ——
	// 它由 gateway 提供（core 与上游都合法依赖 gateway，见其包注释），
	// 上游各自在自己的包里把它翻译出来。
	//
	// # ok=false 的语义
	//
	// 「该上游没有实现 gateway.ErrorClassifier」—— 调用方**必须**回落到
	// `upstream.Classify`（单上游模式的行为，逐字不变），而**不是**回落
	// 到某个通用猜测。原因见 handler.go 里 Classify 回落分支的注释：
	// 默认上游（workbuddy）的分类判据就住在 upstream.Classify 里，
	// 回落它是**正确**的，不是妥协。
	//
	// 与 Models 一样是"可能失败"的能力：拿不到就跳过，不当成上游业务错误。
	Classify(id string, status int, body string) (gateway.ErrorKind, bool)

	// RefreshSkew 问**该上游自己**"距过期不足多久就该提前续期"。
	//
	// # 为什么"要不要刷"必须由上游回答（这是出站修复的漏网）
	//
	// RefreshCredential 修好了"**怎么**刷"，但"**要不要**刷"原先留在核心：
	// 出站循环先做 `acct.NeedsRefresh(h.cfg.RefreshSkew)`（默认 10m），
	// 只有它为真才会走到上游的续期实现。于是核心用一个通用窗口替所有上游
	// 回答了"多早算该刷" —— 而这是**上游的事实**：
	//
	//	workbuddy → access token 寿命以小时计，10m 窗口合理
	//	codearts  → STS 凭证仅约 2h，它自己的窗口是 3m
	//
	// # 10m 对 codearts 的具体后果（不是"太晚"，是"太早"）
	//
	// 剩 8m 时核心判 true → 进续期分支 → 而 codearts 的 CredentialRefresher
	// 内部**没有**自己的 skew 检查（只做断言+转发）→ 真的去消费那个
	// refresh_token —— 它是**一次性**的（用一次即作废）。
	// 为省一次 401 往返烧掉一个凭证，这正是"核心越俎代庖"最贵的形态。
	//
	// 所以判据必须和"怎么刷"待在同一个地方。核心只问，不做判断。
	//
	// # ok=false 的语义
	//
	// 「该上游没有上报窗口」——调用方回落到自己的通用兜底
	// （明确的保守值，而不是假装知道上游的寿命）。
	//
	// ⚠ 与「skew 返回 0」必须区分：0 是上游**明确声明**"不需要提前续期"，
	// 调用方必须尊重；ok=false 是"没有这个信息"，用兜底值是合理的。
	RefreshSkew(id string, cred gateway.Credential) (skew time.Duration, ok bool)

	// ResetAt 问**该上游自己**"额度耗尽的号什么时候能再用"（走 gateway.ResetPolicyExt）。
	//
	// # 为什么这个能力必须按上游分派（P2 设计缺口）
	//
	// 与 RefreshSkew 是同一个形状的缺口，但断在**类型**上而不是判断上：
	// server.Config.NextResetAt 早先是 `func() time.Time` —— 没有参数。
	// 签名不允许按上游分派，于是不管出错的号属于哪个上游，拿到的都是装配层
	// 注入的那**一个**答案（workbuddy 的次日 04:00）。
	//
	// 这正是 handler.go 注释里早就写对、但类型实现不了的那件事：
	// 「出口层只问'这个号什么时候能再用'，具体策略由上游定义」。
	//
	// # 与 RefreshSkew 的对照
	//
	//	RefreshSkew → 我的凭证多早算该刷   （凭证寿命决定）
	//	ResetAt     → 我额度耗尽的号何时能再用（上游排程决定）
	//
	// 两者都收整份 Credential：策略可能取决于凭证本身（套餐/窗口），
	// 而出口层**不得读** Credential.Secret。上游拿到整份凭证自己断言类型。
	//
	// # ok=false 的语义（⚠ 出口层依赖它）
	//
	// 「该上游没有上报恢复排程」—— 没有实现 gateway.ResetPolicyExt，
	// 或实现里明确答"我没有这个信息"。调用方回落到自己的**通用保守值**
	// （现在注入的兜底是 now+1h）。这是明确的兜底，不是"核心假装知道
	// 上游的排程"。
	//
	// codearts 就是这一类：它**没有**签到恢复机制，次日 04:00 对它毫无意义。
	// 把它强加上去会让号"明明已恢复却冷到次日凌晨"（白闲置近 24h）。
	ResetAt(id string, cred gateway.Credential) (until time.Time, ok bool)

	// SoftRateReset 问**该上游自己**"这次软限流有没有精确的重置时刻"
	// （走 gateway.SoftRateExt）。
	//
	// # 为什么它与 Classify 是两个问题
	//
	//	Classify       → 这属于哪一类（软限流？额度耗尽？）
	//	SoftRateReset  → 这个软限流有没有**精确到某模型某时刻**的答案
	//
	// 前者决定走哪条策略路径，后者决定那条路径要不要**收窄**。
	// 合并成一个方法会让"是不是软限流"与"软限流的范围/截止"耦合在一起，
	// 而它们的判据来源完全不同（业务码 vs 文案里的时间串）。
	//
	// # 调用时机
	//
	// 仅在该上游把 (status, body) 判成 gateway.ErrKindSoftRate 之后才被调用。
	// 对其它类别调它没有意义 —— 那些错误体里即便带了"将在 … 重置"
	// 也不是限流语义，收窄冷却会是错的。
	//
	// ok=false 表示"给不出精确时刻"（非模型级限流、文案里没有时间、
	// 或该上游没实现本扩展点）—— 调用方退回既有的账号级软冷却路径。
	SoftRateReset(id string, status int, body string) (resetAt time.Time, ok bool)
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

// CredentialRefresher 是 gateway.CredentialRefresher 的**包内别名**。
//
// 出口层只在这里用一次（出站循环的刷新分支），起别名的意义是让
// "这一步需要的是'谁来做续期'"这件事在 handler.go 里一眼可见，
// 而不需要读者去翻 gateway 包。类型断言本身仍用 gateway.ExtOf 完成。
type CredentialRefresher = gateway.CredentialRefresher

// RefreshSkewExt 是 gateway.RefreshSkewExt 的**包内别名**（同上）。
//
// 出口层不认识任何具体上游，只认识"谁能回答我'多早该刷'"这个**问题形状**。
type RefreshSkewExt = gateway.RefreshSkewExt

// ResetPolicyExt 是 gateway.ResetPolicyExt 的**包内别名**（同上）。
//
// 出口层不认识任何具体上游，只认识"谁能回答我'这个号什么时候能再用'"
// 这个**问题形状**。P2 修复的核心就是把这个问题从"无参回调"（答案唯一，
// 必然隐式绑定默认上游）换成"按 ID 问"（每个上游答自己的）。
type ResetPolicyExt = gateway.ResetPolicyExt

// errNoProviderCredential 是多上游模式下"取不到该账号凭证"的失败原因。
//
// 独立成变量（而不是就地 errors.New）是为了让日志里有稳定的可 grep 文案：
// 这一条出现就意味着装配层与账号池对不上（账号没进池 / 没有 secret），
// 是**接线问题**而不是上游问题，不该与上游的业务错误混在一起排查。
var errNoProviderCredential = errors.New("多上游模式下取不到该账号的凭证（账号不在池里或没有 secret）")

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
