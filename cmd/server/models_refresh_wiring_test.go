// models_refresh_wiring_test.go —— 「刷新模型目录」这条链在**装配层**没断。
//
// # 为什么需要这条
//
// 链是三跳，任何一跳断了都是**静默失败**：
//
//	① 控制台按钮 ui/app.js 的 refreshModelCatalog → POST /admin/models/refresh
//	② 管理端点 internal/admin 的 modelsRefresh → 调用注入的 ResetModelsCache
//	③ 装配层把 server.ResetModelsCache 注入进去（**本文件守的那一跳**）
//
// ①③ 都没有编译期约束：② 用的是注入的函数值，注了个空函数（或干脆忘了注）时
// 端点的回执仍是 200，而缓存一份都没清 ⇒ 用户点完刷新看到的还是旧目录
// （本仓反复出现的形态：按钮点了没反应，代码里却「看起来都对」）。
//
// ② 的行为判据在 internal/admin/models_refresh_test.go（路由级，含防连点），
// ③ 的判据只能落在装配层 —— 这里读 main.go 的注入行。
package main

import (
	"os"
	"regexp"
	"testing"
)

// TestModelsRefreshWiredToServerReset 断言 admin 拿到的是**server 的那份**清缓存实现。
//
// ⚠ 判据刻意认「server.ResetModelsCache 这个值被注入到 ResetModelsCache 字段」，
//
//	而不是「文件里出现过 ResetModelsCache」（后者在注释里就能满足 —— 那等于没守）。
func TestModelsRefreshWiredToServerReset(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	// ⚠ 判据是**行首锚定**的（(?m)^\s*…），不是「文件里出现过这个词」：
	//   · 后者会被**注释里**的同名文字满足 —— 有人把注入行注释掉，守卫照样绿（装饰品）；
	//   · 也不认 ResetModelsCache: func(){} 这种「注了个空函数」的形态。
	//   \s* 容忍 gofmt 的字段对齐空格。
	re := regexp.MustCompile(`(?m)^\s*ResetModelsCache:\s+server\.ResetModelsCache\b`)
	if !re.Match(src) {
		t.Error("★ 装配层没有把 server.ResetModelsCache 注入 admin.Config.ResetModelsCache ——" +
			"控制台的「刷新模型目录」会 200 但一份缓存都不清（静默失败：点了刷新看到旧目录）")
	}
}
