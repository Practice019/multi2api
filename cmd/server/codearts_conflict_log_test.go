package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"workbuddy2api/internal/logx"
)

// resetConflictLog 清空包级指纹表。
//
// ⚠ 必须每个测试开头调：那张表是**包级**的，跨测试会互相污染
// （第一个测试留下的指纹会让第二个测试的"首次"变成"重复"⇒ 假绿/假红）。
func resetConflictLog() {
	codeartsConflictLogged.mu.Lock()
	codeartsConflictLogged.last = map[string]string{}
	codeartsConflictLogged.mu.Unlock()
}

func captureLog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	restoreLogx := logx.CaptureTo(&buf)
	return &buf, func() { log.SetOutput(old); restoreLogx() }
}

// TestCodeartsConflictLogOnlyOnChange 结论不变就**不打**（T-451 的降噪本体）。
//
// # 为什么这条是核心
//
// codeartsWinners 每次目录扫描都被调用（30 秒一次）⇒ 改造前同一条结论一天打 2880 次，
// 把真日志淹掉。而"结论没变"完全可以从上一次的指纹判出来。
func TestCodeartsConflictLogOnlyOnChange(t *testing.T) {
	resetConflictLog()
	buf, restore := captureLog(t)
	defer restore()

	logCodeartsConflict("u1", 2, "A.json", "B.json")
	logCodeartsConflict("u1", 2, "A.json", "B.json")
	logCodeartsConflict("u1", 2, "A.json", "B.json")

	if n := strings.Count(buf.String(), "份凭证"); n != 1 {
		t.Errorf("同一结论连打 3 次 ⇒ 输出 %d 条，want 1（结论不变就不该打）。实际：\n%s",
			n, buf.String())
	}
}

// TestCodeartsConflictLogOnChange 结论**真的变了**时必须打（可诊断性不丢）。
//
// 三种变化各打一次：被丢弃的文件变了、份数变了；重复的那个不打。
func TestCodeartsConflictLogOnChange(t *testing.T) {
	resetConflictLog()
	buf, restore := captureLog(t)
	defer restore()

	logCodeartsConflict("u1", 2, "A.json", "B.json") // 首次 ⇒ 打
	logCodeartsConflict("u1", 2, "A.json", "C.json") // 被丢弃的变了 ⇒ 打
	logCodeartsConflict("u1", 3, "A.json", "C.json") // 份数变了 ⇒ 打
	logCodeartsConflict("u1", 3, "A.json", "C.json") // 又不变 ⇒ 不打

	got := buf.String()
	// 首次用「有 N 份凭证」措辞；变化用「凭证冲突已变化（现有 N 份）」措辞
	// ⇒ 两次变化合计 2 条，加上首次共 3 行。
	if n := strings.Count(got, "账号 u1"); n != 3 {
		t.Errorf("首次/变化/变化/重复 ⇒ 应打 3 行，实得 %d。实际：\n%s", n, got)
	}
	if n := strings.Count(got, "已变化"); n != 2 {
		t.Errorf("「已变化」措辞出现 %d 次，want 2（两次指纹变化；措辞不同是为了在日志里"+
			"一眼区分'首次发现'与'后来变了'）。实际：\n%s", n, got)
	}
}

// TestCodeartsConflictLogIsPerUID 指纹是**按 uid** 的：一个 uid 的变化不影响另一个。
func TestCodeartsConflictLogIsPerUID(t *testing.T) {
	resetConflictLog()
	buf, restore := captureLog(t)
	defer restore()

	logCodeartsConflict("u1", 2, "A.json", "B.json")
	logCodeartsConflict("u2", 2, "A.json", "B.json") // 另一个 uid 的首次 ⇒ 应打
	logCodeartsConflict("u1", 2, "A.json", "B.json") // u1 没变 ⇒ 不打

	if n := strings.Count(buf.String(), "份凭证"); n != 2 {
		t.Errorf("两个 uid 各首次一次 ⇒ 应 2 条，实得 %d。实际：\n%s", n, buf.String())
	}
}
