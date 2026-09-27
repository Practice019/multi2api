// defaultupstream_test.go 测试用的 DefaultUpstream 适配器。
//
// # 为什么测试里需要它（而生产不需要）
//
// 生产环境由装配层（cmd/server/defaultupstream.go）把 `*upstream.Client`
// 适配成 `server.DefaultUpstream`。而本包**不得** import cmd/server
// （那是反向依赖），所以测试要有一份自己的适配器。
//
// # 测试可以 import upstream，生产代码不行
//
// arch_test 的判据看的是 `go list -deps` 的**非测试**依赖 ——
// 测试文件 import 某个上游不违反架构（测试本来就要构造具体实现）。
// 所以这里可以放心 import upstream，而 handler.go / logging.go 不行。
//
// # ⚠ 这份适配器与生产那份是**两份实现**，会不会漂移？
//
// 会，而且这是真实的维护负担。但它换来的是"核心零上游依赖"这条判据 ——
// 而漂移的风险由一处**共享的断言**兜住：两份适配器都声明
// `var _ server.DefaultUpstream = ...`，接口一改两边都会编译失败。
//
// 真正会漂移的是**投影的字段**（比如 MultiplierKnown 的填法）——
// 那由 models_catalog_wiring_test.go 的端到端用例覆盖：
// 它走真 FetchModelCatalog 路径断言倍率与 known 标志，
// 两个适配器都得填对才过。
package server

import (
	"errors"
	"io"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// upstreamAdapter 把 *upstream.Client 适配成 DefaultUpstream（测试用）。
type upstreamAdapter struct {
	c *upstream.Client
}

var _ DefaultUpstream = upstreamAdapter{}

// Client 暴露底层 client，供测试在构造后继续改字段。
//
// ⚠ 嵌**指针**而不是值：测试里大量出现"先构造、再改字段、然后交给 Config"：
//
//	up := newFakeUpstream(t, ...)
//	up.PromptMode = prompt.ModeCustom   ← 改的是 client
//	NewHandler(Config{Upstream: up})
//
// 适配器必须**透传**到那个 client（不是快照一份），否则测试改的是原对象、
// 发请求的却是副本 —— 表现是"配了不生效"。
//
// 这个方法只有测试用得上（生产代码不该在构造后再改 client 字段，
// 那是启动期的配置），所以它定义在测试文件里。
func (a upstreamAdapter) Client() *upstream.Client { return a.c }

func (a upstreamAdapter) FetchModels(acc *auth.Auth) ([]DefaultModel, error) {
	infos, err := a.c.FetchModels(acc)
	if err != nil {
		return nil, err
	}
	out := make([]DefaultModel, 0, len(infos))
	for _, m := range infos {
		out = append(out, DefaultModel{
			ID:            m.ID,
			ContextWindow: m.ContextWindow,
			MaxTokens:     m.MaxTokens,
		})
	}
	return out, nil
}

func (a upstreamAdapter) FetchModelCatalog(acc *auth.Auth) (*gateway.ModelCatalog, error) {
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

func (a upstreamAdapter) RefreshToken(acc *auth.Auth) error {
	err := a.c.RefreshToken(acc)
	if err == nil {
		return nil
	}
	re := &RefreshError{Msg: err.Error(), Err: err}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		re.SessionDead = true
	}
	return re
}

func (a upstreamAdapter) ChatStreamWithIP(acc *auth.Auth, body []byte, clientIP string) (io.ReadCloser, int, []byte, error) {
	return a.c.ChatStreamWithIP(acc, body, clientIP)
}

// wrapUpstream 把 *upstream.Client 包成 DefaultUpstream（测试入口）。
func wrapUpstream(c *upstream.Client) DefaultUpstream { return upstreamAdapter{c: c} }

// defaultClassifierOf 测试用的默认分类器（= workbuddy 的判据）。
//
// ⚠ 生产环境注入的是 `wb.Classify`（workbuddy.Provider 的方法）。
// 测试里不能 import workbuddy（那会让本包的测试依赖具体上游 ——
// 虽然架构判据不管测试，但会让"改 workbuddy 就红一片"），
// 所以这里直接包 upstream.Classify 的翻译。
//
// 判据与 workbuddy.toGatewayKind **必须一致** —— 由
// upstream_kind_mirror_test.go 的镜像测试钉住（它枚举两个枚举的每一档）。
func defaultClassifierOf() func(int, string) gateway.ErrorKind {
	return func(status int, body string) gateway.ErrorKind {
		return mirrorKind(upstream.Classify(status, body))
	}
}

// mirrorKind 是 upstream.ErrKind → gateway.ErrorKind 的逐项镜像（测试用）。
//
// ⚠ 它**故意**与生产代码里那份分开：生产的那份在 internal/workbuddy
//（`toGatewayKind`），本份只在测试里用。两者由
// TestUpstreamKindMirrorCoversEveryKind 一起钉住 —— 那条测试枚举
// upstream.ErrKind 的**每一个**取值，任一档漏映射就红。
func mirrorKind(k upstream.ErrKind) gateway.ErrorKind {
	switch k {
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
		return gateway.ErrKindContentBlocked
	default:
		return gateway.ErrKindNone
	}
}
