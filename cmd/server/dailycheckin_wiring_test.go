package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rootForCheckin 从测试的工作目录往上找到仓库根（有 go.mod 的那层）。
//
// ⚠ 名字带 Checkin 前缀：本包已有别的 repoRoot（job_labels_test.go）——
// 重名会让整个测试包编译失败。
func rootForCheckin(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("从 %s 往上找不到 go.mod", dir)
		}
		dir = parent
	}
}

// readCheckinFile 读仓库内相对路径的文件；读不到**直接失败**（空匹配即失败）。
func readCheckinFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rootForCheckin(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读 %s 失败（它必须存在 —— 空匹配不算通过）: %v", rel, err)
	}
	return string(b)
}

// checkinFuncBody 截出 `Due: func(…)` 的函数体（到下一个字段 `Run: func(` 为止）。
//
// ⚠ 不能用「\n\t},」当结尾：字段在**结构体字面量**里是两格缩进（`\n\t\tRun: func(`），
// 而 Due 体内部还有它自己的 `},` ⇒ 用一格缩进当锚点会**吃过头**，
// 把 Run 体一起圈进来（那里面当然有 CheckinAll 调用 ⇒ 断言 b2 假红）。
// ⚠ 本函数最初就是这么错的，实测假红一次 —— 所以注释写清楚。
func checkinFuncBody(src, sig string) string {
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n\t\tRun: func("); j > 0 {
		return rest[:j]
	}
	if j := strings.Index(rest, "\n\t},"); j > 0 {
		return rest[:j]
	}
	return rest
}

// TestSharedDailyCheckinJobIsWired 共享签到 Job 的接线（T-427）。
//
// # 守的是什么
//
// 用户报障：「你说这个有定时自动签到，但是它自动签到好像没有用，
// 因为这两个账号都是我自己手动签的。」
//
// 根因：8 个上游都实现了 DailyCheckinExt，但**只有 2 个注册了签到 Job**
// （trae / workbuddy）⇒ 另外 6 个的自动签到**静默失效**。
// 设计见 `docs/design/design-notes.md`（Q1/Q2/Q6 定的就是"一处注册 + 运行时发现"）。
//
// # 为什么这几条是"源码级"断言
//
// 共享 Job 落地前，行为测试连编译都过不去（符号不存在）—— 那不是有效红
// （它只证明"符号不存在"，不证明"行为缺失"）。
// 所以先用源码级断言把"接线是否存在"钉住，实现完再补行为测试（fake provider 版）。
//
// ⚠ 每条都**空匹配即失败**：文件不存在、或名字被改掉 ⇒ 直接红，不会静默变绿。
func TestSharedDailyCheckinJobIsWired(t *testing.T) {
	// a) 共享 Job 的构造函数存在
	sched := readCheckinFile(t, "internal/scheduler/dailycheckin.go")
	if !strings.Contains(sched, "func NewDailyCheckinJob(") {
		t.Error("internal/scheduler/dailycheckin.go 里没有 NewDailyCheckinJob —— " +
			"共享签到 Job 没落地，6 个上游的自动签到仍然哑着")
	}
	// b) 间隔是 1 小时（用户明确否掉了 30 分钟）
	if !strings.Contains(sched, "Interval: time.Hour") {
		t.Error("共享签到 Job 的 Interval 不是 time.Hour —— " +
			"用户原话「30分钟扫描太频繁了，要不就是6个小时扫描一次，或者一个小时扫描一次吧」")
	}
	// b2) Due 必须只读内存（L1 梯级）—— 源码里不许出现发请求的痕迹
	for _, banned := range []string{"CheckinAll", "http.Get", "http.Post", "Do("} {
		if strings.Contains(checkinFuncBody(sched, "Due: func("), banned) {
			t.Errorf("Due 里出现了 %q —— Due 每个 tick（30 秒）都会被问，"+
				"在里面发请求就是 23040 次/天（notes §1 把这条列为一号热点）", banned)
		}
	}
	// c) main.go 真的注册了它（注册了但没进 tick 循环 = 等于没注册）
	mainSrc := readCheckinFile(t, "cmd/server/main.go")
	if !strings.Contains(mainSrc, "NewDailyCheckinJob(") {
		t.Error("cmd/server/main.go 里没有注册共享签到 Job —— " +
			"写在 scheduler 里但没人调用它，等于不存在")
	}
	// d) codar 的单品必须已删除（否则每小时对 codar 调两次 CheckinAll ⇒ 上游有风控）
	codarSrc := readCheckinFile(t, "internal/codar/p1.go")
	if strings.Contains(codarSrc, `"codar-daily-checkin"`) {
		t.Error("codar 的 Jobs() 里仍有 codar-daily-checkin —— " +
			"共享 Job 已覆盖它 ⇒ 留着会让 codar 每小时被 CheckinAll 两次，" +
			"而上游面板明确写着「上游已有风控记录，请低频使用」")
	}
	// e) minimax 的编译期断言必须在**非测试**文件里（测试文件不参与 go build ./...）
	mini := readCheckinFile(t, "internal/minimax/dailycheckin_assert.go")
	if !strings.Contains(mini, "var _ gateway.DailyCheckinExt = (*Provider)(nil)") {
		t.Error("internal/minimax/dailycheckin_assert.go 里没有编译期断言 —— " +
			"minimax 原来只在 interfaces_compile_test.go 里有 ⇒ 不参与 go build，" +
			"签名写歪了不会当场失败，只会安静地漏掉这个上游")
	}
}
