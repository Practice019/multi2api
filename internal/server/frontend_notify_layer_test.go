// frontend_notify_layer_test.go —— 前端**通知层**与**操作反馈**的结构判据。
//
// # 为什么有这条（2026-11-14，真实踩坑，不是假想）
//
// 给模型目录页加「重拉」按钮时，我（Agent）自己造了一个 `.toasts` 浮动层。
// 事后才发现：本仓库 **2026-11-01 就已经统一过**通知层 —— 共享的 `#toast`
// 容器 + `.toastline` 多行堆叠（由 theme_sync 同步到所有页面）。
//
// 重复造轮的后果不是"丑"，是**行为退化**：
//
//	· 共享层有「多行堆叠 / 连续同文案合并成 ×N / 超上限折叠成『…还有 N 条』」
//	  —— 我造的那套一条都没有，批量操作会退化成"只读得到最后一条"，
//	    也就是外壳页当初那个老 bug（见 webui-tokens.html 里那段注释）。
//	· 同一个产品里出现两套通知语义，用户在两个页面看到的反馈行为不一致。
//
// 根因是**协议没写清「新页面接入通知层该照抄谁、宿主必须提供什么」** ——
// 所以本判据与 `docs/protocols/前端操作反馈协议.md` 配套存在。
// 协议负责说清怎么做，这里负责**让违反它的人立刻红**。
//
// # 判据为什么是静态的（扫源码文本），而不是浏览器里量位置
//
// 「点按钮后按钮不位移」最直接的判据当然是在 Chrome 里量 getBoundingClientRect，
// 但那要求一个跑起来的实例 + Chrome（本目录的 e2e 那一档）。
// 这里要守的是一条**更早、更便宜**的结构约束：结论不许落在行内。
// 落行内 → 列宽变化 → 位移，这条因果链在源码层就能判定，
// 而且它对每个提交都跑得起（不需要实例）。量位置那档留给 e2e。
package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// frontendPages 列出**全部**内嵌页面（webui*.html）。
//
// ⚠ 刻意扫磁盘文件而不是读 `webui.go` 的 `//go:embed` 变量清单：
// 变量清单是"已登记"的页面，而这条判据要连**没登记就放进来的页面**一起管
// （webui.go 自己的注释就在防这件事：漏登记 ⇒ 那一页静默逃过所有门禁）。
// 扫出来的集合是登记清单的超集，更严不更松。
func frontendPages(t *testing.T) []string {
	t.Helper()
	pages, err := filepath.Glob("webui*.html")
	if err != nil {
		t.Fatalf("glob webui*.html 失败: %v", err)
	}
	if len(pages) == 0 {
		// 空匹配必须失败，不能静默通过：本文件所在目录就是页面所在目录，
		// 一个都没扫到说明 cwd 变了或页面被搬走了 —— 那正是最需要红的时候。
		t.Fatal("一个 webui*.html 都没扫到 —— 判据已失效（空匹配即失败）")
	}
	return pages
}

// TestFrontendPagesWereScanned 判据自身的**非空匹配**守护。
//
// 护栏的反向验证：只证明"能抓违规"不够，还要证明"它没有悄悄失效"。
// 若哪天页面被移出本目录，上面那条 glob 会返回空集，而**所有**基于它的
// 循环断言都会"全绿"—— 那是比没有护栏更危险的假安全。
func TestFrontendPagesWereScanned(t *testing.T) {
	pages := frontendPages(t)
	want := []string{
		"webui.html", "webui-login.html", "webui-channels.html", "webui-groups.html",
		"webui-status.html", "webui-models.html", "webui-tokens.html",
	}
	have := map[string]bool{}
	for _, p := range pages {
		have[filepath.Base(p)] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("页面 %s 没被扫到 —— 判据的覆盖面已经缩水（它可能被改名/搬走）", w)
		}
	}
}

// TestFrontendHasSingleNotificationLayer **通知层只有一套**。
//
// 断言两件事，缺一不可：
//
//	① 不许自造通知容器 —— 只许共享的 `id="toast"`（主题层已经在管它的样式）；
//	② 行内结果类名不许出现 —— 「结论落在行内」会让按钮被结果文字挤着位移
//	   （2026-11-14 实测：td 是 width:1% + 右对齐，插一段文本整列就动）。
//
// ⚠ ② 的判据串取 `class="actres` 与 `data-actres` 这类**形态**而不是词根
// `actres`：后者会命中解释性注释（本文件与页面注释里都在讲这件事）。
func TestFrontendHasSingleNotificationLayer(t *testing.T) {
	// 自造通知层的形态（今天的错 + 同类变体）
	//
	// ⚠ 形态要精确到「真的在写 CSS 规则」：`.toasts` 这个宽串会命中解释性注释
	//   （注释里就是在讲这件事，比如「我在这一页自造过一套 .toasts」）。
	//   误伤比漏掉更糟 —— 判据一旦红得莫名其妙，下一次就会有人改判据而不是改代码。
	selfMade := []string{
		`id="toasts"`,
		`class="toasts"`,
		`.toasts {`,
		`.toasts{`,
		`id="noticebar"`,
		`class="floatnotice"`,
	}
	// 行内结论的形态
	inline := []string{
		`class="actres`,
		`data-actres`,
		`actionResultEl`,
	}

	for _, page := range frontendPages(t) {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", page, err)
		}
		s := string(raw)

		for _, bad := range selfMade {
			if strings.Contains(s, bad) {
				t.Errorf("%s 出现了自造的通知层 %q —— 统一用共享的 id=\"toast\"（见 docs/protocols/前端操作反馈协议.md）",
					page, bad)
			}
		}
		for _, bad := range inline {
			if strings.Contains(s, bad) {
				t.Errorf("%s 出现了行内结论 %q —— 结论落在行内会把按钮挤得位移，改走 toast()",
					page, bad)
			}
		}
	}
}

// TestFrontendLoadingButtonsKeepWidth 会随状态改文案的按钮，宽度必须固定。
//
// # 因果链（用户原话："点击按钮后不要位移"）
//
// 按钮文案在 idle / run 之间切换（"重拉" → "重拉中…"）
//   ⇒ 按钮宽度变化
//   ⇒ 同一行的内容被推挤
//   ⇒ 用户看到"按钮动了"
//
// 2026-11-14 实测：模型页那一列是 `width:1%` + 右对齐，文案一变整列就跳。
// 修法是给操作列的按钮声明 `min-width`（含最宽的那一档文案，留出余量）。
//
// # 为什么只断言「有 min-width」而不是断言具体宽度
//
// 每页的最宽文案不一样（"重拉中…" 5 字、"批量轮换中…" 7 字…），写死像素值
// 就是给每一页留一份魔法数字 —— 与本仓库「不复制常量、从源码现取」的既有做法
// 相悖。断言"有"是最小可失败的判据；宽度本身由人工按该页文案定。
//
// # 匹配口径
//
// 只认"按钮与 loading 文案**在同一行**"（`button ... 中…`）：纯状态文案
// （如"加载中…"）不算，避免误伤。
func TestFrontendLoadingButtonsKeepWidth(t *testing.T) {
	// 口径：只认「把 loading 文案**赋给按钮或按钮变量**」——
	//
	//   · `el('button', ..., '重拉中…')`          按钮构造时带 loading 文案
	//   · `btn.textContent = '轮换中…'`           按钮文案切换
	//
	// ⚠ 第一版按「页面里出现 中…」判定（宽口径），结果把整块表格的**占位文字**
	//   也算进来：webui.html 满屏 `加载中…`、status 页注释里的「熔断中…」都被误伤，
	//   于是这两页被要求去固定按钮宽度（无意义）。
	//   而第二版（只认 textContent）又漏了模型页 —— 它的文案是拼在按钮构造里的。
	//   所以两种形态都要认。
	loadingTextRe := regexp.MustCompile(
		`(textContent|button)[^\n]{0,60}中…|中…[^\n]{0,60}(textContent|button)`)
	ruleRe := regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)

	for _, page := range frontendPages(t) {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", page, err)
		}
		s := string(raw)

		if !loadingTextRe.MatchString(s) {
			continue // 没有会切换文案的按钮 ⇒ 这条判据与本页无关
		}

		hasFixed := false
		for _, m := range ruleRe.FindAllStringSubmatch(s, -1) {
			sel, props := m[1], m[2]
			if strings.Contains(sel, "button") && strings.Contains(props, "min-width") {
				hasFixed = true
				break
			}
		}
		if !hasFixed {
			t.Errorf("%s 有按钮且文案会在 idle/run 之间切换，但 CSS 里没有作用于 button 的 min-width"+
				" —— 按钮会因文案变化推挤同行内容（用户要求：操作后不漂移）", page)
		}
	}
}

// TestToastCallersProvideHostContract 调 `toast()` 的页面必须提供**宿主契约**。
//
// 契约（写在 webui-tokens.html 的实现注释里，本判据把它变成会失败的命令）：
//
//	id="toast"   通知容器
//	function toast(  实现本体（照抄共享那份，不许改结构）
//
// 为什么必须查这条：`toast()` 内部第一句就是 `if (!el) return;` ——
// **容器不存在时它是静默 no-op**。也就是说"调了 toast 但页面没提供容器"
// 这个错误的表现是"点了没反应"，而排查方向会被引到网络/后端上去。
// 这正是本仓库反复吃过的亏（错误表现指不到根因），所以要在静态层拦下来。
func TestToastCallersProvideHostContract(t *testing.T) {
	for _, page := range frontendPages(t) {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", page, err)
		}
		s := string(raw)

		calls := strings.Contains(s, "toast(") && !strings.Contains(s, "function toast(")
		defined := strings.Contains(s, "function toast(")
		container := strings.Contains(s, `id="toast"`)

		// ① 页面自己定义了 toast ⇒ 必须同时有容器（否则自己的实现也是 no-op）
		if defined && !container {
			t.Errorf("%s 定义了 toast() 但没有 id=\"toast\" 容器 —— 它会静默 no-op（点按钮看起来没反应）", page)
		}
		// ② 页面调用了 toast ⇒ 必须能解析到实现（自己定义，或走外壳的共享实现）
		if calls && !defined && !container {
			t.Errorf("%s 调用了 toast() 却既没定义它、也没有 id=\"toast\" 容器 —— 运行期 ReferenceError 或静默无反馈", page)
		}
	}
}
