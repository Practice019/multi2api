package pool

import (
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// autodisable_reason_test.go —— 「一次即摘」的**原因文案**必须自洽。
//
// # 为什么值得一条测试
//
// entry.reason 会**原样**出现在管理台的「原因」列，是排查时唯一能看的东西。
// 而 force（见 RecordAutoDisableFailureNow）摘号时 streak 只有 1 —— 若沿用
// 通用文案就会写成「自动禁用：连续失败 1 次（阈值 5）」：
// 1 < 5 却摘了号，读的人只会认定逻辑坏了，然后去查一个不存在的 bug。
func TestForcedDisableReasonDoesNotMentionThreshold(t *testing.T) {
	p := New("")
	const uid = "prov-1"
	p.AddFor("prov", &auth.Auth{UID: uid, Nickname: uid}, nil)
	p.SetAutoDisable(5, DefaultAutoDisableWindow)

	if !p.RecordAutoDisableFailureNow(uid, "额度刷新：账号层失效") {
		t.Fatal("一次即摘应当返回 true（发生了状态跃迁）")
	}
	st, ok := p.Status(uid)
	if !ok || !st.Disabled {
		t.Fatalf("应当已被禁用：ok=%v disabled=%v", ok, st.Disabled)
	}
	// ⚠ 判据是「连续失败」这四个字，不是「阈值」二字：
	//   文案里出现「未走累计阈值」是**有用**的（它解释了为什么 1 次就够），
	//   而「连续失败 1 次（阈值 5）」才是误导（1 < 5 却摘了号 ⇒ 让人以为逻辑坏了）。
	if strings.Contains(st.Reason, "连续失败") {
		t.Errorf("一次即摘的文案不该写「连续失败 N 次（阈值 M）」——那会让人以为逻辑坏了：%s", st.Reason)
	}
	if !strings.Contains(st.Reason, "一次即摘") {
		t.Errorf("文案应说明这是一次即摘：%s", st.Reason)
	}
	if !strings.Contains(st.Reason, "账号层失效") {
		t.Errorf("文案应带上原因原文，否则排查时只能翻日志：%s", st.Reason)
	}
}

// TestCountedDisableReasonKeepsThreshold 反向钉子：**计数版**的文案仍要写明阈值。
//
// 两条路径的文案必须可区分 —— 否则排查时分不清"是累计到的"还是"一次就摘的"。
func TestCountedDisableReasonKeepsThreshold(t *testing.T) {
	p := New("")
	const uid = "prov-2"
	p.AddFor("prov", &auth.Auth{UID: uid, Nickname: uid}, nil)
	p.SetAutoDisable(2, DefaultAutoDisableWindow)

	for i := 0; i < 2; i++ {
		p.RecordAutoDisableFailure(uid, "upstream server")
	}
	st, _ := p.Status(uid)
	if !st.Disabled {
		t.Fatal("达阈值后应被禁用")
	}
	if !strings.Contains(st.Reason, "阈值") {
		t.Errorf("计数版文案应写明阈值（它是「为什么是这次」的依据）：%s", st.Reason)
	}
}
