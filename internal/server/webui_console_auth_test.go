// webui_console_auth_test.go T7 的守卫：控制台脚本本身不再持有/持久化任何凭据，
// 且 `GET /status` 已从「api_key 可读」改为「控制台身份 + 管理员」。
//
// # 为什么这些判据要落在**源码文本**上（而不只是行为）
//
// T7 改的是"凭据从哪来"。行为测试能覆盖"cookie 有效时控制台能用"，但覆盖不了
// "将来有人又往 localStorage 里塞一把令牌" —— 那种回退只会表现为"某天用户
// 报告轮换后不生效"，而那时很难定位。源码断言便宜、直接，且失败信息能指名道姓。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"workbuddy2api/internal/account"
	"workbuddy2api/internal/pool"
)

// ★ 控制台脚本里不得再出现被删除的旧模型符号。
//
// 每一条都对应一个**真实故障**（不是洁癖）：
//
//	ADMIN_INJECTED / INJECTED  服务端不再替换那两个占位符 ⇒ 哨兵恒等于占位符
//	                           ⇒ 旧判据恒假 ⇒ 已登录的人被告知"未获授权"
//	ADMIN_TOKEN                浏览器不再持有令牌；留着会诱使后人重新注入它
//	wb2api.key                 localStorage 里的旧 api_key ⇒ 轮换后不生效（报障 ②）
//	本机免令牌                 回环地址不再是凭据（报障 ③ 的根因）
func TestConsoleJSHasNoCredentialModel(t *testing.T) {
	src := string(webuiHTML)

	// 只看**可执行部分**：先去掉 HTML 注释块，再逐行去掉 // 行注释。
	//
	// ⚠ 两个坑（都实测踩过）：
	//
	//	① HTML 注释是多行的，`<!--` 与 `-->` 往往不在同一行 ⇒ 必须按**块**去掉。
	//	   否则注释里的历史说明会被当成活代码（第一次跑就是这个假阳性）。
	//	② `ADMIN_TOKEN` 是 `__WB2API_ADMIN_TOKEN__` 的子串 ⇒ 必须用词边界匹配，
	//	   否则页面上那两行占位符自赋值（它们**要保留**，见 webui_cols_test.go）
	//	   会被误判。
	htmlComments := regexp.MustCompile(`(?s)<!--.*?-->`)
	live := htmlComments.ReplaceAllString(src, "")
	var code []string
	for _, line := range strings.Split(live, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" ||
			strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "/*") ||
			strings.HasPrefix(trimmed, "*") {
			continue
		}
		// 行内注释也去掉。⚠ 这会把字符串里的 `//`（例如 https://…）之后的内容
		// 一并截断 —— 方向是**更宽松**（少判红），不会制造假阳性。
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}
	exec := strings.Join(code, "\n")

	dropped := []struct{ re, why string }{
		{`\bADMIN_INJECTED\b`, "服务端不再替换哨兵 ⇒ 旧判据恒假 ⇒ 已登录的人被告知「未获授权」"},
		{`\bINJECTED\b`, "同上：服务端不再注入凭据，哨兵永远等于占位符"},
		{`\bADMIN_TOKEN\b`, "浏览器不再持有令牌；留着会诱使后人重新把它注入页面"},
		{`wb2api\.key`, "localStorage 里的旧 api_key ⇒ 轮换后不生效（用户报障 ②）"},
		{`本机免令牌`, "回环地址不再是凭据（用户报障 ③ 的根因）"},
	}
	for _, d := range dropped {
		if m := regexp.MustCompile(d.re).FindStringIndex(exec); m != nil {
			line := lineOf(exec, m[0])
			t.Errorf("★ 控制台脚本的可执行部分仍引用 %s ——\n"+
				"这是 T7 删掉的旧凭据模型：%s。\n"+
				"命中处：%s\n"+
				"（若只是注释里解释历史，请确保整段都在 HTML 注释或 // 注释里。）",
				d.re, d.why, strings.TrimSpace(line))
		}
	}
}

// lineOf 返回 pos 所在的那一行（给失败信息定位用）。
func lineOf(s string, pos int) string {
	start := strings.LastIndex(s[:pos], "\n") + 1
	end := strings.Index(s[pos:], "\n")
	if end < 0 {
		return s[start:]
	}
	return s[start : pos+end]
}

// ★ 控制台只允许把**非凭据**的 UI 偏好写进 localStorage。
//
// 白名单：主题、分页、导航折叠、列宽、面板缓存、模型/账号分组。
// 任何"看起来像密钥"的键都要拒绝 —— 旧实现在这里写的是 `wb2api.key`。
func TestConsoleJSWritesNoCredentialsToStorage(t *testing.T) {
	src := string(webuiHTML)

	allowed := []string{
		"wb2api.theme",
		"wb2api.pagesize",
		"wb2api.navcollapsed",
		"wb2api.panelkey", // 上次停留的面板（切页时恢复用），纯 UI 状态
		"wb2api.acctcols.",
		"wb2api.pcache.",
		"wb2api.modelgroup",
		"wb2api.acctgroup",
		"wb2api.modelprovider",
	}
	setRe := regexp.MustCompile(`localStorage\.setItem\(\s*(?:'([^']*)'|"([^"]*)")`)
	seen := 0
	for _, m := range setRe.FindAllStringSubmatch(src, -1) {
		key := m[1]
		if key == "" {
			key = m[2]
		}
		seen++
		ok := false
		for _, a := range allowed {
			if key == a || strings.HasPrefix(key, a) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("★ localStorage.setItem(%q) 不在白名单里 ——\n"+
				"控制台不应该把任何**凭据**持久化到磁盘（用户报障 ② 的根因）。"+
				"真的是 UI 偏好就加进本测试的 allowed 列表，并在注释里说明为什么。", key)
		}
	}
	if seen == 0 {
		t.Fatal("一个 localStorage.setItem 都没解析到 —— 正则或页面结构变了，本测试已失去守卫作用")
	}

	// 「记住」那个复选框与写凭据的开关必须已经删掉（它是旧模型的入口）
	if strings.Contains(src, `id="remember"`) {
		t.Error("顶栏那个「记住」复选框仍在 —— 它的唯一作用就是把凭据写进 localStorage")
	}
}

// ★ GET /status 的鉴权口径：控制台身份（会话或 admin_token）+ 管理员。
//
// 它回的是账号池画像（昵称/积分/上游熔断态）。改造前挂在 withAuth 上 ⇒
// **任何一把发给客户端的 api_key 都能读到它**。这条测试同时钉住两个方向：
//
//	api_key            ⇒ 401（不再是可读凭据）
//	会话（管理员）      ⇒ 200
//	非管理员历史记录    ⇒ 401（成员账号已取消，它不构成身份）
//	admin_token        ⇒ 200
//
// ⚠ 语义变更（2026-10-19 单账号口径）：原先那条是「会话（成员）⇒ 403 +
// code=forbidden_role」。成员身份取消后，非管理员在**身份层**就被拒了
// （account.Lookup 不认它）⇒ 连不到 403 那一层。所以断言从 403 改成 401：
// 这不是放宽，而是"它现在更早、更彻底地被挡住了"。
func TestStatusRequiresConsoleAdmin(t *testing.T) {
	f := newUIFixture(t, true)

	// 夹具带一个空池子 ⇒ 过门禁之后是 200（而不是"不是 403 就算过"）。
	get := func(t *testing.T, mutate func(*http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		if mutate != nil {
			mutate(req)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("无凭据 → 401 auth_required", func(t *testing.T) {
		rec := get(t, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("状态码 = %d，want 401", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "auth_required") {
			t.Errorf("回执里的 code 应为 auth_required，实际 %s", rec.Body.String())
		}
	})

	t.Run("★ api_key → 401（账号池画像不再对 /v1 客户端开放）", func(t *testing.T) {
		rec := get(t, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testAPIKey)
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("★ 拿 api_key 读 /status 得到 %d，want 401 ——\n"+
				"api_key 是给客户端/机器人的对话凭据，不该能读到账号池画像。", rec.Code)
		}
	})

	t.Run("管理员会话 → 过门禁", func(t *testing.T) {
		rec := get(t, func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: account.CookieName, Value: f.sessionCookieFrom(t, f.user)})
		})
		if rec.Code != http.StatusOK {
			t.Errorf("管理员会话读 /status = %d，want 200：%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("admin_token → 过门禁", func(t *testing.T) {
		rec := get(t, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testAdminToken)
		})
		if rec.Code != http.StatusOK {
			t.Errorf("admin_token 读 /status = %d，want 200：%s", rec.Code, rec.Body.String())
		}
	})

	// ★ 非管理员的历史记录：另起一个夹具（账号库里有一条改造前留下的成员记录）。
	//
	// 为什么要另起夹具：`newUIFixture` 用 CreateBootstrap 建**唯一**那个账号，
	// 而 CreateBootstrap 只认空库（那正是"单账号"的实现）⇒ 想造出"盘上有一条
	// 成员记录"的形态，只能**手写文件**（也就是目标态 3 说的那种旧数据）。
	t.Run("★ 成员历史记录的会话 → 401（成员账号已取消，不构成身份）", func(t *testing.T) {
		lf, member := newUILegacyMemberFixture(t)
		if member == nil {
			t.Fatal("前置：成员历史记录应当被加载（New 不得把它丢掉）")
		}
		sess, err := lf.sessions.Issue(member, "127.0.0.1", "probe/1.0")
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		req.AddCookie(&http.Cookie{Name: account.CookieName, Value: sess.SID})
		rec := httptest.NewRecorder()
		lf.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("★ 成员历史记录读 /status = %d，want 401 ——\n"+
				"成员账号已取消：它在 account.Lookup 那层就换不到身份，\n"+
				"根本走不到「角色不足」那一步。", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "forbidden_role") {
			t.Errorf("不该再出现 forbidden_role（成员口径已取消），实际 %s", rec.Body.String())
		}
	})
}

// legacyMemberFileRows 一个账号文件的内容：唯一的管理员 + 一条成员历史记录。
//
// 口令用真 bcrypt 哈希（account.HashPassword）—— 这样"口令对但角色不是管理员"
// 这条反例在 HTTP 层也真的走到角色判定，而不是被口令校验挡下。
func writeLegacyMemberFile(t *testing.T, path string) {
	t.Helper()
	row := func(id, name, plain string, role int) map[string]any {
		h, err := account.HashPassword(plain)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		return map[string]any{
			"id":            id,
			"username":      name,
			"display_name":  name,
			"password_hash": h,
			"role":          role,
			"enabled":       true,
			"auth_version":  1,
			"created_at":    "2026-01-01T00:00:00Z",
		}
	}
	raw, err := json.MarshalIndent([]map[string]any{
		row("u-boss", "boss", "boss-password-001", account.RoleAdmin),
		row("u-staff", "staff", "staff-password-002", account.RoleMember),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写账号库: %v", err)
	}
}

// newUILegacyMemberFixture 起一个 /ui 夹具，账号库由**手写文件**构成
// （一个管理员 + 一条成员历史记录），返回夹具与那条成员记录。
func newUILegacyMemberFixture(t *testing.T) (*uiFixture, *account.User) {
	t.Helper()
	dir := t.TempDir()
	accPath := filepath.Join(dir, "accounts.json")
	writeLegacyMemberFile(t, accPath)
	accounts, err := account.New(accPath)
	if err != nil {
		t.Fatalf("★ 盘上有成员历史记录时 New 不得报错（不许崩）：%v", err)
	}
	sessions, err := account.NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	f := &uiFixture{
		h: NewHandler(Config{
			Pool:         pool.New(filepath.Join(dir, "pool-state.json")),
			APIKey:       testAPIKey,
			AdminToken:   testAdminToken,
			CookieSecure: account.CookieSecureNever,
			Accounts:     accounts,
			Sessions:     sessions,
		}),
		accounts: accounts,
		sessions: sessions,
		user:     accounts.ByUsername("boss"),
	}
	return f, accounts.ByUsername("staff")
}

// ⚠⚠ 已删除：TestConsoleMemberModeHidesAdminOnlyPanels。
//
// # 它原来测什么
//
// 前端"成员模式"（T7 的最小降级）的三件事：
//
//	① 成员能看的三块（dashboard/apikeys/chat）不得在 `.member-mode` 隐藏名单里
//	② 五个管理员专属面板必须在隐藏名单里（accounts/models/jobs/logs/settings）
//	③ 403 的处置是"进入成员模式"（applyMemberMode()），**不是** hideConsole()
//
// 判据的载体是 `webui.html` 里的 CSS 规则与 refresh() 里的 `memberMode = true`。
//
// # 为什么它必须消失（不是"改成红的"）
//
// 2026-10-19 用户新口径：「只要一个管理员就行了，只要一个账号。用户其实可以不用。」
// ⇒ **成员这个身份不存在了**，服务端那边 memberRules 白名单与 403 的
// forbidden_role 路径已删（见 internal/admin/admin.go 的 canAccess），
// `/status` 对非管理员也不再回 403（它在身份层就是 401）。
//
// 因此 ③ 那句话已经**没有对应的服务端行为**：不会再有"403 触发的成员模式"。
// ①② 测的那份隐藏名单，是"前端要迁就服务端的分角色授权"的产物 ——
// 授权不分角色之后它没有存在理由；窗口 B 负责把 `.member-mode` 与
// `applyMemberMode()` 从 `webui.html` / `webui-login.html` 里删掉。
//
// # 它留下的空缺由谁接管
//
// 服务端这一侧（本窗口能管的部分）：
//
//	· 非管理员不构成身份 ⇒ `TestStatusRequiresConsoleAdmin` 的成员子用例（401）
//	· 门禁不再有"角色不足"这一档 ⇒ `internal/admin` 的
//	  TestGate_SingleAdminSession / TestGate_NonAdminSessionDeniedEverywhere
//	· 回执里不再有分角色字段 ⇒ TestAPIKeysPayloadHasNoMemberFlag
//
// 前端那一侧的判据（"页面里不许再出现成员模式的残留"）归属窗口 B 的文件，
// 由那边的测试负责 —— 本窗口不写、也不假装验证 `webui.html`。

// sessionCookieFrom 给任意账号签一条会话并返回 SID。
//
// （夹具里另有一个只签管理员会话的便捷方法 `sessionCookie`；成员相关的断言
// 需要能指定账号，所以两个都在。）
func (f *uiFixture) sessionCookieFrom(t *testing.T, u *account.User) string {
	t.Helper()
	sess, err := f.sessions.Issue(u, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("签发会话: %v", err)
	}
	return sess.SID
}

// 装配层忘了接线时 /status 不得静默放行（fail-closed）。
func TestStatusWithoutAccountsConfiguredDeniesAll(t *testing.T) {
	f := newUIFixture(t, false) // Accounts/Sessions 均为 nil
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("没接账号体系时 /status 应当 401（fail-closed），实际 %d", rec.Code)
	}
	// 目录里确实存在被嵌入的登录页（避免"文件没打包进去"这种部署期故障）
	if len(loginHTML) == 0 {
		t.Error("webui-login.html 没被 embed 进二进制")
	}
}
