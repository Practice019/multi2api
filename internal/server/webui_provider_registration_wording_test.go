package server

import (
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 源码级守卫：账号池分组标题必须区分两种"上游未知"
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug（用户报的，已在运行实例上证实）
//
// 实例 127.0.0.1:7863 加载的 config **没有启用 codearts**，但账号池是持久化的、
// 里面还留着 2 个 provider="codearts" 的号（/admin/accounts 里
// provider_inferred=false —— 即这些号**明确**带着 provider 标签），
// 而 /admin/ui/manifest 的 providers 只有 workbuddy。
//
// 于是 accountGroupRow 把这一组渲染成：
//
//	codearts — 2 个账号 · 按默认上游推断（未在 manifest 里注册）
//
// 两处事实错误：
//
//	① 「按默认上游推断」是假的 —— 这两个号明确打了 provider=codearts，
//	   同一张表里它们那两行的「上游」列正显示 codearts（没有 `?` 角标，
//	   见 accountRow 的 a.provider_inferred 分支）。
//	   组标题说"推断"、行内说"codearts"：**同一个表里两处渲染点互相矛盾**，
//	   正是本仓库注释里反复警告的漂移形态。
//	② 真正发生的事是"这个上游**本次启动没有注册/未启用**，账号是从持久化池
//	   恢复出来的"。旧文案把它说成"没在 manifest 里注册"，而且与 meta 一起
//	   把同一件事说了两遍。
//
// # 为什么必须写成"源码级守卫测试"而不是只靠浏览器实测
//
// 浏览器实测（tests/frontend/ 里那套 Node 套件 + 真 Chrome）能证明"今天是对的"，
// 但它要起实例、要跑 CDP，不会在**每次** `go test` 时执行 —— 于是把这段区分
// 逻辑删掉的人不会立刻看到红。下面的断言只看 webui.html 的源码，每次 go test 都跑。
//
// # 判据为什么剥注释
//
// ⚠ 与 webui_endpoint_guard_test.go 的教训逐字相同：webui.html 里那段解释
// "修复前渲染成什么、旧措辞是哪一句"的注释**必然**会提到旧文案 —— 那是说明文字，
// 不是代码。不剥注释的话，说明文字会让这条守卫自己红，而红得"有理由"，
// 于是下一个人会去删注释而不是改代码：守卫就废了。
// ∴ 断言 3 判的是**剥注释后**的函数体（stripJSComments 保留换行，行号不失真）。
// 作为补偿，另配一条更严的子断言：原文件里若仍出现这句旧措辞，它必须只落在
// 以 `//` 开头的注释行上（即：绝不在会被渲染出去的代码里）。

// removedGroupNoteWording 是本次修复从组标题里**删掉**的那句旧措辞。
//
// 提成包级常量是为了让"断言 3"（不许它回到代码里）与"守卫反向验证"
// （回退样本必须被它抓到）用**同一个字符串**，避免两处各写一遍而漂移。
const removedGroupNoteWording = "未在 manifest 里注册"

func TestWebUIAccountGroupWordingDistinguishesUnknownUpstreams(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	// 必须真的读到内容，否则这条守卫会静默变成"永远绿灯"（fail-open）。
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}

	code := stripJSComments(src)
	const marker = "function accountGroupRow("
	body, ok := extractJSFunction(code, marker)
	if !ok {
		// 切不出函数体时**必须红**，不能"找不到就跳过"：
		// 跳过等于这条守卫可以被一次重命名轻易关掉。
		t.Fatal("找不到 accountGroupRow 函数 —— 守卫无法定位（fail-open）；" +
			"如果它被改名/拆分了，本守卫的判据也要跟着改，而不是让它空转")
	}
	// fail-closed 自检：确认切出来的**确实**是那个函数，而不是切短了或串到别处。
	// 只有结构标记（ACCT_COLS / data-acctgroup 是 accountGroupRow 独有的）
	// 齐了才继续判定 —— 否则后面的断言会在错误的文本上"绿"。
	if !strings.Contains(body, "ACCT_COLS") || !strings.Contains(body, "data-acctgroup") {
		t.Fatalf("切出来的函数体不像 accountGroupRow（缺 ACCT_COLS / data-acctgroup，共 %d 字节）—— "+
			"切分逻辑已失效，本守卫此时无论绿红都不可信", len(body))
	}
	if len(body) > 20000 || strings.Contains(body, "function accountEmptyNote(") {
		t.Fatalf("切出来的函数体过大或串到了相邻函数（共 %d 字节）—— 切分逻辑已失效", len(body))
	}

	// 四条断言全部由同一个判据函数给出（它同时被反向验证测试复用）。
	for _, p := range providerGroupWordingProblems(body) {
		t.Errorf("%s", p)
	}

	// ---- ③b 更严的子断言：旧措辞只允许留在注释里 --------------------------
	//
	// 断言 3 判的是剥注释后的代码。这里再把"注释里提一句"这条退路也钉住：
	// 原文件里若仍有这句措辞，它必须落在以 `//` 开头的行上。
	// 破了会怎样：说明这句措辞又回到了**会被渲染出去**的字符串里，
	// 组标题会重新对一个有明确 provider 标签的上游说它"没注册"。
	const removed = removedGroupNoteWording
	iBody := strings.Index(code, marker)
	startLine := strings.Count(code[:iBody], "\n") + 1
	endLine := startLine + strings.Count(body, "\n")
	rawLines := strings.Split(src, "\n")
	for ln := startLine; ln <= endLine && ln-1 < len(rawLines); ln++ {
		line := rawLines[ln-1]
		if !strings.Contains(line, removed) {
			continue
		}
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if !strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "*") {
			t.Errorf("webui.html 第 %d 行在**代码行**上出现了 %q：\n    %s\n"+
				"这条措辞必须只作为说明文字留在注释里 —— 一旦回到会被渲染的字符串里，"+
				"组标题就会重新对一个明确打了 provider 标签的上游说它「没在 manifest 里注册」，"+
				"而账号行那一列正显示着它的真实 provider。", ln, removed, trimmed)
		}
	}
}

// providerGroupWordingProblems 判定一段 accountGroupRow 的**函数体**
// （必须已剥注释）是否违反文案约束，返回每一条违规的说明；全绿时返回 nil。
//
// 抽成纯函数是为了让"守卫本身也被验证"成为可能：反向测试
// （TestWebUIProviderRegistrationWordingGuardCatchesRevertedCode）把**修复前的
// 真实函数体**喂进来，必须报出违规 —— 抓不住回退的守卫是装饰品。
//
// 四条断言各自对应一条真实会发生的回退：
//
//	① 判据 g.provider === UNLABELED_PROVIDER 消失（或改成恒真/恒假）
//	② '按默认上游推断' 不再只属于兜底分支（写死、或两个分支输出同一句）
//	③ 旧措辞"未在 manifest 里注册"回到代码里
//	④ "有明确 provider、但本次启动未注册它"这条路径的专属文案缺失，
//	   或那条文案自己又说「推断」
func providerGroupWordingProblems(body string) []string {
	var problems []string
	if strings.TrimSpace(body) == "" {
		return []string{"传入的 accountGroupRow 函数体是空的 —— 判据失效（fail-open），守卫变成装饰品"}
	}

	// ---- ① 必须存在"是不是兜底组"这条判据 --------------------------------
	const cond = "g.provider === UNLABELED_PROVIDER"
	iCond := strings.Index(body, cond)
	if iCond < 0 {
		problems = append(problems, "accountGroupRow 里找不到判据 "+cond+" —— "+
			"两种『上游未知』（① 兜底组：账号连 provider 字段都没有；"+
			"② 有明确 provider、但本次启动没启用它）又被混成一句话说。"+
			"破了会怎样：一个明确打了 provider 标签的上游（provider_inferred=false）"+
			"会在组标题里被说成「按默认上游推断」，而同一张表里它那几行的「上游」列"+
			"正显示着真实 provider —— 组标题与行内自相矛盾（本次实测事故）。")
	}

	// ---- ② 兜底组那句文案：恰好一次，且落在上面那条判据的真分支里 ---------
	const fallback = "按默认上游推断"
	lits := jsStringLiterals(body)
	fbPos := -1
	nFallback := 0
	for i, l := range lits {
		if strings.TrimSpace(l.text) == fallback {
			nFallback++
			fbPos = i
		}
	}
	if nFallback == 0 {
		problems = append(problems, "找不到兜底组文案「"+fallback+"」—— "+
			"兜底组（账号根本没带 provider 字段）会连一句「这个号没有归属」都不说。"+
			"破了会怎样：用户看到一组没有归属的账号却不知道它们是从哪来的。")
	}
	if nFallback > 1 {
		problems = append(problems, fmt.Sprintf("「%s」在函数体里出现了 %d 次 —— 它只允许出现在"+
			"兜底分组那一个分支里。破了会怎样：多出来的那处几乎必然是在替"+
			"『有明确 provider、但本次启动未启用』的上游说「推断」，"+
			"那正是本次实测事故的原文案（组标题说「推断」、行内说「codearts」）。",
			fallback, nFallback))
	}

	if nFallback == 1 {
		start := lits[fbPos].start
		switch {
		case iCond >= 0 && start < iCond:
			problems = append(problems, "「"+fallback+"」出现在判据 "+cond+" **之前** —— "+
				"它没有被判据管辖。破了会怎样：分支顺序一变，"+
				"有明确 provider 的上游就会落进这句文案。")
		default:
			if iCond >= 0 {
				between := body[iCond:start]
				// 两者之间必须是"三目的真分支"：有 `?`、且没有语句结束符。
				if !strings.Contains(between, "?") || strings.Contains(between, ";") {
					problems = append(problems, fmt.Sprintf("「%s」不在 %s 这个三目的真分支里"+
						"（两者之间是 %q）—— 判据与文案脱钩。破了会怎样：判据还在，"+
						"但改判据不再牵动文案，回退时只看代码看不出来。",
						fallback, cond, between))
				}
			}
			if fbPos+1 >= len(lits) {
				problems = append(problems, "兜底分支之后再没有别的文案 —— "+
					"『有明确 provider、但本次启动未注册它』这条路径没有专属措辞，"+
					"它会落进兜底那句「"+fallback+"」。破了会怎样：本次实测事故原样复发。")
			} else {
				next := lits[fbPos+1]
				if iCond >= 0 && strings.Contains(body[start:next.start], ";") {
					problems = append(problems, "兜底文案与紧随其后的那句文案不在同一个三目里"+
						"（中间出现了语句结束符）—— 『有明确 provider 但未注册』"+
						"这条路径没有与判据绑定的专属措辞。")
				}
				// ---- ④ 非兜底路径的专属文案必须"说未注册/未启用"且"不说推断" ----
				txt := next.text
				if !strings.Contains(txt, "未注册") && !strings.Contains(txt, "未启用") {
					problems = append(problems, fmt.Sprintf("『有明确 provider、但本次启动没注册它』"+
						"这条路径的文案是 %q —— 它没有说出「未注册/未启用」。破了会怎样："+
						"用户看不出真正发生的事是『这个上游本次没启用，账号来自持久化池』，"+
						"反而会以为是账号本身有问题（本次实测事故里那两个 codearts 号就是这样被误读的）。",
						txt))
				}
				if strings.Contains(txt, "推断") {
					problems = append(problems, fmt.Sprintf("『有明确 provider、但本次启动没注册它』"+
						"这条路径的文案里出现了「推断」（%q）—— 这些号明确打了 provider 标签"+
						"（/admin/accounts 的 provider_inferred=false），说「推断」是**假话**，"+
						"且与账号行的上游列自相矛盾（组标题说「推断」、行内说「codearts」）。",
						txt))
				}
			}
		}
	}

	// ---- ③ 组标题里不得再有旧措辞 -----------------------------------------
	//
	// 两种情形下它都是错的：与 meta 的「未注册」重复说同一件事；
	// 而在兜底组上更是**没有意义** —— 兜底组本来就不属于任何上游，
	// 「没在 manifest 里注册」会被读成「它本该注册而漏了」。
	if strings.Contains(body, removedGroupNoteWording) {
		problems = append(problems, "webui.html 的 accountGroupRow 函数体（剥注释后）里仍有 "+
			fmt.Sprintf("%q", removedGroupNoteWording)+
			" —— 本次修复已把它从组标题里删掉：有明确 provider 时它与 meta 的「未注册」"+
			"重复说同一件事；在兜底组上更没有意义（兜底组本来就不属于任何上游）。"+
			"破了会怎样：同一行里关于『这个上游注册了没有』又出现两句措辞，"+
			"以后改一处就会漂。")
	}

	return problems
}

// ---------------------------------------------------------------------------
// 反向验证：守卫必须抓得住"回退到修复前"的真实函数体
// ---------------------------------------------------------------------------

// TestWebUIProviderRegistrationWordingGuardCatchesRevertedCode 用**修复前的真实
// 代码形态**喂给上面的判据，确认它报违规；同时确认干净样本不报（否则守卫会退化成噪音）。
//
// # 为什么守卫本身也需要被验证
//
// 一条抓不住回退的守卫 = 装饰品。与 webui_endpoint_guard_test.go 的
// TestWebUIEndpointGuardCatchesInjectedDefect 同一条理由：
// 用与真实缺陷逐字同形的样本喂给判据，确认它报违规。
//
// ⚠ 两个脏样本都不是编的，它们正是本次改动要消灭的两种形态：
//
//	脏样本 1：修复前的函数体（known + 写死的 '按默认上游推断'）
//	脏样本 2：保留了判据、却把两个分支写成同一句话（判据形同恒真）
func TestWebUIProviderRegistrationWordingGuardCatchesRevertedCode(t *testing.T) {
	// 脏样本 1：修复前 webui.html 里 accountGroupRow 的真实形状。
	// 判据（g.provider === UNLABELED_PROVIDER）不存在，非兜底路径与兜底路径
	// 共用同一句「按默认上游推断」，且 known 又拼了一句「（未在 manifest 里注册）」。
	const dirtyBeforeFix = "" +
		"    const def = '';\n" +
		"    const known = info ? '' : '（未在 manifest 里注册）';\n" +
		"    const caps = info && Array.isArray(info.capabilities) ? info.capabilities.length : 0;\n" +
		"    const meta = info\n" +
		"      ? `${caps} 项能力`\n" +
		"      : '按默认上游推断';\n" +
		"    const n = g.accounts.length;\n" +
		"    return '<tr data-acctgroup=\"...\" colspan=\"' + ACCT_COLS + '\">'\n" +
		"      + n + ' 个账号 · ' + meta + def + known;\n"

	got := providerGroupWordingProblems(dirtyBeforeFix)
	if len(got) == 0 {
		t.Fatal("守卫抓不住『回退到修复前的函数体』—— 它是装饰品（本用例证明判据是假绿的）")
	}
	joined := strings.Join(got, "\n")
	for _, need := range []string{
		"UNLABELED_PROVIDER",    // ① 判据缺失被报出
		removedGroupNoteWording, // ③ 旧措辞回来了被报出
	} {
		if !strings.Contains(joined, need) {
			t.Errorf("回退样本没有被报出 %q —— 这条违规漏网了。守卫输出：\n%s", need, joined)
		}
	}
	if !strings.Contains(joined, "未注册") {
		t.Errorf("回退样本没有被报出『非兜底路径缺「未注册/未启用」文案』—— "+
			"有明确 provider 的上游仍会被说成「推断」。守卫输出：\n%s", joined)
	}

	// 脏样本 2：判据还在，但两个分支输出**同一句话** ——
	// 判据形同恒真，实际渲染与修复前完全一样。
	const dirtyBothBranchesSame = "" +
		"    const def = '';\n" +
		"    const caps = 4;\n" +
		"    const meta = info\n" +
		"      ? `${caps} 项能力`\n" +
		"      : (g.provider === UNLABELED_PROVIDER ? '按默认上游推断' : '按默认上游推断');\n"

	got2 := providerGroupWordingProblems(dirtyBothBranchesSame)
	if len(got2) == 0 {
		t.Fatal("守卫抓不住『判据恒真』（两个分支同一句话）—— 它是装饰品")
	}

	// 干净样本 = 修复后的真实形状：判据在，兜底分支说「按默认上游推断」，
	// 非兜底分支如实说"本次启动未注册该上游…"，且没有 known 那句旧措辞。
	const clean = "" +
		"    const def = '';\n" +
		"    const caps = 4;\n" +
		"    const meta = info\n" +
		"      ? `${caps} 项能力`\n" +
		"      : (g.provider === UNLABELED_PROVIDER\n" +
		"        ? '按默认上游推断'\n" +
		"        : '本次启动未注册该上游（config 未启用，账号来自持久化池）');\n" +
		"    const n = g.accounts.length;\n" +
		"    return '<tr data-acctgroup=\"...\" colspan=\"' + ACCT_COLS + '\">' + n + ' 个账号 · ' + meta + def;\n"

	if gotClean := providerGroupWordingProblems(clean); len(gotClean) != 0 {
		t.Fatalf("守卫对修复后的写法误报（会变成噪音，让人不再看它的输出）：\n%s",
			strings.Join(gotClean, "\n"))
	}
}

// jsStringLit 一个单引号字符串字面量及其在函数体里的**绝对下标**。
//
// 记下标（而不是只记内容）是为了判"这句文案落在哪个分支里"：
// 分支关系靠位置表达，只比对内容无法说明"它在不在那个三目里"。
type jsStringLit struct {
	start int
	text  string
}

// jsStringLiterals 扫出一段 JS 代码里的单引号字符串字面量（含位置）。
//
// # 为什么只认单引号
//
// webui.html 的约定是：模板串用反引号、HTML 属性用双引号、
// 需要按"这句话到底是哪一句"来判定的文案用单引号。
// 本守卫要链的正是**单引号文案之间的分支关系**，所以只扫单引号。
//
// ⚠ 这是**近似**扫描（不处理正则字面量）。失配时的表现是"少认一个字面量"
// ——那会让上面的断言报红/或找不到那句文案而报红，不会静默变绿
// （守卫的失败方向落在安全的那一侧）。
func jsStringLiterals(s string) []jsStringLit {
	var out []jsStringLit
	for i := 0; i < len(s); i++ {
		if s[i] != '\'' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] != '\'' {
			if s[j] == '\\' {
				j++ // 跳过被转义的字符（\' \n 等）
			}
			j++
		}
		if j >= len(s) {
			break // 未闭合：后面的内容不可能是安全的字面量
		}
		out = append(out, jsStringLit{start: i, text: s[i+1 : j]})
		i = j
	}
	return out
}
