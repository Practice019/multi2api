package authlimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// baseTime 定一个固定基准时刻：本包的判据全都要靠调用方传入的 now，
// 用例里就用它 + 偏移量推进时间，不碰真实时钟。
func baseTime() time.Time {
	return time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
}

// TestLimiterAllowsUpToMaxThenRejects 窗口内第 max 次放行、第 max+1 次拒绝，
// 且 retryAfter 正好指向"最旧那次放行滑出窗口"的时刻。
func TestLimiterAllowsUpToMaxThenRejects(t *testing.T) {
	// 窗口与规模取 new-api 关键路径限速的量级（20 次 / 20 分钟，
	// common/constants.go:230-232），次数改小便于断言。
	l := NewLimiter(20*time.Minute, 3)
	now := baseTime()

	for i := 1; i <= 3; i++ {
		ok, retry := l.Allow("1.2.3.4", now)
		if !ok {
			t.Fatalf("第 %d 次应当放行，得到拒绝（retry=%v）", i, retry)
		}
		if retry != 0 {
			t.Errorf("放行时 retryAfter 应为 0，得到 %v", retry)
		}
	}
	if got := l.Count("1.2.3.4", now); got != 3 {
		t.Errorf("窗口内计数应为 3，得到 %d", got)
	}

	ok, retry := l.Allow("1.2.3.4", now.Add(30*time.Second))
	if ok {
		t.Fatal("第 4 次应当被拒")
	}
	if want := 20*time.Minute - 30*time.Second; retry != want {
		t.Errorf("retryAfter 应为 %v（最旧那次放行还要多久滑出窗口），得到 %v", want, retry)
	}

	// ⚠ 被拒的这一**不**记账：否则持续刷的客户端每失败一次就把窗口往后推，
	// 永远等不到恢复。
	if got := l.Count("1.2.3.4", now.Add(30*time.Second)); got != 3 {
		t.Errorf("拒绝不应记账，计数应仍是 3，得到 %d", got)
	}
}

// TestLimiterSlidesWindowAndSeparatesKeys 窗口滑过就恢复；一个 key 不影响另一个。
func TestLimiterSlidesWindowAndSeparatesKeys(t *testing.T) {
	l := NewLimiter(time.Minute, 1)
	now := baseTime()

	if ok, _ := l.Allow("ip-a", now); !ok {
		t.Fatal("ip-a 第一次应当放行")
	}
	if ok, _ := l.Allow("ip-b", now); !ok {
		t.Error("ip-b 不该被 ip-a 用掉的额度影响（分 key 是这套东西的前提）")
	}

	if ok, retry := l.Allow("ip-a", now.Add(59*time.Second)); ok || retry != time.Second {
		t.Errorf("窗口未滑过应当拒绝且还剩 1s，得到 ok=%v retry=%v", ok, retry)
	}
	if ok, _ := l.Allow("ip-a", now.Add(time.Minute)); !ok {
		t.Error("窗口正好滑过（t+window）后应当恢复放行")
	}
}

// TestLimiterResetAndPrune Reset 只清一个 key；Prune 只清窗口外的 key。
func TestLimiterResetAndPrune(t *testing.T) {
	l := NewLimiter(time.Minute, 2)
	now := baseTime()

	l.Allow("ip-a", now)
	l.Allow("ip-b", now)
	if n := l.Prune(now.Add(30 * time.Second)); n != 0 {
		t.Errorf("窗口内的 key 不该被清，清了 %d 个", n)
	}
	if n := l.Prune(now.Add(2 * time.Minute)); n != 2 {
		t.Errorf("两个 key 都过期在外，应当清掉 2 个，清了 %d 个", n)
	}
	if got := l.Count("ip-a", now.Add(2*time.Minute)); got != 0 {
		t.Errorf("被 Prune 掉的 key 计数应为 0，得到 %d", got)
	}

	l.Allow("ip-x", now)
	l.Allow("ip-y", now)
	l.Reset("ip-x")
	if got := l.Count("ip-x", now); got != 0 {
		t.Errorf("Reset 后计数应为 0，得到 %d", got)
	}
	if got := l.Count("ip-y", now); got != 1 {
		t.Errorf("Reset 不该动别人的窗口，ip-y 计数应为 1，得到 %d", got)
	}
	l.Reset("") // 空 key 是 no-op，不该 panic
}

// TestLimiterEmptyKeyAlwaysRejected 空 key（含纯空白）一律拒绝 —— 这是本文件里
// 最要紧的一条判据，两种"温和"的处理都是错的：退化成共用窗口会让一个刷子把
// 所有人挡在门外；空 key 就放行等于给攻击者一条免检通道。见 Allow 的注释。
func TestLimiterEmptyKeyAlwaysRejected(t *testing.T) {
	l := NewLimiter(time.Minute, 5)
	now := baseTime()

	for _, key := range []string{"", " ", "\t", "\n\r\n", "   \t "} {
		ok, retry := l.Allow(key, now)
		if ok {
			t.Errorf("空 key %q 必须拒绝，却放行了", key)
		}
		// 空 key 没有"最旧的一次"可算，只能给窗口长度这个保守上界。
		if retry != time.Minute {
			t.Errorf("空 key %q 的 retryAfter 应为窗口长度 %v，得到 %v", key, time.Minute, retry)
		}
		if got := l.Count(key, now); got != 0 {
			t.Errorf("空 key %q 不该有任何计数，得到 %d", key, got)
		}
	}

	// 放行路径不受影响（别把"空 key 被拒"做成"全都拒绝"）。
	if ok, _ := l.Allow("1.2.3.4", now); !ok {
		t.Error("正常 key 应当照常放行")
	}
	// 键的归一化口径与 Allow 一致：带空白的正常 key 归到同一个窗口。
	if ok, _ := l.Allow(" 1.2.3.4 ", now); !ok {
		t.Fatal("带空白的同一个 key 第二次应当仍放行（5 次额度）")
	}
	if got := l.Count("1.2.3.4", now); got != 2 {
		t.Errorf("「1.2.3.4」与「 1.2.3.4 」应共用一个窗口，计数应为 2，得到 %d", got)
	}
}

// TestLimiterFailClosedOnBadParams 参数没给对（或干脆没装配）时**拒绝**而不是放行。
//
// 注：这与本仓 apikey 的 `rpm == 0 表示不限速`（internal/apikey/apikey.go）取向
// 相反，理由写在 Allow 的注释里 —— 那里的 0 是用户在界面上显式要的东西，
// 这里的 0 只会来自装配时漏传参数。
func TestLimiterFailClosedOnBadParams(t *testing.T) {
	now := baseTime()
	var notWired *Limiter

	for _, tc := range []struct {
		name string
		l    *Limiter
	}{
		{"窗口为 0", NewLimiter(0, 5)},
		{"窗口为负", NewLimiter(-time.Minute, 5)},
		{"次数为 0", NewLimiter(time.Minute, 0)},
		{"次数为负", NewLimiter(time.Minute, -1)},
		{"没装配（nil）", notWired},
	} {
		if ok, _ := tc.l.Allow("1.2.3.4", now); ok {
			t.Errorf("%s：应当拒绝一切，却放行了", tc.name)
		}
		if got := tc.l.Count("1.2.3.4", now); got != 0 {
			t.Errorf("%s：不该有计数，得到 %d", tc.name, got)
		}
		if n := tc.l.Prune(now); n != 0 {
			t.Errorf("%s：不该清出东西，得到 %d", tc.name, n)
		}
		tc.l.Reset("1.2.3.4") // 不该 panic
	}
}

// TestLimiterConcurrentAllowIsExact 并发下放行次数**正好**等于 max。
//
// ⚠ 本机没有 gcc，`go test -race` 起不来（-race 要 cgo），所以这条用"计数必须
// 精确"顶上来：Allow 的判读与记账在同一把锁里，记账若丢了或重算了，放行次数就
// 不会正好等于 max。它没有竞态检测器严格（抓不到纯读路径上的竞态），但比"因为
// 装不了 race 就什么都不测"强。
func TestLimiterConcurrentAllowIsExact(t *testing.T) {
	const (
		goroutines = 8
		perG       = 50
		maxPerWin  = 100
	)
	l := NewLimiter(time.Hour, maxPerWin)
	now := baseTime()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if ok, _ := l.Allow("same-ip", now); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != maxPerWin {
		t.Errorf("并发下放行次数应正好 %d，得到 %d", maxPerWin, got)
	}
	if got := l.Count("same-ip", now); got != maxPerWin {
		t.Errorf("窗口内计数应正好 %d，得到 %d", maxPerWin, got)
	}
}
