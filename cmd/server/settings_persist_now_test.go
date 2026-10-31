package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/admin"
)

// fakeUpstreamSettings 一个最小上游设置适配器（键 → 当前值）。
//
// applyToMemory 故意实现成"把值记进 cfg"以外的空操作：本组要验的是**落盘时机**，
// 不是上游自己的内存同步（那个由各上游适配器自己的测试守）。
type fakeUpstreamSettings struct{ values map[string]any }

func (f *fakeUpstreamSettings) Values() map[string]any { return f.values }

// TestPersistNowWritesFreshUpstreamValues 「应用后落盘」必须写**当前**值。
//
// # 这条守的是实测到的一个真问题
//
// 顺序是：宿主 Apply（校验 + 落盘）→ 各上游 ApplySettings（改内存）。
// 于是 Apply 里那次落盘读到的上游值**永远滞后一次保存**：
// 用户把代理从 9999 改回 10808，config.json 里仍写着 9999 —— 重启后又回到错的端口，
// 表现就是「改了没用」。PersistNow 就是补的那一次。
func TestPersistNowWritesFreshUpstreamValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{\n  \"admin\": {}\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := &fakeUpstreamSettings{values: map[string]any{"monkeycode_proxy": "http://127.0.0.1:10808"}}
	// ⚠ cfg 必须是**非 nil**：PersistNow 走 currentSettings()，它会读 cfg 的各个段。
	//   用空 Config 就够 —— 本组验的是时机，不是那几个通用字段的值。
	s := &settingsStore{path: path, cfg: &Config{}, ups: []UpstreamSettings{up}}

	// 第一次落盘（例如用户第一次保存）：写当前值。
	if err := s.persist(admin.Settings{}); err != nil {
		t.Fatalf("persist 失败: %v", err)
	}
	// 模拟"上游 ApplySettings 之后"：值变了。
	up.values["monkeycode_proxy"] = "http://127.0.0.1:9999"
	if err := s.PersistNow(); err != nil {
		t.Fatalf("PersistNow 失败: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "9999") {
		t.Errorf("落盘里没有新值 9999 —— 落盘读的还是应用前的值（滞后一次保存）:\n%s", got)
	}
	if strings.Contains(string(got), "10808") {
		t.Errorf("落盘里还留着旧值 10808 —— 两个值并存会让下次启动读到哪个变得不确定:\n%s", got)
	}
}

// TestPersistNowKeepsForeignKeysAndComments 补的这一次落盘**不能**把别的配置弄丢。
//
// 它是第二次写同一个文件 —— 正是 2026-10-15 那次事故的形态（"再写一次"最容易出问题），
// 所以单独钉一条：注释、api_key、上游段都必须原样活着。
func TestPersistNowKeepsForeignKeysAndComments(t *testing.T) {
	const orig = `{
  // 用户手写的说明
  "api_key": "USER-KEY-MUST-SURVIVE",
  "monkeycode": { "proxy": "http://127.0.0.1:10808" },
  "admin": {}
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	up := &fakeUpstreamSettings{values: map[string]any{"monkeycode_proxy": "http://127.0.0.1:9999"}}
	s := &settingsStore{path: path, cfg: &Config{}, ups: []UpstreamSettings{up}}
	if err := s.PersistNow(); err != nil {
		t.Fatalf("PersistNow 失败: %v", err)
	}
	got, _ := os.ReadFile(path)
	for _, want := range []string{"USER-KEY-MUST-SURVIVE", "用户手写的说明", "\"monkeycode\"", "9999"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("补落盘后丢了 %q:\n%s", want, got)
		}
	}
}
