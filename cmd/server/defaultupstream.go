// defaultupstream.go 把 workbuddy 的 upstream.Client 适配成 server 需要的窄接口。
//
// # 这个文件是干什么的（以及为什么它必须存在）
//
// `internal/server` 是核心出口层，**不得**依赖任何具体上游
//（arch_test 判据 3）。但它确实需要默认上游提供四件事：
//
//	拉模型列表 / 拉模型目录（含倍率）/ 续期凭证 / 发对话流
//
// 于是核心只声明一个**窄接口**（`server.DefaultUpstream`），
// 由装配层把具体实现接上去 —— 与 poolAdapter / workbuddyLogin 同一手法。
//
// 这里同时是**两处投影**的落点，两处都从核心搬出来的：
//
//  1. `FetchModels`：`[]upstream.ModelInfo` → `[]server.DefaultModel`
//     （上游用 int64 存 token 数，核心的 DefaultModel 也保留 int64）
//  2. `FetchModelCatalog`：`*upstream.ModelCatalog` → `*gateway.ModelCatalog`
//     （workbuddy /v3/config 的 schema → 中立目录类型）
//
// # 为什么投影放这里而不是留在核心
//
// 只要函数的签名里出现 `*upstream.ModelCatalog`，核心就依赖了那个上游 ——
// 无论函数体多"中立"。投影本身一个字段都没变，只是搬到了唯一
// 能同时认识两边的位置。
package main

import (
	"errors"
	"io"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/upstream"
)

// defaultUpstream 把 *upstream.Client 适配成 server.DefaultUpstream。
type defaultUpstream struct {
	c *upstream.Client
}

// 编译期断言：适配器确实满足核心的接口。
var _ server.DefaultUpstream = defaultUpstream{}

// FetchModels 拉动态模型列表并投影成中立形状。
//
// 只搬核心真正读的三个字段（ID / ContextWindow / MaxTokens）。
// `Name` 与 `Efforts` **刻意不搬** —— 核心从不读它们
//（模型名直接用 ID；思考档位由出站改写层处理）。
func (a defaultUpstream) FetchModels(acc *auth.Auth) ([]server.DefaultModel, error) {
	infos, err := a.c.FetchModels(acc)
	if err != nil {
		return nil, err
	}
	out := make([]server.DefaultModel, 0, len(infos))
	for _, m := range infos {
		out = append(out, server.DefaultModel{
			ID:            m.ID,
			ContextWindow: m.ContextWindow,
			MaxTokens:     m.MaxTokens,
		})
	}
	return out, nil
}

// FetchModelCatalog 拉模型目录并投影成中立类型。
//
// # 这段投影是**逐字段原样搬来的**（原在 server.toGatewayCatalog）
//
// 唯一值得说的是 `MultiplierKnown` 的填法：
//
//	MultiplierKnown: m.CreditsRaw != ""
//
// 语义是"上游给了非空 CreditsRaw 就说明它表态过"—— 哪怕系数解析成 0，
// 那是"免费"而不是"未知"。这与 admin 的 modelsPreview「保留 0 倍率」
// 一致：x0.00 是"免费"这个有意义的事实。
//
// 漏掉这一行不会编译失败，而表现是"免费模型显示成未知倍率"。
func (a defaultUpstream) FetchModelCatalog(acc *auth.Auth) (*gateway.ModelCatalog, error) {
	src, err := a.c.FetchModelCatalog(acc)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, nil
	}
	out := &gateway.ModelCatalog{Models: make([]gateway.ModelCatalogEntry, 0, len(src.Models))}
	for _, m := range src.Models {
		out.Models = append(out.Models, gateway.ModelCatalogEntry{
			ID:                m.ID,
			Name:              m.Name,
			Vendor:            m.Vendor,
			Tags:              m.Tags,
			CreditsRaw:        m.CreditsRaw,
			Multiplier:        m.Multiplier,
			MultiplierKnown:   m.CreditsRaw != "",
			MaxInputTokens:    m.MaxInputTokens,
			SupportsToolCall:  m.SupportsToolCall,
			SupportsImages:    m.SupportsImages,
			SupportsReasoning: m.SupportsReasoning,
		})
	}
	return out, nil
}

// RefreshToken 续期并**把错误包装成核心的中立类型**。
//
// # 为什么必须包装（这是本适配器最要紧的一处）
//
// 核心要区分两种续期失败：
//
//	session 已死  → 走计数门控（连续 N 次才禁用），避免并发竞态误杀
//	其它失败      → 只记一次错
//
// 判据原先写的是 `errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead` ——
// 直接看 workbuddy 的错误类型。现在核心只认 `*server.RefreshError`，
// 所以**必须在这里把判据翻译过去**。
//
// ⚠ 漏掉这个包装的后果不是编译错误，而是**静默的行为降级**：
// 所有续期失败都走 else 分支（只记错、不禁用），
// 于是"凭证已死的账号"永远留在池子里，每次请求都白撞一次失败往返。
func (a defaultUpstream) RefreshToken(acc *auth.Auth) error {
	err := a.c.RefreshToken(acc)
	if err == nil {
		return nil
	}
	re := &server.RefreshError{Msg: err.Error(), Err: err}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		re.SessionDead = true
	}
	return re
}

// ChatStreamWithIP 发一次对话流（签名与上游逐字一致，直接转发）。
func (a defaultUpstream) ChatStreamWithIP(acc *auth.Auth, body []byte, clientIP string) (io.ReadCloser, int, []byte, error) {
	return a.c.ChatStreamWithIP(acc, body, clientIP)
}
