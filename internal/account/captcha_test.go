package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captcha_test.go —— 人机校验接缝的端点侧契约（T10 的第 ③ 层）。
//
// # 这一层与限速那一层各自在挡什么
//
//	限速（Limiter）    拦"**同一个 IP** 打很多次"
//	人机校验（Captcha）拦"**很多 IP** 各打几次"
//
// 单机部署一旦映射到公网，注册端点就是"陌生人在你机器上建账号"的入口，而
// 首个账号还是**管理员**。只留限速的话换个 IP 就绕过；只留验证码的话同一道题
// 可以被无限次试（题是一次性的，但可以无限取题）⇒ 两层都要。
//
// # 这组测试挡的失效形态
//
//	① 接线成 nil 的人机校验被当成"启用"（Go 的类型化 nil 坑，见 cmd/server
//	   的 captchaOrNil）⇒ 登录页永远提示"请完成人机校验"
//	② 未启用的部署上端点还会拦（"我明明没开"）⇒ 登录功能整个坏掉
//	③ 答错也放行 / 一道题能反复试 ⇒ 人机校验形同虚设
//	④ 发题端点不做 404 ⇒ 前端无法区分"没开"与"题是空的"，只能显示一个空框
//	⑤ 首启建管理员那条路绕过校验 ⇒ 最要紧的那次注册没被保护

// fakeCaptcha 一个可预测的出题器（真实实现是 authlimit.Captcha 的 SVG 算术题）。
//
// 为什么不直接用 authlimit 的实现：本包**不认识**它（只认接口），这条测试要钉
// 的也正是"接缝两侧互不依赖"。真实实现的题面/一次性/过期语义由
// internal/authlimit 自己的测试负责（captcha_test.go 那 12 条）。
type fakeCaptcha struct {
	issued   []string
	verified []string
	// answer 里记着"哪道题的正确答是什么"；Verify 只认这一份。
	answer map[string]string
	// verifyCalls 用来证明"答错也消耗"（真实现里是 one-shot）。
	verifyCalls int
}

func newFakeCaptcha() *fakeCaptcha {
	return &fakeCaptcha{answer: map[string]string{}}
}

func (f *fakeCaptcha) Issue(now time.Time) (string, string) {
	id := "cap-" + itoa(len(f.issued)+1)
	f.issued = append(f.issued, id)
	f.answer[id] = "7" // 所有题答案固定，测试里够用
	return id, `<svg data-cap="` + id + `"></svg>`
}

func (f *fakeCaptcha) Verify(id, ans string, now time.Time) bool {
	f.verifyCalls++
	// one-shot：无论对错都作废（抄 authlimit.Captcha 的语义）
	want, ok := f.answer[id]
	if ok {
		delete(f.answer, id)
	}
	return ok && strings.TrimSpace(ans) == want
}

// capFixture 带人机校验的端点夹具。
type capFixture struct {
	h  *HTTP
	cp *fakeCaptcha
}

func newCapFixture(t *testing.T, withCaptcha bool) *capFixture {
	t.Helper()
	dir := t.TempDir()
	acc, err := New(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sess, err := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	cfg := HTTPConfig{Accounts: acc, Sessions: sess, RegisterEnabled: true}
	f := &capFixture{}
	if withCaptcha {
		f.cp = newFakeCaptcha()
		cfg.Captcha = f.cp
	}
	h, err := NewHTTP(cfg)
	if err != nil {
		t.Fatalf("端点: %v", err)
	}
	f.h = h
	return f
}

func (f *capFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		return ""
	}
	if c, ok := m["code"].(string); ok {
		return c
	}
	// 嵌套信封（本包 writeError 的形状）
	if e, ok := m["error"].(map[string]any); ok {
		if c, ok := e["code"].(string); ok {
			return c
		}
	}
	if e, ok := m["error"].(string); ok {
		return e
	}
	return ""
}

// ④ 未启用 ⇒ 发题端点 404 captcha_disabled（前端据此隐藏整块，而不是显示空框）。
func TestCaptchaDisabledIsDiscoverable(t *testing.T) {
	f := newCapFixture(t, false)
	rec := f.do(t, http.MethodGet, PathCaptcha, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("未启用时发题端点应当 404（前端据此**隐藏**校验区），实际 %d: %s",
			rec.Code, rec.Body.String())
	}
	if got := codeOf(t, rec); got != "captcha_disabled" {
		t.Errorf("code 应当是 captcha_disabled（不是 not_found：前端要区分「本部署没开」与「路径写错」），实际 %q", got)
	}
}

// ② 未启用 ⇒ 登录/注册**完全不受影响**（不能出现"我明明没开还被拦"）。
func TestCaptchaDisabledDoesNotBlockAuth(t *testing.T) {
	f := newCapFixture(t, false)

	rec := f.do(t, http.MethodPost, PathRegister, `{"username":"alice","password":"alice-password-001"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("未启用人机校验时注册应当直接成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"bootstrap":true`) {
		t.Errorf("空库首次注册应当走 bootstrap（建管理员），实际 %s", rec.Body.String())
	}
	rec = f.do(t, http.MethodPost, PathLogin, `{"username":"alice","password":"alice-password-001"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("未启用人机校验时登录应当成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// ①③ 启用后：没带 / 带错 / 带了但题已作废 ⇒ 400 captcha_required；带对才放行。
func TestCaptchaRequiredAndOneShot(t *testing.T) {
	f := newCapFixture(t, true)

	// 发题
	rec := f.do(t, http.MethodGet, PathCaptcha, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("启用后发题应当 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var issued struct {
		OK         bool   `json:"ok"`
		ID         string `json:"id"`
		SVG        string `json:"svg"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
		t.Fatalf("发题回执不是 JSON: %v", err)
	}
	if !issued.OK || issued.ID == "" || !strings.Contains(issued.SVG, "<svg") {
		t.Fatalf("发题回执缺字段（id/svg/ok）: %s", rec.Body.String())
	}
	if issued.TTLSeconds <= 0 {
		t.Errorf("ttl_seconds 应当为正（前端拿它做过期提示），实际 %d", issued.TTLSeconds)
	}

	// ① 没带 ⇒ 400 captcha_required。**注册端点是第一个要判的**：
	// 首启那条路会把调用方变成管理员。
	rec = f.do(t, http.MethodPost, PathRegister, `{"username":"alice","password":"alice-password-001"}`)
	if rec.Code != http.StatusBadRequest || codeOf(t, rec) != "captcha_required" {
		t.Errorf("注册没带人机校验应当 400 captcha_required，实际 %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bootstrap") {
		t.Error("★ 被人机校验拦住的注册**不得**建出账号（尤其首启那次会建成管理员）")
	}

	// 带错 ⇒ 同一个 code（不区分"缺失"与"答错"：少一个探测口）
	rec = f.do(t, http.MethodPost, PathRegister, `{"username":"alice","password":"alice-password-001","captcha_id":"`+issued.ID+`","captcha_answer":"1"}`)
	if rec.Code != http.StatusBadRequest || codeOf(t, rec) != "captcha_required" {
		t.Errorf("答错应当 400 captcha_required，实际 %d %s", rec.Code, rec.Body.String())
	}

	// ③ 同一道题**再答一次对的**也不行 —— 答错已经把它消耗掉了。
	// （不消耗的话，攻击者可以拿一道题无限试到对，人机校验就没意义了。）
	rec = f.do(t, http.MethodPost, PathRegister, `{"username":"alice","password":"alice-password-001","captcha_id":"`+issued.ID+`","captcha_answer":"7"}`)
	if rec.Code == http.StatusOK {
		t.Error("★ 一道题不能答第二次（答错也作废）—— 否则可以无限试到对")
	}

	// 换一道新题、答对 ⇒ 放行
	rec = f.do(t, http.MethodGet, PathCaptcha, "")
	var issued2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &issued2)
	rec = f.do(t, http.MethodPost, PathRegister, `{"username":"alice","password":"alice-password-001","captcha_id":"`+issued2.ID+`","captcha_answer":"7"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("答对应当放行，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"bootstrap":true`) {
		t.Errorf("空库首次注册应当走 bootstrap，实际 %s", rec.Body.String())
	}

	// 登录侧同样要判（第二个入口）
	rec = f.do(t, http.MethodPost, PathLogin, `{"username":"alice","password":"alice-password-001"}`)
	if rec.Code != http.StatusBadRequest || codeOf(t, rec) != "captcha_required" {
		t.Errorf("登录没带人机校验应当 400 captcha_required，实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, PathCaptcha, "")
	var issued3 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &issued3)
	rec = f.do(t, http.MethodPost, PathLogin, `{"username":"alice","password":"alice-password-001","captcha_id":"`+issued3.ID+`","captcha_answer":"7"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("带对题应当能登录，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// ⑤ 异常长的 id/answer 不送进实现（防"一亿位数字"把内存/日志撑爆），
// 且回的仍是同一个 code（不给"我猜对了形状吗"的反馈）。
func TestCaptchaRejectsOversizedInput(t *testing.T) {
	f := newCapFixture(t, true)
	before := f.cp.verifyCalls

	long := strings.Repeat("9", captchaMaxAnswer+1)
	rec := f.do(t, http.MethodPost, PathRegister,
		`{"username":"bob","password":"bob-password-001","captcha_id":"cap-x","captcha_answer":"`+long+`"}`)
	if rec.Code != http.StatusBadRequest || codeOf(t, rec) != "captcha_required" {
		t.Errorf("超长答案应当 400 captcha_required，实际 %d %s", rec.Code, rec.Body.String())
	}
	if f.cp.verifyCalls != before {
		t.Error("超长输入应当在端点层被挡掉，不送进 Captcha 实现（否则实现要替所有调用方做长度校验）")
	}

	longID := strings.Repeat("a", 129)
	rec = f.do(t, http.MethodPost, PathRegister,
		`{"username":"bob","password":"bob-password-001","captcha_id":"`+longID+`","captcha_answer":"7"}`)
	if rec.Code != http.StatusBadRequest || codeOf(t, rec) != "captcha_required" {
		t.Errorf("超长 id 应当 400 captcha_required，实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 发题端点也要限速（否则它可以被当成廉价的"在服务端留 pending"的手段）。
func TestCaptchaIssueIsRateLimited(t *testing.T) {
	f := newCapFixture(t, true)
	f.h.cfg.Limiter = newTightLimiter(2)

	var first429 int
	for i := 0; i < 5; i++ {
		rec := f.do(t, http.MethodGet, PathCaptcha, "")
		if rec.Code == http.StatusTooManyRequests {
			if first429 == 0 {
				first429 = i + 1
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 必须带 Retry-After（前端要显示「还有多久」）")
			}
		}
	}
	// 桶是 captcha|IP：前 2 次放行，第 3 次起 429
	if first429 != 3 {
		t.Errorf("发题限速应当在第 3 次开始拒绝（额度 2），实际第 %d 次", first429)
	}
}

// tightLimiter 一个"额度用尽"的限速器（只服务本文件的限速断言）。
type tightLimiter struct{ max int }

func newTightLimiter(max int) *tightLimiter { return &tightLimiter{max: max} }

func (l *tightLimiter) Allow(key string, now time.Time) (bool, time.Duration) {
	if !strings.HasPrefix(key, "captcha|") {
		return true, 0
	}
	if l.max <= 0 {
		return false, 30 * time.Second
	}
	l.max--
	return true, 0
}

// 端点契约与请求体解码纪律：只能用小写蛇形字段名（严格解码）。
func TestCaptchaBodyFieldNamesAreStrict(t *testing.T) {
	f := newCapFixture(t, true)
	// 自创别名 ⇒ 400 bad_request（**不是** captcha_required）——
	// 这个区别很重要：前者是"你写错了字段名"，后者是"你还没答题"。
	rec := f.do(t, http.MethodPost, PathRegister,
		`{"username":"carol","password":"carol-password-001","captcha_param":"whatever"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应当 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if got := codeOf(t, rec); got != "bad_request" {
		t.Errorf("未知字段应当是 bad_request（而不是 captcha_required —— 后者会让排查方向完全跑偏），实际 %q", got)
	}
}
