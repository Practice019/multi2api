package server

import (
	"regexp"
	"strings"
	"testing"
)

// TestWebUIInvitePanelIsFieldDrivenTable 钉住 T-358 的目标形状。
//
// # 背景
//
// T-355 把 codar 的邀请码面板做成"一张单账号卡片"（renderSingleAccountInvite），
// 用户否决了那个方向：要的是**一行一个账号的表格**，且这张表要**字段驱动**，
// 让 loomy（codes/activated）与 codar（my_code/invited/bind_page）共用同一张表。
//
// # 判据（全部可从 webui.html 文本断言）
//
//	① 单账号卡片与它的分支**必须消失**（否则两张渲染路径并存，用户又要看到旧形状）
//	② 存在字段驱动的列集函数 colSet( )
//	③ 存在页内模态 #pageModal + #pageModalFrame（**不能**塞进 #modal —— 那是登录弹窗）
//	④ 事件委托里有 data-linvite-page 分支（点「绑定」开模态）
//	⑤ 表头是**按列拼**出来的，不再是写死的六个 <th>
func TestWebUIInvitePanelIsFieldDrivenTable(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// ① 旧形状必须清掉
	if strings.Contains(html, "renderSingleAccountInvite") {
		t.Error("单账号卡片 renderSingleAccountInvite 仍在（T-355 的方向已被否决，应删除）")
	}
	if strings.Contains(html, "d.total == null && !Array.isArray(d.accounts)") {
		t.Error("T-355 的单账号型判据仍在（应删除，改走字段驱动表格）")
	}

	// ② 字段驱动的列集
	for _, want := range []string{"function colSet(", "colSet(accts)"} {
		if !strings.Contains(html, want) {
			t.Errorf("缺少字段驱动列集：%s", want)
		}
	}
	// 列显隐必须按"本页有没有该字段"判断，而不是按上游名
	// bind_page 是点取形式（a.bind_page），单独判。
	if !strings.Contains(html, "a.bind_page") {
		t.Error("绑定入口应按条目自带的 bind_page 判断（字段驱动）")
	}
	// 字段名可能写成 'x' 或 "x"（JS 两种引号都合法）⇒ 两种都接受，判的是"用到了这些字段"。
	for _, field := range []string{"my_code", "invited", "codes", "activated"} {
		if !strings.Contains(html, "'"+field+"'") && !strings.Contains(html, `"`+field+`"`) {
			t.Errorf("列集里应出现字段名 %s（字段驱动，不认上游）", field)
		}
	}

	// ③ 页内模态（且不能落在 #modal 里）
	for _, want := range []string{`id="pageModal"`, `id="pageModalFrame"`, "function openPageModal(", "function closePageModal("} {
		if !strings.Contains(html, want) {
			t.Errorf("缺少页内模态要素：%s", want)
		}
	}
	mi := strings.Index(html, `id="modal"`)
	pi := strings.Index(html, `id="pageModal"`)
	if mi < 0 || pi < 0 {
		t.Fatal("找不到 #modal 或 #pageModal")
	}
	// #modal 的 div 结束（它的下一个同级元素）之前不该出现 pageModal
	if pi > mi && pi < strings.Index(html[mi:], "</div>\r\n\r\n") {
		t.Error("pageModal 看起来落在 #modal 内部 —— 必须与它同级（#modal 是登录弹窗，结构固定）")
	}

	// ④ 事件委托分支
	if !strings.Contains(html, "button[data-linvite-page]") {
		t.Error("缺少 button[data-linvite-page] 的事件委托分支（点绑定要开模态）")
	}
	if !strings.Contains(html, "data-linvite-page-uid") {
		t.Error("绑定按钮上应带 data-linvite-page-uid（否则取不到同行的输入框）")
	}

	// ⑤ 表头按列拼（现写死的 <th>UID</th><th>昵称</th>… 必须变成 cols 驱动）
	if strings.Contains(html, "<th>UID</th><th>昵称</th><th>激活状态</th><th>余额</th><th>我生成的码</th><th>绑定别人的码</th>") {
		t.Error("表头仍是写死的六个 th —— 应改成按 colSet 拼（否则 codar 会多出空的表头列）")
	}
}

// TestWebUIInviteTableStructureGuards T-360：三类**只有运行期才暴露**的结构错误。
//
// 来源：T-358 的交付里 node --check 与 Go 测试全绿，但浏览器一打开就炸 ——
// 全是"作用域/未定义变量"这一类：语法合法、类型检查看不见、只有真跑才报。
//
// 三条护栏（纯文本断言，不需要 JS 引擎）：
//
//	a) colSet 与 openPageModal/closePageModal 必须**同级**（都顶格两空格），
//	   且 colSet 在遇到 openPageModal 之前**已经 return** —— 否则那些函数被嵌进 colSet 体内：
//	   ① 它们成了局部符号（调用处 ReferenceError）② 每次 renderPage 都往 document 挂新监听器（泄漏）。
//	b) renderPage 体内必须有 `const accts` —— 少了它整个面板 ReferenceError 白屏。
//	c) 调用 rowHTML 时**必须**带第二参数（cols）—— 懒加载补行时不带就会 TypeError，行永远停在「正在读取…」。
func TestWebUIInviteTableStructureGuards(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// a) 三者同级 + colSet 先 return
	cs := strings.Index(html, "\n  function colSet(")
	op := strings.Index(html, "\n  function openPageModal(")
	cl := strings.Index(html, "\n  function closePageModal(")
	if cs < 0 || op < 0 || cl < 0 {
		t.Fatalf("找不到同级定义（colSet=%d openPageModal=%d closePageModal=%d）", cs, op, cl)
	}
	if !(cs < op && op < cl) {
		t.Error("colSet / openPageModal / closePageModal 应按此顺序同级出现（都顶格两空格）")
	}
	if !strings.Contains(html[cs:op], "return {") {
		t.Error("colSet 在 openPageModal 之前还没 return ⇒ 那些函数被嵌进了 colSet 体内（调用处会 ReferenceError，且每次渲染都会往 document 挂新监听器）")
	}

	// b) renderPage（邀请面板那个）体内必须有 const accts
	li := strings.Index(html, "async function loadLoomyInvite")
	ri := strings.Index(html[li:], "const renderPage = (d) => {")
	if li < 0 || ri < 0 {
		t.Fatal("找不到 loadLoomyInvite / 它的 renderPage")
	}
	body := html[li+ri:]
	if end := strings.Index(body, "\n    };"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "const accts") {
		t.Error("renderPage 体内缺 `const accts = Array.isArray(d.accounts) ? d.accounts : [];` ⇒ 打开面板会 ReferenceError 白屏")
	}
	if !strings.Contains(body, "const cols = colSet(accts)") {
		t.Error("renderPage 体内应有 `const cols = colSet(accts);`")
	}

	// c) rowHTML 的调用不许漏第二参数
	// ⚠ 只在**邀请面板体内**判：tasks 面板有自己的单参数 rowHTML(a)（合法，不动它）。
	// 本条的意图是"邀请面板的每一处 rowHTML 调用都必须拿到 cols"。
	li2 := strings.Index(html, "async function loadLoomyInvite")
	body2 := html[li2:]
	if j := strings.Index(body2, "\n  async function "); j > 0 {
		body2 = body2[:j]
	}
	if m := regexp.MustCompile(`rowHTML\(a\)`).FindString(body2); m != "" {
		t.Error("邀请面板体内存在 `rowHTML(a)`（没带 cols）⇒ 懒加载补行时会 TypeError，那一行永远停在「正在读取…」")
	}
	if !strings.Contains(body2, "rowHTML(a, inviteCols)") {
		t.Error("邀请面板的补行调用应写作 rowHTML(a, inviteCols)（与 renderPage 同一份 cols）")
	}
}

// TestWebUIInviteLoaderHeadAndLazyChunk T-360 追加：覆盖另两类"只有浏览器才报"的错误。
//
// 这两条都是我自己在本轮踩到的（插入时把原行顶掉），Playwright 抓到的原文：
//
//	ReferenceError: host is not defined      ← loadLoomyInvite 的 `const host = panelSkeleton(…)` 被顶掉
//	（以及）懒加载块里 `const html = a.ok ? …` 被拆成裸 return ⇒ html 未定义（node --check 看不见）
//
// ⇒ 护栏：loader 头部必须有 host 声明；懒加载补行块必须有 `const html` 且用三目。
func TestWebUIInviteLoaderHeadAndLazyChunk(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)
	li := strings.Index(html, "async function loadLoomyInvite")
	if li < 0 {
		t.Fatal("找不到 loadLoomyInvite")
	}
	body := html[li:]
	if j := strings.Index(body, "\n  async function "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "const host = panelSkeleton(providerId, 'invite')") {
		t.Error("邀请面板 loader 头部缺 `const host = panelSkeleton(providerId, 'invite')` ⇒ 面板会卡在「加载中…」（ReferenceError: host is not defined）")
	}
	if !strings.Contains(body, "const html = a.ok ? withRowId(rowHTML(a, inviteCols)") {
		t.Error("懒加载补行块缺 `const html = a.ok ? withRowId(rowHTML(a, inviteCols)…)` ⇒ html 未定义，行永远停在「正在读取…」")
	}
	if !strings.Contains(body, "colCountOf(inviteCols)") {
		t.Error("占位行必须用 colCountOf(inviteCols)（列数是动态的，硬编码 colspan 会错位）")
	}
}

// TestWebUIInviteBoundRowKeepsViewEntry T-362：**已绑定**的行也必须保留入口。
//
// # 为什么（用户实测的原话："根本没有绑定功能"）
//
// 两个 codar 账号后来都变成 invited=true（上游状态，会随时间变化：注册时没被邀请、事后绑定）。
// 于是「绑定别人的码」那格两行都画成 `—` ⇒ 用户看到的是"这个上游没有绑定功能"，
// 而且他**再也打不开邀请页** —— 而那正是他要核对的（自己的邀请记录/绑定状态）。
//
// ⇒ 入口必须**恒在**（有 bind_page 就给「查看」），只有**可提交的输入框**按状态显隐。
func TestWebUIInviteBoundRowKeepsViewEntry(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// ① 有「查看」按钮（带 bind_page 与 uid 两个 data 属性，否则取不到路径/账号）
	if !strings.Contains(html, `data-linvite-view="`) {
		t.Error("已绑定的行没有「查看」入口（data-linvite-view）⇒ 用户会以为「根本没有绑定功能」，且打不开邀请页")
	}
	if !strings.Contains(html, "data-linvite-view-uid=") {
		t.Error("「查看」按钮应带 data-linvite-view-uid（否则不知道该开哪个账号的邀请页）")
	}
	// ② 事件委托分支存在
	if !strings.Contains(html, "button[data-linvite-view]") {
		t.Error("缺少 button[data-linvite-view] 的事件委托分支（点了不会开弹窗）")
	}
	// ③ 「查看」自己的那次 openPageModal 调用不许带 code=。
	//    判据要收窄到**那一次调用**：上一版扫描窗口一直吃到后面的 page 分支，
	//    而 page 分支本来就要拼 code=（那是误报，不是实现错）。
	li := strings.Index(html, "button[data-linvite-view]")
	if li < 0 {
		t.Fatal("找不到 view 委托分支")
	}
	seg := html[li:]
	if j := strings.Index(seg, "openPageModal("); j >= 0 {
		call := seg[j:]
		if k := strings.Index(call, ");"); k > 0 {
			call = call[:k]
		}
		if strings.Contains(call, "code=") {
			t.Errorf("「查看」的调用不应拼 code=（查看只是打开页面，不提交绑定）：%s", call)
		}
		if !strings.Contains(call, "邀请码 · ") {
			t.Errorf("「查看」的弹窗标题应区分于「绑定邀请码 · …」：%s", call)
		}
	} else {
		t.Error("「查看」分支应调用 openPageModal 打开邀请页")
	}
	// ④ 已绑定的行仍要有 codes 型的 —（loomy 没有页面，点开也没东西看）
	if !strings.Contains(html, "a.bind_page") {
		t.Error("三态判据应基于 bind_page / invited / activated 三个字段")
	}
}

// TestWebUIDelegationDeclarationsAreHandled 事件委托的结构完整性（T-363）。
//
// # 背景（我引入的真实事故）
//
// 在 T-362 里我插入「查看」分支时，把紧邻的**批量导入**处理行吃掉了：
//
//	// ---- 批量导入（账号池分组行按钮，见 openImportModal）----
//	const imp = ev.target.closest('button[data-gimport]');
//	if (imp) { openImportModal(imp.dataset.gimport); return; }   ← 这一行被我删了
//
// 后果：账号池分组行的「批量导入」按钮**点了没反应**（imp 成了未使用变量）。
// node --check 与既有文本断言都抓不到 —— 语法合法、没人断言过这一对。
//
// # 两条判据
//
//	a) data-gimport 这一对必须同时在（声明 + if + openImportModal 调用）
//	b) 通用：每一个 `const X = ev.target.closest('button[…]')` 声明，都必须在 40 行内
//	   **被用到**（`if (X` 或 `X.`）—— 这条抓的是"声明了但没处理"的整类事故。
func TestWebUIDelegationDeclarationsAreHandled(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)
	lines := strings.Split(html, "\n")

	// a) gimport 三件套
	gi := -1
	for i, l := range lines {
		if strings.Contains(l, "closest('button[data-gimport]')") {
			gi = i
			break
		}
	}
	if gi < 0 {
		t.Fatal("找不到 button[data-gimport] 的声明")
	}
	found := false
	for j := gi; j < len(lines) && j <= gi+4; j++ {
		if strings.Contains(lines[j], "if (imp)") {
			found = true
			if j+1 < len(lines) && !strings.Contains(lines[j], "openImportModal(") {
				t.Error("「批量导入」的 if 里没调用 openImportModal（按钮会点了没反应）")
			}
			break
		}
	}
	if !found {
		t.Error("button[data-gimport] 声明后缺少 `if (imp) { openImportModal(…); return; }` ⇒ 「批量导入」按钮点了没反应（T-363 的真实事故）")
	}

	// b) 通用：声明必须被用到
	re := regexp.MustCompile(`const\s+(\w+)\s*=\s*ev\.target\.closest\('button\[`)
	checked, dead := 0, 0
	for i, l := range lines {
		m := re.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		checked++
		name := m[1]
		used := false
		for j := i + 1; j < len(lines) && j <= i+40; j++ {
			if regexp.MustCompile(`if\s*\(\s*`+regexp.QuoteMeta(name)+`\b`).FindString(lines[j]) != "" ||
				regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\.`).FindString(lines[j]) != "" {
				used = true
				break
			}
		}
		if !used {
			dead++
			t.Errorf("L%d：`const %s = ev.target.closest('button[…]')` 声明后 40 行内没被用到（if 或 属性访问）⇒ 多半是又一个「声明了但没处理」（T-363 的批量导入就是这么没的）", i+1, name)
		}
	}
	if checked == 0 {
		t.Error("一条 closest 声明都没扫到 ⇒ 判据失效（空匹配即失败）")
	}
	_ = dead
}

// TestWebUIDoStartReadsLoginOptions T-365：登录弹窗里用户选的「登录方式」必须被读回请求体。
//
// # 背景（真实用户可见）
//
// 文件里一直在渲染 `<select data-loginopt="…">`（登录选项，见 manifest 的 login.options），
// 但 doStart 里回读它的 4 行被误删了 ⇒ 用户选「国际站」也没用：options 一直是空对象，
// 后端落回 default ⇒ 点国际站却开国内站授权页。
// manifest 里目前唯一声明 login.options 的上游就是 monkeycode（key=region: cn/intl）。
//
// ⇒ 判据：只要文件里存在 data-loginopt 的 select，doStart 体内就必须有回读循环 + 赋值。
func TestWebUIDoStartReadsLoginOptions(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)
	if !strings.Contains(html, `data-loginopt="${esc(o.key)}"`) {
		t.Skip("当前没有渲染 login.options 的 select（上游都没声明）—— 这条护栏此时不适用")
	}
	di := strings.Index(html, "async function doStart()")
	if di < 0 {
		t.Fatal("找不到 doStart")
	}
	body := html[di:]
	if j := strings.Index(body, "\n    function "); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "for (const o of loginOpts) {") {
		t.Error("doStart 里没有回读 loginOpts 的循环 ⇒ 用户在弹窗里选的「登录方式」不会进请求体（选了国际站也开国内站）")
	}
	if !strings.Contains(body, "chosen[o.key] = el.value") {
		t.Error("doStart 里没有 `chosen[o.key] = el.value` ⇒ 选项值从未被读取")
	}
	if !strings.Contains(body, "await admin('/admin/login/start'") {
		t.Error("doStart 里看不到 start 调用（判据失效）")
	}
}

// TestWebUIInviteWechatColumn T-365②：邀请码表格的「微信」列（扫码绑定）。
//
// # 契约（后端 T-364 冻结）
//
//	条目新增 wechat_bound / wechat_bound_known / wechat_page 三个字段；
//	页面 GET /admin/codar/wechat-page?uid=… 负责扫码与轮询。
//
// # 三态 + 恒在的入口
//
//	known=false ⇒ —；bound=false ⇒ 未绑定 +「扫码绑定」；bound=true ⇒ 已绑定 +「查看」。
//	⚠ 已绑定也保留入口：用户要核对自己的绑定状态（与 T-362「查看」同一条理由）。
func TestWebUIInviteWechatColumn(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)
	// 列集开关：按字段存在与否，不认上游名
	if !strings.Contains(html, "wechat: has('wechat_bound')") {
		t.Error("colSet 里缺 wechat 开关（判据应是字段有没有 wechat_bound）")
	}
	// 三个字段都要被用到
	for _, f := range []string{"a.wechat_bound_known", "a.wechat_bound", "a.wechat_page"} {
		if !strings.Contains(html, f) {
			t.Errorf("微信列没用到字段 %s（三态与入口都要靠它）", f)
		}
	}
	// 两种按钮（扫码绑定 / 查看）+ 事件委托
	if !strings.Contains(html, "data-wechat-page=") {
		t.Error("缺 data-wechat-page 按钮（扫码绑定/查看）")
	}
	if !strings.Contains(html, "data-wechat-page-uid=") {
		t.Error("按钮应带 data-wechat-page-uid（否则不知道该开哪个账号的微信页）")
	}
	if !strings.Contains(html, "button[data-wechat-page]") {
		t.Error("缺 button[data-wechat-page] 的事件委托分支（点了不会开弹窗）")
	}
	// 复用现有模态，不许新建第二个
	if strings.Contains(html, `id="wechatModal"`) || strings.Contains(html, `id="wechatPageModal"`) {
		t.Error("不该新建第二个模态 —— 复用 #pageModal / openPageModal")
	}
	// 表头受同样的开关控制
	if !strings.Contains(html, "${cols.wechat ? '<th>微信</th>' : ''}") {
		t.Error("表头的微信列没按 cols.wechat 拼")
	}
	// 列顺序：微信紧挨在「我的邀请码」之后、「绑定别人的码」之前
	mi := strings.Index(html, `<th>我的邀请码</th>`)
	wi := strings.Index(html, `<th>微信</th>`)
	oi := strings.Index(html, `<th>绑定别人的码</th>`)
	if mi < 0 || wi < 0 || oi < 0 {
		t.Fatalf("表头锚点缺失（我的邀请码=%d 微信=%d 绑定别人的码=%d）", mi, wi, oi)
	}
	if !(mi < wi && wi < oi) {
		t.Error("列顺序应为 … 我的邀请码 → 微信 → 绑定别人的码")
	}
}

// TestWebUIInvitePartialDataIsNotAnError T-369：部分成功要能显示，而不是整行红掉。
//
// # 契约变更（后端 T-368）
//
//	新增 invited_known（false ⇒ 这次没查到 invited，该值不可信）与 warning（可缺省）。
//	ok 的含义收窄为「拿到了 my_code」⇒ **ok=true 但 invited/balance 未知是正常的**。
//
// # 三条判据
//
//	a) 绑定状态格必须三态：invited_known === false ⇒ —；true ⇒ 已绑定；否则 ⇒ 未绑定
//	   ⚠ 必须用 `=== false`：loomy 的老响应没有这个字段，用 !v 会把它们全变成 —
//	b) a.warning 非空时要渲染「部分数据缺失」（挂在最后一格末尾，**不新增列**）
//	c) 列数不许因 warning 变化
func TestWebUIInvitePartialDataIsNotAnError(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// a) 三态 + 必须用 === false
	if !strings.Contains(html, "a.invited_known === false") {
		t.Error("绑定状态格没有 invited_known 三态判据（且必须是 `=== false` —— 老响应没这个字段时用 !v 会全变成 —）")
	}
	if !strings.Contains(html, "这次没查到邀请状态") {
		t.Error("缺「这次没查到邀请状态」的提示（用户要能区分「未绑定」与「没查到」）")
	}
	// b) warning 渲染
	if !strings.Contains(html, "部分数据缺失") {
		t.Error("没有渲染 a.warning 的「部分数据缺失」标记 ⇒ 部分成功对用户等于静默丢数据")
	}
	if !strings.Contains(html, "esc(String(a.warning))") && !strings.Contains(html, "esc(a.warning)") {
		t.Error("「部分数据缺失」的 title 应带上 a.warning 原文（排障要看它）")
	}
	// c) 不许新增列：表头里出现 warning 相关 th 就是加列了
	li := strings.Index(html, "host.innerHTML = head + `<table")
	if li > 0 {
		seg := html[li:]
		if j := strings.Index(seg, "</tr></thead>"); j > 0 {
			seg = seg[:j]
		}
		if strings.Contains(seg, "warning") || strings.Contains(seg, "缺失") {
			t.Error("warning 不该新增列（列已经 8 个）—— 它应挂在最后一格末尾")
		}
	}
}

// TestWebUIInviteBalancePartialMarker T-372②：余额可能偏低时要标出来，而不是让用户以为就这么多。
//
// # 契约（后端 T-371）
//
//	balance_partial: true ⇒ 只取到 agent 桶（general/rewrite 未取到）⇒ balance **可能偏低**
//	（实况：老号三桶齐=4206，只 agent 桶=1706）
//
// # 判据必须是 `=== true`
//
//	老响应（loomy）没有这个字段，用 `!== false` 会给它凭空加标记 —— 而 loomy 的 balance 语义不同。
func TestWebUIInviteBalancePartialMarker(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	if !strings.Contains(html, "a.balance_partial === true") {
		t.Error("缺 `balance_partial === true` 判据（且必须是 === true：老响应没这字段，用 !== false 会给 loomy 凭空加标记）")
	}
	if strings.Contains(html, "balance_partial !== false") {
		t.Error("不该用 `balance_partial !== false`（loomy 没有该字段，会被误标）")
	}
	if !strings.Contains(html, `title="只取到部分桶`) {
		t.Error("「部分」标记应带 title 说明（用户要知道「实际可能更高」）")
	}
	// 标记文案与位置：必须在余额格那一行里，且不新增列
	bi := strings.Index(html, "balance_partial === true")
	if bi < 0 {
		t.Fatal("找不到判据")
	}
	lineStart := strings.LastIndex(html[:bi], "\n")
	lineEnd := strings.Index(html[bi:], "\n")
	line := html[lineStart : bi+lineEnd]
	if !strings.Contains(line, "<td") {
		t.Error("「部分」标记必须挂在**余额那一格**里（不是新开一格）")
	}
	if !strings.Contains(line, "balance_known === false") {
		t.Error("余额格应保留 `balance_known === false ⇒ —` 的既有判断")
	}
	// 不新增列
	li := strings.Index(html, "host.innerHTML = head + `<table")
	if li > 0 {
		seg := html[li:]
		if j := strings.Index(seg, "</tr></thead>"); j > 0 {
			seg = seg[:j]
		}
		if strings.Contains(seg, "balance_partial") || strings.Contains(seg, ">部分<") {
			t.Error("balance_partial 不该新增列（列已经 8 个）")
		}
	}
}
