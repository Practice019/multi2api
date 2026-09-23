// Package gateway 定义多上游聚合的**唯一接缝**。
//
// # 这个包存在的理由
//
// 一个反代项目要能长期承载"任意多个上游"，必须有一条**强制**的接缝：
// 新上游只能通过这里定义的接口接入，而不能碰核心（账号池/日志/管理台/出口）。
//
// # 三条硬判据（本包是三者的载体）
//
//	判据 1  加一个新上游 = 加一个目录 + 实现本包接口 + 配置加一段；核心零改动
//	判据 2  新上游必须通过 RunProviderContract 才算合格
//	判据 3  架构约束由 arch_test.go 强制（核心不得依赖具体上游，反之亦然）
//
// # 为什么 Provider 只有 4 个方法
//
// 深模块原则：接口越小，实现越深，越不容易因为某个上游的怪癖被迫改接口。
// 账号生命周期（签到/成长/旅行/福利/额度）**刻意不进 Provider** ——
// 它们在各上游之间差异过大（见 extension.go 里的三个扩展点）。
//
// # 与上游的关系
//
//	核心（pool/admin/server）→ 只认 Provider 与 Credential
//	上游包（workbuddy/codearts/…）→ 实现 Provider，内部随意使用自己的 SDK
//
// 下游判据：如果 internal/pool 里出现了某个具体上游的名字，就是耦合漏了。
package gateway

import (
	"context"
	"io"
	"time"
)

// Provider 一个上游的**全部**对外契约。
//
// 实现者必须满足（由 RunProviderContract 强制检查）：
//   - ID() 非空且全局唯一
//   - 任何方法都不得 panic
//   - Chat 返回的 ChatStream.Body 必须能被调用方 Close（不泄漏连接）
//   - Caps() 里声明的能力必须真的可用
type Provider interface {
	// ID 上游的唯一标识，用于配置、统计、模型前缀。如 "workbuddy" / "codearts"。
	// 必须匹配 ^[a-z][a-z0-9-]*$（便于做模型名前缀），且不得与其它 Provider 重复。
	ID() string

	// Caps 声明该上游具备哪些能力。核心据此决定界面显隐与调度挂载。
	// **声明了就必须实现** —— 否则契约测试判不合格。
	Caps() Capability

	// Chat 发一次对话（流式）。
	//
	// body 是**已经过统一协议层处理**的请求体（OpenAI chat/completions 格式）。
	// 各上游的 header 拼法、签名（DPoP）、base URL、UA 差异**全部关在实现里**。
	//
	// status 非 2xx 时仍需返回 ChatStream（把上游的错误体带回来给调用方判断），
	// 只有网络层失败才返回 error。这样调用方能统一处理"业务错误码"与"传输错误"。
	Chat(ctx context.Context, cred Credential, body []byte) (ChatStream, error)

	// Models 返回该上游的模型目录（含上下文窗口等元信息）。
	// 用于 /v1/models 的合并与前缀标注。
	Models(ctx context.Context, cred Credential) ([]ModelInfo, error)
}

// ChatStream 一次流式响应的结果。
type ChatStream struct {
	// Status 上游返回的 HTTP 状态码（非 2xx 也算正常返回，调用方自行判断）。
	Status int
	// Body 原始 SSE 流。**调用方负责 Close**。
	//
	// 为什么不让 Provider 直接写 http.ResponseWriter：
	// 出口层要做统一协议转换（将来加 /v1/messages 时同一份流要编成不同格式），
	// 所以 Provider 只负责"拿到上游的流"，编码是出口层的事。
	Body io.ReadCloser
}

// ModelInfo 一个模型的元信息。
//
// 刻意保持最小：只有"跨上游真的有共识"的字段。
// 各上游特有的（价格倍率、能力标签、是否默认…）由各自在 AdminExt 端点里暴露。
type ModelInfo struct {
	ID string `json:"id"`
	// ContextWindow 上下文窗口（token 数）。未知时为 0。
	ContextWindow int `json:"context_window,omitempty"`
	// MaxOutputTokens 单次最大输出。未知时为 0。
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// Credential 一个账号的凭证。
//
// Provider/UID/Nickname/ExpiresAt 是**跨上游真的有共识**的字段，核心可以直接读。
// 各上游完全不同的凭证结构放 Secret，核心**不允许**直接读它。
type Credential struct {
	// Provider 该凭证属于哪个上游（与 Provider.ID() 对应）。
	Provider string `json:"provider"`
	// UID 上游账号的唯一标识，用于账号池去重与统计。
	UID string `json:"uid"`
	// Nickname 展示名（上游昵称/手机号/UIN，各上游不同）。
	Nickname string `json:"nickname,omitempty"`
	// ExpiresAt 凭证过期时刻。零值表示"上游不提供过期信息"。
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// FilePath 凭证在磁盘上的落点。"凭证文件在哪"是与 Nickname 同级的
	// **跨上游共识**的运维事实（核心不读内容，只消费位置本身）。
	//
	// 核心只拿它做两件事：账号池"文件"列展示、删除账号时连文件一起移除。
	// 空 = 上游的凭证不是文件形态 → 删除只能出池。
	//
	// ⚠ 不带上它的后果（用户实测报的"删除后重启复活"）：admin 的
	// accountDelete 拿不到 FilePath，purge_file 静默空转，文件还在 →
	// 下次启动并池又把账号装回来。装配层的投影必须把它透传进池子。
	FilePath string `json:"-"`

	// Secret 各上游**完全不同**的凭证结构，由各上游包自己定义并用类型断言读取。
	//
	// 为什么用 any 而不是统一结构体：
	//	workbuddy → {accessToken, refreshToken, uid, domain}
	//	codearts  → {AK, SK, SessionToken, RefreshToken, DPoP 私钥}
	// 强行统一会造出一个"什么字段都有、但每个上游只填三个"的超集结构体，
	// 那是比 any 更糟的设计（读者无法判断哪些字段对哪个上游有效）。
	//
	// **核心不得读 Secret。** 它只在"交给对应 Provider 使用"时被传递。
	Secret any `json:"-"`
}

// ExtOf 从 Provider 上取一个可选扩展点实现（AdminExt / JobExt / LoginFlow）。
//
// 扩展点用类型断言发现而不是写进 Provider 接口 —— 上游只实现自己有的。
// 这是"平台特殊功能解耦"的机制：
//
//	if ax, ok := gateway.ExtOf[gateway.AdminExt](p); ok { 挂载它的路由 }
//
// 泛型而不是为每个扩展点写一个 AsAdminExt()：加第四个扩展点时不用改这里。
func ExtOf[T any](p Provider) (T, bool) {
	var zero T
	if p == nil {
		return zero, false
	}
	t, ok := p.(T)
	if !ok {
		return zero, false
	}
	return t, true
}
