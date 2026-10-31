package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/admin"
)

// TestPersistIntoEmptySectionStaysValidJSON 往**空段**写设置不能产出非法 JSON。
//
// # 这是一个真缺陷（2026-11-01 实测复现）
//
// 插入逻辑无条件写 `"key": value,`，段为空时结果是：
//
//	"admin": {
//	    "checkin_log_keep_days": 0,}     ← 尾逗号紧跟右括号 ⇒ 非法 JSON
//
// 后果不是"这一项没保存"，而是**整份 config.json 下次启动解析失败** ——
// 点一下「保存设置」就把自己挡在门外。可达路径：用户手写 `"admin": {}`，
// 或把某一段的键全删了（出厂模板各段都有内容，所以只有手写过配置的人会中）。
//
// 判据不看字符串匹配，直接 **json.Unmarshal** —— 这正是这个缺陷之前躲过所有断言的原因
// （既有用例只检查"某个键在不在"，而非法 JSON 里那个键照样在）。
func TestPersistIntoEmptySectionStaysValidJSON(t *testing.T) {
	cases := []struct{ name, body string }{
		{"段为空对象（单行）", "{\n  \"admin\": {}\n}\n"},
		{"段为空对象（带空格）", "{\n  \"admin\": {  }\n}\n"},
		{"段为空对象（带注释）", "{\n  /* 说明 */\n  \"admin\": { // 里面什么都没有\n  }\n}\n"},
		{"顶层为空对象", "{}\n"},
		{"顶层只有注释", "{\n  // 用户笔记\n}\n"},
		{"段非空（对照组，必须仍然合法）", "{\n  \"admin\": { \"x\": 1 }\n}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s := &settingsStore{path: path, cfg: &Config{}}
			if err := s.persist(admin.Settings{CheckinEnabled: false, CheckinLogKeepDays: 45}); err != nil {
				t.Fatalf("persist 失败: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// ⚠ 判据用 **unmarshalJSONC**（本仓读 config.json 的那条路径），不是标准库：
			//   写回**保留注释**是设计（用户的说明必须活下来），而标准库不认 // 与 /* */。
			//   第一版用 json.Unmarshal，于是"注释还在"被误判成"文件坏了"。
			var doc map[string]any
			if err := unmarshalJSONC(got, &doc); err != nil {
				t.Fatalf("保存后 config.json 不是合法 JSONC（用户下次启动会起不来）: %v\n----\n%s", err, got)
			}
			// 用户手写的注释必须还在（这是本文件的既有纪律）
			if strings.Contains(c.body, "用户笔记") && !strings.Contains(string(got), "用户笔记") {
				t.Errorf("注释丢了:\n%s", got)
			}
		})
	}
}
