package main

// codeartscreds_race_test.go —— store.List() 与「续期在途」的同步。
//
// # 为什么需要这个文件（对抗评审 R1）
//
// store.list() 要判断"磁盘那份凭证是否比内存新"，于是它读对象的
// ExpiresAt / RefreshToken 等字段。而写这些字段的是
// client.RefreshToken 与 client.adoptDiskRefreshToken —— 它们持的是
// **Auth.refreshMu / mu**，与 store 自己的 s.mu 没有任何同步关系。
//
// 原先 list() 只在 s.mu 下做"读 + 比较 + 原地写"，于是这是一个 data race；
// 而且有一个具体交错能让账号**永久报废**：
//
//	1. list() 的 LoadDir 读到旧盘值 w
//	2. 此刻 RefreshToken 正在同一对象上跑：内存已写新值，尚未 SaveAtomic
//	3. 无锁读 cur.ExpiresAt 拿到尚未发布的旧值 → 判据成立 → 判"有差异"
//	4. 原地写时才取 refreshMu（阻塞到续期落盘）
//	5. → 用第 1 步那份旧盘值覆盖刚用一次性 token 换回的新凭证
//
// 修法是让"读-判-写"整段进入 refreshMu 临界区。
//
// # 为什么用"锁争用"而不是 -race
//
// 本机跑不了 `go test -race`：它需要 cgo，而 Windows 上没有 gcc。
// 试过用 zig 当 C 编译器（`CGO_ENABLED=1 CC='zig cc'`）—— 编译能过，
// 但链接 tsan 运行时失败：zig 自带的 mingw 导入库缺
// `WaitOnAddress` / `WakeByAddressSingle`（`-lsynchronization` 在 zig 里也找不到）。
// 所以这里不依赖 race detector，而是**确定性地**证明同步存在：
//
//	先持有对象的 refreshMu（模拟续期在途），再让 list() 跑 ——
//	它必须被挡住；释放后才允许完成。
//
// 这个判据的优点是**不依赖时序运气**：漏掉锁会 100% 红，而不是"偶尔红"。
// 同一个不变量在 CI 上还应该配一条 -race 的用例（见 ci.yml）。

import (
	"testing"
	"time"
)

// TestStoreListWaitsForInFlightRefresh 钉住 store.list() 必须等对象的 refreshMu。
//
// 漏掉这把锁 = 在无同步的情况下读凭证字段（data race），
// 并可能用旧盘值覆盖刚续期得到的新凭证。
func TestStoreListWaitsForInFlightRefresh(t *testing.T) {
	dir := t.TempDir()
	const uid = "uid-ca-race"
	writeCred(t, dir, "codearts-race.json",
		credJSON(uid, "AK_RACE", "race", 1999999999, "RT_RACE", true))

	store := newCodeartsCredStore(dir)
	// 第一次 list 把该 uid 的对象装进 byUID（后续调用才会走到"同路径比较"分支）。
	if _, err := store.list(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	cur := store.byUID[uid]
	store.mu.Unlock()
	if cur == nil {
		t.Fatalf("store 里没有 uid=%s 的对象", uid)
	}

	// 模拟"续期在途"：对象自己的续期锁被持有。
	cur.LockRefresh()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = store.list()
	}()

	select {
	case <-done:
		cur.UnlockRefresh()
		t.Fatal("续期在途时 store.list() 直接返回了 —— 它没有等待 refreshMu。\n" +
			"这意味着它在无同步的情况下读凭证字段（data race），\n" +
			"并可能用 LoadDir 读到的旧盘值覆盖刚续期得到的新凭证（账号永久报废）。")
	case <-time.After(300 * time.Millisecond):
		// 正确：被 refreshMu 挡住了。
	}

	// 释放后必须能完成 —— 顺带排除"其实是死锁"。
	cur.UnlockRefresh()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("释放 refreshMu 后 store.list() 仍未返回 —— 死锁（加锁顺序被写反了？）")
	}
}

// TestStoreListDoesNotRollBackInFlightRefresh 钉住"内存比磁盘新时不许回抄"。
//
// 这是上面那个交错要保护的结果不变式：续期已把新凭证写进内存、尚未落盘时，
// List() 读到的磁盘那份是**旧的且已作废**。回抄它 = 丢掉刚用一次性 token
// 换回的新凭证，该账号只能重新走浏览器登录。
func TestStoreListDoesNotRollBackInFlightRefresh(t *testing.T) {
	dir := t.TempDir()
	const uid = "uid-ca-roll"
	// 磁盘上是**旧的**凭证：过期时刻在过去、refresh_token 是旧的那个。
	writeCred(t, dir, "codearts-roll.json",
		credJSON(uid, "AK_OLD", "roll", 1000000000, "RT_OLD", true))

	store := newCodeartsCredStore(dir)
	if _, err := store.list(); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	cur := store.byUID[uid]
	store.mu.Unlock()
	if cur == nil {
		t.Fatalf("store 里没有 uid=%s 的对象", uid)
	}

	// 模拟"刚续期完、还没 SaveAtomic"：内存已比磁盘新。
	// 加锁方式刻意与 client.RefreshToken 一致（refreshMu → mu）。
	cur.LockRefresh()
	cur.Lock()
	cur.RefreshToken = "RT_NEW"
	cur.AccessKey = "AK_NEW"
	cur.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
	cur.Unlock()
	cur.UnlockRefresh()

	list, err := store.list()
	if err != nil {
		t.Fatal(err)
	}
	got := caFindByUID(list, uid)
	if got == nil {
		t.Fatalf("list() 把 uid=%s 丢了", uid)
	}
	if got != cur {
		t.Fatal("list() 换掉了对象 —— 池 secret / 后台任务 / 管理端点会全部变成孤儿")
	}
	if got.RefreshToken != "RT_NEW" || got.AccessKey != "AK_NEW" {
		t.Fatalf("List() 用旧盘值覆盖了续期结果：AK=%q RT=%q（期望 AK_NEW / RT_NEW）。\n"+
			"旧 refresh_token 已作废，这会让该账号永久报废、只能重新登录。",
			got.AccessKey, got.RefreshToken)
	}
}
