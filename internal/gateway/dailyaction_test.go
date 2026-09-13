// dailyaction_test.go 每日动作扩展点的契约测试。
//
// # 这组测试要守住的三条性质
//
//  1. **报了的动作必须真的能执行** —— OneURL / AllURL 指向的端点必须真的
//     挂在这条上游的 AdminRoutes 里。报一个不存在的端点，前端就会渲染出
//     一个点下去报错的按钮（"假按钮"，与 LoginFlow.Configured 要避免的同类）。
//
//  2. **workbuddy 的既有动作逐字不变** —— 端点、文案、顺序。
//     这是本次改动最容易搞砸的地方：它们现在能用，漂移就是回归。
//     所以断言是**全等**（不是"包含"）——"多一个/少一个/改一个字"都要红。
//
//  3. **codearts 不得报 checkin / keepalive** —— 它的 Caps() 里没有
//     CapCheckin。两处是同一件事实的两种表达，不一致就是缺陷：
//     报了会让前端给 codearts 渲染「签到」，而那条路由是 workbuddy 挂的。
package gateway_test

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"workbuddy2api/internal/codearts"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/workbuddy"
)

// actionsOf 取某个上游自报的每日动作（过滤后）。
func actionsOf(t *testing.T, p gateway.Provider) []gateway.DailyAction {
	t.Helper()
	ext, ok := gateway.ExtOf[gateway.DailyActionExt](p)
	if !ok {
		t.Fatalf("%s 未实现 DailyActionExt", p.ID())
	}
	return gateway.SanitizeDailyActions(ext.DailyActions())
}

// routeSet 该上游声明的 AdminRoutes 路径集合（"METHOD PATH" 形式）。
func routeSet(t *testing.T, p gateway.Provider) map[string]bool {
	t.Helper()
	ext, ok := gateway.ExtOf[gateway.AdminExt](p)
	if !ok {
		t.Fatalf("%s 未实现 AdminExt", p.ID())
	}
	out := map[string]bool{}
	for _, r := range ext.AdminRoutes() {
		out[strings.ToUpper(r.Method)+" "+r.Path] = true
	}
	return out
}

// newWorkbuddy 建一个可用的 workbuddy Provider（不需要真账号 —— 这里只查声明）。
func newWorkbuddy(t *testing.T) *workbuddy.Provider {
	t.Helper()
	return workbuddy.NewWithConfig(workbuddy.Config{})
}

func newCodearts(t *testing.T) *codearts.Provider {
	t.Helper()
	return codearts.NewWithConfig(codearts.Config{})
}

// TestWorkbuddyDailyActionsExactList 逐字钉住 workbuddy 的每日动作清单。
//
// # 为什么是全等而不是"包含 checkin"
//
// 改造前前端写死的三行里，每日动作是两条（checkin / keepalive），
// 第三条 credits 走能力位路径（见下面 TestCreditsNotADailyAction）。
// "包含"式断言放得过"多报了一个动作"——那会让界面上凭空多一个按钮，
// 而没有任何测试会红。
func TestWorkbuddyDailyActionsExactList(t *testing.T) {
	got := actionsOf(t, newWorkbuddy(t))

	type want struct {
		ID, Label, Title, OneURL, AllURL string
		Batch                            bool
	}
	// ⚠ 这些值与改造前 webui.html 的写死值**逐字对应**：
	//
	//	data-act="checkin"   title="单账号签到"  → POST /admin/checkin
	//	data-act="keepalive" title="刷新 token"  → POST /admin/keepalive
	//
	// 顺序同理：改造前签到在保活前面。
	//
	// Batch：checkin=true（顶部有「全部签到」）、keepalive=**false**
	//（顶部**没有**「全部保活」—— 用户删掉的，见 TestWorkbuddyKeepaliveHasNoBulkButton）。
	expected := []want{
		{"checkin", "签到", "单账号签到", "/admin/checkin", "/admin/checkin", true},
		{"keepalive", "保活", "刷新 token", "/admin/keepalive", "/admin/keepalive", false},
	}

	if len(got) != len(expected) {
		t.Fatalf("workbuddy 报的每日动作 = %d 个，want %d 个\n  实际: %+v",
			len(got), len(expected), got)
	}
	for i, w := range expected {
		g := got[i]
		if g.ID != w.ID {
			t.Errorf("第 %d 个动作 ID=%q want %q（顺序也是契约：改造前签到在前）", i, g.ID, w.ID)
		}
		if g.Label != w.Label {
			t.Errorf("%s 的 Label=%q want %q（按钮文案必须逐字不变）", g.ID, g.Label, w.Label)
		}
		if g.Title != w.Title {
			t.Errorf("%s 的 Title=%q want %q（悬停提示必须逐字不变）", g.ID, g.Title, w.Title)
		}
		if g.OneURL != w.OneURL {
			t.Errorf("%s 的 OneURL=%q want %q", g.ID, g.OneURL, w.OneURL)
		}
		if g.AllURL != w.AllURL {
			t.Errorf("%s 的 AllURL=%q want %q", g.ID, g.AllURL, w.AllURL)
		}
		if g.Batch != w.Batch {
			t.Errorf("%s 的 Batch=%v want %v", g.ID, g.Batch, w.Batch)
		}
	}
}

// TestWorkbuddyActionURLsReallyExist 报了的端点必须真的挂着。
//
// 这是"假按钮"守卫：OneURL/AllURL 指向一条不存在的路由时，前端会渲染
// 一个点下去 404 的按钮，而**没有任何测试会红** —— 除非有这一条。
func TestWorkbuddyActionURLsReallyExist(t *testing.T) {
	p := newWorkbuddy(t)
	routes := routeSet(t, p)
	for _, a := range actionsOf(t, p) {
		for _, u := range []string{a.OneURL, a.AllURL} {
			if u == "" {
				continue
			}
			key := "POST " + u
			if !routes[key] {
				t.Errorf("动作 %s 报了 %s，但 %s 不在 AdminRoutes 里\n  已挂载: %v",
					a.ID, u, key, sortedKeys(routes))
			}
		}
	}
}

// TestWorkbuddyKeepaliveHasNoBulkButton 「全部保活」**不得**回来。
//
// # 这条测试是踩出来的（我的第一版实现真的把它加回来了）
//
// 我第一版让 Batch 表达"有没有全量端点"，于是按 AllURL != "" 给 keepalive
// 填了 true —— 重建重启后，浏览器实测里顶部立刻多出一个
// 「全部保活（workbuddy）」。而那个按钮**是用户明确要求删掉的**。
//
// 更糟的是：**当时没有任何测试会红**。所以补了这一条。
//
// # 为什么用户删掉它（不是"上游不支持"）
//
// POST /admin/keepalive（不带 uid）与**自动保活槽**调的是同一个函数
// RunKeepaliveFor，唯一差别是标签（manual vs auto）。自动保活默认开启，
// 到 keepalive_hours 就会跑一遍。所以"全部保活"= 让它们提前跑一遍 ——
// 而 token 保活刷的是有有效期的凭证，提前几小时刷**没有收益**。
//
// ⇒ Batch 表达的是"**界面要不要给它入口**"（决策），
//
//	不是"上游有没有全量端点"（事实）。两者混一起就会把删掉的功能加回来。
func TestWorkbuddyKeepaliveHasNoBulkButton(t *testing.T) {
	for _, a := range actionsOf(t, newWorkbuddy(t)) {
		if a.ID != "keepalive" {
			continue
		}
		if a.Batch {
			t.Errorf("keepalive 的 Batch=true —— 界面上会冒出「全部保活」，"+
				"而那是用户明确要求删掉的按钮（理由见本测试的注释）。"+
				"AllURL 仍然如实报 %q，但 Batch 必须是 false", a.AllURL)
		}
		return
	}
	t.Fatal("workbuddy 没有报 keepalive 动作 —— 它现在能用，不该消失")
}

// TestWorkbuddyCheckinHasBulkButton 反过来：签到**必须**有顶部入口。
//
// 与上一条配对。只钉"不该有的"而不钉"该有的"，会让一次误删
// （把 Batch 全关掉）静默通过：界面上一个全量按钮都没有，
// 而没有任何断言会红。
func TestWorkbuddyCheckinHasBulkButton(t *testing.T) {
	for _, a := range actionsOf(t, newWorkbuddy(t)) {
		if a.ID != "checkin" {
			continue
		}
		if !a.Batch {
			t.Error("checkin 的 Batch=false —— 顶部的「全部签到」会消失，那是改造前就有的入口")
		}
		if a.AllURL == "" {
			t.Error("checkin 的 AllURL 为空 —— 全量按钮点了没有端点")
		}
		return
	}
	t.Fatal("workbuddy 没有报 checkin 动作")
}

// TestBatchImpliesAllURL Batch=true 而 AllURL 为空是**无效组合**。
//
// 前端的 renderAllDailyButtons 要求 `batch && all_url` 两个都满足才渲染
// （单独看 batch 会渲染出一个点了不知道打到哪的按钮）。
// 这条测试保证上游**不会产出**这种组合，于是前端那道判断是纵深防御
// 而不是唯一防线 —— 判据有两处独立落实，任一处写错另一处会兜住。
func TestBatchImpliesAllURL(t *testing.T) {
	for _, p := range []gateway.Provider{newWorkbuddy(t), newCodearts(t)} {
		for _, a := range actionsOf(t, p) {
			if a.Batch && a.AllURL == "" {
				t.Errorf("%s 的动作 %s 声明 Batch=true 但 AllURL 为空 —— "+
					"无效组合：前端的全量按钮会点到空路径", p.ID(), a.ID)
			}
		}
	}
}

// TestCodeartsDailyActionsExactList codearts 只报**一个**动作：领取福利。
func TestCodeartsDailyActionsExactList(t *testing.T) {
	got := actionsOf(t, newCodearts(t))

	if len(got) != 1 {
		t.Fatalf("codearts 报的每日动作 = %d 个，want 1 个（领取福利）\n  实际: %+v", len(got), got)
	}
	g := got[0]
	if g.ID != "welfare" {
		t.Errorf("ID=%q want %q", g.ID, "welfare")
	}
	if g.Label != "领取福利" {
		t.Errorf("Label=%q want %q（用户原话：就是领取一下福利呗）", g.Label, "领取福利")
	}
	if g.OneURL != "/admin/welfare/claim" {
		t.Errorf("OneURL=%q want %q", g.OneURL, "/admin/welfare/claim")
	}
	// Batch / AllURL 必须为空 —— 上游没有"全部领取"这种端点，如实报。
	//
	// ⚠ 这条不是形式主义：若有人给它填上 AllURL，前端的顶部就会出现
	// 一个「全部领取福利」按钮，点下去要么 404，要么（更糟）遍历
	// **整个账号池**去领取 —— 包括别家上游的账号。
	if g.Batch {
		t.Error("codearts 的福利领取不该声明 Batch=true —— 它没有全量端点")
	}
	if g.AllURL != "" {
		t.Errorf("codearts 的福利领取 AllURL=%q，want 空（没有全量端点）", g.AllURL)
	}
}

// TestCodeartsActionURLsReallyExist 同 workbuddy 的"假按钮"守卫。
func TestCodeartsActionURLsReallyExist(t *testing.T) {
	p := newCodearts(t)
	routes := routeSet(t, p)
	for _, a := range actionsOf(t, p) {
		if a.OneURL == "" {
			continue
		}
		key := "POST " + a.OneURL
		if !routes[key] {
			t.Errorf("动作 %s 报了 %s，但 %s 不在 AdminRoutes 里\n  已挂载: %v",
				a.ID, a.OneURL, key, sortedKeys(routes))
		}
	}
}

// TestCodeartsDoesNotClaimCheckin 能力位与每日动作必须**一致**。
//
// # 这是本次改动的核心不变式
//
// codearts 的 Caps() 里没有 CapCheckin（它没有每日签到端点）。
// 所以它也不能自报 checkin / keepalive 这两个每日动作。
//
// 两处不一致的实测后果（改造前就是这个形态）：
//
//	前端给 codearts 的行渲染「签到」，点下去 POST /admin/checkin ——
//	那条路由是 **workbuddy 挂的**。轻则 404，重则作用在别家账号上
//	（历史上有 703 条这样的脏记录，见 workbuddy/upstream_isolation_test.go）。
func TestCodeartsDoesNotClaimCheckin(t *testing.T) {
	p := newCodearts(t)

	if p.Caps().Has(gateway.CapCheckin) {
		t.Fatal("前提被破坏了：codearts 现在声明了 CapCheckin —— 那就得同时报 checkin 动作，" +
			"本条测试的判据要一起改")
	}
	for _, a := range actionsOf(t, p) {
		if a.ID == "checkin" || a.ID == "keepalive" {
			t.Errorf("codearts 自报了动作 %q，但它的 Caps() 里没有 CapCheckin —— "+
				"前端会渲染出一个打到 workbuddy 路由的假按钮", a.ID)
		}
	}
}

// TestDailyActionsHaveNoSecretKeys 下发的每日动作**不得**夹带上游私有状态。
//
// # 判据为什么是**结构**而不是关键词
//
// 我第一版写的是"字段里出现 token/secret/… 就报错"，实测**假红**：
// workbuddy 的保活动作 title 是「刷新 token」—— 那是**面向用户的文案**，
// 不是凭证。关键词匹配分不出"这个词是给用户看的"与"这个词是秘密"，
// 所以它会把正确的东西判成缺陷。
//
// 而那正是最坏的守卫形态：假红会被当成噪声，真缺陷跟着一起被忽略。
//
// 正确的判据是**结构性的**：DailyAction 的每个字段都是**展示/路由**
// 用途，任何真实的凭证都不可能塞进这六个字符串字段而不改变它们的含义。
// 所以这里断言的是"结构体没有多出字段"：
// 有人加了 `Secret any` / `RawAuth any` 这样的字段，这条才会红 ——
// 而那种改动**确实**该被拦下（DailyAction 原样下发给浏览器）。
//
// 与前端侧的 verify_credential_hygiene.js 同一件事的两个面：
// 那边看的是"页面上有没有泄露"，这边看的是"结构体有没有开这个口子"。
func TestDailyActionsHaveNoSecretKeys(t *testing.T) {
	// 允许的字段名集合（含 json tag）。加字段必须同时改这里 —— 那就是提醒。
	allowed := map[string]bool{
		"ID": true, "Label": true, "Title": true,
		"OneURL": true, "AllURL": true, "Batch": true,
	}
	rt := reflect.TypeOf(gateway.DailyAction{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !allowed[f.Name] {
			t.Errorf("gateway.DailyAction 多出了字段 %q —— 它会**原样下发给浏览器**。"+
				"加字段前先确认它不是上游私有状态（凭证/会话/token）；"+
				"若确认安全，把 %q 加进本测试的 allowed 表并说明理由。", f.Name, f.Name)
		}
	}

	// 反向守卫：那些字段的值里不得出现**凭证形状**的东西。
	//
	// 与上面的关键词判据不同，这里判的是"值看起来像凭证"：
	// Bearer 串、JWT、"ak=" / "sk=" 这类赋值形态。
	// 「刷新 token」这样的自然语言文案不会命中（它没有 = 也没有三个点）。
	secretish := []*regexp.Regexp{
		regexp.MustCompile(`(?i)bearer\s`),
		regexp.MustCompile(`(?i)\b(ak|sk|secret|password|cookie)\s*=`),
		regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}`), // JWT 形态
	}
	for _, p := range []gateway.Provider{newWorkbuddy(t), newCodearts(t)} {
		ext, _ := gateway.ExtOf[gateway.DailyActionExt](p)
		for _, a := range ext.DailyActions() {
			vals := []string{a.ID, a.Label, a.Title, a.OneURL, a.AllURL}
			for _, v := range vals {
				for _, re := range secretish {
					if re.MatchString(v) {
						t.Errorf("%s 的动作 %s 的字段值 %q 看起来像凭证（命中 %s）—— "+
							"每日动作会原样下发给浏览器", p.ID(), a.ID, v, re.String())
					}
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// SanitizeDailyActions 的守卫（畸形输入不得流进渲染管线）
// ---------------------------------------------------------------------------

// TestSanitizeDropsMalformed ID 非法 / Label 为空 / ID 重复都要被丢掉。
//
// # 为什么这些必须被丢掉而不是"照原样渲染"
//
// 前端的渲染管线对畸形输入没有防御 —— 它拿到什么渲染什么：
//
//	ID 非法（含引号/空格）→ `button[data-act="..."]` 选择器静默失效
//	                        表现是"按钮在但点了没反应"，极难定位
//	Label 为空           → 渲染出一个没有文字的按钮，用户不知道那是什么
//	ID 重复              → 前端按 ID 反查端点时会拿到错的那一个
func TestSanitizeDropsMalformed(t *testing.T) {
	in := []gateway.DailyAction{
		{ID: "ok", Label: "好的"},
		{ID: "Bad-Caps", Label: "大写开头"},
		{ID: "has space", Label: "有空格"},
		{ID: `has"quote`, Label: "有引号"},
		{ID: "", Label: "空 ID"},
		{ID: "nodelabel", Label: ""},
		{ID: "ok", Label: "重复 ID"},
		{ID: "second", Label: "第二个好的"},
	}
	got := gateway.SanitizeDailyActions(in)

	ids := make([]string, 0, len(got))
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	if strings.Join(ids, ",") != "ok,second" {
		t.Errorf("过滤后 = %v，want [ok second]（畸形的全丢掉，合法的保持原顺序）", ids)
	}
}

// TestSanitizeAllEmptyReturnsNil 一个都不合法时返回 nil（= 前端渲染 0 个按钮）。
//
// 这条覆盖"上游报了一个不存在的动作"这一类：正确的处理是**不显示**，
// 而不是渲染一个坏按钮。返回 nil 而不是空切片，是因为
// `ExtOf` 的零值语义与该函数的其他调用点都以 nil 表示"没有"。
func TestSanitizeAllEmptyReturnsNil(t *testing.T) {
	got := gateway.SanitizeDailyActions([]gateway.DailyAction{
		{ID: "BAD", Label: "x"},
		{ID: "ok", Label: ""},
	})
	if got != nil {
		t.Errorf("全部不合法时应返回 nil，实际 %+v", got)
	}
}

// TestValidDailyActionID 字符集边界的逐点覆盖。
func TestValidDailyActionID(t *testing.T) {
	cases := []struct {
		id string
		ok bool
	}{
		{"checkin", true},
		{"daily-gift", true},
		{"welfare", true},
		{"a1", true},
		{"", false},
		{"Checkin", false},  // 大写开头
		{"checkIn", false},  // 中间大写
		{"-checkin", false}, // 以 '-' 开头
		{"check in", false}, // 空格
		{"check/in", false}, // 斜杠（会破坏 URL 与选择器）
		{`che"ck`, false},   // 引号（会破坏 HTML 属性）
		{"check_in", false}, // 下划线不在字符集里
		{"checkin'", false}, // 单引号
		{"1checkin", false}, // 数字开头
	}
	for _, c := range cases {
		if got := gateway.ValidDailyActionID(c.id); got != c.ok {
			t.Errorf("ValidDailyActionID(%q) = %v, want %v", c.id, got, c.ok)
		}
	}
}

// TestCreditsNotADailyAction 额度刷新（credits）**不属于**每日动作。
//
// # 为什么这条要单独有
//
// 用户要统一的是"签到/福利领取"这一类**每天能领一次**的东西。
// 额度刷新只是"把余额读一遍写回池"，可以随时点、点几次都行，
// 语义上不是每日动作。把它塞进 DailyActionExt 会让这个槽位的语义糊掉 ——
// 而"槽位语义糊掉"是不可见的技术债：本次改动之后没有任何东西会提醒你。
//
// 它仍然渲染（走 CapQuotaProbe 能力位路径，见 webui.html 的 accountRow），
// 所以功能不丢；丢的只是这个错误的归类。
func TestCreditsNotADailyAction(t *testing.T) {
	for _, p := range []gateway.Provider{newWorkbuddy(t), newCodearts(t)} {
		for _, a := range actionsOf(t, p) {
			if a.ID == "credits" {
				t.Errorf("%s 把 credits 报成了每日动作 —— 额度刷新不是「每天一次」的动作，"+
					"它该走 CapQuotaProbe 能力位路径", p.ID())
			}
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 简单插入排序：集合很小，不值得引 sort
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
