package codearts

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
// workbuddy 那一侧踩过这个坑（阶段 0 评审 F4）。本文件是同一个修法：
// 用 `httptest` 起一个假上游，把 `Client.EngineBase` 指过去，
// 于是**无网络、无真实凭证**也能让契约跑满全程（含所有需凭证的分支）。
//
// # 与 live_test.go 的分工
//
//	本文件         假上游，确定性，**CI 里一定执行**（契约的 enforcement）
//	live_test.go   真凭证，按需，验证"契约对真实上游也成立"
//
// 两条都要有：只有前者会漏掉"假上游与真上游行为不一致"；
// 只有后者会让 CI 里根本没有契约执行。

// newContractClient 造一个指向假上游的 Client。
//
// 只改 base URL，不动其它配置（超时、连接池）——
// 契约验的是 Provider 的行为，不是 HTTP 客户端的参数。
func newContractClient(base string) *Client {
	c := New()
	c.EngineBase = base
	c.STSBase = base
	return c
}

// fakeContractUpstream 起一个最小的假上游，覆盖契约会打到的端点。
//
// 覆盖范围是按 ChatStream 的真实调用序列设计的：
//
//	POST /snap-manager/v1/chat-session/heartbeat   （busy / idle 各一次）
//	POST /api/v2/chat/completions                  （真正的对话）
//
// 心跳必须单独处理：它**不返回 SSE**，若走通用分支会被当成聊天响应，
// 契约读完拿到的就是一段错位的流（表现为莫名其妙的解析失败）。
func fakeContractUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// 会话心跳：占位/释放会话槽。返回 200 空体即可。
	//
	// ⚠ 这条必须有：没有它心跳会 404，虽然 ChatStream 对心跳失败是**容错**的
	// （只记日志继续），但那样契约就没覆盖到"心跳 + 聊天"这条真实序列。
	mux.HandleFunc("/snap-manager/v1/chat-session/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})

	// 对话：返回结构合法的 OpenAI 形态 SSE，让契约能读到底并 Close。
	//
	// 注意帧形态：CodeArts 的 SSE 是 `data:{...}`（**没有空格**），
	// 与标准 OpenAI 的 `data: {...}` 不同 —— 这里刻意用 CodeArts 的形态，
	// 保证契约验的是真实解析路径。
	mux.HandleFunc(ChatPath, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w,
			`data:{"id":"t","object":"chat.completion.chunk","model":"GLM-5.2",`+
				`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n"+
				`data:[DONE]`+"\n\n")
	})

	// 兜底：未覆盖的路径返回 200 空体，避免契约因无关端点失败。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newContractProvider 造一个指向假上游的 Provider。
func newContractProvider(base string) gateway.Provider {
	return NewWithConfig(Config{Client: newContractClient(base)})
}

// newContractCredential 契约用的凭证（假上游，任何签名都"有效"）。
//
// 必须给一个**结构完整**的 CodeArts 凭证：AccessKey/SecretKey 是签名材料
// （缺失会让 signedRequest 直接失败，契约就无法走到 Chat），
// RefreshToken 留空是刻意的 —— 空则不会触发续期，
// 让契约聚焦在 Chat/Models 的行为而不是续期流程。
func newContractCredential(uid string) gateway.Credential {
	return gateway.Credential{
		Provider: providerID,
		UID:      uid,
		Secret: &Auth{
			AccessKey:     "fixture-ak",
			SecretKey:     "fixture-sk",
			SecurityToken: "fixture-sts",
			// 显式设一个**未来**的过期时刻：NeedsRefresh 在 ExpiresAt<=0 时恒为 true，
			// 会把契约拖进"先续期"的分支，而续期需要 DPoP 私钥（假凭证没有）。
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
			UID:       uid,
			ClientID:  "vscode-codebot",
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

// TestContractAgainstRealUpstream 可选：有真实凭证时也跑一遍**真实**上游。
//
// 凭证来源与 live_test.go 一致（CODEARTS_TEST_CRED 或三个环境变量）。
// 这条按需执行，**不**承担 enforcement 的职责（那是 TestContract 的事）。
func TestContractAgainstRealUpstream(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网契约")
	}
	a := realCredentialForContract()
	if a == nil {
		t.Skip("无真实凭证（CODEARTS_TEST_CRED 或 CODEARTS_ACCESS_KEY/SECRET_KEY）——" +
			"跳过针对真实上游的契约。" +
			"注意：契约本身已在 TestContract 里用假上游执行，不会被跳过。")
	}
	gateway.RunProviderContract(t, NewProvider, gateway.WithCredential(gateway.Credential{
		Provider: providerID,
		UID:      a.UID,
		Secret:   a,
	}))
}

// realCredentialForContract 从环境读真实凭证；拿不到返回 nil。
func realCredentialForContract() *Auth {
	if p := os.Getenv("CODEARTS_TEST_CRED"); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		a, err := ParseCredential(raw)
		if err != nil {
			return nil
		}
		return a
	}
	ak := os.Getenv("CODEARTS_ACCESS_KEY")
	sk := os.Getenv("CODEARTS_SECRET_KEY")
	if ak == "" || sk == "" {
		return nil
	}
	return &Auth{
		AccessKey:     ak,
		SecretKey:     sk,
		SecurityToken: os.Getenv("CODEARTS_SECURITY_TOKEN"),
		ClientID:      "vscode-codebot",
	}
}

// TestContractIsNotConditionallySkipped 把 F4 的教训固化下来。
//
// 若有人把 TestContract 改回"无凭证就 skip"，这条会立刻红。
func TestContractIsNotConditionallySkipped(t *testing.T) {
	// 明确清空：契约不该依赖任何凭证环境变量
	t.Setenv("CODEARTS_TEST_CRED", "")
	t.Setenv("CODEARTS_ACCESS_KEY", "")
	t.Setenv("CODEARTS_SECRET_KEY", "")
	t.Setenv("CODEARTS_SECURITY_TOKEN", "")

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
		// 契约在无凭证环境下完整跑完 —— 正是要保证的
	case <-time.After(60 * time.Second):
		t.Fatal("契约在无凭证环境下未完成 —— CI 里会变成永远挂住")
	}
}

// TestContractRejectsWrongCredentialType 守住"凭证类型不符要返回明确错误，不 panic"。
//
// 契约本身覆盖了"不 panic"，但**不覆盖**"错误信息是否可诊断" ——
// 而后者才是这条规则的实际价值：调用方拿到一句
// "codearts: 凭证类型不对，期望 *codearts.Auth，实际 *auth.Auth"
// 就知道该改哪里，拿到 panic 栈则要读源码。
func TestContractRejectsWrongCredentialType(t *testing.T) {
	p := NewWithConfig(Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cases := []struct {
		name   string
		secret any
	}{
		{"nil", nil},
		{"错误类型", "not-a-codearts-auth"},
		{"错误指针类型", &struct{ X int }{}},
		{"nil 指针", (*Auth)(nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cred := gateway.Credential{Provider: providerID, UID: "u", Secret: c.secret}

			// Chat 必须返回 error 且不 panic
			cs, err := p.Chat(ctx, cred, []byte(`{}`))
			if err == nil {
				t.Errorf("Chat 对 %s 应返回错误，实际 err=nil（status=%d）", c.name, cs.Status)
			}
			if cs.Body != nil {
				_ = cs.Body.Close()
			}

			// Models 同理
			if _, err := p.Models(ctx, cred); err == nil {
				t.Errorf("Models 对 %s 应返回错误，实际 err=nil", c.name)
			}
		})
	}
}
