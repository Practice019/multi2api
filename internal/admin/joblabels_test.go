package admin

import (
	"testing"
)

// ⚠ 本文件守的是 `jobLabels` / `jobDescs` 两张表的**互相一致**。
//
// # 为什么这里不再守"每个已注册任务都登记了"
//
// 原来这里有一条 TestUIManifestJobLabels，遍历 admin.New(...) 在**测试夹具**下
// 取到的 manifest.Jobs，断言每条都有 Label/Desc。它看着对，实际是一条**永远为真**
// 的断言 —— 本包的夹具里一个上游都没装配，m.Jobs 恒为空，循环从不执行。
// 它自己的日志把这件事写在脸上：
//
//	joblabels_test.go: 本配置下没有注册任务（前置不满足：没有上游装配），
//	                    本用例本轮只验证到「无数据时不报错」
//
// 2026-10-26 的真实后果：`monkeycode-checkin` 在两张表里**一起漏登记**，
// 任务调度表那一行只有裸英文 key（用户报障），而这条守卫全程绿着。
//
// ⇒ 那条断言已**搬到能看见全部任务名的地方**：`cmd/server/job_labels_test.go` 的
//   TestEveryJobNameLiteralHasLabelAndDesc（扫生产代码里的任务名字面量，
//   与"有没有被配置、有没有在夹具里装配"无关）。
//   它经过变异验证：注释掉 jobLabels 里的一行，它会红并指出定义处。
//
// 本文件只留下夹具里**真能守住**的那条：两张表互相一致
// （只加一张 = "有中文名但悬停空" 或反之）。⚠ 注意它**查不出"两张一起漏"**——
// 那正是上面那条搬走的守卫的职责。

// TestJobLabelTablesConsistent 两张表键集合一致，且值都非空。
func TestJobLabelTablesConsistent(t *testing.T) {
	for k, v := range jobLabels {
		if k == "" {
			t.Error("jobLabels 里有空键")
		}
		if v == "" {
			t.Errorf("jobLabels[%q] 是空串（前端会显示成「无内容 (name)」）", k)
		}
		if _, ok := jobDescs[k]; !ok {
			t.Errorf("jobLabels 有 %q 但 jobDescs 没有 ⇒ 那一行悬停会是空的", k)
		}
	}
	for k, v := range jobDescs {
		if v == "" {
			t.Errorf("jobDescs[%q] 是空串（悬停会弹一个空框）", k)
		}
		if _, ok := jobLabels[k]; !ok {
			t.Errorf("jobDescs 有 %q 但 jobLabels 没有 ⇒ 有说明却显示裸英文名", k)
		}
	}
}
