package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// ⚠ 本文件钉住一次**真 bug** 的修法（2026-10-18）。
//
// # 症状（用户报的）
//
//	503 no_healthy_account —— "44 个账号：**0 禁用、0 冷却**:
//	  chatStreamOnce: 账号 X 不在池里（可能已被移除）"
//	⚠ 而那句提示**把两个可能的原因排除了，却没说真原因** ⇒ 只能从零猜。
//
// # 根因
//
//	① 提示少报一维：`poolUnavailableHintOf` 只看 Disabled/Cooling，**没看 in_flight**；
//	② **真因**：工具循环（chattool_send.go）用 `PickByUID` 取号，而它会判 `inFlightFull`；
//	   而那个名额**正是本次请求自己占的**（出口层 Acquire 后整个请求持有）
//	   ⇒ 同一账号并发达到上限（默认 3）时，那条路径**必失败** ⇒ 503。
//	   ⚠ 而"过几分钟自己好" = 那批并发结束、名额释放。
//
// # 修法
//
//	工具循环改用 `AuthByUID`（只取凭证，不看健康/名额）——
//	⚠ 与本项目既有做法一致：`Provider.authByUID`（chattool.go:278 / imagegen_ext.go:81 等）
//	  本来就是这个语义，⚠ 只有 chattool_send.go 那两处用错了。

// TestAuthByUIDIgnoresInFlight 占满在途名额后，AuthByUID 仍应给出凭证。
//
// ⚠ 这是"工具循环不换号"那条路的**前提**：出口层已经选过号并 Acquire，
// 工具循环里只需要凭证 —— 若那里再判一次名额，就是**把自己重复计数**。
func TestAuthByUIDIgnoresInFlight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)

	if !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	// ⚠ 名额已被本次请求占满：
	if p.PickByUID("u1") != nil {
		t.Error("PickByUID 在满载时应返回 nil（这是它的语义，别改）")
	}
	// ⚠ 而 AuthByUID **不看名额** —— 这正是工具循环要的：
	if got := p.AuthByUID("u1"); got == nil {
		t.Error("AuthByUID 在满载时也必须给凭证 —— 工具循环靠它继续跑，否则必 503")
	}
	p.Release("u1")
}

// TestAuthByUIDStillNilForMissingUID 修法**不能掩盖真错误**：
// uid 真的不在池里时，AuthByUID 仍返回 nil（与 PickByUID 同）。
func TestAuthByUIDStillNilForMissingUID(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if got := p.AuthByUID("no-such-uid"); got != nil {
		t.Errorf("不存在的 uid 应返回 nil，得到 %+v", got)
	}
}

// TestPickByUIDUnchangedForEntryLayer 出口层语义**不许被这次修改带走**：
// 满载时 PickByUID 必须返回 nil（出口层要据此换号）。
func TestPickByUIDUnchangedForEntryLayer(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	p.Acquire("u1")
	if p.PickByUID("u1") != nil {
		t.Error("出口层依赖 PickByUID 在满载时返回 nil 以便换号 —— 这条语义不能变")
	}
	p.Release("u1")
	// 释放后应能再取到
	if p.PickByUID("u1") == nil {
		t.Error("释放后应可再取")
	}
}
