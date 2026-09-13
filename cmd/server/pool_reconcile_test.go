// pool_reconcile_test.go 锁定「幽灵账号」修复的判据（见 pool_reconcile.go）。
//
// # 这个文件守的是什么
//
// 用户实测：先用启用了 codearts 的 config 跑过（2 份凭证入池并落盘），
// 再改用**没有 codearts 段**的 config 启动（上游不注册），但持久化的
// state.json 让那 2 个 codearts 账号照样回到池子里 —— 它们永远选得到、
// 却没有上游能服务、也没有续期任务。修复就是装配层的一次对账。
//
// # 为什么用真实 pool 而不是 mock
//
// 故障恰恰体现为"池子里到底还剩谁"，那是 pool 的真实状态。
// mock 一个只会按我预期作动的池子，等于把被测对象换成了我自己 ——
// 真实实现（providerOf 的默认上游回落、SyncToDirWithSecrets 的分域剔除）
// 正是本文件要钉的东西，它们全在 pool 里。
//
// # 变异验证（本文件的关键用例因此是场景 B）
//
// 把 pool_reconcile.go 里 `if prov == "" { continue }` 注释掉，
// TestPruneKeepsUntaggedLegacyAccounts 的「默认上游未设」子用例**必红**；
// 把该行改回来即恢复绿。若这条用例不变红，就说明空串守卫没有任何断言覆盖。
package main

import (
	"fmt"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// ---------------------------------------------------------------------------
// 造池子的公共件
// ---------------------------------------------------------------------------

// newReconcilePool 造一个**真实** Pool 且全程内存态（stateFp=""）。
//
// 空 stateFp 是刻意的：既不读也不写 state.json，测试不会污染真实状态文件，
// 也不会留下后台落盘 goroutine 在测试结束后写盘。
func newReconcilePool(t *testing.T) *pool.Pool {
	t.Helper()
	return pool.New("")
}

// seedProvider 往指定上游塞 count 个账号，并**前置自检**它们真的进了池子。
//
// 走 SyncToDirWithSecrets 是因为它正是装配层入池的入口（main.go 的
// syncCodeartsAccounts 与 p.SyncToDir 同源），语义与生产路径一致：
// "本次同步的账号集合就是这个上游在池中应有的集合"。
func seedProvider(t *testing.T, p *pool.Pool, provider string, count int) {
	t.Helper()
	auths := make([]*auth.Auth, 0, count)
	for i := 1; i <= count; i++ {
		auths = append(auths, &auth.Auth{
			UID:      fmt.Sprintf("%s-uid-%d", provider, i),
			Nickname: fmt.Sprintf("%s-号-%d", provider, i),
		})
	}
	p.SyncToDirWithSecrets(provider, auths, nil)
	if got := len(p.ListFor(provider)); got != count {
		t.Fatalf("前置条件不成立：往上游 %q 塞了 %d 个账号，池里只有 %d 个 —— "+
			"账号没真的进池，下面所有断言都会失去意义（不是被测逻辑的问题，是造数据失败）",
			provider, count, got)
	}
}

// seedUntagged 往池子里放 count 个**不带 provider 标签**的历史账号。
//
// provider 显式传 "" 是关键：pool.AddFor 会把空上游归一成"当时的默认上游"，
// 因此调用顺序决定了这些号在池内是"有标签"还是"无标签"：
//
//	SetDefaultProvider 之后放 → entry.provider = 默认上游（有标签）
//	SetDefaultProvider 之前放 → entry.provider = ""，生效标识靠回落
func seedUntagged(t *testing.T, p *pool.Pool, count int) {
	t.Helper()
	for i := 1; i <= count; i++ {
		p.AddFor("", &auth.Auth{
			UID:      fmt.Sprintf("legacy-uid-%d", i),
			Nickname: fmt.Sprintf("历史号-%d", i),
		}, nil)
	}
	if got := len(p.List()); got != count {
		t.Fatalf("前置条件不成立：放了 %d 个未打标签的历史账号，池里只有 %d 个", count, got)
	}
}

// collectLogs 收集 logf 里的日志行，用于断言"用户能不能看懂号为什么不见了"。
func collectLogs() (*[]string, func(format string, args ...any)) {
	var lines []string
	return &lines, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
}

// ---------------------------------------------------------------------------
// 用例 1：逐出未注册上游
// ---------------------------------------------------------------------------

// TestPruneEvictsUnregisteredProvider 是本修复的**主场景**：
// 池里 codearts 2 个 + workbuddy 3 个，本次只注册了 workbuddy
// ⇒ codearts 被逐出（1 个上游 / 2 个账号），workbuddy 一个不许动。
//
// 同时钉住日志内容：用户看到号不见了，必须能从日志里知道
// "是本次启动没注册这个上游"以及"凭证文件还在"，
// 否则他只会以为凭证被删了（那是最糟的误解）。
func TestPruneEvictsUnregisteredProvider(t *testing.T) {
	p := newReconcilePool(t)
	seedProvider(t, p, "codearts", 2)
	seedProvider(t, p, "workbuddy", 3)

	lines, logf := collectLogs()
	gotProviders, gotAccounts := pruneUnregisteredProviders(p, []string{"workbuddy"}, logf)

	if gotProviders != 1 || gotAccounts != 2 {
		t.Fatalf("对账返回 (%d 个上游, %d 个账号)，期望 (1, 2) —— "+
			"返回 0 说明幽灵账号根本没被逐出（codearts 号仍在池里、仍选得到却没有上游能服务）；"+
			"返回多余的值说明误伤了本次注册过的上游", gotProviders, gotAccounts)
	}

	if left := p.ListFor("codearts"); len(left) != 0 {
		t.Fatalf("逐出后 codearts 还剩 %d 个账号（%v），期望 0 —— "+
			"这些号没有上游能服务，留在池里只会让请求选中后必然失败", len(left), left)
	}
	if left := p.ListFor("workbuddy"); len(left) != 3 {
		t.Fatalf("workbuddy 被打成 %d 个账号，期望仍是 3 —— "+
			"本次注册过的上游被误伤，用户会莫名其妙少号", len(left))
	}

	if len(*lines) != 1 {
		t.Fatalf("逐出产生了 %d 条日志，期望 1 条（一个上游一条）—— "+
			"日志条数与逐出动作对不上，要么静默删号、要么刷屏。实际日志=%q", len(*lines), *lines)
	}
	msg := (*lines)[0]
	for _, want := range []string{"codearts", "未注册", "凭证文件未改动"} {
		if !strings.Contains(msg, want) {
			t.Errorf("日志里没有 %q，用户无法据此判断「为什么号不见了、凭证还在不在」。实际日志=%q", want, msg)
		}
	}
	if !strings.Contains(msg, "2") {
		t.Errorf("日志里没有逐出数量 2，用户无法知道少了几个号。实际日志=%q", msg)
	}
}

// ---------------------------------------------------------------------------
// 用例 2（关键红线）：不误删"没打标签的历史账号"
// ---------------------------------------------------------------------------

// TestPruneKeepsUntaggedLegacyAccounts 守的是**最贵的一条**：
// 对账绝不能把"没有 provider 标签的历史账号"整批删掉。
//
// 那批号在 pool 里没有标签，生效标识靠默认上游回落（pool.providerOf）：
//
//	默认上游已注入 → 生效标识 = 默认上游（在 known 里 ⇒ 安全）
//	默认上游未注入 → 生效标识 = ""（**不在 known 里** ⇒ 危险）
//
// 后一条正是"把对账放到 SetDefaultProvider 之前"的等价复现，
// 也是 pruneUnregisteredProviders 里空串守卫唯一被覆盖的路径。
//
// # 这条用例的价值
//
// 若把对账挪到 SetDefaultProvider 之前，或去掉 `if prov == "" { continue }`，
// 场景 B 必红（池子被清空）。这条断言是"两道防线都存在"的证据，
// 没有它，空串守卫就是一行没有任何测试覆盖的代码。
func TestPruneKeepsUntaggedLegacyAccounts(t *testing.T) {
	// ---- 场景 A：装配顺序正确（先 SetDefaultProvider，再入池）----
	//
	// 未打标签的号在池内的生效上游就是默认上游 workbuddy，known 含它 ⇒ 一个不删。
	t.Run("默认上游已设_未打标签的号归默认上游", func(t *testing.T) {
		p := newReconcilePool(t)
		p.SetDefaultProvider("workbuddy") // 生产里对应 main.go 的 SetDefaultProvider
		seedUntagged(t, p, 3)

		// 前置自检：这 3 个号在 Providers() 里确实以 workbuddy 出现
		//（而不是以空串出现）—— 证明场景 A 走的不是空串守卫那条路。
		if got := p.Providers(); len(got) != 1 || got[0] != "workbuddy" {
			t.Fatalf("前置条件不成立：默认上游已设时 Providers()=%v，期望 [workbuddy] —— "+
				"场景 A 没有真正构造出来，本子用例会假通过", got)
		}

		before := len(p.ListFor("workbuddy"))
		gotProviders, gotAccounts := pruneUnregisteredProviders(p, []string{"workbuddy"}, nil)

		if gotProviders != 0 || gotAccounts != 0 {
			t.Fatalf("默认上游的账号被当成未注册上游逐出：返回 (%d, %d)，期望 (0, 0) —— "+
				"用户的号会莫名其妙消失，而重启一次就少一批", gotProviders, gotAccounts)
		}
		if after := len(p.ListFor("workbuddy")); after != before {
			t.Fatalf("workbuddy 的账号数从 %d 变成 %d —— "+
				"没有标签的历史账号被误删，账号池一重启就空", before, after)
		}
	})

	// ---- 场景 B：默认上游**未设**（= 对账被提前到 SetDefaultProvider 之前）----
	//
	// 此时未打标签的号生效标识是空串，Providers() 返回的就是 ""。
	// 空串不在 known 里，唯一的保护是 pruneUnregisteredProviders 里的空串守卫。
	// 去掉那一行，本子用例必红。
	t.Run("默认上游未设_空串必须被跳过", func(t *testing.T) {
		p := newReconcilePool(t)
		// 刻意**不调** SetDefaultProvider：模拟"先入池/postpone 之后才设默认"的时序
		seedUntagged(t, p, 3)

		// 前置自检：Providers() 必须返回空串 —— 证明本子用例真的走到了空串那条路径。
		// 没有这条自检，将来 pool 改了归一逻辑（例如入池时就强制打标签），
		// 本子用例会**静默退化成场景 A**，看起来还绿，实际什么都没守住。
		if got := p.Providers(); len(got) != 1 || got[0] != "" {
			t.Fatalf("前置条件不成立：默认上游未设时 Providers()=%v，期望 [\"\"] —— "+
				"空串路径没被走到，本子用例守不住空串守卫，会假绿", got)
		}

		before := len(p.List())
		gotProviders, gotAccounts := pruneUnregisteredProviders(p, []string{"workbuddy"}, nil)

		if gotProviders != 0 || gotAccounts != 0 {
			t.Fatalf("空串上游被当成「未注册上游」逐出：返回 (%d, %d)，期望 (0, 0) —— "+
				"『没有 provider 标签的历史账号』会被整批删除（它们回落的生效标识就是空串），"+
				"用户重启一次就丢掉全部老账号", gotProviders, gotAccounts)
		}
		if after := len(p.List()); after != before {
			t.Fatalf("池子里的账号从 %d 个变成 %d 个 —— "+
				"空串守卫失效，未打标签的历史账号被静默删除", before, after)
		}
	})
}

// ---------------------------------------------------------------------------
// 用例 3：known 为空 ⇒ 不动池子
// ---------------------------------------------------------------------------

// TestPruneNoopWhenKnownEmpty 守的是**防御性**：
// 拿不到注册表（known 为空）时，绝不能"因为所有上游都不在 known 里"而清空池子。
//
// 为什么这条重要：known 来自 registry.IDs()，它为空意味着装配链某一环出了问题。
// 那种时候正确的反应是"什么都不做"，而不是把用户的账号全部删掉 ——
// 后者会让一个装配失误升级成数据事故。
func TestPruneNoopWhenKnownEmpty(t *testing.T) {
	p := newReconcilePool(t)
	seedProvider(t, p, "codearts", 2)
	before := len(p.List())

	for _, known := range [][]string{nil, {}} {
		gotProviders, gotAccounts := pruneUnregisteredProviders(p, known, nil)
		if gotProviders != 0 || gotAccounts != 0 {
			t.Fatalf("known=%v（长度为 0）时返回 (%d, %d)，期望 (0, 0) —— "+
				"注册表拿不到就把整个池子清空，一次装配失误就能删光用户的账号",
				known, gotProviders, gotAccounts)
		}
		if after := len(p.List()); after != before {
			t.Fatalf("known=%v 时池子账号数从 %d 变成 %d —— "+
				"空注册表下池子被动过，这是最不可逆的一类误删", known, before, after)
		}
	}
	if left := p.ListFor("codearts"); len(left) != 2 {
		t.Fatalf("codearts 账号从 2 个变成 %d 个，期望原样保留 —— known 为空时不该有任何动作", len(left))
	}
}

// ---------------------------------------------------------------------------
// 用例 4：幂等
// ---------------------------------------------------------------------------

// TestPruneIdempotent 锁定幂等：同一次启动里对账被调用两次（或第二次启动时
// 池子已经是干净的）不能产生额外动作，第二次必须返回 (0, 0)。
//
// 为什么重要：返回值会被写成日志。若第二次仍报"逐出 N 个"，
// 日志就在说谎，用户会以为每次启动都在丢号。
func TestPruneIdempotent(t *testing.T) {
	p := newReconcilePool(t)
	seedProvider(t, p, "codearts", 2)
	seedProvider(t, p, "workbuddy", 3)

	first, firstAcc := pruneUnregisteredProviders(p, []string{"workbuddy"}, nil)
	if first != 1 || firstAcc != 2 {
		t.Fatalf("第一次对账返回 (%d, %d)，期望 (1, 2) —— "+
			"前置条件不成立，后面的幂等断言就不成立", first, firstAcc)
	}

	second, secondAcc := pruneUnregisteredProviders(p, []string{"workbuddy"}, nil)
	if second != 0 || secondAcc != 0 {
		t.Fatalf("第二次对账返回 (%d, %d)，期望 (0, 0) —— "+
			"对账不幂等，日志会在每次启动时谎报丢号，用户无法判断到底是哪一次丢的",
			second, secondAcc)
	}
	if left := len(p.ListFor("workbuddy")); left != 3 {
		t.Fatalf("workbuddy 账号数=%d，期望仍是 3 —— 重复对账误伤了本次注册过的上游", left)
	}
}
