// verify_live_test.go 真机验证：用真实凭证跑一条完整对话。
//
// ⚠ 这是 **live** 测试：需要 auths/monkeycode/ 里有真实凭证，否则 Skip。
// CI 不该因缺凭证而红（与 codearts 的 live 测试同款约定）。
package monkeycode

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestLiveChat 真机跑一条对话，验证签名与双协议路由。
func TestLiveChat(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 跳过联网测试")
	}
	dir := liveAuthDir()
	list, err := LoadDir(dir)
	if err != nil || len(list) == 0 {
		t.Skipf("无真实凭证（%s），跳过", dir)
	}
	a := list[0]
	if !a.ChatUsable() {
		t.Skip("凭证缺 api_key/signing_secret，跳过")
	}

	p := New(Config{AuthDir: dir})
	cred := gateway.Credential{
		Provider: ProviderID,
		UID:      a.UID(),
		Nickname: a.DisplayName(),
		Secret:   a,
	}

	// 先拉目录，学协议归属。
	models, err := p.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("拉模型目录失败: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("模型目录为空")
	}
	t.Logf("模型目录 %d 条，首条 %s", len(models), models[0].ID)

	// 挑一个 basic 档的模型跑对话。
	model := models[0].ID
	for _, m := range models {
		if m.ID == "monkeycode-basic/qwen3.8-flash" || m.ID == "monkeycode-basic/deepseek-flash" {
			model = m.ID
			break
		}
	}
	t.Logf("用模型 %s（协议 %s）", model, interfaceOf(model))

	body := []byte(`{"model":"` + model + `","stream":true,
		"messages":[{"role":"user","content":"只回答两个字：收到"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	stream, err := p.Chat(ctx, cred, body)
	if err != nil {
		t.Fatalf("Chat 失败: %v", err)
	}
	defer stream.Body.Close()
	if stream.Status != 200 {
		raw, _ := io.ReadAll(io.LimitReader(stream.Body, 500))
		t.Fatalf("HTTP %d: %s", stream.Status, raw)
	}

	got, err := io.ReadAll(io.LimitReader(stream.Body, 64*1024))
	if err != nil {
		t.Fatalf("读流失败: %v", err)
	}
	if !bytes.Contains(got, []byte("data:")) {
		t.Fatalf("返回里没有 SSE 帧: %s", got)
	}
	if !bytes.Contains(got, []byte("[DONE]")) {
		t.Errorf("流没有以 [DONE] 收尾")
	}
	t.Logf("收到 %d 字节 SSE，片段: %s", len(got), firstN(got, 200))
}

// firstN 取前 n 字节（日志用）。
func firstN(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// liveAuthDir 真机凭证目录（相对仓库根）。
//
// 找不到就是 Skip —— 不能因为"这台机器没放凭证"而让测试红。
func liveAuthDir() string {
	candidates := []string{
		filepath.Join("..", "..", "auths", "monkeycode"),
		filepath.Join("auths", "monkeycode"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return candidates[0]
}
