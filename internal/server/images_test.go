package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestImagesGenerationsRouteRegistered 路由必须真的挂上了。
//
// # 为什么单独一条（而不是靠端到端跑通）
//
// "方法写好了但忘了注册路由"是本仓出现过的形态。表现是 404 ——
// 而那与"客户端路径写错了"无法区分（两边都是 404）。这条直接断言
// mux 认得这条路径：未接线时应当是言明的 503 not_ready，不是 404。
func TestImagesGenerationsRouteRegistered(t *testing.T) {
	h := NewHandler(Config{})
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"m","prompt":"p"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Fatal("POST /v1/images/generations 返回 404 —— 路由没注册")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("未接线时状态码 = %d，want 503；body=%s", w.Code, w.Body.String())
	}
	// 未接线必须**说清**是没接线，而不是一个笼统的错误 ——
	// nil Pool 时不判空就会 panic（表现为 500 + 空响应）。
	if !strings.Contains(w.Body.String(), "not_ready") {
		t.Errorf("未接线时应报 not_ready，实际: %s", w.Body.String())
	}
}

// TestImagesGenerationsEntryValidation 入口校验：这三类请求在**本地**就该被拒。
//
// 它们共同的道理：发到上游是**确定性的浪费** ——
//
//	缺 prompt      必然失败，却要先打一次上游（实测一次 20+ 秒）
//	非法 JSON      上游也会拒，但错误信息不如本地精确（少了字段位置）
//	未知前缀       是客户端写错了，不该被当成"服务端暂时不可用"去重试
func TestImagesGenerationsEntryValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"缺 prompt", `{"model":"doubao-seedream-5-lite"}`, http.StatusBadRequest},
		{"prompt 全空白", `{"model":"doubao-seedream-5-lite","prompt":"   "}`, http.StatusBadRequest},
		{"空对象", `{}`, http.StatusBadRequest},
		{"非法 JSON", `{"model":`, http.StatusBadRequest},
		{"未知上游前缀", `{"model":"nosuch/doc","prompt":"p"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newImagesTestHandler(t, nil)
			req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("状态码 = %d，want %d；body=%s", w.Code, tc.want, w.Body.String())
			}
			// 错误必须是 OpenAI 形状的信封（与 chat 出口一致），
			// 否则调用方要按端点写两套错误解析。
			var env struct {
				Error struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("错误响应不是 OpenAI 信封: %s", w.Body.String())
			}
			if env.Error.Message == "" {
				t.Errorf("错误信封里没有 message: %s", w.Body.String())
			}
		})
	}
}

// TestImagesGenerationsNoAccountIs503 池里没号时必须是 503 且带可读提示。
//
// 与 chat 出口同一条语义：没有可用账号是**服务端**的状况，
// 调用方应当稍后重试（而不是以为请求写错了去改参数）。
//
// ⚠ 断言 503 而不是 501 很重要：两者对调用方的含义完全相反 ——
//
//	503 没有可用账号   → 等一会儿再来（它会自己好）
//	501 上游不支持生图 → 永远别再来（重试一亿次也不会好）
//
// 把"暂时没号"报成"不支持"会让调用方永久放弃一个完全正常的功能。
func TestImagesGenerationsNoAccountIs503(t *testing.T) {
	// 空池：Provider 接好了（所以能过 requireReady），但一个号都没有。
	h := NewHandler(Config{
		Pool:            emptyTestPool(t),
		Provider:        &imageRouter{hasGen: true},
		DefaultProvider: "loomy",
	})
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，want 503（没号不该报成不支持）；body=%s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no_healthy_account") {
		t.Errorf("应当报 no_healthy_account，实际: %s", w.Body.String())
	}
}

// TestImagesGenerationsUnsupportedUpstreamIs501 上游没实现生图时明确 501。
//
// ⚠ 这里刻意**不回落**别的上游，也**不**回 503。
//
//	回落 → "生图请求打到了另一个上游"变成一个静默的错图（最难查的形态）
//	503  → 调用方会重试，而重试一亿次也不会变得支持生图
//
// 501 才是"这个功能在这条路径上不存在"的准确表达。
func TestImagesGenerationsUnsupportedUpstreamIs501(t *testing.T) {
	// 上游有号，但它的 Provider 没实现 ImageGenExt。
	h := newImagesTestHandler(t, &imageRouter{hasGen: false})
	req := httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"doubao-seedream-5-lite","prompt":"一只猫"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，want 501；body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "image_not_supported") {
		t.Errorf("应当报 image_not_supported，实际: %s", w.Body.String())
	}
}

// TestImageModelsAppearInCatalog 生图模型必须进 /v1/models。
//
// # 为什么这条是整条链路的关键
//
// 上游的对话目录**不含**这两个生图模型（它们走 chat 会 404）。
// 若目录里没有它们，调用方就不知道能生图、也不知道该填哪个 model ——
// 端点实现了也没人会用。所以"进目录"与"端点可用"是同一件事的两半。
func TestImageModelsAppearInCatalog(t *testing.T) {
	img := []gateway.ImageModel{
		{ID: "doubao-seedream-5-lite", Name: "Doubao seedream 5 lite"},
		{ID: "qwen-image-3.0-pro", Name: "Qwen Image 3.0 Pro"},
	}
	h := newImagesTestHandler(t, &imageRouter{hasModels: true, genModels: img})

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/models 状态码 = %d；body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"doubao-seedream-5-lite", "qwen-image-3.0-pro"} {
		if !strings.Contains(body, want) {
			t.Errorf("生图模型 %s 没进 /v1/models —— 调用方不会知道能用它生图", want)
		}
	}
	// 必须标出生图能力：调用方据此知道该用 /v1/images/generations
	//（靠名字猜"seedream 是生图"是脆的，程序不该那么做）。
	if !strings.Contains(body, "image_generation") {
		t.Error("生图模型条目缺少 capabilities=image_generation 标记")
	}
	// 两条都要带前缀（多上游模式）：前缀是网关的路由记号，
	// 调用方要把它填进 model 才能路由到正确上游。
	if !strings.Contains(body, "loomy/doubao-seedream-5-lite") {
		t.Errorf("生图条目应当带上游前缀 loomy/；body=%s", body)
	}
}

// TestImageModelsAbsentWhenNotImplemented 没实现 ImageModelExt 时目录不变。
//
// 这是**向后兼容**的判据：既有上游（workbuddy/codearts/…）都没实现它，
// 它们的 /v1/models 必须与改造前逐字一致（不多出任何条目）。
func TestImageModelsAbsentWhenNotImplemented(t *testing.T) {
	h := newImagesTestHandler(t, &imageRouter{})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "image_generation") {
		t.Error("未实现 ImageModelExt 的上游不该多出生图条目")
	}
}

// ---- 测试替身 ----
//
// # 为什么嵌入 multiRouter 而不是自己再写一个完整桩
//
// `ProviderRouter` 有 11 个方法，而本文件只关心其中两件（Models / ImageModels）。
// 手写一个实现要抄 11 个方法 —— 抄错了（签名差一点）是**静默**不满足接口，
// 而桩里那些方法与生图无关，抄它们没有任何信息量。
//
// 嵌入已有的 `multiRouter`（chat_multiprovider_test.go，已实现全部 11 个）
// 只覆盖需要改的那一个方法：判据集中、改动最小。

// newImagesTestHandler 造一个"已接线"的 handler：
//
//	Pool      有号（uid=img-1），能过选号
//	Provider  由 r 决定（nil 时用一个不含生图能力的桩）
//
// 用 pool.SyncToDirWithSecrets 而不是 Add：前者是"按 provider 分域入池"的
// 唯一入口，Add 的 provider 标签是默认上游 —— 那会让
// PickFor("loomy", ...) 选不到号，用例退化成"没号"而假绿。
func newImagesTestHandler(t *testing.T, r ProviderRouter) *Handler {
	t.Helper()
	p := pool.New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.SetDefaultProvider("loomy")
	p.SetMaxInFlight(10)
	a := &auth.Auth{UID: "img-1", Nickname: "生图号"}
	p.SyncToDirWithSecrets("loomy", []*auth.Auth{a}, map[string]any{"img-1": &struct{}{}})
	p.SetCredits("img-1", 100000)

	if r == nil {
		// 默认桩：上游存在但**没有**生图可选能力。
		r = &imageRouter{}
	}
	// ⚠ 把内嵌 multiRouter 的身份补齐。
	//
	// 这是本测试替身的一个**易错点**（实测踩过）：`imageRouter` 嵌的是
	// `multiRouter`，而它的 `def`/`reg`/`p` 三个字段默认是零值 ——
	// 只写 `&imageRouter{hasModels:true, genModels:…}` 时 `def` 是空串，
	// 于是 `ImageModels("loomy")` 因 `id != r.def` 返回 ok=false，
	// **目录里就没有生图条目**。而失败信息看起来像"生图功能没接上"，
	// 与真实原因（桩没配身份）完全无关 —— 一次典型的自造假红。
	if ir, ok := r.(*imageRouter); ok {
		ir.def = "loomy"
		ir.p = p
		ir.reg = map[string]*recordingProvider{"loomy": {id: "loomy"}}
	}
	// ⚠ Upstream 必须注入：`/v1/models` 的构建会走 fetchDynamicModels，
	// 那里对 nil Upstream 直接解引用（测试里表现为 panic，不是一条失败断言）。
	// 这是既有实现的既有行为，不是本次引入的 —— 用 wrapUpstream 提供一个
	// 指向不可达地址的真客户端：拉取会失败并**回落到静态表**，
	// 而这正是我们要测的路径（生图条目是静态补进来的）。
	return NewHandler(Config{
		Pool:            p,
		Provider:        r,
		DefaultProvider: "loomy",
		Upstream:        wrapUpstream(upstream.New()),
	})
}

// emptyTestPool 一个**没有账号**的池子。
//
// 用来把"暂时没号"与"上游不支持"这两种失败分开断言 ——
// 它们对调用方的含义完全相反（见 TestImagesGenerationsNoAccountIs503）。
func emptyTestPool(t *testing.T) *pool.Pool {
	t.Helper()
	p := pool.New("")
	p.SetDefaultProvider("loomy")
	p.SetMaxInFlight(10)
	return p
}

// imageRouter 在 multiRouter 之上加生图两个可选能力。
//
// hasGen=false 时模拟"上游没实现 ImageGenExt"（→ 出口层回 501）。
type imageRouter struct {
	multiRouter
	// hasGen 报告是否实现 ImageGenExt
	hasGen bool
	// hasModels 报告是否实现 ImageModelExt
	hasModels bool
	// genModels 是 ImageModels 的返回
	genModels []gateway.ImageModel
}

func (r *imageRouter) ImageModels(id string) ([]gateway.ImageModel, bool) {
	if !r.hasModels || id != r.def {
		return nil, false
	}
	return r.genModels, true
}

func (r *imageRouter) ImageGen(_ context.Context, id, uid string, _ []byte) ([]byte, int, bool, error) {
	if !r.hasGen || id != r.def {
		return nil, 0, false, nil
	}
	return []byte(`{"data":[{"url":"https://example.invalid/x.png"}]}`), http.StatusOK, true, nil
}
