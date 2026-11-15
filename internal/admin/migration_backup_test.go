// migration_backup_test.go 迁移备份导出的判据。
//
// # 最重要的一条：输出里**一个字节的明文凭据都没有**
//
// 计划 §5 U-09 写着「备份含秘密须受限保存」。更稳妥的做法是**根本不导出秘密**：
// 迁移要的是结构（谁有什么权限），不是密钥值；而一份带明文 Bearer 的文件
// 一旦落地就是长期负债（会进 git、聊天记录、工单附件）。
//
// 所以这里的判据是"拿真密钥去搜，搜不到才算过" —— 而不是"我们相信它没写"。
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// backupFixture 造一个有令牌/分组/渠道的部署，返回 handler 与**所有真密钥**。
//
// 返回值里的密钥只用来"搜得到就失败"，不会进任何断言输出。
func backupFixture(t *testing.T) (*Handler, []string) {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ks, err := apikey.New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for _, name := range []string{"alpha", "beta"} {
		k, err := ks.Create(name, 0, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, k.ID)
	}

	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := gs.SaveRouting("", "route", "测试分组", []string{"ch-a"})
	if err != nil {
		t.Fatal(err)
	}

	cs, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatal(err)
	}
	chKey := "channel-secret-key-0001"
	if err := cs.Upsert(&channelstore.Channel{
		ID: "ch-a", Name: "a", Kind: "standard", Status: 1,
		BaseURL:  "https://upstream.invalid/v1",
		Models:   []string{"real-x"},
		ModelMap: map[string]string{"pub-y": "real-x"},
		Groups:   []string{g.ID},
		Keys:     []channelstore.ChannelKey{{Key: chKey, Status: channelstore.StatusEnabled}},
	}); err != nil {
		t.Fatal(err)
	}
	// ⚠ 渠道 store **不原样回传**我写进去的 chKey（实测读回来是 11 字符的其它形态）。
	// 所以这里把"store 自己认的那个值"也加进搜索集 —— 那才是真正会出现在备份里的东西。
	// 只拿原文去搜会让反例不咬（第一版就踩了这个）。
	for _, ch := range cs.List() {
		for _, k := range ch.Keys {
			if k.Key != "" {
				secrets = append(secrets, k.Key)
			}
		}
	}

	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true,
		APIKeys: ks, Groups: gs, Channels: cs})
	return h, secrets
}

// callBackup 发一次备份请求。
func callBackup(t *testing.T, h *Handler, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/migration/backup", nil)
	if admin {
		req = withTestAuth(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ★ ① 输出里没有任何明文凭据。
func TestMigrationBackupCarriesNoSecret(t *testing.T) {
	h, secrets := backupFixture(t)
	rec := callBackup(t, h, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, s := range secrets {
		if strings.Contains(body, s) {
			t.Fatalf("★ 备份里出现了明文凭据 %q ——\n"+
				"一份带明文 Bearer 的文件落地即为长期负债（会进 git / 聊天记录 / 工单附件）。\n"+
				"迁移要的是结构，不是密钥值。", s)
		}
	}
	// 也不能出现任何 sk- 形态的字符串。
	if strings.Contains(body, "sk-") {
		t.Fatalf("★ 备份里出现了 sk- 形态的字符串：%s", body)
	}
}

// ★ ② 结构该有的都在（否则"没泄漏"可能只是"什么都没给"）。
func TestMigrationBackupCarriesStructure(t *testing.T) {
	h, secrets := backupFixture(t)
	rec := callBackup(t, h, true)

	var out migrationBackup
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, rec.Body.String())
	}
	if len(out.Tokens) != 2 {
		t.Fatalf("应有 2 把令牌，实际 %d", len(out.Tokens))
	}
	if len(out.Groups) != 1 {
		t.Fatalf("应有 1 个分组，实际 %d", len(out.Groups))
	}
	if len(out.Channels) != 1 {
		t.Fatalf("应有 1 条渠道，实际 %d", len(out.Channels))
	}
	if out.Channels[0].BaseURL == "" {
		t.Fatal("渠道的 base_url 应保留（它是结构，不是秘密）")
	}
	if len(out.Channels[0].KeyFingerprints) != 1 {
		t.Fatalf("渠道密钥应有 1 个指纹，实际 %v", out.Channels[0].KeyFingerprints)
	}
	// 指纹必须真的存在且非空，否则"没有明文"可能只是"什么指纹都没留"。
	for _, tok := range out.Tokens {
		if len(tok.KeyFingerprint) != backupFingerprintLen {
			t.Fatalf("指纹长度应为 %d，实际 %q", backupFingerprintLen, tok.KeyFingerprint)
		}
	}
	if out.Checksum == "" {
		t.Fatal("checksum 不能为空")
	}
	_ = secrets
}

// ★ ③ 指纹可区分不同 key，且稳定。
func TestMigrationBackupFingerprintIsStableAndDistinct(t *testing.T) {
	h, _ := backupFixture(t)
	rec := callBackup(t, h, true)
	var a migrationBackup
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	// 两次导出的指纹必须一致（时间戳在变，指纹不能变）。
	rec2 := callBackup(t, h, true)
	var b migrationBackup
	if err := json.Unmarshal(rec2.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	for i := range a.Tokens {
		if a.Tokens[i].KeyFingerprint != b.Tokens[i].KeyFingerprint {
			t.Fatalf("★ 两次导出的指纹不一致（%q vs %q）—— 指纹不该随导出时间变化",
				a.Tokens[i].KeyFingerprint, b.Tokens[i].KeyFingerprint)
		}
	}
	// 两把不同的 key 必须有不同指纹（否则"还是原来那把吗"这个问题答不了）。
	if a.Tokens[0].KeyFingerprint == a.Tokens[1].KeyFingerprint {
		t.Fatalf("★ 两把不同的 key 得到了同一指纹 %q —— 指纹无法区分它们",
			a.Tokens[0].KeyFingerprint)
	}
}

// ★ ④ checksum 只覆盖结构：内容不变则相同，内容一变必不同。
func TestMigrationBackupChecksumTracksStructure(t *testing.T) {
	h, _ := backupFixture(t)

	rec1 := callBackup(t, h, true)
	var a migrationBackup
	if err := json.Unmarshal(rec1.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	// ⚠ 必须跨过一秒再导第二次：同一秒内 generated_at 相同，
	// "checksum 是否把时间戳算进去"就测不出来（第一版就这么漏的）。
	time.Sleep(1100 * time.Millisecond)
	rec2 := callBackup(t, h, true)
	var b migrationBackup
	if err := json.Unmarshal(rec2.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if a.Checksum != b.Checksum {
		t.Fatalf("★ 内容没变但 checksum 变了（%q → %q）—— checksum 把时间戳也算进去了，"+
			"那样「变了没有」就无从判断", a.Checksum, b.Checksum)
	}
	// 跨秒之后时间戳必须不同 —— 否则上一条 checksum 判据仍然测不到时间戳污染。
	if a.GeneratedAt == b.GeneratedAt {
		t.Fatalf("前置条件不成立：两次导出的 generated_at 相同（%q），"+
			"checksum 判据测不到「把时间戳算进去」这个反例", a.GeneratedAt)
	}

	// 改一条渠道 ⇒ checksum 必变。
	if err := h.cfg.Channels.Upsert(&channelstore.Channel{
		ID: "ch-new", Name: "new", Kind: "standard", Status: 1, Models: []string{"real-new"},
	}); err != nil {
		t.Fatal(err)
	}
	rec3 := callBackup(t, h, true)
	var c migrationBackup
	if err := json.Unmarshal(rec3.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.Checksum == a.Checksum {
		t.Fatal("★ 加了渠道但 checksum 没变 —— 备份无法证明「迁移后结构只改了预期的那些」")
	}
}

// 只有管理员能导出。
func TestMigrationBackupRequiresAdmin(t *testing.T) {
	h, _ := backupFixture(t)
	rec := callBackup(t, h, false)
	if rec.Code == http.StatusOK {
		t.Fatalf("★ 非管理员拿到了备份：%s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("应 401/403，实际 %d", rec.Code)
	}
}

// 禁缓存。
func TestMigrationBackupIsNoStore(t *testing.T) {
	h, _ := backupFixture(t)
	rec := callBackup(t, h, true)
	if got := rec.Header().Get("Cache-Control"); got != credentialCacheControl {
		t.Fatalf("必须带 no-store（%q），实际 %q", credentialCacheControl, got)
	}
}
