// create_groups_test.go 守「新建 key 时就能指定分组」。
//
// 之前只有 /admin/apikeys/update 能改分组，新建必须先建再 update 一次 ——
// 界面上做不到，调用方得发两个请求。
package apikey

import (
	"path/filepath"
	"testing"
)

// TestCreateOwnedWithGroups 新建时直接带上分组。
func TestCreateOwnedWithGroups(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	k, err := s.CreateOwnedWithGroups("u-1", "app", 0, 0, nil, []string{"g-a", "g-b"})
	if err != nil {
		t.Fatalf("CreateOwnedWithGroups: %v", err)
	}
	groups, ok := s.GroupsOf(k.ID)
	if !ok {
		t.Fatal("key 应存在")
	}
	if len(groups) != 2 {
		t.Errorf("应有 2 个分组，实际 %v", groups)
	}
}

// TestCreateOwnedWithoutGroupsIsUnrestricted 不传分组 = 不限制（兼容存量调用点）。
func TestCreateOwnedWithoutGroupsIsUnrestricted(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	k, err := s.CreateOwned("u-1", "app", 0, 0, nil)
	if err != nil {
		t.Fatalf("CreateOwned: %v", err)
	}
	groups, _ := s.GroupsOf(k.ID)
	if len(groups) != 0 {
		t.Errorf("不传分组应保持不限制，实际 %v", groups)
	}
}

// TestCreateOwnedWithGroupsNormalizes 分组 ID 要去空、去重、TrimSpace。
func TestCreateOwnedWithGroupsNormalizes(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	k, err := s.CreateOwnedWithGroups("u-1", "app", 0, 0, nil,
		[]string{"  g-a  ", "g-a", "", "g-b"})
	if err != nil {
		t.Fatalf("CreateOwnedWithGroups: %v", err)
	}
	groups, _ := s.GroupsOf(k.ID)
	if len(groups) != 2 {
		t.Errorf("应归一化成 2 个（去重去空 Trim），实际 %v", groups)
	}
}
