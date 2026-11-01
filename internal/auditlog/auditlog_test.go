package auditlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogWritesJSONLRow 行形状照 lib/audit.js：{at,event,pid,...data}，一行一条。
func TestLogWritesJSONLRow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	l := Open(path)
	if !l.Log("refusal_retry", map[string]any{"chars": 23, "match": 0}) {
		t.Fatal("写入应当成功")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("应当是一行，实际 %d 行", len(lines))
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	for _, k := range []string{"at", "event", "pid"} {
		if _, ok := row[k]; !ok {
			t.Errorf("缺保留字段 %q", k)
		}
	}
	if row["event"] != "refusal_retry" {
		t.Errorf("event = %v", row["event"])
	}
	if row["chars"] != float64(23) {
		t.Errorf("附字段没写进去: %v", row["chars"])
	}
}

// TestLogReservedFieldsNotOverwritten 附字段**不许**覆盖 at/event/pid。
//
// 覆盖了会让审计"看起来有时间"、实际时间是调用方随手给的值 ——
// 而审计的全部价值就是"什么时候发生了什么"。
func TestLogReservedFieldsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	l := Open(path)
	l.Log("probe", map[string]any{"at": "假的", "event": "假的", "pid": -1, "ok": true})
	raw, _ := os.ReadFile(path)
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &row); err != nil {
		t.Fatal(err)
	}
	if row["event"] != "probe" {
		t.Errorf("event 被附字段覆盖成 %v", row["event"])
	}
	if row["at"] == "假的" {
		t.Error("at 被附字段覆盖了")
	}
	if row["pid"] == float64(-1) {
		t.Error("pid 被附字段覆盖了")
	}
	if row["ok"] != true {
		t.Error("正常附字段应当保留")
	}
}

// TestLogRotatesAndKeepsTwo 超过 1 MiB 轮转，保留 .1 / .2（原实现 AUDIT_KEEP=2）。
func TestLogRotatesAndKeepsTwo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	// 直接造一个超过上限的文件（不必真写 1 MiB）。
	big := strings.Repeat("x", MaxBytes+16)
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Open(path)
	if !l.Log("first", nil) {
		t.Fatal("写入应当成功")
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("应当生成 .1（轮转）: %v", err)
	}
	// 再超一次：.1 → .2
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if !l.Log("second", nil) {
		t.Fatal("写入应当成功")
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("应当生成 .2: %v", err)
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("不该出现 .3（只保留 2 份）")
	}
}

// TestLogFailuresAreNotFatal 写不进去只返回 false，不 panic（审计不能变成可用性风险）。
func TestLogFailuresAreNotFatal(t *testing.T) {
	// 指向一个不存在的盘符/非法路径。
	l := Open(filepath.Join("Z:", "不可能", "audit.jsonl"))
	if l.Log("x", nil) {
		t.Error("非法路径不该报告成功")
	}
	// 再写一次仍然不 panic（只警告一次的实现细节不该影响这一点）。
	_ = l.Log("x", nil)
	// 空 event / 空路径也不写。
	if Open("").Log("x", nil) {
		t.Error("空路径不该写入")
	}
}
