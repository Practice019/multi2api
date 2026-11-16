// credential_vs_fail_test.go 钉住「凭据类失败只许说一句」。
//
// # 实测 bug（2026-11-16 的启动日志）
//
//	lobsterai 一个 401 打了两条：
//	  09:47:31 WARN lobsterai- 签到失败 uid=103668: 领取 HTTP 401: {"code":51102,"message":"请先登录"}
//	  09:47:31 WARN lobsterai- 签到凭据失效 uid=103668：领取 HTTP 401: {…}（按设计上报…）
//
// 同一件事两行，而且第二行才是可行动的那条（它说明"要重登"）。
// driver.go 里本来就有注释写着「凭据类错误**只由 reportCredentialFail 说一句**」——
// 但代码没实现它。**注释描述的规则不等于代码实现的规则**，这条判据把它钉死。
package dailycheckin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ★ 通用「签到失败」日志必须被 !CredentialError 保护。
func TestGenericFailLogGuardedByCredentialCheck(t *testing.T) {
	raw, err := os.ReadFile("driver.go")
	if err != nil {
		t.Fatalf("读 driver.go: %v", err)
	}
	src := stripComments(string(raw))

	// 找到通用失败日志那一行。
	idx := strings.Index(src, "checkin.fail")
	if idx < 0 {
		t.Fatal("找不到通用签到失败日志（checkin.fail）—— 改动越界了？")
	}
	// 它前面 N 个字符内必须有 CredentialError 的判断。
	start := idx - 400
	if start < 0 {
		start = 0
	}
	window := src[start:idx]
	if !strings.Contains(window, "CredentialError(cerr)") {
		t.Fatalf("★ 通用「签到失败」日志没有被 CredentialError 保护 ——\n"+
			"凭据类错误会同时打两条（实测：lobsterai 一个 401 打两行）。\n"+
			"窗口内文本：\n%s", window)
	}
}

// ★ reportCredentialFail 必须**恰好**有一条日志（不是零条，也不是两条）。
func TestCredentialFailLogsExactlyOnce(t *testing.T) {
	raw, err := os.ReadFile("reportCredentialFail.go")
	if err != nil {
		t.Fatalf("读 reportCredentialFail.go: %v", err)
	}
	src := stripComments(string(raw))
	re := regexp.MustCompile(`logx\.(?:Coalesce|Event|Eventf|Infof|Warnf|Errorf|Logf)\s*\(`)
	if n := len(re.FindAllString(src, -1)); n != 1 {
		t.Fatalf("★ reportCredentialFail 里应有恰好 1 条日志，实际 %d 条 ——\n"+
			"0 条 ⇒ 用户看不到「这个号要重登」；>1 条 ⇒ 又变成重复刷屏", n)
	}
}

// stripComments 去掉 // 行注释（判据前必须剥 —— 本仓踩过没剥的坑）。
func stripComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, ln := range lines {
		if at := strings.Index(ln, "//"); at >= 0 {
			lines[i] = ln[:at]
		}
	}
	return strings.Join(lines, "\n")
}
