// adminroute_test.go —— 诊断端点的守卫。
//
// # 这里最重要的一条是"绝不回凭证材料"
//
// 本仓库已经吃过一次真实凭证泄漏（测试夹具里抄了作者的活 session）。
// 而这个端点会把本机客户端的登录态**投影出来给人看** ——
// 它是最容易把 session/手机号顺手写进 JSON 的地方。
//
// 所以下面有一条**字节级**断言：回执正文里不许出现 session 的任何一个
// 连续片段，也不许出现未脱敏的展示名。
package loomy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// callClientStore 打一次诊断端点并返回 (状态码, 原始正文)。
func callClientStore(t *testing.T, p *Provider) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, clientStorePath, nil)
	rec := httptest.NewRecorder()
	// 契约测试会直接调 handler（verifyAdminHandlersNoPanic），这里也照同一条走：
	// 万一 handler 对裸请求假设了什么，这条会先炸。
	p.handleClientStore(rec, req)
	return rec.Code, rec.Body.String()
}

// decodeClientStore 解码回执。
func decodeClientStore(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("回执不是合法 JSON: %v\nbody=%s", err, body)
	}
	return out
}

// TestAdminRoutesShape 路由的结构必须完整（契约会真调一次 handler）。
func TestAdminRoutesShape(t *testing.T) {
	p := NewWithConfig(Config{})
	routes := p.AdminRoutes()
	if len(routes) == 0 {
		t.Fatal("★ 声明了 CapQuotaProbe 就必须有管理端点 —— " +
			"契约（unverifiableCaps）会判不合格")
	}
	for i, r := range routes {
		if r.Method == "" {
			t.Errorf("routes[%d] 缺 Method", i)
		}
		if r.Path == "" || !strings.HasPrefix(r.Path, "/") {
			t.Errorf("routes[%d] 的 Path 必须以 / 开头，得到 %q", i, r.Path)
		}
		if r.Handler == nil {
			t.Errorf("routes[%d] 缺 Handler（挂上去是 404）", i)
		}
		if r.Title == "" {
			t.Errorf("routes[%d] 缺 Title", i)
		}
	}
	// ⚠ 不再断言"所有路由都是 CapQuotaProbe + Hidden"。
	//
	// 本轮加了两组**专属面板**路由（tasks / invite），它们**故意**是非 hidden
	// 的 GET —— 只有"非 hidden 的 GET"才会让前端生成一个导航标签页
	//（见 webui.html 的 buildProviderPanels）。所以判据改成分组计数。
	var hiddenQuota, taskPanels, invitePanels int
	for _, r := range routes {
		if r.Capability == gateway.CapQuotaProbe && r.Hidden {
			hiddenQuota++
		}
		if r.Capability == gateway.CapTasks && r.Method == http.MethodGet && !r.Hidden {
			taskPanels++
		}
		if r.Capability == gateway.CapInvite && r.Method == http.MethodGet && !r.Hidden {
			invitePanels++
		}
	}
	if hiddenQuota == 0 {
		t.Error("额度诊断端点应当是 Hidden（它是排障用的，不是面板入口）")
	}
	if taskPanels != 1 {
		t.Errorf("tasks 应当恰好有 1 条**非 hidden 的 GET**（那才会生成标签页），实际 %d", taskPanels)
	}
	if invitePanels != 1 {
		t.Errorf("invite 应当恰好有 1 条非 hidden 的 GET，实际 %d", invitePanels)
	}
}

// TestClientStoreEndpointNeverLeaksSession 回执里**绝不能**出现凭证材料。
//
// # 断言方式：字节级
//
// 不"检查字段名对不对"，而是把 session 的若干片段拿去 `strings.Contains` ——
// 这样无论它出现在哪个字段、以什么嵌套层级，都会被抓到。
func TestClientStoreEndpointNeverLeaksSession(t *testing.T) {
	const session = "abcdef0123456789abcdef0123456789"
	dir := pointsStoreDir(t, "UID-X", session,
		`{"balance":8000,"updatedAt":"2026-09-14T08:15:28.627Z"}`)
	p := NewWithConfig(Config{ClientDataDir: dir})

	code, body := callClientStore(t, p)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200", code)
	}
	// 整串、前半、后半、以及任何 8 字符以上的连续片段。
	for _, frag := range []string{session, session[:8], session[8:16], session[16:24], session[24:]} {
		if strings.Contains(body, frag) {
			t.Fatalf("★ 回执里出现了 session 片段 %q —— 这是真实凭证材料的泄漏：\n%s",
				frag, body)
		}
	}
	// 未脱敏的手机号同样不许出现（Auth.Nickname 在掩码缺失时会回落成完整号码）。
	dir2 := writeStore(t,
		storeRecord(keyAuthSession, storeSessionJSON(session, "UID-Y", "13800138000",
			"2026-09-11T09:35:22.593Z")))
	_, body2 := callClientStore(t, NewWithConfig(Config{ClientDataDir: dir2}))
	if strings.Contains(body2, "13800138000") {
		t.Fatalf("★ 回执里出现了完整手机号：\n%s", body2)
	}

	// 但"可诊断"这件事必须仍然成立：长度与形态要给出来。
	v := decodeClientStore(t, body)
	sess, ok := v["session"].(map[string]any)
	if !ok {
		t.Fatalf("回执里应当有 session 诊断块（脱敏后的）：%s", body)
	}
	if got := sess["session_len"]; got != float64(len(session)) {
		t.Errorf("session_len = %v，want %d（诊断「抄漏了字符」要靠它）",
			got, len(session))
	}
	if got, _ := sess["session_shape"].(string); !strings.Contains(got, "32") {
		t.Errorf("session_shape = %q，应当说明它是 32 位小写 hex", got)
	}
	if got, _ := sess["uid"].(string); got != "UID-X" {
		t.Errorf("uid = %q，want UID-X", got)
	}
}

// TestClientStoreEndpointReportsPoints 积分摘要在（含新鲜度）。
func TestClientStoreEndpointReportsPoints(t *testing.T) {
	dir := pointsStoreDir(t, "UID-X", fixtureSession,
		`{"balance":8000,"dailyBalance":5000,"updatedAt":"2026-09-14T08:15:28.627Z"}`)
	p := NewWithConfig(Config{ClientDataDir: dir})

	_, body := callClientStore(t, p)
	v := decodeClientStore(t, body)
	if v["configured"] != true {
		t.Errorf("configured = %v，want true", v["configured"])
	}
	if v["source"] != "config" {
		t.Errorf("source = %v，want config（诊断「路径从哪来」要靠它）", v["source"])
	}
	pts, ok := v["points"].(map[string]any)
	if !ok {
		t.Fatalf("回执里应当有 points 块：%s", body)
	}
	if pts["balance"] != float64(8000) {
		t.Errorf("points.balance = %v，want 8000", pts["balance"])
	}
	if pts["daily_balance"] != float64(5000) {
		t.Errorf("points.daily_balance = %v，want 5000"+
			"（这一列在账号表里不展示，但诊断要给出两个账本）", pts["daily_balance"])
	}
	if _, present := pts["is_today"]; !present {
		t.Error("points.is_today 必须下发 —— 跨日之后 daily_balance 就不再代表今天的额度")
	}
}

// TestClientStoreEndpointWithoutStore 没有客户端目录 → 明确说明，不 panic。
func TestClientStoreEndpointWithoutStore(t *testing.T) {
	p := NewWithConfig(Config{ClientDataDir: filepath.Join(t.TempDir(), "missing")})
	code, body := callClientStore(t, p)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200（诊断端点对「读不到」回 200 + 说明，而不是错误码）", code)
	}
	v := decodeClientStore(t, body)
	if v["configured"] != false {
		t.Errorf("configured = %v，want false", v["configured"])
	}
	if v["source"] != "config-missing" {
		t.Errorf("source = %v，want config-missing（必须与「本机没装客户端」分开）", v["source"])
	}
	note, _ := v["note"].(string)
	if !strings.Contains(note, "client_data_dir") {
		t.Errorf("说明里应当指出是 loomy.client_data_dir 的问题，实际: %q", note)
	}
}

// TestClientStoreEndpointEmptyStoreDirectory 目录在但没有数据文件 → 说清原因。
func TestClientStoreEndpointEmptyStoreDirectory(t *testing.T) {
	p := NewWithConfig(Config{ClientDataDir: t.TempDir()})
	_, body := callClientStore(t, p)
	v := decodeClientStore(t, body)
	if v["configured"] != true {
		t.Errorf("目录存在时 configured 应当为 true（按钮才出现）")
	}
	note, _ := v["note"].(string)
	if !strings.Contains(note, "登录") {
		t.Errorf("说明里应当指出「客户端尚未登录」，实际: %q", note)
	}
}

// TestClientStoreEndpointToleratesNilProvider 方法在 nil 接收者上不 panic。
//
// 不是理论问题：核心的挂载路径会遍历注册表，而桩/测试里出现过 nil Provider。
// 一个 panic 会让**整张账号表**打不出来（handler 崩在渲染路径上）。
func TestClientStoreEndpointToleratesNilProvider(t *testing.T) {
	var p *Provider
	if routes := p.AdminRoutes(); len(routes) == 0 {
		t.Fatal("nil Provider 也该给出路由，得到 0 条")
	}
	req := httptest.NewRequest(http.MethodGet, clientStorePath, nil)
	rec := httptest.NewRecorder()
	p.handleClientStore(rec, req) // 不许 panic
	if rec.Code != http.StatusOK {
		t.Errorf("HTTP %d，want 200", rec.Code)
	}
}

// TestMaskDisplay 展示名脱敏。
func TestMaskDisplay(t *testing.T) {
	// 本来就掩码过的 → 原样保留（再打一遍码会让用户认不出是哪个号）
	if got := maskDisplay("150****3411"); got != "150****3411" {
		t.Errorf("maskDisplay(已掩码) = %q，want 原样", got)
	}
	// 完整手机号 → 打码（Auth.Nickname 在掩码缺失时会回落成完整号码）
	if got := maskDisplay("13800138000"); got != "138******00" {
		t.Errorf("maskDisplay(完整手机号) = %q，want 138******00（首3末2）", got)
	}
	if got := maskDisplay(""); got != "" {
		t.Errorf("空串应当原样返回，得到 %q", got)
	}
	// 长串（uid）：必须打码，且**保持长度**（长度是辨认依据之一）
	long := maskDisplay("260911173523492332")
	if !strings.Contains(long, "*") {
		t.Errorf("长串必须被打码，得到 %q", long)
	}
	if len(long) != len("260911173523492332") {
		t.Errorf("打码应当保持长度（可辨认），得到 %q", long)
	}
	// 短串：全打码（首3末2放不下）
	if got := maskDisplay("1234"); got != "****" {
		t.Errorf("短串应当全打码，得到 %q", got)
	}
}

// TestClientStoreHint 启动日志的三种形态必须**互不相同**。
//
// 它存在的理由就是"按钮不在"有三个需要分开说的原因（见 ClientStoreHint
// 的注释）。若三种都渲染成同一句，这个函数就没有价值了。
func TestClientStoreHint(t *testing.T) {
	config := ClientStoreHint(t.TempDir()) // 存在的目录 → "config"
	auto := ClientStoreHint("")
	missing := ClientStoreHint(filepath.Join(t.TempDir(), "missing"))

	if !strings.Contains(config, "client_data_dir") {
		t.Errorf("配置路径存在时应当说来自配置，实际: %q", config)
	}
	if !strings.Contains(missing, "不是一个目录") {
		t.Errorf("配置路径不存在时必须**明确指出是配置错**（而不是「没装客户端」），实际: %q", missing)
	}
	if config == missing {
		t.Error("「来自配置」与「配置指错了」不能渲染成同一句话")
	}
	// auto 取决于测试机上有没有装 Loomy，两种都合法，但不能是空串。
	if strings.TrimSpace(auto) == "" {
		t.Error("自动探测的提示不能为空")
	}
}
