package server

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 源码级守卫：模型 id 展示口径统一为**完整 id**（`provider/xxx`）
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug（用户原话）
//
//	「可用模型重复了，很多，我希望是这样的，workbuddy/xxx 或者 codearts/xxx
//	 这样的形式，统一，不要重复」
//	「对话测试里面的列表同理」
//
// 改之前，同一份 webui.html 里有**三处口径不一致**，而且都不报错：
//
//	① groupModelsByOwner 把 `provider/xxx` 拆成 prefix + bare，归属用 prefix，
//	   **展示却取裸名**（`g.items.push({ id: bare, fullId: m.id, … })`）。
//	   → chip 上显示 `glm-5.2`，而旁边别的上游显示 `provider/xxx`：不统一。
//	② renderModelGroup 读的就是 items 里的 id → 于是①的裸名直接落到用户眼里。
//	③ renderModels 的对话测试下拉**故意不去重**（源码注释写着"用户要求保留"），
//	   与①的裸名口径叠在一起 → 同一个模型在列表里出现两次（裸名 + 带前缀名）。
//
// 同期另一路在改**后端**（`/v1/models` 只发 `provider/model`，不再发默认上游的
// 裸名副本）。本守卫守的是**前端展示口径**：不论后端发什么形态，前端都统一显示
// 完整 id，并且**倍率标记不能因此丢失**。
//
// # 为什么倍率标记会丢（这条错的实测记录）
//
// `tests/frontend/diag_multiplier_mismatch.js` 的结论是
// 「倍率表用的是裸名，而面板显示 provider/model 形态 → id 对不上」。
// `modelMultipliers` 来自 `/admin/models/preview`，键是上游目录里的**裸 id**
// （那是后端契约，前端不改它）。一旦展示 id 变成 `workbuddy/glm-5.2`，
// 还拿它直接查表就必然全部落空 → 满屏 `x无`（把"没配倍率"错报成事实）。
//
// ∴ 必须有且只有一处归一 `multKeyOf(id)`（去掉 `provider/` 前缀），
// 且 `multTag` / `multTagPlain` **两个查询点**都要经由它取键。
//
// # 为什么写成源码级守卫而不是只靠浏览器实测
//
// 浏览器实测（tests/frontend/ 那套 Node + 真 Chrome）能证明"今天是对的"，
// 但它要起实例、要跑 CDP，不会在**每次** go test 时执行 —— 于是把这段口径
// 改回去的人不会立刻看到红。下面的断言只看 webui.html 的源码，每次 go test 都跑。
//
// # 判据为什么剥注释
//
// ⚠ 与 webui_endpoint_guard_test.go 的教训逐字相同：本文件与 webui.html 里
// 解释"改造前取的是裸名"的注释**必然**会提到旧写法（`id: bare` / `modelMultipliers[id]`）。
// 那是说明文字，不是代码。不剥注释的话，说明文字会让守卫自己红，而红得"有理由"，
// 于是下一个人会去删注释而不是改代码：守卫就废了。
// ∴ 所有判据都跑在剥注释后的结果上（剥法保留换行，行号不失真）。
//
// ⚠ **不用** webui_endpoint_guard_test.go 里那个 stripJSComments：
// 它的注释里写明了"不处理字符串里的 `//`"，而 webui.html 里确有
// `'http://…'` 这类字面量 —— 实测它的输出会把 `function multTag(` 与
// `function renderModels(` 两行**整段吃掉**（输出 153336 字节 vs 原文 339068），
// 于是本守卫会以"找不到函数"的样子报红，红得与服务端无关。
// 这个坑不修在那个文件里（那是另一路 agent 的领地），本文件自带
// stripJSCommentsDeep：跳过 ' " ` 三种字面量，只在**真注释**上生效。
//
// # 守卫本身也被验证（防止"假绿"）
//
// 每条判据都抽成**纯函数**，好让 TestWebUIModelIDsUnifiedGuardCatchesRevertedCode
// 把**回退前的真实写法**喂进去，确认它报违规；同时确认干净样本不报
// （否则守卫会退化成永远报错的噪音）。

// stripJSCommentsDeep 去掉 webui.html 里真正的注释，保留代码、字面量与换行。
//
// # 与 stripJSComments 的区别
//
// 这里能识别单引号、双引号、反引号三种字面量：字面量内部原样拷贝
// （其中的 `//` 与 `/*` **不是**注释），因此不会像 stripJSComments 那样
// 把字符串里的 `//` 当成注释、连带吃掉后面成片的真代码。
//
// # 保留换行
//
// 注释体被换成**等量**换行，所以剥注释前后行号一一对应 ——
// 失败信息里的行号才能指向要改的那一行。
//
// ⚠ 仍是**近似**：不识别正则字面量（本文件的目标函数里没有含 `//`/`/*` 的正则）。
// 失配时的表现是"某段代码被当注释吃掉" → 下面的 extractJSFunction 找不到函数
// → 守卫**报红**（失败方向落在安全的那一侧，不会静默变绿）。
func stripJSCommentsDeep(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		switch {
		// 块注释 /* … */（跨行；保留其中的换行数）
		case c == '/' && i+1 < n && src[i+1] == '*':
			j := strings.Index(src[i+2:], "*/")
			body := ""
			if j < 0 {
				body, i = src[i:], n // 未闭合：吃到结尾
			} else {
				body, i = src[i:i+2+j+2], i+2+j+2
			}
			b.WriteString(newlinesOnly(body))
		// 行注释 // … 到行尾（换行留给下一轮）
		case c == '/' && i+1 < n && src[i+1] == '/':
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				i = n
			} else {
				i += j
			}
		// HTML 注释 <!-- … -->
		case strings.HasPrefix(src[i:], "<!--"):
			j := strings.Index(src[i+4:], "-->")
			body := ""
			if j < 0 {
				body, i = src[i:], n
			} else {
				body, i = src[i:i+4+j+3], i+4+j+3
			}
			b.WriteString(newlinesOnly(body))
		// 字面量：整体原样拷贝，内部的 // 与 /* 一律不当注释
		case c == '\'' || c == '"' || c == '`':
			b.WriteByte(c)
			i++
			for i < n {
				if src[i] == '\\' && i+1 < n { // 转义：连同下一个字符一起拷
					b.WriteByte(src[i])
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				if src[i] == c { // 收尾引号
					b.WriteByte(c)
					i++
					break
				}
				b.WriteByte(src[i])
				i++
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// newlinesOnly 返回与原串**等量**的换行，其余字符丢弃（保住行号）。
func newlinesOnly(s string) string {
	n := strings.Count(s, "\n")
	if n == 0 {
		return ""
	}
	return strings.Repeat("\n", n)
}

// --------------------------------------------------------------------------
// 判据组 1：chip 区展示完整 id，去重仍按裸名
// --------------------------------------------------------------------------

// TestWebUIModelIDsUnifiedChipsShowFullID 守住"可用模型"面板的 chip 区。
//
// 判据（都在 groupModelsByOwner 的函数体上）：
//
//	① 不得再出现 `id: bare`（旧写法：把裸名当展示 id）
//	② items 里必须放进完整 id：`id: m.id`，且 `fullId: m.id`
//	   （data-id / title 与展示文本取同一个值）
//	③ 组内去重仍按**裸名**（`g.seen.has(bare)` / `g.seen.add(bare)`）
//	④ renderModelGroup 渲染的确实是完整 id，三处口径一致
func TestWebUIModelIDsUnifiedChipsShowFullID(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("webui.html 读到了空内容 —— 守卫失效（fail-open）")
	}
	code := stripJSCommentsDeep(src)

	const marker = "function groupModelsByOwner("
	body, ok := extractJSFunction(code, marker)
	if !ok {
		// 切不出函数体时必须红，不能"找不到就跳过"：
		// 跳过等于这条守卫可以被一次重命名轻易关掉。
		t.Fatal("找不到 groupModelsByOwner 函数 —— 守卫无法定位（fail-open）；" +
			"如果它被改名/拆分了，本守卫的判据也要跟着改，而不是让它空转")
	}
	// fail-closed 自检：确认切出来的**确实**是那个函数。
	if !strings.Contains(body, "g.items.push(") || !strings.Contains(body, "byOwner") {
		t.Fatalf("切出来的函数体不像 groupModelsByOwner（缺 g.items.push / byOwner，共 %d 字节）—— "+
			"切分逻辑已失效，本守卫此时无论绿红都不可信", len(body))
	}
	if len(body) > 6000 || strings.Contains(body, "function renderModelGroup(") {
		t.Fatalf("切出来的函数体过大或串到了相邻函数（共 %d 字节）—— 切分逻辑已失效", len(body))
	}

	for _, p := range modelChipFullIDProblems(body) {
		t.Errorf("%s", p)
	}

	// ---- ④ renderModelGroup：确认它渲染的就是完整 id -------------------------
	//
	// 破了会怎样：items 里放的是完整 id，渲染却读了另一个字段（或反过来），
	// 于是 chip 文本、data-id、title 三者不一致 —— data-id 是给"点击复制/定位"
	// 用的，文本与它不符时用户复制的和看到的是两个东西。
	rgBody, ok := extractJSFunction(code, "function renderModelGroup(")
	if !ok {
		t.Fatal("找不到 renderModelGroup 函数 —— 无法确认 chip 渲染的 id 口径（守卫失效）")
	}
	for _, need := range []string{
		`data-id="${esc(m.fullId)}"`, // 与 items 的完整 id 同源
		`title="${esc(m.fullId)}"`,   // 悬停提示也必须是完整 id
		`esc(m.id)}${multTag(m.id)}`, // 展示文本 + 倍率标记挂在同一个 id 上
	} {
		if !strings.Contains(rgBody, need) {
			t.Errorf("renderModelGroup 里找不到 %s —— chip 的 id 口径与 items 不一致。\n"+
				"    破了会怎样：chip 文本可能显示裸名而 data-id/title 是完整 id（或反过来），"+
				"用户看到的与复制到的不是同一个东西；倍率标记也可能挂到查不到表的 id 上。", need)
		}
	}
}

// modelChipFullIDProblems 判定 groupModelsByOwner 的**函数体**（必须已剥注释）
// 是否满足「chip 展示完整 id、去重仍按裸名」；全绿返回 nil。
//
// 抽成纯函数是为了让守卫本身也被验证：反向测试把**回退前的真实写法**喂进来，
// 必须报违规 —— 抓不住回退的守卫是装饰品。
func modelChipFullIDProblems(body string) []string {
	if strings.TrimSpace(body) == "" {
		return []string{"传入的 groupModelsByOwner 函数体是空的 —— 判据失效（fail-open），守卫变成装饰品"}
	}
	var problems []string

	// ---- ① 展示 id 不得再取裸名 ------------------------------------------
	if strings.Contains(body, "id: bare") {
		problems = append(problems, "groupModelsByOwner 里又把**裸名**当展示 id 了（函数体里出现 `id: bare`）。\n"+
			"    破了会怎样：可用模型的 chip 会退回只显示 `glm-5.2` 这类裸名，"+
			"同一个面板里既有裸名又有 `workbuddy/glm-5.2` —— 用户看到的正是他要消灭的『不统一』。")
	}

	// ---- ② items 里必须是完整 id ------------------------------------------
	push := squeezeJS(jsCallStatement(body, "g.items.push("))
	if push == "" {
		problems = append(problems, "groupModelsByOwner 里找不到 `g.items.push(` 语句 —— 判据无法定位 items 的写法。\n"+
			"    破了会怎样：守卫读不到展示 id 的来源，无论它是对是错都判不出来（fail-open）")
	} else {
		if !strings.Contains(push, "id:m.id") {
			problems = append(problems, "groupModelsByOwner 推进 items 时**没有**把完整 id 当展示 id（语句里找不到 `id: m.id`）：\n"+
				"    "+push+"\n"+
				"    破了会怎样：chip 会退回裸名（见判据①），用户报的『不统一』原样复发")
		}
		if !strings.Contains(push, "fullId:m.id") {
			problems = append(problems, "groupModelsByOwner 推进 items 时没有把完整 id 同时放进 `fullId`（语句里找不到 `fullId: m.id`）：\n"+
				"    "+push+"\n"+
				"    破了会怎样：renderModelGroup 的 data-id/title 读 `fullId`，"+
				"它一旦与展示文本不是同一个值，用户看到的和 data-id 指的就分叉了")
		}
		if strings.Contains(push, "bare") {
			problems = append(problems, "groupModelsByOwner 推进 items 的那条语句里仍出现 `bare`：\n"+
				"    "+push+"\n"+
				"    破了会怎样：裸名又混进了展示字段 —— 它正是本项目"+
				"『同一件事两处口径』的经典形态（分组算 workbuddy，展示却给裸名）")
		}
	}

	// ---- ③ 组内去重仍按裸名 ----------------------------------------------
	if !strings.Contains(body, "g.seen.has(bare)") || !strings.Contains(body, "g.seen.add(bare)") {
		problems = append(problems, "groupModelsByOwner 的组内去重不再按**裸名**（找不到 `g.seen.has(bare)` / `g.seen.add(bare)`）。\n"+
			"    破了会怎样：`auto` 与 `workbuddy/auto` 指的是同一个模型，"+
			"只按完整 id 去重会让它们在同一组里各占一条 chip —— 两个名字看起来一模一样，"+
			"用户看到的仍然是『重复』，而这正是本 bug 要消灭的现象")
	}
	if !strings.Contains(body, "new Set()") {
		problems = append(problems, "groupModelsByOwner 里找不到 `new Set()` —— 去重集合消失了。\n"+
			"    破了会怎样：同一组内同名模型会重复展示（用户报的『可用模型重复了，很多』）")
	}

	// ---- ④ 去前缀的归一只有一处：复用 multKeyOf ----------------------------
	//
	// 组内去重的键（bare）与倍率表的键是**同一个变换**（去掉 `provider/` 前缀）。
	// 破了会怎样：两处各写一份规则，迟早会漂 —— 漂的表现是
	// "界面看起来不重复，但倍率整列 x无"，很难一眼定位是哪一份错了。
	if strings.Contains(body, "slice(slash + 1)") {
		problems = append(problems, "groupModelsByOwner 里又自己实现了一份『去掉 provider/ 前缀』（出现 `slice(slash + 1)`）—— "+
			"归一必须只有一处。\n"+
			"    破了会怎样：去重键与倍率表的键各按一份规则算，两份迟早会漂"+
			"（典型：去重按裸名、查表按完整 id → 界面不重复但倍率整列 x无）")
	}
	if !strings.Contains(body, "multKeyOf(m.id)") {
		problems = append(problems, "groupModelsByOwner 的组内去重键没有复用 multKeyOf（找不到 `multKeyOf(m.id)`）。\n"+
			"    破了会怎样：这里会长出第二份去前缀实现，归一逻辑分叉；"+
			"本仓库反复出现的『同一件事两处口径』就是这么来的")
	}

	return problems
}

// --------------------------------------------------------------------------
// 判据组 2：倍率查表按前缀归一，且归一只有一处
// --------------------------------------------------------------------------

// TestWebUIModelIDsUnifiedMultiplierKeying 守住倍率标记不因 id 口径统一而丢失。
//
// 判据：
//
//	① `multKeyOf` 存在、只定义一次，且真的去掉 `provider/` 前缀
//	② `multTag` 与 `multTagPlain` **两个查询点**都经由 multKeyOf 取键
//	③ 两个查询点都不再拿完整 id 直接查表（`modelMultipliers[id]`）
func TestWebUIModelIDsUnifiedMultiplierKeying(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	multTagBody, ok := extractJSFunction(code, "function multTag(")
	if !ok {
		t.Fatal("找不到 multTag 函数 —— 守卫无法定位（fail-open）；" +
			"如果它被改名/拆分了，本守卫的判据也要跟着改，而不是让它空转")
	}
	multTagPlainBody, ok := extractJSFunction(code, "function multTagPlain(")
	if !ok {
		t.Fatal("找不到 multTagPlain 函数 —— 对话测试下拉的倍率标记没有渲染器（守卫失效）")
	}

	for _, p := range multiplierKeyingProblems(code, multTagBody, multTagPlainBody) {
		t.Errorf("%s", p)
	}
}

// multiplierKeyingProblems 判定「倍率查表按前缀归一」是否成立；全绿返回 nil。
//
// 三个入参分别是：剥注释后的整份源码、multTag 函数体、multTagPlain 函数体。
func multiplierKeyingProblems(code, multTagBody, multTagPlainBody string) []string {
	var problems []string

	// ---- ① 归一函数存在、唯一、真的去前缀 --------------------------------
	switch n := strings.Count(code, "function multKeyOf("); {
	case n == 0:
		problems = append(problems, "webui.html 里找不到 `multKeyOf` —— 完整 id 与倍率表的裸名键之间没有归一。\n"+
			"    破了会怎样：chip 与下拉里显示的是 `workbuddy/glm-5.2`，"+
			"而 `modelMultipliers` 的键是上游目录里的裸名 `glm-5.2`，"+
			"直接查表必然全部落空 → 满屏 `x无`，把『目录里有这个模型』说成『上游没配倍率』"+
			"（tests/frontend/diag_multiplier_mismatch.js 实测过这条错位）")
	case n > 1:
		problems = append(problems, "`multKeyOf` 被定义了多次 —— 归一逻辑出现了第二份。\n"+
			"    破了会怎样：两份规则迟早漂（比如一处按 `/` 切、一处按第一个 `-` 切），"+
			"而漂的表现只是『某处又变回 x无』，很难一眼看出是哪一份错了")
	}
	if keyBody, ok := extractJSFunction(code, "function multKeyOf("); ok {
		if !strings.Contains(keyBody, "indexOf('/')") {
			problems = append(problems, "multKeyOf 里找不到 `indexOf('/')` —— 它没有在找 `provider/` 前缀的分界。\n"+
				"    破了会怎样：归一恒等于原样返回，查表照样全部落空（满屏 `x无`），"+
				"而且看起来'函数在、也被调用了'，排查时更难")
		}
		if !strings.Contains(keyBody, "slice(s + 1)") {
			problems = append(problems, "multKeyOf 里找不到 `slice(s + 1)` —— 它没有真的把前缀切掉。\n"+
				"    破了会怎样：返回的仍是完整 id，倍率表查不到，chip 上全是 `x无`")
		}
	}

	// ---- ②③ 两个查询点都必须经由归一，且不得再直接查表 --------------------
	for _, q := range []struct {
		name   string
		body   string
		marker string // 用来确认切出来的确实是这个函数（fail-closed）
	}{
		{"multTag", multTagBody, "mult unknown"},
		{"multTagPlain", multTagPlainBody, "(x${txt})"},
	} {
		if strings.TrimSpace(q.body) == "" {
			problems = append(problems, "传入的 "+q.name+" 函数体是空的 —— 判据失效（fail-open），守卫变成装饰品")
			continue
		}
		if !strings.Contains(q.body, q.marker) {
			problems = append(problems, "切出来的函数体不像 "+q.name+"（找不到结构标记 "+q.marker+"）—— "+
				"切分逻辑已失效，本守卫此时无论绿红都不可信")
			continue
		}
		if !strings.Contains(q.body, "multTableKey(id)") {
			problems = append(problems, q.name+" 没有经 multTableKey 取键就查倍率表（函数体里找不到 `multTableKey(id)`）。\n"+
				"    破了会怎样：展示 id 是 `provider/xxx`，而倍率表的键是裸名**且只覆盖默认上游**；\n"+
				"    直接去前缀查表会把别家上游的同名模型算成默认上游的系数"+
				"（真 Chrome 实测：codearts 的 glm-5.3-flash 显示成 workbuddy 的 x0.06），"+
				"或者退化回满屏 `x无`。\n"+
				"    正确写法：调 multTableKey —— 它内部再经 multKeyOf 归一，归一仍只有一处")
		}
		if !strings.Contains(q.body, "modelMultipliers, key") || !strings.Contains(q.body, "modelMultipliers[key]") {
			problems = append(problems, q.name+" 虽然提到了 multKeyOf，却不是用归一后的键查表"+
				"（找不到 `modelMultipliers, key` / `modelMultipliers[key]`）。\n"+
				"    破了会怎样：归一白做了 —— 查询仍落在完整 id 上，倍率标记全丢")
		}
		if strings.Contains(q.body, "modelMultipliers[id]") {
			problems = append(problems, q.name+" 里仍然出现 `modelMultipliers[id]` —— 拿**完整 id** 直接查表。\n"+
				"    破了会怎样：这就是本 bug 的回退形态（`provider/glm-5.2` 查裸名键的表），"+
				"倍率必然查不到，界面显示 `x无`；而倍率表其实是好的，用户会去怀疑上游")
		}
	}

	return problems
}

// --------------------------------------------------------------------------
// 判据组 2b：倍率表按 (provider, 裸名) 组织 —— 别家上游不得撞默认上游的裸名系数
// --------------------------------------------------------------------------

// TestWebUIModelIDsUnifiedMultiplierUpstreamOwnership 守住「别家上游不继承默认上游系数」。
//
// # 这条缺陷的实测形态（真 Chrome，不是推断）
//
//	workbuddy 组 16 个：workbuddy/glm-5.2 x0.79 …            ← 正确
//	codearts  组  7 个：codearts/glm-5.3-flash **x0.06**，同组其余 6 个 x无
//	/admin/models/preview 只返回一条裸名 `glm-5.3-flash`:0.06，**没有任何 codearts 条目**
//
// 那个 0.06 是 **workbuddy** 目录里 glm-5.3-flash 的系数 —— 被「无条件去前缀查表」
// 当成了 codearts 的事实。**显示一个错的数字比显示 x无 有害得多**。
//
// # 多上游改造后的判据（2026-09）
//
// 后端 /admin/models/preview 已遍历全部已注册上游、每条带 provider，
// 前端表键 = `provider/裸名`（workbuddy 与 workbuddy-intl 的同名模型互不覆盖）。
// 于是带前缀的展示 id 直接以 `provider/裸名` 组合键查表，**不再去前缀** ——
// 去前缀正是旧缺陷（撞默认上游裸名）的来源。
//
//	① `multTableKey` 存在且只定义一次（键规则只有一处）
//	② 带前缀分支返回**原 id**（`provider/裸名` 组合键），**绝不**去前缀
//	   —— 全文件里带前缀分支不应出现 `multKeyOf(id)`（那会撞默认上游裸名）
//	③ 无前缀分支原样查表（单上游行为不变）
//	④ 它内部仍调 multKeyOf —— 仅限无前缀分支（单上游裸名键）
//	⑤ multTag / multTagPlain 都经它取键，且都不自己写去前缀
//	⑥ 前端表键由 loadModelMultipliers 拼成 `provider/裸名`（`m.provider + '/' + m.model`），
//	   与 multTableKey 的组合键规则同源
func TestWebUIModelIDsUnifiedMultiplierUpstreamOwnership(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	multTagBody, ok := extractJSFunction(code, "function multTag(")
	if !ok {
		t.Fatal("找不到 multTag 函数 —— 守卫无法定位（fail-open）；改名/拆分了就要跟着改判据")
	}
	multTagPlainBody, ok := extractJSFunction(code, "function multTagPlain(")
	if !ok {
		t.Fatal("找不到 multTagPlain 函数 —— 对话测试下拉的倍率标记没有渲染器（守卫失效）")
	}

	for _, p := range multiplierOwnershipProblems(code, multTagBody, multTagPlainBody) {
		t.Errorf("%s", p)
	}

	// 表键拼接在真源码层检查（multiplierOwnershipProblems 的入参可能是测试样本，
	// 不含 loadModelMultipliers —— 那把检查放这里，用完整的真实源码）。
	if !strings.Contains(code, "m.provider + '/' + m.model") {
		t.Errorf("loadModelMultipliers 里找不到表键拼接 `m.provider + '/' + m.model`。\n" +
			"    破了会怎样：表键不是 provider/裸名，multTableKey 的组合键查不到，满屏 x无")
	}
}

// TestWebUIModelIDsUnifiedDefaultProviderTiming 守住「默认上游从哪来、什么时候有值」。
//
// # 为什么这条必须单独守
//
// multTableKey 的判据依赖一个约定：**归属未知 == DEFAULT_PROVIDER 是空串**。
// 这个约定成立的前提是「它只在 manifest 到达后才被赋值」（见 webui.html 定义处与
// applyManifest）。若有人把它改成有初值的常量、或另造一个默认上游，
// 判据会在 manifest 未到达时把某个真实上游当成默认 —— 于是**别家的数字又被显示出来**，
// 而所有别的断言都还是绿的。
//
// # 判据
//
//	① 定义处初值是空串 `let DEFAULT_PROVIDER = '';`
//	② 全文件 `DEFAULT_PROVIDER = ` 恰好 2 次：定义处 + applyManifest 里的赋值
//	   （>2 = 另造了一份；<2 = applyManifest 不再写它，默认上游自己也全变 x无）
//	③ applyManifest 里是 `DEFAULT_PROVIDER = def ? def.id : ''` —— 从 manifest 的
//	   providers 里选，而不是硬编码
func TestWebUIModelIDsUnifiedDefaultProviderTiming(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	for _, p := range defaultProviderTimingProblems(code) {
		t.Errorf("%s", p)
	}
}

// multiplierOwnershipProblems 判定「倍率查表尊重上游归属」是否成立；全绿返回 nil。
//
// 三个入参分别是：剥注释后的整份源码、multTag 函数体、multTagPlain 函数体。
// 抽成纯函数是为了让反向用例能把**回退前的真实写法**（退回无条件去前缀）喂进来，
// 确认它报违规 —— 抓不住回退的守卫是装饰品。
func multiplierOwnershipProblems(code, multTagBody, multTagPlainBody string) []string {
	var problems []string

	// ---- ① 判据函数存在且唯一 --------------------------------------------
	switch n := strings.Count(code, "function multTableKey("); {
	case n == 0:
		problems = append(problems, "webui.html 里找不到 `multTableKey` —— 倍率查表不再判上游归属。\n"+
			"    破了会怎样：这就是本缺陷的原始形态 —— 别家上游的 `codearts/glm-5.3-flash`\n"+
			"    去前缀后撞上默认上游目录里的同名裸名，于是显示出**别人的系数**"+
			"（真 Chrome 实测 x0.06）。\n"+
			"    它看起来是一个完全正常的数字，比 `x无` 危险得多 —— 用户会按它算成本")
	case n > 1:
		problems = append(problems, "`multTableKey` 被定义了多次 —— 归属判据出现了第二份。\n"+
			"    破了会怎样：两个查表点各按一份判据走，迟早漂（一处判归属、一处不判），\n"+
			"    而漂的表现只是「某个面板又显示出别家的数字」，很难一眼看出是哪一份")
	}

	body, ok := extractJSFunction(code, "function multTableKey(")
	if !ok {
		return append(problems, "切不出 multTableKey 的函数体 —— 键规则判不了（fail-open），"+
			"此时本守卫无论绿红都不可信")
	}
	// fail-closed 自检：切出来的必须像 multTableKey（否则判据读的不是它）。
	if !strings.Contains(body, "multKeyOf(id)") || len(body) > 3000 {
		return append(problems, "切出来的函数体不像 multTableKey（缺 `multKeyOf(id)` 或过大，共 "+
			itoa(len(body))+" 字节）—— 切分逻辑已失效，本守卫此时无论绿红都不可信")
	}

	// ---- ② 带前缀分支绝不"去前缀"（去前缀 = 撞默认上游裸名 = 旧缺陷） --------
	// multKeyOf 只允许出现在无前缀分支一次；带前缀分支若也调它（去前缀查表）
	// 就会变成 2 次 —— 那正是旧缺陷形态（codearts/glm-5.3-flash 撞默认上游裸名）。
	if n := strings.Count(body, "multKeyOf(id)"); n != 1 {
		problems = append(problems, "multTableKey 里 `multKeyOf(id)` 出现 "+itoa(n)+
			" 次（应恰好 1 次，只在无前缀分支）。\n"+
			"    破了会怎样：带前缀分支去前缀查表 → `codearts/glm-5.3-flash` 撞上默认上游\n"+
			"    目录里的同名裸名，显示出**别人的系数**（真 Chrome 实测 x0.06）—— \n"+
			"    一个看起来正常的错数字，比 x无 危险得多")
	}
	// 带前缀分支必须返回原 id（provider/裸名 组合键），而不是别的东西。
	if !strings.Contains(body, "return String(id)") {
		problems = append(problems, "multTableKey 的带前缀分支没有返回原 id（`return String(id)`）。\n"+
			"    破了会怎样：前端表键是 `provider/裸名`，若这里去前缀或返回 null，\n"+
			"    别家上游（含海外版 workbuddy-intl）的模型全部 x无")
	}

	// ---- ③ 无前缀（裸名）原样查表 --------------------------------
	if !strings.Contains(body, "!(slash > 0)") {
		problems = append(problems, "multTableKey 里找不到无前缀（裸名）原样查表的分支（`!(slash > 0)`）。\n"+
			"    破了会怎样：单上游部署下后端下发的裸名会被当成「没有前缀就没有归属」而返回 null，\n"+
			"    界面从有倍率变成满屏 `x无` —— 而裸名本来就是倍率表的键，本该原样查")
	}

	// ---- ④ 去前缀的归一没有被复制成第二份 --------------------------------
	if strings.Contains(body, "slice(slash + 1)") || strings.Contains(body, "slice(s + 1)") {
		problems = append(problems, "multTableKey 里自己实现了一份去前缀（出现 `slice(... + 1)`）—— "+
			"归一必须只有一处。\n"+
			"    破了会怎样：去前缀规则出现第二份，与 multKeyOf 迟早漂"+
			"（典型：一处按第一个 `/` 切、一处按最后一个）")
	}

	// ---- ⑤⑥ 两个查询点都经它取键，且各自不写第二份判断 --------------------
	for _, q := range []struct {
		name   string
		body   string
		marker string // 用来确认切出来的确实是这个函数（fail-closed）
	}{
		{"multTag", multTagBody, "mult unknown"},
		{"multTagPlain", multTagPlainBody, "(x${txt})"},
	} {
		if strings.TrimSpace(q.body) == "" {
			problems = append(problems, "传入的 "+q.name+" 函数体是空的 —— 判据失效（fail-open），守卫变成装饰品")
			continue
		}
		if !strings.Contains(q.body, q.marker) {
			problems = append(problems, "切出来的函数体不像 "+q.name+"（找不到结构标记 "+q.marker+"）—— "+
				"切分逻辑已失效，本守卫此时无论绿红都不可信")
			continue
		}
		if !strings.Contains(q.body, "multTableKey(id)") {
			problems = append(problems, q.name+" 没有经 multTableKey 取键（函数体里找不到 `multTableKey(id)`）。\n"+
				"    破了会怎样：这个渲染点绕开了归属判据 —— 别家上游在这里继承默认上游的系数，"+
				"而同一份文件里的另一个渲染点是正确的（两个面板显示两个数字）")
		}
		if strings.Contains(q.body, "indexOf('/')") {
			problems = append(problems, q.name+" 里又自己写了去前缀（出现 `indexOf('/')`）—— 判据必须只有一处。\n"+
				"    破了会怎样：判据分叉，一处判归属一处不判，漂的表现是"+
				"「同一个模型在 chips 上有数字、在下拉里没有」（或反过来）")
		}
	}

	// ---- ⑥ multTag 的 x无 必须说清是「表不覆盖这个上游」 -------------------
	if !strings.Contains(multTagBody, "倍率表只覆盖默认上游") {
		problems = append(problems, "multTag 的 x无 没有说明「倍率表只覆盖默认上游」这个原因。\n"+
			"    破了会怎样：别家上游的 x无 与「上游真的没配倍率」长得一模一样，\n"+
			"    用户会去问上游要倍率，而真相是本界面只有默认上游的目录")
	}
	if !strings.Contains(multTagBody, "上游未提供该模型的倍率") {
		problems = append(problems, "multTag 丢掉了原有的 title「上游未提供该模型的倍率」—— 两种"+
			"「没有」被合并成了一种。\n"+
			"    破了会怎样：表里没有这个模型（该问上游）与表不覆盖这个上游（本界面拿不到）无法区分")
	}

	return problems
}

// defaultProviderTimingProblems 判定 DEFAULT_PROVIDER 的「来源与时机」；全绿返回 nil。
func defaultProviderTimingProblems(code string) []string {
	var problems []string

	if !strings.Contains(code, "let DEFAULT_PROVIDER = '';") {
		problems = append(problems, "DEFAULT_PROVIDER 的初值不是空串（找不到 `let DEFAULT_PROVIDER = '';`）。\n"+
			"    破了会怎样：multTableKey 依赖「归属未知 == 空串」这个约定；\n"+
			"    初值若被写成某个真实上游名，manifest 未到达（或取不到）时它就被当成事实 ——\n"+
			"    别家的数字又会被显示出来，而所有别的断言都还是绿的")
	}
	if n := strings.Count(code, "DEFAULT_PROVIDER = "); n != 2 {
		problems = append(problems, "`DEFAULT_PROVIDER = ` 出现 "+itoa(n)+
			" 次（应为 2：定义处 + applyManifest 里的赋值）。\n"+
			"    破了会怎样：>2 说明有人在别处**另造**了默认上游（本任务明确要求复用这一份，\n"+
			"    两处迟早给出不同的默认上游）；<2 说明 applyManifest 不再写它 —— \n"+
			"    DEFAULT_PROVIDER 永远是空串，multTableKey 于是对**所有**带前缀的 id 返回 null，\n"+
			"    连默认上游自己那 16 个 chip 都会从 x0.79 变成 x无")
	}

	const marker = "function applyManifest("
	am, ok := extractJSFunction(code, marker)
	if !ok {
		problems = append(problems, "找不到 applyManifest —— 无法确认 DEFAULT_PROVIDER 的赋值时机（守卫失效）")
		return problems
	}
	if !strings.Contains(am, "MANIFEST = {") || !strings.Contains(am, "PROVIDERS = MANIFEST.providers") {
		problems = append(problems, "切出来的函数体不像 applyManifest（缺 `MANIFEST = {` / `PROVIDERS = MANIFEST.providers`）—— "+
			"切分逻辑已失效，本守卫此时无论绿红都不可信")
		return problems
	}
	if !strings.Contains(am, "DEFAULT_PROVIDER = def ? def.id : ''") {
		problems = append(problems, "applyManifest 里找不到 `DEFAULT_PROVIDER = def ? def.id : ''`。\n"+
			"    破了会怎样：默认上游不再是从 manifest 的 providers 里选出来的（可能被硬编码），\n"+
			"    或者赋值被挪到 manifest 到达之前 —— 两种都会让 multTableKey 的归属判断失真")
	}

	return problems
}

// --------------------------------------------------------------------------
// 判据组 3：对话测试下拉按 id 去重，且 <option> 里只能放纯文本
// --------------------------------------------------------------------------

// TestWebUIModelIDsUnifiedChatDropdownDedup 守住"对话测试"下拉不重复。
//
// # 判据为什么要挑"能变红"的那种
//
// renderModels 函数体很大，弱判据（比如"包含 seen 这个词就算过"）会假绿：
// 只要文件里还留着别的 `seen`（chip 区就有 `g.seen`），把去重删掉也照样绿。
// ∴ 这里的判据是**结构性**的：
//
//	· 必须有一个 `new Set()` 作为已见集合，并且对 **m.id** 做 has/add；
//	· `sel.innerHTML = X.map(...)` 里的 `X` 必须是**去重循环里 push 出来的那个变量**
//	  （既不是 `models`、也不是筛选后的 `filtered`）；
//	· 去重必须发生在渲染**之前**（顺序判据）；
//	· option 里只能调 multTagPlain（它只产出 ` (x0.51)`），不能调 multTag（产出 HTML）。
func TestWebUIModelIDsUnifiedChatDropdownDedup(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	const marker = "function renderModels("
	body, ok := extractJSFunction(code, marker)
	if !ok {
		t.Fatal("找不到 renderModels 函数 —— 守卫无法定位（fail-open）；" +
			"如果它被改名/拆分了，本守卫的判据也要跟着改，而不是让它空转")
	}
	// fail-closed 自检：确认切出来的**确实**是 renderModels。
	if !strings.Contains(body, "$('mcount')") || !strings.Contains(body, "syncModelProviderOptions(") {
		t.Fatalf("切出来的函数体不像 renderModels（缺 $('mcount') / syncModelProviderOptions，共 %d 字节）—— "+
			"切分逻辑已失效，本守卫此时无论绿红都不可信", len(body))
	}
	if len(body) > 8000 || strings.Contains(body, "function modelOwnerOf(") {
		t.Fatalf("切出来的函数体过大或串到了相邻函数（共 %d 字节）—— 切分逻辑已失效", len(body))
	}

	for _, p := range modelDropdownDedupProblems(body) {
		t.Errorf("%s", p)
	}
}

// modelDropdownDedupProblems 判定 renderModels 的**函数体**（必须已剥注释）
// 是否满足「对话测试下拉按 id 去重」；全绿返回 nil。
func modelDropdownDedupProblems(body string) []string {
	if strings.TrimSpace(body) == "" {
		return []string{"传入的 renderModels 函数体是空的 —— 判据失效（fail-open），守卫变成装饰品"}
	}
	var problems []string

	const renderMark = "sel.innerHTML ="
	iSel := strings.Index(body, renderMark)
	if iSel < 0 {
		return []string{"renderModels 里找不到 `sel.innerHTML =` —— 对话测试下拉的渲染点定位失败（fail-open）。\n" +
			"    破了会怎样：守卫读不到『下拉到底渲染了哪个列表』，无论它去没去重都判不出来。"}
	}

	// 渲染用的列表变量名（`sel.innerHTML = X.map(…)` 里的 X）。
	rest := strings.TrimLeft(body[iSel+len(renderMark):], " \t\r\n")
	j := 0
	for j < len(rest) && isJSIdentByte(rest[j]) {
		j++
	}
	listVar := rest[:j]
	if listVar == "" || !isJSIdentStart(listVar[0]) {
		problems = append(problems, "`sel.innerHTML =` 右边不是『变量.map(…)』形式（读到的标识符是 "+quoteForMsg(listVar)+"）。\n"+
			"    破了会怎样：判据取不到『渲染的是哪个列表』，去重与否就无法判定（fail-open）")
	}

	// ---- ① 必须有"已见集合"，并且对 m.id 真的做去重 ----------------------
	if !strings.Contains(body, "new Set()") {
		problems = append(problems, "renderModels 里找不到 `new Set()` —— 下拉的已见集合没有了。\n"+
			"    破了会怎样：后端若又同时下发裸名与 `provider/裸名`（改造前实测 32 条 = 16 个模型 × 2 种写法），"+
			"对话测试下拉里会出现两条一模一样的选项 —— 用户报的『重复了，很多』原样复发")
	}
	if !strings.Contains(body, "seen.has(m.id)") || !strings.Contains(body, "seen.add(m.id)") {
		problems = append(problems, "renderModels 里找不到按 **m.id** 的去重动作"+
			"（`seen.has(m.id)` / `seen.add(m.id)` 不全）。\n"+
			"    破了会怎样：要么整段去重被删（下拉重复），"+
			"要么改成按别的字段去重（比如按裸名）—— 那会把 `auto` 与 `workbuddy/auto` 合并成一个选项，"+
			"用户就没法在测试时显式指定上游了")
	}

	// ---- ② 渲染的必须是**去重后**的那个列表变量，不是原始列表 -------------
	if listVar != "" {
		switch listVar {
		case "models", "filtered", "list":
			problems = append(problems, "`sel.innerHTML` 渲染的是**未去重**的列表（"+listVar+"）—— "+
				"去重循环写了也没接上。\n"+
				"    破了会怎样：这就是最隐蔽的回退形态（`seen` 还在、`has/add` 还在，"+
				"但渲染用的是原始列表），下拉照样重复，而看代码像是『已经去重了』")
		default:
			if !strings.Contains(body, listVar+".push(m)") {
				problems = append(problems, "`sel.innerHTML` 渲染的变量 "+listVar+
					" 不是去重循环里累计出来的（找不到 `"+listVar+".push(m)`）。\n"+
					"    破了会怎样：判据无法证明渲染用的是去重后的列表 —— "+
					"去重动作与实际渲染脱钩，下拉可能依旧重复（假绿）")
			}
		}
	}

	// ---- ③ 顺序：去重必须在渲染之前 --------------------------------------
	iSeen := strings.Index(body, "seen.add(m.id)")
	iPush := -1
	if listVar != "" {
		iPush = strings.Index(body, listVar+".push(m)")
	}
	if iSeen >= 0 && iSel < iSeen {
		problems = append(problems, "`sel.innerHTML` 的赋值排在去重循环**之前**（下标 "+itoa(iSel)+" < "+itoa(iSeen)+"）—— "+
			"顺序反了等于没去重。\n"+
			"    破了会怎样：列表先渲染出去、随后才去重，用户看到的仍是重复项")
	}
	if iPush >= 0 && iSel < iPush {
		problems = append(problems, "`sel.innerHTML` 的赋值排在 `"+listVar+".push(m)` **之前** —— "+
			"渲染时那个列表还是空的（下拉会变成空列表）。\n"+
			"    破了会怎样：对话测试选不出模型 —— 比重复更严重")
	}

	// ---- ④ <option> 只能放纯文本：必须用 multTagPlain ---------------------
	if !strings.Contains(body[iSel:], "multTagPlain(m.id)") {
		problems = append(problems, "对话测试下拉的 `<option>` 里没有调用 multTagPlain(m.id)。\n"+
			"    破了会怎样：倍率标记在选项里显示不出来（用户看不出这个模型贵不贵）")
	}
	if strings.Contains(body, "multTag(m.id)") {
		problems = append(problems, "对话测试下拉里出现了 `multTag(m.id)` —— 它产出的是 HTML 标签。\n"+
			"    破了会怎样：`<option>` 里塞 `<span class=\"mult\">` 会被当成纯文本显示，"+
			"用户在下拉里看到一串标签源码（这就是 multTagPlain 存在的理由）")
	}

	return problems
}

// --------------------------------------------------------------------------
// 反向验证：守卫必须抓得住"回退到修复前"的真实写法
// --------------------------------------------------------------------------

// TestWebUIModelIDsUnifiedGuardCatchesRevertedCode 用**回退前的真实代码形态**
// 喂给上面三组判据，确认它们报违规；同时确认干净样本不报（否则会退化成噪音）。
//
// # 为什么守卫本身也需要被验证
//
// 一条抓不住回退的守卫 = 装饰品。与 webui_endpoint_guard_test.go 的
// TestWebUIEndpointGuardCatchesInjectedDefect 同一条理由：
// 用**与真实缺陷逐字同形**的样本喂给判据，确认它报违规。
//
// ⚠ 三个脏样本都不是编的，它们正是本次改动消灭的形态：
//
//	脏样本 1：`g.items.push({ id: bare, fullId: m.id, owner: owner });`（修复前原文）
//	脏样本 2：multTag/multTagPlain 直接 `modelMultipliers[id]`（修复前原文）
//	脏样本 3：`sel.innerHTML = filtered.map(…)`，去重整段不存在（修复前原文）
func TestWebUIModelIDsUnifiedGuardCatchesRevertedCode(t *testing.T) {
	// ---- 脏样本 1：chip 展示裸名 ----------------------------------------
	const dirtyChips = "" +
		"    const slash = m.id.indexOf('/');\n" +
		"    const bare = slash > 0 ? m.id.slice(slash + 1) : m.id;\n" +
		"    const g = { owner: owner, items: [], seen: new Set(), accountCount: 0 };\n" +
		"    if (g.seen.has(bare)) continue;\n" +
		"    g.seen.add(bare);\n" +
		"    g.items.push({ id: bare, fullId: m.id, owner: owner });\n"
	got := modelChipFullIDProblems(dirtyChips)
	if len(got) == 0 {
		t.Fatal("守卫抓不住『chip 展示裸名』（`id: bare`）—— 它是装饰品（本用例证明判据是假绿的）")
	}
	joinedChips := strings.Join(got, "\n")
	if !strings.Contains(joinedChips, "id: bare") {
		t.Errorf("回退样本被报了，但没报出『又拿裸名当展示 id』这一条 —— 报的是别的问题。守卫输出：\n%s", joinedChips)
	}
	// 修复前那份代码同样**自己实现**了一份去前缀（`slice(slash + 1)`）——
	// 这条也要被抓住，否则"归一只有一处"就没人守。
	if !strings.Contains(joinedChips, "slice(slash + 1)") {
		t.Errorf("回退样本没有被报出『又自己实现了一份去前缀』（`slice(slash + 1)`）—— 这条违规漏网了。守卫输出：\n%s", joinedChips)
	}

	// 干净样本 1：本次改动后的真实写法（完整 id + 裸名去重 + 复用 multKeyOf）。
	const cleanChips = "" +
		"    const slash = m.id.indexOf('/');\n" +
		"    const prefix = slash > 0 ? m.id.slice(0, slash) : '';\n" +
		"    const bare = multKeyOf(m.id);\n" +
		"    const g = { owner: owner, items: [], seen: new Set(), accountCount: 0 };\n" +
		"    if (g.seen.has(bare)) continue;\n" +
		"    g.seen.add(bare);\n" +
		"    g.items.push({ id: m.id, fullId: m.id, owner: owner });\n"
	if got := modelChipFullIDProblems(cleanChips); len(got) != 0 {
		t.Fatalf("守卫对修复后的 chip 写法误报（会变成噪音，让人不再看它的输出）：\n%s", strings.Join(got, "\n"))
	}

	// ---- 脏样本 2：倍率直接拿完整 id 查表 --------------------------------
	//
	// code 里**留着** multKeyOf（所以"函数不存在"那条不触发），
	// 只把两个查询点改回 `modelMultipliers[id]` —— 这正是本次变异验证的第 1 条。
	const keyCode = "" +
		"  function multKeyOf(id) {\n" +
		"    const s = String(id || '').indexOf('/');\n" +
		"    return s > 0 ? String(id).slice(s + 1) : String(id || '');\n" +
		"  }\n"
	const dirtyMultTag = "" +
		"  function multTag(id) {\n" +
		"    if (!Object.prototype.hasOwnProperty.call(modelMultipliers, id)) {\n" +
		"      return '<span class=\"mult unknown\">x无</span>';\n" +
		"    }\n" +
		"    const txt = multText(modelMultipliers[id]);\n" +
		"    const v = modelMultipliers[id];\n" +
		"    return `<span class=\"mult\">x${esc(txt)}</span>`;\n" +
		"  }\n"
	const dirtyMultTagPlain = "" +
		"  function multTagPlain(id) {\n" +
		"    if (!Object.prototype.hasOwnProperty.call(modelMultipliers, id)) return '';\n" +
		"    const txt = multText(modelMultipliers[id]);\n" +
		"    return txt === null ? '' : ` (x${txt})`;\n" +
		"  }\n"
	got2 := multiplierKeyingProblems(keyCode, dirtyMultTag, dirtyMultTagPlain)
	if len(got2) == 0 {
		t.Fatal("守卫抓不住『倍率直接拿完整 id 查表』（`modelMultipliers[id]`）—— 它是装饰品")
	}
	joined2 := strings.Join(got2, "\n")
	if !strings.Contains(joined2, "multKeyOf") {
		t.Errorf("回退样本被报了，但没报出『没经 multKeyOf 归一』这一条 —— 报的是别的问题。守卫输出：\n%s", joined2)
	}
	// 还要报出"拿完整 id 直接查表"这条 —— 它是本 bug 的原始形态
	//（回退的人更可能把一整段替换成 `modelMultipliers[id]`，而不是只删掉归一调用）。
	if !strings.Contains(joined2, "modelMultipliers[id]") {
		t.Errorf("回退样本没有被报出『仍然拿完整 id 直接查表』（`modelMultipliers[id]`）—— 这条违规漏网了。守卫输出：\n%s", joined2)
	}

	// 干净样本 2：本次改动后的真实写法。
	//
	// ⚠ 已是**第二版**：倍率查表不再直接调 multKeyOf，而是调 multTableKey
	//（它内部再调 multKeyOf）。样本必须与真源码同步，否则它证明的只是"旧写法没问题"。
	const cleanMultTag = "" +
		"  function multTag(id) {\n" +
		"    const key = multTableKey(id);\n" +
		"    if (key === null) {\n" +
		"      if (multCatalogFailed) return '';\n" +
		"      return '<span class=\"mult unknown\" title=\"倍率表只覆盖默认上游 workbuddy 的目录\">x无</span>';\n" +
		"    }\n" +
		"    if (!Object.prototype.hasOwnProperty.call(modelMultipliers, key)) {\n" +
		"      return '<span class=\"mult unknown\" title=\"上游未提供该模型的倍率\">x无</span>';\n" +
		"    }\n" +
		"    const txt = multText(modelMultipliers[key]);\n" +
		"    const v = modelMultipliers[key];\n" +
		"    return `<span class=\"mult\">x${esc(txt)}</span>`;\n" +
		"  }\n"
	const cleanMultTagPlain = "" +
		"  function multTagPlain(id) {\n" +
		"    const key = multTableKey(id);\n" +
		"    if (key === null) return '';\n" +
		"    if (!Object.prototype.hasOwnProperty.call(modelMultipliers, key)) return '';\n" +
		"    const txt = multText(modelMultipliers[key]);\n" +
		"    return txt === null ? '' : ` (x${txt})`;\n" +
		"  }\n"
	if got := multiplierKeyingProblems(keyCode, cleanMultTag, cleanMultTagPlain); len(got) != 0 {
		t.Fatalf("守卫对修复后的倍率写法误报（会变成噪音）：\n%s", strings.Join(got, "\n"))
	}

	// ---- 脏样本 2b / 干净样本 2b：倍率表按 (provider, 裸名) 组织 --------------------------
	//
	// 这是本轮的新缺陷形态：**带前缀分支退回"无条件去前缀查表"**，
	// 于是别家上游的 `codearts/glm-5.3-flash` 继承默认上游的 0.06。
	// 真实回退长这样 —— 函数还在、也被调用，看起来"归一仍然只有一处"。
	const cleanMultTableKeySrc = "" +
		"  function multTableKey(id) {\n" +
		"    const slash = String(id || '').indexOf('/');\n" +
		"    if (!(slash > 0)) return multKeyOf(id);\n" +
		"    return String(id);\n" +
		"  }\n"
	const revertedMultTableKeySrc = "" +
		"  function multTableKey(id) {\n" +
		"    const slash = String(id || '').indexOf('/');\n" +
		"    if (!(slash > 0)) return multKeyOf(id);\n" +
		"    return multKeyOf(id);\n" +
		"  }\n"
	gotOwnership := multiplierOwnershipProblems(keyCode+revertedMultTableKeySrc, cleanMultTag, cleanMultTagPlain)
	if len(gotOwnership) == 0 {
		t.Fatal("守卫抓不住『带前缀分支去前缀查表』（multTableKey 退回无条件去前缀）—— " +
			"它是装饰品：这正是本次缺陷的回退形态，别家上游会重新继承默认上游的系数")
	}
	joinedOwnership := strings.Join(gotOwnership, "\n")
	if !strings.Contains(joinedOwnership, "multKeyOf") {
		t.Errorf("回退样本被报了，但没报出『multKeyOf 出现在带前缀分支』这一条 —— 报的是别的问题。守卫输出：\n%s",
			joinedOwnership)
	}
	if got := multiplierOwnershipProblems(keyCode+cleanMultTableKeySrc, cleanMultTag, cleanMultTagPlain); len(got) != 0 {
		t.Fatalf("守卫对修复后的倍率写法误报（会变成噪音）：\n%s", strings.Join(got, "\n"))
	}

	// ---- 脏样本 3：下拉不去重 --------------------------------------------
	const dirtyDropdown = "" +
		"  function renderModels(list) {\n" +
		"    const sel = $('model');\n" +
		"    const filtered = list;\n" +
		"    sel.innerHTML = filtered.map(m =>\n" +
		"      `<option value=\"${esc(m.id)}\">${esc(m.id)}${multTagPlain(m.id)}</option>`).join('');\n" +
		"  }\n"
	got3 := modelDropdownDedupProblems(dirtyDropdown)
	if len(got3) == 0 {
		t.Fatal("守卫抓不住『下拉不去重』（直接渲染未去重列表）—— 它是装饰品")
	}
	joined3 := strings.Join(got3, "\n")
	if !strings.Contains(joined3, "去重") {
		t.Errorf("回退样本被报了，但没报出『没有去重』这一条 —— 报的是别的问题。守卫输出：\n%s", joined3)
	}

	// 干净样本 3：本次改动后的真实写法。
	const cleanDropdown = "" +
		"  function renderModels(list) {\n" +
		"    const sel = $('model');\n" +
		"    const filtered = list;\n" +
		"    const seen = new Set();\n" +
		"    const shown = [];\n" +
		"    for (const m of filtered) {\n" +
		"      if (seen.has(m.id)) continue;\n" +
		"      seen.add(m.id);\n" +
		"      shown.push(m);\n" +
		"    }\n" +
		"    sel.innerHTML = shown.map(m =>\n" +
		"      `<option value=\"${esc(m.id)}\">${esc(m.id)}${multTagPlain(m.id)}</option>`).join('');\n" +
		"  }\n"
	if got := modelDropdownDedupProblems(cleanDropdown); len(got) != 0 {
		t.Fatalf("守卫对修复后的下拉写法误报（会变成噪音）：\n%s", strings.Join(got, "\n"))
	}

	// ---- 假绿专项：`seen` 还在、has/add 还在，但渲染用的是原始列表 --------
	//
	// 这是最容易骗过弱判据的形态：文件里 grep 得到 `seen`、`new Set()`、
	// `seen.add(m.id)`，只有"渲染的到底是哪个变量"这一处不同。
	// 若下面这一条不红，说明判据确实是"包含某个词就算过"的假绿写法。
	const fakeGreenDropdown = "" +
		"  function renderModels(list) {\n" +
		"    const sel = $('model');\n" +
		"    const filtered = list;\n" +
		"    const seen = new Set();\n" +
		"    const uniq = [];\n" +
		"    for (const m of filtered) {\n" +
		"      if (seen.has(m.id)) continue;\n" +
		"      seen.add(m.id);\n" +
		"      uniq.push(m);\n" +
		"    }\n" +
		"    sel.innerHTML = filtered.map(m =>\n" +
		"      `<option value=\"${esc(m.id)}\">${esc(m.id)}${multTagPlain(m.id)}</option>`).join('');\n" +
		"  }\n"
	if got := modelDropdownDedupProblems(fakeGreenDropdown); len(got) == 0 {
		t.Fatal("假绿：去重循环在、但渲染用的是未去重的 filtered，判据却没报 —— " +
			"说明这条判据是『包含某个词就算过』的弱写法，必须重写")
	}
}

// --------------------------------------------------------------------------
// 小工具
// --------------------------------------------------------------------------

// jsCallStatement 截出函数体里 `name(` 起到第一个 ';' 的整条语句。
//
// 够用即可：本守卫只用来取 groupModelsByOwner 里那条单行 `g.items.push({...});`。
// 截不到时返回空串，调用方会**报红**（失败方向落在安全的那一侧）。
func jsCallStatement(body, name string) string {
	i := strings.Index(body, name)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.IndexByte(rest, ';'); j >= 0 {
		return rest[:j+1]
	}
	return rest
}

// squeezeJS 去掉所有空白，便于对"同一件事的两种排版"做同一判据
// （例如把 `{ id: m.id, fullId: m.id }` 与 `{id:m.id,fullId:m.id}` 视为同一种写法）。
func squeezeJS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isJSIdentByte 判断是否属于 JS 标识符字符（用于从 `sel.innerHTML = X.map(` 里取 X）。
func isJSIdentByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isJSIdentStart 判断能否作为标识符首字符（数字不能开头）。
func isJSIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// quoteForMsg 把读到的片段放进失败信息里（空串也要看得见）。
func quoteForMsg(s string) string {
	if s == "" {
		return "空"
	}
	return s
}

// itoa 极简十进制转换，仅用于失败信息里的下标比较
// （避免为了报个下标而引入 strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestWebUIModelIDsUnifiedChatDropdownAutoFallback 对话测试下拉的「保持原选择」回落分支。
//
// # 守的是什么（收窄列目录的**连带**后果，第一版漏了）
//
// 收窄之后 `auto` 只会以 `workbuddy/auto` 出现（裸名不再下发）。而这段回落逻辑
// 原本写的是 `shown.find(m => m.id === 'auto')` —— 拿**完整 id** 去比一个裸名，
// 永远匹配不到，于是每次渲染都把用户的选择静默换成 `shown[0]`。
//
// 这不是崩溃，是**静默改变用户意图**：`auto` 恰恰是"不指定模型"的那个默认项，
// 最该被保持的就是它。而界面上没有任何提示 —— 正是本项目最难查的那类。
//
// # 为什么判据是"必须经 multKeyOf"
//
// 归一只能有一处（见 multKeyOf 的注释）。这里再写一份 `slice(slash+1)` 也能过功能，
// 但会制造第二份判据 —— 那正是本文件反复警告的漂移形态（分组算 workbuddy、
// 筛选算成别的）。所以断言的是"**经 multKeyOf 比对**"，而不是"能匹配上就行"。
func TestWebUIModelIDsUnifiedChatDropdownAutoFallback(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	code := stripJSCommentsDeep(src)

	body, ok := extractJSFunction(code, "function renderModels(")
	if !ok {
		t.Fatal("找不到 renderModels 函数 —— 守卫无法定位（fail-open）；" +
			"改名/拆分了就要跟着改判据，而不是让它空转")
	}
	if !strings.Contains(body, "sel.innerHTML") {
		t.Fatalf("切出来的函数体不像 renderModels（缺 sel.innerHTML，共 %d 字节）—— "+
			"切分逻辑已失效，本守卫此时无论绿红都不可信", len(body))
	}

	if strings.Contains(body, "m.id === 'auto'") {
		t.Error("renderModels 里又按**完整 id** 比 'auto' 了。\n" +
			"  后果：收窄后目录里只有 `workbuddy/auto`，这个分支永远为假 —— 下拉每次渲染\n" +
			"        都把用户的选择静默换成列表第一项（auto 是「不指定模型」的默认项，最该被保持）。\n" +
			"  正确写法：经 multKeyOf 去前缀后再比（归一只有一处）。")
	}
	if !strings.Contains(body, "multKeyOf(m.id) === 'auto'") {
		t.Error("renderModels 的 auto 回落分支没有经 multKeyOf 归一。\n" +
			"  两种可能，都不可接受：① 该分支被删了（下拉不再优先回落 auto）；\n" +
			"  ② 它自己实现了一份去前缀 —— 第二份判据迟早与 multKeyOf 漂移。")
	}
}
