// refresh_test.go 续期日志规格的判据。
//
// # 用户 2026-11-16 的实测日志（这就是要消灭的东西）
//
//	08:54:21 INFO  phanthywor uid=cmwf…jqq3dhad 上游拒绝了续期凭据（可重试，等下一轮；
//	               常见成因是被另一个端轮换掉了）：phanthywork: 上游拒绝了 refresh_token
//	               （可能已被另一个端轮换）——稍后重试: gateway: 上游拒绝了续期凭据
//	               （常见成因是被另一个端轮换掉了）——可重试，不应计为账号失效
//	               （上游 400：{"error":{"code":"invalid_grant",...}}）
//	…同一段文字再来 4 遍（每个账号一遍）
//	08:54:21 INFO  phanthywor 后台续期 成功 0 / 失败 5（跳过 0）
//
// 三个毛病：**逐账号重复**、**单行极长（层层嵌套的同一句）**、**汇总措辞各包不一**。
package logx

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func captureOne(fn func()) string {
	var buf bytes.Buffer
	restore := CaptureTo(&buf)
	defer restore()
	fn()
	return buf.String()
}

// ★ ① 逐账号行必须**短**（限长 80 的错误，不是 200 字符的嵌套串）。
func TestRefreshFailIsShort(t *testing.T) {
	long := errors.New("phanthywork: 上游拒绝了 refresh_token（可能已被另一个端轮换）——稍后重试: " +
		"gateway: 上游拒绝了续期凭据（常见成因是被另一个端轮换掉了）——可重试，不应计为账号失效" +
		`（上游 400：{"error":{"code":"invalid_grant","message":"Refresh token is invalid or expired."}}）`)

	out := captureOne(func() {
		RefreshFail("phanthywork", "refresh.rejected", "cmwfxhqzl12tkr53jjqq3dhad", long)
	})
	body := strings.TrimRight(out, "\n")

	// 只允许一行。
	if strings.Count(body, "\n") != 0 {
		t.Fatalf("★ 逐账号行被拆成了多行：\n%s", out)
	}
	// 长度上限：前缀约 40 字符 + 字段，超过 200 就是没限长。
	if len(body) > 200 {
		t.Fatalf("★ 逐账号行太长（%d 字符）—— 长错误应限长，完整原文在续期历史里：\n%s", len(body), body)
	}
	// 必须有短码（可聚合），且不能把整段中文塞进 reason。
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
	out := captureOne(func() {
		RefreshSummary("phanthywork", 0, 0, 5)
		RefreshSummary("cline", 3, 40, 0)
		RefreshSummary("minimax", 1, 2, 1, "elapsed_ms", 10339)
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("应有 3 行，实际 %d：\n%s", len(lines), out)
	}
	for _, ln := range lines {
		if !strings.Contains(ln, "refresh.summary ") {
			t.Fatalf("★ 汇总行应以 refresh.summary 开头（六个包必须同一形态）：%s", ln)
		}
		for _, f := range []string{"refreshed=", "skipped=", "failed="} {
			if !strings.Contains(ln, f) {
				t.Fatalf("★ 汇总行缺字段 %s：%s", f, ln)
			}
		}
	}
	// 有失败 ⇒ WARN；全成功 ⇒ INFO。
	if !strings.Contains(lines[0], "WARN") {
		t.Fatalf("失败 5 应是 WARN：%s", lines[0])
	}
	if !strings.Contains(lines[1], "INFO") {
		t.Fatalf("全成功应是 INFO：%s", lines[1])
	}
}

// ★ ④ 一轮 5 个账号失败：日志应是 **5 条短行 + 1 条汇总**，
// 而不是 5 条 200 字符的同义长行。
func TestRefreshRoundOutputIsCompact(t *testing.T) {
	var buf bytes.Buffer
	restore := CaptureTo(&buf)
	defer restore()

	long := errors.New("gateway: 上游拒绝了续期凭据（常见成因是被另一个端轮换掉了）——可重试，" +
		`不应计为账号失效（上游 400：{"error":{"code":"invalid_grant","message":"Refresh token is invalid or expired."}}）`)
	for _, uid := range []string{"cmwfxhqzl12tkr53jjqq3dhad", "cmwg6l3pu2dxrr53jyqrpz6uo",
		"cmwgexj2v0ivflp3jsgq950z9", "cmwh9skx34qioq83jwm7nling", "cmwhd329v5b5voq3joqexhd4l"} {
		RefreshFail("phanthywork", "refresh.rejected", uid, long)
	}
	RefreshSummary("phanthywork", 0, 0, 5)

	body := buf.String()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("应是 5 条逐账号 + 1 条汇总，实际 %d 行：\n%s", len(lines), body)
	}
	// 总字节数要显著小于"5 × 完整嵌套错误"。
	if len(body) > 1500 {
		t.Fatalf("★ 一整轮输出 %d 字节（应远小于改造前的 5×200+）—— 还是太长：\n%s", len(body), body)
	}
	// ★ 每个 uid 都必须**可区分**。
	//
	// ⚠ 这里不能断言"缩写以某几个字符开头"：`logbuf.ShortUID` 不是"取前 8 位"，
	//   它按各上游 uid 的真实形状剥前缀再取区分位（cuid 取尾部才是区分位）。
	//   第一版我按"前 8 位"写了断言，被它当场否掉 —— 断言形状会绑死实现，
	//   而真正的不变式是**五个号必须显示成五个不同的值**。
	seen := map[string]bool{}
	for _, ln := range lines {
		if at := strings.Index(ln, "uid="); at >= 0 {
			v := ln[at+4:]
			if sp := strings.IndexByte(v, ' '); sp > 0 {
				v = v[:sp]
			}
			seen[v] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("★ 5 个不同账号应显示成 5 个不同的 uid，实际 %d 个（%v）—— 缩写丢了区分位", len(seen), seen)
	}
}
