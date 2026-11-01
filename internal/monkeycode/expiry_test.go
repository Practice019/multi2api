package monkeycode

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestTokenExpiryFromAddedAt 入库章优先：+30 天就是推断的到期时刻。
func TestTokenExpiryFromAddedAt(t *testing.T) {
	added := time.Date(2026, 10, 23, 18, 26, 0, 0, time.Local)
	p := &Provider{}
	at, ok := p.TokenExpiry(gateway.Credential{Secret: &Auth{Session: "s", AddedAt: added.Format(time.RFC3339)}})
	if !ok {
		t.Fatal("有入库时刻就该能报出到期时刻")
	}
	want := added.Add(SessionLifetime).Unix()
	if at != want {
		t.Errorf("到期时刻 = %d，want %d（入库 + 30 天）", at, want)
	}
}

// TestTokenExpiryFallsBackToFileMtime 没有入库章时用文件 mtime（≈ 写进本仓的时刻）。
//
// 为什么这条重要：现有账号（注册机导入的那批）**文件里没有 added_at**，
// 而用户看到的正是这批号 —— 没有这条回落，那一列对它们全是空的。
func TestTokenExpiryFallsBackToFileMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monkeycode-x.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 10, 23, 23, 22, 0, 0, time.Local)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	p := &Provider{}
	at, ok := p.TokenExpiry(gateway.Credential{Secret: &Auth{Session: "s", FilePath: path}})
	if !ok {
		t.Fatal("有文件就该能回落到 mtime")
	}
	if want := mtime.Add(SessionLifetime).Unix(); at != want {
		t.Errorf("到期时刻 = %d，want %d（mtime + 30 天）", at, want)
	}
}

// TestTokenExpiryUnknownIsNotZero 未知必须是 (0,false)，不能是 0 秒。
//
// 把未知写成 0 的后果是界面渲染成「1970 年就过期了」——比不显示更糟
// （本仓在别的上游上踩过，见 admin.go 里那段注释）。
func TestTokenExpiryUnknownIsNotZero(t *testing.T) {
	p := &Provider{}
	for _, c := range []struct {
		what string
		cred gateway.Credential
	}{
		{"secret 类型不对", gateway.Credential{Secret: "不是 Auth"}},
		{"secret 为空", gateway.Credential{}},
		{"既没有入库章也没有文件", gateway.Credential{Secret: &Auth{Session: "s", AddedAt: "看不懂的时间"}}},
		{"文件不存在", gateway.Credential{Secret: &Auth{Session: "s", FilePath: filepath.Join(t.TempDir(), "没有这个文件.json")}}},
	} {
		at, ok := p.TokenExpiry(c.cred)
		if ok || at != 0 {
			t.Errorf("%s：应当 (0,false)，得到 (%d,%v)", c.what, at, ok)
		}
	}
}

// TestAccountColumnsHasTokenExpiryButNotToken 列集：到期要在，bearer 那列不要。
//
// 这条守的是用户报的那个 bug **反过来**再犯：
// 2026-11-01 之前 token_expiry 被从列集里删掉过一次，而那次"删干净"的外观
// 很合理（注释还写着理由），所以必须有一条判据钉住。
func TestAccountColumnsHasTokenExpiryButNotToken(t *testing.T) {
	cols := (&Provider{}).AccountColumns()
	seen := map[string]bool{}
	for _, c := range cols {
		seen[c] = true
	}
	if !seen[gateway.AccountColTokenExpiry] {
		t.Error("列集里缺 token_expiry —— 用户在界面上就看不到「哪个号要到期」了")
	}
	if seen[gateway.AccountColToken] {
		t.Error("列集里出现了 token —— 本上游没有 bearer，这一列会恒为 —，只会让用户以为凭证坏了")
	}
	// 今日额度那一列要一直在（用户之前专门要过，见文件头）。
	if !seen[gateway.AccountColFreeQuota] {
		t.Error("列集里缺 free_quota（今日额度）")
	}
}
