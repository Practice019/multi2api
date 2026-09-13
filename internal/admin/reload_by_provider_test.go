package admin

// reload_by_provider_test.go —— 「重载 auths」必须按**请求体里的上游**重载。
//
// # 用户实测报出来的症状
//
// 在账号池页面点 **codearts** 那行的「重载 auths」，三条回执都是：
//
//	workbuddy: HTTP 200 {"provider":"workbuddy","scanned":3}
//	codearts : HTTP 200 {"provider":"workbuddy","scanned":3}   ← 点 codearts，回执说 workbuddy
//	ghost    : HTTP 200 {"provider":"workbuddy","scanned":3}   ← 不存在的上游被默默接受
//
// 后端**完全不读请求体**，无论点哪一行都走 `h.reloadProvider()`
//（= 配置里硬编码的 workbuddy）。
//
// # 为什么这条比看起来严重
//
// 这条按钮的**存在意义**就是"往 `auths/<provider>/` 手工拷凭证后不用重启网关"。
// 对 codearts 而言它**根本不工作**，而界面说成功了 ——
// 用户以为拷进去的凭证已经生效，实际上网关还在用旧池子。
//
// # ⚠ 本文件最关键的设计约束：fixture 的**两个目录文件数必须不同**
//
// 我第一版端到端脚本在**真实实例**上跑，发现两个目录的文件数恰好相同：
//
//	目录文件数: {"workbuddy":3,"codearts":3}
//
// 那意味着 `scanned` 这条断言**恒真** —— 一个"恒用 reloadProvider()"
// 的错误实现会**蒙对**变异验证（它扫到 3，期望也是 3）。
// 那样的守卫是装饰品。
//
// 所以下面的 fixture 刻意造成 **2 个 vs 3 个**：
// 请求 codearts 时若错误地回落成 workbuddy，`scanned` 会是 2 而不是 3，必红。
//
// # fixture 的文件数（明确写出来，便于复核本文件的前提是否还成立）
//
//	workbuddy 目录: 2 个凭证文件（wb-a.json、wb-b.json）
//	codearts  目录: 3 个凭证文件（ca-1.json、ca-2.json、ca-3.json）
//	base 根目录  : 0 个凭证文件
//
// ⚠ 3 vs 2 之外**还有一层**保险：两个上游的 uid 集合完全不相交
//（wb-* vs ca-*），所以即使将来有人把某处改成 "扫全部再按 provider 过滤"，
// 池子断言仍能抓住"动错了上游的号"。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// authDirProvider 一个自报凭证目录的最小 Provider（满足 gateway.AuthDirExt）。
//
// 它**不**实现 LoginFlow —— 这是刻意的：手工拷凭证再重载这条路径
// 根本不需要登录流程，而 AuthDirExt 拆出来的理由正是"不实现登录流程的
// 上游也要能回答'我的凭证在哪'"（见 gateway.AuthDirExt 的注释）。
// 若这里挂上 LoginFlow，就验不出"重载与登录流程无关"这个设计意图。
type authDirProvider struct {
	stubProvider
	dir string
}

func (p *authDirProvider) AuthDir() string { return p.dir }

// 编译期断言：桩必须被核心当成"凭证目录自报者"认出来。
// 少了它，ExtOf 断言失配会**静默**退化成空串回落，测试会红得莫名其妙。
var _ gateway.AuthDirExt = (*authDirProvider)(nil)

// seedWorkbuddyFiles 在 dir 下落 n 个能被 `auth.LoadDir` 扫到的凭证文件。
//
// ⚠ 文件名前缀必须保持 `workbuddy`：`auth.LoadDir` 只 glob
// `workbuddy*.json`（见 auth.go）。这里的"上游"是桩，但**扫描器**是真的，
// 所以文件必须写成真实扫描器认得的形状 ——
// 否则两个目录都会扫出 0 个，断言又变回"恒真"。
func seedWorkbuddyFiles(t *testing.T, dir string, uids ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, uid := range uids {
		a := &auth.Auth{
			UID:          uid,
			AccessToken:  "at-" + uid,
			RefreshToken: "rt-" + uid,
			FilePath:     filepath.Join(dir, "workbuddy-"+uid+".json"),
		}
		if err := a.SaveAtomic(); err != nil {
			t.Fatalf("落 fixture 凭证 %s 失败: %v", uid, err)
		}
	}
}

// countCredFiles 数一遍目录里真实存在的凭证文件数 —— 用例自己复核前提用。
func countCredFiles(t *testing.T, dir string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// reloadFixture 一次把"两个上游 + 两个目录"的完整场景搭好。
//
// 两个目录的文件数**必须不同**（2 vs 3）—— 见文件头那段说明。
type reloadFixture struct {
	h          *Handler
	p          *pool.Pool
	base       string
	wbDir      string
	caDir      string
	wbFileN    int
	caFileN    int
	reloadProv string // 配置里的 ReloadProvider（错误实现会把所有请求都归到它）
}

func newReloadFixture(t *testing.T) *reloadFixture {
	t.Helper()

	base := t.TempDir()
	wbDir := filepath.Join(base, "workbuddy")
	caDir := filepath.Join(base, "codearts")

	// ⚠ 数量刻意不同：2 vs 3。两者相同会让 scanned 断言恒真、变异蒙对。
	seedWorkbuddyFiles(t, wbDir, "wb-a", "wb-b")
	seedWorkbuddyFiles(t, caDir, "ca-1", "ca-2", "ca-3")
	// base 根**故意不放**任何凭证：保证可见的差异只来自两个子目录。
	// （若根里也有文件，LoadDirCompat 会把它们并进来，两个上游的
	// scanned 都会被抬高同样的数量，从而**掩盖**真正的目录分派错误。）

	reg := gateway.NewRegistry()
	if err := reg.Register(&authDirProvider{
		stubProvider: stubProvider{id: "workbuddy", caps: gateway.CapChat},
		dir:          wbDir,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&authDirProvider{
		stubProvider: stubProvider{id: "codearts", caps: gateway.CapChat},
		dir:          caDir,
	}); err != nil {
		t.Fatal(err)
	}

	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("workbuddy")
	// 两个上游各预置一个**不相交**的号，用来验"没动别的上游的池子"。
	p.SyncToDirFor("workbuddy", []*auth.Auth{{UID: "wb-existing"}})
	p.SyncToDirFor("codearts", []*auth.Auth{{UID: "ca-existing"}})
	p.SyncToDirFor("other", []*auth.Auth{{UID: "zz-existing"}})

	h := New(Config{
		Pool:            p,
		Registry:        reg,
		AuthDir:         wbDir, // 默认上游的目录
		AuthsBase:       base,
		DefaultProvider: "workbuddy",
		ReloadProvider:  "workbuddy", // ← 配置里的固定值（缺省回落用）
	})

	return &reloadFixture{
		h: h, p: p, base: base, wbDir: wbDir, caDir: caDir,
		wbFileN: countCredFiles(t, wbDir), caFileN: countCredFiles(t, caDir),
		reloadProv: "workbuddy",
	}
}

// reloadResp 回执的形状。
type reloadResp struct {
	Scanned  int    `json:"scanned"`
	Before   int    `json:"before"`
	After    int    `json:"after"`
	Provider string `json:"provider"`
	Dir      string `json:"dir"`
}

// postReload 发一次重载请求，body 为 ""（空体）。
func postReload(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, reloadResp) {
	t.Helper()
	req := localReq("POST", "/admin/accounts/reload")
	req.Body = jsonBody(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out reloadResp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// TestReloadFixtureHasDistinctCounts 先钉住 fixture 自己的前提。
//
// 这条**不是**在测业务代码 —— 它测的是"上面的断言有没有可能恒真"。
// 用户实测踩过一次：真实实例里两个目录文件数恰好相同（3 和 3），
// 于是一个"恒用 reloadProvider()"的错误实现会在 scanned 上**蒙对**。
//
// 前提一旦被谁改坏（比如有人顺手把 fixture 改成两边各 3 个），
// 这里立刻红，而不是让下面几条静默失效。
func TestReloadFixtureHasDistinctCounts(t *testing.T) {
	f := newReloadFixture(t)

	if f.wbFileN == f.caFileN {
		t.Fatalf("fixture 前提不满足：两个上游目录的文件数相同（都 %d）—— "+
			"那样 scanned 断言**恒真**，变异验证会蒙对。"+
			"必须让它们不同（当前设计：workbuddy=%d、codearts=%d）",
			f.wbFileN, f.wbFileN, f.caFileN)
	}
	if f.wbFileN == 0 || f.caFileN == 0 {
		t.Fatalf("fixture 前提不满足：某目录扫不到凭证（wb=%d ca=%d）—— "+
			"文件名前缀可能不再被 auth.LoadDir 匹配", f.wbFileN, f.caFileN)
	}
	t.Logf("fixture 文件数：workbuddy=%d、codearts=%d、base 根=0", f.wbFileN, f.caFileN)
}

// TestReloadUsesRequestedProvider 核心用例：点 codearts 就重载 codearts。
//
// 这是用户实测报的那条 bug 的**直接回归**：
// 改之前，请求体里的 provider 被完全忽略，回执恒为 workbuddy。
func TestReloadUsesRequestedProvider(t *testing.T) {
	f := newReloadFixture(t)

	rec, resp := postReload(t, f.h, `{"provider":"codearts"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}

	// ① 回执里的 provider 必须是**请求的那个**（前端拿它做文案，
	//    不能用前端自己猜的值 —— 与 loginPoll 同一个原则）。
	if resp.Provider != "codearts" {
		t.Errorf("provider=%q want codearts —— 点 codearts 却回 workbuddy，"+
			"正是用户实测报的那条（界面在骗人）", resp.Provider)
	}

	// ② ⚠ 这条是变异验证要打的目标：scanned 必须对上 **codearts 的**文件数。
	//    错误实现（恒用 reloadProvider()）会扫 workbuddy 目录 → 得到 2。
	if resp.Scanned != f.caFileN {
		t.Errorf("scanned=%d want %d（codearts 目录的实际文件数）—— "+
			"得到 workbuddy 的 %d 说明扫的是默认上游的目录；"+
			"两个目录文件数刻意不同就是为了让这条能抓住它",
			resp.Scanned, f.caFileN, f.wbFileN)
	}
	if resp.Scanned == f.wbFileN {
		t.Errorf("scanned=%d 恰好等于 workbuddy 目录的文件数 —— "+
			"请求的是 codearts，这不可能是对的", resp.Scanned)
	}

	// ③ 回执的 dir 应是 codearts 自报的目录
	if resp.Dir != f.caDir {
		t.Errorf("dir=%q want %q（上游自报的目录）", resp.Dir, f.caDir)
	}

	// ④ 池子：codearts 被对齐到扫描结果（3 个），workbuddy **一个都不能少**
	if got := len(f.p.ListFor("codearts")); got != f.caFileN {
		t.Errorf("codearts 池内账号=%d want %d（扫描结果）", got, f.caFileN)
	}
	if got := len(f.p.ListFor("workbuddy")); got != 1 {
		t.Errorf("workbuddy 池内账号=%d want 1 —— "+
			"重载别的上游**绝不能**动它的号（SyncToDirFor 按上游剔除，"+
			"这里验证这条保证没被破坏）", got)
	}
	if got := len(f.p.ListFor("other")); got != 1 {
		t.Errorf("other 池内账号=%d want 1", got)
	}
}

// TestReloadBothProvidersDistinct 两个上游各点一次，结果必须**彼此不同**。
//
// 为什么单独立一条：单看"codearts 得到 3"仍可能被一个
// "恒扫两个目录之和"的实现蒙对（3+2=5≠3，其实不会，但更关键的是
// **对称性**）。同时断言两次调用的 (provider, scanned, dir) 三元组
// 互不相同，才能说明"分派真的按请求走"。
func TestReloadBothProvidersDistinct(t *testing.T) {
	f := newReloadFixture(t)

	_, wb := postReload(t, f.h, `{"provider":"workbuddy"}`)
	_, ca := postReload(t, f.h, `{"provider":"codearts"}`)

	if wb.Provider != "workbuddy" || ca.Provider != "codearts" {
		t.Errorf("回执 provider 不匹配：wb=%q ca=%q", wb.Provider, ca.Provider)
	}
	if wb.Scanned != f.wbFileN {
		t.Errorf("workbuddy scanned=%d want %d", wb.Scanned, f.wbFileN)
	}
	if ca.Scanned != f.caFileN {
		t.Errorf("codearts scanned=%d want %d", ca.Scanned, f.caFileN)
	}
	// 最关键的一条：两次结果**必须不同**。相同 = 分派没生效。
	if wb.Scanned == ca.Scanned {
		t.Errorf("两个上游的 scanned 相同（都 %d）—— "+
			"说明请求里的 provider 没被用上，两次扫的是同一个目录", wb.Scanned)
	}
	if wb.Dir == ca.Dir {
		t.Errorf("两个上游的 dir 相同（都 %q）—— 同上", wb.Dir)
	}

	// 池子互不干扰
	if got := len(f.p.ListFor("workbuddy")); got != f.wbFileN {
		t.Errorf("workbuddy 池=%d want %d", got, f.wbFileN)
	}
	if got := len(f.p.ListFor("codearts")); got != f.caFileN {
		t.Errorf("codearts 池=%d want %d", got, f.caFileN)
	}
}

// TestReloadUnknownProviderNotFound 不存在的上游必须 **404**，绝不默默回落。
//
// 用户实测：`ghost` 收到了 `200 {"provider":"workbuddy","scanned":3}`。
// 那比"报错"糟得多 —— 调用方以为重载了 ghost，实际动的是 workbuddy 的池子。
//
// 404 而不是 400：这是"这个上游不存在"（资源问题），
// 不是"provider 字段格式不对"（参数问题）。
func TestReloadUnknownProviderNotFound(t *testing.T) {
	f := newReloadFixture(t)

	before := len(f.p.ListFor("workbuddy"))

	rec, resp := postReload(t, f.h, `{"provider":"ghost"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的上游应 404，实际 %d body=%s —— "+
			"回落到默认目录正是要修的 bug 形态（界面显示成功、实际重载了别人）",
			rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ghost") {
		t.Errorf("404 的错误信息应带上游名（便于排查），实际 %s", rec.Body)
	}
	if resp.Provider == "workbuddy" {
		t.Errorf("回执里出现了 workbuddy —— 说明它**回落**了，而不是失败")
	}

	// 副作用必须为零：既没扫、也没动池子。
	//
	// ⚠ 这里断言的是 fixture **预置**的 1 个，不是目录里的 3 个 ——
	// 404 路径根本没扫目录，池子该原封不动。
	// （我第一版误写成 `f.caFileN`，红了；错的是断言不是代码。）
	if got := len(f.p.ListFor("workbuddy")); got != before {
		t.Errorf("请求不存在上游后 workbuddy 池=%d want %d —— "+
			"失败路径**不能有副作用**", got, before)
	}
	if got := len(f.p.ListFor("codearts")); got != 1 {
		t.Errorf("请求不存在上游后 codearts 池=%d want 1（预置值，未被触碰）—— "+
			"失败路径**不能有副作用**", got)
	}
}

// TestReloadBackwardCompatibleNoProvider 不带 provider（空体 / `{}`）时
// 行为与改造前**逐字一致** —— 走 reloadProvider() 的配置回落。
//
// 为什么必须钉住：老客户端和缓存里的旧页面发的就是 `{}` 或空体。
// 若"没有 provider"被当成"未知上游"而 404，重载功能对它们整个消失。
func TestReloadBackwardCompatibleNoProvider(t *testing.T) {
	for _, body := range []string{"", "{}", `{"provider":""}`} {
		body := body
		t.Run("body="+body, func(t *testing.T) {
			f := newReloadFixture(t)

			rec, resp := postReload(t, f.h, body)
			if rec.Code != http.StatusOK {
				t.Fatalf("body=%q 应 200（向后兼容），实际 %d body=%s —— "+
					"老客户端发空体，把它当错误会让重载功能整个消失",
					body, rec.Code, rec.Body)
			}
			if resp.Provider != f.reloadProv {
				t.Errorf("body=%q provider=%q want %q（配置回落）",
					body, resp.Provider, f.reloadProv)
			}
			// 回落目标 = 配置值，所以扫的是 workbuddy 的目录
			if resp.Scanned != f.wbFileN {
				t.Errorf("body=%q scanned=%d want %d（ReloadProvider 的目录）",
					body, resp.Scanned, f.wbFileN)
			}
		})
	}
}

// TestReloadMalformedBodyNotFatal 坏体不能把端点打挂。
//
// 宽容解析是刻意的：解析失败只是"没拿到 provider"，应回落配置，
// 而不是 400 —— 那会让一个纯粹的客户端编码错误看起来像服务端故障。
func TestReloadMalformedBodyNotFatal(t *testing.T) {
	f := newReloadFixture(t)

	rec, resp := postReload(t, f.h, `{"provider":`)

	if rec.Code != http.StatusOK {
		t.Fatalf("坏体应宽容处理成「不带 provider」（200 + 配置回落），"+
			"实际 %d body=%s", rec.Code, rec.Body)
	}
	if resp.Provider != f.reloadProv {
		t.Errorf("坏体后 provider=%q want %q", resp.Provider, f.reloadProv)
	}
}

// TestReloadNilPoolDoesNotPanic 池子为 nil 时不能 panic。
//
// 与 pollViaFlow 那个坑同源：本包多处都判了 Pool == nil，
// 说明"Pool 可缺省"是本包自己的设计假设，漏判的后果是
// **nil pointer panic（进程级）**。
func TestReloadNilPoolDoesNotPanic(t *testing.T) {
	base := t.TempDir()
	caDir := filepath.Join(base, "codearts")
	seedWorkbuddyFiles(t, caDir, "ca-1", "ca-2", "ca-3")

	reg := gateway.NewRegistry()
	if err := reg.Register(&authDirProvider{
		stubProvider: stubProvider{id: "codearts", caps: gateway.CapChat},
		dir:          caDir,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg, AuthDir: base, AuthsBase: base, DefaultProvider: "codearts"})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Pool 为 nil 时 panic 了：%v", r)
		}
	}()

	rec, resp := postReload(t, h, `{"provider":"codearts"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	// 扫描本身不该受池子缺失影响
	if resp.Scanned != 3 {
		t.Errorf("scanned=%d want 3（扫描与池子无关）", resp.Scanned)
	}
	if resp.Before != 0 || resp.After != 0 {
		t.Errorf("无池子时 before/after 应为 0，实际 %d/%d", resp.Before, resp.After)
	}
}

// TestReloadFallsBackToUpstreamDirWhenNoAuthDirExt 上游不自报目录时，
// 回落 `auth.UpstreamDir(base, provider)`（= `auths/<provider>/`）。
//
// 这条守的是"只实现 Provider、没实现 AuthDirExt"的上游 ——
// 它们仍应能被按上游重载（目录由命名约定给出），而不是完全用不了。
func TestReloadFallsBackToUpstreamDirWhenNoAuthDirExt(t *testing.T) {
	base := t.TempDir()
	caDir := filepath.Join(base, "codearts")
	seedWorkbuddyFiles(t, caDir, "ca-1", "ca-2", "ca-3")

	// ⚠ 用纯 stubProvider：它**没有** AuthDir()，所以 ExtOf 会失配。
	reg := gateway.NewRegistry()
	if err := reg.Register(&stubProvider{id: "codearts", caps: gateway.CapChat}); err != nil {
		t.Fatal(err)
	}
	if _, ok := gateway.ExtOf[gateway.AuthDirExt]((*stubProvider)(nil)); ok {
		t.Fatal("前提不满足：stubProvider 不该实现 AuthDirExt")
	}

	h := New(Config{Registry: reg, AuthDir: base, AuthsBase: base, DefaultProvider: "codearts"})
	rec, resp := postReload(t, h, `{"provider":"codearts"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if resp.Dir != caDir {
		t.Errorf("dir=%q want %q（回落 auth.UpstreamDir(base, provider)）", resp.Dir, caDir)
	}
	if resp.Scanned != 3 {
		t.Errorf("scanned=%d want 3", resp.Scanned)
	}
}
