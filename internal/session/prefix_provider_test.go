// prefix_provider_test.go 粘性分配必须按上游过滤（2026-10-20，issue #2 第二层）。
//
// # 这个文件钉的是什么
//
// issue #2 报告了第一层缺陷：`ExtractKey` 只认三个客户端不发的字段 ⇒ sessKey 恒空。
// 修完第一层后，**真实隔离实例实测仍然不粘**：
//
//	11:42:09 uid=260913075224031074 decision=sticky_unavailable:session_rule
//	11:42:13 uid=260825100642697603 decision=sticky_unavailable:session_rule
//	11:42:17 uid=260825101823357254 decision=sticky_unavailable:session_rule
//	→ 每轮换一个号，decision 全是 sticky_unavailable
//
// # 第二层根因
//
// `Router.Resolve` 的慢路径**自己就分配并绑定**账号：
//
//	uids := r.availableSlice()                            ← 候选池
//	uid := pool2[hashIndex(key, len(pool2))]
//	r.entries[key] = entry{uid: uid, ...}                 ← 直接写入绑定
//
// 而候选池来自 `Config.Available`，`main.go` 里赋的是 `pool.AvailableUIDs()` ——
// 它等价于 `AvailableUIDsFor(DefaultProvider)`，**只返回默认上游（workbuddy）的号**。
//
// ⇒ loomy 的请求从 workbuddy 的号里挑一个绑定 ⇒ handler 的
//
//	`PickByUIDFor("loomy", workbuddy_uid)` 必然返回 nil ⇒ Unbind ⇒
//	下一轮又慢路径 ⇒ 死循环。
//
// # 修法
//
// `Resolve(key, provider)` 带上游；`Config.AvailableFor(provider)` 按上游取候选。
// 粘性是**池内**概念，而池子是多上游共用的 —— 跨上游的绑定对本次请求毫无意义
// （凭证结构都不同），只会让"选号"这一步白跑。
package session

import (
	"testing"
	"time"
)

// TestResolveRespectsProvider ★ 核心：候选池必须按 provider 过滤。
func TestResolveRespectsProvider(t *testing.T) {
	// 模拟一个双上游池：2 个 loomy + 3 个 workbuddy
	byProv := map[string][]string{
		"loomy":     {"L1", "L2"},
		"workbuddy": {"W1", "W2", "W3"},
	}
	r := New(Config{
		TTL: time.Minute,
		AvailableFor: func(provider string) []string {
			return byProv[provider]
		},
	})
	defer r.StopGC()

	// loomy 的会话只能拿到 L 开头的号
	for i := 0; i < 20; i++ {
		key := "pfx:loomy-" + string(rune('a'+i))
		uid, ok := r.Resolve(key, "loomy")
		if !ok {
			t.Fatalf("第 %d 次 loomy Resolve 失败", i)
		}
		if uid != "L1" && uid != "L2" {
			t.Fatalf("★ loomy 的会话被绑到了 %q —— 那是别的上游的号，"+
				"handler 的 PickByUIDFor 必然返回 nil，粘性会永远不生效", uid)
		}
	}

	// workbuddy 的会话只能拿到 W 开头的号
	for i := 0; i < 20; i++ {
		key := "pfx:wb-" + string(rune('a'+i))
		uid, ok := r.Resolve(key, "workbuddy")
		if !ok {
			t.Fatalf("第 %d 次 workbuddy Resolve 失败", i)
		}
		if uid != "W1" && uid != "W2" && uid != "W3" {
			t.Fatalf("workbuddy 的会话被绑到了 %q", uid)
		}
	}
}

// TestResolveStickyAcrossTurns 同一 key 多轮必须同 uid（真实修复的观测量）。
func TestResolveStickyAcrossTurns(t *testing.T) {
	r := New(Config{
		TTL: time.Minute,
		AvailableFor: func(provider string) []string {
			if provider == "loomy" {
				return []string{"L1", "L2", "L3"}
			}
			return nil
		},
	})
	defer r.StopGC()

	key := "pfx:same-conversation"
	first, ok := r.Resolve(key, "loomy")
	if !ok {
		t.Fatal("首次 Resolve 失败")
	}
	for i := 0; i < 10; i++ {
		got, ok := r.Resolve(key, "loomy")
		if !ok {
			t.Fatalf("第 %d 轮 Resolve 失败", i)
		}
		if got != first {
			t.Fatalf("★ 同一会话第 %d 轮换了号：%s → %s（这就是 issue #2 报的现象）", i, first, got)
		}
	}
}

// TestResolveEmptyProviderPoolReturnsFalse 该上游没有可用号时返回 false，不跨界兜底。
//
// ⚠ 这条防的是另一种退化：候选为空时去其他上游拿号 —— 那等于把"跨上游绑定"
// 从慢路径又放回来了，只是换了个入口。
func TestResolveEmptyProviderPoolReturnsFalse(t *testing.T) {
	r := New(Config{
		TTL: time.Minute,
		AvailableFor: func(provider string) []string {
			if provider == "workbuddy" {
				return []string{"W1"}
			}
			return nil // loomy 没有可用号
		},
	})
	defer r.StopGC()

	if uid, ok := r.Resolve("pfx:x", "loomy"); ok {
		t.Fatalf("★ loomy 无可用号时应返回 false，实际绑到了 %q（可能跨上游兜底了）", uid)
	}
	// workbuddy 仍然正常
	if uid, ok := r.Resolve("pfx:y", "workbuddy"); !ok || uid != "W1" {
		t.Fatalf("workbuddy 应正常分配 W1，实际 %q ok=%v", uid, ok)
	}
}

// TestAvailableForFallsBackToAvailable 没配 AvailableFor 时退回旧的 Available（向后兼容）。
func TestAvailableForFallsBackToAvailable(t *testing.T) {
	called := false
	r := New(Config{
		TTL: time.Minute,
		Available: func() []string {
			called = true
			return []string{"X1"}
		},
	})
	defer r.StopGC()
	if uid, ok := r.Resolve("pfx:z", ""); !ok || uid != "X1" {
		t.Fatalf("应退回 Available 拿到 X1，实际 %q ok=%v", uid, ok)
	}
	if !called {
		t.Fatal("Available 没被调用 —— 回退路径没生效")
	}
}
