// state_test.go U-10 状态文件落盘的判据。
//
// # 三件必须成立的事
//
//	① **先判定再打标** —— 反过来会把"盘上是 v3"覆盖成 v1，
//	   「数据比二进制新」这个事实被自己抹掉
//	② 文件**损坏不许当首次启动** —— 静默覆盖会把迁移日志一起丢掉
//	③ binary_too_old 时**连写都不能写** —— 写就是拿旧代码改新数据
package channelstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.json")
}

// 首次启动：打上初始版本（不是 LocalSchemaVersion）。
func TestStateMarksInitialOnFirstStartup(t *testing.T) {
	p := statePath(t)
	compat, st, err := StartupGuard(p)
	if err != nil {
		t.Fatal(err)
	}
	if compat.Verdict != CompatUnmarked {
		t.Fatalf("首次启动应判 unmarked，实际 %q", compat.Verdict)
	}
	if st.SchemaVersion != InitialSchemaVersion {
		t.Fatalf("★ 打标应写 InitialSchemaVersion=%d（不是 %d）——"+
			"直接标成二进制版本会让「迁移跑了一半」永久不可见",
			InitialSchemaVersion, LocalSchemaVersion)
	}
	// 落盘了。
	if _, found, err := LoadState(p); err != nil || !found {
		t.Fatalf("打标后文件应存在：found=%v err=%v", found, err)
	}
}

// ★ 先判定再打标：盘上是 v3 时，**不许**被覆盖成 v1。
func TestStateDoesNotClobberNewerVersion(t *testing.T) {
	p := statePath(t)
	if err := SaveState(p, PersistedState{SchemaVersion: LocalSchemaVersion + 1}); err != nil {
		t.Fatal(err)
	}

	compat, st, err := StartupGuard(p)
	if err != nil {
		t.Fatal(err)
	}
	if compat.Verdict != CompatBinaryTooOld || !compat.MustBlockStartup() {
		t.Fatalf("★ 盘上 v%d 比二进制 v%d 新 ⇒ 必须判 binary_too_old 并阻塞启动，实际 %q",
			LocalSchemaVersion+1, LocalSchemaVersion, compat.Verdict)
	}
	if st.SchemaVersion != LocalSchemaVersion+1 {
		t.Fatalf("★ 盘上的版本被改写成 %d —— 判定跑在打标之后，"+
			"「数据比二进制新」这个事实被自己抹掉了", st.SchemaVersion)
	}
	// 磁盘上也必须还是 v3。
	after, _, err := LoadState(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.SchemaVersion != LocalSchemaVersion+1 {
		t.Fatalf("★ 磁盘上的版本被改成 %d（应为 %d）", after.SchemaVersion, LocalSchemaVersion+1)
	}
}

// ★ 文件损坏 ⇒ 报错，不当首次启动。
func TestStateCorruptIsNotTreatedAsFresh(t *testing.T) {
	p := statePath(t)
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := StartupGuard(p)
	if err == nil {
		t.Fatal("★ 状态文件损坏必须报错 —— 当成首次启动会静默覆盖，把迁移日志一起丢掉")
	}
	if !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("错误信息应说清是损坏，实际 %q", err.Error())
	}
	// 原文件内容不许被改写。
	raw, _ := os.ReadFile(p)
	if string(raw) != "{not json" {
		t.Fatalf("损坏的文件被改写了：%q", string(raw))
	}
}

// 已有标记（v1）⇒ 判 need_migration，不阻塞启动，也不覆盖。
func TestStateOlderVersionNeedsMigration(t *testing.T) {
	p := statePath(t)
	if err := SaveState(p, PersistedState{SchemaVersion: InitialSchemaVersion}); err != nil {
		t.Fatal(err)
	}
	compat, st, err := StartupGuard(p)
	if err != nil {
		t.Fatal(err)
	}
	if compat.Verdict != CompatNeedMigration {
		t.Fatalf("v%d + 二进制 v%d 应判 need_migration，实际 %q",
			InitialSchemaVersion, LocalSchemaVersion, compat.Verdict)
	}
	if compat.MustBlockStartup() {
		t.Fatal("数据更旧不该阻塞启动（只读路径还要能用）")
	}
	if st.SchemaVersion != InitialSchemaVersion {
		t.Fatalf("不该覆盖已有标记，实际 %d", st.SchemaVersion)
	}
}

// 迁移日志与版本在**同一个文件**里（一次原子写 = 要么全旧要么全新）。
func TestStatePersistsJournalWithVersion(t *testing.T) {
	p := statePath(t)
	j := NewJournal("plan-x", [][2]string{{"token:app", Checksum("app")}})
	if err := j.Commit("token:app"); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, PersistedState{SchemaVersion: LocalSchemaVersion, Journal: j}); err != nil {
		t.Fatal(err)
	}

	st, found, err := LoadState(p)
	if err != nil || !found {
		t.Fatalf("读取失败：found=%v err=%v", found, err)
	}
	if st.Journal == nil {
		t.Fatal("日志丢了")
	}
	if !st.Journal.Done() {
		t.Fatalf("日志状态丢了：Resume=%v", st.Journal.Steps)
	}
	// 版本也在同一个文件里。
	if st.SchemaVersion != LocalSchemaVersion {
		t.Fatalf("版本 = %d，want %d", st.SchemaVersion, LocalSchemaVersion)
	}
}

// 打标保留已有日志（只补版本，不重置）。
func TestMarkInitialKeepsJournal(t *testing.T) {
	p := statePath(t)
	j := NewJournal("plan-y", [][2]string{{"token:a", Checksum("a")}})
	_ = j.Commit("token:a")
	if err := SaveState(p, PersistedState{Journal: j}); err != nil { // 故意不写版本
		t.Fatal(err)
	}
	st, err := MarkInitial(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != InitialSchemaVersion {
		t.Fatalf("应补上版本 %d，实际 %d", InitialSchemaVersion, st.SchemaVersion)
	}
	if st.Journal == nil || !st.Journal.Done() {
		t.Fatalf("★ 打标把日志弄丢了：%+v", st.Journal)
	}
}

// 原子写：临时文件最终不存在（不该留垃圾）。
func TestSaveStateLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	if err := SaveState(p, PersistedState{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Fatalf("目录里留下了多余文件：%s", e.Name())
		}
	}
}
