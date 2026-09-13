package server

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 源码级守卫：前端不得引用**上游私有端点**
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug
//
// `/admin/credits/refresh` 是 **workbuddy 挂的私有路由**
// （internal/workbuddy/admin.go 的 routeTable）。它不是核心端点，
// 语义是"刷 workbuddy 一个上游的余额"，回执形状也是 workbuddy 自己的
// `{mode:"all", results:[{uid,status,credits,has_quota}]}`。
//
// 而 webui.html 曾经**两处**把它当成"全站额度刷新"的入口：
//
//	:2278 行内「额度」按钮  data-dayurl="/admin/credits/refresh"
//	:5574 顶部「刷新全部额度」all_url    ="/admin/credits/refresh"
//
// 后果有两层，且**都不报错**：
//
//	① 作用域错：codearts 也声明了 quota-probe（界面上有额度胶囊），
//	   但它没有这条路由 → 它那一整片账号的额度从来没被这个按钮刷新过，
//	   界面上恒为 `—`，而且完全沉默。
//	② 回执形状错：startAll 当时只认 `r.results`。core 的通用端点
//	   /admin/accounts/quota/refresh 回的是
//	   `{updated,unknown,failed,skipped,providers,accounts}` —— **没有 results**，
//	   于是新回执落进兜底 else，toast 说"已提交"而四个计数一个都没显示。
//
// 修法是把两处都改指 core 的通用端点。但**修好不等于不会复发**：
// 下一个加按钮的人会照着旁边旧代码抄（本项目已有先例：
// upstream_isolation_test.go 的 TestNoPoolWideIterationInPackage
// 就是为"第 8 个调用点"立的同款守卫）。
//
// 所以这里立一条**结构性**约束，与调用点数量无关：
//
//	webui.html 中不得出现 `/admin/credits/refresh` 字面量。
//
// # 为什么是"剥注释后"扫
//
// ⚠ 与 upstream_isolation_test.go 的教训逐字相同：判据不能只看注释。
// 本文件（以及 webui.html 里那段解释"改造前写的是什么"的注释）**必然**
// 会提到这个旧路径 —— 那是说明文字，不是代码。
// 不剥注释的话，我的说明文字会让这条守卫自己红，而红得"有理由"，
// 于是下一个人会去删注释而不是改代码：守卫就废了。
//
// # 这条路由本身不删
//
// workbuddy 的 `/admin/credits/refresh` **保留**（别的调用方可能还在用，
// 且它是 CapQuotaProbe 能力位的原生实现）。这里约束的只是
// **前端不得再引用它** —— 前端该走 core 的通用端点。
func TestWebUINoUpstreamPrivateEndpoint(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}

	// 必须真的读到内容，否则这条守卫会静默变成"永远绿灯"（fail-open）。
	// 与 upstream_isolation_test.go 的 `len(src) == 0` 检查同一条理由：
	// 一个读不到东西的守卫等于装饰品。
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}

	const needle = "/admin/credits/refresh"

	for _, lit := range webUIPrivateEndpointLiterals(stripJSComments(src), needle) {
		t.Errorf("webui.html 里出现了上游私有端点 %q（第 %d 行附近）：\n    %s\n"+
			"它只在 workbuddy 一个上游存在，被当成'全站额度刷新'用的后果是：\n"+
			"  · 别的上游（如 codearts）的额度永远刷不到，且界面完全沉默；\n"+
			"  · 回执形状是 workbuddy 私有的 {results:[…]}，与 core 的\n"+
			"    {updated,unknown,failed,skipped} 不同，改端点时必须同步改解析，\n"+
			"    否则会静默走 else、toast 说'已提交'却看不到任何真实结果。\n"+
			"请改用核心通用端点 /admin/accounts/quota/refresh（它按每个账号\n"+
			"自己的 provider 分派到各自的 gateway.QuotaExt）。",
			needle, lit.line, lit.text)
	}
}

// webUIPrivateEndpointLiterals 返回 needle 在剥离注释后的 JS/HTML 里出现的每一处。
//
// 报"每一处 + 行号"而不是布尔值，是为了让失败信息直接指向要改的那一行 ——
// 一条只说"文件里存在 X"的守卫，在一个 5800 行的 HTML 里等于让人自己 grep。
//
// ⚠ 行号必须基于**原始**源码：剥注释会改行数（注释体被替换成空行以外的东西时）。
// 所以这里对原文逐行判断，剥注释只用于"这一行的内容算不算代码"。
func webUIPrivateEndpointLiterals(src, needle string) []webUILiteral {
	var out []webUILiteral
	code := stripJSComments(src)
	// 逐行在**剥注释后的文本**里找；行号用同一份文本算（stripJSComments
	// 保留换行，见那里的实现），这样行号与原文一致。
	for i, line := range strings.Split(code, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, webUILiteral{line: i + 1, text: strings.TrimSpace(line)})
		}
	}
	if len(out) == 0 {
		// 兜底：万一 stripJSComments 把换行吃掉了（行号会失真），
		// 仍然要能报出"存在"这件事 —— 守卫宁可行号不准，也不能漏报。
		if strings.Contains(code, needle) {
			out = append(out, webUILiteral{line: -1, text: "(行号不可用：剥离注释后换行丢失)"})
		}
	}
	return out
}

type webUILiteral struct {
	line int
	text string
}

// stripJSComments 去掉 webui.html 里 JS 的注释，保留代码与换行。
//
// # 为什么必须保留换行
//
// 上面按行报行号。如果这里把注释连同换行一起吃掉，之后所有行号都会
// 整体前移 —— 失败信息会指向错误的行，比不报行号更糟（它会让人改错地方）。
//
// # 支持的三类（webui.html 实际用到的）
//
//	// 行注释        （JS 与 CSS 都用）
//	/* 块注释 */     （含跨行；JS 与 CSS 都用）
//	<!-- HTML 注释 -->
//
// ⚠ 不处理字符串里的 `//`（如 URL）：本项目历史上确有 `'http://...'`
// 这样的字面量，但它**只影响"是否把某段代码误判成注释"**，
// 而误判只会让守卫**漏报**真代码里的 needle —— 不会误报。
// 由于本守卫的判据是两个具体的路径字面量（且真实代码里它们总在
// 模板字符串/JS 字符串中，不含 `//` 序列），这个简化是可接受的。
// 需要更强解析时再引入真正的词法器，而不是在这里堆正则。
func stripJSComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	i := 0
	for i < len(src) {
		// 块注释 /* ... */（跨行，保留其中的换行数以维持行号）
		if strings.HasPrefix(src[i:], "/*") {
			j := strings.Index(src[i+2:], "*/")
			var body string
			if j < 0 {
				body = src[i:] // 未闭合：吃到结尾
				i = len(src)
			} else {
				body = src[i : i+2+j+2]
				i = i + 2 + j + 2
			}
			b.WriteString(keepNewlines(body))
			continue
		}
		// HTML 注释 <!-- ... -->
		if strings.HasPrefix(src[i:], "<!--") {
			j := strings.Index(src[i+4:], "-->")
			var body string
			if j < 0 {
				body = src[i:]
				i = len(src)
			} else {
				body = src[i : i+4+j+3]
				i = i + 4 + j + 3
			}
			b.WriteString(keepNewlines(body))
			continue
		}
		// 行注释 // ... 到行尾（换行本身留给下一轮处理）
		if strings.HasPrefix(src[i:], "//") {
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				b.WriteString(keepNewlines(src[i:]))
				i = len(src)
			} else {
				body := src[i : i+j] // 不含换行
				b.WriteString(keepNewlines(body))
				i = i + j
			}
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}

// keepNewlines 返回与原串**等量**的换行，其余字符丢弃。
//
// 这是"剥注释但保行号"的关键：注释体被换成同数量的 '\n'，
// 于是剥注释前后**每一行的行号与内容行一一对应**。
func keepNewlines(s string) string {
	n := strings.Count(s, "\n")
	if n == 0 {
		return ""
	}
	return strings.Repeat("\n", n)
}

// ---------------------------------------------------------------------------
// 反向验证：守卫必须抓得住注入的缺陷
// ---------------------------------------------------------------------------

// TestWebUIEndpointGuardCatchesInjectedDefect 反向验证上面那条守卫。
//
// # 为什么守卫本身也需要被验证
//
// 一条抓不住违规的守卫 = 装饰品。与 upstream_isolation_test.go 的
// TestGuardCatchesInjectedDefect 同一条理由：
// 用**与真实缺陷逐字同形**的样本喂给判据，确认它报违规；
// 同时确认"干净样本不报"（否则守卫会退化成永远报错的噪音）。
//
// ⚠ 这里用**当前真实的两处旧写法**当脏样本 —— 不是编的：
// 它们就是本次改动删掉的那两行，将来有人照抄回去时，
// 本测试证明守卫会红。
func TestWebUIEndpointGuardCatchesInjectedDefect(t *testing.T) {
	const needle = "/admin/credits/refresh"

	// 脏样本 1：行内按钮的 data-dayurl（本次改掉的第一处）。
	dirtyInline := `<button data-act="credits" data-uid="${esc(U)}" data-dayurl="/admin/credits/refresh" title="重新探测该账号的剩余额度">额度</button>`
	if len(webUIPrivateEndpointLiterals(stripJSComments(dirtyInline), needle)) == 0 {
		t.Fatal("守卫抓不住注入的**行内按钮**缺陷 —— 它是装饰品")
	}

	// 脏样本 2：顶部按钮的 all_url（本次改掉的第二处）。
	dirtyAll := `$('btnAllCredits').onclick = () => startAll({
      id: 'credits', label: '额度', all_url: '/admin/credits/refresh',
    });`
	if len(webUIPrivateEndpointLiterals(stripJSComments(dirtyAll), needle)) == 0 {
		t.Fatal("守卫抓不住注入的**顶部按钮**缺陷 —— 它是装饰品")
	}

	// 干净样本：改用 core 的通用端点（本次改动后的真实写法）。
	cleanInline := `<button data-act="credits" data-uid="${esc(U)}" data-dayurl="/admin/accounts/quota/refresh" title="...">额度</button>`
	if got := webUIPrivateEndpointLiterals(stripJSComments(cleanInline), needle); len(got) != 0 {
		t.Fatalf("守卫误报干净样本：%+v", got)
	}

	// 注释里的说明文字不算代码 —— 否则会让人靠删注释绕过守卫，
	// 而 webui.html 里**确实**有解释"改造前写的是 /admin/credits/refresh"
	// 的注释（本次改动新增了三处）。这三条用例同时守住"剥注释"这件事。
	for _, commented := range []string{
		`// 改造前它写死 data-dayurl="/admin/credits/refresh" —— 那是 workbuddy 的私有路由。`,
		"/* 旧的入口是 /admin/credits/refresh，回执形状是 {mode:\"all\",results:[…]} */",
		`<!-- 历史：顶部按钮曾打 /admin/credits/refresh -->`,
	} {
		if got := webUIPrivateEndpointLiterals(stripJSComments(commented), needle); len(got) != 0 {
			t.Fatalf("守卫把注释当成了代码（会让人靠改注释绕过它）：%q → %+v", commented, got)
		}
	}

	// 换行必须被保留 —— 否则行号会整体前移，失败信息指向错误的行。
	withBlock := "line1\n/* a\nb\nc */\nneedsleuth\n"
	if got := strings.Count(stripJSComments(withBlock), "\n"); got != strings.Count(withBlock, "\n") {
		t.Fatalf("剥注释改变了换行数（行号会失真）：原 %d 行 → 剥后 %d 行",
			strings.Count(withBlock, "\n"), got)
	}
	// 且块注释之后的真代码仍然会被抓到（证明剥注释没有把它一起吃掉）。
	if len(webUIPrivateEndpointLiterals(stripJSComments("/* x\ny */\n"+dirtyInline), needle)) == 0 {
		t.Fatal("剥注释把注释**之后**的真代码一起吃掉了")
	}
}

// TestWebUIQuotaButtonsUseCoreEndpoint 正面守住"两处都指向 core 通用端点"。
//
// # 为什么光有"禁止旧字面量"不够
//
// 上面那条只保证"不再引用私有端点"。把两处**删掉**（按钮不渲染了）
// 同样能让它绿 —— 那不是修复，是功能消失。
// 所以配一条正面断言：顶部按钮与行内按钮必须**各自**指向
// /admin/accounts/quota/refresh。
//
// 判据用剥注释后的源码做字符串匹配，与 webui.html 的实际写法对齐：
//
//	· 行内：data-dayurl="/admin/accounts/quota/refresh"
//	· 顶部：all_url: '/admin/accounts/quota/refresh'
//
// ⚠ 只匹配端点是不够的 —— 还要确认**行内那一处确实挂在 data-dayurl 上**。
// 否则把顶部那行的端点复制到注释旁边也能绿。
func TestWebUIQuotaButtonsUseCoreEndpoint(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	// 必须剥注释：webui.html 的解释性注释里也写了这个端点
	//（"请改用核心通用端点 /admin/accounts/quota/refresh"），
	// 不剥的话"注释里提一句"就能让这条正面断言蒙对。
	code := stripJSComments(src)

	const endpoint = "/admin/accounts/quota/refresh"

	// 1) 行内按钮：端点必须出现在 data-dayurl 的值上。
	inline := `data-dayurl="` + endpoint + `"`
	if !strings.Contains(code, inline) {
		t.Errorf("行内「额度」按钮没有指向核心通用端点：webui.html 里找不到 %s\n"+
			"（它是本 bug 的第一处：写死 workbuddy 私有端点，导致别的上游刷不到）", inline)
	}

	// 2) 顶部按钮：端点必须出现在 all_url 的值上。
	allURL := `all_url: '` + endpoint + `'`
	if !strings.Contains(code, allURL) {
		t.Errorf("顶部「刷新全部额度」没有指向核心通用端点：webui.html 里找不到 %s\n"+
			"（它是本 bug 的第二处：私有端点回执没有 results，startAll 会静默走 else）", allURL)
	}

	// 3) 正面确认这两处**确实**是按钮相关的代码，而不是随便哪一行。
	//    用极小范围内的上下文匹配，避免"端点漂到别处也算数"。
	if !strings.Contains(code, `hasCap(pid, 'quota-probe')`) {
		t.Error("找不到行内额度按钮的能力位判据 hasCap(pid, 'quota-probe') —— " +
			"按钮可能被整个删掉了（那不是修复）")
	}
	if !strings.Contains(code, "$('btnAllCredits').onclick") {
		t.Error("找不到顶部按钮的绑定 $('btnAllCredits').onclick —— 按钮可能被删掉了")
	}
}

// ---------------------------------------------------------------------------
// 回执分支守卫：startAll 必须认 core 的额度回执
// ---------------------------------------------------------------------------

// TestStartAllHandlesQuotaReceipt 守住"第二形态"：新回执不得落进兜底 else。
//
// # 为什么这条要单独守（而不是只靠浏览器实测）
//
// 浏览器实测能证明"今天是对的"，但它需要跑实例、跑 CDP，
// 不会在**每次** go test 时执行 —— 于是删掉那条分支的人不会立刻看到红。
//
// 判据（三条，都是"错误实现必然失败"的）：
//
//  1. startAll 里存在 `typeof r.updated === 'number'` 分支
//  2. 该分支排在 `r.results` 分支**之前**
//     —— 顺序反了就等于给未来埋雷：core 若给额度回执补上 results，
//     旧分支会先截胡，本 bug 以同款形态复发。
//  3. 该分支真的调用了 quotaToast（而不是又一句"已提交"）
//
// ⚠ 判据 2 用**下标比较**而不是"包含"：只包含不能表达顺序，
// 而顺序正是这条守卫的全部意义。
func TestStartAllHandlesQuotaReceipt(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSComments(src)

	// 先定位 startAll 的函数体，避免匹配到别处的同名片段
	//（页面里 `r.results` 还有 codearts 福利那条用法）。
	body, ok := extractJSFunction(code, "async function startAll(")
	if !ok {
		t.Fatal("找不到 startAll 函数 —— 守卫无法定位（守卫失效，fail-open）")
	}

	quotaMark := "typeof r.updated === 'number'"
	resultMark := "r.results"

	iQuota := strings.Index(body, quotaMark)
	iResult := strings.Index(body, resultMark)

	if iQuota < 0 {
		t.Fatalf("startAll 里没有 %q 分支 —— core 的额度回执会落进兜底 else，"+
			"toast 说'已提交'而四个计数一个都不显示（本 bug 的第二形态）", quotaMark)
	}
	if iResult < 0 {
		// 旧的 results 分支被删了也要报 —— 它是 codearts/workbuddy 的
		// 签到/福利回执的渲染路径，删掉会让那些按钮重新变哑。
		t.Fatalf("startAll 里没有 %q 分支 —— 旧回执形状的兼容被删了", resultMark)
	}
	if iQuota > iResult {
		t.Errorf("startAll 里额度分支排在 %q 之后（下标 %d > %d）—— "+
			"core 若给额度回执补上 results，旧分支会先截胡，本 bug 以同款形态复发。"+
			"新分支必须排在 r.results 之前。", resultMark, iQuota, iResult)
	}
	if !strings.Contains(body, "quotaToast(") {
		t.Error("startAll 的额度分支没有调用 quotaToast —— " +
			"skipped（某个上游整片没被刷到）就不会被说出来，而那正是本 bug 的形态")
	}
}

// TestQuotaToastReportsSkipped 守住 toast 文案必须**显式**说出 skipped。
//
// # 为什么单独立一条
//
// 回执把结果分成四类，其中三类不是错误。只报"已更新 N"会让
// unknown/failed/skipped 全部隐形 —— 而 skipped 意味着
// **某个上游整片账号没被刷到**（正是本 bug：codearts 从来没被刷过）。
// 旧按钮对这种情况完全沉默，所以这一条不是文案偏好，是判据。
func TestQuotaToastReportsSkipped(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSComments(src)

	body, ok := extractJSFunction(code, "function quotaToast(")
	if !ok {
		t.Fatal("找不到 quotaToast 函数 —— core 额度回执没有可读的渲染器（守卫失效）")
	}

	for _, need := range []string{
		"r.updated",   // 已更新
		"r.unknown",   // 上游如实回答"不知道"（正常，不该隐形）
		"r.failed",    // 真错误
		"r.skipped",   // ⚠ 本 bug 的形态，必须显式出现
		"r.providers", // 本次真正被问过的上游
	} {
		if !strings.Contains(body, need) {
			t.Errorf("quotaToast 没有读 %s —— 该计数在界面上不可见", need)
		}
	}

	// bad 只由 failed 决定：把 unknown/skipped 也标红会让"上游不报额度"
	// 看起来像故障，从而被误修（与后端 refreshQuotas 的分开计数同一条理由）。
	if !strings.Contains(body, "r.failed || 0") {
		t.Error("quotaToast 的 bad 判据看起来不是只取 failed —— " +
			"unknown/skipped 被标红会让正常情况看起来像故障")
	}
	if strings.Contains(body, "r.skipped || 0) > 0") && !strings.Contains(body, "const bad = (r.failed || 0) > 0") {
		t.Error("quotaToast 把 skipped 也算进了 bad —— skipped 不是错误，是'没接额度'")
	}
}

// extractJSFunction 从 startMarker 处截出到下一个顶层 `}` 的近似函数体。
//
// ⚠ 这是**近似**：不解析字符串/模板串里的花括号，只按花括号计数。
// 对本文件里这两个函数够用（它们的模板串里没有裸 `{`），
// 而且失配时的表现是"找不到"或"截短了" —— 那会让上面的断言**报红**
// 而不是静默绿（守卫失败方向是安全的那一侧）。
func extractJSFunction(code, startMarker string) (string, bool) {
	i := strings.Index(code, startMarker)
	if i < 0 {
		return "", false
	}
	depth := 0
	started := false
	for j := i; j < len(code); j++ {
		switch code[j] {
		case '{':
			depth++
			started = true
		case '}':
			depth--
			if started && depth == 0 {
				return code[i : j+1], true
			}
		}
	}
	return code[i:], true
}
