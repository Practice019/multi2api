// ref.go 「分组引用」的**唯一**解析口径：ID 或 名字 → 分组 ID。
//
// # 为什么需要它（一次被证实的断裂）
//
// 分组真正的引用是 ID（groupstore.Group.ID 创建后不变），名字只用于展示 ——
// 这条纪律写在两个地方（store.go:216 与 apikey.go:130-134），而且 apikey 那段
// 还预言了存名字的后果。可界面上、人手里出现的却都是**名字**：
//
//	· 令牌抽屉的分组多选器把分组名当 value 发给后端（webui-tokens.html）
//	· admin 的引用计数报告为了容错，自己写了一份"名字也认"（groups.go 的 canonical）
//
// 于是"名字也认"这件事在引用计数那边有、在**判定链**那边没有：
//
//	token.groups = ["666 的模型"]（名字）
//	→ apikey.UnionGroupModels → 注入函数里的 groupstore.Get("666 的模型") → 未命中
//	→ 并集为空 → 「受限且为空」⇒ 这把令牌一个模型都不许用（/v1/models 空列表）
//
// 实测（2026-11-01，demo 7860）：
//
//	令牌 id=3  group="666 的模型"  → GET /v1/models ⇒ {"data":[],"object":"list"}
//	而分组本身的 id 是 g-4d283c8f（auths/groups.json），里面有 7 个模型。
//
// 反过来"查不到就当没限制"也不行 —— 那会把权限放大到全部模型（另一种看不见的错）。
// ⇒ 所以这里把"ID 优先、名字兜底"写成**一份**实现，三处调用方都用它：
// 引用计数（admin）、模型判定（apikey 经装配层注入）、别名表（server）。
//
// # 为什么 ID 优先
//
// 用户完全可以把一个分组命名为 "g-1234abcd"。ID 优先意味着"看起来像 ID 的那个"
// 永远是 ID —— 与 groupstore.Get 的既有行为一致，也不改变任何已按 ID 存对的数据。
package groupstore

import "strings"

// ResolveRef 把一条引用解析成分组 ID（ID 优先，其次名字）。
//
// groups 由调用方给出，用来避免重复 List —— 引用计数那条路一次要处理全部分组
// 与全部凭据，逐条引用去查库会是 N×M 次加锁。
//
// 返回 (id, true) 命中；(\"\", false) 未命中（垃圾串、分组已被删）。
// 未命中**不报错**：悬空引用是 P5 引用完整性的问题，不能让它打断判定链。
func ResolveRef(groups []*Group, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	for _, g := range groups {
		if g != nil && strings.TrimSpace(g.ID) == ref {
			return g.ID, true
		}
	}
	for _, g := range groups {
		if g != nil && g.Name == ref {
			return g.ID, true
		}
	}
	return "", false
}

// GetRef 按**引用**取一条分组（ID 优先，其次名字），返回深拷贝。
//
// 与 Get（只认 ID）的关系：GetRef ≈ Get(id) ∪ GetByName(name)，但两个查询在
// **同一次读锁内**完成 —— 分开调用会各自加锁，中间那次改名能让两次查询落到
// 不同的分组上（旧 ID 命中一条、新名字命中另一条）。
func (s *Store) GetRef(ref string) (*Group, bool) {
	ref = strings.TrimSpace(ref)
	if s == nil || ref == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if g, ok := s.groups[ref]; ok {
		return cloneGroup(g), true
	}
	for _, g := range s.groups {
		if g.Name == ref {
			return cloneGroup(g), true
		}
	}
	return nil, false
}
