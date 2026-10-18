package admin

import (
	"testing"
)

// ⚠ 本文件是 `jobLabels` / `jobDescs` 两张表的**守门测试**（2026-10-18 新建）。
//
// # 为什么需要它（照 TestUIManifestCapabilityTitles 那条的理由）
//
// 前端在拿不到 `label` 时会**静默退回英文名** —— 页面不报错，只是任务那一列
// 变回 `codearts-refresh` 这种裸英文。⚠ 那种缺陷**不会自我暴露**，
// 只能靠断言抓住。这与 capTitles 漏登记时"静默退回 id"是同一个问题。
//
// 分两条：
//  1. 两张表必须**互相一致**（只加一张 = 出现"有中文名但悬停空"或反之）；
//  2. manifest 里每个已注册任务都必须**两样都有** —— 这条在运行期生效，
//     是真正能抓住"新任务忘了登记"的那一条。

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

// TestUIManifestJobLabels 每个已注册任务都必须在两张表里登记。
//
// ⚠ 与本配置下"有没有注册任务"无关：没有任务时 `m.Jobs` 为空，循环自然不执行
//（那是前置条件不满足，不是断言失败）—— 所以这里**不**用 Skip，
// 免得把"真的漏登记"与"本次没注册任务"混为一种结果。
func TestUIManifestJobLabels(t *testing.T) {
	m := manifestFor(t, New(Config{ListensLoopback: true}))
	for _, j := range m.Jobs {
		if j.Name == "" {
			t.Error("manifest.jobs 里有一条没有 name")
			continue
		}
		if j.Label == "" {
			t.Errorf("任务 %q 没有中文名（前端会显示成裸英文）—— 往 jobLabels 加一行", j.Name)
		}
		if j.Desc == "" {
			t.Errorf("任务 %q 没有说明（悬停会是空的）—— 往 jobDescs 加一行", j.Name)
		}
	}
	// ⚠ 本配置下若真的注册了任务，上面两条才是有效守门；一个都没有时如实说明。
	if len(m.Jobs) == 0 {
		t.Log("本配置下没有注册任务（前置不满足：没有上游装配），本用例本轮只验证到「无数据时不报错」")
	}
}
