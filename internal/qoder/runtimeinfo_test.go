package qoder

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRuntimeInfoArgsExact 参数形态**逐字锁定**。
//
// # 为什么这条必须有（用户的报障就是这条路径）
//
// asar 源码是 `[String(environment), '--account-stdin']`。参照实现用实测消融
// 证明过：**漏掉第一个参数会拿到另一套身份** —— 服务端只回 1 条
// VIEW_DETAILS、没有可领项，于是被误判成「今天已领」。
//
// 那种缺陷极难定位：身份"有值"，只是错的，任何"拿到了吗"的断言都绿。
// 所以这里断言**数组本身**（含顺序），而不是"能不能拿到身份"。
func TestRuntimeInfoArgsExact(t *testing.T) {
	got := runtimeInfoArgs()
	want := []string{"3", "--account-stdin"}
	if len(got) != len(want) {
		t.Fatalf("参数个数 = %d，want %d：%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("参数[%d] = %q，want %q —— "+
				"漏第一参（environment）会让服务端不下发可领活动，"+
				"症状是「今天已领」的假象", i, got[i], want[i])
		}
	}
}

// TestParseRuntimeInfoOutput 解析 exe 输出（**驼峰**字段名）。
//
// ⚠ 字段名是 `machineToken` / `machineType`，与磁盘缓存的 `token` / `type`
// **不同**。弄混会让解析静默失败 → 退回缓存 → 拿不到头 → 签到失败，
// 症状与"没修"完全一样，所以单独钉一条。
func TestParseRuntimeInfoOutput(t *testing.T) {
	// 本机实测输出的形状（值已替换为假值）。
	real := `{"machineToken":"P1gAfake","machineType":"127b4f9014042f1a85",` +
		`"machineCode":"18eb8dd976c832ba43","vmInfo":{"isVm":true},` +
		`"accountOutcome":"invalid_input"}`
	id := parseRuntimeInfoOutput(real)
	if id == nil {
		t.Fatal("真实形状的输出必须能解析出身份")
	}
	if id.Token != "P1gAfake" || id.Type != "127b4f9014042f1a85" {
		t.Errorf("解析结果不对：%+v", id)
	}

	// 前缀噪音（从第一个 `{` 截）
	noisy := "some warning line\n" + real
	if parseRuntimeInfoOutput(noisy) == nil {
		t.Error("带前缀噪音时也应能解析（应从第一个 { 开始）")
	}

	for _, tc := range []struct{ name, in string }{
		{"没有 JSON", "no json here"},
		{"缺 machineType", `{"machineToken":"t"}`},
		{"缺 machineToken", `{"machineType":"x"}`},
		{"两者都空", `{"machineToken":"","machineType":""}`},
		{"用了缓存的字段名（token/type）", `{"token":"t","type":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRuntimeInfoOutput(tc.in); got != nil {
				t.Errorf("应解析失败（两者必须成对且非空），得到 %+v", got)
			}
		})
	}
}

// TestLocateRuntimeInfoFindsInstalledLayout 能定位到**带哈希的目录**。
//
// 真实形态：`<home>/<dataDir>/.bin/umid-<platform>-<hash>/runtime-info(.exe)`
// —— 目录名随版本变，所以必须枚举；写死路径会在升级后失效。
//
// # 为什么两个数据目录都要试（参照记过的真实缺陷）
//
// 原先只认国际版的 `.qoder`，于是"只装了中国版"的用户找不到 exe →
// 退回陈旧缓存 → 拿不到 machine 头。本机（用户环境）正是只有 `.qoder-cn`。
func TestLocateRuntimeInfoFindsInstalledLayout(t *testing.T) {
	home := t.TempDir()
	exeName := "runtime-info"
	if runtime.GOOS == "windows" {
		exeName = "runtime-info.exe"
	}

	// 只造**中国版**目录（`.qoder-cn`），且目录名带哈希。
	binDir := filepath.Join(home, ".qoder-cn", ".bin", "umid-win32-x64-abc123")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, exeName)
	if err := os.WriteFile(exe, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("QODER_RUNTIME_INFO", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows 上 os.UserHomeDir 读它

	got := locateRuntimeInfo()
	if got != exe {
		t.Errorf("没找到 exe：got %q，want %q —— "+
			"必须遍历 .qoder **与** .qoder-cn，只认前者会让中国版用户拿不到身份",
			got, exe)
	}
}

// TestLocateRuntimeInfoSkipsEmptyUmidDir 空的 `umid-*` 目录要**继续试下一个**。
//
// 半安装 / 清理残留会留下没有 exe 的 `umid-*` 目录。若在那里返回空，
// 就永远找不到同目录下的另一个可用版本。
func TestLocateRuntimeInfoSkipsEmptyUmidDir(t *testing.T) {
	home := t.TempDir()
	exeName := "runtime-info"
	if runtime.GOOS == "windows" {
		exeName = "runtime-info.exe"
	}

	binDir := filepath.Join(home, ".qoder", ".bin")
	// 一个空目录 + 一个真有的（名字排序上空的在前，逼出"继续试"这条路径）
	if err := os.MkdirAll(filepath.Join(binDir, "umid-win32-x64-aaa"), 0o755); err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(binDir, "umid-win32-x64-zzz")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, exeName), []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("QODER_RUNTIME_INFO", "")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	got := locateRuntimeInfo()
	if got == "" {
		t.Fatal("遇到空的 umid-* 目录就放弃了 —— 应继续试下一个")
	}
	if !strings.Contains(got, "zzz") {
		t.Errorf("应选中真有 exe 的那个目录，实际 %q", got)
	}
}

// TestLocateRuntimeInfoOverrideMustExist 显式覆盖的路径**必须真实存在**。
//
// 指向不存在的路径 = "本机没装 Qoder"，于是走缓存退路。
// 这是测试隔离的机制本身，所以也要钉住 —— 否则隔离会静默失效变成假绿。
func TestLocateRuntimeInfoOverrideMustExist(t *testing.T) {
	t.Setenv("QODER_RUNTIME_INFO", "/nonexistent/runtime-info")
	if got := locateRuntimeInfo(); got != "" {
		t.Errorf("覆盖路径不存在时应返回空串（= 没装），得到 %q", got)
	}
}
