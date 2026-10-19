// select.go 渠道选路 —— 逻辑对标 new-api 的 channel-ability-routing。
//
// 蓝本（知识库 viking://resources/wiki/new-api/concept/channel-ability-routing.md）里的三条：
//
//  1. "Candidate abilities for the group and model are ordered by priority descending,
//     then weight descending."
//  2. "The retry counter selects the priority tier: retry n addresses the n-th priority level."
//  3. "Inside the tier, a channel is chosen by weighted random sampling with a bias applied
//     on top of the configured weight."
//
// 也就是：**priority 是耗尽阶梯，weight 是同一档内的分布旋钮**。
// 本文件只做"随机候选"这一步；new-api 解析顺序里的 pin / session affinity 两步本轮不做
// （它的顺序是 pin → affinity → random → filter check），留出同名入口便于将来补。
//
// ⚠ 失败**不返回 nil 了事**：new-api 返回 `ChannelSelectError`，带 HTTP status、error code、
// 可本地化的 message id、filter kind，以及一个区分"这个组+模型确实没有渠道"与其它失败
// （原文：a flag distinguishing "no channel for this group and model" from other failures）的标志。
// 我们照抄这个语义：调用方据此决定给客户端 400 还是 503，不用自己猜。
package channelstore

import (
	"fmt"
	"math/rand"
	"sort"
)

// 选路失败的错误码。与 new-api 的固定码同款（它是字符串常量，便于前端/日志辨认）。
const (
	CodeNoChannel = "no_channel"         // 这个组+模型没有任何启用渠道能服务
	CodeTierDone  = "priority_exhausted" // 优先级阶梯已经走完（重试次数超过档数）
)

// FilterKind 说明"是哪种过滤器拒掉了候选"。本轮只有渠道状态这一种，
// 值域与 new-api 的 request-path / task-plugin identity 两类区分开的语义一致：留字段不做。
type FilterKind string

const (
	FilterNone    FilterKind = ""
	FilterStatus  FilterKind = "status"  // 渠道不是 enabled
	FilterUnknown FilterKind = "unknown" // 其它（本轮不产生）
)

// SelectError 是选路失败的结构化错误（对标 new-api 的 ChannelSelectError）。
type SelectError struct {
	Status    int        // 建议给客户端的 HTTP 状态：无渠道 ⇒ 400（请求里的模型不存在）
	Code      string     // CodeNoChannel / CodeTierDone
	MessageID string     // 可本地化消息 id（前端按它取文案）
	Filter    FilterKind // 是哪种过滤导致没候选
	NoChannel bool       // true = 这个组+模型确实没有可用渠道（而非其它故障）
	Group     string
	Model     string
	Attempt   int
}

func (e *SelectError) Error() string {
	return fmt.Sprintf("没有可用渠道：group=%s model=%s attempt=%d code=%s filter=%s",
		e.Group, e.Model, e.Attempt, e.Code, e.Filter)
}

// Select 为 (group, model) 挑一条渠道。attempt 从 0 起（第 0 次是首次请求，第 n 次是第 n 次重试）。
//
// 与 new-api 一致：**第 n 次重试走第 n 档优先级**；档内按 weight 加权随机。
// 返回的渠道是深拷贝；调用方拿它去连接上游（base_url + PickKey 选出的 key）。
func (s *Store) Select(group, model string, attempt int) (*Channel, error) {
	if group == "" {
		group = DefaultGroup
	}
	if attempt < 0 {
		attempt = 0
	}
	s.mu.RLock()
	cands := make([]*Ability, 0, 8)
	for _, ab := range s.abilities {
		if ab.Group == group && ab.Model == model && ab.Enabled {
			cp := *ab
			cands = append(cands, &cp)
		}
	}
	// 取候选渠道（能力记录只有 ID，渠道本体在 channels 里）
	chans := map[string]*Channel{}
	for _, ab := range cands {
		if ch, ok := s.channels[ab.ChannelID]; ok {
			chans[ab.ChannelID] = ch
		}
	}
	s.mu.RUnlock()

	if len(cands) == 0 {
		return nil, &SelectError{
			Status: 400, Code: CodeNoChannel, MessageID: "no_channel_for_model",
			Filter: FilterNone, NoChannel: true, Group: group, Model: model, Attempt: attempt,
		}
	}

	// ① 按 priority 降序、同档按 weight 降序
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Priority != cands[j].Priority {
			return cands[i].Priority > cands[j].Priority
		}
		return cands[i].Weight > cands[j].Weight
	})

	// ② 取出所有优先级档（去重、降序）——"阶梯"
	tiers := make([]int, 0, 4)
	for _, ab := range cands {
		if len(tiers) == 0 || tiers[len(tiers)-1] != ab.Priority {
			tiers = append(tiers, ab.Priority)
		}
	}
	if attempt >= len(tiers) {
		return nil, &SelectError{
			Status: 503, Code: CodeTierDone, MessageID: "priority_tiers_exhausted",
			Filter: FilterNone, NoChannel: true, Group: group, Model: model, Attempt: attempt,
		}
	}

	// ③ 取第 attempt 档（重试 n 次 ⇒ 第 n 档）
	want := tiers[attempt]
	tier := make([]*Ability, 0, 4)
	for _, ab := range cands {
		if ab.Priority == want {
			tier = append(tier, ab)
		}
	}

	// ④ 档内加权随机（weight ≤ 0 当 1 处理：权重 0 的渠道不该被饿死，
	//    与 new-api "weight 是分布旋钮、不是开关"的口径一致；真正的开关是 status）
	total := 0
	for _, ab := range tier {
		w := ab.Weight
		if w <= 0 {
			w = 1
		}
		total += w
	}
	pick := s.rand.Intn(total)
	chosen := tier[len(tier)-1]
	for _, ab := range tier {
		w := ab.Weight
		if w <= 0 {
			w = 1
		}
		if pick < w {
			chosen = ab
			break
		}
		pick -= w
	}
	ch, ok := chans[chosen.ChannelID]
	if !ok || ch == nil {
		// 理论上不会发生（能力索引刻意与渠道同步）；真发生就是索引坏了，如实报。
		return nil, &SelectError{
			Status: 500, Code: CodeNoChannel, MessageID: "ability_without_channel",
			Filter: FilterUnknown, NoChannel: false, Group: group, Model: model, Attempt: attempt,
		}
	}
	if ch.Status != StatusEnabled {
		return nil, &SelectError{
			Status: 503, Code: CodeNoChannel, MessageID: "channel_disabled",
			Filter: FilterStatus, NoChannel: false, Group: group, Model: model, Attempt: attempt,
		}
	}
	return cloneChannel(ch), nil
}

// TierCount 返回 (group, model) 的优先级档数 —— 调用方用它决定"最多重试几次"。
func (s *Store) TierCount(group, model string) int {
	if group == "" {
		group = DefaultGroup
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[int]bool{}
	for _, ab := range s.abilities {
		if ab.Group == group && ab.Model == model && ab.Enabled {
			seen[ab.Priority] = true
		}
	}
	return len(seen)
}

// PickKey 从渠道的多个 key 里挑一个可用的（对标 new-api：channel info 里的 key 选择模式参与选路）。
//
// 语义：只挑 Status==Enabled 的 key；全坏了返回 ""（调用方据此把渠道判为不可用）。
// mode: "" / "random" 走随机；"round_robin" 用游标。
//
// ⚠ 实现在 PickKeyIdx（keyops.go），这里只是不要下标的薄封装 —— 挑 key 的规则**只允许有一份**。
// 曾经在两处各写一遍（这里一份、转发层 pickEnabledKey 一份），结果转发层那份"只取第一个"
// 把轮询彻底架空了：界面上写着轮询，实际永远打同一把 key。
func (s *Store) PickKey(ch *Channel) string {
	_, key := s.PickKeyIdx(ch)
	return key
}

// newRand 造一个选路用的随机源。测试用 NewWithRand 注入固定种子，保证分布判据可复现；
// 生产走 New（种子里混时间，避免同秒起两个进程拿到同一序列）。
func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
