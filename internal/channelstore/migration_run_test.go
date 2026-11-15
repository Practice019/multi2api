// migration_run_test.go U-09 执行机器的判据。
//
// # 三条硬规则各有判据
//
//	① 计划不 Ready ⇒ 一步都不走
//	② 写失败 ⇒ 立即停（继续下去会让"逆序回滚"失去意义）
//	③ 幂等 ⇒ 已提交且 checksum 相同的步骤是 no-op
//
// 写入器是**假的**（`fakeWriter`），所以整条链路能在没有真实库的情况下
// 被完整钉住 —— 尤其是"写失败之后日志是什么状态"。
package channelstore

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeWriter 记录调用、可注入失败。
type fakeWriter struct {
	calls   []string
	writes  []TokenPolicyWrite
	failOn  map[string]error // name → 错误
	panicOn string
}

func (f *fakeWriter) WriteTokenPolicy(name string, w TokenPolicyWrite) error {
	if f.panicOn == name {
		panic("boom")
	}
	f.calls = append(f.calls, name)
	f.writes = append(f.writes, w)
	if f.failOn != nil {
		if err, ok := f.failOn[name]; ok {
			return err
		}
	}
	return nil
}

// runPlan 造一份两项、可执行的计划。
func runPlan() MigrationPlan {
	return MigrationPlan{
		Items: []MigrationPlanItem{
			{Name: "app", PinnedWhitelist: []string{"public"}},
			{Name: "web", WhitelistDisabled: true},
		},
		Total: 2,
		Ready: true,
	}
}

// ★ ① 计划不 Ready ⇒ 一步都不走。
func TestRunnerRefusesUnreadyPlan(t *testing.T) {
	plan := runPlan()
	plan.Ready = false
	plan.Blocked = 1

	w := &fakeWriter{}
	r, err := NewMigrationRunner(plan, nil, w)
	if err != nil {
		t.Fatal(err)
	}
	rep := r.RunAll()

	if len(rep.Executed) != 0 {
		t.Fatalf("★ 计划有阻塞项却执行了 %v —— 那是带病迁移", rep.Executed)
	}
	if rep.Blocked == "" {
		t.Fatal("必须说明为什么没走")
	}
	if len(w.calls) != 0 {
		t.Fatalf("★ 写入器被调用了 %d 次 —— 一步都不该走", len(w.calls))
	}
}

// ★ ② 写失败 ⇒ 立即停，且日志记失败。
func TestRunnerStopsOnWriteFailure(t *testing.T) {
	plan := runPlan()
	w := &fakeWriter{failOn: map[string]error{"web": errors.New("boom")}}
	r, err := NewMigrationRunner(plan, nil, w)
	if err != nil {
		t.Fatal(err)
	}
	rep := r.RunAll()

	if !reflect.DeepEqual(rep.Executed, []string{"token:app"}) {
		t.Fatalf("第一条应成功，实际 %v", rep.Executed)
	}
	if rep.Failed != "token:web" || !rep.Stopped {
		t.Fatalf("★ 第二条失败就该停下，实际 failed=%q stopped=%v", rep.Failed, rep.Stopped)
	}
	// 日志里 web 必须是 failed、app 是 committed。
	for _, s := range r.Journal().Steps {
		switch s.ID {
		case "token:app":
			if s.Status != StepCommitted {
				t.Fatalf("app 应为 committed，实际 %q", s.Status)
			}
		case "token:web":
			if s.Status != StepFailed {
				t.Fatalf("★ web 应为 failed，实际 %q", s.Status)
			}
			if !strings.Contains(s.Error, "boom") {
				t.Fatalf("失败原因应记进日志，实际 %q", s.Error)
			}
		}
	}
}

// ★ ③ 幂等：重跑不重复写。
func TestRunnerIsIdempotent(t *testing.T) {
	plan := runPlan()
	w := &fakeWriter{}
	r, err := NewMigrationRunner(plan, nil, w)
	if err != nil {
		t.Fatal(err)
	}
	first := r.RunAll()
	if len(first.Executed) != 2 {
		t.Fatalf("首轮应执行 2 条，实际 %v", first.Executed)
	}

	// 重跑三次：一次写入都不该再发生。
	for i := 0; i < 3; i++ {
		rep := r.RunAll()
		if len(rep.Executed) != 0 {
			t.Fatalf("★ 第 %d 次重跑又执行了 %v —— 不幂等，会重复写", i+2, rep.Executed)
		}
		if len(rep.Skipped) != 2 {
			t.Fatalf("重跑应全部 skipped，实际 %v", rep.Skipped)
		}
	}
	if len(w.calls) != 2 {
		t.Fatalf("★ 写入器总共应被调用 2 次，实际 %d 次（%v）", len(w.calls), w.calls)
	}
}

// 失败之后重跑：只补做失败的那一步（不重做已成功的）。
func TestRunnerResumesAfterFailure(t *testing.T) {
	plan := runPlan()
	w := &fakeWriter{failOn: map[string]error{"web": errors.New("boom")}}
	r, err := NewMigrationRunner(plan, nil, w)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.RunAll()

	// 修好之后重跑。
	w.failOn = nil
	w.calls = nil
	rep := r.RunAll()

	if !reflect.DeepEqual(rep.Executed, []string{"token:web"}) {
		t.Fatalf("★ 重跑应只执行失败的那一步，实际 %v", rep.Executed)
	}
	if !reflect.DeepEqual(w.calls, []string{"web"}) {
		t.Fatalf("★ 已成功的 app 不该被重写，实际写入调用 %v", w.calls)
	}
}

// 计划变了 ⇒ 日志拒绝续跑（不能把"写的是旧计划"当成"新计划已完成"）。
func TestRunnerRejectsPlanDrift(t *testing.T) {
	plan := runPlan()
	w := &fakeWriter{}
	r, err := NewMigrationRunner(plan, nil, w)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.RunAll()

	// 改了第一项的白名单 ⇒ 计划 checksum 变。
	plan.Items[0].PinnedWhitelist = []string{"public", "other"}
	w2 := &fakeWriter{}
	r2, err := NewMigrationRunner(plan, r.Journal(), w2) // 拿旧日志
	if err != nil {
		t.Fatal(err)
	}
	rep := r2.RunAll()

	if rep.Failed == "" || !rep.Stopped {
		t.Fatalf("★ 计划变了却继续续跑旧日志 —— 实际 %+v", rep)
	}
	if len(w2.calls) != 0 {
		t.Fatalf("★ 计划漂移时一次写入都不该发生，实际 %v", w2.calls)
	}
}

// 同名的两个令牌 ⇒ 步骤 ID 冲突 ⇒ 建执行器时就拒绝（不许指错对象）。
func TestRunnerRejectsDuplicateStepIDs(t *testing.T) {
	plan := MigrationPlan{
		Items: []MigrationPlanItem{
			{Name: "dup", PinnedWhitelist: []string{"a"}},
			{Name: "dup", PinnedWhitelist: []string{"b"}},
		},
		Total: 2, Ready: true,
	}
	_, err := NewMigrationRunner(plan, nil, &fakeWriter{})
	if err == nil {
		t.Fatal("★ 同名令牌必须拒绝 —— 否则「某个令牌已提交」会指错对象")
	}
	if !strings.Contains(err.Error(), "重名") {
		t.Fatalf("错误信息应说清是重名，实际 %q", err.Error())
	}
}

// 计划项 → 写入的翻译（两条规则）。
func TestPlanItemWriteTranslation(t *testing.T) {
	// 钉住白名单 ⇒ 启用 + 那个集合。
	a := MigrationPlanItem{Name: "a", PinnedWhitelist: []string{"x", "y"}}
	wa := a.Write()
	if !wa.ModelLimitsEnabled || !reflect.DeepEqual(wa.ModelLimits, []string{"x", "y"}) {
		t.Fatalf("钉住翻译错了：%+v", wa)
	}
	// 关闭白名单 ⇒ 不启用 + 空。
	b := MigrationPlanItem{Name: "b", WhitelistDisabled: true}
	wb := b.Write()
	if wb.ModelLimitsEnabled || len(wb.ModelLimits) != 0 {
		t.Fatalf("关闭翻译错了：%+v", wb)
	}
	// 阻塞项不参与 PlanChecksum（改它的说明不该让整份计划失效）。
	p1 := MigrationPlan{Items: []MigrationPlanItem{{Name: "x", PinnedWhitelist: []string{"a"}}}}
	p2 := MigrationPlan{Items: []MigrationPlanItem{
		{Name: "x", PinnedWhitelist: []string{"a"}},
		{Name: "z", Blocked: "whatever"},
	}}
	if PlanChecksum(p1) != PlanChecksum(p2) {
		t.Fatal("阻塞项不该影响计划 checksum（它不参与执行）")
	}
}
