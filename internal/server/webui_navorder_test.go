package server

import (
	"strings"
	"testing"
)

// TestWebUINavOrderIsUserAdjustable 导航顺序（T-378 修订版）：**只**「通用」组可调，上游固定字母序。
//
// # 目标对象
//
// 主（唯一可调）：左侧「通用」组那 11 项（仪表盘 / 系统状态 / 渠道 / 分组 / 令牌 / 账号池 / 任务调度 /
// 请求日志 / 模型目录 / 对话测试 / 设置）。
// 上游分组：**用户不调**，固定按 provider id 字母序（上游数量随部署变化，用户对它没有稳定偏好）。
//
// # 判据
//
//	1  orderedUpstreamGroups 体内是字母序实现（.sort( 且比较 a.id / b.id）
//	2  buildNav 用 orderedUpstreamGroups()；且它体内不再直接 for upstreamGroups()
//	3  生成上游面板的消费者仍用 upstreamGroups()（顺序一变会全量重生成面板）
//	4  入口与弹窗元素都在；#btnNavOrder 在 nav 元素之后
//	5  只有「通用」那把键（不得再出现 'wb2api.navorder.upstream'，除 migrate 的一处 removeItem）
//	6  orderedGenericSections 存在且用 navKey( 取键；buildNav 的 generic 行调用它
//	7  弹窗里**不得**再有 data-navorder-kind="upstream"（那段已移除）
//	8  【布局护栏】#btnNavOrder 必须在 class="navcol" 之后、class="content" 之前，且 .navcol 在 .layout 之内
//	   —— 这是 T-378 那个 P0 的结构性防护（按钮若挂在 .layout 下会抢占 grid 第二列 ⇒ 内容塌成窄柱）
//
// ⚠ T-378 删掉的旧判据：①"orderedUpstreamGroups 没读 upstream 键"（不再读）②"缺两个独立键"（只剩一把）
//
//	③"弹窗缺 upstream 容器"（那段已删）④"removeItem(LS_NAVORDER_UPSTREAM)"（常量已删）
func TestWebUINavOrderIsUserAdjustable(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// 1 上游：字母序
	ui := strings.Index(html, "function orderedUpstreamGroups(")
	if ui < 0 {
		t.Fatal("缺 orderedUpstreamGroups()")
	}
	ubody := html[ui:]
	if j := strings.Index(ubody, "\n  function "); j > 0 {
		ubody = ubody[:j]
	}
	if !strings.Contains(ubody, ".sort(") || !strings.Contains(ubody, "a.id") || !strings.Contains(ubody, "b.id") {
		t.Error("orderedUpstreamGroups 不是字母序实现（应有 .sort( 且比较 a.id / b.id）—— T-378 起上游固定按字母序")
	}
	if strings.Contains(ubody, "readNavOrder('upstream')") {
		t.Error("orderedUpstreamGroups 仍在读 upstream 顺序键 ⇒ 用户还能调上游顺序（T-378 已取消）")
	}
	// 2 buildNav
	bi := strings.Index(html, "function buildNav(")
	if bi < 0 {
		t.Fatal("找不到 buildNav")
	}
	body := html[bi:]
	if j := strings.Index(body, "\n  function "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "orderedUpstreamGroups()") {
		t.Error("buildNav 没用 orderedUpstreamGroups()")
	}
	if strings.Contains(body, "for (const p of upstreamGroups())") {
		t.Error("buildNav 里仍用 upstreamGroups()（应走 orderedUpstreamGroups）")
	}
	// 3 面板生成消费者
	if n := strings.Count(html, "for (const p of upstreamGroups())"); n < 1 {
		t.Errorf("生成上游面板的消费者也必须保持 upstreamGroups()，现在 %d 处", n)
	}
	// 4 元素（T-379：入口移到设置页，弹窗元素不变）
	for _, want := range []string{`id="btnNavOrder"`, `id="btnNavOrderReset"`, `id="navOrderModal"`, `id="navOrderList"`, `id="navOrderReset"`, `id="navOrderDone"`, `data-navorder-up`, `data-navorder-down`} {
		if !strings.Contains(html, want) {
			t.Errorf("缺元素/属性：%s", want)
		}
	}
	bn := strings.Index(html, `id="btnNavOrder"`)
	nn := strings.Index(html, "</nav>")
	if bn < 0 || nn < 0 {
		t.Fatalf("锚点缺失（btnNavOrder=%d </nav>=%d）", bn, nn)
	}
	// T-379：按钮已移出侧栏 ⇒ 仍必须在 </nav> 之后（buildNav 每轮 innerHTML 覆盖 #nav，
	// 放 #nav 里面会被抹掉；放设置页自然满足）。
	if bn < nn {
		t.Error("#btnNavOrder 必须在 </nav> 之后（buildNav 每轮 innerHTML 覆盖 #nav）")
	}
	// 5 键只剩一把
	if !strings.Contains(html, "'wb2api.navorder.generic'") {
		t.Error("缺 wb2api.navorder.generic 键")
	}
	up := strings.Count(html, "'wb2api.navorder.upstream'")
	if up > 1 {
		t.Errorf("'wb2api.navorder.upstream' 出现 %d 次（只允许 migrateNavOrder 里的一次 removeItem）", up)
	}
	if up == 1 && !strings.Contains(html, "removeItem('wb2api.navorder.upstream')") {
		t.Error("那唯一一次出现必须是 removeItem（一次性清理历史键），不能是 read/write")
	}
	if strings.Contains(html, "getItem('wb2api.navorder'") || strings.Contains(html, "setItem('wb2api.navorder'") {
		t.Error("仍存在裸键 wb2api.navorder（应带 .generic 后缀）")
	}
	// 6 「通用」组
	gi := strings.Index(html, "function orderedGenericSections(")
	if gi < 0 {
		t.Error("缺 orderedGenericSections()（「通用」组仍必须可调）")
	} else if !strings.Contains(html[gi:gi+900], "navKey(") {
		t.Error("orderedGenericSections 必须用 navKey( 取键（下标会漂移）")
	}
	if !strings.Contains(html, "const generic = orderedGenericSections(visible.filter(sec => !sec.dataset.provider));") {
		t.Error("buildNav 的通用组没调用 orderedGenericSections(…)")
	}
	// 6b 出厂默认顺序（T-380）
	//
	// 用户看到 HTML 的 DOM 顺序（tokens 在 accounts 之前、channels 在 status 之前）
	// 觉得不对，明确指定了这 11 项的顺序 ⇒ 必须落成一张**显式清单**，
	// 而不是依赖"碰巧的 HTML 排版"。这条护栏钉住清单本身。
	di := strings.Index(html, "const NAV_ORDER_DEFAULT = [")
	if di < 0 {
		t.Fatal("缺 NAV_ORDER_DEFAULT（通用组的出厂默认顺序）")
	}
	desired := []string{"dashboard", "accounts", "status", "channels", "groups", "tokens", "jobs", "logs", "modelcatalog", "chat", "settings"}
	end := strings.Index(html[di:], "];")
	if end < 0 {
		t.Fatal("NAV_ORDER_DEFAULT 没有闭合的 ];")
	}
	block := html[di : di+end]
	for idx, k := range desired {
		if !strings.Contains(block, "'"+k+"'") {
			t.Errorf("NAV_ORDER_DEFAULT 缺 %q（期望顺序 %v）", k, desired)
			continue
		}
		// 顺序：后一个 key 的位置必须在前一个之后
		if idx > 0 {
			prev := strings.Index(block, "'"+desired[idx-1]+"'")
			cur := strings.Index(block, "'"+k+"'")
			if prev >= 0 && cur >= 0 && cur < prev {
				t.Errorf("NAV_ORDER_DEFAULT 顺序不对：%q 应在 %q 之后", k, desired[idx-1])
			}
		}
	}
	// orderedGenericSections 必须在"没保存"时回落到这张清单（否则清单是死代码）
	if !strings.Contains(html[gi:gi+1400], "NAV_ORDER_DEFAULT") {
		t.Error("orderedGenericSections 没有引用 NAV_ORDER_DEFAULT ⇒ 那张清单是死代码（默认顺序不生效）")
	}
	// 7 弹窗里不得再有 upstream 段
	if strings.Contains(html, `data-navorder-kind="upstream"`) {
		t.Error("弹窗里仍有 data-navorder-kind=\"upstream\" 段（T-378 已移除）")
	}
	if !strings.Contains(html, `data-navorder-kind="generic"`) {
		t.Error("弹窗缺 data-navorder-kind=\"generic\" 段")
	}
	// 8 布局护栏（P0 的结构性防护）
	//
	// # 为什么判据从"按钮在 navcol 之内"改成"navcol 里只有 nav"
	//
	// T-378 的 P0 是：`#btnNavOrder` 被放成 `.layout` 的**第三个 grid 子元素**，
	// 于是它抢占第二列、把 `.content` 挤到 row2 col1（实测内容宽 168、高 4816）。
	// 当时的防线是"按钮必须在 .navcol 之内"。
	//
	// T-379 用户要求把按钮移到**设置页**（低频设置不该占 168px 的侧栏），
	// 于是更强的判据是：**`.navcol` 里只允许有 `#nav` 一个子元素** ——
	// 只要有人再往里塞东西，侧栏那一列就会变形，不管塞的是按钮还是别的。
	li := strings.Index(html, `class="layout"`)
	ni := strings.Index(html, `class="navcol"`)
	ci := strings.Index(html, `<div class="content" id="content">`)
	if li < 0 || ni < 0 || ci < 0 {
		t.Fatalf("布局锚点缺失（layout=%d navcol=%d content=%d）", li, ni, ci)
	}
	if !(li < ni && ni < ci) {
		t.Errorf("布局顺序不对：.layout(%d) < .navcol(%d) < .content(%d) 必须成立", li, ni, ci)
	}
	// 取 navcol 的开标签结尾与其后第一个 </div> 之间的内容
	navcolOpenEnd := strings.Index(html[ni:], ">")
	if navcolOpenEnd < 0 {
		t.Fatal("找不到 navcol 开标签的结尾")
	}
	navcolBodyStart := ni + navcolOpenEnd + 1
	navcolClose := strings.Index(html[navcolBodyStart:], "</div>")
	if navcolClose < 0 {
		t.Fatal("找不到 navcol 的闭合 </div>")
	}
	navcolBody := html[navcolBodyStart : navcolBodyStart+navcolClose]
	if strings.Count(navcolBody, "<nav") != 1 || strings.Count(navcolBody, "</nav>") != 1 {
		t.Errorf(".navcol 里应当**恰好一个** <nav>（该列只放侧栏导航），实际 <nav×%d </nav>×%d",
			strings.Count(navcolBody, "<nav"), strings.Count(navcolBody, "</nav>"))
	}
	if strings.Contains(navcolBody, "<button") {
		t.Error(".navcol 里出现了 <button> —— 侧栏那一列只放导航；" +
			"按钮塞进这一列会把 168px 的侧栏挤变形（T-379 起「导航顺序」已移到设置页）")
	}
	// 入口按钮必须在设置页内（用 bn = #btnNavOrder 的位置；bi 已被上面用来指 buildNav）
	si := strings.Index(html, `<section data-key="settings">`)
	if bn < 0 {
		t.Fatal("缺 #btnNavOrder（设置页的「⇅ 调整顺序」入口）")
	}
	if si < 0 || bn < si {
		t.Errorf("#btnNavOrder 应在**设置页**内（settings=%d btn=%d）", si, bn)
	}
	if !strings.Contains(html, `id="btnNavOrderReset"`) {
		t.Error("设置页缺 #btnNavOrderReset（「恢复默认顺序」按钮）")
	}
	// 弹窗是全局浮层，必须在入口按钮之后
	mi := strings.Index(html, `id="navOrderModal"`)
	if mi < 0 {
		t.Fatal("缺 #navOrderModal")
	}
	if mi < bn {
		t.Error("#navOrderModal 应在 #btnNavOrder **之后**（它是全局浮层，挂在 </main> 外）")
	}
}
