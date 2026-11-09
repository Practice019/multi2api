package main

// codar_registration_test.go —— codar 注册块**不得寄生在别的上游的条件块里**（T-462）。
//
// # 守的是什么（实际发生过的缺陷）
//
// `main.go` 里 codar 的 `registry.Register(cd)` 整段曾被误放进
// `if mk != nil && cfg.MonkeyCodePoolAccounts { ... }` 内部。
// 后果不是编译错误、也不是测试红，而是**运行时静默消失**：
//
//	monkeycode 关着（或 pool_accounts=false）⇒ codar 完全不注册
//	⇒ 注册表里没有它，但 /admin/channels 的候选里仍有它
//	⇒ 日志里连一句 "codar: 已启用" 都不打
//	⇒ 用户看到的现象是"这版新增的上游根本不存在"
//
// 上游之间不共享启用条件（`Config.normalize` 里每家的开关都是独立的），
// 所以这块必须待在**函数顶层**。
//
// # 判据为什么用缩进
//
// 块被嵌进去时唯一稳定可辨的痕迹就是缩进多了一层。
// 而"往上找最近的 `if ... {`"要写括号匹配，源码里还夹着字符串和注释 ——
// 那种判据会静默失效，而静默失效的守卫比没有守卫更糟（它给人已经防住的错觉）。

import (
	"strings"
	"testing"
)

// TestCodarRegistrationIsTopLevel 确认 codar 的启用分支在 main() 顶层。
func TestCodarRegistrationIsTopLevel(t *testing.T) {
	src := readCheckinFile(t, "cmd/server/main.go")

	const needle = "if cfg.CodarEnabled {"
	if !strings.Contains(src, needle) {
		t.Fatalf("cmd/server/main.go 里找不到 %q —— codar 的注册块不见了（或缩进被改成嵌套）", needle)
	}

	// 整文件里所有出现处都必须是一层缩进（一个 tab）。
	lines := strings.Split(src, "\n")
	found := 0
	for i, ln := range lines {
		if !strings.HasPrefix(strings.TrimLeft(ln, "\t"), needle) {
			continue
		}
		found++
		indent := len(ln) - len(strings.TrimLeft(ln, "\t"))
		if indent != 1 {
			t.Errorf("cmd/server/main.go:%d 的 %q 缩进 %d 层（want 1）——\n"+
				"codar 的注册块被嵌进了别的条件块内部。上游之间的启用条件互相独立，"+
				"嵌进去就意味着那个上游关掉时 codar 会**静默不注册**（T-462 实际发生过："+
				"monkeycode.pool_accounts=false 时 codar 整块不执行）。", i+1, needle, indent)
		}
	}
	if found == 0 {
		t.Fatal("没找到任何 `if cfg.CodarEnabled {` 行 —— 空匹配不算通过")
	}

	// 反向：注册调用本身也必须在顶层块里（缩进同样是 2 层的语句级）。
	if !strings.Contains(src, "\n\t\tif err := registry.Register(cd); err != nil {") {
		t.Error("main.go 里没有顶层块内的 `registry.Register(cd)` —— " +
			"codar 没有被真正注册进注册表")
	}
}
