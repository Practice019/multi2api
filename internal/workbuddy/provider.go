// Package workbuddy 把 WorkBuddy（腾讯 CodeBuddy）上游适配为 gateway.Provider。
//
// # 这个包的角色
//
// 它是**第一个 Adapter**，也是 gateway 接口的第一个真实检验：
// 如果 4 个方法装不下 workbuddy，说明接口设计错了 —— 现在发现比搬完业务后再发现便宜。
//
// 本包（Task 3a）只做**薄适配**：把现有的 internal/upstream 包成 gateway.Provider。
// workbuddy 的业务逻辑（成长中心/猫猫旅行/签到）后续搬进来（Task 3b/3c）。
//
// # 与 internal/upstream 的关系
//
//	internal/upstream   HTTP 客户端封装（ChatStream / FetchModels / 签名 / header）
//	internal/workbuddy  上游的**身份**与**能力声明**（本包）
//
// 搬运时的原则：upstream 是与上游无关的 HTTP 细节，留在原地；
// 只有"CodeBuddy 专属的业务语义"才搬进本包。
package workbuddy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// providerID 上游标识。
//
// 会被用作：
//   - 模型名前缀（"workbuddy/auto"）
//   - 配置里的 provider key
//   - 统计维度（by_provider）
//
// 格式受 gateway 约束（^[a-z][a-z0-9-]*$），有契约测试守着。
const providerID = "workbuddy"

// Provider 实现 gateway.Provider。
type Provider struct {
	client *upstream.Client
}

// New 建一个 workbuddy Provider。
//
// 只构造，不做网络请求 —— 契约测试会多次调用 factory，不能有副作用。
func New() gateway.Provider { return &Provider{client: upstream.New()} }

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力声明。
//
// ⚠ **声明了就必须实现**（契约测试会查）。
// workbuddy 实际具备：对话、动态模型目录、签到、成长中心、猫猫旅行。
// 不具备：福利领取（codearts 专属）、主动额度探测（workbuddy 是被动从响应推断的）。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat |
		gateway.CapModels |
		gateway.CapCheckin |
		gateway.CapGrowth |
		gateway.CapTravel
}

// Chat 转发一次对话请求。
//
// 各上游的差异（/v2 前缀、header 拼法、UA）全部关在 upstream.Client 里。
//
// 注意 status 的语义：非 2xx **不返回 error**，而是把状态码与错误体一起返回，
// 让调用方统一处理"业务错误码"（如 429 触发冷却）与"传输错误"。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	// ChatStream 目前不接受 ctx（内部自建）。这里检查一次，
	// 保证调用方传已取消的 ctx 时能立刻返回而不是白跑一趟请求。
	if err := ctx.Err(); err != nil {
		return gateway.ChatStream{}, err
	}

	rc, status, respBody, err := p.client.ChatStream(a, body)
	if err != nil {
		// respBody 非空时说明是"上游返回了错误体"，属于业务错误，仍按流返回。
		if len(respBody) > 0 {
			return gateway.ChatStream{
				Status: status,
				Body:   io.NopCloser(bytes.NewReader(respBody)),
			}, nil
		}
		return gateway.ChatStream{}, err
	}
	return gateway.ChatStream{Status: status, Body: rc}, nil
}

// Models 返回上游动态模型目录。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	a, err := authOf(cred)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ms, err := p.client.FetchModels(a)
	if err != nil {
		return nil, err
	}
	out := make([]gateway.ModelInfo, 0, len(ms))
	for _, m := range ms {
		out = append(out, gateway.ModelInfo{
			// upstream.ModelInfo 的 ContextWindow/MaxTokens 就是
			// 上游的 maxInputTokens/maxOutputTokens（见其字段注释）。
			ID:              m.ID,
			ContextWindow:   clampInt64(m.ContextWindow),
			MaxOutputTokens: clampInt64(m.MaxTokens),
		})
	}
	return out, nil
}

// clampInt64 把上游给的 int64 收进 int（契约里是 int）。
//
// 上游字段是 int64、gateway.ModelInfo 用 int 是为了跨平台一致。
// 极端值（上游返回超大数据）会溢出成负数 —— 那会让下游算出负的窗口大小，
// 比"截断到上限"更糟。所以显式收窄。
func clampInt64(v int64) int {
	const maxInt = int64(^uint(0) >> 1)
	if v < 0 {
		return 0
	}
	if v > maxInt {
		return int(maxInt)
	}
	return int(v)
}

// authOf 从 Credential 里取出 workbuddy 的凭证结构。
//
// Secret 是 any（各上游凭证结构不同），所以必须类型断言。
// **断言失败要返回明确错误，不能 panic** —— 这是契约要求，也有测试守着。
func authOf(cred gateway.Credential) (*auth.Auth, error) {
	if cred.Secret == nil {
		return nil, errors.New("workbuddy: 凭证为空（Credential.Secret 未设置）")
	}
	a, ok := cred.Secret.(*auth.Auth)
	if !ok {
		return nil, fmt.Errorf("workbuddy: 凭证类型不对，期望 *auth.Auth，实际 %T", cred.Secret)
	}
	if a == nil {
		return nil, errors.New("workbuddy: 凭证是 nil 指针")
	}
	return a, nil
}

var _ gateway.Provider = (*Provider)(nil)
