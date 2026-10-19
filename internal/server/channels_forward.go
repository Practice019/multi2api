// channels_forward.go 渠道口径的**转发**：把一次 chat 请求真的送到某条 Key 型渠道。
//
// 这是 R3 的最后一环（此前渠道能被建/列/影响目录，但请求还去不了它）。
//
// # 链路
//
//	客户端发 model=pub-y
//	  → channelroute.Plan：哪些启用渠道能服务它、各自要发什么上游名（R2 请求侧）
//	  → 挑一条（本轮取首选；加权随机/重试档位的 Select 见下面 TODO）
//	  → 取该渠道一个启用 key
//	  → 把请求体里的 model 改成**上游名** real-x
//	  → openaicompat 打它的 Base URL（逐字节透传其余内容）
//
// # ⚠ 刻意的边界
//
//  1. **不回落到账号池**：这是 Key 型渠道与"账号上游"的根本区别。渠道说"我能服务 pub-y"
//     但它的 key 全停用/上游 401 时，正确行为是**明确失败**，而不是悄悄换一条账号池里的号 ——
//     否则用户看到的是"模型能用但计费口径变了/身份变了"，且排查时完全不知道走了哪条。
//  2. **只有配了渠道存储才走这条**（h.cfg.Channels != nil）；没配 ⇒ 调用方回落既有路径。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"workbuddy2api/internal/channelroute"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
)

// errNoChannelMode：没配渠道存储 ⇒ 调用方按既有路径处理（不是错误，是一种"模式未开启"）。
var errNoChannelMode = errors.New("channel_mode_disabled")

// channelForward 一次渠道转发的结果。
type channelForward struct {
	Stream        gateway.ChatStream
	ChannelID     string
	ChannelName   string
	UpstreamModel string
}

// forwardViaChannel 按渠道口径转发；未开启渠道模式时返回 errNoChannelMode。
func (h *Handler) forwardViaChannel(ctx context.Context, publicModel string, body []byte) (channelForward, error) {
	var zero channelForward
	if h.cfg.Channels == nil {
		return zero, errNoChannelMode
	}
	group := channelstore.DefaultGroup
	cands, err := channelroute.Plan(h.cfg.Channels, group, publicModel)
	if err != nil {
		// 默认收紧：没有任何启用渠道能服务这个名字 ⇒ 明确失败（不回落、不猜）
		return zero, err
	}

	// TODO(下一轮)：这里换成 channelstore.Select(group, cands[0].UpstreamModel, attempt) ——
	// 它的"优先级阶梯 + 同档加权随机 + 第 n 次重试走第 n 档"才是 new-api 的完整语义。
	// 本轮先取首选，把链路（规划→取key→改写model→打上游）先钉住并测通。
	cand := cands[0]

	key := pickEnabledKey(cand.Channel)
	if key == "" {
		return zero, fmt.Errorf("渠道 %s 没有可用 key（全部停用或为空）", cand.Channel.Name)
	}

	rewritten, err := withUpstreamModel(body, cand.UpstreamModel)
	if err != nil {
		return zero, fmt.Errorf("改写请求体里的 model 失败: %w", err)
	}

	// ⚠ 这里**故意不认识**具体上游包（架构守卫 TestCoreDoesNotDependOnUpstreams）：
	// 核心只通过 gateway.Provider 说话，由 cmd/server（组合根）注入工厂。
	// 我第一版直接 import openaicompat 在这里 new 实例 ⇒ 守卫立刻红：
	// 「核心包 internal/server 依赖了上游包 internal/openaicompat」。
	// 那个红是对的 —— 核心一旦认识具体上游，"再加一种 Key 型上游"就要改核心。
	if h.cfg.ChannelProvider == nil {
		return zero, fmt.Errorf("渠道模式已开启但没有注入 ChannelProvider 工厂（组合根漏接线）")
	}
	p := h.cfg.ChannelProvider(cand.Channel.ID, cand.Channel.BaseURL)
	if p == nil {
		return zero, fmt.Errorf("ChannelProvider 工厂对渠道 %s 返回了 nil", cand.Channel.ID)
	}
	cred := gateway.Credential{
		Provider: cand.Channel.ID,
		UID:      cand.Channel.ID,
		Nickname: cand.Channel.Name,
		// ⚠ Secret 的形状要跨上游通用：核心不知道也不该知道"哪种上游怎么读 key"。
		// 用 map 这个最小公共形状 —— 具体上游自己解释（openaicompat 就接受这种形状）。
		Secret: map[string]any{"api_key": key},
	}
	cs, err := p.Chat(ctx, cred, rewritten)
	if err != nil {
		return zero, err
	}
	return channelForward{
		Stream: cs, ChannelID: cand.Channel.ID, ChannelName: cand.Channel.Name,
		UpstreamModel: cand.UpstreamModel,
	}, nil
}

// pickEnabledKey 取一个启用状态的 key；空 = 没得用（不是"返回空字符串当 key 用"）。
//
// 与 channelstore 的 PickKey 区别：这里不做 round_robin（那属于"多 key 轮换"的运维语义，
// 见 channelstore.PickKey）。先取**第一个启用的**，保证"能跑通"这件事先确定下来。
func pickEnabledKey(ch *channelstore.Channel) string {
	if ch == nil {
		return ""
	}
	for _, k := range ch.Keys {
		if k.Status == channelstore.StatusEnabled && k.Key != "" {
			return k.Key
		}
	}
	return ""
}

// withUpstreamModel 把请求体里的 model 换成上游名，其余字段**原样保留**。
//
// 为什么必须改写：客户端发的是对外公开名（pub-y），而上游只认识 real-x。
// 不改写就会把公开名透传给上游 ⇒ 上游 400/404，而用户看到的是"网关坏了"。
//
// ⚠ 为什么要解析而不是字符串替换：model 值里可能有转义、也可能有别的字段叫别的名字。
// 解析成 map[string]json.RawMessage 只动 model 这一个键，其余键的字节序与内容保持不变
// （json.RawMessage 保留原始字节 ⇒ 我们不会"顺手美化"用户的其他字段）。
func withUpstreamModel(body []byte, upstream string) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		// 不是 JSON 对象（例如已经是别的协议形状）⇒ 不动它，交给上游去报错：
		// 在这里擅自改字节会让"上游的真实抱怨"消失，排查更难。
		return body, nil //nolint:nilerr // 有意不报错：见上
	}
	if _, ok := fields["model"]; !ok {
		return body, nil // 没有 model 字段 ⇒ 没什么可改的
	}
	enc, err := json.Marshal(upstream)
	if err != nil {
		return nil, err
	}
	fields["model"] = enc
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return out, nil
}
