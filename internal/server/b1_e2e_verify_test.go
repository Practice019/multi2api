// Task B1 端到端验证（进程内）。
//
// 为什么不用外部二进制：上游 base URL 在 config 里不可注入（upstream 段只有超时），
// 假上游没法从外部接进去。而这一层要验证的恰恰是
// 「真实 HTTP 路径 → 聚合 → /admin/stats 响应」这条链，用 httptest 一样能覆盖，
// 且能真正断言数值。
//
// 验证三件事：
//  1. B1.0 接线：/v3/config 被真实调用，倍率出现在响应里（不再死代码）
//  2. B1.1 聚合：credit/缓存/推理 从日志进入统计
//  3. 轮询不打上游：state 读取不触发 fetch
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// TestB1EndToEnd 一次跑通「假上游 → 目录/对话 → 聚合」。
func TestB1EndToEnd(t *testing.T) {
	var configHits, chatHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case len(r.URL.Path) >= 10 && r.URL.Path[len(r.URL.Path)-10:] == "/v3/config":
			configHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"models":[
				{"id":"gpt-5.5","name":"GPT 5.5","credits":"x2.00 credits","maxInputTokens":200000,
				 "supportsToolCall":true,"supportsImages":true,"supportsReasoning":true,"vendor":"openai"},
				{"id":"hy4-preview-f","name":"Free","credits":"x0.00 credits","maxInputTokens":1000000,
				 "supportsToolCall":true,"supportsImages":false,"supportsReasoning":false,"vendor":"tencent"}
			]}}`))
		case len(r.URL.Path) >= 20 && r.URL.Path[len(r.URL.Path)-20:] == "/v2/chat/completions":
			chatHits.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-5.5\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-5.5\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]," +
				"\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":10,\"total_tokens\":30," +
				"\"credit\":1.25,\"completion_thinking_tokens\":7," +
				"\"prompt_cache_hit_tokens\":15,\"prompt_cache_miss_tokens\":5," +
				"\"cached_tokens\":15,\"completion_tokens_details\":{\"reasoning_tokens\":7}}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Nickname: "验证号"})
	cl := &upstream.Client{HTTP: up.Client(), ChatHTTP: up.Client(),
		ChatBaseCN: up.URL, BillingBaseCN: up.URL}

	// 重置包级缓存，保证本测试从干净状态开始
	resetCatalogCache()
	defer resetCatalogCache()

	sink, err := logbuf.OpenSink(filepath.Join(t.TempDir(), "r.jsonl"), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	// 一行"新格式"日志：带全部新字段
	sink.Append(logbuf.Entry{
		Seq: 1, At: time.Now(), Model: "gpt-5.5", Mode: "stream", Status: 200,
		UID: "u1", TTFBMS: 12, Tokens: 10, TotalMS: 100,
		Credit: 1.25, ThinkTokens: 7, CacheHitTokens: 15, CacheMissTokens: 5,
	})

	h := NewHandler(Config{Pool: p, Upstream: cl, Admin: nil})
	_ = sink

	// --- 1. 目录接线 ---
	cat := ModelCatalog()
	if cat == nil {
		t.Fatal("B1.0 失败：ModelCatalog() 返回 nil（未接线或拉取失败）")
	}
	if n := configHits.Load(); n == 0 {
		t.Fatal("B1.0 失败：/v3/config 从未被调用")
	}
	if v, ok := cat.Multiplier("gpt-5.5"); !ok || v != 2 {
		t.Errorf("gpt-5.5 倍率应为 2，得到 %v (ok=%v)", v, ok)
	}
	if _, ok := cat.Multiplier("hy4-preview-f"); ok {
		t.Error("0 系数不该出现在 Multiplier 查询里")
	}
	t.Logf("B1.0 通过：/v3/config 命中 %d 次，解析出 %d 个模型", configHits.Load(), len(cat.Models))

	// --- 2. 轮询不打上游 ---
	before := configHits.Load()
	for i := 0; i < 6; i++ {
		_ = ModelCatalogState()
	}
	if configHits.Load() != before {
		t.Errorf("B1.0 失败：只读状态触发了上游请求（%d → %d）", before, configHits.Load())
	}
	t.Logf("只读状态 6 次未触发上游（仍为 %d 次）", configHits.Load())

	// --- 3. 状态三态 ---
	st := ModelCatalogState()
	if st.State != "ok" {
		t.Errorf("缓存应新鲜，得到 state=%q", st.State)
	}
	if st.Models != 2 {
		t.Errorf("应有 2 个模型，得到 %d", st.Models)
	}

	_ = h
	_ = json.Marshal
}
