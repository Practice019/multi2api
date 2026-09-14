// headers_identity_test.go 出站身份头的判据锁定。
//
// # 本文件最重要的一条是第一个用例
//
// 本次改造给 headers 加了一批能力（UA 覆盖 / 归属头 / 设备令牌 / IP 透传），
// 全部设计成 opt-in。因此**最该被钉住的性质是"什么都不配时，出站请求头
// 与改造前逐字节相同"** —— 一旦这条破了，所有既有部署都会在上游那里
// 变成一个新指纹，而那是静默发生的（没有错误、没有日志、只是风控画像变了）。
//
// 其余用例才验证"配了之后确实生效"。
package upstream

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newHdrReq 造一个空请求，供直接调 Headers 方法。
func newHdrReq(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDefaultOutboundIdentityUnchanged 什么都不配 → 与改造前逐字节一致。
//
// # 反向判别力
//
// 把 userAgent() 的第三级（返回 clientUA）改成返回三段式默认值，
// 或让 injectAttribution 无条件发 X-IDE-*，本用例立刻红。
func TestDefaultOutboundIdentityUnchanged(t *testing.T) {
	c := New() // 不设任何身份字段
	a := &auth.Auth{AccessToken: "tok", UID: "u1", EnterpriseID: "e1", Domain: "d1"}
	req := newHdrReq(t)
	c.ChatHeaders(req, a, "")

	if got := req.Header.Get("User-Agent"); got != clientUA {
		t.Errorf("缺省 UA=%q，必须是改造前的常量 %q", got, clientUA)
	}
	if got := req.Header.Get("X-Product"); got != "SaaS" {
		t.Errorf("缺省 X-Product=%q，期望 SaaS（改造前行为）", got)
	}
	// 新增的头一个都不该出现。
	for _, h := range []string{
		"X-IDE-Name", "X-IDE-Type", "X-IDE-Version", "X-Agent-Purpose",
		"X-Device-Token", "X-Forwarded-For", "X-Real-IP", "X-Client-IP",
	} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("缺省不该出现 %s=%q（新增能力必须 opt-in）", h, v)
		}
	}
	// 既有的账号头一个都不能少。
	for h, want := range map[string]string{
		"Authorization":    "Bearer tok",
		"X-User-Id":        "u1",
		"X-Enterprise-Id":  "e1",
		"X-Domain":         "d1",
		"X-Requested-With": "XMLHttpRequest",
	} {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s=%q，期望 %q", h, got, want)
		}
	}
	// 安全红线：chat 请求永不携带 X-Refresh-Token。
	if v := req.Header.Get("X-Refresh-Token"); v != "" {
		t.Errorf("chat 请求不得携带 X-Refresh-Token（得到 %q）", v)
	}
}

// TestUserAgentOverrideVerbatim user_agent 非空 → 逐字使用（最高优先）。
func TestUserAgentOverrideVerbatim(t *testing.T) {
	c := New()
	c.UserAgent = "MyAgent/9.9"
	c.ClientVersion = "5.5.4" // 同时配了 client_version，也应被 user_agent 压住
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "")

	if got := req.Header.Get("User-Agent"); got != "MyAgent/9.9" {
		t.Errorf("UA=%q，期望 user_agent 逐字覆盖（优先级最高）", got)
	}
}

// TestClientVersionEnablesThreeSegmentUA client_version 非空 → 桌面端三段式。
//
// 钉住那个"看起来重复"的两段：官方形态就是 `WorkBuddy/X WorkBuddy/X CLI/Y`。
// 把它"优化"成一段会让 UA 与官方不一致 —— 而这正是本字段存在的目的。
func TestClientVersionEnablesThreeSegmentUA(t *testing.T) {
	c := New()
	c.ClientVersion = "5.5.4"
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "")

	want := "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"
	if got := req.Header.Get("User-Agent"); got != want {
		t.Errorf("UA=%q，期望 %q（两段 WorkBuddy 是官方形态，不是笔误）", got, want)
	}
}

// TestCliVersionOverridesCliSegment cli_version 可覆盖三段式的 CLI 段。
func TestCliVersionOverridesCliSegment(t *testing.T) {
	c := New()
	c.ClientVersion = "5.5.6"
	c.CliVersion = "2.140.0"
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "")

	want := "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.140.0"
	if got := req.Header.Get("User-Agent"); got != want {
		t.Errorf("UA=%q，期望 %q", got, want)
	}
}

// TestClientNameEnablesAttribution client_name 非空 → 归属四头跟随。
func TestClientNameEnablesAttribution(t *testing.T) {
	c := New()
	c.ClientName = "WorkBuddy"
	c.ClientVersion = "5.5.4"
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "")

	for h, want := range map[string]string{
		"X-Agent-Purpose": "conversation",
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   "5.5.4",
		"X-Product":       "WorkBuddy",
	} {
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s=%q，期望 %q", h, got, want)
		}
	}
}

// TestDeviceTokenPrecedence 设备令牌三源优先级：每号 > 配置 > 文件。
func TestDeviceTokenPrecedence(t *testing.T) {
	resetDeviceTokenFileCache()
	t.Cleanup(resetDeviceTokenFileCache)

	dir := t.TempDir()
	fp := filepath.Join(dir, "dev.token")
	if err := os.WriteFile(fp, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// ③ 只有文件 → 用文件值（注意文件里带换行，必须被 trim）。
	c := New()
	c.DeviceTokenFile = fp
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "")
	if got := req.Header.Get("X-Device-Token"); got != "from-file" {
		t.Errorf("文件来源：X-Device-Token=%q，期望 from-file（且已 trim）", got)
	}

	// ② 配置覆盖文件。
	c2 := New()
	c2.DeviceTokenFile = fp
	c2.DeviceToken = "from-config"
	req2 := newHdrReq(t)
	c2.ChatHeaders(req2, &auth.Auth{AccessToken: "t", UID: "u"}, "")
	if got := req2.Header.Get("X-Device-Token"); got != "from-config" {
		t.Errorf("配置来源：X-Device-Token=%q，期望 from-config（高于文件）", got)
	}

	// ① 每号覆盖配置。
	c3 := New()
	c3.DeviceTokenFile = fp
	c3.DeviceToken = "from-config"
	req3 := newHdrReq(t)
	c3.ChatHeaders(req3, &auth.Auth{AccessToken: "t", UID: "u", DeviceToken: "per-account"}, "")
	if got := req3.Header.Get("X-Device-Token"); got != "per-account" {
		t.Errorf("每号来源：X-Device-Token=%q，期望 per-account（优先级最高）", got)
	}
}

// TestDeviceTokenInjectedOnBillingToo billing 域同样注入设备令牌。
//
// report / travel / balance / checkin 都走 billing 域，它们与聊天同属
// 设备风控范围。只在 chat 上注入会让这些自动动作缺一个风控头。
func TestDeviceTokenInjectedOnBillingToo(t *testing.T) {
	c := New()
	c.DeviceToken = "dev-xyz"
	req := newHdrReq(t)
	c.BillingHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"})

	if got := req.Header.Get("X-Device-Token"); got != "dev-xyz" {
		t.Errorf("billing 域 X-Device-Token=%q，期望 dev-xyz", got)
	}
}

// TestDeviceTokenMissingFileDegradesSilently 文件不可读 → 不注入该头（优雅降级）。
//
// 设备令牌是**可选**的：配错了不该让请求失败，只要不发这个头即可。
// 这是"缺一个头的请求上游照样受理，只是风控画像不完整"的取舍。
func TestDeviceTokenMissingFileDegradesSilently(t *testing.T) {
	resetDeviceTokenFileCache()
	t.Cleanup(resetDeviceTokenFileCache)

	c := New()
	c.DeviceTokenFile = filepath.Join(t.TempDir(), "不存在.token")
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "") // 不得 panic

	if v := req.Header.Get("X-Device-Token"); v != "" {
		t.Errorf("文件不可读时不该注入该头，得到 %q", v)
	}
}

// TestPassthroughIPInjectsThreeHeaders passthrough_ip 开启 → 三个等价头都注入。
//
// 三个头一起发是因为不同中间件读不同的名字（Nginx 读 X-Real-IP、
// 网关读 X-Forwarded-For、部分风控读 X-Client-IP）。只发一个会有一半路径看不到。
func TestPassthroughIPInjectsThreeHeaders(t *testing.T) {
	c := New()
	c.PassthroughIP = true
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "203.0.113.7")

	for h := range map[string]bool{
		"X-Forwarded-For": true, "X-Real-IP": true, "X-Client-IP": true,
	} {
		if got := req.Header.Get(h); got != "203.0.113.7" {
			t.Errorf("%s=%q，期望 203.0.113.7", h, got)
		}
	}
}

// TestPassthroughIPOffIgnoresClientIP 默认关闭 → 即使传了 IP 也不注入。
//
// 这是 opt-in 的另一半：XFF 可伪造，默认必须不信任。
func TestPassthroughIPOffIgnoresClientIP(t *testing.T) {
	c := New() // PassthroughIP 默认 false
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "203.0.113.7")

	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "X-Client-IP"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("passthrough_ip=false 时不该注入 %s=%q", h, v)
		}
	}
}

// TestEmptyAccountHeadersUseNoConvention 缺省字段走 X-No-* 约定。
//
// 这是与官方 CLI 一致的约定：上游据此区分"字段确实为空"与"头根本没发"。
// 改造把 headers 从自由函数改成方法时，这一条很容易被顺手改坏。
func TestEmptyAccountHeadersUseNoConvention(t *testing.T) {
	c := New()
	req := newHdrReq(t)
	c.ChatHeaders(req, &auth.Auth{}, "") // 全空账号

	for h := range map[string]bool{
		"X-No-Authorization": true, "X-No-User-Id": true,
		"X-No-Enterprise-Id": true, "X-No-Department-Info": true,
	} {
		if got := req.Header.Get(h); got != "1" {
			t.Errorf("空账号应设 %s=1，得到 %q", h, got)
		}
	}
}

// TestRefreshHeadersCarriesRefreshToken refresh 端点必须带 X-Refresh-Token。
//
// 它与上面那条"chat 永不携带 X-Refresh-Token"是一对：
// 该头只允许出现在刷新请求上，两条一起才构成完整约束。
func TestRefreshHeadersCarriesRefreshToken(t *testing.T) {
	c := New()
	req := newHdrReq(t)
	c.RefreshHeaders(req, &auth.Auth{RefreshToken: "rt-1", EnterpriseID: "e1"})

	if got := req.Header.Get("X-Refresh-Token"); got != "rt-1" {
		t.Errorf("X-Refresh-Token=%q，期望 rt-1", got)
	}
	if got := req.Header.Get("X-Auth-Refresh-Source"); got != "workbuddy" {
		t.Errorf("X-Auth-Refresh-Source=%q，期望 workbuddy", got)
	}
}

// ── 设备令牌文件的缓存语义 ──────────────────────────────────────────────

// TestDeviceTokenFileCacheServesWithinTTL TTL 内不重复读盘。
//
// 做法：先读一次拿到值，然后把文件删掉，再读 —— 若仍能拿到旧值，
// 说明走的是缓存（而不是每次都读盘）。
func TestDeviceTokenFileCacheServesWithinTTL(t *testing.T) {
	resetDeviceTokenFileCache()
	t.Cleanup(resetDeviceTokenFileCache)

	dir := t.TempDir()
	fp := filepath.Join(dir, "tok")
	if err := os.WriteFile(fp, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readDeviceTokenFile(fp); got != "v1" {
		t.Fatalf("首次读取=%q，期望 v1", got)
	}
	if err := os.Remove(fp); err != nil {
		t.Fatal(err)
	}
	if got := readDeviceTokenFile(fp); got != "v1" {
		t.Errorf("TTL 内应命中缓存（文件已删仍返回 v1），得到 %q", got)
	}
}

// TestDeviceTokenFileCacheKeepsLastGoodOnFailure 读失败**保留上一次的有效值**。
//
// # 为什么这是硬判据
//
// 官方客户端写这个文件不是原子的。我们可能恰好在写入窗口读到截断内容 ——
// 清空会让后续所有请求失去风控头（一个静默的能力退化），
// 而旧值至少是曾经被上游接受过的真实令牌。
func TestDeviceTokenFileCacheKeepsLastGoodOnFailure(t *testing.T) {
	resetDeviceTokenFileCache()
	t.Cleanup(resetDeviceTokenFileCache)

	dir := t.TempDir()
	fp := filepath.Join(dir, "tok")
	if err := os.WriteFile(fp, []byte("good-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readDeviceTokenFile(fp); got != "good-token" {
		t.Fatalf("首次读取=%q", got)
	}

	// 模拟"文件被清空"（非原子写的中间态）：值应保留而非变成空。
	if err := os.WriteFile(fp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// 直接把缓存时间戳拨到过期，强制重读。
	dtFileCache.mu.Lock()
	dtFileCache.fetched = time.Now().Add(-2 * deviceTokenFileTTL)
	dtFileCache.mu.Unlock()

	if got := readDeviceTokenFile(fp); got != "good-token" {
		t.Errorf("读到空内容时应保留上一次的有效值，得到 %q", got)
	}
}

// TestDeviceTokenFileRejectsOversize 超大文件判失败（不把整个文件读进内存）。
func TestDeviceTokenFileRejectsOversize(t *testing.T) {
	resetDeviceTokenFileCache()
	t.Cleanup(resetDeviceTokenFileCache)

	dir := t.TempDir()
	fp := filepath.Join(dir, "huge")
	big := make([]byte, deviceTokenFileMaxLen*4)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(fp, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readDeviceTokenFile(fp); got != "" {
		t.Errorf("超出上限的文件应判失败并返回空串，得到 %d 字节的值", len(got))
	}
}

// TestDeviceTokenPathChangeInvalidatesCache 改了配置路径 → 缓存立即失效。
//
// 用户在线改 device_token_file 之后，不该还要等 5 分钟才生效。
func TestDeviceTokenPathChangeInvalidatesCache(t *testing.T) {
	resetDeviceTokenFileCache()
	t.Cleanup(resetDeviceTokenFileCache)

	dir := t.TempDir()
	f1 := filepath.Join(dir, "a")
	f2 := filepath.Join(dir, "b")
	if err := os.WriteFile(f1, []byte("AAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f2, []byte("BBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readDeviceTokenFile(f1); got != "AAA" {
		t.Fatalf("f1=%q", got)
	}
	if got := readDeviceTokenFile(f2); got != "BBB" {
		t.Errorf("路径变更应立刻失效缓存，得到 %q（期望 BBB）", got)
	}
}
