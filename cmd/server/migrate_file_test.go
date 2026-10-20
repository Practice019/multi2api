// cmd/server/migrate_file_test.go —— migrateFileIfAbsent 的守卫（2026-10-20）
//
// # 这个函数为什么值得 5 条测试
//
// 它搬的是**渠道 API key**：`data/channels.json` → `<凭据根>/channels.json`。
// 一次错误的实现会：
//   - 覆盖用户在新位置之后的修改（丢 key）
//   - 留下半截文件（下次启动认为"已迁移"而跳过，永久半坏）
//   - 权限写成 0644（key 被同机其他用户读到）
//
// 所以每条语义单独钉住。
package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMigrateCopiesWhenTargetAbsent 正常迁移：内容一致、权限 0600、源保留。
func TestMigrateCopiesWhenTargetAbsent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "data", "channels.json")
	dst := filepath.Join(dir, "auths", "channels.json")
	os.MkdirAll(filepath.Dir(src), 0o755)

	content := []byte(`{"version":1,"channels":[{"id":"ch-1","keys":[{"key":"sk-secret","status":1}]}]}`)
	if err := os.WriteFile(src, content, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := migrateFileIfAbsent(src, dst); err != nil {
		t.Fatalf("迁移报错: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("★ 目标文件不存在（迁移没生效）: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("★ 内容不一致\n期望: %s\n实际: %s", content, got)
	}

	// 源必须保留（回滚路径）
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("★ 源文件被删了 —— 回滚时旧版 exe 读不到数据: %v", err)
	}

	// 权限：含 key，必须 0600
	//
	// ⚠ 只在**非 Windows** 上断言。Windows 的文件权限模型没有 0600 这个概念
	//   （`os.WriteFile` 的 perm 参数只影响只读位），实测拿到的是 0666 ——
	//   那不是代码写错，是平台表达能力不同。
	//   CI 在 ubuntu 上跑 ⇒ 这条断言在那里是真的在检查。
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(dst); err == nil {
			if mode := fi.Mode().Perm(); mode != 0o600 {
				t.Fatalf("★ 目标权限是 %o，应为 600（这个文件里有 API key）", mode)
			}
		}
	} else {
		t.Log("（Windows：文件权限模型无 0600，跳过权限断言；Linux 上会真检查）")
	}
}

// TestMigrateDoesNotOverwriteExistingTarget ★ 最重要的一条：目标已存在则绝不覆盖。
//
// 这是幂等的前提，也是"用户在新位置改过之后重启不会被回滚"的保证。
func TestMigrateDoesNotOverwriteExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "channels.json")
	dst := filepath.Join(dir, "auths_ch", "channels.json")
	os.MkdirAll(filepath.Dir(dst), 0o755)

	os.WriteFile(src, []byte(`{"old":true}`), 0o600)
	os.WriteFile(dst, []byte(`{"new":true,"user_edited":"yes"}`), 0o600)

	if err := migrateFileIfAbsent(src, dst); err != nil {
		t.Fatalf("不应报错: %v", err)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != `{"new":true,"user_edited":"yes"}` {
		t.Fatalf("★ 目标被覆盖了 —— 用户在新位置的修改被抹掉。实际内容: %s", got)
	}
}

// TestMigrateNoSourceIsNotError 首次部署没有旧文件是正常状态，不该报错。
func TestMigrateNoSourceIsNotError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "nonexistent.json")
	dst := filepath.Join(dir, "auths", "channels.json")

	if err := migrateFileIfAbsent(src, dst); err != nil {
		t.Fatalf("★ 源不存在时不该报错（首次部署就会走到这里）: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("源不存在时不该创建目标文件")
	}
}

// TestMigrateCreatesParentDir 全新机器上 auths/ 可能还不存在。
func TestMigrateCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "channels.json")
	dst := filepath.Join(dir, "deeply", "nested", "auths", "channels.json")
	os.WriteFile(src, []byte(`{}`), 0o600)

	if err := migrateFileIfAbsent(src, dst); err != nil {
		t.Fatalf("应自动建父目录: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("★ 父目录没建成: %v", err)
	}
}

// TestMigrateLeavesNoTempFile 迁移后不该留下 .tmp 半成品。
//
// ⚠ 半成品的危害不在"占空间"，而在：下次启动看到目标已存在就跳过迁移，
//
//	于是磁盘上永远是那份半截数据，而且没人知道。
func TestMigrateLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "channels.json")
	dst := filepath.Join(dir, "auths", "channels.json")
	os.WriteFile(src, []byte(`{"a":1}`), 0o600)

	if err := migrateFileIfAbsent(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("★ 留下了 .tmp 半成品")
	}
}

// TestMigrateEmptyDstPathIsError 目标是空串时必须报错，不能"默认往某处写"。
func TestMigrateEmptyDstPathIsError(t *testing.T) {
	if err := migrateFileIfAbsent("whatever.json", ""); err == nil {
		t.Fatal("★ 目标路径为空时应报错")
	}
}
