package monkeycode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 本文件的测试**全部注入** listProcsFn / killPIDFn —— 绝不碰真实进程。
// （killPID 会执行 taskkill /F，判据写错的后果是杀掉用户自己的浏览器。）

func inject(t *testing.T, procs []procInfo, perr error) (*[]int, func()) {
	t.Helper()
	var killed []int
	oldList, oldKill := listProcsFn, killPIDFn
	listProcsFn = func(string, ...string) ([]procInfo, error) { return procs, perr }
	killPIDFn = func(pid int) error { killed = append(killed, pid); return nil }
	return &killed, func() { listProcsFn, killPIDFn = oldList, oldKill }
}

func mkProfile(t *testing.T, root, name string, age time.Duration) string {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-age)
	if err := os.Chtimes(full, ts, ts); err != nil {
		t.Fatal(err)
	}
	return full
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestSweepKillsLongRunningOrphanEvenWithFreshMtime ★ **T-448 的核心回归测试**。
//
// # 场景（就是实测失败的那个）
//
//	目录 mtime = **现在**（新 —— 因为孤儿进程一直在写它）
//	进程 Age  = **2 小时**（远超登录窗口 10 分钟 ⇒ 一定是孤儿）
//
// # 期望
//
//	killPIDFn 被调 1 次 + 目录**被删**（即使 mtime 是新的）
//
// # 为什么这条能抓住漏洞
//
//	改造前：mtime 新 ⇒ 闸门 continue ⇒ 什么都不做 ✗（实测 removed=1 但目录与 8 个孤儿都还在）
//	改造后：先看**进程年龄** ⇒ 2h > 30min ⇒ 杀 + 删 ✓（绕开 mtime 死结）
func TestSweepKillsLongRunningOrphanEvenWithFreshMtime(t *testing.T) {
	dir := t.TempDir()
	name := loginProfilePrefix + "8888"
	full := mkProfile(t, dir, name, 0) // mtime = 现在（新）
	killed, restore := inject(t, []procInfo{{PID: 4242, Age: 2 * time.Hour}}, nil)
	defer restore()

	removed, failed := SweepStaleLoginProfiles(dir, time.Hour)

	if len(*killed) != 1 || (*killed)[0] != 4242 {
		t.Errorf("killPIDFn 调用 = %v，want [4242] —— 启动 2 小时的进程必是孤儿，"+
			"不杀它这个目录就永远删不掉（孤儿会持续刷新 mtime）", *killed)
	}
	if exists(full) {
		t.Error("目录仍在 —— mtime 新不该救它：已证明占用者全是孤儿（启动 2h > 登录窗口 10min）")
	}
	if removed != 1 || failed != 0 {
		t.Errorf("removed=%d failed=%d，want 1/0", removed, failed)
	}
}

// TestSweepKeepsRecentProcEvenIfMtimeOld 进程可能是**当前登录**时不许杀。
//
// mtime 老（登录一开始就卡住没写）+ 进程只跑了 5 分钟 ⇒ 可能是正在进行的登录 ⇒ 保留进程。
func TestSweepKeepsRecentProcEvenIfMtimeOld(t *testing.T) {
	dir := t.TempDir()
	name := loginProfilePrefix + "7777"
	full := mkProfile(t, dir, name, 2*time.Hour)
	killed, restore := inject(t, []procInfo{{PID: 5555, Age: 5 * time.Minute}}, nil)
	defer restore()

	_, _ = SweepStaleLoginProfiles(dir, time.Hour)

	if len(*killed) != 0 {
		t.Errorf("杀了 %v —— 只跑了 5 分钟的进程可能正是**当前这次登录**（窗口 10 分钟）", *killed)
	}
	// mtime 老 ⇒ 它会被下面的 mtime 流程删掉 ✓（原有行为，与本轮改动不冲突）
	if exists(full) {
		t.Error("mtime 老 + 进程不是孤儿 ⇒ 仍应被 mtime 流程删掉（原有行为）")
	}
}

// TestSweepRemovesOldDirWithoutProcs 原有行为：mtime 老 + 无进程 ⇒ 删，且**不杀任何进程**。
func TestSweepRemovesOldDirWithoutProcs(t *testing.T) {
	dir := t.TempDir()
	full := mkProfile(t, dir, loginProfilePrefix+"6666", 2*time.Hour)
	killed, restore := inject(t, nil, nil)
	defer restore()

	removed, failed := SweepStaleLoginProfiles(dir, time.Hour)

	if len(*killed) != 0 {
		t.Errorf("没有进程可杀却调了 killPIDFn %v", *killed)
	}
	if exists(full) || removed != 1 || failed != 0 {
		t.Errorf("removed=%d failed=%d 存在=%v，want 1/0/false", removed, failed, exists(full))
	}
}

// TestSweepKeepsFreshDirWithoutProcs 原有行为：mtime 新 + 无进程 ⇒ 不删。
//
// 理由：用户刚点「添加账号」，浏览器还没起来（中间有几秒窗口）⇒ 不能删他要用的目录。
func TestSweepKeepsFreshDirWithoutProcs(t *testing.T) {
	dir := t.TempDir()
	full := mkProfile(t, dir, loginProfilePrefix+"5555", 0)
	_, restore := inject(t, nil, nil)
	defer restore()

	SweepStaleLoginProfiles(dir, time.Hour)

	if !exists(full) {
		t.Error("mtime 新 + 无进程的目录被删了 —— 用户刚点登录、浏览器还没起来的窗口里" +
			"删掉它会让登录失败")
	}
}

// TestSweepSurvivesEnumerationError 枚举失败时**不能**因此不删目录。
//
// 理由：枚举是"尽力而为"的辅助 —— 它坏了（PowerShell 被策略禁）不该连累
// "删陈旧目录"这件本来就能做的事。否则一个辅助功能把主功能一起拖死。
func TestSweepSurvivesEnumerationError(t *testing.T) {
	dir := t.TempDir()
	full := mkProfile(t, dir, loginProfilePrefix+"4444", 2*time.Hour)
	killed, restore := inject(t, nil, errors.New("模拟：powershell 不可用"))
	defer restore()

	removed, failed := SweepStaleLoginProfiles(dir, time.Hour)

	if len(*killed) != 0 {
		t.Errorf("枚举报错却杀了 %d 个进程 —— 没有可信的列表时不许杀", len(*killed))
	}
	if exists(full) || removed != 1 || failed != 0 {
		t.Errorf("枚举报错时仍应删掉陈旧目录（removed=%d failed=%d 存在=%v，want 1/0/false）",
			removed, failed, exists(full))
	}
}

// TestSweepKillsOnlyMatchingBrowsers 误杀防线：needles 必须**同时**含前缀与目录全名。
//
// # 为什么这条是必须的
//
// killPID 会执行 taskkill /F。判据写错 = **杀掉用户的浏览器**。
// 正确的判据是 AND 语义：命令行必须同时含 loginProfilePrefix 与该目录**全名**
// ⇒ 用户自己的 Edge 命令行不含这两样，天然隔离。
// （只传前缀会命中所有临时 profile 的进程 ⇒ 包括正在登录中的那一个 ✗）
func TestSweepKillsOnlyMatchingBrowsers(t *testing.T) {
	dir := t.TempDir()
	target := loginProfilePrefix + "111"
	mkProfile(t, dir, target, 2*time.Hour)

	var calls [][]string
	oldList, oldKill := listProcsFn, killPIDFn
	defer func() { listProcsFn, killPIDFn = oldList, oldKill }()
	listProcsFn = func(exe string, needles ...string) ([]procInfo, error) {
		calls = append(calls, append([]string{exe}, needles...))
		return nil, nil
	}
	killPIDFn = func(int) error { return nil }

	SweepStaleLoginProfiles(dir, time.Hour)

	if len(calls) == 0 {
		t.Fatal("枚举一次都没被调用 —— 那就不可能发现孤儿")
	}
	for _, c := range calls {
		if c[0] != "msedge.exe" {
			t.Errorf("枚举的进程名 = %q，want msedge.exe", c[0])
		}
		var hasPrefix, hasDir bool
		for _, n := range c[1:] {
			if n == loginProfilePrefix {
				hasPrefix = true
			}
			if n == target {
				hasDir = true
			}
		}
		if !hasPrefix || !hasDir {
			t.Errorf("needles = %v —— 必须**同时**含前缀与目录全名（AND 语义）。"+
				"只传前缀会命中正在登录中的那个 profile ⇒ 误杀用户正在用的浏览器", c[1:])
		}
	}
}
