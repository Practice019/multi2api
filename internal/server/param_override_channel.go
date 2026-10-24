// param_override_channel.go 把渠道声明的参数覆盖接进出口层。
//
// 数据来源：渠道结构里已有 Overrides.Param（channelstore/store.go），
// 那是 new-api 的 param_override 对应物。本文件只做两件事：
//
//  1. 把一个渠道的 Param 解析成规则表（带缓存，配置变了才重解析）
//  2. 按本次请求命中的上游取出规则
//
// 为什么按 provider 取而不是按渠道 ID：出口层在这一点上只知道 provider
// （渠道选路发生在 forwardViaChannel 内部，那时 body 早被改写）。
// 所以取的是「该上游所有启用渠道的规则并集」，按优先级升序拼接。
package server

import (
	"log"
	"sort"
	"sync"

	"workbuddy2api/internal/channelstore"
)

// paramOverrideCache 渠道规则表的解析缓存。
//
// 键是渠道 ID + Param 的键名指纹 —— 够捕捉增删改，又不为改一个值算全量哈希。
// 不能每请求解析：重复 JSON 往返，且 problems 会刷屏。
var paramOverrideCache struct {
	sync.RWMutex
	m map[string]cachedOverrideRules
}

type cachedOverrideRules struct {
	rules    ParamOverrides
	problems []ParamRuleProblem
}

// channelParamOverrides 返回该上游生效的参数覆盖规则（按渠道优先级升序）。
//
// 没有渠道存储 / 该上游没有渠道 / 渠道没配覆盖 => nil（调用方零开销）。
func (h *Handler) channelParamOverrides(providerID string) ParamOverrides {
	s := h.cfg.Channels
	if s == nil {
		return nil
	}
	chs := s.List()
	if len(chs) == 0 {
		return nil
	}
	// 按优先级升序：后应用的（高优先级）覆盖先应用的。
	// 这与选路顺序一致 —— 目录来源、实际走的渠道、覆盖生效的规则，
	// 三者必须是同一套排序，否则用户看到的与实际生效的不是一回事。
	sorted := make([]*channelstore.Channel, 0, len(chs))
	for _, ch := range chs {
		if ch.Status != channelstore.StatusEnabled {
			continue
		}
		if len(ch.Overrides.Param) == 0 {
			continue
		}
		sorted = append(sorted, ch)
	}
	if len(sorted) == 0 {
		return nil
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })

	var out ParamOverrides
	for _, ch := range sorted {
		rules, problems := cachedParamOverrides(ch)
		for _, pr := range problems {
			log.Printf("[override] 渠道 %s 的规则被跳过: %s", ch.Name, pr.Error())
		}
		out = append(out, rules...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cachedParamOverrides 带缓存地解析一条渠道的规则。
func cachedParamOverrides(ch *channelstore.Channel) (ParamOverrides, []ParamRuleProblem) {
	key := overrideCacheKey(ch)
	paramOverrideCache.RLock()
	if c, ok := paramOverrideCache.m[key]; ok {
		paramOverrideCache.RUnlock()
		return c.rules, c.problems
	}
	paramOverrideCache.RUnlock()

	rules, problems := ParseParamOverrides(ch.Overrides.Param)
	paramOverrideCache.Lock()
	if paramOverrideCache.m == nil {
		paramOverrideCache.m = map[string]cachedOverrideRules{}
	}
	paramOverrideCache.m[key] = cachedOverrideRules{rules: rules, problems: problems}
	paramOverrideCache.Unlock()
	return rules, problems
}

// overrideCacheKey 渠道 ID + 规则键名指纹。
func overrideCacheKey(ch *channelstore.Channel) string {
	keys := make([]string, 0, len(ch.Overrides.Param))
	for k := range ch.Overrides.Param {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sb := make([]byte, 0, 64)
	sb = append(sb, ch.ID...)
	sb = append(sb, '|')
	for _, k := range keys {
		sb = append(sb, k...)
		sb = append(sb, ';')
	}
	return string(sb)
}
