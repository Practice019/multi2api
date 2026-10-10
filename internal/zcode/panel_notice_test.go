// panel_notice_test.go 两个 UI 约定的守卫。
//
// # 守的是用户提的两个要求
//
//	① "这个 zcode 不需要有额外的标签页"
//	② "在上游账号管理里，Zcode 那个地方用小字提醒一下，这个是测试"
//
// 两条都是"界面行为"，而它们**都不会让测试自然变红**：
//
//	· 多一个标签页 → 界面上多一项，没人报错
//	· 少一句提醒   → 界面上少一行小字，没人报错
//
// 所以必须专门断言。这两条也正好是一对**反向**约束：
// ①要求"少一个入口"，②要求"多一句说明" —— 说明这两件事是分开的，
// 各自的判据也不同（前者看 Hidden，后者看 NoticeExt）。
package zcode

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/gateway"
)

// ① 本上游**不该**贡献任何面板标签页。
//
// # 判据链（照前端 panelCapsOf 的实现，不是我自己编的）
//
// 前端生成一个标签页的条件是：
//
//	panelRoutesFor(provider, cap).length > 0
//	  = routesFor(provider, cap) 里存在 method==GET **且 !hidden** 的路由
//
// 所以"不生成标签页" ⟺ 该上游**所有**路由要么不是 GET，要么 hidden。
//
// # 我原来的错误
//
// 我让 /admin/zcode/quota 这条可见，于是左侧导航多出一个「额度探测」。
// 对照其它声明了 CapQuotaProbe 的上游：
//
//	codearts  端点是 POST（routesFor 只认 GET）→ 无面板
//	trae      没有路由                        → 无面板
//	workbuddy 没有独立 GET 额度端点            → 无面板
//
// **zcode 是唯一因此多出一个空标签页的**。而那个面板里什么都没有 ——
// 额度已经在账号池的「额度」列与分组行的「刷新本上游额度」按钮上
// （两者都走 core 的 POST /admin/accounts/quota/refresh，与本路由无关）。
//
// 所以隐藏它零功能损失，只是去掉一个重复的空壳入口。
func TestNoVisiblePanelRoute(t *testing.T) {
	p := New(Config{AuthDir: t.TempDir()})
	p.probeOrigin = false

	for _, r := range p.AdminRoutes() {
		if r.Method == http.MethodGet && !r.Hidden {
			t.Errorf("路由 %s %s 是**可见的 GET** → 前端会为它生成一个标签页。\n"+
				"用户明确要求「这个 zcode 不需要有额外的标签页」——\n"+
				"若确实需要面板，请先确认它不是重复入口；否则应标 Hidden: true",
				r.Method, r.Path)
		}
	}
}

// ①的反面：路由**存在**（只是不可见）。
//
// 为什么这条也要写：把 Hidden 理解成"删掉路由"是常见的误解。
// Hidden 的语义是「存在、但不是面板入口」——
// 路由照挂（curl / 排障仍可用），只是前端不为它生成标签页。
//
// 若有人为了"去掉标签页"而**删掉路由**，额度接口就没了 ——
// 而界面上看起来完全一样（都是没有那个标签页）。
func TestAdminRoutesStillExistButHidden(t *testing.T) {
	p := New(Config{AuthDir: t.TempDir()})
	p.probeOrigin = false

	routes := p.AdminRoutes()
	if len(routes) == 0 {
		t.Fatal("不该为了去掉标签页而删掉路由 —— Hidden 只是「不是面板入口」")
	}
	// 额度端点必须仍在（它是排障与前端可能的直连入口）。
	var foundQuota bool
	for _, r := range routes {
		if r.Path == "/admin/zcode/quota" {
			foundQuota = true
			if r.Handler == nil {
				t.Error("额度路由的 Handler 不能为空")
			}
			if r.Capability != gateway.CapQuotaProbe {
				t.Errorf("额度路由的能力位应是 %q，实际 %q", gateway.CapQuotaProbe, r.Capability)
			}
		}
	}
	if !foundQuota {
		t.Error("额度路由不该被删除（它仍要能被直接调用）")
	}
}

// ② 界面提醒（NoticeExt）**本上游刻意不实现**。
//
// # 为什么留一条"它被删过"的反向断言（而不是把用例整段删掉）
//
// 用户先要求「Zcode 那个地方用小字提醒一下，这个是测试」，我实现了它；
// 后来用户又要求「去掉这个文字」。留一条反向断言能防住两种误改：
//
//	有人以为这里漏了 → 补上提醒 → 这条红（用户已明确不要它）
//	有人恢复时只改一半 → 也红（缺 extensions.go 或编译期断言）
//
// 判据：ExtOf[gateway.NoticeExt] 对本上游**必须失败** ——
// 与「实现了但返回空串」是不同形态（后者会下发一个空字段）。
func TestNoticeIsDeliberatelyAbsent(t *testing.T) {
	p := New(Config{AuthDir: t.TempDir()})
	p.probeOrigin = false

	if _, ok := gateway.ExtOf[gateway.NoticeExt](p); ok {
		t.Error("本上游不该实现 NoticeExt —— 用户已要求去掉那句提醒。若要恢复，需同时补回 " +
			"extensions.go 的 Notice()、interfaces_compile_test.go 的编译期断言，以及本文件的正面用例。")
	}
}
