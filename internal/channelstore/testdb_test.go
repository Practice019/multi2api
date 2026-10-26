// testdb_test.go —— 本包用例的 SQL 底座（B1）。
//
// # 为什么必须有
//
// B1 之前，每个用例用 New(临时 channels.json) 就得到一座**空库**（文件不存在 = 空库）。
// 换成 SQL 之后 New 要求 store.DB 已经打开，所以每个用例必须先开一个临时库并建表。
//
// ⚠ 每个用例各开一个 t.TempDir() 下的库，并在 Cleanup 里 Close：
// store.DB 是**包级单例**，不关掉的话下一个用例会在同一个库上跑，
// 而"空库"这个前提会静默失效（表现是一组莫名其妙的连锁失败）。
package channelstore

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/store"
)

// openTestDB 开一个临时 SQL 库并建表（用例结束时关掉）。
func openTestDB(t *testing.T) {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
}
