// import_test.go 批量导入端点的路由级与行为级测试。
//
// # 测试策略（与 loomy 的导入测试同一思路）
//
// 打**真实 HTTP 路径**（newAdminTestServer 按 Routes() 挂载），
// 而不是直接调 handler —— 这样"路由声明漏了 / 方法写错了 / 前缀没带上"
// 都会在 404/405 层面暴露，而不是静默通过。
//
// 落盘断言用 **auth.Parse 读回来** 而不是比对字节：落盘格式（嵌套形）的
// 唯一权威是 auth 包自己，测试锁"Parse 能读出预期字段"才锁住了语义
// （字节级断言会把实现焊死在 SaveAtomic 的排版上）。
package workbuddy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

// makeJWT 造一个仅 payload 有意义的假 JWT（签名段恒为 "sig"）。
//
// 导入端点解码 JWT 只为兜底（uid/exp/用户名缺省时从 sub/exp 取），
// 不验签 —— 所以签名段随便填。
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// postImport 向导入端点发一条 JSON。
//
// path 由调用方给：默认实例是 /admin/import，intl 实例经 prefixed()
// 挂在 /workbuddy-intl/admin/import（与其它管理端点同一条前缀规则）。
func postImport(t *testing.T, srv *httptest.Server, path, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v（HTTP %d）", err, resp.StatusCode)
	}
	return resp.StatusCode, out
}

// intlImportPath intl 实例的导入路径（prefixed 规则：/<>前缀 + 相对路径）。
const intlImportPath = "/workbuddy-intl/admin/import"

// importProvider 造一个指向临时凭证目录的 intl 实例。
func importProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	dir := t.TempDir()
	p := NewWithConfig(Config{Provider: "workbuddy-intl", ID: "workbuddy-intl", AuthDir: dir})
	return p, dir
}

// 用户导出工具的**原样形态**（中文键）。测试里 token 用合成的假 JWT ——
// 端点只搬运字节、不解码内容，真不真不影响行为断言。
const sampleIntlItem = `{
  "源": "workbuddy",
  "用户名": "perezsteven2",
  "uid": "d0c45ed6-c72d-445e-b91f-d161517316c6",
  "sessionToken": "%s",
  "refreshToken": "%s",
  "可用额度": 350,
  "是否健康": "健康",
  "expiresAt": 1821537224,
  "healthNote": "积分接口正常（鉴权有效）"
}`

// TestImportWritesIntlCredentialFromChineseKeys 主路径：中文键 JSON →
// intl 凭证文件（channel=intl、domain=www.workbuddy.ai，与页内登录
// 产出的文件同形 —— auth.Parse 读得回来）。
func TestImportWritesIntlCredentialFromChineseKeys(t *testing.T) {
	p, dir := importProvider(t)
	srv := newAdminTestServer(t, p)

	sess := makeJWT(t, map[string]any{"sub": "d0c45ed6-c72d-445e-b91f-d161517316c6", "exp": 1821537224})
	refresh := makeJWT(t, map[string]any{"typ": "Offline"})
	status, out := postImport(t, srv, intlImportPath, fmt.Sprintf(sampleIntlItem, sess, refresh))
	if status != http.StatusOK {
		t.Fatalf("HTTP %d: %v", status, out)
	}
	if got := out["imported"].(float64); got != 1 {
		t.Fatalf("imported=%v，want 1（out=%v）", got, out)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-d0c45ed6-c72d-445e-b91f-d161517316c6.json"))
	if err != nil {
		t.Fatalf("凭证文件未落盘: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("落盘文件解析失败: %v", err)
	}
	if a.AccessToken != sess {
		t.Error("accessToken 与 sessionToken 不一致")
	}
	if a.RefreshToken != refresh {
		t.Error("refreshToken 与导入值不一致")
	}
	if a.ExpiresAt != 1821537224 {
		t.Errorf("expiresAt=%d，want 1821537224", a.ExpiresAt)
	}
	if a.UID != "d0c45ed6-c72d-445e-b91f-d161517316c6" {
		t.Errorf("uid=%q", a.UID)
	}
	if a.Nickname != "perezsteven2" {
		t.Errorf("nickname=%q，want 用户名 perezsteven2", a.Nickname)
	}
	if a.Channel != auth.ChannelIntl {
		t.Errorf("channel=%q，want intl（导入进 workbuddy-intl 池的号必须路由到海外版 base）", a.Channel)
	}
	if a.Domain != "www.workbuddy.ai" {
		t.Errorf("domain=%q，want www.workbuddy.ai", a.Domain)
	}
}

// TestImportAcceptsArrayOfItems 数组形态 + 坏条目隔离：一条缺 accessToken
// 的数据只让它自己失败，不拖累整批。
func TestImportAcceptsArrayOfItems(t *testing.T) {
	p, dir := importProvider(t)
	srv := newAdminTestServer(t, p)

	good1 := fmt.Sprintf(sampleIntlItem, "s1", "r1")
	good2 := strings.Replace(strings.Replace(good1, "perezsteven2", "second.user", 1),
		"d0c45ed6-c72d-445e-b91f-d161517316c6", "aaaaaaaa-1111-2222-3333-444444444444", 1)
	bad := `{"源":"workbuddy","用户名":"no-session","uid":"bbbbbbbb-1111-2222-3333-444444444444"}`
	body := "[" + good1 + "," + good2 + "," + bad + "]"

	status, out := postImport(t, srv, intlImportPath, body)
	if status != http.StatusOK {
		t.Fatalf("HTTP %d: %v", status, out)
	}
	if got := out["imported"].(float64); got != 2 {
		t.Errorf("imported=%v，want 2", got)
	}
	if got := out["failed"].(float64); got != 1 {
		t.Errorf("failed=%v，want 1", got)
	}
	results := out["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results=%d 条，want 3", len(results))
	}
	last := results[2].(map[string]any)
	if last["ok"] != false {
		t.Errorf("坏条目应标记失败: %v", last)
	}
	if msg, _ := last["error"].(string); !strings.Contains(msg, "accessToken") {
		t.Errorf("失败原因应指出缺 accessToken，得到 %q", msg)
	}
	// 坏条目不落盘；两个好的都落盘。
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-bbbbbbbb-1111-2222-3333-444444444444.json")); !os.IsNotExist(err) {
		t.Error("缺 sessionToken 的条目不应落盘")
	}
	for _, uid := range []string{
		"d0c45ed6-c72d-445e-b91f-d161517316c6",
		"aaaaaaaa-1111-2222-3333-444444444444",
	} {
		if _, err := os.Stat(filepath.Join(dir, "workbuddy-"+uid+".json")); err != nil {
			t.Errorf("uid=%s 的凭证未落盘: %v", uid, err)
		}
	}
}

// TestImportDerivesMissingFieldsFromJWT 兜底路径：uid/expiresAt/用户名缺省时
// 从 sessionToken 的 JWT claims（sub / exp / preferred_username）推导。
// Keycloak 签发的 token 恒有这三个 claim —— 导出工具漏列也不致命。
func TestImportDerivesMissingFieldsFromJWT(t *testing.T) {
	p, dir := importProvider(t)
	srv := newAdminTestServer(t, p)

	sess := makeJWT(t, map[string]any{
		"exp":                1821537224,
		"sub":                "d0c45ed6-c72d-445e-b91f-d161517316c6",
		"preferred_username": "perezsteven2",
	})
	body := `{"源":"workbuddy","sessionToken":"` + sess + `","refreshToken":"r"}`
	status, out := postImport(t, srv, intlImportPath, body)
	if status != http.StatusOK {
		t.Fatalf("HTTP %d: %v", status, out)
	}
	if got := out["imported"].(float64); got != 1 {
		t.Fatalf("imported=%v（out=%v）", got, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-d0c45ed6-c72d-445e-b91f-d161517316c6.json"))
	if err != nil {
		t.Fatalf("凭证文件未落盘: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if a.UID != "d0c45ed6-c72d-445e-b91f-d161517316c6" {
		t.Errorf("uid 应从 JWT sub 推导，得到 %q", a.UID)
	}
	if a.ExpiresAt != 1821537224 {
		t.Errorf("expiresAt 应从 JWT exp 推导，得到 %d", a.ExpiresAt)
	}
	if a.Nickname != "perezsteven2" {
		t.Errorf("nickname 应从 preferred_username 推导，得到 %q", a.Nickname)
	}
}

// TestImportDefaultInstanceUsesCNChannel 默认实例（国内版 workbuddy）导入的号
// 必须是 cn 渠道 —— 同一个实现注册两个实例，渠道由"导入进哪个池"决定。
func TestImportDefaultInstanceUsesCNChannel(t *testing.T) {
	dir := t.TempDir()
	p := NewWithConfig(Config{AuthDir: dir}) // ID 空 → workbuddy（国内版）
	srv := newAdminTestServer(t, p)

	sess := makeJWT(t, map[string]any{"sub": "cn-uid-1", "exp": 1821537224})
	body := `{"用户名":"cn.user","uid":"cn-uid-1","sessionToken":"` + sess + `","refreshToken":"r"}`
	status, out := postImport(t, srv, "/admin/import", body)
	if status != http.StatusOK {
		t.Fatalf("HTTP %d: %v", status, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-cn-uid-1.json"))
	if err != nil {
		t.Fatalf("凭证文件未落盘: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if a.Channel != auth.ChannelCN {
		t.Errorf("channel=%q，want cn", a.Channel)
	}
	if a.Domain != "copilot.tencent.com" {
		t.Errorf("domain=%q，want copilot.tencent.com", a.Domain)
	}
}

// TestImportRouteDeclaredForButton 前端「批量导入」按钮的**唯一**触发判据是
// manifest 里有 hidden 的 POST 路由以 /import 结尾（adminRouteBySuffix）。
// 这条钉住：路由必须声明、必须 Hidden（否则会生成一个面板入口）、
// 标题必须非空（挂载契约要求）。
func TestImportRouteDeclaredForButton(t *testing.T) {
	p := NewWithConfig(Config{})
	var found *gateway.AdminRoute
	for i := range p.AdminRoutes() {
		r := p.AdminRoutes()[i]
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/import") {
			found = &p.AdminRoutes()[i]
			break
		}
	}
	if found == nil {
		t.Fatal("AdminRoutes() 里没有 POST …/import —— 前端「批量导入」按钮不会出现")
	}
	if !found.Hidden {
		t.Error("导入路由应声明 Hidden（它是账号池分组行的按钮，不是面板入口）")
	}
	if found.Title == "" {
		t.Error("导入路由缺少 Title")
	}
	if found.Path != "/admin/import" {
		t.Errorf("默认实例的路径=%q，want /admin/import（intl 实例由 prefixed 自动加前缀）", found.Path)
	}
}

// TestImportPrefixedForIntlInstance intl 实例的路径必须带 /workbuddy-intl 前缀
// （与既有端点同一条 prefixed 规则；不带前缀会与国内版实例的路由互相覆盖）。
func TestImportPrefixedForIntlInstance(t *testing.T) {
	p := NewWithConfig(Config{Provider: "workbuddy-intl", ID: "workbuddy-intl"})
	var got []string
	for _, r := range p.AdminRoutes() {
		if r.Method == "POST" && strings.HasSuffix(r.Path, "/import") {
			got = append(got, r.Path)
		}
	}
	if len(got) != 1 || got[0] != "/workbuddy-intl/admin/import" {
		t.Errorf("intl 实例的导入路径=%v，want [/workbuddy-intl/admin/import]", got)
	}
}

// TestImportEmptyBodyRejected 空请求体必须 400 且不 panic
// （契约 verifyAdminHandlersNoPanic 会用裸请求打每一条路由）。
func TestImportEmptyBodyRejected(t *testing.T) {
	p, _ := importProvider(t)
	srv := newAdminTestServer(t, p)
	status, out := postImport(t, srv, intlImportPath, "")
	if status != http.StatusBadRequest {
		t.Fatalf("空 body 的 HTTP=%d，want 400（out=%v）", status, out)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Error("空 body 应给出明确错误信息")
	}
}

// TestImportMalformedJSONRejected 非法 JSON 必须 400 而不是 500。
func TestImportMalformedJSONRejected(t *testing.T) {
	p, _ := importProvider(t)
	srv := newAdminTestServer(t, p)
	status, _ := postImport(t, srv, intlImportPath, "{not-json")
	if status != http.StatusBadRequest {
		t.Fatalf("非法 JSON 的 HTTP=%d，want 400", status)
	}
}

// TestImportOverwritesSameUID 同一 uid 重复导入 → 覆盖旧文件（导入是
// "把最新凭证放进去"的语义，而不是报错挡人）。二次导入后文件内容以
// 第二次为准。
func TestImportOverwritesSameUID(t *testing.T) {
	p, dir := importProvider(t)
	srv := newAdminTestServer(t, p)

	first := strings.Replace(fmt.Sprintf(sampleIntlItem, "sess-one", "r"),
		"1821537224", "1111111111", 1)
	second := strings.Replace(fmt.Sprintf(sampleIntlItem, "sess-two", "r"),
		"1821537224", "2222222222", 1)
	for _, body := range []string{first, second} {
		status, out := postImport(t, srv, intlImportPath, body)
		if status != http.StatusOK {
			t.Fatalf("HTTP %d: %v", status, out)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-d0c45ed6-c72d-445e-b91f-d161517316c6.json"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessToken != "sess-two" || a.ExpiresAt != 2222222222 {
		t.Errorf("重复导入应以最新一次为准，得到 token=%q exp=%d", a.AccessToken, a.ExpiresAt)
	}
}

// TestImportRejectsUnsafeUID uid 被用来拼文件名、也是账号池的主键 ——
// 含路径分隔符、以点开头或带空白/控制字符的 uid 必须逐条拒绝
// （否则导入端点就是一个任意路径写文件的原语；主键也不该被静默改写）。
func TestImportRejectsUnsafeUID(t *testing.T) {
	p, dir := importProvider(t)
	srv := newAdminTestServer(t, p)

	for _, uid := range []string{"../../evil", "a/b", "a\\b", "..", ".", "with space", ""} {
		raw := `{"用户名":"x","uid":"` + uid + `","sessionToken":"s"}`
		status, out := postImport(t, srv, intlImportPath, raw)
		if status == http.StatusOK && out["failed"].(float64) == 0 {
			t.Errorf("uid=%q 不应导入成功: %v", uid, out)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("目录里出现了意外文件: %v", names)
	}
}
