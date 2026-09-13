// codeartscreds_test.go 钉住 007 的核心不变量：**一个 uid = 一个进程内 *codearts.Auth**。
//
// # 这些测试守的是什么（用户实测的 503 no_healthy_account）
//
// codearts 上游的 STS 凭证约 30 分钟过期，续期用的 refresh_token 是**一次性**的。
// 改造前，同一份 auths/codearts/*.json 在进程里被表示成**两个**对象：
//
//	池 secret 那份 ← 启动时 syncCodeartsAccounts 读一次（LoadDir）
//	后台任务那份   ← 每次 SetAccounts 的闭包再 LoadDir 一次（全新对象）
//
// 而 `Auth.refreshMu` 是**对象级**锁 —— 跨对象完全无效。于是后台任务消费掉
// 一次性 refresh_token、把新凭证写进**自己那份对象**并落盘之后，池里那份
// **永远停在已作废的旧值上，且永不回读磁盘**：
//
//	所有对话请求续期失败 → 熔断 → 503 no_healthy_account: the refresh token has been used
//
// 磁盘上的新 token 其实是有效且从未用过的（重启即恢复）—— 重启只是掩盖。
//
// # 为什么必须是「指针相等」这种断言
//
// 只断言「字段值一样」是不够的：两个对象在某个瞬间可以有相同的字段值，
// 但续期只会写进其中一个 —— 断言会随着时序时红时绿（假绿）。
// 指针相等才是"池、后台任务、管理端点共享同一个可写对象"的**充要观测**。
//
// ⚠ 变异验证（必做）：把 codeartsCredStore.list 里"按 uid 复用已有指针"
// 改成"每次新建对象"，TestStoreSharesObjectWithPool 必须立刻变红。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/codearts"
)

// caFindByUID 在 store.List() 的结果里按 uid 找对象（找不到返回 nil）。
//
// 用**指针**做返回，测试才能直接比较 `==`，而不是比较字段。
func caFindByUID(list []*codearts.Auth, uid string) *codearts.Auth {
	for _, ca := range list {
		if ca.UID == uid {
			return ca
		}
	}
	return nil
}

// TestStoreSharesObjectWithPool 是本任务的核心不变量测试。
//
// 三处必须拿到**同一个指针**（Go 的 `==`）：
//
//	store.List()      —— 池 secret 的来源
//	store.Resolve(uid)—— 管理端点
//	pool.SecretOf(uid)—— 请求路径真正拿去发请求的那一份
//
// 任何一处被"重新 LoadDir / 重新构造"破坏，续期写回就打不到另外两处，
// 503 必然复发。
func TestStoreSharesObjectWithPool(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "codearts-one.json",
		credJSON("uid-ca-one", "AK_ONE", "one", 1999999999, "RT_ONE", true))
	writeCred(t, dir, "codearts-two.json",
		credJSON("uid-ca-two", "AK_TWO", "two", 1999999999, "RT_TWO", true))

	store := newCodeartsCredStore(dir)
	p := newTestPool(t)

	// 前置自检：并池必须真的生效（否则下面的断言会在空集上假通过）。
	if n := syncCodeartsAccounts(p, store); n != 2 {
		t.Fatalf("并池账号数=%d，期望 2 —— 前置条件不成立（两份凭证都该入池）", n)
	}

	list1 := store.List()
	if len(list1) != 2 {
		t.Fatalf("store.List() 返回 %d 个账号，期望 2（前置条件）", len(list1))
	}

	// 断言 1：池里的 secret 必须**就是** store.List() 里那个指针。
	//
	// 这是整个 bug 的直接观测点：池 secret 与后台任务要写回的对象若不同一个，
	// 续期永远只会写进"没人在用的那份"。
	for _, ca := range list1 {
		sec, ok := p.SecretOf(ca.UID)
		if !ok {
			t.Fatalf("池里取不到 uid=%s 的 secret —— 凭证没并入池，本测试失去意义", ca.UID)
		}
		got, ok := sec.(*codearts.Auth)
		if !ok || got == nil {
			t.Fatalf("uid=%s 的池 secret 不是 *codearts.Auth（实际 %T）", ca.UID, sec)
		}
		if got != ca {
			t.Fatalf("★ uid=%s：池 secret(%p) 与 store.List()(%p) **不是同一个对象** —— "+
				"后台续期会把新凭证写进其中一份并落盘，另一份永远停在已作废的旧 token 上，"+
				"对话请求续期全部失败 → 熔断 → 503 no_healthy_account "+
				"(the refresh token has been used)", ca.UID, got, ca)
		}
	}

	// 断言 2：重复 List() 指针不变（每次调用都重新枚举磁盘，但不得换对象）。
	for round := 2; round <= 4; round++ {
		again := store.List()
		if len(again) != len(list1) {
			t.Fatalf("第 %d 次 List() 返回 %d 个账号，第 1 次 %d 个 —— 目录没变，数量却变了",
				round, len(again), len(list1))
		}
		for i := range list1 {
			if again[i] != list1[i] {
				t.Fatalf("★ 第 %d 次 List() 第 %d 项换了新对象（%p → %p）—— "+
					"「每次重新枚举目录」被写成了「每次重新构造对象」，对象级 refreshMu 因此跨对象失效",
					round, i, list1[i], again[i])
			}
		}
	}

	// 断言 3：管理端点的解析路径必须给出同一指针。
	for _, ca := range list1 {
		r, err := store.Resolve(ca.UID)
		if err != nil {
			t.Fatalf("store.Resolve(%s) 报错: %v", ca.UID, err)
		}
		if r != ca {
			t.Fatalf("★ store.Resolve(%s)=%p 与 List()=%p 不是同一对象 —— "+
				"管理面看到/改到的凭证与请求路径用的不是同一份", ca.UID, r, ca)
		}
	}
}

// TestPoolSecretFollowsInPlaceRefresh 是最贴近线上故障场景的一条：
// 续期成功后池里那份必须**立刻**是新值（不必等下一次 sync）。
//
// 续期（client.RefreshToken）是在**内存对象上原地写字段**再落盘的。
// 因此只要指针共享，池 secret 天然跟着变 —— 这条把"共享指针"翻译成
// "池里不再是已作废的旧 token"，正是 503 的直接反面。
func TestPoolSecretFollowsInPlaceRefresh(t *testing.T) {
	const uid = "uid-ca-refresh"
	dir := t.TempDir()
	writeCred(t, dir, "codearts-r.json",
		credJSON(uid, "AK_OLD", "r", 1000000000, "RT_OLD", true))

	store := newCodeartsCredStore(dir)
	p := newTestPool(t)
	if n := syncCodeartsAccounts(p, store); n != 1 {
		t.Fatalf("并池账号数=%d，期望 1", n)
	}

	// 模拟后台续期：原地换掉一次性 refresh_token 换回来的新凭证
	// （与 client.RefreshToken 的写法一致：LockRefresh 全程持有）。
	obj := store.List()[0]
	obj.LockRefresh()
	obj.AccessKey = "AK_NEW"
	obj.SecretKey = "AK_NEW_SECRET"
	obj.SecurityToken = "AK_NEW_ST"
	obj.ExpiresAt = 1999999999
	obj.RefreshToken = "RT_NEW"
	obj.UnlockRefresh()

	sec, ok := p.SecretOf(uid)
	if !ok {
		t.Fatalf("池里取不到 uid=%s 的 secret", uid)
	}
	got, ok := sec.(*codearts.Auth)
	if !ok || got == nil {
		t.Fatalf("池 secret 不是 *codearts.Auth（实际 %T）", sec)
	}
	if got != obj {
		t.Fatalf("★ 池 secret(%p) 与续期改写的对象(%p) 不是同一个 —— "+
			"续期成功、磁盘已更新，池里却还是已作废的旧 token：这就是 503 的根因", got, obj)
	}
	if got.AccessKey != "AK_NEW" || got.RefreshToken != "RT_NEW" {
		t.Fatalf("★ 续期后的新值没反映到池 secret 上（AK=%q refresh_token 长度=%d）—— "+
			"请求路径会拿旧凭证发出去并再次失败", got.AccessKey, len(got.RefreshToken))
	}
}

// TestStoreDoesNotRecopyOwnMirrorFile 锁定设计里那条最容易被"优化掉"的规则：
//
//	胜出文件与已有对象的 FilePath **相同** → 原样保留原指针，一个字都不从磁盘抄。
//
// 理由：那个文件是对象**自己写出去的镜像**。内存才是权威 ——
// 在途的续期结果（已改内存、尚未落盘，或已落盘但磁盘内容恰好落后）如果被
// 磁盘回抄，就等于**用旧值覆盖刚换来的新凭证**，一次性 refresh_token 就此报废。
func TestStoreDoesNotRecopyOwnMirrorFile(t *testing.T) {
	const uid = "uid-ca-mirror"
	dir := t.TempDir()
	writeCred(t, dir, "codearts-m.json",
		credJSON(uid, "AK_DISK", "m", 1000000000, "RT_DISK", true))

	store := newCodeartsCredStore(dir)
	obj := store.List()[0]

	// 在途续期结果：只有内存是新值，磁盘还是旧的（落盘还没发生）。
	obj.LockRefresh()
	obj.AccessKey = "AK_INFLIGHT"
	obj.ExpiresAt = 1999999999
	obj.RefreshToken = "RT_INFLIGHT"
	obj.UnlockRefresh()

	again := store.List()
	if len(again) != 1 {
		t.Fatalf("List() 返回 %d 个账号，期望 1", len(again))
	}
	if again[0] != obj {
		t.Fatalf("★ List() 换了新对象（%p → %p）—— 对象级 refreshMu 跨对象失效", obj, again[0])
	}
	if again[0].AccessKey != "AK_INFLIGHT" || again[0].RefreshToken != "RT_INFLIGHT" {
		t.Fatalf("★ 内存里的在途续期结果被磁盘旧值回抄了（AK=%q）—— "+
			"刚用一次性 refresh_token 换来的新凭证被旧值覆盖，该账号永久报废",
			again[0].AccessKey)
	}
}

// TestStoreDirFreshness 锁定"目录新鲜度"：每次 List 都重新枚举磁盘，
// 新增立即出现、删除立即消失，**且其余账号的指针一个都不许变**。
//
// 后半句同样关键：用户点一下"刷新账号"不该让所有号都换对象
// （那等于把在途续期结果全部丢掉）。
func TestStoreDirFreshness(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "codearts-a.json",
		credJSON("uid-fresh-a", "AK_A", "a", 1999999999, "RT_A", true))
	writeCred(t, dir, "codearts-b.json",
		credJSON("uid-fresh-b", "AK_B", "b", 1999999999, "RT_B", true))

	store := newCodeartsCredStore(dir)
	before := store.List()
	if len(before) != 2 {
		t.Fatalf("初始 List() 返回 %d 个账号，期望 2（前置条件）", len(before))
	}
	objA := caFindByUID(before, "uid-fresh-a")
	objB := caFindByUID(before, "uid-fresh-b")
	if objA == nil || objB == nil {
		t.Fatalf("初始 List() 缺账号：a=%v b=%v", objA, objB)
	}

	// ---- 新增一个文件：下一次 List 立刻包含它 ----
	cPath := writeCred(t, dir, "codearts-c.json",
		credJSON("uid-fresh-c", "AK_C", "c", 1999999999, "RT_C", true))
	after := store.List()
	if len(after) != 3 {
		t.Fatalf("新增凭证后 List() 返回 %d 个账号，期望 3 —— 目录新鲜度丢了（用户刚登录完看不到号）", len(after))
	}
	if caFindByUID(after, "uid-fresh-c") == nil {
		t.Fatalf("新增的 uid-fresh-c 没出现在 List() 里: %v", caFindByUID(after, "uid-fresh-c"))
	}
	if caFindByUID(after, "uid-fresh-a") != objA || caFindByUID(after, "uid-fresh-b") != objB {
		t.Fatalf("★ 新增一个账号把其余账号的指针换掉了（a: %p→%p, b: %p→%p）—— "+
			"在途续期结果会因此全部丢失", objA, caFindByUID(after, "uid-fresh-a"),
			objB, caFindByUID(after, "uid-fresh-b"))
	}

	// ---- 删除那个文件：下一次 List 立刻不含它，其余指针仍不变 ----
	if err := os.Remove(cPath); err != nil {
		t.Fatalf("删除 %s: %v", filepath.Base(cPath), err)
	}
	shrunk := store.List()
	if len(shrunk) != 2 {
		t.Fatalf("删除凭证后 List() 返回 %d 个账号，期望 2 —— 已删掉的号还留在 store 里", len(shrunk))
	}
	if caFindByUID(shrunk, "uid-fresh-c") != nil {
		t.Fatalf("已删除的 uid-fresh-c 仍在 List() 里 —— 池子会对齐到一个不存在的账号")
	}
	if caFindByUID(shrunk, "uid-fresh-a") != objA || caFindByUID(shrunk, "uid-fresh-b") != objB {
		t.Fatalf("★ 删除一个账号把其余账号的指针换掉了（a: %p→%p, b: %p→%p）",
			objA, caFindByUID(shrunk, "uid-fresh-a"), objB, caFindByUID(shrunk, "uid-fresh-b"))
	}

	// ---- 删掉一个 uid 的**唯一**文件：它自己必须从 store 消失 ----
	if err := os.Remove(filepath.Join(dir, "codearts-b.json")); err != nil {
		t.Fatalf("删除 codearts-b.json: %v", err)
	}
	last := store.List()
	if len(last) != 1 || caFindByUID(last, "uid-fresh-a") != objA {
		t.Fatalf("★ 只剩 uid-fresh-a 时 List() 返回 %d 个（%v）且指针%v —— "+
			"uid 消失后必须从 store 移除，且其余账号指针不变",
			len(last), last, caFindByUID(last, "uid-fresh-a") == objA)
	}
}

// TestStoreAdoptsDifferentFileInPlace 锁定"胜出文件来自另一个路径"的分支：
//
//	取 LockRefresh()，把胜出者的凭证字段**原地**写进已有对象（指针不变）。
//
// 指针不变是硬要求：换成新对象等于把池里那份变成孤儿。
// 同 uid 换文件是真实场景（用户重新登录后 auths/ 里多了一份、旧的还没删）。
func TestStoreAdoptsDifferentFileInPlace(t *testing.T) {
	const uid = "uid-ca-swap"
	dir := t.TempDir()
	oldPath := writeCred(t, dir, "codearts-old.json",
		credJSON(uid, "AK_OLD", "old", 1000000000, "RT_OLD", true))

	store := newCodeartsCredStore(dir)
	first := store.List()
	if len(first) != 1 {
		t.Fatalf("初始 List() 返回 %d 个账号，期望 1（前置条件）", len(first))
	}
	obj := first[0]

	// 新文件（不同路径、更晚过期 → 按 betterCodeartsCred 胜出）。
	freshPath := writeCred(t, dir, "codearts-fresh.json",
		credJSON(uid, "AK_FRESH", "fresh", 1999999999, "RT_FRESH", true))
	mustParseUID(t, oldPath, uid)
	mustParseUID(t, freshPath, uid)

	swapped := store.List()
	if len(swapped) != 1 {
		t.Fatalf("换文件后 List() 返回 %d 个账号，期望 1（同 uid 必须只留一个对象）", len(swapped))
	}
	if swapped[0] != obj {
		t.Fatalf("★ 同 uid 换了来源文件，store 造了新对象（%p → %p）—— "+
			"池里那份立刻变成孤儿，续期写不回池子", obj, swapped[0])
	}
	if obj.AccessKey != "AK_FRESH" || obj.RefreshToken != "RT_FRESH" {
		t.Fatalf("★ 胜出凭证来自另一个路径时必须在**原对象上**更新字段，实际 AK=%q（期望 AK_FRESH）—— "+
			"池 secret 仍是旧凭证却指向同一个对象，等于静默用了过期凭证", obj.AccessKey)
	}
	if obj.FilePath != freshPath {
		t.Fatalf("原地更新后 FilePath=%q，期望 %q —— FilePath 不跟着走会让下一次 List 判错镜像归属",
			filepath.Base(obj.FilePath), filepath.Base(freshPath))
	}

	// 删掉胜出文件：回落回旧文件，**指针仍然不变**。
	if err := os.Remove(freshPath); err != nil {
		t.Fatalf("删除 codearts-fresh.json: %v", err)
	}
	back := store.List()
	if len(back) != 1 || back[0] != obj {
		t.Fatalf("★ 删除胜出文件后对象被换掉（%v）—— 指针必须自始至终不变", back[0] == obj)
	}
	if obj.AccessKey != "AK_OLD" {
		t.Fatalf("回落回来的凭证 AK=%q，期望 AK_OLD —— 来源文件换回去时字段也要跟着回", obj.AccessKey)
	}
}

// TestStoreResolveMatchesAccessKey 保持改造前的 Resolve 语义：
// 既认 uid、也认 AK（管理端点允许用 AK 当标识查账号）。
func TestStoreResolveMatchesAccessKey(t *testing.T) {
	const uid = "uid-ca-resolve"
	dir := t.TempDir()
	writeCred(t, dir, "codearts-x.json",
		credJSON(uid, "AK_RESOLVE", "x", 1999999999, "RT_X", true))

	store := newCodeartsCredStore(dir)
	obj := store.List()[0]

	byUID, err := store.Resolve(uid)
	if err != nil || byUID != obj {
		t.Fatalf("Resolve(uid) = (%p, %v)，期望 (%p, nil)", byUID, err, obj)
	}
	byAK, err := store.Resolve("AK_RESOLVE")
	if err != nil || byAK != obj {
		t.Fatalf("Resolve(AK) = (%p, %v)，期望 (%p, nil) —— 改造前 Resolve 认 AK", byAK, err, obj)
	}
	if _, err := store.Resolve("nope"); err == nil {
		t.Fatal("Resolve(不存在的标识) 应当报错")
	}

	// UIDs() 与 List() 必须是同一批账号（顺序一致，元素一一对应）。
	uids := store.UIDs()
	list := store.List()
	if len(uids) != len(list) {
		t.Fatalf("UIDs()=%v 与 List() 长度不一致（%d vs %d）", uids, len(uids), len(list))
	}
	for i, uid := range uids {
		if list[i].UID != uid {
			t.Fatalf("第 %d 项 UIDs()=%s 与 List()=%s 不一致", i, uid, list[i].UID)
		}
	}
}

// TestStoreConcurrentAccessIsSafe 钉住并发约束：store 会被
// 「每 refresh_interval 一次的后台任务」与「请求路径中的模型目录 / 管理端点」
// 同时调用，所以每个方法都必须自己加锁。
//
// 这条测试在 -race 下才有完全的杀伤力；在正常 L1 下它至少验证不 panic / 不死锁。
func TestStoreConcurrentAccessIsSafe(t *testing.T) {
	dir := t.TempDir()
	writeCred(t, dir, "codearts-c1.json",
		credJSON("uid-conc-1", "AK_C1", "c1", 1999999999, "RT_C1", true))
	writeCred(t, dir, "codearts-c2.json",
		credJSON("uid-conc-2", "AK_C2", "c2", 1999999999, "RT_C2", true))

	store := newCodeartsCredStore(dir)
	p := newTestPool(t)
	syncCodeartsAccounts(p, store)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				for _, ca := range store.List() {
					if r, err := store.Resolve(ca.UID); err != nil || r != ca {
						t.Errorf("并发 Resolve(%s) 给出的不是同一指针", ca.UID)
						return
					}
				}
				store.UIDs()
			}
		}()
	}
	wg.Wait()
}

// credJSONWithRealDPoP 造一份**带真 DPoP 私钥**的扁平形凭证。
//
// 为什么不能复用 credJSON：那里塞的是 `{"x":"a","y":"b","d":"c"}` —— 假 JWK。
// 带假 JWK 的凭证在 `RefreshToken` 里会在**发请求之前**就失败（"恢复 DPoP 密钥"），
// 于是测试根本到不了 HTTP 层，续期永远不会真的发生（假绿的典型形态：
// 断言"对象没变"永远成立，因为什么都没跑）。
func credJSONWithRealDPoP(t *testing.T, uid, ak, nickname string, expiresAt int64, refreshToken string) string {
	t.Helper()
	kp, err := codearts.NewDPoPKeyPair()
	if err != nil {
		t.Fatalf("生成 DPoP 密钥对: %v", err)
	}
	jwk, err := json.Marshal(kp.PrivateJWK())
	if err != nil {
		t.Fatalf("序列化 DPoP JWK: %v", err)
	}
	return `{"accessKeyId":"` + ak + `",` +
		`"secretAccessKey":"` + ak + `_SECRET",` +
		`"securityToken":"` + ak + `_ST",` +
		`"expiresAt":` + itoa(expiresAt) + `,` +
		`"refresh_token":"` + refreshToken + `",` +
		`"clientId":"vscode-codebot",` +
		`"uid":"` + uid + `",` +
		`"nickname":"` + nickname + `",` +
		`"dpopPrivateKeyJwk":` + string(jwk) + `}`
}

// TestStoreAdoptsSamePathExternalRewrite 钉住评审 F2。
//
// # 这个盲区为什么是真的
//
// cmd/login 的文件名规则是 `codearts-<uid>.json`（见 internal/codearts/login.go），
// 于是**同 uid 必然同路径**。原先"同路径一个字都不回抄"的规则会导致：
// 用户重新登录同一个账号 / 手工修好这份文件 → store 永远不采用 →
// 账号一直拿着已作废的凭证，只能重启网关。
//
// 判据是"磁盘那份不比内存旧才采用"，所以这里给磁盘一个**更晚**的过期时刻。
func TestStoreAdoptsSamePathExternalRewrite(t *testing.T) {
	const uid = "uid-ca-rewrite"
	dir := t.TempDir()
	path := writeCred(t, dir, "codearts-rw.json",
		credJSON(uid, "AK_V1", "rw", 1700000000, "RT_V1", true))

	store := newCodeartsCredStore(dir)
	obj := store.List()[0]
	if obj.RefreshToken != "RT_V1" {
		t.Fatalf("前置条件不成立：初始 refresh_token=%q", obj.RefreshToken)
	}

	// 同一个路径、同一份文件被**外部**重写（等价于重新登录同一个 uid）。
	if err := os.WriteFile(path, []byte(
		credJSON(uid, "AK_V2", "rw", 1800000000, "RT_V2", true)), 0o600); err != nil {
		t.Fatal(err)
	}

	back := store.List()
	if len(back) != 1 || back[0] != obj {
		t.Fatalf("★ 外部改写后对象被换掉（%v）—— 指针必须自始至终不变", back[0] == obj)
	}
	if obj.RefreshToken != "RT_V2" || obj.AccessKey != "AK_V2" || obj.ExpiresAt != 1800000000 {
		t.Fatalf("★ 同路径的外部改写没被采用（AK=%q RT=%q exp=%d）—— "+
			"重新登录同一账号后网关会一直用已作废的凭证",
			obj.AccessKey, obj.RefreshToken, obj.ExpiresAt)
	}
}

// TestStoreKeepsMemoryWhenDiskMirrorIsOlder 是上面那条的**反面**，也是最危险的一面：
// 续期成功改了内存、但 SaveAtomic **落盘失败**时，磁盘上留的是更旧的
// （已被服务端消费掉的）凭证。此时若 List 回抄磁盘，就等于把刚用一次性
// token 换回来的新凭证丢掉 —— 该账号永久报废，只能重新走浏览器登录。
func TestStoreKeepsMemoryWhenDiskMirrorIsOlder(t *testing.T) {
	const uid = "uid-ca-oldermirror"
	dir := t.TempDir()
	path := writeCred(t, dir, "codearts-om.json",
		credJSON(uid, "AK_OLD", "om", 1700000000, "RT_OLD", true))

	store := newCodeartsCredStore(dir)
	obj := store.List()[0]

	// 在途续期结果：内存是新的（更晚过期 + 新 token），磁盘还是旧的。
	obj.LockRefresh()
	obj.AccessKey = "AK_NEW"
	obj.ExpiresAt = 1900000000
	obj.RefreshToken = "RT_NEW"
	obj.UnlockRefresh()

	// 磁盘保持旧值不动（模拟落盘失败）。
	_ = path
	back := store.List()
	if len(back) != 1 || back[0] != obj {
		t.Fatal("对象被换掉了")
	}
	if obj.RefreshToken != "RT_NEW" || obj.AccessKey != "AK_NEW" {
		t.Fatalf("★ 磁盘上的**更旧**镜像被回抄了（AK=%q RT=%q）—— "+
			"刚换来的新凭证被丢弃，该账号永久报废",
			obj.AccessKey, obj.RefreshToken)
	}
}

// TestStoreEmptyDirDoesNotGlobCwd 钉住评审 F3：dir 为空必须返回空，
// 不得退化成"相对 CWD 的 codearts*.json 通配"。
func TestStoreEmptyDirDoesNotGlobCwd(t *testing.T) {
	// 在一个临时 CWD 里放一份"看起来像凭证"的文件：
	// LoadDir("") 的通配会命中它（filepath.Join("", "codearts*.json") == "codearts*.json"）。
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	writeCred(t, tmp, "codearts-cwd.json",
		credJSON("uid-cwd", "AK_CWD", "cwd", 1999999999, "RT_CWD", true))

	if got := newCodeartsCredStore("").List(); len(got) != 0 {
		t.Fatalf("★ 空目录返回了 %d 个账号（应当 0）—— "+
			"它把进程 CWD 下的 codearts*.json 当成了凭证", len(got))
	}
}

// TestBackgroundRefreshLandsOnPoolObject 是 007 的**装配级**端到端断言（评审 F1）。
//
// # 为什么必须有它
//
// 上面那些测试都自己 `newCodeartsCredStore(dir)` 再调 `syncCodeartsAccounts` ——
// 守的是 store/sync 这一对函数。而 007 的根因在**装配层那一句**
// （`cb.SetAccounts` 原来是"每次 LoadDir 造一批新对象"的闭包）。
// 实测：把装配退回旧闭包，`go build` + `go test ./cmd/server/` **全绿**。
//
// 所以这里走**真实的装配函数** `wireCodeartsCreds` + 真实的续期任务：
//
//	① 走 wireCodeartsCreds 接线（后台任务用的就是它的 accessor）
//	② 走 syncCodeartsAccounts 把对象装进池子
//	③ 跑**真正的后台任务** cb.Jobs()[0].Run(ctx) —— 它内部经 localAccounts()
//	   拿到 accessor 给的那份对象，然后消费一次性 refresh_token 并落盘
//	④ 断言**池子里那个对象**（请求路径真正用的那份）拿到了新凭证
//
// 第 ④ 步是判据：只要 accessor 又变成"每次造新对象"，续期就写到别人的对象上，
// 池里那份仍是旧 token → 本测试变红。
func TestBackgroundRefreshLandsOnPoolObject(t *testing.T) {
	const uid = "uid-ca-assembly"
	dir := t.TempDir()
	// 凭证必须**已过期**，后台任务才会真的续期（这也是生产故障的现场条件）。
	writeCred(t, dir, "codearts-asm.json",
		credJSONWithRealDPoP(t, uid, "AK_ASSEMBLY", "asm",
			time.Now().Add(-time.Hour).Unix(), "RT_ASSEMBLY_OLD"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readAllLimitedBody(r)
		form, _ := url.ParseQuery(string(raw))
		if form.Get("refresh_token") == "" {
			t.Errorf("续期请求里没有 refresh_token（表单体=%q）", string(raw))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"credentials":{"access_key_id":"AK_ASSEMBLY_NEW",`+
			`"secret_access_key":"SK_NEW","security_token":"ST_NEW",`+
			`"expiration":"2099-01-01T00:00:00Z"},"refresh_token":"RT_ASSEMBLY_NEW"}`)
	}))
	t.Cleanup(srv.Close)

	cl := codearts.New()
	cl.STSBase = srv.URL
	cb := codearts.NewWithConfig(codearts.Config{Client: cl, AuthDir: dir})
	cb.SetRefreshInterval(time.Minute)

	// ①+② 走真实装配
	// hist 传 nil：本用例只验"续期打回池子里那个对象"，不碰福利历史。
	// nil 是**合法值**（上游必须判空）—— 这里顺带把它当默认路径跑一遍。
	creds := wireCodeartsCreds(cb, dir, nil)
	p := newTestPool(t)
	syncCodeartsAccounts(p, creds)

	before, ok := p.SecretOf(uid)
	if !ok {
		t.Fatal("前置条件不成立：池子里没有这个 uid")
	}
	poolObj, ok := before.(*codearts.Auth)
	if !ok {
		t.Fatalf("池 secret 类型 = %T，期望 *codearts.Auth", before)
	}
	if poolObj.RefreshToken != "RT_ASSEMBLY_OLD" {
		t.Fatalf("前置条件不成立：池里 refresh_token=%q", poolObj.RefreshToken)
	}

	// ③ 跑真正的后台续期任务
	jobs := cb.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("Jobs() 返回 %d 个任务，期望 1（前置条件）", len(jobs))
	}
	if err := jobs[0].Run(context.Background()); err != nil {
		t.Fatalf("后台续期任务报错: %v", err)
	}

	// ④ 池子里那个对象必须拿到新凭证
	after, _ := p.SecretOf(uid)
	poolObjAfter, ok := after.(*codearts.Auth)
	if !ok {
		t.Fatalf("池 secret 类型 = %T，期望 *codearts.Auth", after)
	}
	if poolObjAfter != poolObj {
		t.Fatalf("★ 池 secret 换成了新对象（%p → %p）—— 续期结果写不回请求路径用的那份",
			poolObj, poolObjAfter)
	}
	if poolObj.RefreshToken != "RT_ASSEMBLY_NEW" || poolObj.AccessKey != "AK_ASSEMBLY_NEW" {
		t.Fatalf("★ 后台续期成功但**池子里那份对象没变**（AK=%q RT=%q）—— "+
			"这正是 503 no_healthy_account 的根因：续期写到了另一个对象上",
			poolObj.AccessKey, poolObj.RefreshToken)
	}
}

// readAllLimitedBody 读完请求体（上限 1 MiB），与 internal/codearts 里同名工具同语义。
func readAllLimitedBody(r *http.Request) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil || len(buf) > 1<<20 {
			return buf, nil
		}
	}
}
