package apikey

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestCreateValidateBump 生命周期：创建 → 校验通过 → 记用量 → 额度用尽拒绝。
func TestCreateValidateBump(t *testing.T) {
	s := newTestStore(t)
	k, err := s.Create("test-app", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.ID, "sk-") {
		t.Fatalf("key 应以 sk- 开头: %s", k.ID)
	}
	id, err := s.Validate(k.ID)
	if err != nil || id != k.ID {
		t.Fatalf("Validate = %q,%v want %q,nil", id, err, k.ID)
	}
	s.BumpUsage(k.ID, 60, true)
	s.BumpUsage(k.ID, 60, true) // 60+60 > 100
	if _, err := s.Validate(k.ID); !errors.Is(err, ErrQuota) {
		t.Errorf("额度应耗尽: %v", err)
	}
	if got := s.List()[0].Remain; got != 0 {
		t.Errorf("Remain=%d want 0", got)
	}
}

// TestValidateDisabled 禁用后拒绝（401 语义）。
func TestValidateDisabled(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("a", 0, 0)
	if _, err := s.Toggle(k.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Validate(k.ID); !errors.Is(err, ErrDisabled) {
		t.Errorf("禁用后应拒绝: %v", err)
	}
}

// TestValidateUnknown 未知 key 拒绝。
func TestValidateUnknown(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Validate("sk-nonexistent"); !errors.Is(err, ErrUnknown) {
		t.Errorf("未知 key 应 ErrUnknown: %v", err)
	}
}

// TestRateLimit RPM 上限（429 语义）。
func TestRateLimit(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("rpm", 0, 3)
	for i := 0; i < 3; i++ {
		if _, err := s.Validate(k.ID); err != nil {
			t.Fatalf("第 %d 次应通过: %v", i+1, err)
		}
	}
	if _, err := s.Validate(k.ID); !errors.Is(err, ErrRateLimit) {
		t.Errorf("第 4 次应限速: %v", err)
	}
}

// TestPersistReload 持久化：重新加载后状态保留。
func TestPersistReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apikeys.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := s.Create("persist", 50, 0)
	s.BumpUsage(k.ID, 30, true)
	s.Toggle(k.ID, false)

	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.List()) != 1 {
		t.Fatalf("重载后应有 1 把 key，实际 %d", len(s2.List()))
	}
	if _, err := s2.Validate(k.ID); !errors.Is(err, ErrDisabled) {
		t.Errorf("重载后应仍是禁用: %v", err)
	}
	v := s2.List()[0]
	if v.Used != 30 || v.Name != "persist" {
		t.Errorf("重载后用量/名称丢失: %+v", v)
	}
}

// TestDeleteReset 删除与重置。
func TestDeleteReset(t *testing.T) {
	s := newTestStore(t)
	k, _ := s.Create("d", 10, 0)
	s.BumpUsage(k.ID, 5, true)
	if err := s.Reset(k.ID); err != nil {
		t.Fatal(err)
	}
	if s.List()[0].Used != 0 {
		t.Error("Reset 应清空已用")
	}
	if err := s.Delete(k.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Error("Delete 后应无 key")
	}
	if err := s.Delete(k.ID); !errors.Is(err, ErrUnknown) {
		t.Errorf("重复删除应 ErrUnknown: %v", err)
	}
}

// TestMaskID 掩码不暴露完整 key。
func TestMaskID(t *testing.T) {
	m := MaskID("sk-a1b2c3d4e5f6a7b8c9d0e1f2")
	if strings.Contains(m, "a1b2c3d4e5f6a7b8c9d0e1f2") {
		t.Errorf("掩码泄露完整 key: %s", m)
	}
	if !strings.HasPrefix(m, "sk-a1b2c…e1f2") {
		t.Errorf("掩码应保留前缀+后缀: %s", m)
	}
}
