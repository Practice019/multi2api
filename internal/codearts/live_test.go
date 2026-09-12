package codearts

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// liveCredential 从环境变量读取真实凭证（不落仓库）。
//
// 设 CODEARTS_TEST_CRED=<路径到 json> 或直接给三个环境变量：
//
//	CODEARTS_ACCESS_KEY / CODEARTS_SECRET_KEY / CODEARTS_SECURITY_TOKEN
func liveCredential(t *testing.T) *Auth {
	t.Helper()
	if p := os.Getenv("CODEARTS_TEST_CRED"); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Skipf("读不到 %s: %v", p, err)
		}
		a, err := ParseCredential(raw)
		if err != nil {
			t.Fatalf("解析凭证失败: %v", err)
		}
		return a
	}
	ak := os.Getenv("CODEARTS_ACCESS_KEY")
	sk := os.Getenv("CODEARTS_SECRET_KEY")
	st := os.Getenv("CODEARTS_SECURITY_TOKEN")
	if ak == "" || sk == "" {
		t.Skip("未提供真实凭证（CODEARTS_TEST_CRED 或 CODEARTS_ACCESS_KEY/SECRET_KEY），跳过联网测试")
	}
	return &Auth{AccessKey: ak, SecretKey: sk, SecurityToken: st, ClientID: "vscode-codebot"}
}

// TestVerifyLive 用真实凭证调 caller-identity。
//
// 这是端到端证明：Go 的签名实现被华为云服务端接受。
func TestVerifyLive(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	a := liveCredential(t)
	if a.NeedsRefresh(0) {
		t.Skipf("凭证已过期（expiresAt=%d），跳过", a.ExpiresAt)
	}
	c := New()
	if err := c.Verify(a); err != nil {
		t.Fatalf("caller-identity 校验失败（签名或凭证问题）: %v", err)
	}
	t.Logf("✅ 凭证有效，Go 签名被服务端接受 (AK=%s)", a.AccessKey[:4]+"****")
}

// TestChatStreamLive 端到端调模型，校验响应是 OpenAI 形态。
func TestChatStreamLive(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	a := liveCredential(t)
	if a.NeedsRefresh(0) {
		t.Skip("凭证已过期，跳过")
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "GLM-5.2",
		"messages": []map[string]string{{"role": "user", "content": "回答两个字：收到"}},
		"stream":   false,
	})

	c := New()
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if status >= 400 {
		t.Fatalf("上游 %d: %s", status, string(respBody))
	}
	defer rc.Close()

	raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		t.Fatalf("读响应失败: %v", err)
	}

	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("响应不是 OpenAI 形态: %v\n原文: %s", err, string(raw[:min(len(raw), 300)]))
	}
	if len(resp.Choices) == 0 {
		t.Fatalf("无 choices: %s", string(raw[:min(len(raw), 300)]))
	}
	t.Logf("✅ 模型=%s 回答=%q tokens=%d",
		resp.Model, resp.Choices[0].Message.Content, resp.Usage.TotalTokens)
}

// TestChatStreamSSELive 校验流式响应（SSE，data: 前缀无空格）。
//
// 注意：CodeArts 的 SSE 帧是 `data:{...}`（**没有空格**），
// 与标准 OpenAI 的 `data: {...}` 不同，解析时必须兼容两者。
func TestChatStreamSSELive(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	a := liveCredential(t)
	if a.NeedsRefresh(0) {
		t.Skip("凭证已过期，跳过")
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "GLM-5.2",
		"messages": []map[string]string{{"role": "user", "content": "数到三"}},
		"stream":   true,
	})

	c := New()
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if status >= 400 {
		t.Fatalf("上游 %d: %s", status, string(respBody))
	}
	defer rc.Close()

	buf := make([]byte, 8192)
	n, _ := rc.Read(buf)
	head := string(buf[:n])
	if !strings.HasPrefix(head, "data:") {
		t.Fatalf("SSE 应以 data: 开头，实际: %q", truncate(head, 120))
	}
	// 不带空格是 CodeArts 的形态，确认我们没假设错
	if strings.HasPrefix(head, "data: ") {
		t.Log("注意：本次返回带空格形态，两种都要兼容")
	}
	t.Logf("✅ SSE 流已建立，首帧: %s", truncate(head, 150))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = time.Now
