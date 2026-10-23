// groups_resolve.go 把「key.Groups（分组 ID）」解析成「这把 key 能用哪些模型」（P2）。
//
// # 它解决的问题
//
// P2 之前，"这把 key 能用哪些模型"只有一个来源：Key.Models 白名单。
// 现在多了一个（而且是**将要取代它**的那个）：Key.Groups → 若干分组 →
// 分组的模型并集。判定链在 server 侧只有**一处**（allowedModelsForRequest），
// 本文件只负责把"分组 ID 列表"这件事**解析**成模型名列表 —— 只做取数，不做判定。
//
// # 为什么用注入的函数值，而不是直接 import groupstore
//
// 本包有一条硬约束：**零内部依赖**（见 apikey.go 里 ModelAllowed 那段注释）。
// 一旦 import groupstore，本包就会进到核心的依赖图里；而依赖方向长出来之后
// 就再也收不回去 —— 下一个想复用 groupstore 的包会照着这里的样子来。
//
// 更实际的理由：本包在**测试里**不需要分组存储。注入一个函数值，测试可以喂
// 任意"分组 → 模型"的映射，不需要建文件、不需要临时目录，也就能把
// "分组存在但不含模型""分组已被删"这些边界写干净。
//
// # 为什么本文件**不**提供「model 在不在分组里」的判断
//
// 这是本文件最容易被写歪的地方，所以写清楚。
//
// 「模型名怎么比」在本仓已经有且只有**一份**实现：server 的 modelAllowed，
// 它用 gateway.SplitModel（与选路同一个函数）剥上游前缀。若在这里再写一份
// 近似的比较（哪怕只是 strings.TrimSpace + 逐字比），就会出现：
//
//	白名单认的模型  ≠  分组认的模型  ≠  请求实际会路由到的模型
//
// 而分叉的表现是"某个看起来完全合法的请求偶尔 403"，且只在模型名含 '/' 时
// 出现 —— 这是本仓已经踩过一次的坑（见 apikey.go 与 models_test.go 的注释）。
//
// ⇒ 所以本文件只回答「这个 key 的分组里，一共声明了哪些模型名」（并集、去重），
//
//	**是否放行**由 server 侧那唯一一份比较逻辑判。这也是 server 侧只改
//	allowedModelsForRequest 一个函数就能接上的原因。
package apikey

import (
	"strings"
)

// GroupModelsFunc 按分组 ID 取该分组的模型清单（内部模型名）。
//
// 约定：
//
//	· 分组不存在 / 已被删 → 返回 nil（**不要** panic，也不要报错 ——
//	  悬空引用是 P5 引用完整性的问题，不能让它把整条判定链打断）
//	· 返回的切片会被复制，调用方不需要另给副本
//	· 本函数会在**没有本包锁**的情况下被调用（见 UnionGroupModels）
type GroupModelsFunc func(groupID string) []string

// SetGroupModels 注入"分组 → 模型清单"的解析函数（由装配层在启动时调用一次）。
//
// 传 nil 表示**未接线**（例如这个部署根本没有分组功能）。
// 未接线时 UnionGroupModels 返回空，而 HasGroupResolver 返回 false ——
// 让调用方能区分"解析不出来"和"解析出来是空的"，这两件事的后果完全不同
// （前者是配置问题应当降级，后者是权限问题应当拒绝）。
func (s *Store) SetGroupModels(fn GroupModelsFunc) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.groupModels = fn
	s.mu.Unlock()
}

// HasGroupResolver 报告分组解析函数是否已接线。
//
// ⚠ 这个判据不能省。考虑"一把 key 配了分组、但装配层忘了注入"：
//
//	若把"解析不出来"当成"分组里没有模型" ⇒ 并集为空 ⇒ 这把 key 一个模型
//	都用不了。表现是运维改了个配置，用户的调用全 403，而错误信息只会说
//	"不允许调用模型 X" —— 排查方向直接跑偏。
//
// 所以未接线时调用方应当**降级到白名单路径**（存量行为），而不是拒绝。
func (s *Store) HasGroupResolver() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.groupModels != nil
}

// UnionGroupModels 求若干分组的模型名**并集**（去重、保序）。
//
// # 参数一个都没有 ⇒ 返回 nil
//
// "没有分组"在调用方那里是**另一个判据**（不限制），不该在这里用返回值表达 ——
// 返回 nil 与"分组都解析不出模型"在这里长得一样，调用方必须自己先判 groups 是否为空。
//
// # 为什么保序而不是排序
//
// 排序要多一次 O(n log n)，而结果只被用来做集合判定（modelAllowed 是线性扫）。
// 保序让"第一个出现的分组优先"这个隐含信息留着，将来若要实现"优先用某分组"
// 不必改数据形状。
//
// # ⚠ 调用注入函数时**不持锁**
//
// fn 内部会去问 groupstore（那边有自己的锁）。若这里持着 s.mu 调用它，两个
// 存储的锁就会形成"谁先拿谁"的顺序依赖 —— 今天不会死锁，但下一个在
// fn 里回调本包的人就会制造一个。锁只用来取函数值，取完就放。
func (s *Store) UnionGroupModels(groups []string) []string {
	if s == nil || len(groups) == 0 {
		return nil
	}
	s.mu.Lock()
	fn := s.groupModels
	s.mu.Unlock()
	if fn == nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool, len(groups)*4)
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		for _, m := range fn(g) {
			m = strings.TrimSpace(m)
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// GroupModelsOf 取一把 key 的分组列表与它们的模型并集。
//
// 返回：
//
//	groups   该 key 的分组 ID（空 = 这把 key 不受分组限制）
//	models   groups 里所有分组的模型并集（groups 为空时也是 nil）
//	ok       key 是否存在
//
// ⚠ 三个返回值都要用：只看 models 无法区分"没分组"与"分组里没模型"。
func (s *Store) GroupModelsOf(id string) (groups, models []string, ok bool) {
	groups, ok = s.GroupsOf(id)
	if !ok || len(groups) == 0 {
		return groups, nil, ok
	}
	return groups, s.UnionGroupModels(groups), true
}
