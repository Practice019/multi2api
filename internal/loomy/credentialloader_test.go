package loomy

// credentialloader_test.go —— 守住「凭证文件在哪」这个事实必须投影出去。
//
// # 为什么必须有这条测试（用户实测报的缺陷，2026-10-19）
//
// 现象：账号池里点「移除」，行过一会儿又回来（用户原话「导入之后删除不掉」）。
//
// 根因：本包是**唯一**一个 `LoadCredentials` 没带 `FilePath` 的上游
//（其余 9 个都带了），于是池条目上它恒为空 ⇒ `DELETE …?purge_file=1`
// 只能把账号移出池子、**删不到凭证文件** ⇒ 下次「重载 auths」或重启，
// `LoadDir` 又把它装回池子。
//
// ⚠ 两条投影路都要测，因为装配层用的**不一定**是投影那条：
// `poolAuthsFromCreds` 只在"secret 恰好是 `*auth.Auth`"时直接用 secret 里那份
//（workbuddy 是这种），而 loomy 的 secret 是 `*loomy.Auth` ⇒ 类型断言不成立
// ⇒ **实际落到投影**。也就是说：这里的 FilePath 就是池条目唯一的来源。
//
// 合成数据：文件当场写进 t.TempDir()，不读仓库里任何真实凭证。

import (
	"path/filepath"
	"testing"
)

const (
	credTestUID     = "260101010101010101"
	credTestSession = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// credTestFile 落一份最小 loomy 凭证，返回 (目录, 文件全路径)。
func credTestFile(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	name := "loomy-x.json"
	writeFile(t, dir, name, `{"session":"`+credTestSession+`","userid":"`+credTestUID+`"}`)
	return dir, filepath.Join(dir, name)
}

// TestLoadCredentialsCarriesFilePath 投影必须带上凭证文件路径。
func TestLoadCredentialsCarriesFilePath(t *testing.T) {
	dir, file := credTestFile(t)
	p := NewWithConfig(Config{AuthDir: dir})

	// 前提：LoadDir 自己是记路径的（它不记的话，本测试的断言会"
	// 靠上游另一处实现巧合通过"，判据就不再判别了）。
	loaded, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir 失败: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadDir 读出 %d 份凭证，期望 1", len(loaded))
	}
	if loaded[0].FilePath != file {
		t.Fatalf("前提不满足：LoadDir 没把 FilePath 记成 %q，实际 %q —— "+
			"那说明「投影漏传」不是真正的根因，先别信这条测试的结论", file, loaded[0].FilePath)
	}

	creds, err := p.LoadCredentials(dir)
	if err != nil {
		t.Fatalf("LoadCredentials 失败: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("投影出 %d 份凭证，期望 1", len(creds))
	}
	if creds[0].UID != credTestUID {
		t.Errorf("uid=%q want %q", creds[0].UID, credTestUID)
	}
	if creds[0].FilePath == "" {
		t.Error("★ LoadCredentials 没带 FilePath —— " +
			"装配层投影进池子后它是空，`DELETE …?purge_file=1` 就删不到文件：" +
			"账号出池了、文件还在盘上，重载/重启又装回来（用户实测的「删除不掉」）")
	} else if creds[0].FilePath != file {
		t.Errorf("FilePath=%q want %q", creds[0].FilePath, file)
	}
}

// TestLoadCredentialsWithSecretsCarriesFilePath 带 secret 的那条路同样要带。
//
// ⚠ 这条**不是重复**：装配层优先走带 secret 的扫描器
// （`credentialSecretsOf` 成立时），而它的投影正是池条目 FilePath 的来源。
// 只修投影那条、漏掉这条，缺陷会原样存在 —— 本包两条投影是分别写的。
func TestLoadCredentialsWithSecretsCarriesFilePath(t *testing.T) {
	dir, file := credTestFile(t)
	p := NewWithConfig(Config{AuthDir: dir})

	secs, err := p.LoadCredentialsWithSecrets(dir)
	if err != nil {
		t.Fatalf("LoadCredentialsWithSecrets 失败: %v", err)
	}
	if len(secs) != 1 {
		t.Fatalf("投影出 %d 份凭证，期望 1", len(secs))
	}
	if secs[0].UID != credTestUID {
		t.Errorf("uid=%q want %q", secs[0].UID, credTestUID)
	}
	if secs[0].FilePath == "" {
		t.Error("★ LoadCredentialsWithSecrets 没带 FilePath —— " +
			"这是装配层**实际**走的那条（loomy 的 secret 断言不成 ⇒ 用投影），" +
			"漏了它等于没修")
	} else if secs[0].FilePath != file {
		t.Errorf("FilePath=%q want %q", secs[0].FilePath, file)
	}
	// secret 必须仍然原样带着（核心只保管、由装配层断言回本包类型）
	if secs[0].Secret == nil {
		t.Error("Secret 丢了 —— 池条目会在取号时说「凭证为空」")
	}
}
