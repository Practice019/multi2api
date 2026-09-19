package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"domain":""},"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.AccessToken != "at" || sa.RefreshToken != "rt" || sa.ExpiresAt != 1753600000 {
		t.Errorf("tokens: %+v", sa)
	}
	if sa.UID != "u1" || sa.EnterpriseID != "e1" || sa.Nickname != "n1" {
		t.Errorf("account: %+v", sa)
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2"}`)
	sa, err := Parse(raw)
	if err != nil || sa.UID != "u2" || sa.AccessToken != "at" {
		t.Fatalf("flat: %+v %v", sa, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" {
		t.Errorf("roundtrip: %+v", b)
	}
}

// TestLoadDirLoadsAllValid 不再按 region 过滤：所有可解析的 auth 文件都被加载，
// 解析失败的文件静默跳过。
func TestLoadDirLoadsAllValid(t *testing.T) {
	dir := t.TempDir()
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"cn1"}}`
	other := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"example.com"},"account":{"uid":"u2"}}`
	bad := `not json`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(cn), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-u2.json"), []byte(other), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(bad), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 valid accounts, got %+v", list)
	}
	for _, a := range list {
		if a.FilePath == "" {
			t.Error("FilePath not set")
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
}

// TestDeriveChannel 渠道推导：workbuddy.ai → intl，其余（含空）→ cn。
func TestDeriveChannel(t *testing.T) {
	cases := []struct {
		domain string
		want   string
	}{
		{"", ChannelCN},
		{"copilot.tencent.com", ChannelCN},
		{"www.codebuddy.cn", ChannelCN},
		{"www.workbuddy.ai", ChannelIntl},
		{"workbuddy.ai", ChannelIntl},
		{"WWW.WORKBUDDY.AI", ChannelIntl}, // 大小写不敏感
	}
	for _, c := range cases {
		if got := DeriveChannel(c.domain); got != c.want {
			t.Errorf("DeriveChannel(%q) = %q, want %q", c.domain, got, c.want)
		}
	}
}

// TestParseChannelFallback 旧凭证（无 channel 字段）按 domain 推导渠道。
func TestParseChannelFallback(t *testing.T) {
	intl := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"www.workbuddy.ai"},"account":{"uid":"u-intl"}}`
	a, err := Parse([]byte(intl))
	if err != nil {
		t.Fatalf("parse intl: %v", err)
	}
	if a.Channel != ChannelIntl {
		t.Errorf("intl fallback: got %q, want %q", a.Channel, ChannelIntl)
	}

	cn := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"copilot.tencent.com"},"account":{"uid":"u-cn"}}`
	a2, err := Parse([]byte(cn))
	if err != nil {
		t.Fatalf("parse cn: %v", err)
	}
	if a2.Channel != ChannelCN {
		t.Errorf("cn fallback: got %q, want %q", a2.Channel, ChannelCN)
	}

	// 显式 channel 优先于 domain 推导。
	explicit := `{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"copilot.tencent.com","channel":"intl"},"account":{"uid":"u-x"}}`
	a3, err := Parse([]byte(explicit))
	if err != nil {
		t.Fatalf("parse explicit: %v", err)
	}
	if a3.Channel != ChannelIntl {
		t.Errorf("explicit channel: got %q, want %q", a3.Channel, ChannelIntl)
	}
}

// TestSaveAtomicWritesChannel channel 字段随 auth 块落盘，重读不变。
func TestSaveAtomicWritesChannel(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u-intl.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		Domain: "www.workbuddy.ai", Channel: ChannelIntl,
		UID: "u-intl", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	b, err := Parse(mustRead(t, fp))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.Channel != ChannelIntl {
		t.Errorf("roundtrip channel: got %q, want %q", b.Channel, ChannelIntl)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}
