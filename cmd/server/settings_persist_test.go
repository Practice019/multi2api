package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/admin"
)

// 这组守的是 2026-10-15 的真实事故：
//
// 用户的 config.json 从 26 个顶层键变成 **9 个** —— 只剩 persist 管的那些设置段。
// 丢掉的包括 api_key / listen / auth_dir / state_file、全部上游段，以及**注释**。
//
// 两条原因叠加：
//
//	① persist 用 `json.Unmarshal` 读这个文件，而它**不认注释**
//	   （而本仓的 config.json 支持注释 —— 用户手写的 73 行说明让它解析失败）
//	② 解析失败时旧代码写的是 `_ = json.Unmarshal(...)`，
//	   把"解析失败"当成"从空开始" ⇒ **丢掉用户的一切配置**再写回
//
// 所以两条都要守：**读得懂注释** + **读不懂就停下**。

// persistFixture 造一个带注释 + 带"非设置键"的 config.json。
func persistFixture(t *testing.T, body string) (*settingsStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &settingsStore{path: path}, path
}

// ⚠ persist 必须**逐字保留**注释与它不认识的键。
//
// 这是事故的直接判据：用户只改一个设置项，`api_key` / `listen` / 注释必须原样还在。
func TestPersistKeepsCommentsAndForeignKeys(t *testing.T) {
	const orig = `{
  // 这是用户手写的说明 —— 必须活下来
  "listen": "127.0.0.1:7863",
  "api_key": "USER-KEY-MUST-SURVIVE",
  /* 块注释也要活 */
  "auth_dir": "./auths",
  "schedule": { "checkin_enabled": true }
}
`
	s, path := persistFixture(t, orig)

	p := admin.Settings{}
	p.CheckinEnabled = false // 用户关掉签到
	if err := s.persist(p); err != nil {
		t.Fatalf("persist 不该失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)

	for _, must := range []struct{ what, s string }{
		{"行注释", "// 这是用户手写的说明 —— 必须活下来"},
		{"块注释", "/* 块注释也要活 */"},
		{"api_key", "USER-KEY-MUST-SURVIVE"},
		{"listen", `"listen": "127.0.0.1:7863"`},
		{"auth_dir", `"auth_dir": "./auths"`},
	} {
		if !strings.Contains(out, must.s) {
			t.Errorf("★ 保存设置把%s丢了（%q）——\n"+
				"\n"+
				"事故原貌：persist 用 json.Unmarshal 读 config.json，而它**不认注释**；\n"+
				"解析失败时旧代码 `_ = json.Unmarshal(...)` 把「解析失败」当成了\n"+
				"「从空开始」⇒ 写回去就只剩 persist 管的那些设置段。\n"+
				"用户点一下「保存设置」，api_key / listen / 注释 全部消失。",
				must.what, must.s)
		}
	}
	// 用户真正改的那一项要生效。
	if !strings.Contains(out, `"checkin_enabled": false`) {
		t.Error("用户改的那个设置没有被写进去 —— 那是另一个方向的回归")
	}
}

// ⚠⚠ 配置文件**读不懂时必须停下**，绝不能"当成空的"继续写。
//
// 这条是这次最关键的一条：**任何读失败都不该放大成一次数据丢失**。
// 判据：① 返回 error ② 文件**逐字节未变**。
func TestPersistRefusesToWriteWhenExistingConfigUnparsable(t *testing.T) {
	const broken = `{
  "api_key": "MUST-NOT-BE-LOST",
  "listen": "127.0.0.1:7863"
  "oops": 这里少了一个逗号
}
`
	s, path := persistFixture(t, broken)

	p := admin.Settings{}
	err := s.persist(p)
	if err == nil {
		t.Error("★ 现有配置读不懂时 persist 必须**返回错误** ——\n" +
			"旧代码是 `_ = json.Unmarshal(raw, &doc)`，把解析失败当成从空开始，\n" +
			"于是把用户的整个配置重写成只剩设置段。\n" +
			"判据：读不懂就停下，让用户看到「保存失败」，而不是**静默擦掉他的配置**。")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != broken {
		t.Errorf("★ 读不懂时文件被改动了！\n  期望逐字节不变\n  得到: %s\n"+
			"任何读取失败都不该放大成一次数据丢失。", string(got))
	}
}

// 反向：**读不懂就不写**不能把"正常保存"一起挡掉。
//
// 只加"读不懂就报错"而不验"正常路径照旧"，会把功能整个修死 ——
// 那种回归更容易发现，但一样要守住。
func TestPersistStillWritesOnParsableConfig(t *testing.T) {
	const orig = `{ "listen": "127.0.0.1:7863" }` + "\n"
	s, path := persistFixture(t, orig)

	p := admin.Settings{}
	p.CheckinEnabled = false
	p.CheckinHours = []int{9, 21}
	if err := s.persist(p); err != nil {
		t.Fatalf("正常配置应当能保存: %v", err)
	}
	got, _ := os.ReadFile(path)
	out := string(got)
	if !strings.Contains(out, `"listen": "127.0.0.1:7863"`) {
		t.Error("正常路径把无关的 listen 弄丢了")
	}
	if !strings.Contains(out, `"checkin_enabled": false`) {
		t.Error("正常路径没写进用户改的设置")
	}
}
