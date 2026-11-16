// log_style_test.go 钉住「逐账号不打日志、文本要工程化」这条纪律。
//
// # 用户的两次明确要求
//
//	2026-11-16：「一个账号干什么事情之后就打印一遍 ⇒ 太多账号会刷屏。
//	             希望这件事情结束之后，直接统计数字一行就可以了。」
//	2026-11-16：「打印的文本也要符合工程化的那种语言，现在特别乱，
//	             不像成熟项目 / 企业级项目的语言。」
//
// # 判据的形状：扫源码，而不是跑运行时
//
// 「逐账号打日志」是**代码结构**问题：只要那个 log 调用还在循环里，换个输入
// 就会刷屏。跑一次运行时只能证明"这次没刷"，证明不了"结构上不会刷"。
// 所以这里直接扫源码。
package workbuddy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bannedPerAccount 逐账号路径上禁止出现的裸 log.Printf 形态。
//
// 这些行改造前一个账号一趟打 1~11 行（travel.go 就有 13 处），
// 几十个账号就是几百行，真正的失败被淹在里面。
var bannedPerAccount = []*regexp.Regexp{
	// travel / activity / checkin / keepalive / user-resource 的逐账号行
	regexp.MustCompile(`log\.Printf\("travel %s:`),
	regexp.MustCompile(`log\.Printf\("activity %s:`),
	regexp.MustCompile(`log\.Printf\("checkin %s:`),
	regexp.MustCompile(`log\.Printf\("keepalive %s`),
	regexp.MustCompile(`log\.Printf\("user-resource %s`),
	// 逐账号的成功日志（成功只该进汇总计数）
	regexp.MustCompile(`log\.Printf\("%s: 签到成功`),
}

// workbuddyFiles 本包里会被扫的源文件（排除测试）。
func workbuddyFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(all))
	for _, f := range all {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// ★ ① 逐账号路径上不许有裸 log.Printf。
func TestNoPerAccountLogPrintf(t *testing.T) {
	for _, name := range workbuddyFiles(t) {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		// ⚠ 必须先剥注释：本仓在别处踩过同一个坑（ui_wiring_toast_test.go 的注释写着
		//   「判据前先剥注释」）—— 注释里为了说明来历会**原文引用**旧实现那几行，
		//   不剥就会把"解释错误做法"当成"还在用错误做法"。
		src := stripLineComments(string(raw))
		for _, re := range bannedPerAccount {
			if loc := re.FindString(src); loc != "" {
				t.Fatalf("★ %s 里还有逐账号裸日志：%s\n"+
					"  —— 几十个账号就是几百行，用户明确要求改成「结束后一行统计」。\n"+
					"  改用：成功 → logx.Event(LevelDebug, ...)；错误 → logx.Coalesce(LevelWarn, ...)",
					name, loc)
			}
		}
	}
}

// ★ ② 逐账号路径上必须用**结构化**事件名（点分小写）。
//
// 这条防的是"从 log.Printf 改成了 Infof，但文本还是人话句子"——
// 那样刷屏解决了，可机器仍然解析不了。
func TestPerAccountEventsAreStructured(t *testing.T) {
	// travel.go / activity.go 是本次改造的两个文件，必须出现结构化事件名。
	wantIn := map[string][]string{
		"travel.go": {
			`"travel.depart.ok"`, `"travel.depart.fail"`,
			`"travel.claim.ok"`, `"travel.claim.fail"`,
			`"travel.adopt.ok"`, `"travel.adopt.fail"`,
			`"travel.skip"`, `"travel.status.fail"`,
		},
		"activity.go": {
			`"activity.report.ok"`, `"activity.report.fail"`,
		},
	}
	for name, wants := range wantIn {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		src := string(raw)
		for _, w := range wants {
			if !strings.Contains(src, w) {
				t.Fatalf("★ %s 缺少结构化事件名 %s —— 改造只做了一半"+
					"（刷屏解决了，但机器仍解析不了文本）", name, w)
			}
		}
	}
}

// ★ ③ 事件名本身必须点分小写（可被 grep / jq 聚合）。
//
// 扫出全部 `logx.Event(` / `logx.Coalesce(` 的第 3 个参数字面量来校验。
func TestEventNamesAreDottedLower(t *testing.T) {
	re := regexp.MustCompile(`logx\.(?:Event|Coalesce)\([^"]*"[^"]*",\s*"([^"]+)"`)
	nameRe := regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	for _, name := range workbuddyFiles(t) {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			if !nameRe.MatchString(m[1]) {
				t.Fatalf("★ %s 里的事件名 %q 不是点分小写（如 travel.depart.ok）——"+
					"机器聚合会失效", name, m[1])
			}
		}
	}
}

// stripLineComments 去掉 `//` 行注释（保留其余内容）。
//
// 够用即可：本包源码里没有含 `//` 的字符串字面量需要在扫描时保留
// （事件名与字段名都是纯 ASCII 标识符）。真需要精确时再上 go/parser。
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, ln := range lines {
		if at := strings.Index(ln, "//"); at >= 0 {
			lines[i] = ln[:at]
		}
	}
	return strings.Join(lines, "\n")
}
