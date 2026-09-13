package codearts

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadDirSkipsSubdirectories 钉住"归档目录不被扫描"。
//
// auths/ 旁边有一个 `.rejected/` 目录，放的是**被判定为坏的**凭证
// （以及续期备份 .bak/）。它们在语义上属于"已经出局"的东西，
// 绝不能被 LoadDir 当成账号加载 —— 否则一个已知损坏的凭证会被反复
// 塞进账号池，表现为"每次刷新都有一个号报错"，而用户早就把它扔进归档了。
//
// 目前 Glob 是 `dir/codearts*.json`（单层），天然不跨目录。
// 这条断言的作用是**把"不跨目录"变成一个受约束的事实**：
// 一旦有人为了方便把它改成递归（`**`、WalkDir、或者 glob 前缀改成 `dir/*/`），
// 归档目录里的坏凭证就会悄悄复活，而这条测试会立刻变红。
//
// 注意断言的写法：故意用**两个都合法**的凭证。
// 重点验证的是"子目录被排除"，而不是"坏文件被排除" ——
// 后者即使 Glob 递归了也可能因为解析失败而凑巧通过，验不出真东西。
func TestLoadDirSkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()

	// 顶层：应被读到
	good := filepath.Join(dir, "codearts-good.json")
	if err := os.WriteFile(good, makeCredDoc("AK_TOP", "RT_TOP"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 子目录（归档）：即使内容完全合法，也不该被读到
	rejectedDir := filepath.Join(dir, ".rejected")
	if err := os.MkdirAll(rejectedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(rejectedDir, "codearts-bad.json")
	if err := os.WriteFile(bad, makeCredDoc("AK_NESTED", "RT_NESTED"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 前置自检：两个文件必须**都能单独解析成功**。
	// 否则这条测试可能因为"坏文件被跳过"而假通过，验不出 Glob 的层数。
	for _, p := range []string{good, bad} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("前置自检读 %s: %v", p, err)
		}
		if _, err := ParseCredential(raw); err != nil {
			t.Fatalf("前置自检失败：%s 本身就无法解析（%v）—— 这条测试会假通过", p, err)
		}
	}

	creds, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	if len(creds) != 1 {
		var got []string
		for _, c := range creds {
			got = append(got, c.FilePath)
		}
		t.Fatalf("读到 %d 个凭证 %v，期望恰好 1 个（只有顶层）—— "+
			"子目录被扫进来了，归档的坏凭证会复活进账号池", len(creds), got)
	}

	if got := filepath.Base(creds[0].FilePath); got != "codearts-good.json" {
		t.Errorf("读到的是 %q，期望 codearts-good.json", got)
	}
	// 再钉一次 AK：即使数量对，也不能是子目录里那个
	if creds[0].AccessKey != "AK_TOP" {
		t.Errorf("读到子目录里的凭证（AK=%q），顶层 AK_TOP 被漏掉", creds[0].AccessKey)
	}
}

// TestLoadDirSkipsSubdirectoryEvenWhenTopLevelEmpty 是上一条的补强：
// 顶层一个文件都不放，只放子目录。这排除了"数量恰好对上"的巧合 ——
// 若 Glob 变成递归，这里会读到 1 个而不是 0 个。
func TestLoadDirSkipsSubdirectoryEvenWhenTopLevelEmpty(t *testing.T) {
	dir := t.TempDir()

	rejectedDir := filepath.Join(dir, ".rejected")
	if err := os.MkdirAll(rejectedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rejectedDir, "codearts-bad.json"),
		makeCredDoc("AK_NESTED", "RT_NESTED"), 0o600); err != nil {
		t.Fatal(err)
	}

	creds, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(creds) != 0 {
		var got []string
		for _, c := range creds {
			got = append(got, c.FilePath)
		}
		t.Fatalf("顶层没有凭证却读到 %d 个 %v —— Glob 已经跨进子目录了", len(creds), got)
	}
}

// TestLoadDirLogsSkippedFile 锁定 S1：
// 解析失败的文件被跳过时，**必须**留下一条带文件名和原因的日志。
//
// 为什么这条测试存在：早先的 `continue` 是静默的。
// 用户放了 3 个文件只进 2 个，界面上没有任何提示 ——
// "文件没进池，也没人告诉他为什么"。
// 而"静默"这种缺陷用返回值测不出来（返回值本来就只有 2 个），
// 只能抓**副作用**：日志输出。
//
// 用 log.SetOutput 把标准 logger 接到 buffer 上抓。
// 恢复用 defer，避免污染同包其他测试。
func TestLoadDirLogsSkippedFile(t *testing.T) {
	dir := t.TempDir()

	// 一个合法文件（不该产生日志）
	if err := os.WriteFile(filepath.Join(dir, "codearts-good.json"),
		makeCredDoc("AK_OK", "RT"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 一个坏文件：缺 accessKeyId/secretAccessKey，ParseCredential 会拒
	if err := os.WriteFile(filepath.Join(dir, "codearts-broken.json"),
		[]byte(`{"securityToken":"only"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	}()

	creds, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("读到 %d 个凭证，期望 1 个（坏文件应被跳过）", len(creds))
	}

	got := buf.String()
	if !strings.Contains(got, "codearts-broken.json") {
		t.Errorf("跳过时未打出文件名。实际日志=%q", got)
	}
	// 原因也要在：只说"跳过 X"而不说为什么，用户还是得自己去猜
	if !strings.Contains(got, "accessKeyId") && !strings.Contains(got, "missing") {
		t.Errorf("跳过时未打出原因。实际日志=%q", got)
	}
	// 不能打全路径（日志够长了）
	if strings.Contains(got, dir) {
		t.Errorf("日志里出现了完整路径，应只用 filepath.Base。实际日志=%q", got)
	}

	// 关键的另一半：正常文件**不产生日志**。
	// 否则账号一多就刷屏，日志里再也找不到真正的原因。
	if strings.Contains(got, "codearts-good.json") {
		t.Errorf("正常文件也产生了日志（会刷屏）。实际日志=%q", got)
	}
}
