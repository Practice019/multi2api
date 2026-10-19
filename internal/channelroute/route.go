// Package channelroute 把"客户端要的公开模型名"翻成"该发给哪条渠道的哪个上游模型名"。
//
// # 这是 R2 的**请求侧**
//
// 目录侧（internal/modelvis）是反的：上游名 → 公开名。请求侧是正的：
//
//	客户端发的 public-y  ──(该渠道的 model_mapping：公开名→上游名)──▶  上游真实名 real-x
//
// 两侧必须用**同一份** model_mapping（channel.ModelMap）⇒ 所以两个方向都从
// internal/modelmap 走，谁都不自己解析这份表。
//
// # 为什么只做"规划"、不做"挑一条"
//
// 挑一条要按 new-api 的阶梯 + 权重随机（internal/channelstore 的 Select 已经实现了，
// 含"重试 n 次走第 n 档"的语义）。这里只回答"**有哪些候选、各自该发什么上游名**"，
// 把随机与重试留给路由器 —— 这样规划层是纯的（可穷举断言），随机性只在一个地方。
package channelroute

import (
	"errors"
	"fmt"
	"sort"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/modelmap"
)

// ErrNoCandidate：没有任何启用渠道能服务这个模型名。
//
// ⚠ 它与"渠道存在但都停用"是同一个结论（默认收紧：停用 ⇒ 不可服务），
// 但错误信息里要把**请求的名字**带上 —— 否则用户排查时无从知道自己发的是哪个名字。
var ErrNoCandidate = errors.New("no_channel_for_model")

// Candidate 一条能服务该模型的渠道，以及要发给它的上游模型名。
type Candidate struct {
	Channel       *channelstore.Channel
	UpstreamModel string // 真正写进上游请求体的 model
	Priority      int
	Weight        int
}

// Plan 算出所有能服务 publicModel 的候选，按 (优先级 desc, 渠道名 asc) 排序。
//
// group 目前固定 channelstore.DefaultGroup（多分组留到以后）。
//
// 匹配规则（与目录侧**对称**，这是关键）：
//
//	把 publicModel 用该渠道的映射表解析成上游名：
//	  · 渠道没写这条映射  ⇒ 上游名 = publicModel 本身（映射只对表里列出的名字生效）
//	  · 渠道写了          ⇒ 上游名 = 映射的值（并检测 A→B→A 这类环）
//	再问：该渠道声明的模型里有这个上游名吗？有 ⇒ 它是候选。
//
// 于是"目录里看得见的名字，一定发得出去"，两者不会分叉。
func Plan(s *channelstore.Store, group, publicModel string) ([]Candidate, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: 未配置渠道存储", ErrNoCandidate)
	}
	if publicModel == "" {
		return nil, fmt.Errorf("%w: 模型名为空", ErrNoCandidate)
	}
	if group == "" {
		group = channelstore.DefaultGroup
	}

	var out []Candidate
	for _, ch := range s.List() {
		if ch == nil || ch.Status != channelstore.StatusEnabled {
			continue // 停用 ⇒ 不参与（默认收紧）
		}
		if ch.Group != "" && ch.Group != group {
			continue
		}
		up, err := upstreamName(ch, publicModel)
		if err != nil {
			// 该渠道的映射表自身有问题（例如环）：**跳过它**而不是整体报错 ——
			// 别的渠道可能正好能服务这个名字，为一条坏渠道让整个模型不可用是过度惩罚。
			// （环本来在保存时就被 modelmap.Validate 拦了，这里是纵深防御。）
			continue
		}
		if !declares(ch, up) {
			continue
		}
		out = append(out, Candidate{Channel: ch, UpstreamModel: up, Priority: ch.Priority, Weight: ch.Weight})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoCandidate, publicModel)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].Channel.ID < out[j].Channel.ID // 稳定：避免"同样输入、两次计划顺序不同"
	})
	return out, nil
}

// upstreamName 用该渠道的映射表把公开名解析成上游名。
func upstreamName(ch *channelstore.Channel, publicModel string) (string, error) {
	if len(ch.ModelMap) == 0 {
		return publicModel, nil
	}
	// Resolve 返回 (上游名, 是否命中, error)：
	// 未命中 ⇒ (入参, false)，正是我们要的"按原样透传"。
	resolved, mapped, err := modelmap.Mapping(ch.ModelMap).Resolve(publicModel)
	if err != nil {
		return "", err
	}
	if !mapped || resolved == "" {
		return publicModel, nil
	}
	return resolved, nil
}

// declares 该渠道是否声明了这个上游模型。
func declares(ch *channelstore.Channel, up string) bool {
	for _, m := range ch.Models {
		if m == up {
			return true
		}
	}
	return false
}
