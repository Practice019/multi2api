// refresh_due_test.go —— ★ T-292：后台续期任务的**到期判据**（`refreshDue`）与
// 挑号判据（`accountsPastSkew`）的行为钉子。
//
// # 为什么必须有这些断言
//
// 用户报的现象是"3 个号都剩 46 分钟了却没续期"。查下来有两条独立原因：
//
//	① 判据是"寿命过半"，而扫描粒度是 refreshInterval（默认 30m）⇒ 过半点后最多等 30 分钟；
//	② 期间的盘点只能靠 Interval（老代码），所以"刚过半点"这件事要等下一轮才被发现。
//
// 修法是给任务加 `Due`（核心每 30s 问一次），并让 `Due` 与 `runRefresh` **共用**
// `accountsPastSkew` 这一个判据函数。这里钉的正是这条共用的边界：
// 过期/未过半/无 refresh_token/退避中/文件不存在 —— 五种输入下"该不该跑、该挑谁"。
package codearts

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dueTestProvider 造一个只装了"凭证访问器 + 续期间隔"的 Provider（不碰网络）。
func dueTestProvider(t *testing.T, accounts []*Auth, interval time.Duration) *Provider {
	t.Helper()
	p := NewWithConfig(Config{})
	p.SetRefreshInterval(interval)
	p.SetAccounts(func() []*Auth { return accounts })
	return p
}

// 未过半：不该跑（Due=false），也不该挑到任何号。
func TestRefreshDueSkipsFreshAccounts(t *testing.T) {
	now := time.Now()
	// 刚签发：寿命 2h，剩 ~2h > 窗口（寿命一半 ≈ 1h）⇒ 还没过半
	a := &Auth{UID: "u-fresh", AccessKey: "AK", RefreshToken: "rt", ExpiresAt: now.Add(stsLifetime).Unix()}
	p := dueTestProvider(t, []*Auth{a}, 30*time.Minute)
	p.lastRefreshScanNS.Store(now.UnixNano()) // 刚扫过 ⇒ 只剩"过半"这一条判据

	if p.refreshDue(now) {
		t.Error("刚签发的凭证（剩满寿命）不该触发续期 —— 会把 refresh_token 白烧掉")
	}
	need, err := p.accountsPastSkew(now)
	if err != nil {
		t.Fatalf("accountsPastSkew 出错: %v", err)
	}
	if len(need) != 0 {
		t.Errorf("挑号数 = %d，want 0（还没过半）", len(need))
	}
}

// ★ 核心回归（用户报的那条）：剩 46 分钟（已过半）⇒ Due 立刻为真，且挑得到它。
func TestRefreshDueFiresOncePastHalfLife(t *testing.T) {
	now := time.Now()
	a := &Auth{UID: "u-half", AccessKey: "AK", RefreshToken: "rt", ExpiresAt: now.Add(46 * time.Minute).Unix()}
	p := dueTestProvider(t, []*Auth{a}, 30*time.Minute)
	p.lastRefreshScanNS.Store(now.UnixNano()) // 刚扫过：若只靠 Interval 就要再等 30 分钟

	if !p.refreshDue(now) {
		t.Error("★ 已过半寿命（剩 46m ≤ 寿命×0.5）时 refreshDue 必须为真 —— 否则就是用户报的'一直不续'")
	}
	need, _ := p.accountsPastSkew(now)
	if len(need) != 1 || need[0].UID != "u-half" {
		t.Errorf("应挑出 1 个（u-half），得到 %d 个", len(need))
	}
}

// 兜底节奏：从未扫过 ⇒ 立刻跑（保持"启动即首扫"）；距上次扫描满一个 interval ⇒ 也要跑。
func TestRefreshDueFallbackInterval(t *testing.T) {
	now := time.Now()
	fresh := &Auth{UID: "u1", AccessKey: "AK", RefreshToken: "rt", ExpiresAt: now.Add(stsLifetime).Unix()}

	// ① 从未扫过
	p := dueTestProvider(t, []*Auth{fresh}, 30*time.Minute)
	if !p.refreshDue(now) {
		t.Error("从未扫过时应立刻跑（启动即首扫）")
	}

	// ② 刚扫过 + 没有号过半 ⇒ 不跑
	p.lastRefreshScanNS.Store(now.UnixNano())
	if p.refreshDue(now) {
		t.Error("刚扫过且无号过半时不该再跑（否则每 30 秒空转）")
	}

	// ③ 距上次扫描满 30 分钟 ⇒ 跑（旧节奏语义保留）
	p.lastRefreshScanNS.Store(now.Add(-31 * time.Minute).UnixNano())
	if !p.refreshDue(now) {
		t.Error("距上次扫描超过 interval 时应强制扫一轮（否则换了快过期的凭证也不会被发现）")
	}
}

// 无 refresh_token / 退避中：既不该跑，也不该挑中。
func TestRefreshDueSkipsUnrefreshable(t *testing.T) {
	now := time.Now()
	exp := now.Add(1 * time.Minute).Unix() // 都"过半"了

	noToken := &Auth{UID: "u-notoken", AccessKey: "AK", RefreshToken: "", ExpiresAt: exp}
	held := &Auth{UID: "u-held", AccessKey: "AK", RefreshToken: "rt", ExpiresAt: exp}

	p := dueTestProvider(t, []*Auth{noToken, held}, 30*time.Minute)
	p.lastRefreshScanNS.Store(now.UnixNano())
	// 让 held 进入退避（非永久错误 ⇒ 短退避）
	p.holdRefresh(held, os.ErrDeadlineExceeded)

	if p.refreshDue(now) {
		t.Error("只剩'续不了'的号时 refreshDue 不该为真（挑不到任何号 ⇒ 跑起来就是空转）")
	}
	need, _ := p.accountsPastSkew(now)
	if len(need) != 0 {
		t.Errorf("挑号数 = %d，want 0（一个没 token、一个在退避）", len(need))
	}
}

// 退避解除后立刻恢复可挑（凭证文件被重新登录写过 ⇒ onRefreshHold 提前解除）。
func TestAccountsPastSkewResumesAfterFileUpdate(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "codearts-u.json")
	a := &Auth{UID: "u-file", AccessKey: "AK", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Minute).Unix(), FilePath: file}
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p := dueTestProvider(t, []*Auth{a}, 30*time.Minute)
	p.lastRefreshScanNS.Store(now.UnixNano())

	p.holdRefresh(a, os.ErrDeadlineExceeded)
	if need, _ := p.accountsPastSkew(now); len(need) != 0 {
		t.Fatalf("退避中不该被挑中，得到 %d 个", len(need))
	}

	// 模拟"用户重新登录写了新凭证"：文件修改时间晚于进入退避的时刻
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}
	need, _ := p.accountsPastSkew(time.Now())
	if len(need) != 1 {
		t.Errorf("凭证文件更新后应恢复可挑（得到 %d 个，want 1）", len(need))
	}
}

// 未开启后台续期（interval<=0）⇒ Due 恒假（Jobs() 本就不会注册它）。
func TestRefreshDueDisabledWhenIntervalZero(t *testing.T) {
	now := time.Now()
	a := &Auth{UID: "u", AccessKey: "AK", RefreshToken: "rt", ExpiresAt: now.Add(time.Minute).Unix()}
	p := dueTestProvider(t, []*Auth{a}, 0)
	if p.refreshDue(now) {
		t.Error("refreshInterval<=0（关闭后台续期）时 Due 必须为假")
	}
}
