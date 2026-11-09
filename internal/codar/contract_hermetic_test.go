package codar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// TestContract 行为契约（**不碰网络**：伪造凭证，网络失败被容忍）。
func TestContract(t *testing.T) {
	gateway.RunProviderContract(t, NewProvider)
}

// TestCapsOnlyChatAndModels 能力位必须是**恰好**这四个（T-318 起含 checkin/quota-probe）。
func TestCapsOnlyChatAndModels(t *testing.T) {
	p := NewProvider()
	if got := p.Caps(); got != gateway.CapChat|gateway.CapModels|gateway.CapCheckin|gateway.CapQuotaProbe|gateway.CapImport|gateway.CapInvite {
		t.Errorf("Caps() = %v，期望 chat|models|checkin|quota-probe|import|invite", got)
	}
}

// TestModelsNeverEmpty 远端不可用也要返回非空目录（本仓铁律：Models 空 = 声明了却没实现）。
func TestModelsNeverEmpty(t *testing.T) {
	p := NewWithConfig(Config{Client: &Client{APIBase: "http://127.0.0.1:1"}})
	ms, err := p.Models(context.Background(), gateway.Credential{})
	if err != nil {
		t.Fatalf("Models 不该把错误抛出来（应回退静态目录）: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("Models() 返回空：会被判成「声明了 CapModels 却没实现」")
	}
}

// TestProtocolRouting OpenAI 系走 bearer，claude 系走 x-api-key（T-311 实测目录的 protocol 字段）。
func TestProtocolRouting(t *testing.T) {
	cases := []struct{ model, wantPath string }{
		{"deepseek-v4.1-flash", "/v1/chat/completions"},
		{"glm-5.3-flash", "/v1/chat/completions"},
		{"claude-opus-5-5", "/v1/messages"},
		{"claude-sonnet-5-5-kiro", "/v1/messages"},
	}
	for _, c := range cases {
		got, _ := protocolOf([]byte(`{"model":"` + c.model + `"}`))
		if got != c.wantPath {
			t.Errorf("model=%s 路由 = %s，want %s", c.model, got, c.wantPath)
		}
	}
}

// TestClassifySeparatesIPBlockFromAccountRestricted ★ 本包最关键的一条：
// 出口 IP 被封（nginx 403）与账号被封（account_restricted）必须分开，否则会把好号批量禁用。
func TestClassifySeparatesIPBlockFromAccountRestricted(t *testing.T) {
	p := NewWithConfig(Config{})
	nginx := "<html><center><h1>403 Forbidden</h1></center><hr><center>nginx/1.24.0 (Ubuntu)</center></html>"
	if got := p.Classify(403, nginx); got != gateway.ErrKindSoftRate {
		t.Errorf("nginx 403（出口 IP 被封）判成 %v，期望 SoftRate（短冷却、不禁用账号）", got)
	}
	if got := p.Classify(403, `{"error":{"message":"account_restricted"}}`); got != gateway.ErrKindSessionDead {
		t.Errorf("account_restricted 判成 %v，期望 SessionDead（该号需人工处理）", got)
	}
	if got := p.Classify(401, `{"error":{"message":"Not authenticated"}}`); got != gateway.ErrKindSessionDead {
		t.Errorf("401 token 失效判成 %v，期望 SessionDead", got)
	}
	if got := p.Classify(429, "too many requests"); got != gateway.ErrKindSoftRate {
		t.Errorf("429 判成 %v，期望 SoftRate", got)
	}
	if got := p.Classify(500, "oops"); got != gateway.ErrKindServer {
		t.Errorf("500 判成 %v，期望 Server", got)
	}
	if got := p.Classify(400, `{"error":{"message":"Missing 'model' field","type":"invalid_request_error","code":400}}`); got != gateway.ErrKindFatalClient {
		t.Errorf("缺 model 的 400 判成 %v，期望 FatalClient（换号也不会成功）", got)
	}
}

// TestImportFromDesktopConfig 能读出桌面端 token（临时文件，不碰真实配置）。
func TestImportFromDesktopConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	token := makeJWT(time.Now().Unix(), time.Now().Add(3*time.Hour).Unix())
	raw := `{"version":1,"provider":{"auth_type":"speedai","active_profile":"speedai",` +
		`"speedai":{"token":"` + token + `","username":"user_test_1","login_at":1794070329644},` +
		`"api_base":"https://api.claw.speedai.com/v1","api_type":"openai"},` +
		`"provider_profiles":{"speedai":{"api_base":"https://api.claw.speedai.com/v1"}}}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	a, base, err := ImportFromDesktopConfig(p)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if a.Token != token || a.Username != "user_test_1" || a.UID != "user_test_1" {
		t.Errorf("导入字段不对: token_len=%d username=%q uid=%q", len(a.Token), a.Username, a.UID)
	}
	if base != "https://api.claw.speedai.com/v1" {
		t.Errorf("api_base = %q", base)
	}
	if a.ExpiresAt == 0 {
		t.Error("应当从 JWT 解出 exp")
	}
}

// TestLoadDirDedupesByUID 同一 UID 只保留到期最晚的一份（账号边界收敛）。
func TestLoadDirDedupesByUID(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"codar-a.json": `{"auth":{"token":"t-old","username":"u1","expiresAt":100},"account":{"uid":"u1"}}`,
		"codar-b.json": `{"auth":{"token":"t-new","username":"u1","expiresAt":200},"account":{"uid":"u1"}}`,
		"codar-c.json": `{"auth":{"token":"t-other","username":"u2","expiresAt":50},"account":{"uid":"u2"}}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("去重后应剩 2 个账号，得到 %d", len(list))
	}
	for _, a := range list {
		if a.UID == "u1" && a.Token != "t-new" {
			t.Errorf("u1 应保留到期最晚的 t-new，得到 %s", a.Token)
		}
	}
}

// TestSaveOneIs0600 凭证落盘权限必须是 0600。
func TestSaveOneIs0600(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{Token: "t", Username: "u9", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p, err := SaveOne(dir, a)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠ Windows 上 Go 的权限位是 POSIX 语义的近似：Mode().Perm() 恒为 0666
	//   （ACL 才决定真实访问权）⇒ 严格断言只在 POSIX 上做，Windows 上改为
	//   "文件确实写出来且内容能解析回来"（否则这条测试会在 Windows 上永远红）。
	if runtime.GOOS != "windows" {
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("权限 = %v，期望 0600", fi.Mode().Perm())
		}
	}
	if fi.Size() == 0 {
		t.Error("凭证文件是空的")
	}
	if _, err := os.ReadFile(p); err != nil {
		t.Errorf("凭证文件读不回来: %v", err)
	}
}

// makeJWT 造一个 payload 段有意义、签名段无意义的 JWT（测试专用）。
func makeJWT(iat, exp int64) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	payload := `{"iat":` + strconv.FormatInt(iat, 10) + `,"exp":` + strconv.FormatInt(exp, 10) + `,"username":"user_test_1"}`
	return enc(`{"alg":"HS256","typ":"JWT"}`) + "." + enc(payload) + ".sig"
}

// TestForceStreamForcesStreamTrue ★ 真机踩出来的回归：不强制流式 ⇒ 出口层解析不到 SSE ⇒ 502 upstream_parse。
func TestForceStreamForcesStreamTrue(t *testing.T) {
	in := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}],"max_tokens":50}`)
	out := forceStream(in)
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("没有强制 stream=true: %s", out)
	}
	// 其它字段（含数字精度）必须原样保留
	for _, want := range []string{`"model":"glm-5.3-flash"`, `"max_tokens":50`, `"content":"hi"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("字段被改坏：期望含 %s，实际 %s", want, out)
		}
	}
	// 已经是 true 的原样返回（不改字节）
	already := []byte(`{"stream":true,"x":1}`)
	if string(forceStream(already)) != string(already) {
		t.Error("已经是 true 时不该改动原始字节")
	}
	// 非 JSON 原样返回
	bad := []byte(`not json`)
	if string(forceStream(bad)) != string(bad) {
		t.Error("非 JSON 应原样透传")
	}
}

// TestSaveOneDerivesExpiryFromToken ★ 真机踩过：ExpiresAt=0 落盘 ⇒ 界面看不到到期、
// 且 dedupeByUID 会把这份当成"最旧"。SaveOne 必须自己从 token 解出 exp 补上。
func TestSaveOneDerivesExpiryFromToken(t *testing.T) {
	dir := t.TempDir()
	exp := time.Now().Add(6 * 24 * time.Hour).Unix()
	a := &Auth{Token: makeJWT(time.Now().Unix(), exp), Username: "u-exp"} // 故意不填 ExpiresAt
	p, err := SaveOne(dir, a)
	if err != nil {
		t.Fatal(err)
	}
	if a.ExpiresAt != exp {
		t.Errorf("内存里 ExpiresAt = %d，want %d", a.ExpiresAt, exp)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Auth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Auth.ExpiresAt != exp {
		t.Errorf("落盘的 expiresAt = %d，want %d（0 表示没解出来）", back.Auth.ExpiresAt, exp)
	}
}

// TestP0Extensions P0 四个小扩展点 + 管理端点（T-317）。
func TestP0Extensions(t *testing.T) {
	p := NewWithConfig(Config{})
	if got := p.DisplayName(); got == "" {
		t.Error("DisplayName 为空")
	}
	if got := p.Notice(); got == "" {
		t.Error("Notice 为空")
	}
	if p.NeverExpires(gateway.Credential{}) {
		t.Error("NeverExpires 必须为 false：JWT 带 exp，报 true 会让核心不再续期")
	}
	a := &Auth{Token: "t"}
	if !p.HasToken(gateway.Credential{Secret: a}) {
		t.Error("HasToken 对有 token 的凭证应为 true")
	}
	if p.HasToken(gateway.Credential{}) {
		t.Error("HasToken 对空凭证应为 false（不 panic 也要判 false）")
	}
	routes := p.AdminRoutes()
	if len(routes) < 4 {
		t.Fatalf("AdminRoutes = %d 条，want >= 4", len(routes))
	}
	for _, r := range routes {
		if r.Method == "" || r.Path == "" || r.Handler == nil {
			t.Errorf("路由字段不全: %+v", r)
		}
		// ★ T-351 修正：这条断言的本意是"路由报的能力位必须**已在 Caps() 里声明**"
		//（否则前端会画一个点了必然失败的假按钮）。原写法是 `r.Capability != 0` 一律拒，
		// 那是能力位还没被用到路由上的年代写的 —— 现在邀请码页正是靠路由的能力位显示。
		if r.Capability != 0 && (p.Caps()&r.Capability) != r.Capability {
			t.Errorf("路由 %s 报了能力位 %v，但 Caps() 只声明了 %v —— 前端会画假按钮", r.Path, r.Capability, p.Caps())
		}
	}
	// 未注入凭证源时必须**优雅报错**，不能 panic。
	w := httptest.NewRecorder()
	p.handleQuota(w, httptest.NewRequest("GET", "/admin/codar/quota", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("没凭证源时状态码 = %d，want 503", w.Code)
	}
}

// TestP1Capabilities P1 能力（T-318）：能力位、每日动作、续期**默认开启**（T-382）、倍率算法。
func TestP1Capabilities(t *testing.T) {
	p := NewWithConfig(Config{})
	caps := p.Caps()
	for _, want := range []gateway.Capability{gateway.CapChat, gateway.CapModels, gateway.CapCheckin, gateway.CapQuotaProbe} {
		if caps&want == 0 {
			t.Errorf("Caps 缺少 %v", want)
		}
	}
	// 每日动作：**只剩签到**（T-465 删掉了"领活动模型"那条，理由见 p1.go 的 DailyActions()），
	// 且 OneURL/AllURL 必须非空（空 URL = 点了 404）。
	acts := p.DailyActions()
	if len(acts) != 1 {
		t.Fatalf("DailyActions = %d 个，want 1（只剩签到）", len(acts))
	}
	if acts[0].ID != DailyActionSign {
		t.Errorf("唯一的动作 ID = %q，want %q", acts[0].ID, DailyActionSign)
	}
	for _, a := range acts {
		if a.ID == "" || a.Label == "" || a.OneURL == "" || a.AllURL == "" {
			t.Errorf("动作字段不全: %+v", a)
		}
	}
	// 路由必须真的挂上动作声明的 URL（否则按钮点了 404）
	routes := map[string]bool{}
	for _, r := range p.AdminRoutes() {
		routes[r.Path] = true
	}
	for _, a := range acts {
		if !routes[a.OneURL] {
			t.Errorf("动作 %s 的 OneURL %s 没挂在 AdminRoutes 里", a.ID, a.OneURL)
		}
		if !routes[a.AllURL] {
			t.Errorf("动作 %s 的 AllURL %s 没挂在 AdminRoutes 里", a.ID, a.AllURL)
		}
	}
	// 签到的 OneURL 必须把 uid 传成 query（多账号不能串号）。
	// T-465 后只剩 1 个动作 ⇒ 1 个动作 × (OneURL+AllURL) = 2。
	if got := len(p.dailyActionURLs()); got != 2 {
		t.Errorf("dailyActionURLs = %d，want 2（1 个动作 × OneURL+AllURL）", got)
	}
	// ★ T-382：续期**默认开启**（原为默认关闭）。
	//
	// 依据：p1.go 文件头写的"默认关闭"理由（怕提前轮换踢掉桌面端），已被**同一个文件里的实测**推翻 ——
	// 上游对未临期的 token 明确拒绝：POST /v1/token/refresh → 400
	// {"code":400,"message":"Token is not close to expiration"}
	// ⇒ 早调不会轮换、不会踢掉桌面端会话 ⇒ 默认关闭买不到安全，只让用户撞上"token 过期要重新登录"。
	//
	// 下面四档钉的是**护栏**（防"一天 2880 次请求"那次事故），一条都不许放宽：
	// ① 无凭证 ⇒ 不触发（无事可做）
	// ② 新鲜凭证（剩 10 天）⇒ 不触发（不该续的时候不续）
	// ③ 距上次尝试 < 1h ⇒ 不触发（失败也要退避）
	// ④ 临期凭证（剩 2h）⇒ 触发（这才该干活）
	//
	// ① 无凭证 ⇒ Due 恒 false
	for _, j := range p.Jobs() {
		if j.Due != nil && j.Due(time.Now()) {
			t.Errorf("任务 %s 在无凭证时不该到期（无事可做）", j.Name)
		}
	}
	// ② 新鲜凭证 ⇒ 不触发
	dir := t.TempDir()
	freshJWT := makeJWT(time.Now().Add(-1*time.Hour).Unix(), time.Now().Add(10*24*time.Hour).Unix())
	if _, err := SaveOne(dir, &Auth{Token: freshJWT, Username: "u-fresh"}); err != nil {
		t.Fatal(err)
	}
	pFresh := NewWithConfig(Config{AuthDir: dir})
	for _, j := range pFresh.Jobs() {
		// ★ T-424：只看 token-refresh。签到 Job（codar-daily-checkin）在
		// 「有凭证但今天没签」时 **Due 合法地为真** —— 那是它的职责，
		// 与本用例要验的「新鲜凭证不该触发**续期**」无关。
		if j.Name != "codar-token-refresh" {
			continue
		}
		if j.Due != nil && j.Due(time.Now()) {
			t.Error("新鲜凭证（剩 10 天）不该触发续期")
		}
	}
	// ③ 临期凭证 ⇒ 触发（默认开启后**不需要任何环境变量**）
	near := makeJWT(time.Now().Add(-6*24*time.Hour).Unix(), time.Now().Add(2*time.Hour).Unix())
	if _, err := SaveOne(dir, &Auth{Token: near, Username: "u-near"}); err != nil {
		t.Fatal(err)
	}
	pNear := NewWithConfig(Config{AuthDir: dir})
	due := false
	for _, j := range pNear.Jobs() {
		if j.Due != nil && j.Due(time.Now()) {
			due = true
		}
	}
	if !due {
		t.Error("临期凭证（剩 2 小时）应当触发续期（默认开启后不需要任何环境变量）")
	}
	// ④ 距上次尝试不足 1 小时 ⇒ 必须退避
	pNear.lastRefreshTry = time.Now()
	for _, j := range pNear.Jobs() {
		// ★ T-424：同上 —— 只看 token-refresh（签到 Job 的 Due 与本用例无关）
		if j.Name != "codar-token-refresh" {
			continue
		}
		if j.Due != nil && j.Due(time.Now()) {
			t.Error("距上次尝试不足 1 小时 ⇒ 必须退避（否则失败后每 30s 重试）")
		}
	}
}

// TestCodarHasNoFreeModelClaimAction T-465 防复活守卫：codar **不应**再有"领取上游
// 赠送模型"那套入口（每日动作 + 管理路由）。
//
// # 为什么删（依据是实测，不是审美）
//
//	GET /admin/codar/claim?uid=… ⇒ activity_status=unavailable、visible=false
//
// 那个入口唯一的作用是把"不可领"回显给用户。用户看到按钮问「这按钮有啥用」——
// 答不上来就是不该存在。删掉后若上游重开活动，重新实现比维护一个恒失败入口更好。
//
// # 为什么这条守卫断言的是"三件不同的事"
//
// 只查一处会漏：动作清单、管理路由、以及**URL 生成器**（dailyActionURLs）是三条
// 独立的代码路径，历史上（本仓）出现过"删了动作定义但 URL 常量还留着"这种半删状态。
//
// # 三条断言都刻意用**子串**而不是完整标识符
//
// 子串足够精确（本包内没有任何正当路径含这些片段），而完整标识符会让本文件的注释
// 变成"被删代码的第二次登记" —— 那正是墓碑注释要避免的。
func TestCodarHasNoFreeModelClaimAction(t *testing.T) {
	p := NewWithConfig(Config{})

	// ① 每日动作里不能有第二类条目（签到以外的一律不该存在）
	for _, a := range p.DailyActions() {
		if a.ID != DailyActionSign {
			t.Errorf("每日动作里出现了非签到条目：ID=%q Label=%q（T-465 已删除该功能）", a.ID, a.Label)
		}
	}

	// ② 管理路由里不能有那条路径
	for _, r := range p.AdminRoutes() {
		if strings.Contains(r.Path, "/claim") {
			t.Errorf("管理路由里还有已删除的端点：%s %s（Title=%q）", r.Method, r.Path, r.Title)
		}
	}

	// ③ 面向界面的文案里不能有那两个词（它们就是账号池上那个按钮的字）
	for _, r := range p.AdminRoutes() {
		if strings.Contains(r.Title, "免费模型") || strings.Contains(r.Title, "领模型") {
			t.Errorf("路由 Title 里还有已删除功能的名字：%q", r.Title)
		}
	}
	for _, a := range p.DailyActions() {
		if strings.Contains(a.Label, "免费模型") || strings.Contains(a.Title, "免费模型") {
			t.Errorf("每日动作文案里还有已删除功能的名字：Label=%q Title=%q", a.Label, a.Title)
		}
	}

	// ④ URL 生成器只该产出签到那两个（1 个动作 × OneURL+AllURL = 2）
	for _, u := range p.dailyActionURLs() {
		if strings.Contains(u, "/claim") {
			t.Errorf("dailyActionURLs 里还有已删除的 URL：%s", u)
		}
	}
	if got := len(p.dailyActionURLs()); got != 2 {
		t.Errorf("dailyActionURLs = %d，want 2（只剩签到）", got)
	}
}

// TestRecordSignWritesCheckinlog 签到结果必须落进 checkinlog（「今日签到」列的数据源）。
// ★ 不碰上游：直接调 recordSign，用临时文件当日志。
func TestRecordSignWritesCheckinlog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkin.json")
	lg := checkinlog.New(path, 3)
	p := NewWithConfig(Config{Log: lg})
	// already 必须是 already（不能美化成功）
	p.recordSign(gateway.DailyCheckinResult{UID: "u-already", Status: checkinlog.StatusAlready, Detail: "Already signed"}, "manual")
	// fail 也要落盘
	p.recordSign(gateway.DailyCheckinResult{UID: "u-fail", Status: checkinlog.StatusFail, Detail: "上游 HTTP 500"}, "manual")
	// 空状态必须兜底成 skip（而不是写一条空 status 的僵尸记录）
	p.recordSign(gateway.DailyCheckinResult{UID: "u-empty"}, "manual")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("日志文件没写出来: %v", err)
	}
	s := string(raw)
	for _, want := range []string{"u-already", `"already"`, "u-fail", `"fail"`, "u-empty", `"skip"`, `"kind": "checkin"`, `"trigger": "manual"`} {
		if !strings.Contains(s, want) {
			t.Errorf("日志里缺少 %s（实际内容 %s）", want, s)
		}
	}
	// nil Log 时不能 panic（契约：零值 Config 也要能跑）
	NewWithConfig(Config{}).recordSign(gateway.DailyCheckinResult{UID: "x", Status: "ok"}, "manual")
}

// TestRenewNotCloseIsBenign ★ T-321：上游对未临期 token 返回 400
// {"code":400,"message":"Token is not close to expiration"} —— 必须当"还没到时候"静默跳过，
// 否则续期任务每 6 小时刷一条假错误（实测踩过）。
func TestRenewNotCloseIsBenign(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/token/refresh" {
			t.Errorf("路径 = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"message":"Token is not close to expiration"}`))
	}))
	defer srv.Close()
	p := NewWithConfig(Config{PayBase: srv.URL, Client: &Client{APIBase: DefaultAPIBase, PayBase: srv.URL}})
	if err := p.renewOne(context.Background(), &Auth{Token: "t", Username: "u"}); err != nil {
		t.Fatalf("未临期应当是良性跳过（返回 nil），实际 %v", err)
	}
	// 其它 400（真错误）仍必须报错
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"message":"bad request"}`))
	}))
	defer srv2.Close()
	p2 := NewWithConfig(Config{PayBase: srv2.URL, Client: &Client{APIBase: DefaultAPIBase, PayBase: srv2.URL}})
	if err := p2.renewOne(context.Background(), &Auth{Token: "t", Username: "u"}); err == nil {
		t.Fatal("其它 400 应当报错（不能把所有 400 都当良性）")
	}
}

// TestImportCredentialsForms ★ T-322：手工粘贴必须能吃三种形态（用户要求自包含，这是保底入口）。
func TestImportCredentialsForms(t *testing.T) {
	p := NewWithConfig(Config{})
	tok := makeJWT(time.Now().Unix(), time.Now().Add(72*time.Hour).Unix())

	// ① 裸 JWT
	out, err := p.ImportCredentials(tok)
	if err != nil {
		t.Fatalf("裸 JWT 导入失败: %v", err)
	}
	if len(out) != 1 || out[0].UID == "" || !strings.HasPrefix(out[0].FileName, "codar-") {
		t.Fatalf("裸 JWT 结果不对: %+v", out)
	}
	if !strings.Contains(string(out[0].Raw), tok) {
		t.Error("落盘内容里没有令牌")
	}

	// ② 桌面端 config 全文（兼容老路径，方便用户直接粘贴）
	cfg := `{"provider":{"speedai":{"token":"` + tok + `","username":"user_paste_1"}}}`
	out2, err := p.ImportCredentials(cfg)
	if err != nil {
		t.Fatalf("config 导入失败: %v", err)
	}
	if len(out2) != 1 || out2[0].UID != "user_paste_1" {
		t.Fatalf("config 导入结果不对: %+v", out2)
	}

	// ③ 本仓落盘格式（可再导入）
	blob := `{"auth":{"token":"` + tok + `","username":"user_paste_1"},"account":{"uid":"user_paste_1","nickname":"n"}}`
	out3, err := p.ImportCredentials(blob)
	if err != nil {
		t.Fatalf("本仓格式导入失败: %v", err)
	}
	if len(out3) != 1 || out3[0].UID != "user_paste_1" {
		t.Fatalf("本仓格式结果不对: %+v", out3)
	}

	// ④ 垃圾输入必须报错（不返回半成品）
	if _, err := p.ImportCredentials("这不是 token"); err == nil {
		t.Error("垃圾输入应当报错")
	}
	// ⑤ 解析出来的 expiresAt 必须非 0（T-315 修过的坑）
	a, err := ParseCredential([]byte(tok))
	if err != nil || a.ExpiresAt == 0 {
		t.Fatalf("裸 JWT 应解出 exp: err=%v exp=%d", err, a.ExpiresAt)
	}
}

// TestPhoneLoginFlow ★ T-324：手机号验证码登录（对标 loomy）。全程 httptest，不发真短信。
func TestPhoneLoginFlow(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/v1/verification-code":
			_, _ = w.Write([]byte(`{"code":200,"success":true,"message":"ok"}`))
		case "/v1/login":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["password"] == "000000" {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"code":201,"status":"ip_verify_required","message":"need ip verify","data":{"token":"tmp-token"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":200,"data":{"token":"` + makeJWT(time.Now().Unix(), time.Now().Add(72*time.Hour).Unix()) + `","username":"13800138000"}}`))
		case "/v1/user/verify_ip":
			if r.Header.Get("token") != "tmp-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"code":200,"data":{"token":"` + makeJWT(time.Now().Unix(), time.Now().Add(72*time.Hour).Unix()) + `","username":"13800138000"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	p := NewWithConfig(Config{PayBase: srv.URL, Client: &Client{APIBase: DefaultAPIBase, PayBase: srv.URL}})

	good := map[string]string{"lot_number": "l", "captcha_output": "c", "pass_token": "p", "gen_time": "1"}

	// ① 手机号格式不对 ⇒ **本地**拦下，绝不能打上游（否则会真发短信）
	if err := p.SendLoginCode(context.Background(), "12345", good, "login"); err == nil {
		t.Fatal("非法手机号应当报错")
	}
	if hits != 0 {
		t.Fatalf("非法手机号不该打上游，实际打了 %d 次", hits)
	}
	// ② 缺 captcha 字段 ⇒ 本地拦下（同样不打上游）
	if err := p.SendLoginCode(context.Background(), "13800138000", map[string]string{"lot_number": "l"}, "login"); err == nil {
		t.Fatal("缺 captcha 字段应当报错")
	}
	if hits != 0 {
		t.Fatalf("缺 captcha 时不该打上游，实际 %d 次", hits)
	}
	// ③ 正常发码 ⇒ 打上游 1 次
	if err := p.SendLoginCode(context.Background(), "13800138000", good, "login"); err != nil {
		t.Fatalf("发码失败: %v", err)
	}
	if hits != 1 {
		t.Fatalf("发码应当只打 1 次上游，实际 %d", hits)
	}
	// ④ 直接登录 ⇒ 拿到 Auth
	a, err := p.LoginByCode(context.Background(), "13800138000", "123456")
	if err != nil || a == nil || a.Token == "" || a.UID == "" {
		t.Fatalf("登录失败: a=%+v err=%v", a, err)
	}
	if a.ExpiresAt == 0 {
		t.Error("登录得到的凭证必须带 exp（否则界面看不到到期时间）")
	}
	// ⑤ IPC 验证分支 ⇒ 必须返回**带临时 token 的明确信号**（不能当普通错误吞掉）
	_, err = p.LoginByCode(context.Background(), "13800138000", "000000")
	ipErr, ok := err.(*errIPVerifyRequired)
	if !ok {
		t.Fatalf("应当返回 *errIPVerifyRequired，实际 %T（%v）", err, err)
	}
	if ipErr.TmpToken != "tmp-token" {
		t.Errorf("临时 token = %q", ipErr.TmpToken)
	}
	// ⑥ 带上临时 token 走 IP 验证 ⇒ 成功
	a2, err := p.VerifyIP(context.Background(), "13800138000", "654321")
	if err != nil || a2 == nil || a2.Token == "" {
		t.Fatalf("IP 验证失败: %v", err)
	}
	// ⑦ 没有待办会话时不得瞎走
	p2 := NewWithConfig(Config{PayBase: srv.URL, Client: &Client{APIBase: DefaultAPIBase, PayBase: srv.URL}})
	if _, err := p2.VerifyIP(context.Background(), "13800138000", "654321"); err == nil {
		t.Error("没有 IP 验证会话时应当报错")
	}
}

// TestLoginPageServed 登录页必须能下发，且带官方 captcha 脚本与三个端点路径。
func TestLoginPageServed(t *testing.T) {
	p := NewWithConfig(Config{})
	rec := httptest.NewRecorder()
	p.handleLoginPage(rec, httptest.NewRequest(http.MethodGet, loginPagePath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("登录页状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"initAlicom4", CaptchaJSURL, DefaultCaptchaID, loginSMSPath, loginVerPath, loginIPPath, "手机号"} {
		if !strings.Contains(body, want) {
			t.Errorf("登录页缺少 %s", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
}
