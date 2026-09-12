package codearts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeCredDoc 造一份可落盘的凭证 JSON。
func makeCredDoc(ak, rt string) []byte {
	doc := credFile{
		Auth: credBody{
			AccessKey:     ak,
			SecretKey:     "SK_" + ak,
			SecurityToken: "ST_" + ak,
			ExpiresAt:     time.Now().Add(30 * time.Minute).Unix(),
			RefreshToken:  rt,
		},
		Account:  accountBlock{UID: ak},
		DPoP:     dpopBlock{PrivateKeyJWK: json.RawMessage(`{"kty":"EC","crv":"P-256","x":"a","y":"b","d":"c"}`)},
		ClientID: "vscode-codebot",
	}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return b
}

// TestBackupBeforeRefreshAndCleanAfter 锁定两阶段落盘的核心不变量：
//
// 续期是"消费型"操作 —— 服务端把用过的 refresh_token 立即作废。
// 若在"请求成功但未落盘"之间失败，凭证就永久报废。
// 因此必须：
//  1. 续期前把当前凭证整份备份
//  2. 成功后先写新凭证、再删备份
//  3. 写盘失败时备份保留（供诊断"上次续期在哪一步挂了"）
func TestBackupBeforeRefreshAndCleanAfter(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "codearts-AK1.json")
	if err := os.WriteFile(credPath, makeCredDoc("AK1", "OLD_RT"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := ParseCredential(makeCredDoc("AK1", "OLD_RT"))
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = credPath

	bakDir := BackupDir(dir)

	// 阶段 1：备份
	if err := a.BackupTo(bakDir); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	entries, err := os.ReadDir(bakDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("备份目录应有文件，实际 err=%v entries=%d", err, len(entries))
	}

	// 模拟"服务端已消费旧 RT、本地拿到新凭证"
	a.AccessKey = "AK1"
	a.RefreshToken = "NEW_RT"

	// 阶段 2：写新凭证 + 清备份
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	if err := a.ClearBackup(bakDir); err != nil {
		t.Fatalf("ClearBackup: %v", err)
	}

	// 备份应已清空
	entries, _ = os.ReadDir(bakDir)
	if len(entries) != 0 {
		t.Errorf("成功后备份应清空，实际剩 %d 个", len(entries))
	}

	// 主文件应是新凭证
	raw, _ := os.ReadFile(credPath)
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.RefreshToken != "NEW_RT" {
		t.Errorf("主文件 refresh_token = %q, 期望 NEW_RT", back.RefreshToken)
	}
}

// TestBackupSurvivesWhenSaveFails 证明写盘失败时主文件不被破坏、备份保留。
//
// 这是 D1 的核心防护：宁可"没续期成功"，也不能"把可用凭证写成半更新"。
func TestBackupSurvivesWhenSaveFails(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "codearts-AK2.json")
	original := makeCredDoc("AK2", "GOOD_RT")
	if err := os.WriteFile(credPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := ParseCredential(original)
	if err != nil {
		t.Fatal(err)
	}
	a.FilePath = credPath

	bakDir := BackupDir(dir)
	if err := a.BackupTo(bakDir); err != nil {
		t.Fatal(err)
	}

	// 让写盘失败：把 FilePath 指向一个不存在的目录
	a.FilePath = filepath.Join(dir, "no-such-dir", "x.json")
	a.RefreshToken = "NEW_RT"
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("写盘应失败")
	}

	// 备份必须还在（这是诊断依据）
	entries, _ := os.ReadDir(bakDir)
	if len(entries) == 0 {
		t.Error("写盘失败后备份被删了 —— 将无法诊断上次续期停在哪一步")
	}

	// 主文件必须保持原样
	raw, _ := os.ReadFile(credPath)
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("主文件被破坏: %v", err)
	}
	if back.RefreshToken != "GOOD_RT" {
		t.Errorf("主文件 refresh_token 被改成 %q，应为 GOOD_RT", back.RefreshToken)
	}
}

// TestFindStaleBackups 确认启动时能发现"上次续期未完成"的残留。
func TestFindStaleBackups(t *testing.T) {
	dir := t.TempDir()
	bakDir := BackupDir(dir)

	// 无备份时返回空
	got, err := FindStaleBackups(dir)
	if err != nil {
		t.Fatalf("FindStaleBackups: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("无备份时应返回空，实际 %v", got)
	}

	// 造一个残留备份
	if err := os.MkdirAll(bakDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bakDir, "codearts-AK3.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err = FindStaleBackups(dir)
	if err != nil {
		t.Fatalf("FindStaleBackups: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应发现 1 个残留备份，实际 %d", len(got))
	}
	if filepath.Base(got[0]) != "codearts-AK3.json" {
		t.Errorf("返回路径不对: %s", got[0])
	}
}

// TestBackupIsFullCopy 确认备份是**整份**凭证（含 DPoP 私钥与 refresh_token）。
//
// 只备份一半没用：恢复诊断时需要看到当时的 refresh_token 与密钥。
func TestBackupIsFullCopy(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "codearts-AK4.json")
	raw := makeCredDoc("AK4", "RT4")
	if err := os.WriteFile(credPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, _ := ParseCredential(raw)
	a.FilePath = credPath

	bakDir := BackupDir(dir)
	if err := a.BackupTo(bakDir); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(bakDir)
	if len(entries) != 1 {
		t.Fatalf("应恰好 1 个备份，实际 %d", len(entries))
	}
	bakRaw, err := os.ReadFile(filepath.Join(bakDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(bakRaw)
	if err != nil {
		t.Fatalf("备份不是合法凭证: %v", err)
	}
	if back.RefreshToken != "RT4" {
		t.Error("备份缺 refresh_token")
	}
	if len(back.DPoPPrivateKeyJWK) == 0 {
		t.Error("备份缺 DPoP 私钥")
	}
}
