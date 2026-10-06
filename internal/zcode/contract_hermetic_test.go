// contract_hermetic_test.go 契约测试与假上游 —— **判据的 enforcement**。
//
// # 为什么要有这个文件
//
// 本仓的架构判据是"加一个上游 = 加一个目录 + 实现一组接口，核心零改动"，
// 而 `arch_test.go` 守的是**依赖方向**（核心不依赖上游、上游不依赖核心）。
// 但依赖方向对了，**行为**仍可能是错的。契约测试守的是后者：
//
//	✓ gateway.Provider 的四个方法都真的能用（不是空实现）
//	✓ 声明的扩展点都能被 ExtOf 发现（不是写了没人认）
//	✓ ChatStream 的契约被遵守（非 2xx 也算正常返回、Body 可 Close）
//
// # 全程 hermetic（不碰真上游）
//
// 用 httptest 起一个假上游，把 Client 指过去。这样：
//   - CI 里能跑（不需要真 Key）
//   - 断言是**确定的**（真上游的行为会变，假上游不会）
//   - 能测**错误路径**（真上游不会为我造一个 3012 风控）
package zcode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// fakeUpstream 一个假 Z.ai 上游。
//
// 它刻意**只实现我们真的会打的端点**，并对请求头做断言 ——
// 这样"我们发的头不对"会在这里暴露，而不是在上游变成风控。
type fakeUpstream struct {
	srv   *httptest.Server
	calls struct {
		chat    int32
		billing int32
	}
	// lastHeaders 记录最后一次对话请求的头（供断言用）。
	lastHeaders http.Header
	// lastBody 记录最后一次对话请求的体。
	lastBody []byte
}

// newFakeUpstream 起一个假上游。
//
// handler 可以让用例定制行为（比如返回 3012 风控）。
func newFakeUpstream(t *testing.T, handler http.HandlerFunc) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/paas/v4/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls.chat, 1)
		f.lastHeaders = r.Header.Clone()
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.lastBody = body
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	})
	mux.HandleFunc("/api/coding/paas/v4/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls.chat, 1)
		f.lastHeaders = r.Header.Clone()
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		f.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"cp"},"finish_reason":"stop"}]}`))
	})
	mux.HandleFunc("/api/v1/zcode-plan/billing/current", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls.billing, 1)
		if r.Method != http.MethodGet {
			// ⚠ 实测上游是 GET；POST 会 404。这里钉住它，
			// 免得将来有人"顺手"改成 POST 而没人发现。
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"plans":[]}}`))
	})
	mux.HandleFunc("/api/v1/zcode-plan/billing/balance", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls.billing, 1)
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"balances":[{"remaining_units":1234,"total_units":5000,"unit_type":"tokens","show_name":"GLM-5.3"}]}}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 未预期的路径也要能被发现（返回一个可识别的错误）。
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"9999","message":"unexpected path ` + r.URL.Path + `"}}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newTestProvider 造一个指向假上游的 Provider。
func newTestProvider(t *testing.T, f *fakeUpstream, creds ...*Auth) *Provider {
	t.Helper()
	p := New(Config{
		Origin:     f.srv.URL,
		AuthDir:    t.TempDir(),
		HTTPClient: f.srv.Client(),
	})
	p.probeOrigin = false
	for _, a := range creds {
		if a.UID == "" {
			a.ensureUID()
		}
		p.creds[a.UID] = a
	}
	return p
}

// gateway.Credential 的构造助手。
func credOf(a *Auth) gateway.Credential {
	return gateway.Credential{Provider: ProviderID, UID: a.UID, Secret: a}
}

// ---- 契约测试 ----

// TestContract 本仓的标准契约测试入口。
//
// 它跑 gateway.RunProviderContract，覆盖"四个方法真的能用"与
// "声明的扩展点能被发现"。新上游必须挂上它 —— 否则
// "实现了一堆扩展点但核心一个都没认出来"这类问题没人守。
func TestContract(t *testing.T) {
	f := newFakeUpstream(t, nil)
	gateway.RunProviderContract(t, func() gateway.Provider {
		return newTestProvider(t, f, &Auth{
			Kind: CredKindAPIKey, APIKey: "test.key", UID: "contract-user",
		})
	}, gateway.WithCredential(contractCredential("contract-user")))
}

// contractCredential 造一份契约测试用的凭证。
//
// ⚠ 必须带 **Secret**（不是只有 UID）：契约测试会拿它调 Chat/Models，
// 而本包的 authOf 优先读 Secret。只给 UID 会走"按 UID 查活凭证"的
// 回落路径 —— 而契约测试用的是新造的 Provider，本地表里没有那份凭证。
func contractCredential(uid string) gateway.Credential {
	a := &Auth{Kind: CredKindAPIKey, APIKey: "test.key", UID: uid}
	a.ensureUID()
	return gateway.Credential{Provider: ProviderID, UID: uid, Secret: a}
}

// Models **无凭证也必须给出目录**。
//
// # 为什么这条必须存在（实测发现的缺口）
//
// 出口层调 Models 时**不带凭证**（handler.go 的 modelList 只传 provider id）。
// 早先的实现在拿不到凭证时返回 error，于是：
//
//	新装的 zcode 上游在 /v1/models 里**一个模型都不出现**，
//	直到有人加账号 —— 而那正是用户第一次配置上游的时刻。
//	他会看到"zcode 没有模型"，以为装坏了。
//
// 更要紧的是：**内置兜底清单成了死代码**（永远走不到），
// 而单测因为总是塞了凭证，对此完全无感。
func TestModelsWithoutCredentialStillReturnsCatalog(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f) // 刻意不装任何凭证

	list, err := p.Models(context.Background(), gateway.Credential{})
	if err != nil {
		t.Fatalf("无凭证时 Models 不该报错（出口层就是不带凭证调的）: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("无凭证时也要给出兜底目录 —— 否则新装的上游在 /v1/models 里是空的")
	}
	var found bool
	for _, m := range list {
		if m.ID == "GLM-5.3" {
			found = true
		}
	}
	if !found {
		t.Errorf("兜底目录里应有 GLM-5.3，实际 %+v", list)
	}

	// 只带 UID（没有 Secret）时也要能用 —— 那是装配层探测路径的形状。
	list2, err := p.Models(context.Background(), gateway.Credential{UID: "unknown-uid"})
	if err != nil {
		t.Fatalf("只带 UID 时也不该报错: %v", err)
	}
	if len(list2) == 0 {
		t.Error("只带 UID 时也要给出目录")
	}
}

// 官网实时配置取不到时回落内置清单（不能整个空掉）。
func TestModelsFallsBackToBuiltin(t *testing.T) {
	// 假上游没有 /api/v1/client/configs（它在 planOrigin 上，换不进去），
	// 所以这条必然走回落路径 —— 正是我们要守的行为。
	f := newFakeUpstream(t, nil)
	a := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u1"}
	p := newTestProvider(t, f, a)

	list, err := p.Models(context.Background(), credOf(a))
	if err != nil {
		t.Fatalf("取不到实时配置时不该报错: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("应回落内置清单，而不是返回空")
	}
}

// 管理端点：**无账号时必须回 `[]` 而不是 `null`**。
//
// # 为什么这条必须存在（实测发现的缺陷）
//
// Go 的 nil 切片序列化成 `null`，而前端 `data.accounts.length`
// 在 null 上会抛 TypeError —— 后端返回的却是 200。
// 症状是"界面什么都不显示 + 控制台一个异常"，看起来像接口坏了。
//
// 而触发它的正是**第一次配置上游**的时刻（还没加账号），
// 也就是说：用户最需要看到"我该做什么"的那一次，看到的是空白。
func TestAdminEndpointsReturnEmptyArrayNotNull(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f) // 无任何凭证

	for _, path := range []string{"/admin/zcode/diagnose", "/admin/zcode/quota"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			for _, r := range p.AdminRoutes() {
				if r.Path == path {
					r.Handler(rec, req)
				}
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("HTTP %d，期望 200", rec.Code)
			}
			body := rec.Body.String()

			// 决定性断言：原始 JSON 里必须是 `[]`，不能是 `null`。
			if strings.Contains(body, `"accounts":null`) {
				t.Fatalf("accounts 是 null —— 前端 .length 会抛 TypeError。"+
					"必须是 []：\n%s", body)
			}
			if !strings.Contains(body, `"accounts":[]`) {
				t.Errorf("accounts 应是空数组，实际：\n%s", body)
			}

			// 顺带守：response 里要有 provider 字段（前端据此分组）。
			if !strings.Contains(body, `"provider":"zcode"`) {
				t.Errorf("响应缺少 provider 字段：\n%s", body)
			}
		})
	}
}

// 管理端点：**有账号时要真的列出诊断信息**（不是空数组）。
//
// # ⚠ 这条测试为什么要刻意把凭证放在 credSrc 而不是本地表
//
// 真实运行时凭证在 `pool` 里，Provider 侧的 `p.creds` **是空的**
// （见 knownUIDs 的注释：装配层只注入 credSrc 与枚举器）。
//
// 我第一版测试把凭证同时塞进 `p.creds` 与枚举器 —— 于是**变异验证
// 发现它守不住**：把枚举器停掉后，knownUIDs 回落到本地表照样能返回
// 那个 uid，测试仍然绿。而生产中本地表是空的，端点会返回 `[]`。
//
// 所以这里刻意**只**走 credSrc 路径：凭证不进 p.creds，
// 只能通过 credSrc 与枚举器拿到 —— 与真实运行时的形状一致。
func TestDiagnoseListsAccountsWhenPresent(t *testing.T) {
	f := newFakeUpstream(t, nil)
	a := &Auth{
		Kind: CredKindAPIKey, APIKey: "diag-key-1111.2222",
		UID: "diag-user", Nickname: "诊断号", Origin: originBigModel,
	}
	a.ensureUID()

	// ⚠ p.creds 刻意**不装**这份凭证 —— 模拟真实运行时。
	p := New(Config{Origin: f.srv.URL, AuthDir: t.TempDir(), HTTPClient: f.srv.Client()})
	p.probeOrigin = false
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		if uid != "diag-user" {
			return gateway.Credential{}, false
		}
		return gateway.Credential{Provider: ProviderID, UID: uid, Nickname: a.Nickname, Secret: a}, true
	})
	// 枚举器是**唯一**能列出账号的途径（真实运行时它来自 pool.ListFor）。
	p.SetUIDEnumerator(func() []string { return []string{"diag-user"} })

	req := httptest.NewRequest(http.MethodGet, "/admin/zcode/diagnose", nil)
	rec := httptest.NewRecorder()
	for _, r := range p.AdminRoutes() {
		if r.Path == "/admin/zcode/diagnose" {
			r.Handler(rec, req)
		}
	}
	body := rec.Body.String()
	if strings.Contains(body, `"accounts":[]`) || strings.Contains(body, `"accounts":null`) {
		t.Fatalf("枚举器给了 uid，端点却没列出账号 —— "+
			"说明它没走枚举器（真实运行时 p.creds 是空的）：\n%s", body)
	}
	var resp struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(resp.Accounts) != 1 {
		t.Fatalf("应列出 1 个账号，实际 %d：%s", len(resp.Accounts), body)
	}
	row := resp.Accounts[0]
	if row["uid"] != "diag-user" {
		t.Errorf("uid 不对: %v", row["uid"])
	}
	if row["platform"] != "BigModel" {
		t.Errorf("平台应识别成 BigModel（凭证里写的是 bigmodel），实际 %v", row["platform"])
	}
	if row["channel"] != "API Key" {
		t.Errorf("通道应识别成 API Key，实际 %v", row["channel"])
	}
	// ⚠ 令牌**不能**被完整回显 —— 诊断输出常被贴到 issue 里求助。
	if tok, _ := row["token_tail"].(string); tok != "…2222" {
		t.Errorf("token_tail 应只给尾 4 位，实际 %q", tok)
	}
	if s, _ := row["endpoint"].(string); !strings.Contains(s, pathPaaS) {
		t.Errorf("endpoint 不对（BigModel 的普通 Key 应走 %s）: %q", pathPaaS, s)
	}
}

// 管理端点：**只靠 credSrc 也要能取到凭证**（不是只读本地表）。
//
// 与上一条配合：那条测"能列出账号"，这条测"能读到凭证内容"。
// 两条分开是因为它们走的是**两条不同的注入通道**
// （枚举器 vs credSrc），任何一条断了都要能单独发现。
func TestQuotaUsesCredentialSourceNotLocalTable(t *testing.T) {
	f := newFakeUpstream(t, nil)
	a := &Auth{Kind: CredKindAPIKey, APIKey: "src-key", UID: "src-user"}
	a.ensureUID()

	p := New(Config{Origin: f.srv.URL, AuthDir: t.TempDir(), HTTPClient: f.srv.Client()})
	p.probeOrigin = false
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		if uid != "src-user" {
			return gateway.Credential{}, false
		}
		return gateway.Credential{Provider: ProviderID, UID: uid, Secret: a}, true
	})
	p.SetUIDEnumerator(func() []string { return []string{"src-user"} })

	req := httptest.NewRequest(http.MethodGet, "/admin/zcode/quota", nil)
	rec := httptest.NewRecorder()
	for _, r := range p.AdminRoutes() {
		if r.Path == "/admin/zcode/quota" {
			r.Handler(rec, req)
		}
	}
	body := rec.Body.String()
	// API Key 通道没有额度端点 → has_data 必须是 false（不是 0）。
	var resp struct {
		Accounts []struct {
			UID      string `json:"uid"`
			Channel  string `json:"channel"`
			HasData  bool   `json:"has_data"`
			Note     string `json:"note"`
			Platform string `json:"platform"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	if len(resp.Accounts) != 1 {
		t.Fatalf("应列出 1 个账号（说明读到了 credSrc 里的凭证），实际 %d：%s",
			len(resp.Accounts), body)
	}
	row := resp.Accounts[0]
	if row.UID != "src-user" {
		t.Errorf("uid 不对: %q", row.UID)
	}
	if row.HasData {
		t.Error("API Key 通道没有额度端点 → has_data 必须为 false（不能谎报 0）")
	}
	if row.Note == "" {
		t.Error("没有额度数据时必须说明原因，否则用户以为是接口坏了")
	}
}

// ID 与能力位。
func TestProviderIdentity(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	if p.ID() != "zcode" {
		t.Errorf("ID() = %q，期望 zcode", p.ID())
	}
	if p.DisplayName() == "" {
		t.Error("DisplayName 不能为空（控制台要显示它）")
	}

	// 必须声明 chat 与 models（否则核心不会把它当对话上游）。
	caps := p.Caps()
	for _, need := range []gateway.Capability{gateway.CapChat, gateway.CapModels} {
		if caps&need == 0 {
			t.Errorf("能力位缺少 chat/models 之一（实际 %v）", caps)
		}
	}
	// 刻意**不**声明的能力：本上游没有这些端点，声明了会让前端
	// 显示出不存在的按钮（点了就报错，比不显示更糟）。
	for _, bad := range []gateway.Capability{gateway.CapCheckin, gateway.CapGrowth, gateway.CapTravel, gateway.CapWelfare} {
		if caps&bad != 0 {
			t.Errorf("ZCode 没有对应端点，不该声明能力位 %v", bad)
		}
	}
}

// Chat 的成功路径：非 2xx 也算正常返回、Body 可读可 Close。
func TestChatReturnsBodyAndStatus(t *testing.T) {
	f := newFakeUpstream(t, nil)
	a := &Auth{Kind: CredKindAPIKey, APIKey: "k.k", UID: "u1"}
	p := newTestProvider(t, f, a)

	cs, err := p.Chat(context.Background(), credOf(a),
		[]byte(`{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	if cs.Body == nil {
		t.Fatal("Body 不能为 nil（调用方负责 Close）")
	}
	defer cs.Body.Close()
	if cs.Status != http.StatusOK {
		t.Errorf("Status = %d，期望 200", cs.Status)
	}
	body, err := io.ReadAll(cs.Body)
	if err != nil {
		t.Fatalf("读 Body 失败: %v", err)
	}
	if !strings.Contains(string(body), `"chat.completion"`) {
		t.Errorf("Body 不是 OpenAI 形态: %s", body)
	}
	if atomic.LoadInt32(&f.calls.chat) != 1 {
		t.Errorf("假上游应收到 1 次对话请求，实际 %d", f.calls.chat)
	}
}

// Chat 的错误路径：**非 2xx 也要返回 ChatStream**（不是 error）。
//
// # 这条契约为什么重要
//
// 出口层要靠它把上游的业务错误体带回给调用方判分类
// （限流 vs 额度耗尽 vs 需重登）。如果这里返回 error，
// 业务码就丢了，所有失败都会退化成"未知错误"。
func TestChatReturnsStreamOnUpstreamError(t *testing.T) {
	f := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// 本上游的风格：HTTP 200 + 业务码（也可能是 4xx）。
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"3012","message":"unusual activity"}}`))
	})
	a := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u1"}
	p := newTestProvider(t, f, a)

	cs, err := p.Chat(context.Background(), credOf(a),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("非 2xx 不该返回 error（那会丢掉业务码）: %v", err)
	}
	defer cs.Body.Close()
	if cs.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d，期望 429", cs.Status)
	}
	body, _ := io.ReadAll(cs.Body)
	if !strings.Contains(string(body), "3012") {
		t.Errorf("业务码丢了: %s", body)
	}
}

// Chat 缺凭证要报错（不是静默发一个无鉴权请求 —— 那会被上游当风控）。
func TestChatWithoutCredentialFails(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	if _, err := p.Chat(context.Background(), gateway.Credential{UID: "nobody"},
		[]byte(`{"model":"m","messages":[]}`)); err == nil {
		t.Error("缺凭证应报错")
	}
	if n := atomic.LoadInt32(&f.calls.chat); n != 0 {
		t.Errorf("缺凭证时不该发出请求，实际发了 %d 次", n)
	}
}

// 凭证里的 origin 必须被用上（Z.ai 与 BigModel 是两套平台）。
//
// ⚠ 断言的是**配置为空时**的行为。newTestProvider 会把 Origin 指到假上游
// （否则打不到 httptest），而显式配置的 origin 优先级最高 —— 那样就测不出
// "凭证自己带的平台的优先级"了。所以这里用**裸 Client** 测选路逻辑。
func TestChatUsesCredentialOrigin(t *testing.T) {
	// Origin 未配置：凭证显式带的平台要生效。
	c := NewClient(Config{})

	explicit := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u1", Origin: originBigModel}
	if got := c.originFor(explicit); got != originBigModel {
		t.Errorf("凭证显式 origin 未被使用: %q，期望 %q", got, originBigModel)
	}
	// 两者都没配 → 回落 Z.ai（不能返回空串，否则 URL 拼出来是相对路径）。
	bare := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u2"}
	if got := c.originFor(bare); got != originZAI {
		t.Errorf("都没配时应回落 Z.ai，实际 %q", got)
	}
	// 全局配置的作用是"默认值"：凭证没带平台时用它。
	global := NewClient(Config{Origin: "https://custom.example"})
	bareForGlobal := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u6"}
	if got := global.originFor(bareForGlobal); got != "https://custom.example" {
		t.Errorf("凭证没带平台时应回落全局配置，实际 %q", got)
	}
	// ⚠ 但凭证带的平台**优先于**全局配置 —— 因为"这个账号属于哪个平台"
	// 是账号的属性，而全局配置只能表达默认值。多平台部署时若全局配置
	// 压过凭证，BigModel 的 Key 会被打到 Z.ai。
	if got := global.originFor(explicit); got != originBigModel {
		t.Errorf("凭证的平台应优先于全局配置，实际 %q（期望 %q）", got, originBigModel)
	}

	// 带 CodingPlan 标记时走 coding 端点。
	cp := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u3", CodingPlan: true}
	if got := c.chatURL(cp); !strings.Contains(got, pathCodingPaaS) {
		t.Errorf("Coding Plan 的 Key 应走 %s，实际 %s", pathCodingPaaS, got)
	}
	// 不带标记时走通用端点。
	plain := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u4"}
	if got := c.chatURL(plain); !strings.Contains(got, pathPaaS) {
		t.Errorf("普通 Key 应走 %s，实际 %s", pathPaaS, got)
	}
	if strings.Contains(c.chatURL(plain), pathCodingPaaS) {
		t.Error("普通 Key **不该**走 coding 端点 —— 官方文档说两者额度不互通")
	}
	// JWT 通道走 plan 的 Anthropic 端点（不是 api.z.ai）。
	jwt := &Auth{Kind: CredKindJWT, JWT: "j", UID: "u5"}
	got := c.chatURL(jwt)
	if !strings.Contains(got, planAnthropicPath) {
		t.Errorf("JWT 通道应走 %s，实际 %s", planAnthropicPath, got)
	}
	if !strings.HasPrefix(got, planOrigin) {
		t.Errorf("JWT 通道应打 zcode.z.ai（%s），实际 %s", planOrigin, got)
	}
}

// Models 必须给出可用的模型清单。
func TestModelsReturnsCatalog(t *testing.T) {
	f := newFakeUpstream(t, nil)
	a := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u1"}
	p := newTestProvider(t, f, a)

	list, err := p.Models(context.Background(), credOf(a))
	if err != nil {
		t.Fatalf("Models 失败: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("模型清单不能为空（空清单会让 /v1/models 里没有 zcode/ 前缀的模型）")
	}
	// 官方实时配置取不到时（假上游没有那个端点）应回落到内置清单。
	var found bool
	for _, m := range list {
		if m.ID == "GLM-5.3" {
			found = true
			// 上下文窗口要填（官方配置写明 1M）——
			// 留 0 会让前端的上下文用量条显示不出来。
			if m.ContextWindow != 1_000_000 {
				t.Errorf("GLM-5.3 的 ContextWindow = %d，期望 1000000", m.ContextWindow)
			}
			if m.MaxOutputTokens != 128_000 {
				t.Errorf("GLM-5.3 的 MaxOutputTokens = %d，期望 128000", m.MaxOutputTokens)
			}
		}
	}
	if !found {
		t.Errorf("清单里没有 GLM-5.3: %+v", list)
	}
}

// 官方实时配置端点在（真上游才有）时用它的清单。这里用一个假的
// client/configs 响应验证解析。
func TestModelsParseOfficialConfig(t *testing.T) {
	raw := []byte(`{"code":200,"msg":"ok","data":{"builtinModels":[
	  {"modelId":"GLM-9.9","name":"GLM-9.9","contextWindow":2048000,"maxCompletionTokens":65536},
	  {"modelId":"GLM-9.9-Flash","name":"Flash","contextWindow":1000000,"maxCompletionTokens":128000},
	  {"modelId":"","name":"坏条目"}
	]}}`)
	list, err := parseClientConfigModels(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应解析出 2 个模型（跳过空 modelId），实际 %d: %+v", len(list), list)
	}
	if list[0].ID != "GLM-9.9" || list[0].ContextWindow != 2048000 {
		t.Errorf("第一个模型解析不对: %+v", list[0])
	}
	if list[1].MaxOutputTokens != 128000 {
		t.Errorf("第二个模型的 MaxOutputTokens 不对: %+v", list[1])
	}
}

// ---- 扩展点被发现 ----

// 声明的扩展点必须都能被 gateway.ExtOf 发现。
//
// # 为什么这条要单独守
//
// 扩展点的发现是**纯类型断言**（见 gateway.ExtOf）。所以
// "实现了方法"与"核心能发现它"是两件事 —— 如果方法签名差一个
// 参数（比如返回值数量不对），编译能过，而核心**永远发现不了它**。
// 那类缺陷的表现是"功能静默消失"（额度列一直空、续期不触发）。
func TestExtensionsAreDiscoverable(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f, &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "u1"})

	checks := []struct {
		name string
		ok   bool
	}{
		{"DisplayNameExt", hasExt[gateway.DisplayNameExt](p)},
		{"AuthDirExt", hasExt[gateway.AuthDirExt](p)},
		{"CredentialTokenExt", hasExt[gateway.CredentialTokenExt](p)},
		{"CredentialExpiryExt", hasExt[gateway.CredentialExpiryExt](p)},
		{"CredentialLifetimeExt", hasExt[gateway.CredentialLifetimeExt](p)},
		{"RefreshSkewExt", hasExt[gateway.RefreshSkewExt](p)},
		{"AccountColumnsExt", hasExt[gateway.AccountColumnsExt](p)},
		{"AccountImportExt", hasExt[gateway.AccountImportExt](p)},
		{"QuotaExt", hasExt[gateway.QuotaExt](p)},
		{"HealthProbeExt", hasExt[gateway.HealthProbeExt](p)},
		{"SoftRateExt", hasExt[gateway.SoftRateExt](p)},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("扩展点 %s 未被发现 —— 实现写了但核心认不出来（签名不对？）", c.name)
		}
	}
}

// hasExt 就是生产那条判据（ExtOf 的薄包装，避免测试里重复泛型样板）。
func hasExt[T any](p gateway.Provider) bool {
	_, ok := gateway.ExtOf[T](p)
	return ok
}

// CredentialTokenExt：空凭证不该被当成有令牌。
func TestHasTokenRejectsEmpty(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	if p.HasToken(credOf(&Auth{Kind: CredKindAPIKey, APIKey: ""})) {
		t.Error("空 API Key 不该算有令牌")
	}
	if p.HasToken(credOf(&Auth{Kind: CredKindJWT, JWT: ""})) {
		t.Error("空 JWT 不该算有令牌")
	}
	if !p.HasToken(credOf(&Auth{Kind: CredKindAPIKey, APIKey: "k"})) {
		t.Error("有 API Key 应算有令牌")
	}
	if !p.HasToken(credOf(&Auth{Kind: CredKindJWT, JWT: "j"})) {
		t.Error("有 JWT 应算有令牌")
	}
}

// CredentialLifetimeExt：API Key 永不过期，JWT 会过期。
//
// # 这条守的是"每个请求都续期"那个缺陷
//
// 若把 API Key 也报成"会过期"，出口层会为它触发续期逻辑 ——
// 而它根本没有续期一说，于是白做一次往返（本仓实测过这个症状）。
func TestNeverExpiresOnlyForAPIKeyChannel(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	if !p.NeverExpires(credOf(&Auth{Kind: CredKindAPIKey, APIKey: "k"})) {
		t.Error("API Key 通道应声明永不过期（它没有续期一说）")
	}
	if p.NeverExpires(credOf(&Auth{Kind: CredKindJWT, JWT: "j", ExpiresAt: time.Now().Add(time.Hour).Unix()})) {
		t.Error("JWT 通道会过期，不该声明永不过期")
	}
}

// RefreshSkewExt：只有 JWT 通道需要提前量。
func TestRefreshSkewOnlyForJWT(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	if _, ok := p.RefreshSkew(credOf(&Auth{Kind: CredKindAPIKey, APIKey: "k"})); ok {
		t.Error("API Key 通道不需要续期提前量")
	}
	skew, ok := p.RefreshSkew(credOf(&Auth{Kind: CredKindJWT, JWT: "j", ExpiresAt: time.Now().Add(time.Hour).Unix()}))
	if !ok {
		t.Fatal("JWT 通道应有续期提前量")
	}
	if skew <= 0 {
		t.Errorf("提前量应为正数，实际 %v", skew)
	}
}

// TokenExpiry 的单位必须是**毫秒**（本仓约定）。
func TestTokenExpiryIsMilliseconds(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	const sec = 1793000000
	a := &Auth{Kind: CredKindJWT, JWT: "j", ExpiresAt: sec}
	got, ok := p.TokenExpiry(credOf(a))
	if !ok {
		t.Fatal("JWT 有 exp 时应报 ok=true")
	}
	if got != sec*1000 {
		t.Errorf("TokenExpiry = %d，期望 %d（毫秒）—— 单位错了会让账号被判早/晚过期", got, sec*1000)
	}

	// 没有过期信息 → ok=false（不能报 0，那会被当成"刚过期"）。
	if _, ok := p.TokenExpiry(credOf(&Auth{Kind: CredKindAPIKey, APIKey: "k"})); ok {
		t.Error("没有过期信息时应返回 ok=false")
	}
}

// AccountColumnsExt：自报的列 id **必须在规范词汇表里**。
//
// # 为什么这条必须存在（实测发现的缺陷）
//
// 我第一版回的是 `[]string{"通道", "平台"}` —— 那是**列标题**，不是 id。
// 核心校验时把它们当"未登记的 id"，前端直接跳过 →
// **两列都不会显示**，而这只在运行时日志里有一行警告：
//
//	admin: 上游 zcode 自报的账号列 id "通道" 不在规范词汇表里
//
// 既不编译失败、测试也不会红 —— 除非专门断言"id 已登记"。
// 这正是本条要守的。
func TestAccountColumnsAreRegisteredIDs(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	// 规范词汇表（gateway 里那些 AccountCol* 常量）。
	// 这里**刻意列全**而不是只列本上游用的几个：将来有人复制本上游的
	// 列集去写新上游时，这个集合能让"有没有搞错"一眼可见。
	registered := map[string]bool{
		gateway.AccountColProvider:    true,
		gateway.AccountColNickname:    true,
		gateway.AccountColUID:         true,
		gateway.AccountColQuota:       true,
		gateway.AccountColStatus:      true,
		gateway.AccountColToken:       true,
		gateway.AccountColTokenExpiry: true,
		gateway.AccountColCheckin:     true,
		gateway.AccountColWelfare:     true,
		gateway.AccountColSuccess:     true,
		gateway.AccountColBreaker:     true,
		gateway.AccountColInFlight:    true,
		gateway.AccountColOps:         true,
	}

	cols := p.AccountColumns()
	if len(cols) == 0 {
		t.Fatal("应至少给出一列")
	}
	for _, c := range cols {
		if !registered[c] {
			t.Errorf("列 id %q 不在规范词汇表里 —— 前端会跳过它，该列**不会显示**。"+
				"（列标题是前端的事实，上游只能报 id。见 gateway/account_columns.go）", c)
		}
		// 中文标题混进来是最典型的错法，单独点出来。
		for _, r := range c {
			if r > 0x4e00 && r < 0x9fff {
				t.Errorf("列 id %q 里含中文字符 —— 那看起来是列标题而不是 id", c)
				break
			}
		}
	}

	// 本上游该有的三列（上游/昵称/额度）。
	for _, want := range []string{
		gateway.AccountColProvider, gateway.AccountColNickname,
		gateway.AccountColQuota,
	} {
		var found bool
		for _, c := range cols {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少列 %q", want)
		}
	}

	// ⚠ **不该**报的列（两类，理由不同，都值得钉住）
	//
	// ① token / token_expiry：zcode 的 JWT **没有 exp**（实测解出来只有 iat，
	//    官方 resolveJwtExpiration 对这种情况也返回 "unknown"）。
	//    报它们会让那两列**永远是 `—`**，而用户会把 `—` 读成"功能没做"。
	// ② checkin / welfare：本上游没有这些概念。
	for _, bad := range []string{
		gateway.AccountColToken,
		gateway.AccountColTokenExpiry,
		gateway.AccountColCheckin,
		gateway.AccountColWelfare,
	} {
		for _, c := range cols {
			if c == bad {
				t.Errorf("本上游不该自报列 %q：\n"+
					"  token/token_expiry → JWT 无 exp，这两列会永远是 `—`\n"+
					"  checkin/welfare    → 本上游没有这些概念", bad)
			}
		}
	}
}

// QuotaExt：Coding Plan 从计量端点取额度，取不到要报 HasData=false。
//
// # 为什么 HasData 不能乱置 true
//
// 本仓的约定是三态：查不到 (`—`) / 真的是 0 (`0`) / 有额度。
// 把"查不到"报成 0 会让用户以为额度用尽 —— 而实际是我们没查到，
// 那会引发一轮无意义的排查。
// QuotaExt：**凭证缺失/不可用时必须报 HasData=false**。
//
// # 为什么这条要单独写（变异验证逼出来的）
//
// 早先只有 parseBillingBalance 的测试 —— 那守的是"解析对不对"，
// 完全没覆盖 RefreshQuota 的第一道分支。把那里改成
// `{HasData: true, Remaining: 0}` 之后**测试仍然全绿**，
// 而那是最坏的一种谎报：凭证坏掉的账号会在界面上显示"额度 0"，
// 用户以为额度用尽，而实际是我们连凭证都读不到。
func TestQuotaReportsNoDataWhenCredentialUnusable(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	// ① 未知 uid（凭证表里没有）。
	view, ok := p.RefreshQuota("no-such-uid")
	if view.HasData {
		t.Errorf("未知凭证应报 HasData=false，实际 %+v（界面会显示额度 0）", view)
	}
	if ok {
		t.Error("未知凭证应返回 ok=false")
	}

	// ② 凭证存在但没有令牌。
	p.creds["empty-user"] = &Auth{Kind: CredKindAPIKey, APIKey: "", UID: "empty-user"}
	view, ok = p.RefreshQuota("empty-user")
	if view.HasData {
		t.Errorf("空令牌的凭证应报 HasData=false，实际 %+v", view)
	}
	if ok {
		t.Error("空令牌的凭证应返回 ok=false")
	}
}

// 计量端点的解析：**真失败**必须报 HasData=false（不是 0）。
//
// ⚠ 与下面那条的区别（这是我第一版搞混的地方）：
//
//	真失败（网络/业务码非 0/非法 JSON）  → HasData=false（界面 `—`）
//	查询成功但账号没有套餐（plans 空）    → HasData=true, 0（界面 `0`）
//
// 两者都"看起来没有数据"，但含义完全相反：前者是"我们没查到"，
// 后者是"这个号确实没额度"。混为一谈会让用户对着 `—` 排查我们的服务。
func TestQuotaBillingRealFailureReportsNoData(t *testing.T) {
	for _, raw := range []string{
		``,         // 空响应
		`not json`, // 非 JSON
		`<html>502</html>`,
		`{"code":3004,"msg":"invalid_flow"}`, // 业务码非 0
		`{"code":401,"msg":"token expired"}`,
	} {
		if view, ok := parseBillingSnapshot([]byte(raw)); ok || view.HasData {
			t.Errorf("响应 %q 是**真失败**，应报 HasData=false，实际 %+v (ok=%v)", raw, view, ok)
		}
	}
}

func TestQuotaFromBilling(t *testing.T) {
	// ① 有额度桶（参照实现的字段名）。
	view, ok := parseBillingSnapshot([]byte(`{"code":0,"data":{"balances":[
	  {"remaining_units":1234,"total_units":5000,"unit_type":"tokens","show_name":"GLM-5.3"}
	]}}`))
	if !ok {
		t.Fatal("应解析出额度")
	}
	if !view.HasData {
		t.Error("解析到数据时 HasData 应为 true")
	}
	if view.Remaining != 1234 {
		t.Errorf("Remaining = %d，期望 1234", view.Remaining)
	}

	// ② **实测的真实形态**：查询成功但账号没有订阅套餐。
	//
	// 这是用户账号的实际响应（我实测抓到的）：
	//
	//	GET /billing/current?app_version=3.14.0
	//	→ 200 {"code":0,"msg":"","data":{"server_time":1700000000,"plans":[]}}
	//
	// ⚠ 这一条必须报 HasData=true + 0，**不能**报 false。
	// 报 false 会让界面显示 `—`，用户以为是我们查不到 ——
	// 而真相是"登录成功了，但这个号没有 Coding Plan 额度"。
	view, ok = parseBillingSnapshot([]byte(`{"code":0,"msg":"","data":{"server_time":1700000000,"plans":[]}}`))
	if !ok {
		t.Fatal("查询成功（code=0）应返回 ok=true —— 即使没有额度数据")
	}
	if !view.HasData {
		t.Error("code=0 但无额度 → 应报 HasData=true, Remaining=0（界面显示 0），" +
			"而不是 HasData=false（界面显示 —，会让用户以为是查询失败）")
	}
	if view.Remaining != 0 {
		t.Errorf("Remaining 应为 0，实际 %d", view.Remaining)
	}

	// ③ 三值互推：只给 total + used，剩余量要能算出来（照参照实现）。
	view, ok = parseBillingSnapshot([]byte(`{"code":0,"data":{"balances":[
	  {"total_units":100,"used_units":30,"show_name":"额度A"}
	]}}`))
	if !ok || view.Remaining != 70 {
		t.Errorf("应能由 total-used 推出剩余 70，实际 ok=%v %+v", ok, view)
	}

	// ④ 字段别名：上游改过字段名，参照实现同时认多套。
	view, ok = parseBillingSnapshot([]byte(`{"code":0,"data":{"balances":[
	  {"remaining":55,"name":"别名桶"}
	]}}`))
	if !ok || view.Remaining != 55 {
		t.Errorf("应认得别名 remaining，实际 ok=%v %+v", ok, view)
	}

	// ⑤ 字符串数字 + 千分位（参照实现专门去掉了逗号）。
	view, ok = parseBillingSnapshot([]byte(`{"code":0,"data":{"balances":[
	  {"remaining_units":"1,234","show_name":"字符串桶"}
	]}}`))
	if !ok || view.Remaining != 1234 {
		t.Errorf("应认得带千分位的字符串数字，实际 ok=%v %+v", ok, view)
	}

	// ⑥ 顶层直给（部分响应不带 balances）。
	view, ok = parseBillingSnapshot([]byte(`{"code":0,"data":{"remaining_units":88}}`))
	if !ok || view.Remaining != 88 {
		t.Errorf("应认得顶层 remaining_units，实际 ok=%v %+v", ok, view)
	}

	// ⑦ 多个桶 → per_model 且带明细。
	view, ok = parseBillingSnapshot([]byte(`{"code":0,"data":{"balances":[
	  {"remaining_units":10,"show_name":"A"},
	  {"remaining_units":20,"show_name":"B"}
	]}}`))
	if !ok || view.Remaining != 30 {
		t.Fatalf("多桶应合计 30，实际 %+v", view)
	}
	if view.Kind != "per_model" || len(view.ByModel) != 2 {
		t.Errorf("多桶应报 per_model 并带明细，实际 %+v", view)
	}
}

// QuotaExt：**必须先打 current，且把它的数据写回**。
//
// # 为什么这条必须存在（用户实测报的 bug）
//
// 我原来直接打 `balance`，而**实测它是 400**（用真请求验的）：
//
//	GET /billing/balance                     → 400 {"code":3001,"msg":"parameter error"}
//	GET /billing/balance?app_version=3.14.0  → 400 {"code":3001,"msg":"parameter error"}
//	GET /billing/balance?user_id=…           → 400 {"code":3001,"msg":"parameter error"}
//	GET /billing/current?app_version=3.14.0  → 200 {"code":0,"data":{"server_time":…,"plans":[]}}
//
// 参照实现（zcode-proxy/quota.go）的注释印证了这个顺序：
//
//	jwt → zcode.z.ai /api/v1/zcode-plan/billing/current（失败再试 /billing/balance）
//
// 顺序反了**不会有任何报错**：只会表现为"额度永远查不到"。
// 所以用一个假上游记录**实际打到的路径**来钉住它。
func TestBillingHitsCurrentFirst(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// current 成功（模拟真实上游 —— 它确实通）。
		if strings.Contains(r.URL.Path, "current") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"server_time":1,"plans":[]}}`))
			return
		}
		// balance 报参数错（模拟实测行为）—— 这样一旦打到它就暴露了。
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3001,"msg":"parameter error"}`))
	}))
	defer srv.Close()

	// 把假上游指进 client（billingBase 可注入 —— 正是为了这条测试）。
	p := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), BillingOrigin: srv.URL})
	p.probeOrigin = false
	a := &Auth{Kind: CredKindJWT, JWT: "jwt-x", UID: "billing-user"}
	p.creds[a.UID] = a

	view, ok := p.RefreshQuota(a.UID)
	if !ok {
		mu.Lock()
		t.Fatalf("应拿到额度；实际打到的路径: %v", paths)
		mu.Unlock()
	}
	if !view.HasData {
		t.Error("应报 HasData=true")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 {
		t.Fatal("没有任何请求 —— quotaFromBilling 没走到网络层")
	}
	// ① 第一条必须是 current（不能是 balance）。
	if !strings.Contains(paths[0], "/billing/current") {
		t.Errorf("第一条请求应打 current（实测可用），实际 %q ——\n"+
			"balance 实测返回 400 parameter error，先打它等于额度永远查不到", paths[0])
	}
	// ② 必须带 app_version（参照实现两个端点都带）。
	if !strings.Contains(paths[0], "app_version=") {
		t.Errorf("应带 app_version 查询参数（参照实现两个端点都带），实际 %q", paths[0])
	}
	// ③ current 成功就不该再打 balance（省一次无用请求，且它必然失败）。
	if len(paths) != 1 {
		t.Errorf("current 成功后不该再打别的端点，实际打了 %v", paths)
	}
}

// QuotaExt：current 失败时要**回退到 balance**（参照实现的主备语义）。
func TestBillingFallsBackToBalance(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "current") {
			// current 失败 → 应触发兜底。
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":2007,"msg":"server error"}`))
			return
		}
		// balance 这次给真实数据。
		_, _ = w.Write([]byte(`{"code":0,"data":{"balances":[
		  {"remaining_units":777,"show_name":"兜底桶"}]}}`))
	}))
	defer srv.Close()

	p := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), BillingOrigin: srv.URL})
	p.probeOrigin = false
	a := &Auth{Kind: CredKindJWT, JWT: "jwt-x", UID: "fb-user"}
	p.creds[a.UID] = a

	view, ok := p.RefreshQuota(a.UID)
	if !ok || !view.HasData {
		mu.Lock()
		t.Fatalf("current 失败后应回退 balance 并拿到数据；路径: %v", paths)
		mu.Unlock()
	}
	if view.Remaining != 777 {
		t.Errorf("应拿到兜底的 777，实际 %d", view.Remaining)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || !strings.Contains(paths[0], "current") || !strings.Contains(paths[1], "balance") {
		t.Errorf("应先 current 再 balance，实际 %v", paths)
	}
}

// QuotaExt：两条端点都失败 → 如实报 HasData=false（不编 0）。
func TestBillingBothFailReportsNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":3001,"msg":"parameter error"}`))
	}))
	defer srv.Close()

	p := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), BillingOrigin: srv.URL})
	p.probeOrigin = false
	a := &Auth{Kind: CredKindJWT, JWT: "jwt-x", UID: "fail-user"}
	p.creds[a.UID] = a

	view, ok := p.RefreshQuota(a.UID)
	if ok && view.HasData {
		t.Errorf("两条端点都失败时应报 HasData=false（界面 —），实际 %+v", view)
	}
}

// 两个注入点**必须是分开的字段**（我把它们合成一个，结果 404）。
//
// # 这个 bug 的形态很隐蔽，值得专门钉住
//
// 我一度让 billing 复用 `oauthBase`（看起来合理：都是 zcode.z.ai 上的端点），
// 但两个前缀约定不同：
//
//	CLI OAuth  cliOAuthBase = https://zcode.z.ai/api/v1  +  /oauth/cli/init
//	                                                        ↑ 路径**不含**前缀
//	计量端点   planOrigin   = https://zcode.z.ai         +  /api/v1/zcode-plan/…
//	                                                        ↑ 路径**自带**前缀
//
// 复用会拼出 `https://zcode.z.ai/api/v1/api/v1/zcode-plan/billing/current` → 404。
//
// ⚠ 症状特别难查：**不报编译错**，只表现为"额度刷新 failed=1"，
// 日志里只有一个 HTTP 404 —— 看起来像上游改了端点。
// 我是靠"改完之后 updated 从 1 变成 failed=1"的**回归**才发现的，
// 而这原本应该由测试挡住。
func TestOAuthAndBillingOriginsAreSeparateFields(t *testing.T) {
	// ① 两个 base 的默认值**不同**：一个带 /api/v1，一个不带。这正是不能复用的根据。
	if !strings.HasSuffix(cliOAuthBase, "/api/v1") {
		t.Errorf("cliOAuthBase 应带 /api/v1 后缀（其路径不含前缀），实际 %q", cliOAuthBase)
	}
	if strings.HasSuffix(planOrigin, "/api/v1") {
		t.Errorf("planOrigin 不该带 /api/v1（计量路径自带前缀），实际 %q", planOrigin)
	}
	// ② 计量路径自带 /api/v1 —— 所以 base 必须**不含**它。
	if !strings.HasPrefix(planBillingCurrentPath, "/api/v1/") {
		t.Errorf("计量路径应自带 /api/v1 前缀，实际 %q", planBillingCurrentPath)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"remaining_units":1}}`))
	}))
	defer srv.Close()

	// ③ 注入 BillingOrigin 后必须能取到额度。
	p := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), BillingOrigin: srv.URL})
	p.probeOrigin = false
	a := &Auth{Kind: CredKindJWT, JWT: "jwt-x", UID: "u1"}
	p.creds[a.UID] = a
	if view, ok := p.RefreshQuota(a.UID); !ok || !view.HasData {
		t.Fatalf("BillingOrigin 注入后应能取到额度（ok=%v view=%+v）——"+
			"若这里失败，说明 billingBase 又复用了 oauthBase（会拼出 /api/v1/api/v1/… → 404）",
			ok, view)
	}

	// ④ 反过来：只注入 OAuthBase **不该**改变 billing 的指向（两者独立）。
	p2 := New(Config{AuthDir: t.TempDir(), HTTPClient: srv.Client(), OAuthBase: srv.URL})
	p2.probeOrigin = false
	a2 := &Auth{Kind: CredKindJWT, JWT: "jwt-x", UID: "u2"}
	p2.creds[a2.UID] = a2
	if _, ok := p2.RefreshQuota(a2.UID); ok {
		t.Error("只注入 OAuthBase 时 billing 不该被指到那个假上游 —— " +
			"两者是一对独立的注入点，混用会拼出带重复 /api/v1 的 URL")
	}

	// ⑤ **生产默认值**必须拼出正确的 URL（上面几条都注入了假上游，
	//    所以它们挡不住"默认值被改错"）。
	//
	// ⚠ 这条是变异验证逼出来的：我把 `billBase = planOrigin` 改成
	// `cliOAuthBase` 之后，前四条**全部仍然绿** —— 因为它们都注入了
	// 假 origin，默认值根本没被走到。
	// 而那个改动的后果是线上拼出 `/api/v1/api/v1/zcode-plan/…` → 404。
	def := New(Config{AuthDir: t.TempDir()})
	if got := def.client.billingBase(); got != planOrigin {
		t.Errorf("计量端点的默认 base 必须是 planOrigin（%q），实际 %q ——\n"+
			"若这里变成 cliOAuthBase 会拼出 /api/v1/api/v1/… → 404", planOrigin, got)
	}
	// 并且拼出来的完整 URL 里 `/api/v1` 只能出现一次。
	full := def.client.billingBase() + planBillingCurrentPath
	if n := strings.Count(full, "/api/v1"); n != 1 {
		t.Errorf("完整 URL 里 /api/v1 应恰好出现 1 次，实际 %d 次: %s", n, full)
	}
	if !strings.HasPrefix(full, "https://zcode.z.ai/api/v1/zcode-plan/") {
		t.Errorf("完整 URL 形状不对: %s", full)
	}
}

// HealthProbeExt：鉴权失败要报错，额度耗尽**不算**不健康。
//
// # 为什么额度耗尽不算不健康
//
// "额度耗尽"说明这个号是好的，只是这阵子没额度。
// 把它当不健康会让账号池在额度恢复后仍不敢用它 ——
// 那会把一个可用的号永久搁在冷却里。
func TestProbeHealthOnlyFailsOnAuthErrors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"正常", http.StatusOK, `{"choices":[]}`, false},
		{"401 鉴权失败", http.StatusUnauthorized, `{"error":{"code":"1001"}}`, true},
		{"403 鉴权失败", http.StatusForbidden, `{"error":{"code":"1001"}}`, true},
		{"429 限流（号是好的）", http.StatusTooManyRequests, `{"error":{"code":"1302"}}`, false},
		{"402 额度耗尽（号是好的）", http.StatusPaymentRequired, `{"error":{"code":"1113"}}`, false},
		{"500 上游故障（号是好的）", http.StatusInternalServerError, `{"error":{"code":"2007"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			a := &Auth{Kind: CredKindAPIKey, APIKey: "k", UID: "probe-user"}
			p := newTestProvider(t, f, a)

			err := p.ProbeHealth(context.Background(), credOf(a))
			if tc.wantErr && err == nil {
				t.Errorf("HTTP %d 应报不健康", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("HTTP %d 不该报不健康（号是好的）: %v", tc.status, err)
			}
		})
	}
}

// SoftRateExt：业务码形态的限流必须被识别。
//
// # 这是本上游最要紧的一个扩展点
//
// 上游的失败可能是 HTTP 200/4xx + body 里的业务码。出口层默认只看
// HTTP 状态码，于是"3012 风控"会被记成**成功** —— 那正是本仓
// 修 TRAE 时踩过的同一个坑：失败能被发现，假成功会一直骗到用户
// 自己去核对额度。
func TestSoftRateRecognizesBusinessCodes(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	cases := []struct {
		name    string
		status  int
		body    string
		wantOK  bool
		wantMin time.Duration // 冷却至少这么久
	}{
		{
			name: "3012 风控（HTTP 200！）", status: http.StatusOK,
			body:   `{"error":{"code":"3012","message":"unusual activity"}}`,
			wantOK: true, wantMin: time.Minute,
		},
		{
			name: "3012 风控（Anthropic 形态，码在 type）", status: http.StatusOK,
			body:   `{"error":{"message":"unusual activity","type":"3012"}}`,
			wantOK: true, wantMin: time.Minute,
		},
		{
			name: "3008 并发上限", status: http.StatusOK,
			body: `{"error":{"code":"3008"}}`, wantOK: true, wantMin: time.Second,
		},
		{
			name: "1302 限流", status: http.StatusOK,
			body: `{"error":{"code":"1302"}}`, wantOK: true, wantMin: time.Second,
		},
		{
			name: "529 过载（HTTP 层）", status: 529,
			body: `{"overloaded":true}`, wantOK: true, wantMin: time.Second,
		},
		{
			name: "429（HTTP 层）", status: http.StatusTooManyRequests,
			body: `{"error":{"code":"1303"}}`, wantOK: true, wantMin: time.Second,
		},
		{
			name: "正常成功 —— 不该被判成限流", status: http.StatusOK,
			body: `{"choices":[{"message":{"content":"ok"}}]}`, wantOK: false,
		},
		{
			name: "额度耗尽 —— 是换号不是限流等待", status: http.StatusOK,
			body: `{"error":{"code":"1005"}}`, wantOK: false,
		},
		{
			name: "模型不存在 —— 重试无意义", status: 400,
			body: `{"error":{"code":"3006"}}`, wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, ok := p.SoftRateReset(tc.status, tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v，期望 %v（status=%d body=%s）", ok, tc.wantOK, tc.status, tc.body)
			}
			if !ok {
				return
			}
			if at.Before(time.Now()) {
				t.Errorf("重置时刻在过去（%v）—— 冷却会立刻失效", at)
			}
			if d := time.Until(at); d < tc.wantMin {
				t.Errorf("冷却只有 %v，至少应 %v（太短会反复撞同一堵墙）", d, tc.wantMin)
			}
		})
	}
}

// IsTerminal：终止类业务码不该被重试。
func TestIsTerminalClassification(t *testing.T) {
	f := newFakeUpstream(t, nil)
	p := newTestProvider(t, f)

	// 终止类（重试只是重复失败，还会白烧额度）。
	for _, code := range []string{"3007", "1005", "1006", "1113", "3006", "1261", "1304"} {
		if !p.IsTerminal(200, `{"error":{"code":"`+code+`"}}`) {
			t.Errorf("业务码 %s 应为终止类", code)
		}
	}
	// 可重试类（过早摘号会浪费好号）。
	for _, code := range []string{"1234", "1302", "1312", "2007", "3008", "3012"} {
		if p.IsTerminal(200, `{"error":{"code":"`+code+`"}}`) {
			t.Errorf("业务码 %s 不该是终止类", code)
		}
	}
	// 未知码不算终止（保守：宁可重试也不要误摘好号）。
	if p.IsTerminal(200, `{"error":{"code":"9999"}}`) {
		t.Error("未知业务码不该被判成终止")
	}
}

// ImportCredentials 的产物必须能落盘并被 LoadDir 读回来。
//
// 这条把"导入"与"加载"两端接起来 —— 单测任一端都可能漏掉
// "导入产出的文件名与加载的扫描规则不匹配"这类问题。
func TestImportThenLoadRoundTrip(t *testing.T) {
	f := newFakeUpstream(t, nil)
	dir := t.TempDir()
	p := New(Config{Origin: f.srv.URL, AuthDir: dir, HTTPClient: f.srv.Client()})

	out, err := p.ImportCredentials("zai:cp:mykey.secret\nbigmodel:other.key")
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("应导入 2 条，实际 %d", len(out))
	}
	// 按导入产出的文件名与内容落盘（模拟 admin 的行为）。
	for _, ic := range out {
		if err := writeFile(dir, ic.FileName, ic.Raw); err != nil {
			t.Fatalf("落盘 %s 失败: %v", ic.FileName, err)
		}
	}
	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("回读应得 2 条，实际 %d（文件名与扫描规则不匹配？）", len(list))
	}
	// 核验导入时的选项被保住了。
	var sawCP, sawBigModel bool
	for _, a := range list {
		if a.CodingPlan && a.Origin == originZAI {
			sawCP = true
		}
		if a.Origin == originBigModel {
			sawBigModel = true
		}
	}
	if !sawCP {
		t.Error("Coding Plan 标记在落盘/回读中丢了（会让请求打到错误端点）")
	}
	if !sawBigModel {
		t.Error("BigModel 平台标记在落盘/回读中丢了（会让请求打到错误平台）")
	}
}

// stream 字段的探测（决定走 SSE 还是 JSON 转换路径）。
func TestDetectStream(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"stream":true}`, true},
		{`{"stream":false}`, false},
		{`{"stream":"true"}`, false}, // 字符串不算 —— OpenAI 用布尔
		{`{}`, false},
		{`not json`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := detectStream([]byte(tc.body)); got != tc.want {
			t.Errorf("detectStream(%q) = %v，期望 %v", tc.body, got, tc.want)
		}
	}
}

// JWT 通道缺验证码求解器时**必须如实报错**，不能静默去掉验证码头。
//
// # 为什么不能静默降级
//
// 不带验证码的请求会被上游当风控（3012）—— 那比明确报错
// 难查得多：用户看到的是"偶发的风控"，而真因是我们偷偷少发了一个头。
func TestJWTWithoutCaptchaFailsLoudly(t *testing.T) {
	f := newFakeUpstream(t, nil)
	a := &Auth{Kind: CredKindJWT, JWT: "jwt-value", UID: "jwt-user", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	p := newTestProvider(t, f, a) // 未注入 Captcha

	_, err := p.Chat(context.Background(), credOf(a),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err == nil {
		t.Fatal("JWT 通道缺验证码求解器时应报错")
	}
	if !strings.Contains(err.Error(), "验证码") {
		t.Errorf("错误信息应说清是验证码问题，实际: %v", err)
	}
	if n := atomic.LoadInt32(&f.calls.chat); n != 0 {
		t.Errorf("不该把无验证码的请求发出去，实际发了 %d 次", n)
	}
}

// 假 CaptchaSolver：验证缓存与并发去重。
func TestCachedSolverCachesAndDeduplicates(t *testing.T) {
	var calls int32
	inner := &countingSolver{calls: &calls}
	s := NewCachedSolver(inner)

	// 第一次求解。
	p1, err := s.Solve(context.Background(), "cn")
	if err != nil {
		t.Fatalf("首次求解失败: %v", err)
	}
	// 第二次应命中缓存（不再调用内部求解器）。
	p2, err := s.Solve(context.Background(), "cn")
	if err != nil {
		t.Fatalf("二次求解失败: %v", err)
	}
	if p1 != p2 {
		t.Errorf("缓存未命中：%q vs %q", p1, p2)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("应只求解 1 次（其余走缓存），实际 %d 次", n)
	}

	// 区域变了必须重新求解（区域要与签发时一致）。
	if _, err := s.Solve(context.Background(), "sg"); err != nil {
		t.Fatalf("换区域求解失败: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("换区域应重新求解，实际 %d 次", n)
	}
}

// countingSolver 记录求解次数。
type countingSolver struct{ calls *int32 }

func (c *countingSolver) Solve(ctx context.Context, region string) (string, error) {
	n := atomic.AddInt32(c.calls, 1)
	return "param-" + string(rune('0'+n)) + "-" + region, nil
}

// 求解失败要如实上报（不能返回空串当成成功）。
func TestCachedSolverPropagatesError(t *testing.T) {
	s := NewCachedSolver(&failingSolver{})
	if _, err := s.Solve(context.Background(), "cn"); err == nil {
		t.Error("求解失败应返回 error")
	}
	if s := NewCachedSolver(nil); s != nil {
		t.Error("nil 求解器应返回 nil（而不是一个会 panic 的包装）")
	}
}

type failingSolver struct{}

func (f *failingSolver) Solve(ctx context.Context, region string) (string, error) {
	return "", errors.New("solver boom")
}

// writeFile 测试助手：落盘一个文件（模拟 admin 写凭证的行为）。
func writeFile(dir, name string, raw []byte) error {
	return os.WriteFile(filepath.Join(dir, name), raw, 0o600)
}
