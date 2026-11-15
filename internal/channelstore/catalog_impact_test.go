// catalog_impact_test.go 删除影响分析的判据（U-11 验收「删除影响明确」）。
//
// # 核心断言：两类影响必须分开
//
//	Lost     删掉后**没有供给** ⇒ 模型从目录消失（要去补）
//	Degraded 还有别的供给        ⇒ 仍可用（通常什么都不用做）
//
// 混成一句"影响 N 个模型"是没用的 —— 那两种情况的处理方式完全相反。
package channelstore

import (
	"reflect"
	"strings"
	"testing"
)

// impactFixture 三条渠道：
//
//	ch-a  sole   → only-here   （唯一供给）
//	ch-a + ch-b  → shared      （两条供给）
//	ch-b  other  → elsewhere   （与 ch-a 无关）
func impactFixture() UnifiedCatalog {
	return BuildCatalog([]*Channel{
		{ID: "ch-a", Name: "渠道A", Kind: "standard", Status: StatusEnabled,
			Models: []string{"only-here", "shared"}},
		{ID: "ch-b", Name: "渠道B", Kind: "standard", Status: StatusEnabled,
			Models: []string{"shared", "elsewhere"}},
	})
}

// ★ ① 唯一供给 ⇒ Lost（会从目录消失）。
func TestDeletionImpactSoleProviderIsLost(t *testing.T) {
	impact := CatalogDeletionImpactOf(impactFixture(), "ch-a")

	if !reflect.DeepEqual(impact.Lost, []string{"only-here"}) {
		t.Fatalf("★ 唯一供给的模型必须进 Lost，实际 %v（degraded=%v）", impact.Lost, impact.Degraded)
	}
}

// ★ ② 还有别的供给 ⇒ Degraded（不是 Lost）。
func TestDeletionImpactSharedProviderIsDegraded(t *testing.T) {
	impact := CatalogDeletionImpactOf(impactFixture(), "ch-a")

	for _, m := range impact.Lost {
		if m == "shared" {
			t.Fatalf("★ shared 还有 ch-b 能服务，不该被判成 Lost（%v）", impact.Lost)
		}
	}
	if !reflect.DeepEqual(impact.Degraded, []string{"shared"}) {
		t.Fatalf("★ 多供给的模型应进 Degraded，实际 %v", impact.Degraded)
	}
}

// ★ ③ 无关渠道 ⇒ 不受影响（计数不加进 Lost/Degraded）。
func TestDeletionImpactUnaffectedCounted(t *testing.T) {
	impact := CatalogDeletionImpactOf(impactFixture(), "ch-a")

	// elsewhere 只有 ch-b 供给 ⇒ 对 ch-a 而言不受影响。
	if impact.Unaffected != 1 {
		t.Fatalf("不受影响的数量应为 1（elsewhere），实际 %d", impact.Unaffected)
	}
	for _, m := range append(append([]string{}, impact.Lost...), impact.Degraded...) {
		if m == "elsewhere" {
			t.Fatalf("★ 无关模型进了影响列表：%v", m)
		}
	}
}

// ④ 不存在的渠道 ⇒ 什么都没影响，且说明里说清（而不是假装"没影响"）。
func TestDeletionImpactUnknownChannel(t *testing.T) {
	impact := CatalogDeletionImpactOf(impactFixture(), "ch-nope")
	if len(impact.Lost) != 0 || len(impact.Degraded) != 0 {
		t.Fatalf("未知渠道不该有影响：%+v", impact)
	}
	if impact.ChannelName != "" {
		t.Fatalf("未知渠道不该有名字，实际 %q", impact.ChannelName)
	}
	// 所有模型都"不受影响"—— 数字要对得上，否则会读成"目录是空的"。
	total := 0
	for _, m := range impactFixture().Models {
		_ = m
		total++
	}
	if impact.Unaffected != total {
		t.Fatalf("未知渠道时全部模型都该算不受影响：%d vs %d", impact.Unaffected, total)
	}
}

// ⑤ 说明文案必须区分三种情形（不许一句话糊过去）。
func TestDeletionImpactNoteDistinguishes(t *testing.T) {
	// 有 Lost ⇒ 警示"会消失"。
	lost := CatalogDeletionImpactOf(impactFixture(), "ch-a")
	if !strings.Contains(lost.Note, "消失") {
		t.Fatalf("有 Lost 时说明应警示「消失」，实际 %q", lost.Note)
	}
	// 只有 Degraded ⇒ 说"没有模型消失"。
	deg := CatalogDeletionImpactOf(BuildCatalog([]*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled, Models: []string{"m"}},
		{ID: "ch-b", Name: "b", Kind: "standard", Status: StatusEnabled, Models: []string{"m"}},
	}), "ch-a")
	if len(deg.Lost) != 0 {
		t.Fatalf("前置条件：不该有 Lost，实际 %v", deg.Lost)
	}
	if !strings.Contains(deg.Note, "没有模型消失") {
		t.Fatalf("只有 Degraded 时说明应说「没有模型消失」，实际 %q", deg.Note)
	}
	// ⚠ 我第一版这里写错了：以为删 ch-a 不影响任何模型 ——
	//   而 m 是 ch-a 的**唯一供给**，删掉它 m 就从目录消失了。
	//   要测"完全不影响"，被删的必须是一条**没有唯一供给**的渠道。
	none := CatalogDeletionImpactOf(BuildCatalog([]*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled, Models: []string{"m"}},
		{ID: "ch-b", Name: "b", Kind: "standard", Status: StatusEnabled, Models: []string{"m"}},
	}), "ch-b")
	if len(none.Lost) != 0 {
		t.Fatalf("前置条件：ch-b 删掉不该有模型消失，实际 %v", none.Lost)
	}
	if len(none.Degraded) != 1 || none.Degraded[0] != "m" {
		t.Fatalf("应报 m 降级，实际 %v", none.Degraded)
	}
}

// ⑥ 停用的渠道也算"供给"吗？不算 —— 它本来就不出目录。
//
// 这条钉的是"影响分析必须基于目录里**实际存在**的供给"，
// 而不是"渠道配置里声明的模型"。前者才是客户端看到的。
func TestDeletionImpactIgnoresDisabledChannel(t *testing.T) {
	catalog := BuildCatalog([]*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled,
			Models: []string{"only-here"}, ModelMap: map[string]string{"pub": "only-here"}},
		{ID: "ch-off", Name: "off", Kind: "standard", Status: 0,
			Models: []string{"only-here"}, ModelMap: map[string]string{"pub": "only-here"}},
	})
	// ch-off 停用 ⇒ 它对 pub 没有供给 ⇒ 删掉它零影响。
	impact := CatalogDeletionImpactOf(catalog, "ch-off")
	if len(impact.Lost) != 0 || len(impact.Degraded) != 0 {
		t.Fatalf("★ 停用的渠道本就不出目录，删掉它不该有任何影响：%+v", impact)
	}
}
