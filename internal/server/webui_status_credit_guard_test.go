package server

import (
	"strings"
	"testing"
)

// TestStatusPageHasNoUpstreamCredit 状态页**不许再出现**上游归属文案（T-455）。
//
// # 背景（留痕，别当它是"忘了"）
//
// 页脚原来有一行 **AGPL-3.0 §7(b)** 要求的归属文案 + 指向原项目的可见链接。
// 用户 2026-11-09 明确要求删除（原话：「为什么有外部链接啊」⇒ 选择「连文字一起删掉」）。
//
// ⚠ **这是用户的产品决定，不是我们的遗漏**。本测试的作用是：
//
//	· 防止将来有人"看到别的页面都有、就这个没有"而顺手补回来；
//	· 如果有人真的要恢复它，就会先撞到这条测试 —— 那时他必须给出理由（而不是随手改）。
//
// ⚠ 判据是**词级**（整页文本）⇒ 连注释里出现都会被拦 ✓（这是按设计工作 ✓
//
//	见 T-442/T-454 两次同型教训：墓碑注释里写原名会让守卫红）。
//
// ⚠ 同款文案在其余 6 个页面仍然保留 —— 本守卫**只管状态页**（用户只框了状态页那一行）。
func TestStatusPageHasNoUpstreamCredit(t *testing.T) {
	page := readStatusPage(t)
	for _, gone := range []string{"New API contributors", "Frontend design and development"} {
		if strings.Contains(page, gone) {
			t.Errorf("状态页里仍有「%s」—— 用户 2026-11-09 明确要求连文字一起删掉"+
				"（页脚那行归属文案）。若要恢复，请先说明是哪条合规要求、"+
				"以及为什么只收回状态页这一处。", gone)
		}
	}
	// 反向：确认**只删了**那一句，页脚其余两段还在。
	//（防的是"顺手把整个 footer 删了"这类过度删除 —— 用户只说删外部链接那句。）
	for _, keep := range []string{"数据来源", "更新于"} {
		if !strings.Contains(page, keep) {
			t.Errorf("页脚里的「%s」不见了 —— 本轮只该删归属那一句，"+
				"数据来源与更新时间必须保留", keep)
		}
	}
}
