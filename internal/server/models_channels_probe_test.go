// models_channels_probe_test.go —— 模型目录页两个行内动作端点（T-02）。
//
// # 这三个 seam 为什么值得测（各自的"能抓到什么 / 漏掉什么"）
//
//	① POST /ui/models/provider/refresh —— 只回源**指定**那一个上游
//	   能抓到：把全量刷新误接进来（顺手刷了别的上游）；把缓存直接删掉（那不叫刷新）。
//	   漏掉：同一个上游内部"新目录是否真的覆盖旧目录"的细节（由 storeProviderModels 保证）。
//
//	② POST /ui/models/provider/probe —— 上游 error.code 原样透传
//	   能抓到：把 request_not_authorized / model_not_found 折成 502 / "未知错误"。
//	   漏掉：上游用非 JSON 体报错时的文案（只保证状态码与原文片段）。
//
//	③ POST /ui/models/provider/probe —— 5 秒内同一目标只打一次上游
//	   能抓到：连点把上游打进风控冷却窗（实测 403 窗口 2–4 分钟）。
//	   漏掉：并发点击的竞态（用串行两次点击覆盖典型路径）。
//
// 桩只在 HTTP 边界与 ProviderRouter 上做（都是本包声明的接口），不 mock 内部协作者。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
)

const providerActionsToken = "tkn-provider-actions"

// countingRouter 在 testRouter 之上加两件事：**调用计数**与**可控的上游响应**。
//
// 为什么用嵌入而不是重写：ProviderRouter 的方法集很长（Has/Default/Models/
// ModelMultipliers/Chat/Credential/RefreshCredential/RefreshSkew/SoftRateReset/
// ResetAt/Classify），重写会有十来个"为了编译"的空实现 —— 那些空实现一旦
// 有人真去用，测试就会**假绿**（本仓在 models_multiprovider_test.go 里
// 已经为这件事写过一段注释）。嵌入则只覆盖本次真正需要观察的两个方法。
type countingRouter struct {
	testRouter
	mu        sync.Mutex
	modelCall map[string]int
	chatCall  int
	chatSt    int
	chatBody  string
}

var _ ProviderRouter = (*countingRouter)(nil)

func newCountingRouter() *countingRouter {
	return &countingRouter{testRouter: newTestRouter(false), modelCall: map[string]int{}}
}

func (r *countingRouter) Models(ctx context.Context, id string) ([]gateway.ModelInfo, bool) {
	r.mu.Lock()
	r.modelCall[id]++
	r.mu.Unlock()
	return r.testRouter.Models(ctx, id)
}

func (r *countingRouter) Chat(_ context.Context, _ string, _ gateway.Credential, _ []byte) (gateway.ChatStream, bool, error) {
	r.mu.Lock()
	r.chatCall++
	st, body := r.chatSt, r.chatBody
	r.mu.Unlock()
	if st == 0 {
		st = http.StatusOK
	}
	return gateway.ChatStream{Status: st, Body: io.NopCloser(strings.NewReader(body))}, true, nil
}

func (r *countingRouter) resetCounts() {
	r.mu.Lock()
	r.modelCall = map[string]int{}
	r.chatCall = 0
	r.mu.Unlock()
}

func (r *countingRouter) modelCallsOf(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.modelCall[id]
}

func (r *countingRouter) chatCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.chatCall
}

// Credential 必须真的给一份（哪怕内容为空）：探测的链路是
// "选号 → 取凭证 → 发请求"，凭证取不到就会在**发上游之前**返回，
// 那样本文件的三个 seam 一个都测不到（会假绿成"哦，探测是通的"）。
func (r *countingRouter) Credential(_, _ string) (gateway.Credential, bool) {
	return gateway.Credential{}, true
}

// providerActionsHandler 一个"两个上游目录都已就绪、管理令牌可用、且有号可挑"的 Handler。
//
// ⚠ SetDefaultProvider 必须调：池子的 provider 域是从这里注入的（与 cmd/server 同一手法）。
// 少了这一步，PickFor("workbuddy", ...) 会因为"账号不属于这个域"而回 nil，
// 探测会在**发上游之前**就返回 no_healthy_account —— 三个 seam 全部测不到（假绿）。
func providerActionsHandler(t *testing.T, r *countingRouter) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetDefaultProvider("workbuddy")
	return NewHandler(Config{
		Pool:            p,
		Upstream:        newFakeUpstream(t, func(string) (int, string, bool) { return 200, baselineDynamicBody(), false }),
		Provider:        r,
		DefaultProvider: "workbuddy",
		OwnedBy:         "workbuddy",
		MaxBodyMB:       1,
		AdminToken:      providerActionsToken,
	})
}

// postJSON 带管理令牌发一次 JSON 请求，返回 (状态码, 解析后的体, 原始体)。
func postJSON(t *testing.T, h *Handler, path string, body any, withAuth bool) (int, map[string]any, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		rdr = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(http.MethodPost, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+providerActionsToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// warmCatalogForActions 先让两个上游的目录都进缓存（否则"刷新只碰一个"无法与"冷启动必须取数"区分）。
func warmCatalogForActions(t *testing.T, h *Handler) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/ui/models/data", nil)
	req.Header.Set("Authorization", "Bearer "+providerActionsToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("预热 GET /ui/models/data = %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestProviderRefreshOnlyTouchesOneVendor 是 Q7 的第一条证伪命令。
//
// 判据用**上游被调用的次数**（fake 计数），不用缓存时间戳：
// 时间戳是内部状态，而"另外那个上游一次都没被问过"才是用户看得见的行为差异
// （它的目录/新鲜度不可能变，因为它根本没被触碰）。
func TestProviderRefreshOnlyTouchesOneVendor(t *testing.T) {
	r := newCountingRouter()
	h := providerActionsHandler(t, r)
	warmCatalogForActions(t, h)
	if r.modelCallsOf("codearts") == 0 {
		t.Fatalf("预热阶段 codearts 应当被问过一次（否则本用例的基线不成立）")
	}
	r.resetCounts()

	code, body, raw := postJSON(t, h, "/ui/models/provider/refresh", map[string]any{"provider": "codearts"}, true)
	if code != http.StatusOK {
		t.Fatalf("刷新 codearts 应 200，实际 %d: %s", code, raw)
	}
	if got := r.modelCallsOf("codearts"); got != 1 {
		t.Fatalf("刷新 codearts 应当只回源 1 次，实际 %d 次", got)
	}
	if got := r.modelCallsOf("workbuddy"); got != 0 {
		t.Fatalf("★ 刷新 codearts 却碰了 workbuddy %d 次 —— 「只影响一行」被破坏", got)
	}
	// count 的真源是桩里写死的模型条数（不是我按实现算出来的值）。
	want := len(r.testRouter.models["codearts"])
	if got, _ := body["count"].(float64); int(got) != want {
		t.Fatalf("count 应为 codearts 的模型数 %d，实际 %v（body=%s）", want, body["count"], raw)
	}
	if body["source"] != "upstream" {
		t.Fatalf("source 应为 upstream，实际 %v", body["source"])
	}
}

// TestProbePassthroughUpstreamCode 是 Q7 的第二条证伪命令，也是本次调试的**教训固化**。
//
// 背景（真实代价）：上游用 `403 + {"error":{"code":"request_not_authorized"}}`
// 表达"这个请求没被授权"，而中间层把它折成了"502 空消息"，
// 排查方向因此偏了几小时。所以这条断言不是在测 JSON 解析，
// 而是在测「我们**没有**把上游的判据吞掉」。
func TestProbePassthroughUpstreamCode(t *testing.T) {
	const upstreamBody = `{"error":{"code":"request_not_authorized","message":"当前请求不可用，请更新或重新打开客户端后重试。"}}`
	r := newCountingRouter()
	r.chatSt = http.StatusForbidden
	r.chatBody = upstreamBody
	h := providerActionsHandler(t, r)
	clearProbeCache(t)

	code, body, raw := postJSON(t, h, "/ui/models/provider/probe",
		map[string]any{"provider": "workbuddy", "model": "glm-5.2"}, true)
	// 探测是**测量**：上游报错也是"测到了"，所以 HTTP 必须 200。
	if code != http.StatusOK {
		t.Fatalf("探测失败也应当 HTTP 200（测量语义），实际 %d: %s", code, raw)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Fatalf("上游 403 时 ok 必须为 false，实际 %v（body=%s）", body["ok"], raw)
	}
	if got, _ := body["status"].(float64); int(got) != http.StatusForbidden {
		t.Fatalf("status 必须是上游原始状态码 403，实际 %v（body=%s）", body["status"], raw)
	}
	// ★ 硬要求：上游 code 原文。
	if body["code"] != "request_not_authorized" {
		t.Fatalf("★ 上游 error.code 没有被透传（期望 request_not_authorized，实际 %v）—— body=%s", body["code"], raw)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "当前请求不可用") {
		t.Fatalf("message 应当带上游原文，实际 %q", msg)
	}
	if r.chatCalls() != 1 {
		t.Fatalf("应当恰好打 1 次上游，实际 %d", r.chatCalls())
	}
}

// clearProbeCache 清掉探测去重表。
//
// 为什么要显式清：probeCache 是包级状态（去重窗口必须跨请求生效），
// 而测试之间共享这个进程 —— 不清的话，上一个用例缓存下来的
// "workbuddy/glm-5.2 → 403" 会让下一个用例拿到回放结果，
// 于是它测的其实是**上一个用例的**结论（最难查的一类假绿）。
func clearProbeCache(t *testing.T) {
	t.Helper()
	probeCache.Lock()
	probeCache.entries = nil
	probeCache.Unlock()
	t.Cleanup(func() {
		probeCache.Lock()
		probeCache.entries = nil
		probeCache.Unlock()
	})
}

// TestProbeDedupedWithinFiveSeconds 是 Q7 的第三条证伪命令。
//
// 为什么这条规则有产品含义（不是防抖洁癖）：上游对密集请求会进入
// **2–4 分钟的 403 冷却窗**（本次真机实测：连点十几次后，连已知能成功的
// 配方也开始 403，冷却 32–35 秒后恢复）。按钮很容易被连点，
// 所以"5 秒内同目标只打一次上游"是保护**用户额度与可用性**的规则。
func TestProbeDedupedWithinFiveSeconds(t *testing.T) {
	r := newCountingRouter()
	r.chatSt = http.StatusOK
	r.chatBody = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	h := providerActionsHandler(t, r)
	clearProbeCache(t)

	target := map[string]any{"provider": "workbuddy", "model": "glm-5.2"}
	for i := 0; i < 3; i++ {
		code, body, raw := postJSON(t, h, "/ui/models/provider/probe", target, true)
		if code != http.StatusOK {
			t.Fatalf("第 %d 次探测应 200，实际 %d: %s", i+1, code, raw)
		}
		if ok, _ := body["ok"].(bool); !ok {
			t.Fatalf("第 %d 次探测 ok 应为 true，实际 %v（body=%s）", i+1, body["ok"], raw)
		}
	}
	if got := r.chatCalls(); got != 1 {
		t.Fatalf("★ 5 秒内连点 3 次应当只打 1 次上游，实际 %d 次 —— 连点会打出上游的风控冷却窗", got)
	}
}

// TestProviderActionsRequireUIIdentity 两条新端点必须与 /ui/models/data 同一套身份判据。
//
// 为什么单独测：这两条是**写入面**（会打上游、会覆盖缓存快照）。
// 一旦有人把它们挪到 /admin/* 或忘了 uiIdentify，页面上仍是 401（看起来"正常"），
// 而真正危险的是另一种改法：把判据删掉——那时它们变成匿名可打的写端点。
func TestProviderActionsRequireUIIdentity(t *testing.T) {
	r := newCountingRouter()
	h := providerActionsHandler(t, r)
	for _, path := range []string{"/ui/models/provider/refresh", "/ui/models/provider/probe"} {
		code, _, raw := postJSON(t, h, path, map[string]any{"provider": "workbuddy", "model": "glm-5.2"}, false)
		if code != http.StatusUnauthorized {
			t.Fatalf("匿名打 %s 应 401，实际 %d: %s", path, code, raw)
		}
	}
	if r.modelCallsOf("workbuddy") != 0 || r.chatCalls() != 0 {
		t.Fatalf("未认证请求不该触达上游（models=%d chat=%d）",
			r.modelCallsOf("workbuddy"), r.chatCalls())
	}
}

// TestModelsPageHasProviderActionColumns 页面结构钩子（e2e 不读源码也能验）。
//
// 断的是 **data-act / data-actres** 而不是按钮文案：文案会改（中英文/措辞），
// 钩子是契约。同时断言两个端点路径出现在页面里 —— 前端与后端路由同源，
// 任何一边改名另一边没跟上，这条会立刻红。
func TestModelsPageHasProviderActionColumns(t *testing.T) {
	page := string(modelsUIPage)
	for _, hook := range []string{
		`setAttribute('data-act', 'refresh')`,
		`setAttribute('data-act', 'probe')`,
		`setAttribute('data-act', 'probe-go')`,
		`setAttribute('data-actres', kind)`,
		"/ui/models/provider/refresh",
		"/ui/models/provider/probe",
	} {
		if !strings.Contains(page, hook) {
			t.Fatalf("模型页缺少契约钩子 %q —— 前端与后端路由/行为已经分叉", hook)
		}
	}
}
