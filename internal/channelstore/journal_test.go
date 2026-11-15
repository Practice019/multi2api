// journal_test.go U-10 分步提交日志的判据（四条不变量）。
//
//	不变式 1  幂等重跑   已 committed 且 checksum 相同 ⇒ no-op
//	不变式 2  校验和     同一步 checksum 变了 ⇒ 拒绝
//	不变式 3  计划钉住   整份计划 checksum 变了 ⇒ 拒绝续跑
//	不变式 4  逆序回滚   只允许回滚最后一步
//
// # 为什么不变式 2、3 是这套东西的灵魂
//
// 「已 committed」只是个标记。没有 checksum 的话，改完计划再跑一遍，
// 日志会把"那次写的是旧内容"当成"这次要写的新内容"已经完成 ——
// 于是永远不写，而报告说全绿。两种漂移（单步 / 整份）都要拦。
package channelstore

import (
	"strings"
	"testing"
)

const testPlan = "plan-v1"

func newTestJournal() *MigrationJournal {
	return NewJournal(testPlan, [][2]string{
		{"token:app", Checksum("app|public")},
		{"token:web", Checksum("web|public2")},
		{"group:route", Checksum("route|ch-a")},
	})
}

// ★ 不变式 1：幂等重跑。
func TestJournalIdempotentRerun(t *testing.T) {
	j := newTestJournal()
	cs := Checksum("app|public")

	act, err := j.Apply(testPlan, "token:app", cs)
	if err != nil || !act.Execute {
		t.Fatalf("第一次应执行：%+v %v", act, err)
	}
	if err := j.Commit("token:app"); err != nil {
		t.Fatal(err)
	}

	// 重跑：明确告诉我们"不用执行"。
	for i := 0; i < 3; i++ {
		act, err = j.Apply(testPlan, "token:app", cs)
		if err != nil {
			t.Fatalf("第 %d 次重跑不该报错：%v", i+2, err)
		}
		if act.Execute {
			t.Fatalf("第 %d 次重跑仍然要执行 —— 不幂等，会重复写", i+2)
		}
		if !strings.Contains(act.Reason, "已提交") {
			t.Fatalf("应说明「已提交，跳过」，实际 %q", act.Reason)
		}
	}
	// 步骤数不变（没有因为重跑而新增条目）。
	if len(j.Steps) != 3 {
		t.Fatalf("步骤数不应变化，实际 %d", len(j.Steps))
	}
	committed, _, _ := j.Summary()
	if committed != 1 {
		t.Fatalf("committed 应为 1，实际 %d", committed)
	}
}

// ★ 不变式 2a：已提交的步骤内容变了 ⇒ 拒绝。
func TestJournalRejectsChecksumDriftAfterCommit(t *testing.T) {
	j := newTestJournal()
	if err := j.Commit("token:app"); err != nil {
		t.Fatal(err)
	}
	_, err := j.Apply(testPlan, "token:app", Checksum("app|DIFFERENT"))
	if err == nil {
		t.Fatal("★ 已提交的步骤内容变了必须拒绝 —— 否则日志会把旧内容当成新内容已完成")
	}
	if !strings.Contains(err.Error(), "内容变了") {
		t.Fatalf("错误信息应说清成因，实际 %q", err.Error())
	}
}

// ★ 不变式 2b：未提交的步骤内容变了 ⇒ 同样拒绝（不能悄悄换）。
func TestJournalRejectsChecksumDriftBeforeCommit(t *testing.T) {
	j := newTestJournal()
	_, err := j.Apply(testPlan, "token:app", Checksum("app|DIFFERENT"))
	if err == nil {
		t.Fatal("★ 未提交的步骤内容变了也必须拒绝")
	}
}

// ★ 不变式 3：整份计划变了 ⇒ 旧日志不能续跑。
func TestJournalRejectsPlanDrift(t *testing.T) {
	j := newTestJournal()
	if err := j.Commit("token:app"); err != nil {
		t.Fatal(err)
	}
	_, err := j.Apply("plan-v2", "token:web", Checksum("web|public2"))
	if err == nil {
		t.Fatal("★ 整份计划变了必须拒绝续跑")
	}
	if !strings.Contains(err.Error(), "整份计划") {
		t.Fatalf("错误信息应指出是整份计划变了，实际 %q", err.Error())
	}
}

// ★ 中断恢复：从第一个未提交的步骤继续。
func TestJournalResumesFromFirstUncommitted(t *testing.T) {
	j := newTestJournal()

	if id, more := j.Resume(); !more || id != "token:app" {
		t.Fatalf("初始应从第一步开始，实际 %q more=%v", id, more)
	}
	if err := j.Commit("token:app"); err != nil {
		t.Fatal(err)
	}
	if id, more := j.Resume(); !more || id != "token:web" {
		t.Fatalf("提交第一步后应续到第二步，实际 %q", id)
	}
	// 中间一步失败不影响"从哪继续"。
	if err := j.Fail("token:web", "upstream 502"); err != nil {
		t.Fatal(err)
	}
	if id, _ := j.Resume(); id != "token:web" {
		t.Fatalf("失败的步骤应仍然是续跑起点，实际 %q", id)
	}
	// 全部提交 ⇒ Done。
	_ = j.Commit("token:web")
	_ = j.Commit("group:route")
	if !j.Done() {
		t.Fatal("全部提交后应 Done")
	}
	if id, more := j.Resume(); more || id != "" {
		t.Fatalf("Done 后不该再给出步骤，实际 %q", id)
	}
}

// ★ 不变式 4：只允许逆序回滚。
func TestJournalRollsBackInReverseOrder(t *testing.T) {
	j := newTestJournal()
	_ = j.Commit("token:app")

	// 还没提交第二步 ⇒ 没有任何"已提交"可回滚第二次。
	if err := j.Rollback("token:app"); err != nil {
		t.Fatalf("回滚唯一的已提交步骤应成功：%v", err)
	}
	committed, _, _ := j.Summary()
	if committed != 0 {
		t.Fatalf("回滚后 committed 应为 0，实际 %d", committed)
	}

	// 再提交两步，然后试图回滚**前**一步 ⇒ 必须拒绝。
	_ = j.Commit("token:app")
	_ = j.Commit("token:web")
	rollErr := j.Rollback("token:app")
	if rollErr == nil {
		t.Fatal("★ 跳过最后一步回滚前面的必须拒绝（日志会与实际写入脱节）")
	}
	if !strings.Contains(rollErr.Error(), "逆序") {
		t.Fatalf("错误信息应指出只能逆序，实际 %q", rollErr.Error())
	}
	// 回滚最后一步 ⇒ 成功。
	if err := j.Rollback("token:web"); err != nil {
		t.Fatalf("回滚最后一步应成功：%v", err)
	}
}

// 重试耗尽要停下来等人（无限重试会把配置错误变成写库风暴）。
func TestJournalRetryExhausted(t *testing.T) {
	j := newTestJournal()
	for i := 0; i < 3; i++ {
		_ = j.Fail("token:app", "boom")
	}
	if !j.RetryExhausted(3) {
		t.Fatal("★ 同一一步失败 3 次后应判为「重试耗尽」，停下来等人")
	}
	// 已提交的步骤不计入。
	if err := j.Commit("token:app"); err != nil {
		t.Fatal(err)
	}
	if j.RetryExhausted(3) {
		t.Fatal("已提交的步骤不该计入重试耗尽")
	}
}

// 报告只含 id 与状态，不含任何凭据。
func TestJournalSummaryCarriesNoSecret(t *testing.T) {
	j := newTestJournal()
	_ = j.Commit("token:app")
	_ = j.Fail("token:web", "boom")
	committed, pending, failed := j.Summary()
	if committed != 1 || pending != 1 || failed != 1 {
		t.Fatalf("计数不对：%d/%d/%d", committed, pending, failed)
	}
	for _, s := range j.Steps {
		if strings.Contains(s.ID, "sk-") {
			t.Fatalf("步骤 ID 里出现了密钥形态：%q", s.ID)
		}
	}
}

// 步骤顺序必须稳定（两次建日志排出不同顺序会让日志与写入错位）。
func TestJournalStepIDsAreStable(t *testing.T) {
	ids := []string{"token:web", "group:route", "token:app"}
	a := SortedStepIDs(ids)
	b := SortedStepIDs([]string{"token:app", "token:web", "group:route"})
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("两次排序结果不同：%v vs %v", a, b)
		}
	}
}
