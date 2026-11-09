package codar

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// stubInviteUpstream 桩上游：按路径记录请求体并返回预设响应（T-353 的 seam 测试用）。
func stubInviteUpstream(t *testing.T, routes map[string]string, seen *map[string]string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if seen != nil {
			(*seen)[r.URL.Path] = string(b)
		}
		if body, ok := routes[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"stub: no route"}`))
	}))
	t.Cleanup(srv.Close)
	p := NewWithConfig(Config{PayBase: srv.URL, APIBase: srv.URL, Client: &Client{APIBase: srv.URL, PayBase: srv.URL}})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{Provider: providerID, UID: uid, Secret: &Auth{Token: "t", Username: uid, UID: uid}}, true
	})
	return p
}

// stubInviteUpstreamFn 桩上游（按**请求体**分派的版本）。
//
// # 为什么需要第二个桩（T-357）
//
// 旧桩 stubInviteUpstream 是「一个路径 → 一个固定响应体」，够用于单账号用例。
// 但列表化之后必须能构造「**两个账号各自不同的响应**」——尤其要能造"一个 500、一个 200"
// 来钉住"单账号失败不影响其它"。fn 拿到原始请求体（含 username），自己决定回什么。
func stubInviteUpstreamFn(t *testing.T, fn func(path, body string, n int) (int, string)) *Provider {
	return stubInviteUpstreamFnFor(t, nil, fn)
}

// stubInviteUpstreamFnFor 同上，但可指定**已知 uid 白名单**：不在名单里的 uid 拿不到凭证。
//
// 为什么需要它：pickAuth(uid) 走的是 credSrc，而生产环境的 credSrc 对没并池的 uid 返回 false。
// 桩若"对任何 uid 都给凭证"，"uid 不存在 ⇒ 一条 ok=false"这条契约就测不出来（会假绿）。
func stubInviteUpstreamFnFor(t *testing.T, known []string, fn func(path, body string, n int) (int, string)) *Provider {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n++
		status, body := fn(r.URL.Path, string(b), n)
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewWithConfig(Config{PayBase: srv.URL, APIBase: srv.URL, Client: &Client{APIBase: srv.URL, PayBase: srv.URL}})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		if len(known) > 0 {
			found := false
			for _, k := range known {
				if k == uid {
					found = true
					break
				}
			}
			if !found {
				return gateway.Credential{}, false
			}
		}
		return gateway.Credential{Provider: providerID, UID: uid, Secret: &Auth{Token: "t", Username: uid, UID: uid}}, true
	})
	return p
}

// withTwoAccounts 给桩 Provider 写几份**真凭证文件**（走 SaveOne ⇒ 与线上同格式），并把 authDir 指过去。
//
// 为什么必须写真文件：列表型端点的账号来源是 loadAll() = LoadCredentialsWithSecrets(authDir)，
// 而注入的 credSrc 只能**按 uid 查单个**（枚举不出账号）。
func withTwoAccounts(t *testing.T, p *Provider, dir string, uids ...string) {
	t.Helper()
	for _, uid := range uids {
		if _, err := SaveOne(dir, &Auth{Token: "t-" + uid, Username: uid, UID: uid}); err != nil {
			t.Fatalf("SaveOne(%s): %v", uid, err)
		}
	}
	p.authDir = dir
}

// TestCheckInvitedParsesBool 真源（实测）：已被邀请 → {"code":200,"message":"用户已被邀请","data":true}
func TestCheckInvitedParsesBool(t *testing.T) {
	p := stubInviteUpstream(t, map[string]string{
		"/v1/user/check-invited": `{"code":200,"message":"用户已被邀请","success":true,"data":true}`,
	}, nil)
	invited, msg, err := p.CheckInvited(nil, "u1")
	if err != nil || !invited {
		t.Fatalf("want invited=true，实际 invited=%v msg=%q err=%v", invited, msg, err)
	}
	p2 := stubInviteUpstream(t, map[string]string{
		"/v1/user/check-invited": `{"code":200,"message":"用户未被邀请","success":true,"data":false}`,
	}, nil)
	invited2, _, err2 := p2.CheckInvited(nil, "u2")
	if err2 != nil || invited2 {
		t.Fatalf("want invited=false，实际 %v err=%v", invited2, err2)
	}
}

// TestBindInviteSendsFourCaptchaFields 真源（前端 JS 逐字）：六个字段名必须一字不差。
//
//	{username, invite_code, lot_number, captcha_output, pass_token, gen_time}
func TestBindInviteSendsFourCaptchaFields(t *testing.T) {
	seen := map[string]string{}
	p := stubInviteUpstream(t, map[string]string{
		"/v1/user/add-invite": `{"code":200,"message":"ok","success":true}`,
	}, &seen)
	captcha := map[string]string{
		"lot_number": "LN", "captcha_output": "CO", "pass_token": "PT", "gen_time": "GT",
	}
	if _, err := p.BindInvite(nil, "u3", "BB5VYS", captcha); err != nil {
		t.Fatalf("BindInvite 失败：%v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(seen["/v1/user/add-invite"]), &got); err != nil {
		t.Fatalf("请求体不是 JSON：%s", seen["/v1/user/add-invite"])
	}
	for k, want := range map[string]any{
		"username": "u3", "invite_code": "BB5VYS", "lot_number": "LN",
		"captcha_output": "CO", "pass_token": "PT", "gen_time": "GT",
	} {
		if got[k] != want {
			t.Errorf("字段 %s = %v，want %v（字段名来自前端 JS，错一个上游就报人机验证失败）", k, got[k], want)
		}
	}
}

// TestBindInviteRequiresCaptchaLocally captcha 四字段不全时**本地**就拒，不发请求。
//
// 依据（实测）：缺 captcha 时上游回 400「请完成人机验证，刷新页面后重试」——
// 与其把上游的模糊话甩给用户，不如本地说清缺哪个。
func TestBindInviteRequiresCaptchaLocally(t *testing.T) {
	seen := map[string]string{}
	p := stubInviteUpstream(t, map[string]string{
		"/v1/user/add-invite": `{"code":200}`,
	}, &seen)
	if _, err := p.BindInvite(nil, "u4", "BB5VYS", map[string]string{"lot_number": "LN"}); err == nil {
		t.Fatal("captcha 不全时应当本地报错")
	}
	if _, ok := seen["/v1/user/add-invite"]; ok {
		t.Error("captcha 不全时不该发出请求（应本地拦下）")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// T-357：/admin/codar/invite 由「单对象」改为「按账号的列表型」（与 loomy 同形）
//
// 为什么列表化（一句话）：面板要**一行一个账号**渲染，单对象形状表达不了多账号。
//
// 冻结契约（前端按它写，字段名一字不许改）：
//
//	顶层：{"ok","total","page","pages","accounts"}
//	每账号：{"uid","nickname","ok","my_code","my_link","invited","invited_message",
//	          "balance","balance_known","bind_page","bind_requires","invite_records"}
//	         失败时改成 {"uid","nickname","ok":false,"error"}
// ─────────────────────────────────────────────────────────────────────────────

// decodeInvite 解出列表型响应体（seam = handler 的 HTTP 响应形状）。
func decodeInvite(t *testing.T, p *Provider, target string) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	p.handleInvite(w, httptest.NewRequest(http.MethodGet, target, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%s 期望 HTTP 200，得到 %d：%s", target, w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON：%s", w.Body.String())
	}
	return got
}

// accountsOf 取出 accounts 数组（顺手做类型检查）。
func accountsOf(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	raw, ok := got["accounts"].([]any)
	if !ok {
		t.Fatalf("accounts 不是数组：%#v", got["accounts"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("accounts 元素不是对象：%#v", v)
		}
		out = append(out, m)
	}
	return out
}

// inviteStubFn 两个账号的通用桩：按请求体里的 username 返回该账号的码；failUID 指定的账号回 500。
func inviteStubFn(codes map[string]string, failUID string) func(string, string, int) (int, string) {
	return func(path, body string, _ int) (int, string) {
		uid := ""
		for k := range codes {
			if strings.Contains(body, `"`+k+`"`) {
				uid = k
				break
			}
		}
		if uid == "" {
			for k := range codes {
				uid = k
			}
		}
		if failUID != "" && uid == failUID {
			return http.StatusInternalServerError, `{"code":500,"message":"stub: boom"}`
		}
		switch path {
		case "/v1/promotion/info":
			return 0, `{"code":200,"data":{"invite_code":"` + codes[uid] + `","invite_link":"https://kuaipaper.com/?i=` + codes[uid] + `"}}`
		case "/v1/user/check-invited":
			return 0, `{"code":200,"message":"用户未被邀请","data":false}`
		case "/v1/agent/balance":
			return 0, `{"code":200,"data":{"agent_balance":4206}}`
		default:
			return 0, `{"code":200,"data":{}}`
		}
	}
}

// TestInviteRouteExposesInvited（T-357 改写）单账号：新契约的顶层 + 每账号字段 +
// **护栏**：raw 不得再出现（列表化会把它放大 25 倍）。
func TestInviteRouteExposesInvited(t *testing.T) {
	dir := t.TempDir()
	p := stubInviteUpstreamFn(t, inviteStubFn(map[string]string{"u5": "PB23JQ"}, ""))
	withTwoAccounts(t, p, dir, "u5")

	body := w2body(t, p, "/admin/codar/invite?uid=u5")
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应不是 JSON：%s", body)
	}

	// 顶层：只允许这 5 个键（raw/note/stats/upstream_status 等一律下放或删除）
	for _, k := range []string{"ok", "total", "page", "pages", "accounts"} {
		if _, ok := got[k]; !ok {
			t.Errorf("顶层缺少 %q：%#v", k, got)
		}
	}
	if n := len(got); n != 5 {
		t.Errorf("顶层键数应为 5（ok/total/page/pages/accounts），实际 %d：%#v", n, got)
	}
	if got["ok"] != true {
		t.Errorf("顶层 ok 应为 true，得到 %v", got["ok"])
	}
	accs := accountsOf(t, got)
	if len(accs) != 1 {
		t.Fatalf("uid 过滤应只回 1 条，得到 %d", len(accs))
	}
	a := accs[0]
	for k, want := range map[string]any{
		"uid": "u5", "ok": true, "my_code": "PB23JQ",
		"my_link":         "https://kuaipaper.com/?i=PB23JQ",
		"invited":         false,
		"invited_message": "用户未被邀请",
		"balance":         float64(4206),
		"balance_known":   true,
		"bind_page":       "/admin/codar/invite-page?uid=u5",
	} {
		if a[k] != want {
			t.Errorf("accounts[0].%s = %v，want %v", k, a[k], want)
		}
	}
	if _, ok := a["invite_records"]; !ok {
		t.Errorf("每账号必须带 invite_records 键：%#v", a)
	}
	if _, ok := a["bind_requires"]; !ok {
		t.Errorf("每账号必须带 bind_requires 键：%#v", a)
	}
	// 护栏：顶层不得再出现 raw（列表化会放大 25 倍）
	if strings.Contains(body, `"raw"`) {
		t.Error(`响应里出现了 "raw" —— 按契约必须删掉（列表化会把它放大 25 倍）`)
	}
}

// w2body 取一次响应体（字符串级断言用）。
func w2body(t *testing.T, p *Provider, target string) string {
	t.Helper()
	w := httptest.NewRecorder()
	p.handleInvite(w, httptest.NewRequest(http.MethodGet, target, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%s 期望 HTTP 200，得到 %d：%s", target, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestInviteListTwoAccounts 多账号：total=2、accounts 长度 2、各自 my_code 正确、顺序稳定。
func TestInviteListTwoAccounts(t *testing.T) {
	dir := t.TempDir()
	p := stubInviteUpstreamFn(t, inviteStubFn(map[string]string{"user_a": "AAA111", "user_b": "BBB222"}, ""))
	withTwoAccounts(t, p, dir, "user_a", "user_b")

	got := decodeInvite(t, p, "/admin/codar/invite")
	if got["total"] != float64(2) {
		t.Errorf("total = %v，want 2", got["total"])
	}
	accs := accountsOf(t, got)
	if len(accs) != 2 {
		t.Fatalf("accounts 长度 = %d，want 2", len(accs))
	}
	if accs[0]["uid"] != "user_a" || accs[0]["my_code"] != "AAA111" {
		t.Errorf("第 1 条 = %v/%v，want user_a/AAA111", accs[0]["uid"], accs[0]["my_code"])
	}
	if accs[1]["uid"] != "user_b" || accs[1]["my_code"] != "BBB222" {
		t.Errorf("第 2 条 = %v/%v，want user_b/BBB222", accs[1]["uid"], accs[1]["my_code"])
	}
}

// TestInvitePagingSecondPage page=2&page_size=1 ⇒ 第 2 个账号（total 仍是全量 2）。
func TestInvitePagingSecondPage(t *testing.T) {
	dir := t.TempDir()
	p := stubInviteUpstreamFn(t, inviteStubFn(map[string]string{"user_a": "AAA111", "user_b": "BBB222"}, ""))
	withTwoAccounts(t, p, dir, "user_a", "user_b")

	got := decodeInvite(t, p, "/admin/codar/invite?page=2&page_size=1")
	for k, want := range map[string]any{"page": float64(2), "pages": float64(2), "total": float64(2)} {
		if got[k] != want {
			t.Errorf("%s = %v，want %v", k, got[k], want)
		}
	}
	accs := accountsOf(t, got)
	if len(accs) != 1 {
		t.Fatalf("第 2 页应只回 1 条，得到 %d", len(accs))
	}
	if accs[0]["uid"] != "user_b" {
		t.Errorf("第 2 页第 1 条 = %v，want user_b", accs[0]["uid"])
	}
}

// TestInviteUIDFilter uid 不存在 ⇒ 该账号一条 ok=false（而不是空数组/500）。
func TestInviteUIDFilter(t *testing.T) {
	dir := t.TempDir()
	p := stubInviteUpstreamFnFor(t, []string{"user_a"}, inviteStubFn(map[string]string{"user_a": "AAA111"}, ""))
	withTwoAccounts(t, p, dir, "user_a")

	got := decodeInvite(t, p, "/admin/codar/invite?uid=ghost")
	accs := accountsOf(t, got)
	if len(accs) != 1 {
		t.Fatalf("未知 uid 应回 1 条，得到 %d", len(accs))
	}
	if accs[0]["uid"] != "ghost" {
		t.Errorf("uid = %v，want ghost", accs[0]["uid"])
	}
	if accs[0]["ok"] != false {
		t.Errorf("未知 uid 的 ok 应为 false，得到 %v", accs[0]["ok"])
	}
	if s, _ := accs[0]["error"].(string); s == "" {
		t.Errorf("未知 uid 应带 error 说明：%#v", accs[0])
	}
	if got["ok"] != true {
		t.Errorf("顶层 ok 仍应为 true（单账号失败不算整体失败），得到 %v", got["ok"])
	}
}

// TestInviteOneAccountFailureDoesNotBreakOthers 一个账号上游 500 ⇒ 只它 ok=false，其余照常。
func TestInviteOneAccountFailureDoesNotBreakOthers(t *testing.T) {
	dir := t.TempDir()
	p := stubInviteUpstreamFn(t, inviteStubFn(map[string]string{"user_a": "AAA111", "user_b": "BBB222"}, "user_a"))
	withTwoAccounts(t, p, dir, "user_a", "user_b")

	got := decodeInvite(t, p, "/admin/codar/invite")
	if got["ok"] != true {
		t.Errorf("顶层 ok 应仍为 true，得到 %v", got["ok"])
	}
	if got["total"] != float64(2) {
		t.Errorf("total = %v，want 2（失败的账号也算一条）", got["total"])
	}
	accs := accountsOf(t, got)
	if len(accs) != 2 {
		t.Fatalf("accounts 长度 = %d，want 2", len(accs))
	}
	if accs[0]["ok"] != false {
		t.Errorf("user_a 应 ok=false，得到 %v", accs[0]["ok"])
	}
	if s, _ := accs[0]["error"].(string); s == "" {
		t.Errorf("user_a 应带 error：%#v", accs[0])
	}
	if accs[1]["ok"] != true || accs[1]["my_code"] != "BBB222" {
		t.Errorf("user_b 应照常成功（ok/my_code = %v/%v）", accs[1]["ok"], accs[1]["my_code"])
	}
}

// TestInvitePagePrefillsCode 邀请页支持 ?code=<预填码> ⇒ HTML 里那个输入框带 value。
func TestInvitePagePrefillsCode(t *testing.T) {
	p := stubInviteUpstreamFn(t, inviteStubFn(map[string]string{"u9": "SELF111"}, ""))
	w := httptest.NewRecorder()
	p.handleInvitePage(w, httptest.NewRequest(http.MethodGet, "/admin/codar/invite-page?uid=u9&code=BB5VYS", nil))
	body := w.Body.String()
	if !strings.Contains(body, `id="invite-code"`) {
		t.Fatalf("页面里没有 invite-code 输入框：%s", truncateStr(body, 300))
	}
	if !strings.Contains(body, `value="BB5VYS"`) {
		t.Errorf("输入框未预填 ?code=BB5VYS：%s", truncateStr(body, 300))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// T-361：账号级**有界并发**（串行 125 次调用会撞 60s ctx 超时）
//
// 两个 seam（都在公共边界上观察，不测私有函数）：
//  ① accounts 的**顺序**必须等于 targets 顺序（分页顺序不能因"谁先回来"而乱）
//  ② 同时在上游的**请求数上限**
// ─────────────────────────────────────────────────────────────────────────────

// TestInviteAccountsKeepUpstreamOrder 后一个账号先返回，也不能排到前面。
//
// 做法：第一个账号的**每一次**上游响应先 sleep 30ms，第二个账号立即返回
// ⇒ 顺序若由"完成先后"决定，accounts[0] 就会变成第二个账号（红）。
func TestInviteAccountsKeepUpstreamOrder(t *testing.T) {
	dir := t.TempDir()
	uids := []string{"zz_one", "aa_two", "mm_three"}
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		return 0, `{"code":200,"data":{"invite_code":"C","agent_balance":1}}`
	})
	withTwoAccounts(t, p, dir, uids...)

	// ① 先拿到"基准顺序"（不注入任何延迟）
	base := accountsOf(t, decodeInvite(t, p, "/admin/codar/invite"))
	if len(base) != len(uids) {
		t.Fatalf("accounts 长度 = %d，want %d", len(base), len(uids))
	}
	want := make([]string, 0, len(base))
	for _, a := range base {
		want = append(want, a["uid"].(string))
	}

	// ② 把「基准顺序里的第一个账号」的每一次上游响应都拖慢 40ms，其余立即返回。
	//
	//	若顺序由「谁先返回」决定，第一个账号就会被挤到后面 ⇒ 用例红。
	//	（真源：顺序应只由 targets 决定，与响应时序无关 —— 所以断言是"两次顺序完全相同"。）
	slow := want[0]
	p2 := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		if strings.Contains(body, `"`+slow+`"`) {
			time.Sleep(40 * time.Millisecond)
		}
		return 0, `{"code":200,"data":{"invite_code":"C","agent_balance":1}}`
	})
	withTwoAccounts(t, p2, dir, uids...)

	got := accountsOf(t, decodeInvite(t, p2, "/admin/codar/invite"))
	if len(got) != len(want) {
		t.Fatalf("accounts 长度 = %d，want %d", len(got), len(want))
	}
	for i := range want {
		if got[i]["uid"] != want[i] {
			t.Errorf("第 %d 位 = %v，want %v —— 顺序被响应时序改变了（延迟了 %q）",
				i, got[i]["uid"], want[i], slow)
		}
	}
}

// TestInviteAccountConcurrencyIsBounded 20 个账号 ⇒ 同时在飞的请求数必须在 [2, inviteConcurrency]。
//
//	> 1 证明真的并发了（不是退化回串行 —— 串行时恒为 1，本用例在改并发之前**必红**）
//	≤ inviteConcurrency 证明有界（不能 20 个账号 100 条同时在飞）
func TestInviteAccountConcurrencyIsBounded(t *testing.T) {
	const n = 20
	uids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		uids = append(uids, "acct_"+string(rune('a'+i)))
	}
	var inFlight, maxInFlight int32
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		defer atomic.AddInt32(&inFlight, -1)
		time.Sleep(5 * time.Millisecond) // 拉开窗口，让「同时在飞」可被观测
		return 0, `{"code":200,"data":{"invite_code":"X","agent_balance":1}}`
	})
	withTwoAccounts(t, p, t.TempDir(), uids...)

	got := decodeInvite(t, p, "/admin/codar/invite?page_size=25")
	if got["total"] != float64(n) {
		t.Fatalf("total = %v，want %d", got["total"], n)
	}
	accs := accountsOf(t, got)
	if len(accs) != n {
		t.Fatalf("accounts 长度 = %d，want %d", len(accs), n)
	}
	peak := atomic.LoadInt32(&maxInFlight)
	t.Logf("实测同时在飞上限 = %d（inviteConcurrency = %d）", peak, inviteConcurrency)
	if peak <= 1 {
		t.Errorf("同时在飞上限 = %d ⇒ 仍是串行（应当并发）", peak)
	}
	if int(peak) > inviteConcurrency {
		t.Errorf("同时在飞上限 = %d > inviteConcurrency(%d) ⇒ 并发未受控", peak, inviteConcurrency)
	}
	// 顺序仍然稳定（并发下最容易坏的就是它）
	for i, uid := range uids {
		if accs[i]["uid"] != uid {
			t.Fatalf("第 %d 位顺序错：%v，want %v", i, accs[i]["uid"], uid)
		}
	}
}

// TestInviteOrderUnderPaging 分页切片的顺序也必须稳定：5 个账号、每页 3 ⇒ 第 1 页前 3、第 2 页后 2。
func TestInviteOrderUnderPaging(t *testing.T) {
	uids := []string{"p1", "p2", "p3", "p4", "p5"}
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		return 0, `{"code":200,"data":{"invite_code":"C","agent_balance":1}}`
	})
	withTwoAccounts(t, p, t.TempDir(), uids...)

	g1 := decodeInvite(t, p, "/admin/codar/invite?page=1&page_size=3")
	a1 := accountsOf(t, g1)
	if len(a1) != 3 {
		t.Fatalf("第 1 页长度 = %d，want 3", len(a1))
	}
	for i := 0; i < 3; i++ {
		if a1[i]["uid"] != uids[i] {
			t.Errorf("第 1 页第 %d 位 = %v，want %v", i, a1[i]["uid"], uids[i])
		}
	}
	g2 := decodeInvite(t, p, "/admin/codar/invite?page=2&page_size=3")
	a2 := accountsOf(t, g2)
	if len(a2) != 2 {
		t.Fatalf("第 2 页长度 = %d，want 2", len(a2))
	}
	for i := 0; i < 2; i++ {
		if a2[i]["uid"] != uids[3+i] {
			t.Errorf("第 2 页第 %d 位 = %v，want %v", i, a2[i]["uid"], uids[3+i])
		}
	}
	if g2["pages"] != float64(2) {
		t.Errorf("pages = %v，want 2", g2["pages"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// T-364 seam ④：inviteAccountView 暴露「微信是否绑定」
//
// 为什么邀请码列表要带它：邀请奖励是**两段式** —— ①被邀请人绑邀请码 ②被邀请人**再绑微信**，
// 之后邀请人的 2000 点数才真正入账。所以面板上必须一眼看到"微信：已绑定/未绑定"。
// 口径与 invited 一致：**拿不到只影响展示，不影响 ok**（否则一个可选字段会把整行标成失败）。
// ─────────────────────────────────────────────────────────────────────────────

// TestInviteAccountExposesWechatBound 取得到 ⇒ wechat_bound 与 known 都为真，wechat_page 恒填。
func TestInviteAccountExposesWechatBound(t *testing.T) {
	dir := t.TempDir()
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		switch path {
		case "/v1/promotion/info":
			return 0, `{"code":200,"data":{"invite_code":"BB5VYS"}}`
		case "/v1/user/check-invited":
			return 0, `{"code":200,"message":"用户未被邀请","data":false}`
		case "/v1/user/wechat-bound":
			return 0, `{"code":200,"message":"查询成功","success":true,"data":true}`
		case "/v1/agent/balance":
			return 0, `{"code":200,"data":{"agent_balance":7}}`
		default:
			return 0, `{"code":200,"data":{}}`
		}
	})
	withTwoAccounts(t, p, dir, "u1")

	a := accountsOf(t, decodeInvite(t, p, "/admin/codar/invite?uid=u1"))[0]
	if a["ok"] != true {
		t.Errorf("ok = %v，want true", a["ok"])
	}
	if a["wechat_bound"] != true {
		t.Errorf("wechat_bound = %v，want true", a["wechat_bound"])
	}
	if a["wechat_bound_known"] != true {
		t.Errorf("wechat_bound_known = %v，want true", a["wechat_bound_known"])
	}
	if a["wechat_page"] != "/admin/codar/wechat-page?uid=u1" {
		t.Errorf("wechat_page = %v", a["wechat_page"])
	}
}

// TestInviteWechatUnavailableDoesNotAffectOK 微信端点失败 ⇒ known=false，但 ok 仍 true、page 仍填。
func TestInviteWechatUnavailableDoesNotAffectOK(t *testing.T) {
	dir := t.TempDir()
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		switch path {
		case "/v1/promotion/info":
			return 0, `{"code":200,"data":{"invite_code":"BB5VYS"}}`
		case "/v1/user/check-invited":
			return 0, `{"code":200,"message":"用户未被邀请","data":false}`
		case "/v1/user/wechat-bound":
			// 业务失败（HTTP 仍 200、code != 200）—— 上游就是这个风格
			return 0, `{"code":500,"message":"服务异常","success":false,"data":null}`
		case "/v1/agent/balance":
			return 0, `{"code":200,"data":{"agent_balance":7}}`
		default:
			return 0, `{"code":200,"data":{}}`
		}
	})
	withTwoAccounts(t, p, dir, "u1")

	a := accountsOf(t, decodeInvite(t, p, "/admin/codar/invite?uid=u1"))[0]
	if a["ok"] != true {
		t.Errorf("ok = %v，want true（微信查询失败只影响展示，不能把整行标成失败）", a["ok"])
	}
	if a["wechat_bound_known"] != false {
		t.Errorf("wechat_bound_known = %v，want false", a["wechat_bound_known"])
	}
	if a["wechat_page"] != "/admin/codar/wechat-page?uid=u1" {
		t.Errorf("wechat_page 应恒填（失败也要能点进去自己看）：%v", a["wechat_page"])
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// T-366：把「每账号 6 次上游 × 25 账号 = 150 次 / 60s ctx」结构性修掉
//   （a）缺省 page_size 25 → 10（前端本来就有分页器；面板是"按行看"）
//   （b）单账号独立超时：一个账号卡住不能拖垮整页
// ─────────────────────────────────────────────────────────────────────────────

// TestInviteDefaultPageSizeIsTen 缺省页大小是 **10**（行为变更：原为 25）。
//
// 为什么改：每账号要打 6 次上游（invite 主流程 5 次 + wechat-bound 1 次），
// 25 账号 × 6 = 150 次；即使 6 并发也要 25 轮，60s 兜底很容易撞。
// 10 账号 ⇒ 60 次 ⇒ 10 轮，留出余量。上限仍是 100。
func TestInviteDefaultPageSizeIsTen(t *testing.T) {
	uids := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		uids = append(uids, "d"+string(rune('a'+i))) // 12 个账号 > 缺省页 10
	}
	p := stubInviteUpstreamFn(t, inviteStubFn(map[string]string{}, ""))
	withTwoAccounts(t, p, t.TempDir(), uids...)

	got := decodeInvite(t, p, "/admin/codar/invite") // 不带 page_size
	if got["total"] != float64(12) {
		t.Fatalf("total = %v，want 12", got["total"])
	}
	if n := len(accountsOf(t, got)); n != 10 {
		t.Errorf("缺省页大小 = %d 条，want 10（原 25；见 T-366 的成本算术）", n)
	}
	if got["pages"] != float64(2) {
		t.Errorf("pages = %v，want 2（12 条按每页 10 分）", got["pages"])
	}
	got2 := decodeInvite(t, p, "/admin/codar/invite?page_size=1000")
	if n := len(accountsOf(t, got2)); n != 12 {
		t.Errorf("page_size 上限 100 时 12 个账号应全回，得到 %d", n)
	}
}

// TestInviteAccountTimeoutIsIsolated 一个账号的上游挂起 ⇒ 只它超时失败，其余照常，且**不等它**。
//
// 做法：把包级 inviteAccountTimeout 临时压到 50ms（生产值 12s），桩对某个账号 sleep 400ms。
// 断言：①超时那条 ok=false 且 error 点出"超时" ②另一条 ok=true ③整体耗时远小于 400ms。
func TestInviteAccountTimeoutIsIsolated(t *testing.T) {
	old := inviteAccountTimeout
	inviteAccountTimeout = 50 * time.Millisecond
	t.Cleanup(func() { inviteAccountTimeout = old })

	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		if strings.Contains(body, `"slow"`) {
			time.Sleep(400 * time.Millisecond) // 远大于 50ms 的单账号超时
			return 0, `{"code":200,"data":{"invite_code":"LATE"}}`
		}
		switch path {
		case "/v1/promotion/info":
			return 0, `{"code":200,"data":{"invite_code":"FAST1"}}`
		case "/v1/user/check-invited":
			return 0, `{"code":200,"message":"用户未被邀请","data":false}`
		default:
			return 0, `{"code":200,"data":{}}`
		}
	})
	withTwoAccounts(t, p, t.TempDir(), "fast", "slow")

	start := time.Now()
	got := decodeInvite(t, p, "/admin/codar/invite?page_size=10")
	elapsed := time.Since(start)

	if got["ok"] != true || got["total"] != float64(2) {
		t.Fatalf("顶层应当照常成功：ok=%v total=%v", got["ok"], got["total"])
	}
	byUID := map[string]map[string]any{}
	for _, a := range accountsOf(t, got) {
		byUID[a["uid"].(string)] = a
	}
	if byUID["fast"]["ok"] != true {
		t.Errorf("fast 应 ok=true，得到 %v（error=%v）", byUID["fast"]["ok"], byUID["fast"]["error"])
	}
	slow := byUID["slow"]
	if slow["ok"] != false {
		t.Errorf("slow 应 ok=false（单账号超时），得到 %v", slow["ok"])
	}
	if s, _ := slow["error"].(string); !strings.Contains(s, "超时") {
		t.Errorf("slow 的 error 应点出「超时」，得到 %q", s)
	}
	if elapsed >= 400*time.Millisecond {
		t.Errorf("整体耗时 %v 已接近挂起的 400ms ⇒ 没有真正隔离（应当在 50ms 就放弃该账号）", elapsed)
	}
	t.Logf("整体耗时 = %v（挂起 400ms / 单账号超时 50ms）", elapsed)
}

// TestInviteAccountTimeoutTruncatesEveryStep 容错语义（T-368 起）：**非致命步骤失败不毁整行**。
//
// 契约：ok 只表示"拿到了 my_code"（唯一必要条件）；其余步骤各自用 *_known 标记可信度，
// 并用 warning 给一句话摘要。**只有 promotion/info 失败才是整行失败**。
//
// 表驱动，期望值全部手写：挂起 400ms、预算 50ms ⇒ 该步被截断，但整行仍应给出邀请码。
func TestInviteAccountTimeoutTruncatesEveryStep(t *testing.T) {
	steps := []struct {
		name     string
		hangPath string
		fatal    bool   // true ⇒ 唯一该整行失败的步骤（拿不到 my_code）
		knownKey string // 该步失败后应为 false 的 *known 标志（fatal 时忽略）
		warnWord string // warning 里应出现的关键词
	}{
		{"微信 wechat-bound", "/v1/user/wechat-bound", false, "wechat_bound_known", "微信"},
		{"邀请状态 check-invited", "/v1/user/check-invited", false, "invited_known", "邀请状态"},
		{"邀请记录 promotion/invites", "/v1/promotion/invites", false, "invite_records", "邀请记录"},
		{"余额 agent/balance", "/v1/agent/balance", false, "balance_known", "余额"},
		// ⚠ gettoken **单项**失败时上游语义是"部分数据"（agent 桶仍有效）⇒ refreshQuotaCtx
		// 返回 known=true（既有 TestQuotaPartialWhenGetTokenFails 就是这个口径，不许改）。
		// 所以这一档只能断言"被预算截断"（耗时），knownKey/warnWord 留空表示不断言。
		{"通用余额 gettoken", "/v1/gettoken", false, "", ""},
		{"我的邀请码 promotion/info", "/v1/promotion/info", true, "", "超时"},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			old := inviteAccountTimeout
			inviteAccountTimeout = 50 * time.Millisecond
			t.Cleanup(func() { inviteAccountTimeout = old })

			p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
				// 只让 slow 这一个账号在该步挂住；fast 全程秒回
				if path == st.hangPath && strings.Contains(body, `"slow"`) {
					time.Sleep(400 * time.Millisecond)
				}
				switch path {
				case "/v1/promotion/info":
					return 0, `{"code":200,"data":{"invite_code":"OK1"}}`
				case "/v1/agent/balance":
					return 0, `{"code":200,"data":{"agent_balance":1}}`
				case "/v1/user/check-invited":
					return 0, `{"code":200,"message":"用户未被邀请","data":false}`
				case "/v1/user/wechat-bound":
					return 0, `{"code":200,"message":"查询成功","data":true}`
				case "/v1/promotion/invites":
					return 0, `{"code":200,"data":[{"username":"invitee"}]}`
				default:
					return 0, `{"code":200,"data":{}}`
				}
			})
			withTwoAccounts(t, p, t.TempDir(), "fast", "slow")

			start := time.Now()
			got := decodeInvite(t, p, "/admin/codar/invite?page_size=10")
			elapsed := time.Since(start)

			if got["ok"] != true {
				t.Fatalf("顶层 ok 应仍为 true，得到 %v", got["ok"])
			}
			byUID := map[string]map[string]any{}
			for _, a := range accountsOf(t, got) {
				byUID[a["uid"].(string)] = a
			}
			// 另一行必须**完全正常**（不被邻居拖累）
			fast := byUID["fast"]
			for k, want := range map[string]any{
				"ok": true, "my_code": "OK1", "invited_known": true,
				"balance_known": true, "wechat_bound": true,
			} {
				if fast[k] != want {
					t.Errorf("fast.%s = %v，want %v", k, fast[k], want)
				}
			}
			slow := byUID["slow"]
			if st.fatal {
				// 唯一该整行失败的步骤：拿不到邀请码 ⇒ 没有可显示的产物
				if slow["ok"] != false {
					t.Errorf("promotion/info 挂起时 ok 应为 false，得到 %v", slow["ok"])
				}
				if s, _ := slow["error"].(string); !strings.Contains(s, "超时") {
					t.Errorf("error 应含「超时」，得到 %q", s)
				}
				return
			}
			// 非致命步骤：整行仍成功，且必须给出邀请码
			if slow["ok"] != true {
				t.Fatalf("非致命步骤挂起时 ok 应为 true（邀请码已拿到），得到 %v（error=%v）", slow["ok"], slow["error"])
			}
			if slow["my_code"] != "OK1" {
				t.Errorf("my_code = %v，want OK1（这是本表唯一的必要条件，不能被连带作废）", slow["my_code"])
			}
			if st.knownKey == "" {
				// 本档不断言 known/warning（见该行的注释）
			} else if st.knownKey == "invite_records" {
				// 记录是**数组**：失败时保留空数组（不是 null，前端会 .map()）
				recs, _ := slow["invite_records"].([]any)
				if len(recs) != 0 {
					t.Errorf("invite_records 应为空数组，得到 %v", slow["invite_records"])
				}
			} else if slow[st.knownKey] == true {
				t.Errorf("%s 应为 false（该步没查到），得到 %v", st.knownKey, slow[st.knownKey])
			}
			if st.knownKey == "" {
				return
			}
			w, _ := slow["warning"].(string)
			if w == "" {
				t.Errorf("应有 warning 说明哪一步没取到")
			} else if !strings.Contains(w, st.warnWord) {
				t.Errorf("warning = %q，应含关键词 %q", w, st.warnWord)
			}
			if elapsed >= 200*time.Millisecond {
				t.Errorf("整体耗时 %v ≥ 200ms（挂起 400ms）⇒ 这一步没被单账号预算截断", elapsed)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// T-371：「余额只取到部分桶」必须能透出（balance_partial）
//
// 为什么需要：实测老号 4206 = agent 1706 + general 2500 + rewrite 0。
// gettoken 挂掉时只有 1706 可报 —— balance_known 仍是 true（agent 桶确实可信）✓，
// 但界面若不区分"三桶齐全"与"只有 agent 桶"，用户会以为余额一夜之间少了两千五。
//
// 三档（期望值手写）：
//
//	gettoken 500 ⇒ balance=1706(agent), known=true,  partial=true,  warning 含「部分桶」
//	三桶齐全     ⇒ balance=4206(total), known=true,  partial=false, warning 空
//	agent 500    ⇒ balance=0,           known=false, partial=false, warning 含「余额未取到」
//	                （没拿到锚数据就谈不上"部分"—— 部分的前提是"有一部分"）
//
// ─────────────────────────────────────────────────────────────────────────────
func TestInviteBalancePartial(t *testing.T) {
	cases := []struct {
		name        string
		gettokenOK  bool
		agentOK     bool
		wantBalance float64
		wantKnown   bool
		wantPartial bool
		warnWord    string
	}{
		{"gettoken 500 ⇒ 只有 agent 桶", false, true, 1706, true, true, "部分桶"},
		{"三桶齐全 ⇒ 4206", true, true, 4206, true, false, ""},
		{"agent 500 ⇒ 没数据", true, false, 0, false, false, "余额未取到"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
				switch path {
				case "/v1/promotion/info":
					return 0, `{"code":200,"data":{"invite_code":"OK1"}}`
				case "/v1/user/check-invited":
					return 0, `{"code":200,"message":"用户未被邀请","data":false}`
				case "/v1/promotion/invites":
					return 0, `{"code":200,"data":[]}`
				case "/v1/user/wechat-bound":
					return 0, `{"code":200,"message":"查询成功","data":false}`
				case "/v1/agent/balance":
					if !c.agentOK {
						return http.StatusInternalServerError, `{"code":500,"message":"boom"}`
					}
					return 0, `{"code":200,"data":{"agent_balance":1706}}`
				case "/v1/gettoken":
					// ⚠ fetchGeneralBuckets 解析的是**顶层** token / rewrite_token（不是 data 里）
					if !c.gettokenOK {
						return http.StatusInternalServerError, `{"code":500,"message":"boom"}`
					}
					return 0, `{"code":200,"token":2500,"rewrite_token":0}`
				default:
					return 0, `{"code":200,"data":{}}`
				}
			})
			withTwoAccounts(t, p, t.TempDir(), "u1")

			a := accountsOf(t, decodeInvite(t, p, "/admin/codar/invite?uid=u1"))[0]
			if a["ok"] != true || a["my_code"] != "OK1" {
				t.Fatalf("整行应当成功且带码：ok=%v my_code=%v", a["ok"], a["my_code"])
			}
			if a["balance"] != c.wantBalance {
				t.Errorf("balance = %v，want %v", a["balance"], c.wantBalance)
			}
			if a["balance_known"] != c.wantKnown {
				t.Errorf("balance_known = %v，want %v", a["balance_known"], c.wantKnown)
			}
			gotPartial := a["balance_partial"] == true
			if gotPartial != c.wantPartial {
				t.Errorf("balance_partial = %v，want %v", a["balance_partial"], c.wantPartial)
			}
			w, _ := a["warning"].(string)
			if c.warnWord == "" {
				if w != "" {
					t.Errorf("warning 应为空，得到 %q", w)
				}
			} else if !strings.Contains(w, c.warnWord) {
				t.Errorf("warning = %q，应含 %q", w, c.warnWord)
			}
		})
	}
}
