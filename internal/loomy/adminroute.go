// adminroute.go Loomy 实现 gateway.AdminExt —— 一条**只读诊断**端点。
//
// # 为什么 loomy 需要一条管理端点（上一轮的结论是"不需要"，这是订正）
//
// 上一轮 loomy 刻意不实现 AdminExt，理由是"没有可用的管理端点：积分余额只在
// 客户端本地缓存里，凭空造一条只会是个假按钮"。**那条理由在"没有额度接入"
// 的前提下成立**，但本轮已经实现了 `QuotaExt`（从同一个本地缓存读额度），
// 于是前提变了 —— 现在 loomy **确实**有一条能自证的数据通道。
//
// 更硬的一条约束来自契约本身：`gateway.unverifiableCaps` 把
// `CapQuotaProbe` 归成"无法自动行为验证"的能力位，并要求
//
//	**声明了这些能力位的上游必须实现 AdminExt，且路由列表非空、结构完整、
//	 handler 调用不 panic**（见 contract.go 的 probeCapabilities / verifyAdminRoutes）
//
// 那条检查的存在理由正是"挡住只声明不实现"。所以要让 loomy 的账号行出现
// 「额度」按钮（前端按 `hasCap(pid,'quota-probe')` 决定），就必须同时给出
// 一条**真的存在且真的有用**的管理端点 —— 而不是让契约放宽。
//
// # 这条端点为什么是"真的有用"，不是为过检查而造的空壳
//
// 它回答的正是用户最初的问题「探究一下怎回事」：
//
//	客户端数据目录在哪（配对了没有）
//	本机客户端里读到的是哪个账号（uid/昵称/登录时间）
//	积分缓存里的两个账本各是多少、**是什么时候写的**
//	缓存是不是今天的（跨日则 dailyBalance 已经不代表今天）
//
// 这些都是"账户列表那一格为什么是 `—`"的唯一可查依据。没有它，
// 「额度未知」与「额度刷新没接线」在界面上**长得一模一样**。
//
// # 为什么 Hidden
//
// 它是**诊断**端点，不是给人天天点的按钮：
//
//  1. 它不发任何改变状态的请求（纯读），却会返回本机的账号信息 ——
//     把它渲染成面板入口会鼓励用户去点一个什么都不改变的东西。
//  2. 真正的"刷新额度"动作在**核心**的 `/admin/accounts/quota/refresh`
//     （它按每个账号自己的上游分派到 QuotaExt 并写回池）。这里再出现一个
//     "刷新额度"按钮会变成第二个入口，两个入口迟早各说一套。
//
// `Hidden` 的语义由 `gateway.AdminRoute.Hidden` 定义：路由照挂
// （HTTP 语义不变），只是"要不要为它生成一个通用面板"的答案是"不要"。
package loomy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// clientStorePath 本端点的路径。
//
// 刻意带上上游前缀（`/admin/loomy/`）：另两个上游的路由名是**语义化**的
// （`/admin/growth`、`/admin/welfare`），而那些语义是它们**独占**的概念。
// "本机客户端存储"同样只属于 loomy，带前缀可以让"这条路由是谁的"
// 在 URL 上自证 —— 排障时看日志里的路径就够了，不必去翻注册表。
const clientStorePath = "/admin/loomy/client-store"

// AdminRoutes 返回 loomy 的管理端点（gateway.AdminExt）。
func (p *Provider) AdminRoutes() []gateway.AdminRoute {
	return []gateway.AdminRoute{
		{
			Method:     http.MethodGet,
			Path:       clientStorePath,
			Handler:    p.handleClientStore,
			Capability: gateway.CapQuotaProbe,
			Title:      "本机客户端存储",
			Hidden:     true,
		},
		// ── 手机号验证码登录（本轮新增）────────────────────────────────
		//
		// 三条路由构成一个最小的自助登录页：
		//
		//	GET  /admin/loomy/login          表单页（authURL 指向它）
		//	POST /admin/loomy/login/sms      发验证码 → msgid
		//	POST /admin/loomy/login/verify   验码 → 凭证就绪
		//
		// 三条都 Hidden：它们**不是面板入口**（账号池那一行的「添加账号」
		// 才是入口），只在登录的那一次交互里被用到。
		{
			Method:  http.MethodGet,
			Path:    loginPagePath,
			Handler: p.handleLoginPage,
			Title:   "手机号登录",
			Hidden:  true,
		},
		{
			Method:  http.MethodPost,
			Path:    loginPagePath + "/sms",
			Handler: p.handleLoginSendSMS,
			Title:   "发送验证码",
			Hidden:  true,
		},
		{
			Method:  http.MethodPost,
			Path:    loginPagePath + "/verify",
			Handler: p.handleLoginVerify,
			Title:   "验证码登录",
			Hidden:  true,
		},
		// ── 本轮新增：两个**专属标签页**（新手任务 / 邀请码）────────────
		//
		//  这两条 GET **不是** hidden —— 正是"非 hidden 的 GET 路由"
		// 才会让前端为它生成一个面板（见 webui.html 的 buildProviderPanels）。
		// 它们各声明一个新能力位，于是导航里多出两个标签页，
		// 而**账号池那张表一行没改**（用户的要求）。
		//
		// 写动作（一键完成 / 绑定邀请码）是 hidden 的 POST：
		// 由面板里的按钮调用，不该各自再占一个导航项。
		{
			Method:     http.MethodGet,
			Path:       tasksPath,
			Handler:    p.handleTasksList,
			Capability: gateway.CapTasks,
			Title:      "新手任务",
		},
		{
			Method:     http.MethodPost,
			Path:       tasksAutoPath,
			Handler:    p.handleTasksAuto,
			Capability: gateway.CapTasks,
			Title:      "一键完成（单账号）",
			Hidden:     true,
		},
		{
			Method:     http.MethodPost,
			Path:       tasksAllPath,
			Handler:    p.handleTasksAutoAll,
			Capability: gateway.CapTasks,
			Title:      "一键完成（全部账号）",
			Hidden:     true,
		},
		{
			Method:     http.MethodGet,
			Path:       invitePath,
			Handler:    p.handleInviteList,
			Capability: gateway.CapInvite,
			Title:      "邀请码",
		},
		{
			Method:     http.MethodPost,
			Path:       inviteBindPath,
			Handler:    p.handleInviteBind,
			Capability: gateway.CapInvite,
			Title:      "绑定邀请码",
			Hidden:     true,
		},
		{
			Method:     http.MethodPost,
			Path:       importPath,
			Handler:    p.handleImport,
			Capability: gateway.CapInvite,
			Title:      "批量导入账号",
			Hidden:     true,
		},
	}
}

// 编译期断言：Provider 实现管理端点扩展点。
var _ gateway.AdminExt = (*Provider)(nil)

// clientStoreView 诊断端点的回执。
//
// 字段全部是**本机读取的事实**，没有一个是从上游猜的 ——
// 这也是这条端点存在的意义：让"为什么那一格是 `—`"有据可查。
type clientStoreView struct {
	Provider   string `json:"provider"`
	DataDir    string `json:"data_dir"`
	Configured bool   `json:"configured"`
	// Source 本机数据目录是怎么定下来的（配置 / 自动探测 / 找不到）。
	//
	// 单独一个字段而不是一句日志：用户看到的"读不到"有两个完全不同的原因
	//（配错了目录 / 本机压根没有客户端），把它们区分开是排障的第一步。
	Source string `json:"source"`
	// Session 本机客户端里读到的登录态（**已脱敏**，见 sessionView）。
	Session *sessionView `json:"session,omitempty"`
	// Points 本机客户端缓存的积分摘要（原样字段）。
	Points *pointsView `json:"points,omitempty"`
	// Note 一句人话，说明当前这条端点的结论。
	Note string `json:"note,omitempty"`
	// Error 读取过程中的失败原因（读目录失败等）。空表示没有失败。
	Error string `json:"error,omitempty"`
}

// sessionView 登录态的**脱敏**视图。
//
// # 为什么不直接回 session
//
// 本仓库已经吃过一次真实凭证泄漏（测试夹具里抄了作者的活 session），
// 所以这里按"默认不回凭证材料"写：
//
//	UID / 昵称 / 登录时间   可回（它们本来就是账号池里公开展示的字段）
//	session_len / shape    可回（诊断"抄漏了字符"要靠它）
//	session 本身            **绝不回**，连前缀都不回
//
// `shape` 而不是 `fingerprint`：fingerprint 需要一个哈希，
// 而任何从 session 派生的短串都仍然是把凭据材料搬上了界面 ——
// 而"是不是 32 位小写 hex"已经足够覆盖真实的诊断需求
// （手册第 5.2 节手工抄写最容易出的错就是漏字符/混入大写）。
type sessionView struct {
	UID         string `json:"uid"`
	Nickname    string `json:"nickname"`
	LoggedInAt  string `json:"logged_in_at,omitempty"`
	SessionLen  int    `json:"session_len"`
	SessionForm string `json:"session_shape"`
	ScannedFile string `json:"scanned_files,omitempty"`
}

// pointsView 积分摘要 + **新鲜度**。
//
// 新鲜度是本端点最有价值的一项：`DailyBalance` 在跨日之后已经不代表
// 今天的额度（服务端自动重置），而缓存里那个数不会自己变。
// 不分青红皂白地展示它，等于把一个昨天的读数说成今天的。
type pointsView struct {
	Balance              int64  `json:"balance"`
	DailyBalance         int64  `json:"daily_balance"`
	DailyConsumedPoints  int64  `json:"daily_consumed_points"`
	DailyRemainingPoints int64  `json:"daily_remaining_points"`
	UpdatedAt            string `json:"updated_at,omitempty"`
	// IsToday 缓存是否写在**今天**（CST 自然日，与服务端重置同一时区）。
	IsToday bool `json:"is_today"`
}

// handleClientStore GET /admin/loomy/client-store —— 只读诊断。
//
// 契约要求 handler 在**裸请求**下也不 panic（`verifyAdminHandlersNoPanic`
// 会真的调一次并 recover），所以这里全程不假设调用方提供了任何东西：
// 没有 body、没有 query、Provider 可能连凭证目录都没配。
func (p *Provider) handleClientStore(w http.ResponseWriter, r *http.Request) {
	view := clientStoreView{
		Provider: providerID,
		Source:   "auto",
	}

	dir, source, exists := p.clientStoreDirWithSource()
	view.DataDir = dir
	view.Source = source
	view.Configured = exists

	if !exists {
		// 两种"不可用"必须分开说（见 clientStoreDirOf 的注释）：
		// 配错了路径 vs 本机压根没有客户端 —— 前者的下一步动作是**改配置**。
		if source == "config-missing" {
			view.Note = fmt.Sprintf("loomy.client_data_dir 指向的 %s 不是一个目录。"+
				"额度与「添加账号」都需要一个可读的 Local Storage 目录。", dir)
		} else {
			view.Note = "本机没有 Loomy 客户端数据目录（Local Storage/leveldb）。" +
				"额度与「添加账号」都需要它；" +
				"网关部署在另一台机器时可配 loomy.client_data_dir，" +
				"或手工放置凭证文件后点「重载 auths」。"
		}
		writeClientStoreJSON(w, view)
		return
	}

	sess, source, err := ReadLocalAuth(dir)
	if err != nil {
		view.Error = err.Error()
		view.Note = "读取客户端存储失败 —— 额度与「添加账号」都会显示为未知。"
		writeClientStoreJSON(w, view)
		return
	}
	if sess == nil {
		view.Note = "目录在，但没有 loomy-auth-session —— 客户端尚未登录" +
			"（或登录态已随登出被清掉）。请在客户端登录后重试。" + sourceHint(source)
		writeClientStoreJSON(w, view)
		return
	}
	view.Session = &sessionView{
		UID:        sess.UID,
		Nickname:   maskDisplay(sess.Nickname),
		LoggedInAt: sess.LoggedInAt,
		// 长度与形态是诊断"抄漏了字符"的依据，且**不泄漏任何字节**。
		SessionLen:  len(sess.Session),
		SessionForm: sessionShape(sess.Session),
		ScannedFile: source,
	}

	pts, perr := ReadLocalPoints(dir)
	if perr != nil {
		view.Error = perr.Error()
	}
	if pts == nil {
		view.Note = "登录态读到了，但没有积分缓存（loomy-points-summary）—— " +
			"客户端还没把摘要写下来，所以额度显示为 `—`。"
		writeClientStoreJSON(w, view)
		return
	}
	view.Points = &pointsView{
		Balance:              pts.Balance,
		DailyBalance:         pts.DailyBalance,
		DailyConsumedPoints:  pts.DailyConsumedPoints,
		DailyRemainingPoints: pts.DailyRemainingPoints,
		UpdatedAt:            pts.UpdatedAt,
		IsToday:              !storeDayChanged(pts.UpdatedAt, time.Now()),
	}
	view.Note = "账号列表的「额度」列取的是 balance（总余额，不随自然日重置）；" +
		"daily_balance 是每日额度，跨日由服务端自动重置 —— " +
		"is_today=false 时那个数已经不是今天的了。"
	writeClientStoreJSON(w, view)
}

// clientStoreDirOf 决定客户端数据目录，并说明依据与**它是否真的在**。
//
// 返回值：
//
//	(目录, "config",       true)   配置显式给了路径，且它确实是个目录
//	(目录, "config-missing", false) 配置给了路径，但那不是一个目录
//	(目录, "auto",         true)   自动探测到了
//	("",   "missing",      false)  都没有
//
// # 为什么把 exists 单独返回（而不是让调用方自己 stat）
//
// 两个调用方要的**不是同一件事**：
//
//	effectiveClientDir 要"用户说在哪" → 拿着配置的路径去试，
//	                   失败了给出**指向那个路径**的明确报错（而不是一句
//	                   "本机没有客户端"，那会把"配错了"说成"没装"）
//	Configured         要"能不能**真的**做到" → 目录不存在就不能给按钮，
//	                   否则用户点下去只会拿到一个失败
//
// 混成一个"清理过的路径"会丢掉第一种用途必需的信息（配错的路径本身），
// 于是错误提示只能退化成泛泛的一句 —— 而那正是本项目反复批评的形态。
//
//	抽成**自由函数**而不是 Provider 的方法，是为了让"启动日志"
//
// （ClientStoreHint）与"诊断端点"走**同一份实现**。两处各写一遍判断，
// 迟早会出现"日志说探测到了、端点说没有"这种让人怀疑自己眼睛的分叉。
func clientStoreDirOf(configured string) (dir string, source string, exists bool) {
	if c := strings.TrimSpace(configured); c != "" {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c, "config", true
		}
		return c, "config-missing", false
	}
	if d := DefaultClientStoreDir(); d != "" {
		return d, "auto", true
	}
	return "", "missing", false
}

// clientStoreDirWithSource 同 clientStoreDirOf，用 Provider 上配置的值。
func (p *Provider) clientStoreDirWithSource() (string, string, bool) {
	if p == nil {
		return clientStoreDirOf("")
	}
	return clientStoreDirOf(p.clientDataDir)
}

// ClientStoreHint 返回一行「客户端数据目录」的**启动日志**。
//
// # 为什么它值得存在（而不是只在诊断端点里说）
//
// "「添加账号」按钮为什么不在"是本轮用户提的第一个问题，而它有三个
// 需要分开说的原因：
//
//	配了路径但它不存在       → 用户能改，报错里给出那个路径就能看出来
//	自动探测不到             → 正常部署形态（网关在服务器上），无需修
//	探测到了                 → 可用，顺便把路径打出来便于核对
//
// 不把这三件事在**启动时**说清楚，用户只能看到一个消失的按钮。
// 诊断端点（GET /admin/loomy/client-store）给出同一份事实，
// 但需要用户先知道有那条端点 —— 启动日志不需要。
func ClientStoreHint(configured string) string {
	dir, source, _ := clientStoreDirOf(configured)
	switch source {
	case "config":
		return fmt.Sprintf("客户端数据目录 %s（来自 loomy.client_data_dir）—— "+
			"「添加账号」与「额度」可用", dir)
	case "auto":
		return fmt.Sprintf("客户端数据目录 %s（自动探测）—— 「添加账号」与「额度」可用", dir)
	case "config-missing":
		return fmt.Sprintf("loomy.client_data_dir 指向的 %s 不是一个目录 —— "+
			"「添加账号」按钮不会出现、「额度」显示为 `—`；"+
			"请核对这个路径（Windows 实测是 %%APPDATA%%\\Loomy\\Local Storage\\leveldb）", dir)
	default:
		return "本机未找到 Loomy 客户端数据目录（Local Storage/leveldb）—— " +
			"「添加账号」按钮不会出现、「额度」显示为 `—`。" +
			"网关不在客户端那台机器上时这是正常形态；" +
			"否则请设置 loomy.client_data_dir，或手工放置凭证文件后点「重载 auths」"
	}
}

// sessionShape 描述 session 的形态 —— **不泄漏任何字符**。
func sessionShape(s string) string {
	if looksLikeSession(s) {
		return "32 位小写 hex（与实测形态一致）"
	}
	if s == "" {
		return "空"
	}
	return "非 32 位小写 hex（上游可能改了格式，或手工抄写有误）"
}

// maskDisplay 把展示名脱敏。
//
// # 为什么需要它（而不是直接用 Auth.Nickname）
//
// `Auth.Nickname` 的顺序是 MaskedPhone → Phone → uid 前缀。也就是说
// **手机号掩码缺失时它会回落成完整手机号**（ParseCredential 的
// firstNonEmpty(a.MaskedPhone, a.Phone) 里那一步）。
// 账号池的表格里展示完整手机号是用户自己的选择，但一个**诊断端点**
// 不该把完整号码回给任何调用方 —— 那是最容易被忽略的泄漏面。
//
// 已经带 `*` 的（本来就掩码过）原样保留；否则保留首 3 末 2，
// 中间全部打码 —— 位数越长的串打码越彻底，同时仍然可辨认。
func maskDisplay(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "*") {
		return s
	}
	rs := []rune(s)
	if len(rs) <= 4 {
		return strings.Repeat("*", len(rs))
	}
	// 首 3 末 2：对手机号（11 位）与 uid 前缀都够辨认。
	head, tail := 3, 2
	if len(rs) < head+tail+1 {
		head, tail = 1, 1
	}
	return string(rs[:head]) + strings.Repeat("*", len(rs)-head-tail) + string(rs[len(rs)-tail:])
}

// writeClientStoreJSON 写 JSON 回执。
//
// 自带一个很小的序列化：本包其余部分只做转发，没有引入过 JSON 回执的先例
// （core 的 writeJSON 在 internal/admin，本包**不许**依赖它 ——
// 那正是"加新上游核心零改动"的反面）。
//
// 序列化失败时**不写半截响应**：先算完再写，失败就回 500 一个空对象。
// 写一半会让客户端拿到语法错误的 JSON，比一个明确的 500 更难查。
func writeClientStoreJSON(w http.ResponseWriter, v clientStoreView) {
	writeLoomyJSON(w, http.StatusOK, v)
}

// writeLoomyJSON 写一个 JSON 回执（本包唯一的 JSON 出口）。
//
// 序列化失败时**不写半截响应**：先算完再写，失败就回 500 一个固定体。
// 写一半会让调用方拿到语法错误的 JSON，比一个明确的 500 更难查。
func writeLoomyJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"序列化回执失败"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ── 手机号验证码登录页 ────────────────────────────────────────────────────

// handleLoginSendSMS POST /admin/loomy/login/sms —— 表单页第 1 步。
//
// 请求体：{"state":"...","phone":"..."}；回执：{"ok":true,"msgid":"..."}
func (p *Provider) handleLoginSendSMS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	f, _ := p.LoginFlow()
	flow, ok := f.(*loginFlow)
	if !ok || flow == nil {
		writeLoomyJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "登录流程未接线"})
		return
	}
	msgid, err := flow.BeginSMS(strings.TrimSpace(req.State), req.Phone)
	if err != nil {
		// 400 而不是 500：绝大多数失败是**用户输入或验证码状态**问题
		//（手机号格式、会话过期、验证码发太频），原样带出网关的 desc。
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeLoomyJSON(w, http.StatusOK, map[string]any{"ok": true, "msgid": msgid})
}

// handleLoginVerify POST /admin/loomy/login/verify —— 表单页第 2 步。
//
// 请求体：{"state","phone","code","msgid"}；回执：{"ok":true,"uid","nickname"}
func (p *Provider) handleLoginVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
		Phone string `json:"phone"`
		Code  string `json:"code"`
		MsgID string `json:"msgid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	f, _ := p.LoginFlow()
	flow, ok := f.(*loginFlow)
	if !ok || flow == nil {
		writeLoomyJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "登录流程未接线"})
		return
	}
	a, err := flow.FinishSMS(strings.TrimSpace(req.State), req.Phone,
		strings.TrimSpace(req.Code), strings.TrimSpace(req.MsgID))
	if err != nil {
		writeLoomyJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// ⚠ 回执里**不回 session**（本仓库吃过一次真实凭证泄漏）。
	// 账号池要的只是 uid/nickname；凭证由核心那条 poll 路径落盘，
	// 前端也不需要看到它。
	writeLoomyJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"uid":      a.UID,
		"nickname": maskDisplay(a.Nickname),
	})
}

// handleLoginPage GET /admin/loomy/login —— 自助登录表单页。
//
// # 为什么自己渲染一小段 HTML，而不是让前端加个弹窗
//
// 前端的 `addAccount` 已经支持"给你一个 auth_url，你自己去打开"这种形态
// （设备码流程就是这么用的）。把它复用过来意味着**前端零改动、
// 核心零改动**，而代价只是本包多一段自包含的 HTML。
//
// 反过来说：如果要在控制台弹窗里加输入框，就得动 `internal/server/webui.html`
// 与核心的登录端点契约（"poll 只带 state"要扩成"还能提交手机号/验证码"）——
// 那是把一个上游的交互形态焊进核心。
const loginPageHTML = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Loomy 手机号登录</title>
<style>
 body{margin:0;padding:32px;font:14px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif;
      background:#0f1115;color:#e6e8ee;display:flex;justify-content:center}
 .card{width:100%;max-width:420px;background:#171a21;border:1px solid #262b36;border-radius:12px;padding:24px}
 h1{margin:0 0 4px;font-size:17px}
 p.sub{margin:0 0 20px;color:#8b93a7;font-size:12px}
 label{display:block;margin:14px 0 6px;color:#8b93a7;font-size:12px}
 input{width:100%;box-sizing:border-box;padding:10px 12px;border-radius:8px;border:1px solid #2c323f;
       background:#0f1115;color:#e6e8ee;font-size:14px}
 .row{display:flex;gap:8px}
 .row input{flex:1}
 button{padding:10px 14px;border-radius:8px;border:1px solid #2c323f;background:#222835;color:#e6e8ee;
        font-size:13px;cursor:pointer;white-space:nowrap}
 button.primary{background:#3b6cf0;border-color:#3b6cf0;color:#fff;width:100%;margin-top:20px;font-weight:600}
 button:disabled{opacity:.55;cursor:default}
 .msg{margin-top:14px;padding:10px 12px;border-radius:8px;font-size:12px;display:none;white-space:pre-wrap}
 .ok{display:block;background:#12301f;border:1px solid #1e5c37;color:#7ee2a8}
 .bad{display:block;background:#301317;border:1px solid #5c1e26;color:#ff9aa5}
 .hint{margin-top:18px;padding-top:14px;border-top:1px solid #262b36;color:#8b93a7;font-size:11.5px}
 code{background:#0f1115;padding:1px 4px;border-radius:4px}
</style></head><body>
<div class="card">
  <h1>Loomy 手机号登录</h1>
  <p class="sub">登录成功后本页可以关闭，控制台会在几秒内自动完成添加。</p>

  <label>手机号（11 位，中国大陆）</label>
  <input id="phone" inputmode="numeric" maxlength="11" placeholder="例如 13800138000" autocomplete="tel">

  <label>短信验证码</label>
  <div class="row">
    <input id="code" inputmode="numeric" maxlength="6" placeholder="6 位数字">
    <button id="send">发送验证码</button>
  </div>

  <button class="primary" id="login">登录并添加账号</button>
  <div class="msg" id="msg"></div>

  <div class="hint">
    验证码由讯飞账号网关下发（<b>会真的发短信</b>，有效期 5 分钟）。<br>
    登录用的是 Loomy 客户端自己的接口与签名，成功后会取回 <code>session</code> 并写入网关的
    <code>auths/loomy/</code>。<br>
    如果本机已经装了 Loomy 客户端并登录过，控制台点「添加账号」时**不会**打开本页
    —— 那种情况直接复用客户端登录态，不发短信。
  </div>
</div>
<script>
(function(){
  var $ = function(id){ return document.getElementById(id); };
  var state = new URLSearchParams(location.search).get('state') || '';
  var msgid = '';
  function show(text, ok){
    var m = $('msg'); m.textContent = text; m.className = 'msg ' + (ok ? 'ok' : 'bad');
  }
  function post(path, body){
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(Object.assign({ state: state }, body))
    }).then(function(r){ return r.json().then(function(j){
      if (!r.ok || !j.ok) throw new Error((j && j.error) || ('HTTP ' + r.status));
      return j;
    }); });
  }
  if (!state) show('缺少 state —— 请回到控制台重新点「添加账号」', false);

  $('send').addEventListener('click', function(){
    var b = this, phone = $('phone').value.trim();
    b.disabled = true; b.textContent = '发送中…';
    post('/admin/loomy/login/sms', { phone: phone }).then(function(j){
      msgid = j.msgid || '';
      show('验证码已发送，请查收短信（5 分钟内有效）。', true);
      var n = 60;
      var t = setInterval(function(){
        n--; b.textContent = n > 0 ? ('重发 ' + n + 's') : '重新发送';
        if (n <= 0) { clearInterval(t); b.disabled = false; b.textContent = '重新发送'; }
      }, 1000);
    }).catch(function(e){
      b.disabled = false; b.textContent = '发送验证码';
      show('发送失败：' + e.message, false);
    });
  });

  $('login').addEventListener('click', function(){
    var b = this;
    b.disabled = true; b.textContent = '登录中…';
    post('/admin/loomy/login/verify', {
      phone: $('phone').value.trim(), code: $('code').value.trim(), msgid: msgid
    }).then(function(j){
      b.textContent = '登录成功';
      show('登录成功：' + (j.nickname || j.uid) + '\n可以关闭本页了，控制台会自动完成添加。', true);
    }).catch(function(e){
      b.disabled = false; b.textContent = '登录并添加账号';
      show('登录失败：' + e.message, false);
    });
  });
})();
</script>
</body></html>`

// handleLoginPage 回表单页。
//
// 不做 state 校验就拒（比如"会话不存在"直接 404）：用户可能拿着一个已经
// 过期的链接打开，那时让他看到表单、点下去才报错，比一个纯 404 更容易理解
// （页面底部有说明）。真正的校验在两条 POST 上。
func (p *Provider) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(loginPageHTML))
}
