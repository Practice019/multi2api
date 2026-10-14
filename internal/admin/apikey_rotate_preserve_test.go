// apikey_rotate_preserve_test.go 「轮换管理钥匙不破坏配置文件」的守卫。
//
// # 用户报的问题
//
//	「我发现这个 Config 配置有点太丑了，不知道怎么回事啊，太乱了我只能说。」
//
// 诊断结果：他的 config.json **被程序写回过**，而旧实现是这样写的 ——
//
//	var cfg map[string]any
//	json.Unmarshal(raw, &cfg)
//	cfg["api_key"] = newKey
//	out, _ := json.MarshalIndent(cfg, "", "  ")   // ← 整个文件重写
//
// 后果三条，都能在他那份文件里直接看到：
//
//	① 注释全丢
//	② 顺序全变（map 序列化按 key 排序 + 嵌套层级与手写习惯无关）
//	③ 空行/风格全变（他的文件里 `uphost` 段后确实有个孤零零的空行）
//
// 而轮换管理钥匙**只需要改一个字符串**。
//
// ⚠ 这是"用户可见的破坏性副作用"里最隐蔽的一类：功能本身是对的
// （钥匙确实轮换了、也立即生效了），被破坏的是**他没盯着的那个文件**。
package admin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ⚠ 核心判据：轮换之后，除了 api_key 那一处的值，**其余字节逐字节不变**。
//
// 这条同时覆盖注释、顺序、空行、缩进 —— 因为它比的是完整文本。
func TestRotatePreservesEverythingButKeyValue(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")

	// 故意写成"用户手写风格"：带注释、顺序不按字母、有额外空行、
	// 缩进是 2 空格但有一处用了 4 空格。
	// 这些**全部**必须活下来。
	original := "{\n" +
		"  // 网关监听地址（改完要重启）\n" +
		"  \"listen\": \"127.0.0.1:7863\",\n" +
		"\n" +
		"  \"api_key\": \"OLD-KEY-VALUE\",\n" +
		"\n" +
		"  /* 上游：这一段是给 loomy 用的\n" +
		"     多行块注释也要活下来 */\n" +
		"  \"loomy\": {\n" +
		"    \"enabled\": true,\n" +
		"      \"login_mode\": \"sms\"\n" +
		"  }\n" +
		"}\n"
	if err := os.WriteFile(fp, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := setConfigAPIKey(fp, "NEW-KEY-VALUE"); err != nil {
		t.Fatalf("setConfigAPIKey: %v", err)
	}
	got, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}

	// 值确实换了。
	if !strings.Contains(string(got), "NEW-KEY-VALUE") {
		t.Fatal("api_key 的值没有被替换")
	}
	// 期望结果 = 原文里那一处值换掉，其余逐字节相同。
	want := strings.Replace(original, "OLD-KEY-VALUE", "NEW-KEY-VALUE", 1)
	if string(got) != want {
		t.Errorf("除了 api_key 的值之外，文件被改动了 ——\n"+
			"  期望 %q\n  得到 %q\n"+
			"轮换钥匙只该改一个字符串；注释/顺序/空行/缩进都必须原样保留"+
			"（用户报的「配置太乱」就是这个副作用造成的）", want, string(got))
	}

	// 逐条点名断言（失败时能一眼看出丢了哪一类）。
	for _, keep := range []struct{ what, s string }{
		{"行注释", "// 网关监听地址（改完要重启）"},
		{"块注释", "/* 上游：这一段是给 loomy 用的"},
		{"空行", "\n\n  \"api_key\""},
		{"原顺序 listen 在 api_key 之前", "\"listen\""},
		{"4 空格那处缩进", "\n      \"login_mode\""},
	} {
		if !strings.Contains(string(got), keep.s) {
			t.Errorf("轮换后丢了%s（%q）", keep.what, keep.s)
		}
	}
}

// 注释里的 `"api_key"` 不能被当成真正的键 —— 否则会改错地方。
func TestRotateIgnoresKeyNameInsideComment(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	original := "{\n" +
		"  // 这里有个人写了 \"api_key\": \"DECOY\" 作为说明\n" +
		"  \"api_key\": \"REAL\",\n" +
		"  \"a\": 1\n" +
		"}\n"
	os.WriteFile(fp, []byte(original), 0o600)

	if err := setConfigAPIKey(fp, "ROTATED"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(fp)
	s := string(got)

	if !strings.Contains(s, "DECOY") {
		t.Error("注释里的内容被改动了 —— 替换必须跳过注释")
	}
	if !strings.Contains(s, `"api_key": "ROTATED"`) {
		t.Errorf("真正的 api_key 没被替换，得到: %s", s)
	}
	if strings.Count(s, "ROTATED") != 1 {
		t.Errorf("替换发生了多次（应当只有一处），得到: %s", s)
	}
}

// 嵌套对象里的同名键不能被动（只改顶层）。
func TestRotateOnlyTouchesTopLevelKey(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	original := "{\n" +
		"  \"api_key\": \"TOP\",\n" +
		"  \"nested\": {\n" +
		"    \"api_key\": \"INNER\"\n" +
		"  }\n" +
		"}\n"
	os.WriteFile(fp, []byte(original), 0o600)

	if err := setConfigAPIKey(fp, "TOP2"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(fp)
	s := string(got)
	if !strings.Contains(s, `"api_key": "TOP2"`) {
		t.Errorf("顶层 key 没换: %s", s)
	}
	if !strings.Contains(s, `"api_key": "INNER"`) {
		t.Errorf("嵌套的 key 被误改了（必须只动顶层）: %s", s)
	}
}

// 文件里**没有** api_key 键时，回落到"解析 + 重写"，且不能把文件写坏。
//
// 这是一条**有损**路径（会丢注释），所以刻意保留日志 ——
// 这条测试只保证"写出来仍是合法 JSON 且键被加上"，注释丢失是已知代价。
func TestRotateFallbackWhenKeyMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	// 注意：这里**没有** api_key 键。
	os.WriteFile(fp, []byte("{\n  \"listen\": \":7863\"\n}\n"), 0o600)

	if err := setConfigAPIKey(fp, "ADDED"); err != nil {
		t.Fatalf("回落路径不该失败: %v", err)
	}
	got, _ := os.ReadFile(fp)
	s := string(got)
	if !strings.Contains(s, "ADDED") {
		t.Errorf("回落路径没有写入新 key: %s", s)
	}
	if !strings.Contains(s, "listen") {
		t.Errorf("回落路径丢了原有字段: %s", s)
	}
}

// 轮换必须**原子**：不留下半截文件，也不留 .tmp 残留。
func TestRotateIsAtomicAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	os.WriteFile(fp, []byte("{\n  \"api_key\": \"A\"\n}\n"), 0o600)

	if err := setConfigAPIKey(fp, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("留下了 .tmp 残留文件 —— 下次写入会覆盖它，但残留本身说明 rename 没走完")
	}
	// ⚠ 权限断言必须区分平台。
	//
	// 我第一版无条件断言 `Perm() == 0o600`，在 Windows 上**红**了 ——
	// 排查后发现那是平台语义，不是 bug：Windows 的 `os.WriteFile`
	// 只把 perm 的"只读位"映射过去，其余位被忽略（实测 0600/0644/0666
	// 写出来都是 0666）。`os.Chmod` 同样只能设/清只读位。
	//
	// 所以这条只在 **POSIX** 上是有意义的断言；Windows 上跳过 ——
	// 而"跳过"要写在注释里，否则下一个人会以为这条守卫被删了。
	// （真正的保护在 POSIX 部署上；Windows 上配置文件的访问控制靠 ACL，
	// 不靠这个 perm —— 这一点也要让读者看到。）
	if runtime.GOOS == "windows" {
		t.Log("Windows：perm 只映射只读位，0600 无法表达 —— 跳过权限断言（非缺陷）")
		return
	}
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("权限应当是 0600（里面有管理钥匙），实际 %o", fi.Mode().Perm())
	}
}
