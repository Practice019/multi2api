// grouprefs.go 「令牌/API key 上的分组引用」在**写路径**的唯一归一入口。
//
// # 它解决的问题（2026-11-01 实测）
//
// 分组真正的引用是 ID，名字只用于展示（groupstore/store.go:216、apikey.go:130）。
// 但界面把一个分组发上来时用的可能是**名字** —— 于是库里同时存在两种形态：
//
//	令牌 id=3  group="666 的模型"     ← 界面建的，名字
//	令牌 id=8  group="g-4d283c8f"     ← P3.5 迁移建的，ID
//
// 读路径必须两种都认（groupstore.GetRef），但**写路径不该再制造第二种**：
// 名字一改就断链（apikey.go:132 那段预言），而"断链"的表现是并集为空 ⇒
// 这把凭据一个模型都不许用（若判成"不限制"则是权限放大，两种都很难查）。
//
// # 为什么不在这里"查不到就丢掉"
//
// 丢掉一个引用 = 作用域变宽（受限 → 不限制）。那等于把用户配的授权悄悄放大，
// 而界面上完全看不出来。所以查不到就**拒**（400 + 指明是哪个引用），让用户当场改。
//
// # 未接分组存储时（groupsStore() == nil）
//
// 原样放过。那种部署根本没有分组功能，在这里拦会让它连令牌都建不了 ——
// 降级方向与 apikey.HasGroupResolver 那条一致（宁可宽松，也不要"谁都用不了"）。
package admin

import (
	"fmt"
	"strings"
)

// canonicalGroupRefs 把一组分组引用归一成**分组 ID**（ID 优先、名字兜底）。
//
// 返回 (归一后的列表, 错误文案)：错误文案为空 = 通过；非空 = 调用方直接 400。
//
// # 三态必须原样传下去（nil / 空 / 非空）
//
// apikey 那边的契约是：nil（字段缺失）⇒ 不改；[] ⇒ 清空（= 不限制）；非空 ⇒ 整体替换。
// 若本函数把 nil 变成 []，一次"只改额度"的更新就会把分组**清空**（权限被放大）。
// 所以入参 nil ⇒ 出参 nil；入参非 nil 但全是空白 ⇒ 出参空切片（那是显式清空）。
func (h *Handler) canonicalGroupRefs(refs []string) ([]string, string) {
	gs := h.groupsStore()
	nilIn := refs == nil
	if gs == nil {
		// 没有分组存储：原样放过（只做 trim/去重/去空，仍保住 nil 与 [] 的区别）。
		if nilIn {
			return nil, ""
		}
		return normList(refs), ""
	}
	in := normList(refs)
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, ref := range in {
		g, ok := gs.GetRef(ref)
		if !ok || g == nil {
			return nil, fmt.Sprintf("引用的分组不存在：%q —— 它可能已被删除；请重新选择分组后再保存", ref)
		}
		if seen[g.ID] {
			// 同一个分组既给了 ID 又给了名字 ⇒ 归一后重了，去重（保序）。
			continue
		}
		seen[g.ID] = true
		out = append(out, g.ID)
	}
	if nilIn {
		return nil, ""
	}
	return out, ""
}

// hasGroupRef 报告一组引用里是否有非空项（写路径判"用户到底有没有配分组"）。
//
// 单独写一个而不是用 len(refs) > 0：请求体里可能出现 ["", "  "] 这种
// "看起来给了、其实什么都没有"的形态，用 len 判会把它当成"配了分组"。
func hasGroupRef(refs []string) bool {
	for _, r := range refs {
		if strings.TrimSpace(r) != "" {
			return true
		}
	}
	return false
}
