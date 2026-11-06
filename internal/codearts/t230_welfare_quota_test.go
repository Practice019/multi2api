package codearts

// t230_welfare_quota_test.go —— T-230 两处 P1 修复的回归网。
//
// # 补的是什么
//
//	P1-4  判序：同一帧既带额度文案又带真实数据时，**不许**判成额度耗尽
//	P1-3  签到：claim/confirm 只回 code:0 = 受理 ≠ 到账 ⇒ **必须回读 delivery** 才算成功
//
// # 为什么这两条必须有测试（而不是靠注释）
//
// 两者的失败模式都是**静默**的：
//   · 判序错 → 把一个**可用**模型标成额度耗尽（界面把它隐藏），日志里只有一行"标记不可用"；
//   - 不回读 → 面板显示"领取成功"，而用户下次看到的积分还是旧的。
// 本包原有的福利测试全是 `*Live`（无凭证环境下全 skip）⇒ 这两条在没有凭证的环境里
// 一行都不会被跑过。本文件用 httptest 假上游覆盖，**不需要任何凭证、不联网**。

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// ── P1-4：判序（先判有内容，再判额度）────────────────────────────────────

// TestT230QuotaNotMarkedWhenFrameHasChoices 同一帧既带额度文案又带真实数据
// ⇒ **不算额度耗尽**（官方 stream_fault.rs 规则②：有答案就绝不判故障）。
func TestT230QuotaNotMarkedWhenFrameHasChoices(t *testing.T) {
	const model = "t230-frame-with-choices"
	ClearQuota(model)

	frame := `{"error_msg":"insufficient quota","choices":[{"delta":{"content":"hi"}}]}`
	b := &quotaTrackingBody{ReadCloser: io.NopCloser(strings.NewReader(frame)), model: model}
	if _, err := b.Read(make([]byte, 4096)); err != nil && err != io.EOF {
		t.Fatalf("读取帧失败: %v", err)
	}

	if IsQuotaExhausted(model) {
		t.Error("同一帧带 choices（有内容）时被判成额度耗尽 —— 判序仍是有内容在后，违反官方规则②")
	}
	if !b.done {
		t.Error("得出结论后应置 done（避免后续帧继续扫描）")
	}
}

// TestT230QuotaMarkedOnPureQuotaFrame 纯额度帧（无 choices）⇒ 仍要标记。
//
// 这条是上一条的**反向保护**：只把判序调过来、却把额度识别一起弄丢，是不允许的。
func TestT230QuotaMarkedOnPureQuotaFrame(t *testing.T) {
	const model = "t230-pure-quota-frame"
	ClearQuota(model)

	frame := `{"error_code":"InferHub.4291.200","error_msg":"insufficient quota"}`
	b := &quotaTrackingBody{ReadCloser: io.NopCloser(strings.NewReader(frame)), model: model}
	if _, err := b.Read(make([]byte, 4096)); err != nil && err != io.EOF {
		t.Fatalf("读取帧失败: %v", err)
	}

	if !IsQuotaExhausted(model) {
		t.Error("纯额度帧没有被标记 —— 判序改错了方向（额度识别被弄丢）")
	}
}

// ── P1-3：回读确认 ──────────────────────────────────────────────────────

// t230Upstream 是假 snap 上游：为了验证"回读"，delivery 必须**状态化** ——
// 第 1 次是领取前的枚举（一项可领），之后每次都是回读（答案由 reread 决定）。
type t230Upstream struct {
	deliveries   int32
	claims       int32
	confirms     int32
	rereadStatus string // CONFIRMED / CONSUMED / 其它（非空即"已到账"）
	rereadHTTP   int    // >=400 时回读直接报 HTTP 错（模拟回读失败）
}

func newT230Upstream(t *testing.T, sc *t230Upstream) (*Client, func()) {
	t.Helper()
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/ops/delivery"):
			n := atomic.AddInt32(&sc.deliveries, 1)
			if n > 1 {
				// —— 回读阶段 ——
				if sc.rereadHTTP >= 400 {
					w.WriteHeader(sc.rereadHTTP)
					_, _ = w.Write([]byte(`{"code":500,"message":"boom"}`))
					return
				}
				claimable, status := "true", "ELIGIBLE"
				if sc.rereadStatus != "" {
					claimable, status = "false", sc.rereadStatus
				}
				_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"campaignId":7,` +
					`"type":"USER_LOGIN","title":"每日签到领1000积分","claimable":` + claimable +
					`,"status":"` + status + `","benefitAmount":1000,"benefitUnit":"CREDIT"}]}}`))
				return
			}
			// —— 领取前的枚举：一项可领 ——
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"campaignId":7,` +
				`"type":"USER_LOGIN","title":"每日签到领1000积分","claimable":true,` +
				`"status":"ELIGIBLE","benefitAmount":1000,"benefitUnit":"CREDIT"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/v1/ops/claim"):
			atomic.AddInt32(&sc.claims, 1)
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"id":123}}`))
		case strings.HasSuffix(r.URL.Path, "/v1/ops/confirm"):
			atomic.AddInt32(&sc.confirms, 1)
			_, _ = w.Write([]byte(`{"code":0,"message":"ok"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	c, done := newMockClient(t, handler, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return c, done
}

// TestT230ClaimReportsUnconfirmedWhenNotArrived claim 回 code:0，但回读仍
// claimable=true ⇒ **不能报成功**（官方：受理 ≠ 到账）。
func TestT230ClaimReportsUnconfirmedWhenNotArrived(t *testing.T) {
	fastArrivalRetry(t)
	sc := &t230Upstream{} // rereadStatus 为空 ⇒ 回读一直 claimable
	c, done := newT230Upstream(t, sc)
	defer done()

	results, err := c.ClaimAllWelfare(testWelfareAuth())
	if err != nil {
		t.Fatalf("ClaimAllWelfare 不该报错（claim 已受理）: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d", len(results))
	}
	got := results[0]
	if got.Claimed {
		t.Error("回读未确认到账却报了 Claimed=true —— 这正是官方点名的『把受理当成功』")
	}
	if !strings.Contains(got.Message, "尚未确认到账") {
		t.Errorf("消息应如实说明未确认到账，实际: %q", got.Message)
	}
	if n := atomic.LoadInt32(&sc.claims); n != 1 {
		t.Errorf("应当只 claim 一次，实际 %d", n)
	}
}

// TestT230ClaimSucceedsWhenArrived 回读看到 `!claimable && status=CONFIRMED`
// ⇒ 才算成功（并带上"已回读确认到账"）。
func TestT230ClaimSucceedsWhenArrived(t *testing.T) {
	fastArrivalRetry(t)
	sc := &t230Upstream{rereadStatus: "CONFIRMED"}
	c, done := newT230Upstream(t, sc)
	defer done()

	results, err := c.ClaimAllWelfare(testWelfareAuth())
	if err != nil {
		t.Fatalf("ClaimAllWelfare 报错: %v", err)
	}
	if len(results) != 1 || !results[0].Claimed {
		t.Fatalf("回读已确认到账时应 Claimed=true，实际: %+v", results)
	}
	if !strings.Contains(results[0].Message, "已回读确认到账") {
		t.Errorf("消息应标注已确认到账，实际: %q", results[0].Message)
	}
}

// TestT230ClaimSucceedsWhenConsumed CONSUMED 与 CONFIRMED 同等对待（官方判据是集合）。
func TestT230ClaimSucceedsWhenConsumed(t *testing.T) {
	fastArrivalRetry(t)
	sc := &t230Upstream{rereadStatus: "CONSUMED"}
	c, done := newT230Upstream(t, sc)
	defer done()

	results, err := c.ClaimAllWelfare(testWelfareAuth())
	if err != nil {
		t.Fatalf("ClaimAllWelfare 报错: %v", err)
	}
	if len(results) != 1 || !results[0].Claimed {
		t.Fatalf("CONSUMED 也应算已到账，实际: %+v", results)
	}
}

// TestT230ClaimUnconfirmedWhenReReadFails 回读**一直失败**（5xx）⇒ 同样不能报成功，
// 且不能把整批判成错误（claim 确实已受理）。
func TestT230ClaimUnconfirmedWhenReReadFails(t *testing.T) {
	fastArrivalRetry(t)
	sc := &t230Upstream{rereadHTTP: 500}
	c, done := newT230Upstream(t, sc)
	defer done()

	results, err := c.ClaimAllWelfare(testWelfareAuth())
	if err != nil {
		t.Fatalf("回读失败不该让整批返回错误（claim 已受理）: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d", len(results))
	}
	if results[0].Claimed {
		t.Error("回读失败时不得报 Claimed=true —— 没有任何证据说明到账了")
	}
	if !strings.Contains(results[0].Message, "尚未确认到账") {
		t.Errorf("消息应如实说明未确认到账，实际: %q", results[0].Message)
	}
	if n := atomic.LoadInt32(&sc.deliveries); n < 2 {
		t.Errorf("应当发生回读（delivery ≥2 次），实际 %d 次", n)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────

// fastArrivalRetry 把回读重试间隔压到 0，避免测试为了等 2 秒而变慢。
func fastArrivalRetry(t *testing.T) {
	t.Helper()
	prev := welfareArrivalRetryEvery
	welfareArrivalRetryEvery = 0
	t.Cleanup(func() { welfareArrivalRetryEvery = prev })
}
