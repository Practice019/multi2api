//go:build probe

// probe_real_test.go 一次性诊断：抓 CodeArts 上游拒绝请求时的**原始帧形态**。
//
// 只在 `go test -tags probe` 下编译运行，不进入常规测试。
// 存在的理由：出口层对"上游 200 + 流内错误"的处理必须按**真实字段名**写，
// 猜字段名会让修复静默失效（网关仍然回 200 + 空响应）。
package codearts

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestProbeRealUpstreamErrorShape(t *testing.T) {
	list, err := LoadDir("../../auths/codearts")
	if err != nil {
		t.Skipf("加载凭证失败: %v", err)
	}
	if len(list) == 0 {
		t.Skip("auths/codearts 下没有凭证")
	}
	t.Logf("载入 %d 份凭证，用第一份探测", len(list))

	c := New()
	for _, model := range []string{"GLM-5.2", "glm-5.2", "no-such-model", "glm-5.3-flash"} {
		body := []byte(fmt.Sprintf(
			`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true}`, model))

		rc, status, respBody, err := c.ChatStream(list[0], body)
		raw := ""
		if rc != nil {
			b, _ := io.ReadAll(io.LimitReader(rc, 8192))
			_ = rc.Close()
			raw = string(b)
		}
		// 只打印前 600 字节，避免刷屏
		if len(raw) > 600 {
			raw = raw[:600] + "...(截断)"
		}
		t.Logf("=== model=%q\n    status=%d err=%v\n    respBody=%q\n    rawStream=%q",
			model, status, err, string(respBody), raw)
	}
}

// TestProbeMaxTokensCeiling 探出上游**真正**接受的 max_tokens 上限。
//
// 存在的理由：models.go 的 knownModels 里 GLM-5.2 写的是 MaxTokens=131072，
// 而实测 131072 本身就被 `InferHub.001001005.400 The request param is invalid`
// 拒绝 —— 也就是说 clampMaxTokens 把一个超限值"裁剪"到了一个**依然非法**的值，
// 表现与不裁剪完全一样。表值必须按实测校准，不能沿用文档/推算值。
func TestProbeMaxTokensCeiling(t *testing.T) {
	list, err := LoadDir("../../auths/codearts")
	if err != nil || len(list) == 0 {
		t.Skipf("无可用凭证: %v", err)
	}
	c := New()

	// 默认通道的 4 个模型：每个用 65536 和自己的表值各试一次，
	// 钉住"65536 是安全上限"这条可推广的结论。
	type spec struct {
		name string
		mid  string
		mt   int
	}
	for _, s := range []spec{
		{"GLM-5.2", "GLM-5.2", 65536},
		{"glm-5.2-sft-harmony", "glm-5.2-sft-harmony", 65536},
		{"openpangu-2.0-pro", "openpangu-2.0-pro", 65536},
		{"openpangu-2.0-flash", "openpangu-2.0-flash", 65536},
	} {
		body := []byte(fmt.Sprintf(
			`{"model":%q,"messages":[{"role":"user","content":"hi"}],`+
				`"stream":true,"max_tokens":%d}`, s.mid, s.mt))

		rc, status, _, _ := c.ChatStream(list[0], body)
		raw := ""
		if rc != nil {
			b, _ := io.ReadAll(io.LimitReader(rc, 1024))
			_ = rc.Close()
			raw = string(b)
		}
		verdict := "接受"
		if status >= 400 || containsErr(raw) {
			verdict = "拒绝"
		}
		t.Logf("%-22s max_tokens=%-6d verdict=%s sample=%.120q",
			s.mid, s.mt, verdict, raw)
	}
}

func containsErr(s string) bool {
	return strings.Contains(s, "error_code") || strings.Contains(s, "invalid")
}
