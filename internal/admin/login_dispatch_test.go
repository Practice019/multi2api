package admin

// login_dispatch_test.go —— /admin/login/start|poll 的**按上游分派**守卫。
//
// # 为什么这些断言必须存在
//
// 分派写错有两种后果，都很难在界面上看出来：
//
//   1. **该 501 的时候回落了** —— 用户在 codearts 那行点「添加账号」，
//      请求静默走了默认上游的 OAuth，**账号加进了 workbuddy**。
//      界面会显示"成功"，而东西加错了地方。
//   2. **向后兼容被破坏** —— 老客户端不带 `provider`，
//      若分派逻辑把它当"未知上游"，添加账号整个功能挂掉。
//
// 所以下面同时钉住：带 provider 的 501 路径、不带 provider 的旧路径。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// fakeFlow 一个可注入的 LoginFlow，用来在测试里控制轮询结果。
type fakeFlow struct {
	startState string
	startURL   string
	startErr   error

	// pollFn 决定 Poll 的行为（返回凭证 / ErrLoginPending / 别的错误）。
	pollFn func(state string) (gateway.Credential, error)

	// configured 由用例显式设置 —— 零值是 false，所以每个需要的用例
	// 都要写明，避免"忘了设"导致用例静默走另一条分支。
	configured bool

	// authDir 凭证落盘目录（供 AuthDir() 返回）。
	authDir string
}

func (f *fakeFlow) Start() (string, string, error) {
	return f.startState, f.startURL, f.startErr
}

func (f *fakeFlow) Poll(state string) (gateway.Credential, error) {
	return f.pollFn(state)
}

// Configured 报告"这份配置真的能登录吗"。
//
// ⚠ 加了 `LoginFlow.Configured` 之后，**所有**测试桩都必须实现它 ——
// 否则它们会被判定为"未配置"，一批既有用例会全红（实测如此）。
//
// 这是设计使然：光"实现了 Start/Poll"不足以说明可用
// （上游为了让类型断言认出来必须挂那两个方法，与"配没配 OAuth"无关）。
//
// 用字段而不是恒 true：这样才能写"实现了但没配置 → 不给按钮"的反例。
func (f *fakeFlow) Configured() bool { return f.configured }

// AuthDir 凭证落盘目录（`gateway.AuthDirExt` 要求）。
//
// 用**字段**而不是恒空串：这样测试才能验"落盘用的是**上游自报的**目录"。
// 实测踩过：我第一版用核心的默认 AuthDir，于是 codearts 授权成功后
// 凭证被写进了 workbuddy 的目录。
func (f *fakeFlow) AuthDir() string { return f.authDir }

// 编译期断言：桩必须满足接口（漏了 Configured 会在这里红，
// 而不是在一堆用例里红 —— 定位快得多）。
var _ gateway.LoginFlow = (*fakeFlow)(nil)

// ⚠ fakeFlow 同时实现 AuthDir()，但它**不**在这里被断言成 AuthDirExt ——
// 因为核心拿 Provider 去问目录（见 pollViaFlow），不会问一个 LoginFlow。
// 这个方法保留的理由不是"满足接口"，而是 flowProvider 要把它**转发出去**
//（见下）。真实上游（workbuddy / codearts）的 AuthDirExt 断言在各自包里。

// flowProvider 实现了 LoginFlow 的 stub —— 供分派测试用。
type flowProvider struct {
	stubProvider
	flow *fakeFlow
}

func (p *flowProvider) Start() (string, string, error)            { return p.flow.Start() }
func (p *flowProvider) Poll(s string) (gateway.Credential, error) { return p.flow.Poll(s) }

// Configured 必须**转发**给内部的 fakeFlow。
//
// ⚠ 漏了它的话，flowProvider 会被判成"实现了但未配置"，
// 而 admin 的判据是 `ExtOf && Configured` —— 于是所有用它造的用例
// 都走 501 分支，红得莫名其妙（实测踩到）。
// **转发型桩函数必须把接口的每一个方法都转出去。**
func (p *flowProvider) Configured() bool { return p.flow.Configured() }

// AuthDir 转发必须**保留** —— 它是本桩唯一回答"我的凭证目录在哪"的路径。
//
// ⚠ 它与 Configured 不同：`Configured` 转发不出去会让用例走 501 全红
// （**会红**），而 AuthDir 转发不出去会走 pollViaFlow 的
// "空串 → 回落 h.cfg.AuthDir" 分支 —— authdir_test.go 里那条
// "凭证不该落进默认上游目录"的断言才会红。**两者都能被抓到**，
// 但后者只在特定用例里红，所以更要明确写在这里。
//
// 拆出 `gateway.AuthDirExt` 之后，真正被核心断言的是 **flowProvider
// （Provider 面）**，不是 fakeFlow。这条转发链因此变成了：
//
//	ExtOf[AuthDirExt](flowProvider) → flowProvider.AuthDir() → fakeFlow.authDir
//
// 所以这里必须转出去，否则 flowProvider 会自报空串。
func (p *flowProvider) AuthDir() string { return p.flow.AuthDir() }

// LoadCredentials 让 flowProvider 也满足 `gateway.CredentialLoader`。
//
// ⚠ 这不是"为了测试方便"加上去的 —— **两个真实上游都实现了它**
// （`workbuddy.Provider.LoadCredentials` / `codearts.Provider.LoadCredentials`），
// 而 `pollViaFlow` 现在按 `ExtOf[CredentialLoader]` 分派重扫解析器。
//
// 少了它，本桩会被判定为"没实现凭证加载器" → 走 501 分支，
// 于是 `TestPollViaFlowWritesToProviderDir` 会红在"应 200，实际 501"上。
//
// ⚠ 这个坑**正是这次修的 bug 的镜像**：改之前核心写死 `auth.LoadDirCompat`，
// 桩缺不缺 `CredentialLoader` 完全看不出来 —— 测试对"核心到底问没问上游"
// **没有判别力**。加上核心改走扩展点之后，缺接口立刻变红（实测如此）。
// 桩与真实上游的接口面必须一致，否则测试守的是**假想的**系统。
//
// 实现按 workbuddy 真实上游的形状：用真实扫描器
// （`auth.LoadDirCompat`，glob 前缀 `workbuddy*.json`）。这样
// `TestPollViaFlowWritesToProviderDir` 的 200 是**真的**由"扫到了落盘的
// 凭证"支撑的，而不是靠桩返回一个恒真的空切片蒙过去。
//
// 返回的凭证**投影成 uid/nickname**（与真实上游同款）—— 核心只要这两样。
func (p *flowProvider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	base := dir
	if parent := filepath.Dir(dir); parent != "" && parent != dir {
		base = parent
	}
	list, err := auth.LoadDirCompat(base, p.ID())
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		if a == nil || a.UID == "" {
			continue
		}
		out = append(out, gateway.Credential{
			Provider: p.ID(),
			UID:      a.UID,
			Nickname: a.Nickname,
		})
	}
	return out, nil
}

// 编译期断言：flowProvider 必须被核心当成"凭证加载器"认出来。
//
// ⚠ 与上面两条同样重要：少了它，`pollViaFlow` 会返 501，
// 而那与"上游真的不支持"无法区分。
var _ gateway.CredentialLoader = (*flowProvider)(nil)

var _ gateway.LoginFlow = (*flowProvider)(nil)

// 编译期断言：flowProvider 必须能被核心当成"凭证目录自报者"认出来。
//
// ⚠ 这条是**真实约束**，不是装饰：`pollViaFlow` 对 p 做
// `ExtOf[AuthDirExt](p)`，而 p 正是注册表里的 flowProvider。
// 少了它，authdir_test.go 里"凭证写到上游自报目录"那条会
// **静默**退化（不报错的空串回落），根本看不出断言失配。
var _ gateway.AuthDirExt = (*flowProvider)(nil)

// TestLoginStartDispatchByProvider 带 provider 时按上游分派。
func TestLoginStartDispatchByProvider(t *testing.T) {
	flow := &fakeFlow{startState: "ST-abc", startURL: "https://flow.example/auth", configured: true}
	reg := gateway.NewRegistry()
	if err := reg.Register(&flowProvider{
		stubProvider: stubProvider{id: "flowup", caps: gateway.CapChat},
		flow:         flow,
	}); err != nil {
		t.Fatal(err)
	}
	// 另一个上游**没有** LoginFlow
	if err := reg.Register(&stubProvider{id: "noflow", caps: gateway.CapChat}); err != nil {
		t.Fatal(err)
	}

	h := New(Config{Registry: reg, DefaultProvider: "flowup"})

	// ---- 1) 有 LoginFlow 的上游：走它自己的流程 ----
	rec := httptest.NewRecorder()
	req := localReq("POST", "/admin/login/start")
	req.Body = io.NopCloser(strings.NewReader(`{"provider":"flowup"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("有 LoginFlow 的上游应 200，实际 %d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		State    string `json:"state"`
		AuthURL  string `json:"auth_url"`
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v body=%s", err, rec.Body)
	}
	if resp.State != "ST-abc" {
		t.Errorf("state=%q，应来自该上游的 LoginFlow（ST-abc）—— "+
			"若走了默认 OAuth 客户端，说明分派没生效", resp.State)
	}
	if resp.AuthURL != "https://flow.example/auth" {
		t.Errorf("auth_url=%q，应来自该上游的 LoginFlow", resp.AuthURL)
	}
	if resp.Provider != "flowup" {
		t.Errorf("响应应回带 provider=flowup（前端据此显示对应文案），实际 %q", resp.Provider)
	}

	// ---- 2) 没有 LoginFlow 的上游：501，**不能**回落 ----
	rec2 := httptest.NewRecorder()
	req2 := localReq("POST", "/admin/login/start")
	req2.Body = io.NopCloser(strings.NewReader(`{"provider":"noflow"}`))
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotImplemented {
		t.Errorf("没有 LoginFlow 的上游应 501，实际 %d body=%s —— "+
			"**回落是最坏的**：用户以为在给 noflow 加账号，实际加进了别处",
			rec2.Code, rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), "noflow") {
		t.Errorf("501 的错误信息应带上游名（便于排查），实际 %s", rec2.Body)
	}

	// ---- 3) 不存在的上游：也是 501（能力不存在，不是参数错） ----
	rec3 := httptest.NewRecorder()
	req3 := localReq("POST", "/admin/login/start")
	req3.Body = io.NopCloser(strings.NewReader(`{"provider":"ghost"}`))
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotImplemented {
		t.Errorf("不存在的上游应 501，实际 %d body=%s", rec3.Code, rec3.Body)
	}
}

// TestLoginStartBackwardCompatible 不带 provider 时行为与改造前一致。
//
// 这是**向后兼容**的核心断言：老的调用方（脚本、缓存里的旧页面）
// 发的是 `{}` 或空体，必须仍能拿到授权链接。
func TestLoginStartBackwardCompatible(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&stubProvider{id: "plain", caps: gateway.CapChat}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg, DefaultProvider: "plain"})

	// 不带 provider，且**没有配 OAuth 客户端** → 应报错但**不能 panic**，
	// 也不能因为"找不到上游的 LoginFlow"而返回 501。
	// （没配 OAuth 是部署错误，502 是合适的；501 会被误读成"这个上游不支持"。）
	rec := httptest.NewRecorder()
	req := localReq("POST", "/admin/login/start")
	req.Body = io.NopCloser(strings.NewReader(`{}`))
	// 捕获 panic：空 OAuth 客户端是最容易崩的地方
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("不带 provider 且未配 OAuth 时 panic 了：%v —— "+
				"老客户端会整片用不了", r)
		}
	}()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotImplemented {
		t.Errorf("不带 provider 时**不该**返回 501 —— 那是「这个上游不支持添加账号」的语义，"+
			"会让老客户端误以为功能没了。实际 %d body=%s", rec.Code, rec.Body)
	}
}

// TestLoginPollDispatchByProvider 轮询与开始用**同一条**分派规则。
//
// 为什么单独测：两者分派不一致会产生"用 A 的 state 去问 B"的诡异现象 ——
// 表现为随机失败，极难定位。
func TestLoginPollDispatchByProvider(t *testing.T) {
	flow := &fakeFlow{
		configured: true,
		pollFn: func(state string) (gateway.Credential, error) {
			return gateway.Credential{}, gateway.ErrLoginPending
		},
	}
	reg := gateway.NewRegistry()
	if err := reg.Register(&flowProvider{
		stubProvider: stubProvider{id: "flowup", caps: gateway.CapChat},
		flow:         flow,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg, DefaultProvider: "flowup"})

	rec := httptest.NewRecorder()
	req := localReq("POST", "/admin/login/poll")
	req.Body = io.NopCloser(strings.NewReader(`{"state":"ST-abc","provider":"flowup"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("pending 时应 202，实际 %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "pending") {
		t.Errorf("应返回 status=pending，实际 %s", rec.Body)
	}

	// 没有 LoginFlow 的上游 → 501（与 start 一致）
	reg2 := gateway.NewRegistry()
	if err := reg2.Register(&stubProvider{id: "noflow", caps: gateway.CapChat}); err != nil {
		t.Fatal(err)
	}
	h2 := New(Config{Registry: reg2, DefaultProvider: "noflow"})
	rec2 := httptest.NewRecorder()
	req2 := localReq("POST", "/admin/login/poll")
	req2.Body = io.NopCloser(strings.NewReader(`{"state":"S","provider":"noflow"}`))
	h2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotImplemented {
		t.Errorf("poll 对没有 LoginFlow 的上游也应 501，实际 %d body=%s", rec2.Code, rec2.Body)
	}
}

// TestLoginFlowSecretMustMarshal 凭证结构没接入落盘时明确 501。
//
// 反例（必须避免）：把未知结构"尽力映射"成 auth 文件 —— 那会写出
// 看起来成功、实际字段错位的凭证，比明确失败糟得多。
func TestLoginFlowSecretMustMarshal(t *testing.T) {
	flow := &fakeFlow{
		configured: true,
		pollFn: func(state string) (gateway.Credential, error) {
			// Secret 是个不能 MarshalAuthFile 的类型
			return gateway.Credential{UID: "u1", Provider: "flowup", Secret: struct{ X int }{1}}, nil
		},
	}
	reg := gateway.NewRegistry()
	if err := reg.Register(&flowProvider{
		stubProvider: stubProvider{id: "flowup", caps: gateway.CapChat},
		flow:         flow,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg, DefaultProvider: "flowup", AuthDir: t.TempDir()})

	rec := httptest.NewRecorder()
	req := localReq("POST", "/admin/login/poll")
	req.Body = io.NopCloser(strings.NewReader(`{"state":"S","provider":"flowup"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("Secret 不能序列化时应 501（明确失败），实际 %d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "MarshalAuthFile") {
		t.Errorf("错误信息应指出缺什么（MarshalAuthFile），实际 %s", rec.Body)
	}
}
