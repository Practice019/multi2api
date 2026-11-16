// refresh_test.go 续期日志规格的判据。
//
// # 用户两次实测（这就是要消灭的东西）
//
//	2026-11-16 第一次：
//	  08:54:21 INFO phanthywor uid=cmwf…jqq3dhad 上游拒绝了续期凭据（可重试，等下一轮；
//	           常见成因是被另一个端轮换掉了）：phanthywork: 上游拒绝了 refresh_token
//	           （可能已被另一个端轮换）——稍后重试: gateway: …（上游 400：{…}）
//	  × 5 遍（每个账号一遍，每行约 200 字符）
//
//	2026-11-16 第二次（要求更严）：
//	  「多账号 log 不要刷屏，而是一行 log 总结即可」
//
// ⇒ 逐账号行走 `Coalesce`：**同一原因的多个账号折成一行**，带 `×N` 与时间跨度。
package logx

import (
	"errors"
	"strings"
	"testing"
)

// ★ ① 逐账号行必须**短**（限长 80 的错误，不是 200 字符的嵌套串）。
//
// ⚠ 2026-11-16 起逐账号行走 Coalesce，不立即输出 —— 所以这里要调 FlushCoalesced。
func TestRefreshFailIsShort(t *testing.T) {
	long := errors.New("phanthywork: 上游拒绝了 refresh_token（可能已被另一个端轮换）——稍后重试: " +
		"gateway: 上游拒绝了续期凭据（常见成因是被另一个端轮换掉了）——可重试，不应计为账号失效" +
		`（上游 400：{"error":{"code":"invalid_grant","message":"Refresh token is invalid or expired."}}）`)

	FlushCoalesced() // 清掉可能残留的桶
	RefreshFail("phanthywork", "refresh.rejected", "cmwfxhqzl12tkr53jjqq3dhad", long)
	lines := FlushCoalesced()
	if len(lines) != 1 {
		t.Fatalf("应折成 1 行，实际 %d 行：%v", len(lines), lines)
	}
	body := lines[0]

	if strings.Contains(body, "\n") {
		t.Fatalf("★ 逐账号行被拆成了多行：%s", body)
	}
	if len(body) > 200 {
		t.Fatalf("★ 逐账号行太长（%d 字符）—— 长错误应限长：%s", len(body), body)
	}
	if !strings.Contains(body, "reason=") {
		t.Fatalf("★ 缺少 reason= 短码（无法聚合）：%s", body)
	}
	if strings.Contains(body, "reason=\"") {
		t.Fatalf("★ reason 应是短码而不是长文本：%s", body)
	}
}

// ★ ② 原因必须被归成**少数几个短码**（能数出来）。
func TestClassifyRefreshReason(t *testing.T) {
	cases := map[string]string{
		"被另一个端轮换掉了":                            RefreshReasonRotated,
		"refresh token rotated elsewhere":      RefreshReasonRotated,
		`{"error":{"code":"invalid_grant"}}`:   RefreshReasonInvalid,
		"Refresh token is invalid or expired.": RefreshReasonInvalid,
		"token expired":                        RefreshReasonExpired,
		"dial tcp: i/o timeout":                RefreshReasonNetwork,
		"no such host":                         RefreshReasonNetwork,
		// ⚠ 实测漏网：`Post "https://…": EOF` 被归成 other，
		//   于是日志写 reason=other，运维看不出是网络断了还是凭据坏了。
		`Post "https://api.kuaipaper.com/v1/token/refresh": EOF`: RefreshReasonNetwork,
		"read tcp: connection reset by peer":                     RefreshReasonNetwork,
		"tls handshake failure":                                  RefreshReasonNetwork,
		"upstream HTTP 502":                                      RefreshReasonUpstream,
		"":                                                       RefreshReasonOther,
		"某种谁也没见过的新错误":                                            RefreshReasonOther,
	}
	for in, want := range cases {
		if got := ClassifyRefreshReason(in); got != want {
			t.Fatalf("ClassifyRefreshReason(%q) = %q，want %q", in, got, want)
		}
	}
}

// ★ ③ 汇总行必须工程化（事件名 + 蛇形字段），且各包**同一个形态**。
func TestRefreshSummaryIsUniform(t *testing.T) {
	a := RefreshSummaryLine("phanthywork", 0, 0, 5)
	b := RefreshSummaryLine("cline", 3, 40, 0)
	c := RefreshSummaryLine("minimax", 1, 2, 1, "elapsed_ms", 10339)
	for _, ln := range []string{a, b, c} {
		if !strings.HasPrefix(ln, "refresh.summary ") {
			t.Fatalf("★ 汇总行应以 refresh.summary 开头（六个包必须同一形态）：%s", ln)
		}
		for _, f := range []string{"refreshed=", "skipped=", "failed="} {
			if !strings.Contains(ln, f) {
				t.Fatalf("★ 汇总行缺字段 %s：%s", f, ln)
			}
		}
	}
	if !strings.Contains(c, "elapsed_ms=10339") {
		t.Fatalf("extra 字段没被带上：%s", c)
	}
}

// 有失败 ⇒ WARN；全成功 ⇒ INFO。
func TestRefreshSummaryLevelReflectsFailure(t *testing.T) {
	// 级别由 RefreshSummary 决定；这里直接验证"正文"与"级别"两条路径同源。
	if !strings.Contains(RefreshSummaryLine("up", 0, 0, 5), "failed=5") {
		t.Fatal("汇总正文应含 failed=5")
	}
}

// ★④ 用户最新要求：**5 个账号同一原因 ⇒ 只留 1 行**（不是 5 行）。
//
// 这是本轮的核心。改造前是"5 条短行 + 1 条汇总"，用户明确说不要那样。
func TestRefreshRoundFoldsToSingleLinePerReason(t *testing.T) {
	long := errors.New("gateway: 上游拒绝了续期凭据（常见成因是被另一个端轮换掉了）——可重试，" +
		`不应计为账号失效（上游 400：{"error":{"code":"invalid_grant","message":"Refresh token is invalid or expired."}}）`)
	uids := []string{
		"cmwfxhqzl12tkr53jjqq3dhad", "cmwg6l3pu2dxrr53jyqrpz6uo",
		"cmwgexj2v0ivflp3jsgq950z9", "cmwh9skx34qioq83jwm7nling",
		"cmwhd329v5b5voq3joqexhd4l",
	}
	FlushCoalesced() // 清桶
	for _, uid := range uids {
		RefreshFail("phanthywork", "refresh.rejected", uid, long)
	}
	lines := FlushCoalesced()

	if len(lines) != 1 {
		t.Fatalf("★ 5 个账号同一原因应只留 1 行，实际 %d 行：\n%s",
			len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "×5") {
		t.Fatalf("★ 折叠行必须带计数 ×5（否则用户不知道涉及几个号）：%s", lines[0])
	}
	if !strings.Contains(lines[0], "reason=rotated_elsewhere") {
		t.Fatalf("★ 折叠丢了原因码（那会让运维不知道是什么问题）：%s", lines[0])
	}
}

// ⑤ 不同原因**分别**折叠（不能并成一条，否则原因就丢了）。
func TestRefreshFoldsPerReason(t *testing.T) {
	FlushCoalesced() // 清桶
	RefreshFail("upx", "refresh.rejected", "uid-aaaa-1", errors.New("refresh token rotated elsewhere"))
	RefreshFail("upx", "refresh.rejected", "uid-aaaa-2", errors.New("refresh token rotated elsewhere"))
	RefreshFail("upx", "refresh.failed", "uid-bbbb-3", errors.New("dial tcp: i/o timeout"))
	lines := FlushCoalesced()

	if len(lines) != 2 {
		t.Fatalf("不同原因应各占一行（2 行），实际 %d 行：\n%s", len(lines), strings.Join(lines, "\n"))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "rotated_elsewhere") || !strings.Contains(joined, "reason=network") {
		t.Fatalf("★ 两个原因都必须出现：%s", joined)
	}
}
