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
	"math/rand"
	"net/http"

	"workbuddy2api/internal/channelroute"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
)

// errNoChannelMode：没配渠道存储 ⇒ 调用方按既有路径处理（不是错误，是一种"模式未开启"）。
var errNoChannelMode = errors.New("channel_mode_disabled")

// errChannelNoCandidate：能力索引里没有这个模型名 —— 与"模式未开启"是**两件不同的事**。
//
// 在 handler.go 里用它判"明确拒绝、绝不回落账号池"；特意做成同包别名，
// 这样 handler.go 不必 import channelroute（核心层少一个依赖，也少一处口径分叉）。
var errChannelNoCandidate = channelroute.ErrNoCandidate

// channelForward 一次渠道转发的结果。
type channelForward struct {
	Stream        gateway.ChatStream
	ChannelID     string
	ChannelName   string
	UpstreamModel string
	// KeyMask 本次实际用到的 API key 的**中间打码**形式（2026-10-20 加）。
	//
	// ⚠ 为什么需要它：请求日志的 UID 列对渠道请求一直是空的 —— 渠道用渠道凭据，
	// 没有账号池那种 uid。用户要求"没 uid 就填缺省的 key"，而且要打码。
	// 有了它，多 key 轮询时能从日志看出这条走了哪一把（换 key 排障不必再猜）。
	// ⚠ 必须是打码值：完整 key 落盘等于把凭据写进日志文件，那是另一类事故。
	// 打码由 channelstore.MaskKey 负责（保留前 4 + 后 4，中间省略）。
	KeyMask string
}

// channelServes 纯内存判断"这个名字能不能被某条启用渠道服务"。
//
// ⚠ 为什么需要它：legacy 的 providerFor 会把 "csg/deepseek-v4.1-flash-1017" 读成
// "上游前缀 csg"，判成未知上游并回 400 unknown_provider —— 而那是**前端建的渠道**给出的
// 公开名。渠道公开名是另一个命名空间，必须在 providerFor **之前**先认领，
// 否则这些名字永远走不到渠道转发这一步（实测现象：界面里模型选得好好的，一发就 400）。
//
// 不真发请求、无副作用 ⇒ 可以安全地"先问一句"。
func (h *Handler) channelServes(model string) bool {
	if h.cfg.Channels == nil || model == "" {
		return false
	}
	_, err := channelroute.Plan(h.cfg.Channels, channelstore.DefaultGroup, model)
	return err == nil
}

// forwardViaChannel 按渠道口径转发；未开启渠道模式时返回 errNoChannelMode。
//
// attempt=0 即"第一次尝试"。带重试的调用方用 forwardViaChannelAttempt 递增它换档 ——
// 之所以分两个函数：既有调用点（入口 + 各测试）都只关心"发一次"，不该被迫传一个恒为 0 的参数。
func (h *Handler) forwardViaChannel(ctx context.Context, publicModel string, body []byte) (channelForward, error) {
	return h.forwardViaChannelAttempt(ctx, publicModel, body, 0)
}

// forwardViaChannelAttempt attempt 递增即换下一优先级档（new-api 的"第 n 次重试走第 n 档"）。
func (h *Handler) forwardViaChannelAttempt(ctx context.Context, publicModel string, body []byte, attempt int) (channelForward, error) {
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

	// 选路：**优先级阶梯 + 同档加权随机 + 重试换档**（new-api 的语义）。
	//
	// ⚠ 为什么这里不直接调 channelstore.Select：它以能力索引的 (group, model, channel_id)
	// 为键，只知道"哪个渠道能服务哪个**上游模型名**"；而这条路径需要的是**已解析出上游名的候选**
	// （映射之后的名字，R2 请求侧），也就是 channelroute.Plan 的产物。两者输入不同，
	// 硬把 Plan 的结果塞回 Select 反而要再造一次键。语义保持一致，输入各用各的。
	//
	// 档位 = 优先级；重试时 attempt 递增即换下一档（与 new-api 的"第 n 次重试走第 n 档"同构）。
	cand, err := pickCandidate(cands, attempt)
	if err != nil {
		return zero, err
	}

	// ⚠ 挑 key 必须走存储层（PickKeyIdx）：那里才有 round_robin 游标与"只挑启用"的判定。
	// 这里曾经自己实现了一个"取第一个启用的"（pickEnabledKey），结果是：界面上写着轮询，
	// 实际永远打同一把 key。挑 key 的规则只允许有一份。
	keyIdx, key := h.pickKeyFor(cand.Channel)
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
		// ★ 上游说"这把 key 无效/没权限" ⇒ 自动禁用它（只禁这一把，不停整条渠道）。
		h.autoDisableKeyOnAuthError(cand.Channel.ID, keyIdx, err)
		return zero, err
	}
	return channelForward{
		Stream: cs, ChannelID: cand.Channel.ID, ChannelName: cand.Channel.Name,
		UpstreamModel: cand.UpstreamModel,
		// 打码后的 key 填进日志 UID 列（完整 key 绝不落盘）
		KeyMask: channelstore.MaskKey(key),
	}, nil
}

// pickCandidate 按 new-api 的选路语义从候选里挑一条。
//
//   - 档位按**优先级**划分：优先用最高档；attempt 递增 ⇒ 往下一档（重试换档）
//   - 同档内按**权重**加权随机（权重 <=0 视为 1，避免"权重没填 ⇒ 永远选不中"）
//   - 候选按 (优先级 desc, 渠道 id asc) 稳定排序（channelroute.Plan 保证）⇒ 同档顺序确定
//
// 返回错误时调用方应把它当"这条请求失败"上报，**不回落账号池**（Key 型渠道的分界）。
func pickCandidate(cands []channelroute.Candidate, attempt int) (channelroute.Candidate, error) {
	if len(cands) == 0 {
		return channelroute.Candidate{}, errChannelNoCandidate
	}
	// 分出档位（Plan 已按优先级降序 ⇒ 相邻相等即为同一档）
	type tier struct {
		prio int
		from int
		to   int // 半开区间 [from, to)
	}
	var tiers []tier
	for i := 0; i < len(cands); {
		j := i
		for j < len(cands) && cands[j].Priority == cands[i].Priority {
			j++
		}
		tiers = append(tiers, tier{prio: cands[i].Priority, from: i, to: j})
		i = j
	}
	ti := attempt
	if ti < 0 {
		ti = 0
	}
	if ti >= len(tiers) {
		// 档位用尽：重试次数超过"不同优先级档数" ⇒ 明确失败。
		// ⚠ 这里**不**回退到最后一档：那会让"重试"变成"无限重试同一条渠道"，
		// 上游真故障时表现为请求挂很久最后仍失败，而日志里看不出在重试什么。
		return channelroute.Candidate{}, fmt.Errorf("渠道候选档位已用尽（attempt=%d，档数=%d）", attempt, len(tiers))
	}
	t := tiers[ti]
	sum := 0
	for k := t.from; k < t.to; k++ {
		w := cands[k].Weight
		if w <= 0 {
			w = 1
		}
		sum += w
	}
	if sum <= 0 { // 理论上到不了（权重下限为 1），防御性保留
		return cands[t.from], nil
	}
	n := rand.Intn(sum)
	for k := t.from; k < t.to; k++ {
		w := cands[k].Weight
		if w <= 0 {
			w = 1
		}
		if n < w {
			return cands[k], nil
		}
		n -= w
	}
	return cands[t.from], nil // 兜底：不该到
}

// writeChannelError 把渠道转发的失败回报给客户端。
//
// # 核心规则：上游的状态码**原样透传**，不要把 429/401/400 包成 502
//
// 实测踩过（2026-10-20）：上游 OpenCSG 对 `deepseek-v4.1-flash-1017` 返回
// 429 capacity_exceeded / tpm_exceeded（该模型每分钟 token 用尽），经网关后变成
// 502 channel_error ⇒ 客户端当成"网关故障"于是**不按限流重试**，用户也去查网关。
// 绕过网关直连上游验证过同样是 429 ⇒ 那是上游的事实，网关的职责是**如实转达**。
//
// 三类分支：
//   - 上游给了状态码 ⇒ 透传它（429 另补一句中文，因为 tpm_exceeded 不是人话）
//   - 状态码不在合法区间（上游乱给）⇒ 502，不让非法码流到客户端
//   - 压根不是 HTTP 错误（没可用 key / 组合根漏接线）⇒ 502 channel_error
func (h *Handler) writeChannelError(w http.ResponseWriter, err error) {
	if ue, ok := gateway.AsUpstreamHTTPError(err); ok {
		if ue.Status < 100 || ue.Status > 599 {
			writeOpenAIError(w, http.StatusBadGateway, "channel_error",
				fmt.Sprintf("上游返回了非法的 HTTP 状态码 %d：%s", ue.Status, ue.Error()))
			return
		}
		msg := ue.Error()
		if ue.Status == 429 {
			// ⚠ 补在**同一条**消息里，而不是换掉上游原文：原文是上游的账，不替它改口径；
			// 但 tpm_exceeded / rpd_exceeded 这些词对用户不是人话，所以要给可行动的解释。
			msg += "；这是**上游**对该模型的频率/额度限制（tpm = 每分钟 token 数），稍后重试或换模型即可，与网关无关"
		}
		writeOpenAIError(w, ue.Status, "upstream_error", msg)
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, "channel_error", err.Error())
}

// pickKeyFor 用存储层的挑 key 规则（含 round_robin 轮询），并返回下标（供失败后自动禁用）。
//
// 下标是唯一可定位的坐标：用户可能填了两条完全相同的 key，靠字符串反查会禁错条目。
func (h *Handler) pickKeyFor(ch *channelstore.Channel) (int, string) {
	if h.cfg.Channels == nil || ch == nil {
		return -1, ""
	}
	return h.cfg.Channels.PickKeyIdx(ch)
}

// autoDisableKeyOnAuthError 上游判定"这把 key 无效/没权限"时，自动禁用**这一个** key。
//
// # 只对 401/403 生效，其余一律不禁
//
//   - 401/403 是**确定性**错误：同一把 key 再打一万次还是 401。留着它只会让每次请求
//     先白试一次（在轮询里就是 1/N 的请求注定失败），用户的体感是"时好时坏"。
//   - 429（限流）绝不能禁：额度按分钟滚动，禁掉之后要人去管理页手动救回来 ——
//     把"等一分钟自己好"变成"人工介入"，是更糟的体验。
//   - 5xx（上游故障）也不能禁：换一把 key 可能就好了，禁了等于自己减员。
//
// ⚠ 全禁光之后渠道会变成"没有可用 key"（502 channel_error）。这是**对的**：
// 与其拿一把已知无效的 key 反复打，不如明确报错让用户去换 key（管理页可一键重新启用）。
// 我们仍在响应里保留上游原文（见 writeChannelError），用户能看见"上游说 key 无效"。
func (h *Handler) autoDisableKeyOnAuthError(channelID string, keyIdx int, err error) {
	if h.cfg.Channels == nil || keyIdx < 0 {
		return
	}
	ue, ok := gateway.AsUpstreamHTTPError(err)
	if !ok {
		return
	}
	if ue.Status != http.StatusUnauthorized && ue.Status != http.StatusForbidden {
		return
	}
	_ = h.cfg.Channels.SetKeyStatus(channelID, keyIdx,
		channelstore.StatusAutoDisabled, channelstore.ReasonUpstreamAuth)
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
