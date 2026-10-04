package raccoon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// TestJobExtRegistered raccoon 必须注册后台续期任务。
//
// # 这是用户报障的回归测试（「这个 token 续期逻辑有 bug」）
//
// 报障现场：管理台同时显示「正常」与「已过期」，而实测：
//
//	access_token  寿命约 3 小时，**已过期 532 分钟**
//	refresh_token 寿命 30 天，**还剩 29.5 天**（完全可续期）
//
// 根因：核心的续期只有两条触发路径 ——
//
//	① 出站请求时   handler 判断该刷了才刷
//	② 后台定时任务 JobExt.Jobs()
//
// raccoon `RefreshCredential` 与 `RefreshSkew` 都有，**但没有 Jobs()**，
// 于是只有"有人拿它发对话请求"时才顺手续期。没人用它就静静过期。
//
// ⚠ 与 cline 修之前**逐字同一种病**（internal/cline/jobs.go）。
// 「具备续期能力」与「会去续期」是两件事：前者是方法签名，
// 后者是注册了后台任务 —— 缺后者时没有任何报错。
func TestJobExtRegistered(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: t.TempDir()})

	ext, ok := gateway.ExtOf[gateway.JobExt](p)
	if !ok {
		t.Fatal("raccoon 没被识别为 gateway.JobExt —— 后台续期永远不会注册，" +
			"token 会静静过期（这正是用户的报障）")
	}
	jobs := ext.Jobs()
	if len(jobs) == 0 {
		t.Fatal("Jobs() 返回空 —— 续期任务不会被调度")
	}

	var found bool
	for _, j := range jobs {
		if j.Name == "raccoon-refresh" {
			found = true
			if j.Interval <= 0 {
				t.Errorf("间隔 = %v，必须为正", j.Interval)
			}
			if j.Run == nil {
				t.Error("Run 为 nil —— 任务会 panic")
			}
		}
	}
	if !found {
		t.Errorf("没有 raccoon-refresh 任务；实际有 %d 个", len(jobs))
	}
}

// TestJobsSkippedWithoutAuthDir 没有凭证目录时不注册任务（避免空转）。
func TestJobsSkippedWithoutAuthDir(t *testing.T) {
	p := NewWithConfig(Config{}) // AuthDir 为空
	if jobs := p.Jobs(); len(jobs) != 0 {
		t.Errorf("没有凭证目录时不该注册任务（空转），实际 %d 个", len(jobs))
	}
}

// TestRunRefreshSkipsNonRenewable 不可续期的凭证要跳过而不是白发请求。
//
// # 为什么这条重要
//
// 没有 refresh_token 的凭证永远刷不了。若不做这个判断，每一轮
// （10 分钟）都会对它发一次必然失败的请求 —— 空转且刷日志。
func TestRunRefreshSkipsNonRenewable(t *testing.T) {
	dir := t.TempDir()
	// 造一个没有 refresh_token、且已过期的凭证
	// （JWT 的 exp=1000000000（2001 年）→ 必然已过期）
	a := &Auth{
		AccessToken: "eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjEwMDAwMDAwMDAsImlhdCI6OTAwMDAwMDAwfQ.x",
		UserID:      "111",
		FilePath:    filepath.Join(dir, "raccoon-111.json"),
	}
	if err := saveAuthFile(a); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	if a.Renewable() {
		t.Fatal("这个凭证应当不可续期（没有 refresh_token）")
	}

	p := NewWithConfig(Config{AuthDir: dir})
	// runRefresh 不该因为"刷不了"而报错，只是跳过
	if err := p.runRefresh(context.Background()); err != nil {
		t.Errorf("runRefresh 对不可续期凭证不该报错: %v", err)
	}
}

// TestRunRefreshRespectsContextCancel 任务必须响应 ctx 取消。
//
// 后台任务在停机时会被取消；不检查 ctx 会让停机卡住。
func TestRunRefreshRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消

	p := NewWithConfig(Config{AuthDir: t.TempDir()})
	if err := p.runRefresh(ctx); err == nil {
		t.Error("ctx 已取消时应当返回错误（否则优雅停机无从生效）")
	}
}

// TestIsRefreshExpiredDistinguishesTerminal 终态与可重试必须分开。
//
// # 为什么必须区分（用户动作完全不同）
//
//	终态   refresh_token 作废 → 只能重新登录，重试再多次也没用
//	可重试 网络抖动/5xx      → 下一轮自动再试
//
// 判据必须用 errors.Is（ErrRefreshExpired 会被 %w 包装），
// 字符串比较在包装后就失效 —— 那会让"终态"被当成"可重试"，
// token 永远停在过期状态（正是这次报障的形态）。
func TestIsRefreshExpiredDistinguishesTerminal(t *testing.T) {
	t.Run("裸错误", func(t *testing.T) {
		if !isRefreshExpired(ErrRefreshExpired) {
			t.Error("ErrRefreshExpired 应当被识别为终态")
		}
	})
	t.Run("%w 包装后仍要识别", func(t *testing.T) {
		wrapped := wrapExpired()
		if !isRefreshExpired(wrapped) {
			t.Error("被 %w 包装的 ErrRefreshExpired 应当仍被识别 —— " +
				"否则用户要白跑一趟重新登录")
		}
	})
	t.Run("普通错误不算终态", func(t *testing.T) {
		if isRefreshExpired(errors.New("网络抖动")) {
			t.Error("普通错误不该被当成终态")
		}
	})
	t.Run("nil 不算终态", func(t *testing.T) {
		if isRefreshExpired(nil) {
			t.Error("nil 不该被当成终态")
		}
	})
}

// wrapExpired 模拟生产里的包装形态（fmt.Errorf("...: %w", ErrRefreshExpired)）。
func wrapExpired() error {
	return errors.Join(ErrRefreshExpired)
}

// TestRefreshCandidatesPrefersLiveObject 必须优先返回**池里的活对象**。
//
// # 这是本仓栽过多次的坑
//
// 池子持有的是启动时放进 secrets[uid] 的那个 *Auth，而 LoadDir 每次都
// 从磁盘**新建**对象。刷后者只会更新磁盘，池里那份纹丝不动 ——
// 界面读的是池，于是"落盘已续期、界面仍显示已过期"。
//
// 所以 creds（装配层注入的活 secret 访问器）存在时必须以它为准。
func TestRefreshCandidatesPrefersLiveObject(t *testing.T) {
	dir := t.TempDir()
	disk := &Auth{
		AccessToken: "disk-token",
		UserID:      "777",
		FilePath:    filepath.Join(dir, "raccoon-777.json"),
	}
	if err := saveAuthFile(disk); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}

	live := &Auth{AccessToken: "live-token", UserID: "777"}
	p := NewWithConfig(Config{AuthDir: dir})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		if uid != "777" {
			return gateway.Credential{}, false
		}
		return gateway.Credential{Provider: providerID, UID: uid, Secret: live}, true
	})

	got, err := p.refreshCandidates()
	if err != nil {
		t.Fatalf("refreshCandidates 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应当返回 1 个候选，实际 %d", len(got))
	}
	if got[0].AccessToken != "live-token" {
		t.Errorf("返回的是磁盘副本（token=%q），应当是池里的活对象（live-token）—— "+
			"刷错对象会导致『落盘已续期、界面仍显示已过期』", got[0].AccessToken)
	}
}

// TestRefreshCandidatesFallsBackToDisk 未接线时退回磁盘（至少能刷上）。
func TestRefreshCandidatesFallsBackToDisk(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{
		AccessToken: "disk-only",
		UserID:      "888",
		FilePath:    filepath.Join(dir, "raccoon-888.json"),
	}
	if err := saveAuthFile(a); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	p := NewWithConfig(Config{AuthDir: dir}) // creds 未接线

	got, err := p.refreshCandidates()
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if len(got) != 1 || got[0].AccessToken != "disk-only" {
		t.Errorf("未接线时应当退回磁盘凭证，实际 %+v", got)
	}
}

// TestRefreshIntervalSane 间隔既要能及时刷上、也要留重试余量。
//
// raccoon 的 access_token 寿命实测约 3 小时，RefreshSkew 判据是
// "已用寿命 ≥50%" → 剩约 90 分钟时该刷。
func TestRefreshIntervalSane(t *testing.T) {
	if iv := NewWithConfig(Config{}).refreshInterval(); iv != 0 {
		t.Errorf("无 authDir 时 interval = %v，want 0", iv)
	}
	iv := NewWithConfig(Config{AuthDir: t.TempDir()}).refreshInterval()
	if iv <= 0 {
		t.Fatal("有 authDir 时 interval 必须为正")
	}
	// 太密：无谓请求；太疏：吃掉重试余量（token 只活 3 小时）
	if iv > 30*time.Minute {
		t.Errorf("interval = %v 偏长：token 寿命约 3 小时，太疏会吃掉重试余量", iv)
	}
}
