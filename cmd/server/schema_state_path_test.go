// schema_state_path_test.go 钉住「启动门禁的状态文件不能是账号池的那个」。
//
// # 这条判据来自一次真实的、差点丢数据的事故（2026-11-16）
//
// 我最初把 `channelstore.StartupGuard` 的路径写成 `data/state.json` ——
// 而那**正是账号池**（internal/pool）每 5 秒落盘的运行态文件
// （见 config.go 的 `StateFile: "./data/state.json"`，本仓 data/ 里 49 KB）。
//
// 事故链：
//
//	门禁读 data/state.json
//	⇒ json.Unmarshal 成 PersistedState 成功（**未知字段被忽略**，不报错）
//	⇒ SchemaVersion == 0
//	⇒ 判成 CompatUnmarked
//	⇒ MarkInitial 把它**覆写成 {"schema_version":1}**
//	⇒ 49 KB 账号状态没了
//
// 那次没丢数据，纯粹因为账号池每 5 秒把内存里的状态又写回来了一次
// （重启后 mtime 比启动时刻晚，是侥幸不是设计）。
//
// ⇒ 「JSON 能被解出来」不等于「这个文件是我的」。判据必须钉住**路径本身**。
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readMainSource 读同包的 main.go（供静态断言）。
func readMainSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatalf("读 main.go: %v", err)
	}
	return string(raw)
}

// ★ ① 启动门禁的路径**不能**是账号池的 state.json。
func TestStartupGuardPathIsNotPoolState(t *testing.T) {
	src := readMainSource(t)

	// 找到 StartupGuard 那一行的完整调用。
	at := strings.Index(src, "channelstore.StartupGuard(")
	if at < 0 {
		t.Fatal("main.go 里找不到 channelstore.StartupGuard 调用 —— 启动门禁被移除了？")
	}
	end := strings.Index(src[at:], "\n")
	if end < 0 {
		end = len(src) - at
	}
	call := src[at : at+end]

	// 账号池的运行态文件名（见 config.go 的 StateFile 默认值）。
	for _, forbidden := range []string{`"state.json"`, `./data/state.json`, `data/state.json`} {
		if strings.Contains(call, forbidden) {
			t.Fatalf("★★ 启动门禁的状态文件不能是账号池的运行态文件（%s）——\n"+
				"那会把它覆写成 {schema_version:1}，冲掉账号池的落盘状态。\n"+
				"调用：%s", forbidden, strings.TrimSpace(call))
		}
	}

	if !strings.Contains(call, "schema-state.json") {
		t.Fatalf("启动门禁应使用专属文件名 data/schema-state.json，实际：%s", strings.TrimSpace(call))
	}
}

// ② 账号池的 StateFile 仍然是 data/state.json（两边确实不同名）。
//
// 这条是上面那条的**锚**：如果哪天账号池换了文件名，这条会红，
// 提醒我们重新核对两边有没有又撞上。
//
// ⚠ 本判据第一版写成了 `t.Skip`（找不到文件就跳过）——
//
//	而 SKIP 在结果里和 PASS 一样是绿的：一个永远跳过的判据等于没有判据。
//	现在改成**找不到就红**。`StateFile` 就在同包的 config.go 里，不需要猜路径。
func TestPoolStateFileIsDistinct(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(".", "config.go"))
	if err != nil {
		t.Fatalf("读同包 config.go: %v（判据不该靠猜路径，也不该在找不到时静默跳过）", err)
	}
	src := string(raw)
	if !strings.Contains(src, `StateFile: "./data/state.json"`) {
		t.Fatalf("★ 账号池的 StateFile 默认值不是 ./data/state.json 了 ——\n" +
			"请重新核对启动门禁的路径是否与它撞上（见本文件头的真实事故）")
	}
	// 门禁路径必须与它不同名。
	if strings.Contains(src, "schema-state.json") {
		t.Fatal("config.go 里出现了 schema-state.json —— 两个用途又混到一起了")
	}
}
