// Package modelmap 是"公开模型名 ↔ 上游真实模型名"的映射层。
//
// 对标 new-api：每个渠道带一份 `model_mapping`（`map[string]string`），请求侧把客户端发来的
// **公开名**改写成上游**真实名**，目录/响应侧反向。它的实现里有一条被测试钉住的语义：
// **映射不能成环**（源码里有固定错误 `model_mapping_contains_cycle`）。
//
// 为什么单独一个包、而不是塞进 channelstore：
//   - 这一层与"渠道怎么连"无关，账号型上游将来也要吃映射（new-api 的映射也是渠道级字段，
//     但改写发生在 relay 的中立位置）；
//   - 纯函数、零依赖 ⇒ 好测（A→B→A 这种必须能一眼测出来）。
package modelmap

import (
	"errors"
	"fmt"
	"sort"
)

// ErrCycle 表示映射成环（直接或间接自指）。
var ErrCycle = errors.New("model_mapping_contains_cycle")

// Mapping 是"公开名 → 上游名"的一张表。空表 = 原样透传（不映射）。
type Mapping map[string]string

// Identity 判断这条映射是否等于"什么都不做"（nil / 空 / 只有自指项）。
func (m Mapping) Identity() bool {
	for k, v := range m {
		if k != v && v != "" {
			return false
		}
	}
	return true
}

// Resolve 把公开名解析成上游真实名。
//
//   - 未命中任何键 ⇒ 返回 (入参, false)：调用方按原样透传（new-api 也是这个行为，
//     映射只对表里列出的名字生效）；
//   - 命中且是链条 A→B→C ⇒ 一路走到不动点返回 C；
//   - 途中重复访问同一个键（含自指以外的环）⇒ 返回 ErrCycle。
//
// ⚠ 自指项 A→A 当**空操作**处理（不是环）：它表达的意图是"这个名字不用改"，
// 若判成环会把合理的配置直接打死。
func (m Mapping) Resolve(public string) (string, bool, error) {
	if len(m) == 0 {
		return public, false, nil
	}
	cur := public
	seen := map[string]bool{}
	changed := false
	for {
		next, ok := m[cur]
		if !ok || next == "" || next == cur {
			return cur, changed, nil
		}
		if seen[cur] {
			return "", false, fmt.Errorf("%w: 在 %q 处成环", ErrCycle, cur)
		}
		seen[cur] = true
		cur = next
		changed = true
	}
}

// Validate 预先检查整张表能不能用（供管理端保存前调用）。
//
// 为什么要提前查：环在**请求时**才炸会让用户看到一个莫名其妙的上游 400；
// 保存时就拒绝，错误信息直接指向出问题的键。这是 new-api 那条固定错误码的同一意图。
func (m Mapping) Validate() error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 稳定报错顺序，测试与日志可复现
	for _, k := range keys {
		if _, _, err := m.Resolve(k); err != nil {
			return err
		}
	}
	return nil
}

// Reverse 造反向表（上游真实名 → 公开名），供模型目录使用：目录要列**公开名**。
//
// 一对多（两个公开名映射到同一个上游名）时**不静默挑一个**：返回选定的表 + 冲突清单，
// 让调用方自己决定（目录里该显示哪个名字是产品决定，不是映射层能替它决定的）。
func (m Mapping) Reverse() (map[string]string, []Conflict) {
	rev := map[string]string{}
	var conflicts []Conflict
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, public := range keys {
		real := m[public]
		if real == "" {
			continue
		}
		if prev, ok := rev[real]; ok && prev != public {
			conflicts = append(conflicts, Conflict{Upstream: real, Kept: prev, Dropped: public})
			continue
		}
		rev[real] = public
	}
	return rev, conflicts
}

// Conflict 描述一次"两个公开名指向同一上游名"的冲突（保留了哪个、忽略了哪个）。
type Conflict struct {
	Upstream string
	Kept     string
	Dropped  string
}

// PublicNames 取这张映射里所有公开名（升序）—— 对外模型目录要用它。
func (m Mapping) PublicNames() []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UpstreamFor 是给"选路阶段"用的便捷入口：给定公开名，返回该渠道真正要对上游说的名字。
// 没有映射时返回原名（第三个返回值恒为 false，表示"没改写"）。
func (m Mapping) UpstreamFor(public string) (string, bool) {
	real, changed, err := m.Resolve(public)
	if err != nil || !changed {
		return public, false
	}
	return real, true
}
