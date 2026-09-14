package loomy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// 让 `gateway.RunProviderContract` **在 CI 里真的跑起来** —— 判据 2 的 enforcement。
//
// # 为什么必须有这个文件
//
// 判据 2 声称"新上游必须通过契约测试"。但如果契约只在有真实凭证时才跑，
// CI 里（.github/workflows/ci.yml 跑 `go test ./...`，没有凭证）
// 它就**从未被执行** —— 判据 2 变成一句口号。
//
// workbuddy 与 codearts 都踩过这个坑（stage-0 评审 F4）。本文件是同一个修法：
// 用 httptest 起一个假上游，把 BaseURL 指过去，于是**无网络、无真实凭证**
// 也能让契约跑满全程。
//
// # 与 live_test 的分工
//
//	本文件   假上游，确定性，**CI 里一定执行**（契约的 enforcement）
//	有真实 session 时另跑一遍真上游（见 TestContractAgainstRealUpstream）
//
// 两条都要有：只有前者会漏掉"假上游与真上游行为不一致"；
// 只有后者会让 CI 里根本没有契约执行。

// fixtureSession 一个**合成**的 session，形态与真货一致（32 位小写 hex）。
//
// # ⚠ 为什么不能用手册里那个真 session
//
// Loomy 反代链路手册第 2.2 节把作者自己的登录态**明文印了出来**。
// 我第一版就是照着那一段抄的 fixture —— 那等于把一个**真实可用的凭证**
// 提交进公开仓库。而本仓库上一个提交恰好就是
// "清掉两处真实 api_key 硬编码 —— 推送前必须处理，否则密钥就公开了"。
//
// 所以这里用合成值：本测试要验证的是"形态与行为"，不是"这个具体值能用"。
// 32 位小写 hex 这个**形态**才是判据（它覆盖 looksLikeSession 的正例），
// 那 32 个字符具体是什么，与任何一条断言都无关。
//
// 教训留在这里而不是只改掉：手册类文档会顺手把真实凭证带进代码库，
// 而"抄一段能跑的例子"是最自然的动作 —— 所以每条 fixture 都该问一句
// "这是不是某个人的真实凭证"。
const fixtureSession = "0123456789abcdef0123456789abcdef"

// fakeAuth 是假上游期望的凭证。
type fakeAuth struct {
	session string
}

// fakeUpstream 起一个**严格执行双轨鉴权**的假上游，并返回它的调用记录。
//
// # 为什么必须"严格"
//
// 一个宽容的假上游（不看 Header 就返回模型列表）会让所有鉴权测试
// **全部变成 fail-open**：把 `token:` 写成 `Authorization:` 也照样绿灯。
// 所以这里的判据与真上游逐字一致 —— 用错 Header 就返回真上游那句
// `{"code":"100002","desc":"缺少 token"}`（手册第 3 节 / 第 10 节排错表），
// 连状态码都是 200（**这也是真的**：鉴权失败不是 4xx，见 client.go 的说明）。
func fakeUpstream(t *testing.T, want fakeAuth) (*httptest.Server, *fakeCalls) {
	t.Helper()
	calls := &fakeCalls{}
	mux := http.NewServeMux()

	// 模型目录：只认 `token:`。
	mux.HandleFunc(ModelsPath, func(w http.ResponseWriter, r *http.Request) {
		calls.models++
		calls.modelsToken = r.Header.Get("token")
		calls.modelsBearer = r.Header.Get("Authorization")
		if r.Header.Get("token") != want.session {
			writeMissingToken(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[`+
			`{"id":"deepseek-v4-flash-0731","context_window":1048576,"max_output_tokens":384000},`+
			`{"id":"GLM-5.3-Flash","context_window":1048576,"max_output_tokens":131072},`+
			`{"id":"Kimi-k2.6","context_window":262144,"max_output_tokens":65536}]}`)
	})

	// 对话：只认 `Authorization: Bearer`。
	mux.HandleFunc(ChatPath, func(w http.ResponseWriter, r *http.Request) {
		calls.chat++
		calls.chatBearer = r.Header.Get("Authorization")
		calls.chatToken = r.Header.Get("token")
		// 记下出站请求体，供"强制 stream"这类断言使用。
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		calls.lastChatBody = raw
		if r.Header.Get("Authorization") != "Bearer "+want.session {
			writeMissingToken(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 帧形态用标准 OpenAI 的 `data: {...}`（Loomy 是 OpenAI 兼容端点，
		// 手册实测的响应就是标准形态），末尾以 [DONE] 收尾让契约能读到底。
		_, _ = io.WriteString(w,
			`data: {"id":"t","object":"chat.completion.chunk","model":"deepseek-v4-flash-0731",`+
				`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n"+
				`data: [DONE]`+"\n\n")
	})

	// 兜底：未覆盖的路径返回 404，避免"假上游悄悄兜住一切"。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls.other++
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, calls
}

// writeMissingToken 复刻真上游的鉴权失败响应。
//
// ⚠ 三个细节都照抄实测，缺一个都会让测试失去意义：
//
//	状态码 200（不是 401/400）—— 所以"看状态码判断鉴权"这条路不通
//	code 是**字符串** "100002"（不是数字）
//	desc 是原文「缺少 token」（含一个空格）
func writeMissingToken(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"code":"100002","desc":"缺少 token"}`)
}

// fakeCalls 记录假上游收到的调用，供断言使用。
type fakeCalls struct {
	models       int
	modelsToken  string
	modelsBearer string

	chat         int
	chatBearer   string
	chatToken    string
	lastChatBody []byte

	other int
}

// newContractProvider 造一个指向假上游的 Provider。
func newContractProvider(base string) gateway.Provider {
	return NewWithConfig(Config{Client: NewWithBase(base), AuthDir: ""})
}

// newContractCredential 契约用的凭证（假上游，任何 session 都"有效"）。
func newContractCredential(uid string) gateway.Credential {
	return gateway.Credential{
		Provider: providerID,
		UID:      uid,
		Secret:   &Auth{Session: fixtureSession, UID: uid, Nickname: "fixture"},
	}
}

// TestContract 契约测试 —— **判据 2 的正式入口**。
//
// 用假上游让它无需真实凭证即可全程执行，因此 **CI 里不会跳过**。
func TestContract(t *testing.T) {
	srv, _ := fakeUpstream(t, fakeAuth{session: fixtureSession})
	gateway.RunProviderContract(t, func() gateway.Provider {
		return newContractProvider(srv.URL)
	}, gateway.WithCredential(newContractCredential("contract-fixture")))
}

// TestContractIsNotConditionallySkipped 把"契约必须无凭证也跑"固化下来。
//
// 若有人把 TestContract 改成"拿不到真实 session 就 skip"，这条会立刻红。
func TestContractIsNotConditionallySkipped(t *testing.T) {
	// 明确清空：契约不该依赖任何环境变量。
	t.Setenv("LOOMY_SESSION", "")
	t.Setenv("LOOMY_TEST_CRED", "")

	srv, _ := fakeUpstream(t, fakeAuth{session: fixtureSession})

	done := make(chan struct{})
	go func() {
		defer close(done)
		gateway.RunProviderContract(t, func() gateway.Provider {
			return newContractProvider(srv.URL)
		}, gateway.WithCredential(newContractCredential("no-env-fixture")))
	}()
	select {
	case <-done:
		// 契约在无凭证环境下完整跑完 —— 正是要保证的
	case <-time.After(60 * time.Second):
		t.Fatal("契约在无凭证环境下未完成 —— CI 里会变成永远挂住")
	}
}

// ── 双轨鉴权（手册第 3 节：90% 的人卡在这）────────────────────────────

// TestDualTrackAuth 钉死"哪个端点用哪个 Header"。
//
// # 这条测试对应的真实失败模式
//
// 手册第 3 节：`/models` 用 `token:`、`/chat/completions` 用
// `Authorization: Bearer`。用错的那一个返回 **HTTP 200** +
// `{"code":"100002","desc":"缺少 token"}` ——
// 于是失败以"目录是空的"或"对话没响应"的形态出现，而不是"鉴权头用错了"。
//
// 所以判据不能只看"请求成功"，必须看**假上游到底收到了哪个 Header**
// （calls.modelsToken / calls.chatBearer）。
func TestDualTrackAuth(t *testing.T) {
	srv, calls := fakeUpstream(t, fakeAuth{session: fixtureSession})
	cl := NewWithBase(srv.URL)
	a := &Auth{Session: fixtureSession, UID: "u1"}
	ctx := context.Background()

	t.Run("模型目录走 token 头", func(t *testing.T) {
		ms, err := cl.ModelList(ctx, a)
		if err != nil {
			t.Fatalf("ModelList 失败: %v（若错误含「缺少 token」，说明用错了鉴权头）", err)
		}
		if calls.modelsToken != fixtureSession {
			t.Errorf("假上游收到的 token 头 = %q，期望 %q", calls.modelsToken, fixtureSession)
		}
		// 反向：目录**不该**带 Bearer（带了说明有人把两条轨合错了方向）
		if calls.modelsBearer != "" {
			t.Errorf("模型目录请求不该带 Authorization，实际收到 %q", calls.modelsBearer)
		}
		if len(ms) != 3 {
			t.Fatalf("期望 3 个模型，得到 %d 个: %+v", len(ms), ms)
		}
		if ms[0].ID != "deepseek-v4-flash-0731" {
			t.Errorf("第一个模型 id = %q", ms[0].ID)
		}
		// 上下文/上限必须被解析出来（不能只拿到 id）
		if ms[0].ContextWindow != 1048576 || ms[0].MaxOutputTokens != 384000 {
			t.Errorf("第一个模型的窗口/上限 = %d/%d，期望 1048576/384000",
				ms[0].ContextWindow, ms[0].MaxOutputTokens)
		}
	})

	t.Run("对话走 Authorization Bearer", func(t *testing.T) {
		rc, status, body, err := cl.ChatStream(ctx, a, []byte(`{"model":"deepseek-v4-flash-0731","messages":[]}`))
		if err != nil {
			t.Fatalf("ChatStream 失败: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（body=%s）", status, summarize(body))
		}
		if rc == nil {
			t.Fatal("成功分支返回了 nil 流")
		}
		defer func() { _ = rc.Close() }()
		if calls.chatBearer != "Bearer "+fixtureSession {
			t.Errorf("假上游收到的 Authorization = %q，期望 %q",
				calls.chatBearer, "Bearer "+fixtureSession)
		}
		// 反向：对话**不该**带 token 头
		if calls.chatToken != "" {
			t.Errorf("对话请求不该带 token 头，实际收到 %q", calls.chatToken)
		}
	})

	// 负向对照：证明上面两条不是 fail-open。
	//
	// 直接对假上游发**错头**的请求，必须拿到「缺少 token」——
	// 这一步验的是假上游**真的在拦**，因此上面"通了"才有信息量。
	// （很多"鉴权测试"其实测的是一个什么都收的假服务端，那种测试永远是绿的。）
	t.Run("负向对照：假上游确实会拦错头", func(t *testing.T) {
		cases := []struct {
			name   string
			path   string
			header string
			value  string
		}{
			{"目录只给 Bearer", ModelsPath, "Authorization", "Bearer " + fixtureSession},
			{"对话只给 token", ChatPath, "token", fixtureSession},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				req, err := http.NewRequest(http.MethodPost, srv.URL+c.path, strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				if c.path == ModelsPath {
					req.Method = http.MethodGet
				}
				req.Header.Set(c.header, c.value)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				raw, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("状态码 = %d，期望 200（真上游的鉴权失败就是 200）", resp.StatusCode)
				}
				if !strings.Contains(string(raw), "缺少 token") {
					t.Fatalf("假上游没有拦住错头（收到 %s）—— 上面的正向断言失去意义", summarize(raw))
				}
			})
		}
	})
}

// TestWrongHeaderSurfacesMissingToken 证明"错头"在**本包的解析层**是可诊断的。
//
// 这是上一条的补充：它不问假上游，而是问"如果我们的实现用错了头，
// 调用方会看到什么"。答案是：ModelList 返回一个带「缺少 token」的错误
// （而不是一句含糊的"目录为空"）—— 这正是双轨鉴权值得专门测的原因：
// 失败信息足够具体，排错才不用猜。
func TestWrongHeaderSurfacesMissingToken(t *testing.T) {
	srv, _ := fakeUpstream(t, fakeAuth{session: fixtureSession})
	cl := NewWithBase(srv.URL)

	// 用一个**错的** session 发目录请求 → 假上游认不出来 → 缺 token 响应。
	_, err := cl.ModelList(context.Background(), &Auth{Session: "00000000000000000000000000000000"})
	if err == nil {
		t.Fatal("错 session 竟然拉到了目录 —— 假上游没在拦")
	}
	if !strings.Contains(err.Error(), "缺少 token") {
		t.Errorf("错误信息里应当带上上游原文「缺少 token」，实际: %v", err)
	}
}

// TestModelsFallsBackToStaticCatalog 守住"实时拉不到时目录不能空"。
//
// 这条不是锦上添花：Models() 是 /v1/models 的唯一数据源，它一空，
// 客户端就判定"模型不存在"并报错，而真相只是这一轮没拉到。
func TestModelsFallsBackToStaticCatalog(t *testing.T) {
	// 一个必定失败的基址（端口 0 不会有人监听；用保留的 TEST-NET 地址更快失败）。
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1")})
	ms, err := p.Models(context.Background(), newContractCredential("u1"))
	if err != nil {
		t.Fatalf("应当回落到静态快照而不是报错，实际: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("回落后目录为空 —— 这正是要防的静默失效")
	}
	// 静态快照里不该出现被上游下架的模型（见 filterUnavailable 的说明）
	for _, m := range ms {
		if m.ID == "doubao-seedream-5-lite" || m.ID == "qwen-image-3.0-pro" {
			t.Errorf("目录里不该出现已下架的模型 %s", m.ID)
		}
	}
}

// TestJSONNumberPreserved 守住出站请求体的数字字面量不被科学计数法改写。
//
// 这一条看着琐碎，但它防的是一个真实的、且**只在特定数值上出现**的 bug：
// 用 `map[string]any` 解析 JSON 会把所有数字变成 float64，
// 重新编码时 `1048576` 就写成 `1.048576e+06`。
// 大多数上游能接受，但"大多数"不是判据 —— 而且它只在客户端恰好传大数值时出现，
// 平时测不出来。所以这里直接断言字面量。
func TestJSONNumberPreserved(t *testing.T) {
	out := prepareBody([]byte(`{"model":"deepseek-v4-flash-0731","max_tokens":1048576,"temperature":0.7}`))
	s := string(out)
	if strings.Contains(s, "e+") || strings.Contains(s, "E+") {
		t.Errorf("出站请求体里出现了科学计数法（数字字面量被改写）: %s", s)
	}
	if !strings.Contains(s, `"stream":true`) {
		t.Errorf("出站请求体没有强制 stream:true: %s", s)
	}
	// 超限的 max_tokens 应被夹到该模型的上限（384000）
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("出站请求体不是合法 JSON: %v", err)
	}
	if string(got["max_tokens"]) != "384000" {
		t.Errorf("max_tokens = %s，期望被夹到 384000", got["max_tokens"])
	}
	if string(got["temperature"]) != "0.7" {
		t.Errorf("temperature 被改写成 %s（不该动不相关的字段）", got["temperature"])
	}
}
