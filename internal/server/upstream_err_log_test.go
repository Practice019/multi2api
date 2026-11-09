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
			http.StatusBadGateway, -1, nil, "provider_breaker:system", body, 217, "10.0.0.9", "probe/1.0")
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
			http.StatusOK, 10, nil, "", `should not be recorded`, 0, "", "")
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
			http.StatusBadGateway, -1, nil, "", huge, 0, "", "")
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
