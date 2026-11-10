// upstream_err_log_test.go 钉住「非 200 请求必须把上游原文记进日志」这条契约。
//
// # 用户原话（2026-11-10）
//
//	「如果说有什么请求报错了，有问题，不是 200，我觉得需要完整的记录一下具体的原文，
//	  这样我们找问题的时候就可以清晰的知道为什么。」
//
// # 为什么需要（一次真实的"查不到"）
//
// 2026-11-09 codar 两条 502 的日志里只有：
//
//	decision: "switched_upstream:codar attempts=5 spentMs=675 last=codar server 403"
//
// 于是只能知道"上游回了 403"，**永远猜不出**正文是风控 / 额度不足 / 账号受限 /
// 边缘 IP 封控 —— 那四者的处置完全不同（等 / 换号 / 重登 / 换出口）。
// 排障只能靠直连上游复现，而风控窗口过了就复现不出来 ⇒ 至今无法定性。
//
// 本文件把"失败必带原文、成功不带"钉成契约。
package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// TestFailedRequestLogsUpstreamErrorBody 失败请求必须带上游原文（脱敏后）。
func TestFailedRequestLogsUpstreamErrorBody(t *testing.T) {
	withChatLog(t)
	const uid = "upstream-err-uid-1"
	// 上游原文：含一个"后端是谁"的线索（host），用来同时验证脱敏生效。
	const body = `{"error":{"message":"API access is restricted for account","code":403},"host":"api.secret-backend.com"}`

	captureStdout(t, func() {
		logChatRowProvider(0, time.Second, "codar/deepseek-v4.1-flash", "stream", uid, "codar",
			http.StatusBadGateway, -1, nil, "provider_breaker:system", `{"model":"codar/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}],"stream":true}`, body, nil, 217, "10.0.0.9", "probe/1.0")
	})

	got := findEntryByUID(t, uid)
	if got.UpstreamErr == "" {
		t.Fatal("★ 失败请求的 upstream_err 为空 —— 用户要的「原文」没落盘")
	}
	// 必须保留判据关键词（这是排障时唯一能用来分类的东西）。
	if !strings.Contains(got.UpstreamErr, "API access is restricted") {
		t.Errorf("原文关键词丢了：%q", got.UpstreamErr)
	}
	// 必须脱敏：这份会渲染到管理台网页。
	if strings.Contains(got.UpstreamErr, "secret-backend.com") {
		t.Errorf("上游 host 未脱敏（会随网页泄露后端身份）：%q", got.UpstreamErr)
	}
}

// TestSuccessRequestOmitsUpstreamErr 成功请求不写该字段（它只在非 2xx 有意义）。
func TestSuccessRequestOmitsUpstreamErr(t *testing.T) {
	withChatLog(t)
	const uid = "upstream-err-uid-2"
	captureStdout(t, func() {
		// 即使调用方误传了 body，200 时也不该落盘。
		logChatRowProvider(0, time.Second, "m", "stream", uid, "codar",
			http.StatusOK, -1, nil, "", `{"model":"x"}`, `should not be recorded`, nil, 0, "", "")
	})
	if got := findEntryByUID(t, uid); got.UpstreamErr != "" {
		t.Errorf("成功请求不该带 upstream_err，得到 %q", got.UpstreamErr)
	}
}

// TestUpstreamErrIsTruncatedUTF8Safe 超长原文必须截断且不切断多字节字符。
func TestUpstreamErrIsTruncatedUTF8Safe(t *testing.T) {
	withChatLog(t)
	const uid = "upstream-err-uid-3"
	huge := strings.Repeat("上游返回了一整页错误说明；", 2000) // 远超 4 KiB，含中文
	captureStdout(t, func() {
		logChatRowProvider(0, time.Second, "m", "stream", uid, "codar",
			http.StatusBadGateway, -1, nil, "", `{"model":"x"}`, huge, nil, 0, "", "")
	})
	got := findEntryByUID(t, uid).UpstreamErr
	if len(got) == 0 {
		t.Fatal("原文为空")
	}
	if len(got) > logbuf.UpstreamErrMaxLen+64 {
		t.Errorf("未截断：%d 字节（上限 %d）", len(got), logbuf.UpstreamErrMaxLen)
	}
	if !strings.Contains(got, "[truncated:") {
		t.Errorf("截断标记缺失（会让人误以为这是全文）：…%q", got[len(got)-40:])
	}
	// UTF-8 合法性：截断处不得出现替换字符 U+FFFD。
	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("截断切断了多字节字符（出现 U+FFFD）")
	}
}

// TestFailedRequestLogsUpstreamRequestBody 失败请求必须落**我们实际发出去的请求体**。
//
// # 为什么会需要它（用户原话）
//
//	「而且很奇怪，手动调用模型全是 200，但是 DSH 调用就不行，为什么啊」
//
// 同一个模型、同一个账号：手写 curl 成功、走网关失败 ⇒ 差异只能出在
// **网关改写后的请求体/头**上。没有这一列时，排障只能靠猜（本次就猜了
// 风控 / IP / 账号受限三轮全错）。有了它才能逐字节对比"我们发了什么"。
func TestFailedRequestLogsUpstreamRequestBody(t *testing.T) {
	withChatLog(t)
	const uid = "upstream-req-uid-1"
	// 模拟网关改写后的出站 body：带 tools（DSH 特征）+ 被强制 stream
	const sent = `{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"t"}}],"stream":true}`

	captureStdout(t, func() {
		logChatRowProvider(0, time.Second, "codar/deepseek-v4.1-flash", "stream", uid, "codar",
			http.StatusBadGateway, -1, nil, "provider_breaker:system", sent,
			`{"detail":{"code":"client_not_supported"}}`, nil, 100, "", "")
	})

	got := findEntryByUID(t, uid)
	if got.UpstreamReq == "" {
		t.Fatal("★ 失败请求的 upstream_req 为空 —— 无法对比「我们发了什么」")
	}
	for _, must := range []string{`"model":"deepseek-v4.1-flash"`, `"tools"`, `"stream":true`} {
		if !strings.Contains(got.UpstreamReq, must) {
			t.Errorf("请求体里缺少判据 %s：%q", must, got.UpstreamReq)
		}
	}
}

// TestSuccessRequestOmitsUpstreamReq 成功请求不写请求体（正常业务流量，占空间且无排障价值）。
func TestSuccessRequestOmitsUpstreamReq(t *testing.T) {
	withChatLog(t)
	const uid = "upstream-req-uid-2"
	captureStdout(t, func() {
		logChatRowProvider(0, time.Second, "m", "stream", uid, "codar",
			http.StatusOK, 10, nil, "", `{"model":"x","messages":[{"role":"user","content":"big"}]}`, "", nil, 0, "", "")
	})
	if got := findEntryByUID(t, uid); got.UpstreamReq != "" {
		t.Errorf("成功请求不该带 upstream_req，得到 %q", got.UpstreamReq)
	}
}

// TestUpstreamReqKeepsHeadAndTail 超长请求体必须保留**头尾两端**。
//
// 判据分布在两端：头部有 model / tools / stream，尾部有最后一个 user 消息。
// 只留头会看不到"客户端最后说了什么"，只留尾会看不到模型名。
func TestUpstreamReqKeepsHeadAndTail(t *testing.T) {
	withChatLog(t)
	const uid = "upstream-req-uid-3"
	head := `{"model":"HEAD_MARKER","messages":[`
	tail := `{"role":"user","content":"TAIL_MARKER"}]}`
	mid := strings.Repeat(`{"role":"assistant","content":"`+strings.Repeat("x", 200)+`"},`, 200)
	sent := head + mid + tail

	captureStdout(t, func() {
		logChatRowProvider(0, time.Second, "m", "stream", uid, "codar",
			http.StatusBadGateway, -1, nil, "", sent, "boom", nil, 0, "", "")
	})

	got := findEntryByUID(t, uid).UpstreamReq
	if !strings.Contains(got, "HEAD_MARKER") {
		t.Error("头部判据（model 名）丢失")
	}
	if !strings.Contains(got, "TAIL_MARKER") {
		t.Error("尾部判据（最后一条 user 消息）丢失 —— 只保留头部会让\"客户端最后说了什么\"不可见")
	}
	if !strings.Contains(got, "omitted") {
		t.Errorf("缺少省略标记（会让人误以为这是全文）")
	}
	if len(got) > logbuf.UpstreamReqMaxLen+64 {
		t.Errorf("未截断：%d 字节（上限 %d）", len(got), logbuf.UpstreamReqMaxLen)
	}
}
