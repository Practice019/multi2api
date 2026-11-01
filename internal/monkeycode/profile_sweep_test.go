package monkeycode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// profile_sweep_test.go —— T-031：陈旧 profile 巡检 + "兜底清理不依赖 closure"的源码级断言。
//
// # 为什么必须有这些用例
//
// 用户现场留下了 **111 个** `wb2api-login-*`（22 小时跨度）。它们来自两件事：
//
//	① profile 清理是"睡 1.5 秒再 RemoveAll"—— 拍脑袋的时长，且失败**静默**
//	   （进程清理已经修好，这条是本轮修的）
//	② 更根本的：**进程被杀时谁也走不到 closure**（测试二进制结束/服务重启/Ctrl-C），
//	   于是那个目录永远没人删
//
// ② 无法在进程内根治，所以加了"下次启动扫掉过期的"作为**止血**；它必须只删旧的，
// 绝不碰正在用的。

// TestSweepStaleLoginProfilesRemovesOnlyOldOnes 只删早于阈值的，新的一个都不能碰。
func TestSweepStaleLoginProfilesRemovesOnlyOldOnes(t *testing.T) {
	root := t.TempDir()

	oldDir := filepath.Join(root, loginProfilePrefix+"111")
	newDir := filepath.Join(root, loginProfilePrefix+"222")
	otherDir := filepath.Join(root, "someone-elses-dir") // 不是我们的前缀 ⇒ 不碰
	for _, d := range []string{oldDir, newDir, otherDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("造目录失败: %v", err)
		}
	}
	// 把 old 的 mtime 推回 2 小时前（判据就是 mtime，不是创建时间）
	two := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldDir, two, two); err != nil {
		t.Fatalf("改 mtime 失败: %v", err)
	}

	removed, failed := SweepStaleLoginProfiles(root, time.Hour)
	if failed != 0 {
		t.Errorf("不该有失败项，实得 %d", failed)
	}
	if removed != 1 {
		t.Errorf("应当只清掉 1 个陈旧 profile，实得 %d", removed)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Error("超过阈值的 profile 必须被删掉")
	}
	// ⚠ 这两条是"止血"手段的安全底线：
	if _, err := os.Stat(newDir); err != nil {
		t.Error("**新的 profile 绝不能删**（可能正在被使用）：mtime 判据就是为这条服务的")
	}
	if _, err := os.Stat(otherDir); err != nil {
		t.Error("不是本工具前缀的目录绝不能碰（那可能是别人的数据）")
	}
}

// TestSweepStaleLoginProfilesMissingDirIsNotError 目录不存在不是错误（首次运行就是这样）。
func TestSweepStaleLoginProfilesMissingDirIsNotError(t *testing.T) {
	removed, failed := SweepStaleLoginProfiles(filepath.Join(t.TempDir(), "nope"), time.Hour)
	if removed != 0 || failed != 0 {
		t.Errorf("目录不存在时应当安静返回 (0,0)，实得 (%d,%d)", removed, failed)
	}
}

// TestCleanupFallbackDoesNotDependOnClosure **源码级**断言（用户明确允许的替代方案）：
// `runBrowserLogin` 的 defer 里必须有一条**不依赖 `sess.closure != nil`** 的兜底清理。
//
// 为什么只能这样测：那条路径要触发"closure 没赋值就返回"，需要构造中间态
// （起进程成功、赋值那行没走到），当前代码在时序上恰好不会发生 ——
// 而它**将来会被插一条早返回破坏**。所以把"结构性保证存在"钉在源码上。
func TestCleanupFallbackDoesNotDependOnClosure(t *testing.T) {
	raw, err := os.ReadFile("browserlogin.go")
	if err != nil {
		t.Fatalf("读 browserlogin.go 失败：%v", err)
	}
	src := string(raw)
	for _, want := range []string{
		"killPidTreeBestEffort(pid)",      // 按 pid 的兜底
		"if pid := sess.pidOf(); pid > 0", // 兜底**不依赖 closure 是否为 nil**
		"removeProfileWhenGone",           // profile 清理与进程清理解耦
		"processAlive",                    // 判据是"进程真退出"，不是"睡够 1.5 秒"
	} {
		if !strings.Contains(src, want) {
			t.Errorf("browserlogin.go 里找不到 %q —— 兜底清理的结构性保证被破坏了", want)
		}
	}
	// 老写法必须已经不存在（"睡固定时长再删" + 静默失败）
	if strings.Contains(src, "time.Sleep(1500 * time.Millisecond)") {
		t.Error("仍然存在「睡 1.5 秒再删 profile」的老写法 —— 那正是 111 个残留的成因")
	}
}
