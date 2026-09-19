package admin

// login_incognito_test.go —— 「添加账号」默认用**无痕窗口**打开授权页。
//
// # 为什么这些断言必须存在
//
// 用户的原话是「我希望是默认打开的就是无痕模式」。这件事有两个
// 都必须被钉住的失败面：
//
//	1. 后端根本不去开浏览器（只回 auth_url 让前端自己 target=_blank）——
//	   那就在用户**已登录的浏览器**里打开授权页，OAuth 拿当前会话
//	   完成授权，账号串到别的号上。而且它"成功"了，没有任何报错。
//	2. 开放一个"随便传 URL 就帮你开"的端点 —— 那等于把
//	   「让本机浏览器访问任意地址」暴露给任何能打到 admin 的人。
//	   所以 /admin/login/open **只认本进程自己签发过的链接**。
//
// 另外两条降级语义也必须钉住：开浏览器失败**不能**让整个
// 添加账号流程挂掉（授权链接照样返回，前端回落"复制链接"），
// 以及没接线 opener 时（老测试装配 / 无 GUI 环境）行为与改造前一致。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// recorderOpener 造一个记录调用参数的 opener 桩。
func recorderOpener(browser string, err error) (func(string) (string, error), *[]string) {
	calls := &[]string{}
	fn := func(u string) (string, error) {
		*calls = append(*calls, u)
		if err != nil {
			return "", err
		}
		return browser, nil
	}
	return fn, calls
}

// loginFlowHandler 造一个带 LoginFlow 上游的 handler，返回它和那个 flow。
func loginFlowHandler(t *testing.T, authURL string, open func(string) (string, error)) (*Handler, *fakeFlow) {
	t.Helper()
	flow := &fakeFlow{startState: "ST-inc", startURL: authURL, configured: true}
	reg := gateway.NewRegistry()
	if err := reg.Register(&flowProvider{
		stubProvider: stubProvider{id: "flowup", caps: gateway.CapChat},
		flow:         flow,
	}); err != nil {
		t.Fatal(err)
	}
	return New(Config{Registry: reg, DefaultProvider: "flowup", OpenAuthURL: open}), flow
}

// postLogin 发一条 admin POST 并返回响应。
func postLogin(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := localReq("POST", path)
	req.Body = io.NopCloser(strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

// TestLoginStartOpensIncognitoByDefault 这是本次需求的**主断言**：
// 点「添加账号」时后端自己就去开浏览器，不需要用户再点什么。
//
// ⚠ 断言的是「opener 收到了**这一次签发的那条** URL」，不是"opener 被调过"。
// 前者才能证明开的就是这次授权的页面；后者在"固定开首页"的实现下也绿。
func TestLoginStartOpensIncognitoByDefault(t *testing.T) {
	const authURL = "https://www.workbuddy.ai/oauth/authorize?state=ST-inc"
	open, calls := recorderOpener("Microsoft Edge（无痕）", nil)
	h, flow := loginFlowHandler(t, authURL, open)

	rec := postLogin(t, h, "/admin/login/start", `{"provider":"flowup"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", rec.Code, rec.Body)
	}
	if flow.startURL != authURL {
		t.Fatalf("前置条件错了：flow 应返回 %q", authURL)
	}
	if len(*calls) != 1 {
		t.Fatalf("登录开始时应**自动**打开一次浏览器（用户要求默认无痕），实际调用了 %d 次 —— "+
			"0 次意味着又回到「让用户自己复制链接到无痕窗口」，那正是用户要改掉的体验", len(*calls))
	}
	if (*calls)[0] != authURL {
		t.Errorf("打开的 URL 必须是本次签发的授权链接 %q，实际 %q", authURL, (*calls)[0])
	}

	var resp struct {
		AuthURL string `json:"auth_url"`
		Browser string `json:"browser"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body)
	}
	if resp.Browser == "" {
		t.Errorf("响应必须回带 browser（前端据此告诉用户「已用哪个无痕窗口打开」），"+
			"实际 body=%s", rec.Body)
	}
	if resp.AuthURL != authURL {
		t.Errorf("无论开没开成，auth_url 都必须照常返回（前端要留着做复制链接回落），实际 %q", resp.AuthURL)
	}
}

// TestLoginStartSurvivesOpenFailure 开浏览器失败**不能**拖垮添加账号。
//
// 无 GUI 的服务器、受策略限制的进程都可能开不起来。
// 这时候正确行为是：授权链接照常给，另外如实说明"没开成、请手动复制"。
// 错误行为有两种，都更糟：
//   - 整个请求 5xx → 用户连链接都拿不到；
//   - 静默吞掉 → 界面显示"已用无痕窗口打开"，而窗口并不存在。
func TestLoginStartSurvivesOpenFailure(t *testing.T) {
	const authURL = "https://www.workbuddy.ai/oauth/authorize?state=ST-inc"
	open, _ := recorderOpener("", errString("spawn: access denied"))
	h, _ := loginFlowHandler(t, authURL, open)

	rec := postLogin(t, h, "/admin/login/start", `{"provider":"flowup"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("开浏览器失败时仍应 200（授权链接要照常给），实际 %d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		AuthURL      string `json:"auth_url"`
		Browser      string `json:"browser"`
		BrowserError string `json:"browser_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.AuthURL != authURL {
		t.Errorf("失败时 auth_url 更必须返回（这是用户唯一的退路），实际 %q", resp.AuthURL)
	}
	if resp.BrowserError == "" {
		t.Errorf("失败原因必须**显式**回传（前端要显示「未能自动打开」），实际 body=%s", rec.Body)
	}
	if resp.Browser != "" {
		t.Errorf("失败时不该报一个浏览器名（会显示「已用 X 打开」而窗口不存在），实际 %q", resp.Browser)
	}
}

// TestLoginStartWithoutOpenerKeepsOldBehaviour 没接线 opener 时不得 panic、
// 也不得在响应里编造 browser 字段。
//
// 这条守的是**装配边界**：一大批既有测试（与未来的无 GUI 部署）
// 都不注入 opener，它们必须继续工作。
func TestLoginStartWithoutOpenerKeepsOldBehaviour(t *testing.T) {
	const authURL = "https://www.workbuddy.ai/oauth/authorize?state=ST-inc"
	h, _ := loginFlowHandler(t, authURL, nil)

	rec := postLogin(t, h, "/admin/login/start", `{"provider":"flowup"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("未接线 opener 时应 200，实际 %d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		AuthURL      string `json:"auth_url"`
		Browser      string `json:"browser"`
		BrowserError string `json:"browser_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.AuthURL != authURL {
		t.Errorf("auth_url 必须照常返回，实际 %q", resp.AuthURL)
	}
	if resp.Browser != "" || resp.BrowserError != "" {
		t.Errorf("未接线时不该出现 browser/browser_error（前端会据此显示一个不存在的动作），"+
			"实际 body=%s", rec.Body)
	}
}

// TestLoginOpenRejectsUnissuedURL 这是**安全**断言：
// /admin/login/open 只认本进程签发过的链接。
//
// 少了它，这条端点等于「帮我用本机浏览器打开任意 URL」——
// 而 admin 端点在部署里通常只对本机开放，但"只对本机"不足以
// 让"任意 URL"变成可接受：本机浏览器里带着用户全部登录态，
// 打开一个攻击者给的地址就够钓鱼了。
func TestLoginOpenRejectsUnissuedURL(t *testing.T) {
	open, calls := recorderOpener("Microsoft Edge（无痕）", nil)
	h, _ := loginFlowHandler(t, "https://www.workbuddy.ai/oauth/authorize?state=ST-inc", open)

	rec := postLogin(t, h, "/admin/login/open", `{"url":"https://evil.example/x"}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("未签发过的 URL 必须被拒（否则这条端点就是「用本机浏览器打开任意地址」），实际 200 body=%s", rec.Body)
	}
	if len(*calls) != 0 {
		t.Errorf("被拒的请求**不能**触达 opener，实际调用了 %v", *calls)
	}
}

// TestLoginOpenAcceptsIssuedURL 签发过的链接可以「再开一次无痕窗口」。
//
// 为什么需要这条：用户可能手滑关掉了弹出窗口，此时不该让他
// 重新走一遍登录（state 会变、旧的作废），而应该能重开同一个链接。
func TestLoginOpenAcceptsIssuedURL(t *testing.T) {
	const authURL = "https://www.workbuddy.ai/oauth/authorize?state=ST-inc"
	open, calls := recorderOpener("Microsoft Edge（无痕）", nil)
	h, _ := loginFlowHandler(t, authURL, open)

	// 先走一次 start：把链接记入"本进程签发过"的集合。
	if rec := postLogin(t, h, "/admin/login/start", `{"provider":"flowup"}`); rec.Code != http.StatusOK {
		t.Fatalf("start 应 200，实际 %d", rec.Code)
	}

	rec := postLogin(t, h, "/admin/login/open", `{"url":"`+authURL+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("已签发的链接应能重开，实际 %d body=%s", rec.Code, rec.Body)
	}
	if len(*calls) != 2 || (*calls)[1] != authURL {
		t.Errorf("重开时 opener 应再次收到同一条链接，实际 %v", *calls)
	}
	var resp struct {
		Browser string `json:"browser"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Browser == "" {
		t.Errorf("重开成功应回带 browser，实际 body=%s", rec.Body)
	}
}

// TestLoginOpenWithoutOpenerIs501 未接线 opener 时明确报"不支持"，
// 不能静默 200（前端会显示"已打开"而什么都没发生）。
func TestLoginOpenWithoutOpenerIs501(t *testing.T) {
	h, _ := loginFlowHandler(t, "https://www.workbuddy.ai/oauth/authorize?state=ST-inc", nil)
	rec := postLogin(t, h, "/admin/login/open", `{"url":"https://www.workbuddy.ai/x"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("未接线 opener 时应 501（能力不存在），实际 %d body=%s", rec.Code, rec.Body)
	}
}

// TestLoginOpenRejectsEmptyURL 空 URL 必须被拒，且**不能**触达 opener。
//
// 空串是一条真实出现过的路径：前端拿到的 auth_url 可能是 ""，
// 而不校验就会调 opener("")，某些平台的 opener 会把空串
// 解释成"打开默认浏览器首页"。
func TestLoginOpenRejectsEmptyURL(t *testing.T) {
	open, calls := recorderOpener("Microsoft Edge（无痕）", nil)
	h, _ := loginFlowHandler(t, "https://www.workbuddy.ai/oauth/authorize?state=ST-inc", open)

	rec := postLogin(t, h, "/admin/login/open", `{"url":""}`)
	if rec.Code == http.StatusOK {
		t.Errorf("空 URL 应被拒，实际 200 body=%s", rec.Body)
	}
	if len(*calls) != 0 {
		t.Errorf("空 URL 不该触达 opener，实际 %v", *calls)
	}
}

// errString 一个能用 errors.Is 之外的简单错误（避免引入额外 import 噪音）。
type errString string

func (e errString) Error() string { return string(e) }
