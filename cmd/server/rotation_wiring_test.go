// rotation_wiring_test.go 「轮换要更新每一份内存副本」的装配层守卫。
//
// # 这个坑出现了两次，都在同一个类上
//
//	第一次  admin.Handler.cfg.APIKey 是构造期快照 → 永不更新
//	        （修法：门禁读 SetAdminToken 维护的可变字段）
//	第二次  main.go 声明了 `var onRotateAdminToken func(string) = func(string) {}`
//	        但**从来没有赋值** → 那个空操作一直被调
//
// 第二次的后果就是用户报的「你给我的我进不去」：
//
//	admin.Handler 的令牌  → 更新了（/admin/* 用新令牌能进）
//	server.Handler 的令牌 → **没更新**（/ui 注入还拿旧令牌）
//
// 于是拿新令牌打开 /ui：页面 200，但显示"未授权"（服务端比对的是旧令牌）。
//
// # 为什么用源码级判据
//
// "两条回调都接了"是**装配事实**，只在 main.go 里能看到。上游包内的测试
// 覆盖不到（它们各自测自己的 SetAdminToken），而"忘了接一条"恰好不发生
// 在那两个包里 —— 它发生在这里。
package main

import (
	"strings"
	"testing"
)

func TestRotationCallbacksAreBothWired(t *testing.T) {
	raw, err := readMainGo()
	if err != nil {
		t.Fatal(err)
	}
	// ⚠ 归一化行尾：main.go 在工作树里是 CRLF，锚点按 LF 写会静默不命中。
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")

	// ① 两条回调都必须被**赋值**（不是只有声明）。
	//
	// 判据：出现 `onRotateAdminToken = func(`。
	// 只声明不赋值就是本次事故的形态 —— 而它不报任何错。
	if !strings.Contains(src, "onRotateAdminToken = func(") {
		t.Errorf("★ main.go 里 onRotateAdminToken **只有声明、没有赋值** ——\n" +
			"\n" +
			"那么 `OnAdminTokenRotated` 里那句 `onRotateAdminToken(newKey)` 永远只调空操作，\n" +
			"于是轮换**只更新一半**：\n" +
			"  · admin.Handler 的令牌  → 更新（/admin/* 用新令牌能进）\n" +
			"  · server.Handler 的令牌 → 没更新（/ui 注入还拿旧令牌）\n" +
			"\n" +
			"用户拿新令牌打开 /ui：页面 200 但显示「未授权」（服务端比对的是旧令牌）。\n" +
			"这正是「你给我的我进不去」的根因。")
	}

	// ② 那条回调里必须更新 **server.Handler** 的那份（h.SetAdminToken）。
	if !strings.Contains(src, "h.SetAdminToken(") {
		t.Errorf("★ 没有 `h.SetAdminToken(...)` —— server.Handler 的令牌副本没有更新路径，\n" +
			"/ui 的注入判据（adminTokenOK）会一直比旧令牌。")
	}

	// ③ 对话 key 那条回调必须更新 h.SetAPIKey（两条尺度分开，别互相覆盖）。
	if !strings.Contains(src, "h.SetAPIKey(") {
		t.Errorf("没有 `h.SetAPIKey(...)` —— 对话 api_key 那一路轮换后不生效")
	}

	// ④ 反向：`OnAdminTokenRotated` 本身必须被接上（否则两条都成摆设）。
	if !strings.Contains(src, "OnAdminTokenRotated:") {
		t.Errorf("admin.Config 的 OnAdminTokenRotated 没有被接上 —— 轮换后两份内存副本都不更新")
	}
}
