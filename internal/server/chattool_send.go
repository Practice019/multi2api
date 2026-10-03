package server

import (
	"context"
	"fmt"
	"io"
	"time"

	"workbuddy2api/internal/gateway"
)

// toolExecTimeout 单次工具执行的超时。
//
// 实测：生图 13-23 秒、搜索约 1 秒。给 5 分钟与 Loomy 官方客户端的
// 生图超时一致 —— 生图是重任务，按普通 API 的 30 秒设会大面积超时。
const toolExecTimeout = 5 * time.Minute

// chatOnceNonStream 发一次**非流式**对话请求，返回完整响应体。
//
// # 为什么工具循环要非流式
//
// 循环必须"先拿到完整响应，才知道模型要不要调工具"。
// 流式是边读边发的，第一帧出去后状态码就收不回来了 ——
// 那时若发现是 tool_calls，客户端已经收到一堆它看不懂的协议帧。
//
// # 与 chatVia 的关系
//
// chatVia 是**流式**出口（既有路径，行为不变）；本函数是非流式的
// 旁路，只在工具循环内用。两者都经 `h.cfg.Provider.Chat(...)`
// 走同一个 Provider 分派，所以多上游路由的语义完全一致。
//
// 上游返回的仍是 SSE（因为各上游的 Provider.Chat 统一把 body 包成流，
// 见 gateway.ChatStream 的契约）。所以这里把流读全 —— 调用方再用
// `wire.Aggregate` 把它聚合成非流式响应。
//
// 返回 (响应字节, 状态码, error)。状态码 >= 400 时响应字节是**上游的错误体**
// （原样带回，交给调用方分类 —— 与 chatVia 的契约一致）。
func (h *Handler) chatOnceNonStream(uid string, body []byte) ([]byte, int, error) {
	if h.cfg.Provider == nil {
		return nil, 0, fmt.Errorf("chatOnceNonStream: 未接线（cfg.Provider 为 nil）")
	}
	// 该 uid 的凭证：工具循环内不换号（已由出口层选好号），
	// 所以这里拿不到凭证就是真错误。
	acct := h.cfg.Pool.PickByUID(uid)
	if acct == nil {
		return nil, 0, fmt.Errorf("chatOnceNonStream: 账号 %s 不在池里（可能已被移除）", uid)
	}

	// providerID 从池子的归属反查 —— 与 applyErrorPolicy/nextResetAt 同一手法。
	providerID, hasProv := h.cfg.Pool.ProviderOf(uid)
	if !hasProv || providerID == "" {
		providerID = h.defaultProvider()
	}

	ctx, cancel := context.WithTimeout(context.Background(), toolExecTimeout)
	defer cancel()

	cred, ok := h.credentialFor(providerID, uid)
	if !ok {
		return nil, 0, fmt.Errorf("chatOnceNonStream: 取不到账号 %s 的凭证", uid)
	}

	cs, herr := h.chatProvider(ctx, providerID, cred, body)
	if herr != nil {
		return nil, 0, herr
	}
	defer cs.Body.Close()

	raw, rerr := io.ReadAll(io.LimitReader(cs.Body, maxToolLoopRespBytes))
	if rerr != nil {
		return nil, cs.Status, fmt.Errorf("读上游响应失败: %w", rerr)
	}
	return raw, cs.Status, nil
}

// maxToolLoopRespBytes 工具循环内读上游响应的上限。
//
// 循环内的响应都是小 JSON（聚合后的 chat.completion，含 tool_calls）。
// 8 MiB 远大于任何正常响应，同时挡住畸形响应。
const maxToolLoopRespBytes = 8 << 20

// chatProvider 经 Provider 分派发一次请求（工具循环用）。
//
// 与 handler.chatVia 的区别：那个按 `auth.Auth`（池子里的对象）走，
// 这里按 `gateway.Credential`（上游私有凭证的投影）走 ——
// 因为工具循环已经拿到了 Credential（见 credentialFor）。
func (h *Handler) chatProvider(ctx context.Context, providerID string, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	type chatDispatcher interface {
		Chat(ctx context.Context, id string, cred gateway.Credential, body []byte) (gateway.ChatStream, bool, error)
	}
	d, ok := h.cfg.Provider.(chatDispatcher)
	if !ok {
		return gateway.ChatStream{}, fmt.Errorf("chatProvider: 装配层不支持按上游分派对话")
	}
	cs, supported, err := d.Chat(ctx, providerID, cred, body)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	if !supported {
		return gateway.ChatStream{}, fmt.Errorf("chatProvider: 上游 %s 未注册", providerID)
	}
	return cs, nil
}

// credentialFor 取某账号在上游私有凭证投影。
//
// 走装配层（它与既有 `Credential(id, uid)` 那个可选能力同一形态）——
// 出口层不认识凭证类型，只把它原样转交给 Provider。
func (h *Handler) credentialFor(providerID, uid string) (gateway.Credential, bool) {
	type credentialSource interface {
		Credential(id, uid string) (gateway.Credential, bool)
	}
	s, ok := h.cfg.Provider.(credentialSource)
	if !ok {
		return gateway.Credential{}, false
	}
	return s.Credential(providerID, uid)
}

// chatStreamOnce 发一次**流式**对话请求，把上游的 SSE 流原样返回。
//
// 与 chatOnceNonStream 成对：那个读全（聚合用），这个不读（逐帧转发用）。
// 工具循环的流式路径必须用这个 —— 用那个就等于把流式体验丢了。
func (h *Handler) chatStreamOnce(uid string, body []byte) (io.ReadCloser, int, error) {
	if h.cfg.Provider == nil {
		return nil, 0, fmt.Errorf("chatStreamOnce: 未接线（cfg.Provider 为 nil）")
	}
	acct := h.cfg.Pool.PickByUID(uid)
	if acct == nil {
		return nil, 0, fmt.Errorf("chatStreamOnce: 账号 %s 不在池里（可能已被移除）", uid)
	}
	providerID, hasProv := h.cfg.Pool.ProviderOf(uid)
	if !hasProv || providerID == "" {
		providerID = h.defaultProvider()
	}
	cred, ok := h.credentialFor(providerID, uid)
	if !ok {
		return nil, 0, fmt.Errorf("chatStreamOnce: 取不到账号 %s 的凭证", uid)
	}

	ctx, cancel := context.WithTimeout(context.Background(), toolExecTimeout)
	// ⚠ 不能 defer cancel()：返回的 rc 还要被调用方读完。
	// 用"读完即取消"的包装把 cancel 绑到 rc 上，避免泄漏计时器。
	cs, err := h.chatProvider(ctx, providerID, cred, body)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	if cs.Body == nil {
		cancel()
		return nil, cs.Status, nil
	}
	return &cancelReadCloser{rc: cs.Body, cancel: cancel}, cs.Status, nil
}

// cancelReadCloser 在 Close 时释放 context（让超时计时器不再挂住）。
type cancelReadCloser struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *cancelReadCloser) Close() error {
	c.cancel()
	return c.rc.Close()
}
