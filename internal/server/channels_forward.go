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
	"strings"

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
	// PublicModel 客户端请求时用的名字（别名或公开名）。
	//
	// 脱敏时回填到响应的 model 字段 —— 客户端认的是自己发出去的那个名字，
	// 而上游原名属于身份指纹（monkeycode-basic/deepseek-flash 点名了上游）。
	PublicModel string
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
// forwardViaChannel 保留原签名：publicModel 同时用作「匹配名」与「对外名」。
//
// 只在两者**不同**时才需要 forwardViaChannelAs（配了别名的场景：
// 匹配用内部名、对外回显用别名）。
func (h *Handler) forwardViaChannel(ctx context.Context, publicModel string, body []byte) (channelForward, error) {
	return h.forwardViaChannelAs(ctx, publicModel, publicModel, body)
}

// forwardViaChannelAs 分别指定「匹配名」与「对外回显名」。
//
// matchName   用于 channelroute.Plan 查渠道（必须是渠道声明的上游名 / 别名）
// displayName 填进 fw.PublicModel，脱敏时回填到响应 model 字段
//
// 为什么需要分开：配了 model_map 之后，客户端发 `007/xxx`（公开名），
// resolveModelAlias 会把它换成内部名再交给渠道匹配；而响应该回显
// 客户端发来的 `007/xxx`，不是内部名。
func (h *Handler) forwardViaChannelAs(ctx context.Context, matchName, displayName string, body []byte) (channelForward, error) {
	return h.forwardViaChannelAttemptAs(ctx, matchName, displayName, body, 0)
}

// forwardViaChannelAttempt 保留原签名（匹配名 = 对外名）。
func (h *Handler) forwardViaChannelAttempt(ctx context.Context, publicModel string, body []byte, attempt int) (channelForward, error) {
	return h.forwardViaChannelAttemptAs(ctx, publicModel, publicModel, body, attempt)
}

// forwardViaChannelAttemptAs attempt 递增即换下一优先级档（new-api 的「第 n 次重试走第 n 档」）。
//
// matchName   渠道匹配用的名字（渠道声明的上游名 / 别名）
// displayName 对外回显名（填进 fw.PublicModel，脱敏时回填响应 model 字段）
func (h *Handler) forwardViaChannelAttemptAs(ctx context.Context, matchName, displayName string, body []byte, attempt int) (channelForward, error) {
	var zero channelForward
	if h.cfg.Channels == nil {
		return zero, errNoChannelMode
	}
	group := channelstore.DefaultGroup
	cands, err := channelroute.Plan(h.cfg.Channels, group, matchName)
	if err != nil {
		// 默认收紧：没有任何启用渠道能服务这个名字 ⇒ 明确失败（不回落、不猜）
		return zero, err
	}

	// # P4.5：同档内失败换人
	//
	// 改造前这里只挑**一条**就发：随机挑到渠道 A，A 挂了（502/429）就
	// 直接把错误丢给客户端 —— 同一档里明明还有渠道 B 却没有机会。
	// 而账号池路径一直是有轮转的（maxAttempts + fail(uid); continue），
	// 于是"渠道模式反而比账号模式更脆"，这是本仓一个真实的容错缺口。
	//
	// ⚠ 只在**同一档内**换，不跨档：
	//
	//	同档换人 = 这条渠道坏了，换同级别的另一条（服务水平不变）
	//	跨档     = 整档不可用，降级（可能更差/更贵）—— 那是另一件事，
	//	           由 attempt 递增表达，不是这里的 retry。
	//
	// 所以这里**不**递增 attempt，而是维护一个"已试过的渠道 ID"集合。
	// 把两者混起来是本任务最容易犯的错（见 pickCandidateExcluding 的注释）。
	tried := make(map[string]bool, len(cands))
	var lastErr error
	for {
		cand, _, ok := pickCandidateExcluding(cands, attempt, tried)
		if !ok {
			// 该档没有未试过的候选了。
			//
			// ⚠ 一条都没试过就 ok=false ⇒ 是"档位用尽"（attempt 超出档数），
			//   要让 pickCandidate 报出那个具体错误（调用方按它区分 404/502）。
			//   试过若干条才耗尽 ⇒ 把**最后一个**错误带回去：那是用户真正该看到的原因。
			if len(tried) == 0 {
				_, perr := pickCandidate(cands, attempt)
				return zero, perr
			}
			return zero, lastErr
		}
		tried[cand.Channel.ID] = true

		fw, ferr := h.tryChannelOnce(ctx, cand, matchName, displayName, body)
		if ferr == nil {
			return fw, nil
		}
		lastErr = ferr

		// ⚠ 只有"上游明确说这次请求不行"才值得换人。Key 型渠道有几类错误
		//   换一条渠道也是同一个结果，换人只会把一次失败变成 N 次耗时：
		//
		//	请求体本身不合法（400 且不是上游的自定义拒绝）—— 但那也可能只是
		//	"这条渠道的上游不认这个参数"，所以仍然换人试，见下面的白名单判定。
		//
		// 这里采取"+白名单"而不是"黑名单"：只有明确不该换的才不换。
		// 否则新出现的错误类型会一律不换人，而"不换人"正是本次要修的缺陷。
		if !worthSwitching(ferr) {
			return fw, ferr
		}
	}
}

// tryChannelOnce 对**一条**候选渠道做一次完整的转发尝试（挑 key / 改写 body / 打上游）。
//
// 单独抽出来的理由：P4.5 要在同档内换渠道重试，而"发一次"这件事本身
// 有 9 个不同的失败出口（没有可用 key、body 改写失败、工厂没接线、上游 4xx/5xx…），
// 它们**各自都该触发换人**。把它抽成一个函数，外层循环就只需关心"换谁"，
// 而不必在 9 个 return 上各挂一次重试逻辑（那样必然漏掉一两个）。
func (h *Handler) tryChannelOnce(ctx context.Context, cand channelroute.Candidate, matchName, displayName string, body []byte) (channelForward, error) {
	var zero channelForward

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
	fw := channelForward{
		Stream: cs, ChannelID: cand.Channel.ID, ChannelName: cand.Channel.Name,
		UpstreamModel: cand.UpstreamModel,
		PublicModel:   matchName,
		// 打码后的 key 填进日志 UID 列（完整 key 绝不落盘）
		KeyMask: channelstore.MaskKey(key),
	}
	// 对外回显名：匹配用的是 matchName，但客户端要看到自己发来的名字。
	//
	// ⚠ 这是 001 泄露 csg/ 的根因：原实现把匹配名同时当对外名，
	//   于是配了别名之后响应 model 回填的还是上游内部名。
	if strings.TrimSpace(displayName) != "" {
		fw.PublicModel = displayName
	}
	return fw, nil
}

// worthSwitching 报告"这次失败换一条同档渠道还有没有意义"。
//
// # 为什么用白名单式（只挡明确不该换的），而不是黑名单式
//
// 黑名单（"只有 5xx 才换"）会漏掉将来的新错误类型，而漏掉的表现正是本次要修的
// 那个缺陷：同档明明有健康渠道，一个偶发失败就把请求打死。
// 白名单只挡**换人不可能改变结果**的那几类：
//
//	· 组合根漏接线（ChannelProvider 为 nil）—— 所有渠道都会同一个报错
//	· 请求上下文已取消（客户端断开）—— 再试只是浪费一次上游调用
//
// 其余（没可用 key、上游 4xx/5xx、body 改写失败）**都换**：
// 没可用 key 是这条渠道自己的问题；上游 4xx 常常是"这条渠道的上游不认这个参数"；
// 5xx 与 429 更是典型的"换一条就好"。
func worthSwitching(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 组合根漏接线：换多少条都是同一个错，换人只会让日志出现 N 条同样的话。
	if strings.Contains(err.Error(), "没有注入 ChannelProvider 工厂") {
		return false
	}
	return true
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

// pickCandidateExcluding 在**同一档**内挑一条候选，跳过 alreadyTried 里的渠道。
//
// ⚠ 这是 P4.5 最容易搞错的地方，所以把语义写死在这里：
//
//	attempt 是**档位**（第几优先级档），不是"同档内第几条"。
//	同档内换人是**另一个维度** —— 用 exclude 集合表达，绝不能靠递增 attempt。
//
// 为什么不能靠递增 attempt：那会把"同档内重试"变成"降级到下一档"。
// 两件事的语义完全不同 ——
//
//	同档内换人 = 这条渠道坏了，换同级别的另一条（用户拿到的还是同一档的服务）
//	跨档          = 这条档位整体不可用，降级（可能更贵/更慢/更差）
//
// 混起来的后果：一条渠道偶发 502 就把全部流量降到低优先级档，而日志里
// 看起来像是"重试了 3 次"，实际上是"静默降级了"。
//
// 返回：
//
//	候选      挑中的那条
//	tierLeft  该档**还剩几条没试过**（含本次挑中的这条之外）—— 调用方据此决定还要不要继续
//	ok        false = 该档已经没有未试过的候选了（调用方应当换档或放弃）
func pickCandidateExcluding(cands []channelroute.Candidate, attempt int, tried map[string]bool) (channelroute.Candidate, int, bool) {
	if len(cands) == 0 {
		return channelroute.Candidate{}, 0, false
	}
	tier := candidatesOfTier(cands, attempt)
	if len(tier) == 0 {
		return channelroute.Candidate{}, 0, false
	}
	left := make([]channelroute.Candidate, 0, len(tier))
	for _, c := range tier {
		if tried[c.Channel.ID] {
			continue
		}
		left = append(left, c)
	}
	if len(left) == 0 {
		return channelroute.Candidate{}, 0, false
	}
	picked := weightedPick(left)
	return picked, len(left) - 1, true
}

// candidatesOfTier 取第 attempt 档的全部候选（Plan 已按优先级降序 ⇒ 相邻相等即同档）。
//
// 档位用尽时返回空（调用方按"没得试"处理）。负 attempt 按 0。
func candidatesOfTier(cands []channelroute.Candidate, attempt int) []channelroute.Candidate {
	ti := attempt
	if ti < 0 {
		ti = 0
	}
	idx := 0
	for i := 0; i < len(cands); {
		j := i
		for j < len(cands) && cands[j].Priority == cands[i].Priority {
			j++
		}
		if idx == ti {
			return cands[i:j]
		}
		idx++
		i = j
	}
	return nil
}

// weightedPick 同档内按权重加权随机（权重 <=0 视为 1，与 pickCandidate 同一口径）。
func weightedPick(tier []channelroute.Candidate) channelroute.Candidate {
	sum := 0
	for _, c := range tier {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		sum += w
	}
	if sum <= 0 { // 理论上到不了（权重下限为 1）
		return tier[0]
	}
	n := rand.Intn(sum)
	for _, c := range tier {
		w := c.Weight
		if w <= 0 {
			w = 1
		}
		if n < w {
			return c
		}
		n -= w
	}
	return tier[0] // 兜底
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
