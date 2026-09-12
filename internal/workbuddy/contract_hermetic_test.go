package workbuddy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/upstream"
)

// 让 `gateway.RunProviderContract` **在 CI 里真的跑起来** —— 阶段 0 评审 F4。
//
// # 问题
//
// 判据 2 声称"新上游必须通过契约测试"，但在唯一的自动化环境里
// **契约从未被执行**：
//
//	TestContractNoCredential   手写了一个子集，**不调用** RunProviderContract
//	TestContractWithCredential 调了它，但无 token 时 t.Skip
//	.github/workflows/ci.yml   跑 `go test ./...`，没有 token
//
// 于是"必须通过契约"实际由**手写的重复断言**保证，而手写断言会与契约各自漂移。
// 这是判据 2 最实质的漏洞：契约存在，但没有 enforcement。
//
// # 修法
//
// 用 `httptest` 起一个假上游，把 `upstream.Client` 的 base URL 指过去。
// 于是**无网络、无真实凭证**也能让契约跑满全程（含所有需凭证的分支），
// CI 里 `go test ./...` 就跑得到真正的契约。
//
// （本文件曾在并发委派中被 `git stash` 冲掉过一次；已重做。）

// newContractUpstreamClient 造一个指向假上游的 upstream.Client。
//
// 只改 base URL，不动其它配置（超时等）—— 契约验的是 Provider 的行为，
// 不是 HTTP 客户端的参数。
func newContractUpstreamClient(base string) *upstream.Client {
	c := upstream.New()
	c.ChatBaseCN = base
	return c
}

// fakeContractUpstream 起一个最小的假上游，覆盖契约会打到的端点。
func fakeContractUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// 模型目录：契约要求"声明了 CapModels 就必须返回非空列表"。
	//
	// ⚠ 形状必须与 upstream.Client.FetchModels 的解析**精确匹配**：
	// 它只在 agents 里找 name=="cli" 的那一项，用它的 models 列表筛选；
	// 被选中的模型还必须出现在 models 里且 disabled=false。
	//
	// 第一版我漏了 agents，契约报 "no cli agent models found" ——
	// 那正是契约在正常工作的证据（它真的调到了 Models 并检查了结果）。
	mux.HandleFunc("/console/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{
			"models":[
				{"id":"fake-model-a","name":"A","maxInputTokens":128000,"maxOutputTokens":8192,"disabled":false},
				{"id":"fake-model-b","name":"B","maxInputTokens":64000,"maxOutputTokens":4096,"disabled":false}
			],
			"agents":[{"name":"cli","models":["fake-model-a","fake-model-b"]}]
		}}`)
	})

	// 对话：返回结构合法的 SSE，让契约能读到底并 Close。
	mux.HandleFunc("/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	// 兜底：未覆盖的路径返回 200 空体，避免契约因无关端点失败。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"msg":"ok","data":{}}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newContractProvider 造一个指向假上游的 Provider。
func newContractProvider(base string) gateway.Provider {
	p := NewWithConfig(Config{})
	p.SetClient(newContractUpstreamClient(base))
	return p
}

// newContractCredential 契约用的凭证（假上游，任何 token 都"有效"）。
func newContractCredential(uid string) gateway.Credential {
	return gateway.Credential{
		Provider: providerID,
		UID:      uid,
		Secret: &auth.Auth{
			UID:          uid,
			AccessToken:  "fixture-token",
			RefreshToken: "fixture-refresh",
		},
	}
}

// TestContract 契约测试 —— **判据 2 的正式入口**。
//
// 用假上游让它无需真实凭证即可全程执行，因此 **CI 里不会跳过**。
func TestContract(t *testing.T) {
	srv := fakeContractUpstream(t)
	gateway.RunProviderContract(t, func() gateway.Provider {
		return newContractProvider(srv.URL)
	}, gateway.WithCredential(newContractCredential("contract-fixture")))
}

// TestContractAgainstRealUpstream 可选：有真实 token 时也跑一遍**真实**上游。
//
// 与上一条的关系：上一条保证"契约在每个 PR 上都被执行"（假上游、确定性）；
// 这条保证"契约对真实上游也成立"（真凭证、按需）。
// 两条都要有 —— 只有前者会漏掉"假上游与真上游行为不一致"；
// 只有后者会让 CI 里根本没有契约执行。
func TestContractAgainstRealUpstream(t *testing.T) {
	tok := os.Getenv("WORKBUDDY_TEST_TOKEN")
	if tok == "" {
		t.Skip("无 WORKBUDDY_TEST_TOKEN —— 跳过针对真实上游的契约。" +
			"注意：契约本身已在 TestContract 里用假上游执行，不会被跳过。")
	}
	gateway.RunProviderContract(t, New, gateway.WithCredential(gateway.Credential{
		Provider: providerID,
		UID:      "contract-real",
		Secret:   &auth.Auth{AccessToken: tok},
	}))
}

// TestContractIsNotConditionallySkipped 把 F4 的教训固化下来。
//
// 若有人把 TestContract 改回"无 token 就 skip"，这条会立刻红。
func TestContractIsNotConditionallySkipped(t *testing.T) {
	t.Setenv("WORKBUDDY_TEST_TOKEN", "") // 明确清空：契约不该依赖它
	srv := fakeContractUpstream(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		gateway.RunProviderContract(t, func() gateway.Provider {
			return newContractProvider(srv.URL)
		}, gateway.WithCredential(newContractCredential("no-env-fixture")))
	}()
	select {
	case <-done:
		// 契约在无 token 环境下完整跑完 —— 正是要保证的
	case <-time.After(60 * time.Second):
		t.Fatal("契约在无 token 环境下未完成 —— CI 里会变成永远挂住")
	}
}
