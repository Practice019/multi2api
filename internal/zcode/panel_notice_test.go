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

// ② 本上游**必须**自报一句提醒，且它要能被 NoticeExt 认出来。
//
// # 为什么必须由 ExtOf 认出来
//
// 核心下发 notice 的判据是**类型断言**：
//
//	if ext, ok := gateway.ExtOf[gateway.NoticeExt](p); ok { info.Notice = … }
//
// 所以"写了 Notice() 方法"与"核心能拿到它"是两件事 ——
// 方法名差一个字母就静默失效，而**编译能过**、界面也不报错，
// 只是那句提醒永远不出现。（本仓在 LoginFlow 上踩过同一个坑。）
func TestNoticeIsDiscoverable(t *testing.T) {
	p := New(Config{AuthDir: t.TempDir()})
	p.probeOrigin = false

	ext, ok := gateway.ExtOf[gateway.NoticeExt](p)
	if !ok {
		t.Fatal("NoticeExt 未被 ExtOf 认出 —— 界面上那句提醒不会出现，" +
			"而前端不会报错（读不到字段就不渲染），所以只能靠这条断言拦")
	}
	got := ext.Notice()
	if got == "" {
		t.Fatal("Notice() 返回空串 = 不显示提醒（与不实现等价）—— " +
			"用户要求这个上游要提醒「这个是测试」")
	}
	if len([]rune(got)) > 40 {
		t.Errorf("提醒过长（%d 字）：它渲染在分组标题的小字区，\n"+
			"太长会挤掉「N 个账号 · M 项能力」。实际: %q", len([]rune(got)), got)
	}
	// 必须真的传达"测试"这件事 —— 否则这句提醒没完成任务。
	if !containsAny(got, "测试", "实验", "试用", "beta", "Beta") {
		t.Errorf("提醒里应表明这是测试/实验性质，实际 %q", got)
	}
}

// 提醒的措辞不该假装稳定（诚实性约束）。
//
// 为什么值得测：这句提醒是给用户做**决策依据**的（"要不要依赖这个上游"）。
// 写成"暂不支持 JWT"这种会被当成**事实陈述**，而真相是"没验证过"。
// 两者对用户的含义完全不同 —— 前者是设计如此，后者是要谨慎。
func TestNoticeIsHonestAboutWhatIsUntested(t *testing.T) {
	p := New(Config{AuthDir: t.TempDir()})
	p.probeOrigin = false

	got := p.Notice()
	// ⚠ 不能写"不支持"/"不可用"这类**断言性**说法 ——
	// JWT 通道是"没验证过"，不是"不支持"（代码是完整的）。
	for _, bad := range []string{"不支持", "不可用", "无法使用", "已废弃"} {
		if containsAny(got, bad) {
			t.Errorf("提醒不该断言「%s」—— JWT 通道是**未验证**而非不支持。\n"+
				"措辞: %q", bad, got)
		}
	}
	// 应当说出"未经真实验证"这层意思。
	if !containsAny(got, "未", "没有", "尚") {
		t.Errorf("提醒应说明「尚未验证」这层含义，实际 %q", got)
	}
}

// containsAny 是否含任一子串（小工具，避免为一个断言引入 strings 依赖的噪音）。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
