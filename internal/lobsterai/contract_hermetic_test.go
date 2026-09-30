// contract_hermetic_test.go 契约测试与假上游 —— **判据 2 的 enforcement**。
//
// 全程 hermetic：假上游同时扮演业务 API、portal 与版本接口，
// 不需要真实账号即可跑通整条链路（含登录回调）。
package lobsterai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fixtureJWT 造带指定 exp 的假 JWT。
func fixtureJWT(expUnix int64) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expUnix)))
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

// fakeUpstream 假 LobsterAI 上游。
func fakeUpstream(t *testing.T, slotState string, claimedToday bool) (*httptest.Server, *fakeCalls) {
	t.Helper()
	calls := &fakeCalls{}
	exp := time.Now().Add(3 * time.Hour).Unix()
	token := fixtureJWT(exp)

	mux := http.NewServeMux()

	// ── 版本接口（code/msg 在**外层**）────────────────────────────────
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.version, 1)
		_, _ = w.Write([]byte(`{"data":{"value":{"version":"2026.9.4","date":"2026-9-4"}},"code":0,"msg":"OK"}`))
	})

	// ── exchange ───────────────────────────────────────────────────────
	mux.HandleFunc(exchangePath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.exchange, 1)
		// ⚠ 不带 Authorization
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"exchange must not carry Authorization"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 五个必需字段
		for _, k := range []string{"authCode", "firstKeyfrom", "latestKeyfrom", "uuid", "version"} {
			if _, ok := body[k]; !ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":1,"msg":"missing ` + k + `"}`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{
			"accessToken":"` + token + `","refreshToken":"rt-1","expiresIn":10800,
			"user":{"id":"u-1","yid":"y-1","userId":"acct-1","nickname":"虾米"}}}`))
	})

	// ── 续期 ───────────────────────────────────────────────────────────
	mux.HandleFunc(refreshPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.refresh, 1)
		// ⚠ 不带 Authorization
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"refresh must not carry Authorization"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rt, _ := body["refreshToken"].(string)
		if rt == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"refreshToken required"}`))
			return
		}
		if rt == "rt-expired" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"msg":"登录态已过期"}`))
			return
		}
		// ⚠ keyfrom 必须用凭据里存储的原值（不是当前时刻）
		if body["firstKeyfrom"] != "1700000000000" || body["latestKeyfrom"] != "1700000000000" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"keyfrom must be the stored values"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"` + fixtureJWT(time.Now().Add(6*time.Hour).Unix()) + `","expiresIn":10800}}`))
	})

	// ── 模型列表（data 是**数组**）────────────────────────────────────
	mux.HandleFunc(modelsPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.models, 1)
		// ⚠ 两个能力头是**准入条件**：不带它们不会返回 kimi-k3
		if r.Header.Get("X-LobsterAI-Client-Capabilities") != clientCapabilities {
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":[{"modelId":"deepseek-v4-flash"}]}`))
			return
		}
		if r.Header.Get("X-LobsterAI-Client-Version") == "" {
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":[{"modelId":"deepseek-v4-flash"}]}`))
			return
		}
		// query 不该含 refreshToken
		if strings.Contains(r.URL.RawQuery, "refreshToken") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"models query must not contain refreshToken"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":[
			{"modelId":"deepseek-v4-flash","modelName":"DeepSeek V4 Flash","provider":"deepseek","apiFormat":"openai"},
			{"modelId":"kimi-k3","modelName":"Kimi K3","provider":"moonshot","apiFormat":"openai"}]}`))
	})

	// ── 聊天（**裸 SSE，不套信封**）──────────────────────────────────
	mux.HandleFunc(chatPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.chat, 1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-LobsterAI-Client-Capabilities") != clientCapabilities {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"chat requires capabilities header"}`))
			return
		}
		raw := make([]byte, 4096)
		n, _ := r.Body.Read(raw)
		var obj map[string]any
		_ = json.Unmarshal(raw[:n], &obj)
		if obj["stream"] != true {
			// ⚠ stream:false → 上游回 500（实测行为）
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// 裸 SSE：直接是 data: {...}，不套 {code,msg,data}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"成功\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	// ── 签到三步 ───────────────────────────────────────────────────────
	mux.HandleFunc(activitySlotPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.slot, 1)
		q := r.URL.Query()
		// 槽位参数是固定值
		if q.Get("placement") != slotPlacement || q.Get("containerApiVersion") != slotContainerAPIVer ||
			q.Get("platform") != slotPlatform || q.Get("clientVersion") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"bad slot query"}`))
			return
		}
		if slotState != "available" {
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"slotState":"` + slotState + `","activity":{}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"slotState":"available","activity":{"activityCode":"act-1","configRevision":7}}}`))
	})
	mux.HandleFunc(activityContextPath+"/act-1/context", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.context, 1)
		if r.URL.Query().Get("configRevision") != "7" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"bad configRevision"}`))
			return
		}
		if claimedToday {
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"state":{"claimedToday":true},"actions":[]}}`))
			return
		}
		// ⚠ actions 是**字符串数组**，不是对象数组。
		//
		// 这里此前写的是 `[{"actionName":"check_in"}]` —— 那是**错的形状**，
		// 而假上游用错形状返回，就等于把真实缺陷**验证成了正确行为**：
		// 生产环境上游返回 `["check_in"]`，解析必然失败，而契约测试全绿。
		// 2026-09-30 用户报「签到失败」就是这个（见 client.go 的注释）。
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"state":{"claimedToday":false},"actions":["check_in"]}}`))
	})
	mux.HandleFunc(activityContextPath+"/act-1/actions/check_in", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.checkin, 1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["idempotencyKey"] == nil || body["configRevision"] == nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1,"msg":"missing idempotencyKey/configRevision"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"result":{"creditsGranted":100}}}`))
	})

	// ── 余额 ───────────────────────────────────────────────────────────
	mux.HandleFunc(profileSummaryPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls.balance, 1)
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"totalCreditsRemaining":5297.72,
			"creditItems":[{"type":"activity","creditsRemaining":4997.72,"expiresAt":"2030-01-01"}]}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, calls
}

type fakeCalls struct {
	version, exchange, refresh, models, chat int32
	slot, context, checkin, balance          int32
}

func newContractProvider(srv *httptest.Server) gateway.Provider {
	c := NewWithBase(srv.URL)
	return NewWithConfig(Config{Client: c})
}

func newContractCredential(uid string) gateway.Credential {
	a := &Auth{
		AccessToken:  fixtureJWT(time.Now().Add(3 * time.Hour).Unix()),
		RefreshToken: "rt-1",
		UID:          uid,
		UserID:       "acct-1",
		Nickname:     "虾米",
		UUID:         "00000000-0000-4000-8000-000000000001",
		// ⚠ keyfrom 用固定值，便于续期用例断言"用的是存储原值"
		FirstKeyfrom:  "1700000000000",
		LatestKeyfrom: "1700000000000",
	}
	return gateway.Credential{Provider: providerID, UID: uid, Nickname: a.Nickname, Secret: a}
}

// TestContract 契约测试 —— **判据 2 的正式入口**。
func TestContract(t *testing.T) {
	srv, _ := fakeUpstream(t, "available", false)
	gateway.RunProviderContract(t, func() gateway.Provider {
		return newContractProvider(srv)
	}, gateway.WithCredential(newContractCredential("u-1")))
}

// TestContractIsNotConditionallySkipped 契约必须无凭证也跑。
func TestContractIsNotConditionallySkipped(t *testing.T) {
	t.Setenv("LOBSTERAI_ACCESS_TOKEN", "")
	t.Setenv("LOBSTERAI_TEST_CRED", "")

	srv, _ := fakeUpstream(t, "available", false)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gateway.RunProviderContract(t, func() gateway.Provider {
			return newContractProvider(srv)
		}, gateway.WithCredential(newContractCredential("u-1")))
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("契约在无凭证环境下未完成 —— CI 里会变成永远挂住")
	}
}

// ── 逐条钉住本 provider 特有的硬判据 ────────────────────────────────────

// TestCapabilityHeadersAreRequiredForModels 两个能力头是模型列表的准入条件。
//
// ⚠ 没有它们时服务端只返回 25 个模型、**没有 kimi-k3**。
// 本用例是"永久缺少 kimi-k3"那个真实缺陷的回归网。
func TestCapabilityHeadersAreRequiredForModels(t *testing.T) {
	srv, _ := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	ms, err := p.Models(context.Background(), newContractCredential("u-1"))
	if err != nil {
		t.Fatalf("Models 报错: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range ms {
		ids[m.ID] = true
	}
	if !ids["kimi-k3"] {
		t.Errorf("必须带 X-LobsterAI-Client-Capabilities 才能拿到 kimi-k3，实际: %v", ids)
	}
}

// TestModelsQueryHasNoRefreshToken query 不该含 refreshToken。
func TestModelsQueryHasNoRefreshToken(t *testing.T) {
	srv, calls := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	if _, err := p.Models(context.Background(), newContractCredential("u-1")); err != nil {
		t.Fatalf("Models 报错: %v", err)
	}
	if atomic.LoadInt32(&calls.models) == 0 {
		t.Fatal("应调用模型列表端点")
	}
}

// TestRefreshUsesStoredKeyfromValues 续期用**存储的** keyfrom 原值。
//
// ⚠ 不是当前时刻。假上游会拒绝非存储值，所以这条测得出。
func TestRefreshUsesStoredKeyfromValues(t *testing.T) {
	srv, calls := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	cred := newContractCredential("u-1")
	a := cred.Secret.(*Auth)

	if err := p.RefreshCredential(cred); err != nil {
		t.Fatalf("续期失败（keyfrom 必须用存储原值）: %v", err)
	}
	if atomic.LoadInt32(&calls.refresh) != 1 {
		t.Fatal("应发 1 次续期请求")
	}
	// uuid / keyfrom 必须保留
	if a.UUID == "" || a.FirstKeyfrom == "" || a.LatestKeyfrom == "" {
		t.Errorf("续期后必须保留 uuid/keyfrom（续期请求体需要它们）: %+v", a)
	}
}

// TestRefreshExpiredIsSentinel HTTP 401 是续期终态。
func TestRefreshExpiredIsSentinel(t *testing.T) {
	srv, _ := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	cred := gateway.Credential{Provider: providerID, UID: "u",
		Secret: &Auth{AccessToken: "a", RefreshToken: "rt-expired",
			FirstKeyfrom: "1700000000000", LatestKeyfrom: "1700000000000"}}
	err := p.RefreshCredential(cred)
	if err == nil || !strings.Contains(err.Error(), ErrRefreshExpired.Error()) {
		t.Errorf("HTTP 401 应返回终态错误，实际 %v", err)
	}

	// 无 refresh_token → 终态
	cred2 := gateway.Credential{Provider: providerID, UID: "u", Secret: &Auth{AccessToken: "a"}}
	if err := p.RefreshCredential(cred2); err == nil || !strings.Contains(err.Error(), ErrRefreshExpired.Error()) {
		t.Errorf("无 refresh_token 应返回终态错误，实际 %v", err)
	}
}

// TestEnvelopeNonZeroCodeIsFailure 非 0 code 即失败（即便 HTTP 200）。
func TestEnvelopeNonZeroCodeIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 + 非 0 code
		_, _ = w.Write([]byte(`{"code":40001,"msg":"accessToken 无效"}`))
	}))
	defer srv.Close()
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	if _, err := p.Models(context.Background(), newContractCredential("u-1")); err == nil {
		t.Error("HTTP 200 + 非 0 code 必须判失败")
	}
}

// TestEnvelopeDataMustBeObject data 非对象（数组）在实体端点视为失败。
func TestEnvelopeDataMustBeObject(t *testing.T) {
	// 模型列表的 data 是数组，parseEnvelope 应拒绝（它要求对象）
	if _, err := parseEnvelope([]byte(`{"code":0,"msg":"","data":[]}`)); err == nil {
		t.Error("data 是数组时 parseEnvelope 应失败（模型列表走独立的解析路径）")
	}
	// data 缺失
	if _, err := parseEnvelope([]byte(`{"code":0,"msg":""}`)); err == nil {
		t.Error("data 缺失应失败")
	}
	// 正常对象
	if _, err := parseEnvelope([]byte(`{"code":0,"msg":"","data":{"a":1}}`)); err != nil {
		t.Errorf("正常信封应通过: %v", err)
	}
}

// TestChatIsBareSSE 聊天返回**裸 SSE**（不套信封）。
//
// ⚠ 模型列表套信封、聊天不套 —— 这是最容易搞错的一点。
func TestChatIsBareSSE(t *testing.T) {
	srv, calls := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	cred := newContractCredential("u-1")

	cs, err := p.Chat(context.Background(), cred, []byte(`{"model":"kimi-k3"}`))
	if err != nil {
		t.Fatalf("Chat 报错: %v", err)
	}
	defer func() { _ = cs.Body.Close() }()
	if cs.Status != 200 {
		t.Fatalf("status=%d", cs.Status)
	}
	raw := make([]byte, 512)
	n, _ := cs.Body.Read(raw)
	body := string(raw[:n])
	// 裸 SSE：直接以 data: 开头，**没有**外层 code/msg 信封
	if !strings.HasPrefix(strings.TrimSpace(body), "data: ") {
		t.Errorf("聊天响应应是裸 SSE（以 data: 开头），实际 %q", body)
	}
	if strings.Contains(body, `"code":`) {
		t.Errorf("聊天响应不该套信封，实际 %q", body)
	}
	// stream 必须被强制
	if atomic.LoadInt32(&calls.chat) != 1 {
		t.Error("应发 1 次聊天请求")
	}
}

// TestCheckinThreeSteps 签到三步全成功。
func TestCheckinThreeSteps(t *testing.T) {
	srv, calls := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	out := p.ClaimDailyCheckin(context.Background(), newContractCredential("u-1"))
	if out.Kind != "claimed" {
		t.Fatalf("应签到成功，实际 kind=%q msg=%q", out.Kind, out.Message)
	}
	if out.Credit != 100 {
		t.Errorf("积分 = %v, want 100", out.Credit)
	}
	if atomic.LoadInt32(&calls.slot) != 1 || atomic.LoadInt32(&calls.context) != 1 ||
		atomic.LoadInt32(&calls.checkin) != 1 {
		t.Errorf("必须走完三步：slot=%d context=%d checkin=%d",
			calls.slot, calls.context, calls.checkin)
	}
}

// TestCheckinAlreadyClaimed 今天已签 → already-claimed（不是 failed）。
func TestCheckinAlreadyClaimed(t *testing.T) {
	srv, calls := fakeUpstream(t, "available", true)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	out := p.ClaimDailyCheckin(context.Background(), newContractCredential("u-1"))
	if out.Kind != "already-claimed" {
		t.Errorf("今天已签应返回 already-claimed，实际 %q", out.Kind)
	}
	if atomic.LoadInt32(&calls.checkin) != 0 {
		t.Error("已签到时不该再发领取请求")
	}
}

// TestCheckinInactiveWhenNoActivity 无可用活动 → inactive（不是 failed）。
func TestCheckinInactiveWhenNoActivity(t *testing.T) {
	srv, calls := fakeUpstream(t, "unavailable", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	out := p.ClaimDailyCheckin(context.Background(), newContractCredential("u-1"))
	if out.Kind != "inactive" {
		t.Errorf("槽位不可用时应返回 inactive，实际 %q（msg=%q）", out.Kind, out.Message)
	}
	if atomic.LoadInt32(&calls.context) != 0 {
		t.Error("槽位不可用时不该继续查上下文")
	}
}

// TestBalanceUsesProfileSummary 余额走 profile-summary（含活动积分）。
func TestBalanceUsesProfileSummary(t *testing.T) {
	srv, calls := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	b, err := p.Balance(context.Background(), newContractCredential("u-1"))
	if err != nil {
		t.Fatalf("余额查询失败: %v", err)
	}
	if b.Total != 5297.72 {
		t.Errorf("总额 = %v, want 5297.72", b.Total)
	}
	if len(b.Items) != 1 || b.Items[0].Type != "activity" {
		t.Errorf("积分项解析不对: %+v", b.Items)
	}
	if atomic.LoadInt32(&calls.balance) == 0 {
		t.Error("应调 profile-summary")
	}
}

// TestVersionFallsBackOnFailure 版本号接口失败时必须兜底。
//
// ⚠ 版本接口在第三方域名上，实测存在网络层差异 —— 兜底是必需的。
func TestVersionFallsBackOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	if got := c.ClientVersion(context.Background()); got != DefaultClientVersion {
		t.Errorf("版本接口失败应兜底 %q，实际 %q", DefaultClientVersion, got)
	}
}

// TestParseClientVersionValidates 版本号必须校验（非法不得拼进 URL）。
func TestParseClientVersionValidates(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2026.9.4", "2026.9.4"},
		{"2026.9.4-beta.1", "2026.9.4"},
		{"  2026.9.4  ", "2026.9.4"},
		{"", ""},
		{"v2026.9.4", ""},           // 非点分数字开头
		{"<html>error</html>", ""},  // HTML 错误页
		{"null", ""},
	}
	for _, c := range cases {
		if got := ParseClientVersion(c.in); got != c.want {
			t.Errorf("ParseClientVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseVersionFromUpdateShape 版本接口的载荷在 data.value 里。
func TestParseVersionFromUpdateShape(t *testing.T) {
	body := []byte(`{"data":{"value":{"version":"2026.9.4","date":"2026-9-4"}},"code":0,"msg":"OK"}`)
	if got := ParseClientVersionFromUpdate(body); got != "2026.9.4" {
		t.Errorf("版本 = %q, want 2026.9.4", got)
	}
	// 结构不符 → 空串（不 panic）
	if got := ParseClientVersionFromUpdate([]byte(`{"code":0}`)); got != "" {
		t.Errorf("结构不符应返回空串，实际 %q", got)
	}
}

// TestLoginEndToEnd 登录端到端（起回调服务器 + exchange）。
func TestLoginEndToEnd(t *testing.T) {
	srv, calls := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})

	lf, ok := p.LoginFlow()
	if !ok {
		t.Fatal("Provider 必须实现 LoginFlow")
	}
	state, loginURL, err := lf.Start()
	if err != nil {
		t.Fatalf("Start 报错: %v", err)
	}
	if !strings.Contains(loginURL, "/portal#/login?") {
		t.Fatalf("登录 URL 形态不对: %s", loginURL)
	}
	// ⚠ 三个 query 参数在 URL 的 **fragment** 里（`/portal#/login?source=...`），
	// 不是普通 query —— url.Parse 只认 `?` 之前的部分，fragment 要单独切。
	frag := loginURL
	if i := strings.Index(loginURL, "#"); i >= 0 {
		frag = loginURL[i+1:]
	}
	if !strings.HasPrefix(frag, "/login?") {
		t.Fatalf("fragment 形态不对: %s", frag)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(frag, "/login?"))
	if err != nil {
		t.Fatalf("fragment query 解析失败: %v", err)
	}
	if q.Get("source") != "electron" {
		t.Errorf("必须带 source=electron: %s", loginURL)
	}
	redirect := q.Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") ||
		!strings.HasSuffix(redirect, callbackPath) {
		t.Fatalf("redirect_uri 形态不对: %s", redirect)
	}
	if q.Get("state") != state {
		t.Errorf("state 不匹配: got %q want %q", q.Get("state"), state)
	}

	// 扮演浏览器打回调
	resp, err := http.Get(redirect + "?code=test-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	_ = resp.Body.Close()

	// 轮询取凭据
	var cred gateway.Credential
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, perr := lf.Poll(state)
		if perr == nil {
			cred = c
			break
		}
		if perr != gateway.ErrLoginPending {
			t.Fatalf("轮询报错: %v", perr)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if cred.UID == "" {
		t.Fatal("20 秒内未拿到凭证")
	}
	if cred.UID != "u-1" {
		t.Errorf("uid = %q, want u-1", cred.UID)
	}
	if atomic.LoadInt32(&calls.exchange) != 1 {
		t.Error("应发 1 次 exchange")
	}
	// ⚠ Secret 是 authFile 包装（核心落盘要求它实现 MarshalAuthFile）。
	// 要断言的字段在对内的 *Auth 上，所以这里解一层。
	// 包装存在的理由见 login.go 的 authFile 注释。
	af, ok := cred.Secret.(*authFile)
	if !ok || af == nil || af.a == nil {
		t.Fatalf("Secret 类型: %T（want *authFile，且内含非空 *Auth）", cred.Secret)
	}
	a := af.a
	// ⚠ uuid / keyfrom 必须随凭据持久化（续期要用）
	if a.UUID == "" || a.FirstKeyfrom == "" || a.LatestKeyfrom == "" {
		t.Errorf("登录后必须带上 uuid/keyfrom: %+v", a)
	}
	if a.RefreshToken != "rt-1" {
		t.Errorf("refresh_token = %q, want rt-1", a.RefreshToken)
	}
	if a.Nickname != "虾米" {
		t.Errorf("nickname = %q, want 虾米", a.Nickname)
	}
}

// TestLoginRejectsWrongState 回调 state 不匹配必须拒绝（防跨会话串号）。
func TestLoginRejectsWrongState(t *testing.T) {
	srv, calls := fakeUpstream(t, "available", false)
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL)})
	lf, ok := p.LoginFlow()
	if !ok {
		t.Fatal("Provider 必须实现 LoginFlow")
	}

	state, loginURL, err := lf.Start()
	if err != nil {
		t.Fatal(err)
	}
	frag := loginURL
	if i := strings.Index(loginURL, "#"); i >= 0 {
		frag = loginURL[i+1:]
	}
	q, _ := url.ParseQuery(strings.TrimPrefix(frag, "/login?"))
	redirect := q.Get("redirect_uri")

	// 用错的 state
	resp, err := http.Get(redirect + "?code=x&state=wrong-state")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("state 不匹配应回 400，实际 %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&calls.exchange) != 0 {
		t.Error("state 不匹配时不该发起 exchange")
	}
	// 会话仍未就绪
	if _, perr := lf.Poll(state); perr != gateway.ErrLoginPending {
		t.Errorf("错误 state 不该影响会话状态，实际 %v", perr)
	}
}

// TestExpiryFallsBackToJWT 过期判定回退 JWT。
func TestExpiryFallsBackToJWT(t *testing.T) {
	now := time.Now()
	// 无 expires_at → 回退 JWT（已过期）
	a := &Auth{AccessToken: fixtureJWT(now.Add(-time.Hour).Unix())}
	if !a.IsExpired(now) {
		t.Error("无 expires_at 时必须回退到 JWT exp")
	}
	// 无任何过期信息 → 不判过期
	if (&Auth{AccessToken: "not-a-jwt"}).IsExpired(now) {
		t.Error("无过期信息时不该判过期")
	}
	// 秒形态的 expires_at
	sec := now.Add(time.Hour).Unix()
	a2 := &Auth{AccessToken: "x", ExpiresAt: fmt.Sprintf("%d", sec)}
	if a2.IsExpired(now) {
		t.Error("未来的秒级 expires_at 不该判过期")
	}
	// 毫秒形态
	a3 := &Auth{AccessToken: "x", ExpiresAt: fmt.Sprintf("%d", now.Add(-time.Hour).UnixMilli())}
	if !a3.IsExpired(now) {
		t.Error("过去的毫秒级 expires_at 应判过期")
	}
}

// TestCredentialRoundTripKeepsKeyfrom 落盘必须保留 uuid/keyfrom。
//
// ⚠ 丢了它们续期会被服务端拒绝 —— 它们不在服务端响应里，
// 只存在于登录流程自己生成的状态中。
func TestCredentialRoundTripKeepsKeyfrom(t *testing.T) {
	a := &Auth{
		AccessToken: fixtureJWT(1790412721), RefreshToken: "rt",
		UID: "u-1", UserID: "acct-1", Nickname: "虾米",
		UUID:          "00000000-0000-4000-8000-000000000001",
		FirstKeyfrom:  "1700000000000",
		LatestKeyfrom: "1700000000000",
	}
	raw, err := MarshalAuthFile(a)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if back.UUID != a.UUID || back.FirstKeyfrom != a.FirstKeyfrom || back.LatestKeyfrom != a.LatestKeyfrom {
		t.Errorf("uuid/keyfrom 必须随凭据持久化:\n  want %+v\n  got  %+v", a, back)
	}
	if back.UIDValue() != "u-1" {
		t.Errorf("uid = %q, want u-1", back.UIDValue())
	}
}

// TestParseCredentialAcceptsFlatForm 扁平形也认。
func TestParseCredentialAcceptsFlatForm(t *testing.T) {
	a, err := ParseCredential([]byte(`{"access_token":"tok","refresh_token":"rt","uid":"u9"}`))
	if err != nil {
		t.Fatalf("扁平形应被接受: %v", err)
	}
	if a.UIDValue() != "u9" {
		t.Errorf("uid = %q, want u9", a.UIDValue())
	}
	if _, err := ParseCredential([]byte(`{"refresh_token":"rt"}`)); err == nil {
		t.Error("缺 access_token 应报错")
	}
}

// TestPrepareBodyForcesStreamOnly 请求体强制 stream（本上游 stream:false → 500）。
func TestPrepareBodyForcesStreamOnly(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"x","stream":false}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream 必须被强制为 true（本上游 stream:false 返回 500）")
	}
	// reasoning_effort 必须原样透传（不加白名单）
	out2 := PrepareBody([]byte(`{"model":"x","reasoning_effort":"off"}`))
	var obj2 map[string]any
	_ = json.Unmarshal(out2, &obj2)
	if obj2["reasoning_effort"] != "off" {
		t.Errorf("reasoning_effort 应原样透传，实际 %v", obj2["reasoning_effort"])
	}
	// 非法 JSON 原样返回
	raw := []byte(`not json`)
	if string(PrepareBody(raw)) != string(raw) {
		t.Error("非法 JSON 必须原样返回")
	}
}

// TestErrorClassification 错误分类（含 200 + 业务失败）。
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{401, ``, gateway.ErrKindAuth},
		{403, ``, gateway.ErrKindAuth},
		{429, ``, gateway.ErrKindSoftRate},
		{400, ``, gateway.ErrKindClient},
		{500, ``, gateway.ErrKindServer},
		{402, ``, gateway.ErrKindHardCredit},
		{200, `{"code":400,"msg":"积分不足"}`, gateway.ErrKindHardCredit},
		{200, `{"code":0,"msg":""}`, gateway.ErrKindNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestProviderDoesNotPanicOnBadCredential 错型凭证返回错误而非 panic。
func TestProviderDoesNotPanicOnBadCredential(t *testing.T) {
	p := NewWithConfig(Config{})
	bad := gateway.Credential{Provider: providerID, UID: "u", Secret: "not-an-auth"}
	if _, err := p.Chat(context.Background(), bad, []byte(`{}`)); err == nil {
		t.Error("错型 Secret 应返回错误")
	}
	if _, err := p.Models(context.Background(), bad); err == nil {
		t.Error("Models 在无有效凭证时应返回错误（本上游目录需要认证）")
	}
}
