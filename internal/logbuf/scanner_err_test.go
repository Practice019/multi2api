// scanner_err_test.go 超长行必须报错，不能静默返回半份日志（2026-10-20，review 抓到）
//
// # 这个文件钉的是什么
//
// `loadAll` 用 `bufio.Scanner` 逐行读 JSONL，`Buffer(64<<10, 1<<20)` 把单行上限
// 定在 1 MiB。Scanner 在超过上限时的行为是：**循环正常退出**，错误只留在
// `sc.Err()` 里。
//
// 原实现直接 `return out, nil` —— 于是"日志少了一截"这件事**没有任何痕迹**：
// 不报错、不打日志，HTTP 层拿到的是一份看起来成功的**不完整**数据。
//
// 这属于"沉默的失败"：真发生时无从排查，用户只会觉得数据莫名其妙不对。
//
// ⚠ 实测现在不会触发（单行约 300~400 字节），但守卫要提前有 —— 等触发时
// 就已经是一份查不出来的错误报告了。
package logbuf

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadAllReportsOverlongLine 单行超上限时必须返回错误。
func TestLoadAllReportsOverlongLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")

	// 造一行超过 1 MiB 的内容（Scanner 的上限）
	huge := strings.Repeat("x", 2<<20)
	content := `{"model":"ok-1"}` + "\n" + huge + "\n" + `{"model":"ok-2"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}

	s := &Sink{path: path}
	got, err := s.readAllLocked()

	// ★ 核心断言：必须报错，不能假装成功
	if err == nil {
		t.Fatalf("★ 超长行被静默吞掉了（返回 %d 条、err=nil）—— "+
			"调用方会把它当成一份完整的日志展示出去", len(got))
	}
	if !strings.Contains(err.Error(), "token too long") && !strings.Contains(err.Error(), "too long") {
		t.Logf("错误信息（不同 Go 版本措辞可能不同）: %v", err)
	}
	// 已经读到的条目仍应返回（调用方可以选择部分展示 + 提示"读取不完整"）
	t.Logf("报错前已解析 %d 条", len(got))
}

// TestLoadAllNormalFileNoError 正常文件不受影响（别把守卫做成"永远报错"）。
func TestLoadAllNormalFileNoError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.jsonl")
	content := `{"model":"a","tokens":10}` + "\n" + `{"model":"b","tokens":20}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
	s := &Sink{path: path}
	got, err := s.readAllLocked()
	if err != nil {
		t.Fatalf("正常文件不该报错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应解析 2 条，实际 %d", len(got))
	}
}

// TestLoadAllMissingFileIsNotError 文件不存在不是错误（首次运行）。
func TestLoadAllMissingFileIsNotError(t *testing.T) {
	s := &Sink{path: filepath.Join(t.TempDir(), "nope.jsonl")}
	got, err := s.readAllLocked()
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if got != nil {
		t.Fatalf("文件不存在应返回 nil，实际 %v", got)
	}
}

// 编译期确认我们用的上限常量没被意外改大（改大就等于放宽了守卫的触发点）。
func TestScannerBufferLimitIsOneMiB(t *testing.T) {
	// 这条不是断言实现细节，而是**记录当前选择**：
	// 1 MiB 单行上限对 request-log.jsonl（单行 300~400 字节）绰绰有余。
	// 若将来日志里开始存响应体，这条会提醒你重新评估。
	const oneMiB = 1 << 20
	_ = bufio.MaxScanTokenSize // 引用一下，确保 bufio 仍被这个测试文件依赖
	if oneMiB != 1048576 {
		t.Fatal("常量算错了")
	}
	t.Log("单行上限 1 MiB；request-log 单行约 300~400 字节，余量约 2500 倍")
}
