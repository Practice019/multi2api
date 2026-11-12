//go:build probe

// probe_real_test.go ★ 有界真机取证（**默认不编译**：需要 -tags probe）。
//
// 与 codearts 的 probe_real_test.go 同一约定：真机探针用 build tag 隔离，
// `go test ./...` 永远不会跑它（否则 CI 会去打真实上游）。
//
// # 本次取证的目的（两条，别扩大）
//
//	(a) 判定"上游到底回没回 usage、回在哪一帧" —— 给日志层一份可判定的证据
//	(b) 探索"怎么才能拿到可见文本"：同一模型换 max_tokens（32 / 256 / 1024）
//	    看 text 块（可见正文）到底出不出现
//
// # 硬边界（越界即违规）
//
//	最多 4 次真实调用 · 同一模型 · max_tokens ≤ 1024 · 串行 · 每次之间 ≥3 秒
//	原始 SSE 脱敏后落 D:/temp/ph1-raw-sse.txt（令牌/签名/uid 全替换）
//	**不**加任何报告里没有的参数（例如 thinking:disabled）—— 那是照抄没有依据的协议
package phanthywork

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/anthroconv"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/wire"
)

// probeModel 本次取证的模型（与你那份 usage 原文同一个量级输出的那个）。
const probeModel = "glm-5.3-flash"

// probeDumpPath 脱敏后的原始 SSE 落盘位置。
const probeDumpPath = "D:/temp/ph1-raw-sse.txt"

// TestProbeRawSSE 真机取证：三次调用（32 / 256 / 1024）。
func TestProbeRawSSE(t *testing.T) {
	dir := os.Getenv("PHANTHY_AUTH_DIR")
	if dir == "" {
		dir = "D:/project_GIT/workbuddy2api/auths/phanthywork"
	}
	list, err := LoadDir(dir)
	if err != nil || len(list) == 0 {
		t.Fatalf("读凭证失败（%s）: %v / %d 份", dir, err, len(list))
	}
	a := list[0]
	t.Logf("凭证: uid=%s 已注册=%v conflict=%v token长度=%d", maskUID(a.UID()), a.Reg.RegisteredAtMS > 0, a.Reg.ConflictAtMS > 0, len(a.OAuth.AccessToken))

	p := NewProvider(Config{AuthDir: dir})
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Nickname: a.Nickname(), FilePath: a.FilePath, Secret: a}

	var dump strings.Builder
	fprintf := func(format string, args ...any) { fmt.Fprintf(&dump, format, args...) }
	fprintf("# phanthywork 真机取证（脱敏）— %s\n", time.Now().Format(time.RFC3339))

	// 三次调用：同一模型、串行、间隔 ≥3 秒。
	for i, maxTok := range []int{32, 256, 1024} {
		if i > 0 {
			time.Sleep(3 * time.Second)
		}
		body := fmt.Sprintf(`{"model":"phanthywork/%s","messages":[{"role":"user","content":"1+1等于几？只回答数字"}],"max_tokens":%d}`, probeModel, maxTok)
		start := time.Now()
		cs, cerr := p.Chat(context.Background(), cred, []byte(body))
		if cerr != nil {
			t.Fatalf("max_tokens=%d Chat 失败: %v", maxTok, cerr)
		}
		raw, _ := io.ReadAll(cs.Body)
		_ = cs.Body.Close()
		elapsed := time.Since(start)

		// ① 上游原始帧的类型序列（这是本次取证的核心事实）
		types, usages, textDeltas, thinkDeltas := scanSSE(string(raw))
		// ② 走出口层的转换 + 聚合，看客户端会拿到什么
		var conv strings.Builder
		convErr := anthroconv.AnthropicSSEToOpenAI(strings.NewReader(string(raw)), &conv)
		var agg map[string]any
		var aggErr error
		if convErr == nil {
			agg, aggErr = wire.Aggregate(strings.NewReader(conv.String()))
		}
		content, reasoning := "", ""
		if aggErr == nil {
			if chs, ok := agg["choices"].([]any); ok && len(chs) > 0 {
				if c0, ok := chs[0].(map[string]any); ok {
					if msg, ok := c0["message"].(map[string]any); ok {
						content, _ = msg["content"].(string)
						reasoning, _ = msg["reasoning_content"].(string)
					}
				}
			}
		}
		var aggUsage any
		if agg != nil {
			aggUsage = agg["usage"]
		}
		t.Logf("max_tokens=%3d | HTTP=%d | 耗时=%s | 帧类型=%v | usage帧=%d | text增量=%d | thinking增量=%d",
			maxTok, cs.Status, elapsed.Round(time.Millisecond), types, usages, textDeltas, thinkDeltas)
		t.Logf("             聚合后: content长度=%d reasoning长度=%d usage=%v (convErr=%v aggErr=%v)",
			len(content), len(reasoning), aggUsage, convErr, aggErr)

		fprintf("\n=== 调用 %d：max_tokens=%d | HTTP=%d | 耗时=%s ===\n", i+1, maxTok, cs.Status, elapsed.Round(time.Millisecond))
		fprintf("# 帧类型序列: %v\n", types)
		fprintf("# usage 所在帧: %d 处 -> %v\n", usages, usageLines(string(raw)))
		fprintf("# text 增量帧数=%d | thinking 增量帧数=%d\n", textDeltas, thinkDeltas)
		fprintf("# 聚合后 content=%q（长度 %d）\n", clip(content, 120), len(content))
		fprintf("# 聚合后 reasoning 长度=%d | usage=%v\n", len(reasoning), aggUsage)
		fprintf("# 原始 SSE（脱敏）:\n%s\n", sanitizeSSE(string(raw)))
	}

	if err := os.MkdirAll("D:/temp", 0o755); err != nil {
		t.Logf("建目录失败: %v", err)
	}
	if err := os.WriteFile(probeDumpPath, []byte(dump.String()), 0o600); err != nil {
		t.Errorf("写取证文件失败: %v", err)
	}
	t.Logf("脱敏原始帧已落盘: %s", probeDumpPath)
}

// scanSSE 扫一份 Anthropic SSE，返回 (事件类型序列, usage 帧数, text 增量数, thinking 增量数)。
func scanSSE(s string) (types []string, usages, textDelta, thinkDelta int) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if t0, ok := ev["type"].(string); ok {
			types = append(types, t0)
		}
		if _, ok := ev["usage"]; ok {
			usages++
		}
		if msg, ok := ev["message"].(map[string]any); ok {
			if _, ok := msg["usage"]; ok {
				usages++
			}
		}
		if d, ok := ev["delta"].(map[string]any); ok {
			switch d["type"] {
			case "text_delta":
				textDelta++
			case "thinking_delta":
				thinkDelta++
			}
		}
	}
	return types, usages, textDelta, thinkDelta
}

// usageLines 收集所有带 usage 的帧的原文（脱敏后），供报告直接引用。
func usageLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.Contains(line, "usage") {
			continue
		}
		out = append(out, clip(sanitizeSSE(strings.TrimSpace(line)), 400))
	}
	return out
}

// 脱敏：JWT / 长 base64 / uid 三类替换。
var (
	reJWT = regexp.MustCompile(`eyJ[A-Za-z0-9_.\-]{10,}`)
	reB64 = regexp.MustCompile(`[A-Za-z0-9_\-]{40,}`)
	reUID = regexp.MustCompile(`cmw[a-z0-9]{16,}`)
	reSig = regexp.MustCompile(`"(signature|signature_delta)":"[^"]*"`)
)

// sanitizeSSE 把一段文本里的敏感值换成占位符。
func sanitizeSSE(s string) string {
	s = reSig.ReplaceAllString(s, `"$1":"<REDACTED>"`)
	s = reJWT.ReplaceAllString(s, "<JWT>")
	s = reUID.ReplaceAllString(s, "<UID>")
	s = reB64.ReplaceAllString(s, "<B64>")
	return s
}

// maskUID 只留 uid 前 8 位（日志/测试输出用）。
func maskUID(uid string) string {
	if len(uid) <= 8 {
		return "<short>"
	}
	return uid[:8] + "…"
}

// clip 截断长字符串。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
