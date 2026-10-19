// channels_error_test.go 渠道失败时**客户端看到什么**（状态码口径）。
//
// # 守的是什么
//
// 2026-10-20 实测踩到：上游 OpenCSG 对某模型返回 429 tpm_exceeded（每分钟 token 用尽），
// 经网关后变成 502 channel_error —— 客户端当成"网关故障"于是不按限流重试，用户也去查网关。
// 绕过网关直连上游验证过同样是 429 ⇒ 那是上游的事实，网关只能**如实转达**。
//
// 所以这里钉三条：
//
//	① 上游 429/401/503 ⇒ 客户端收到的**就是**那个码，且**保留上游原文**
//	② 上游失败时**绝不**回落账号池（Key 型渠道的分界，与 ① 无关但容易一起改坏）
//	③ 非 HTTP 错误（没可用 key / 上游给了非法码）⇒ 仍然 502 channel_error
package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
)

// upstreamWithStatus 一个"总是返回指定状态码 + 该响应体"的假上游。
func upstreamWithStatus(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// ① 状态码原样透传（+ 429 保留上游原文并补一句人话）。
func TestChannelPassesThroughUpstreamStatus(t *testing.T) {
	const upstream429 = `{"error":{"code":"capacity_exceeded","message":"model capacity exceeded: tpm_exceeded","type":"rate_limit_error"}}`
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"429 限流", 429, upstream429},
		{"401 凭据无效", 401, `{"error":{"message":"invalid api key"}}`},
		{"503 上游故障", 503, `{"error":{"message":"upstream down"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := upstreamWithStatus(t, c.status, c.body)
			defer srv.Close()
			var poolHits int32
			h := entryHandler(t, entryStore(t, srv.URL+"/v1", channelstore.StatusEnabled), &poolHits)

			code, body := postEntryChat(t, h, `{"model":"pub-y","stream":true,"messages":[]}`)
			if code != c.status {
				t.Fatalf("★ 上游 %d 应原样透传给客户端（不是 502），实际 %d：%s", c.status, code, body)
			}
			if !strings.Contains(body, "upstream_error") {
				t.Fatalf("应标成 upstream_error（与「网关自身故障」区分开），实际 %s", body)
			}
			if c.status == 429 {
				// 上游原文必须留（不替上游改口径）+ 一句可行动的中文
				if !strings.Contains(body, "tpm_exceeded") {
					t.Fatalf("429 应保留上游原文片段（tpm_exceeded），实际 %s", body)
				}
				if !strings.Contains(body, "上游") {
					t.Fatalf("429 应补一句中文说明「这是上游的额度限制」，实际 %s", body)
				}
			}
			// ② 上游失败**绝不**回落账号池
			if n := atomic.LoadInt32(&poolHits); n != 0 {
				t.Fatalf("★ 渠道失败不该回落账号池，实际被调用 %d 次", n)
			}
		})
	}
}

// ③ 非 HTTP 错误与非法状态码。
func TestChannelErrorFallbacks(t *testing.T) {
	h := NewHandler(Config{MaxBodyMB: 1})

	// 上游给了非法码（不在 100..599）⇒ 502，不让它流到客户端
	rec := httptest.NewRecorder()
	h.writeChannelError(rec, &gateway.UpstreamHTTPError{Status: 999, Body: "weird", Op: "chat"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("非法状态码应回落 502，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "非法的 HTTP 状态码") {
		t.Fatalf("应说清是上游给了非法码，实际 %s", rec.Body.String())
	}

	// 压根不是 HTTP 错误（没可用 key / 漏接线）⇒ 502 channel_error
	rec2 := httptest.NewRecorder()
	h.writeChannelError(rec2, errors.New("渠道 x 没有可用 key（全部停用或为空）"))
	if rec2.Code != http.StatusBadGateway || !strings.Contains(rec2.Body.String(), "channel_error") {
		t.Fatalf("非 HTTP 错误应是 502 channel_error，实际 %d %s", rec2.Code, rec2.Body.String())
	}

	// 渠道真错误：没有可用 key（端到端，确认走的是 channel_error 而不是透传）
	s := testChannelStore(t)
	if err := s.Upsert(&channelstore.Channel{
		ID: "ch-nokey", Name: "没key", BaseURL: "https://a.example.com/v1",
		Status: channelstore.StatusEnabled, Models: []string{"real-x"},
		ModelMap: map[string]string{"pub-y": "real-x"},
		Keys:     []channelstore.ChannelKey{{Key: "sk-off", Status: channelstore.StatusManuallyDisabled}},
	}); err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	var poolHits int32
	h2 := entryHandler(t, s, &poolHits)
	code, body := postEntryChat(t, h2, `{"model":"pub-y","stream":true,"messages":[]}`)
	if code != http.StatusBadGateway || !strings.Contains(body, "channel_error") {
		t.Fatalf("没可用 key 应是 502 channel_error，实际 %d %s", code, body)
	}
	if !strings.Contains(body, "没有可用 key") {
		t.Fatalf("应说清原因，实际 %s", body)
	}
	if n := atomic.LoadInt32(&poolHits); n != 0 {
		t.Fatalf("★ 没 key 也不该回落账号池，实际 %d 次", n)
	}
}
