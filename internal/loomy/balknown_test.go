package loomy

import (
	"testing"
)

// ⚠ 这组测试钉住"缓存里**没写** balance"与"写了 balance=0"必须能分开。
//
// # 来源（用户 2026-10-18 实测报的现象）
//
// 账号池里一批号的「额度」列显示 **0**，而同一行的成功次数是 616 / 701 / 746 ——
// ⚠ 一直在成功发请求的号，额度不可能是 0 ⇒ 那些 0 是"**缓存里没这个字段**"，
// 被 `int64` 的零值压成了 0，而界面无从分辨。
//
// 项目里**早就有**这条纪律（`DailyKnown` / `Points.PermanentKnown` /
// `gateway.QuotaView.HasData` 都在表达"未知 ≠ 零"），⚠ **唯独漏了 `balance`** ——
// 所以这组测试的价值不是"发现新原则"，而是**把漏掉的那一维钉住**，
// 免得下次重构又把 `out.BalanceKnown = true` 那行删掉。

func i64(v int64) *int64 { return &v }

// TestPointsFoldDistinguishesMissingBalanceFromZero 三种形态各断言一次。
func TestPointsFoldDistinguishesMissingBalanceFromZero(t *testing.T) {
	// 形态一：客户端只写了 dailyBalance（实测真实存在的**部分更新**）
	got := pointsFold([]pointsWire{{DailyBalance: i64(4950)}})
	if got.BalanceKnown {
		t.Error("缓存里**没有** balance 键 ⇒ BalanceKnown 必须是 false" +
			"（否则界面把'没读到'画成 0 —— 而一个一直在成功的号被显示成'额度 0'）")
	}
	if got.Balance != 0 {
		t.Errorf("Balance = %d；缺字段时值可以是 0，但必须由 Known=false 表达'未知'", got.Balance)
	}
	if !got.DailyKnown || got.DailyBalance != 4950 {
		t.Errorf("dailyBalance 写着 4950 ⇒ DailyKnown=true 且值 4950，得到 known=%v val=%d",
			got.DailyKnown, got.DailyBalance)
	}

	// 形态二：明确写了 balance=0
	got0 := pointsFold([]pointsWire{{Balance: i64(0)}})
	if !got0.BalanceKnown {
		t.Error("缓存里**写了** balance=0 ⇒ BalanceKnown 必须是 true" +
			"（'确实是 0'与'没读到'不是一件事）")
	}
	if got0.Balance != 0 {
		t.Errorf("Balance = %d，想要 0", got0.Balance)
	}

	// 形态三：写了非零
	gotN := pointsFold([]pointsWire{{Balance: i64(17944)}})
	if !gotN.BalanceKnown || gotN.Balance != 17944 {
		t.Errorf("写出 17944 ⇒ known=true val=17944，得到 known=%v val=%d", gotN.BalanceKnown, gotN.Balance)
	}
}

// TestPointsFoldLaterRecordOverwrites 后一条（按 UpdatedAt 更晚）覆盖前一条的 balance。
//
// ⚠ 顺带钉住"只要**任何一条**写过 balance，Known 就是 true" ——
// 因为客户端会写部分更新：先有 balance、后一条只有 dailyBalance。
// ⚠ 那一条不该把已知的 balance 抹掉（正是 `pointsWire` 用指针的第二个理由）。
func TestPointsFoldLaterRecordOverwrites(t *testing.T) {
	got := pointsFold([]pointsWire{
		{Balance: i64(5500), UpdatedAt: "2026-10-18T03:04:34+08:00"},
		{DailyBalance: i64(4950), UpdatedAt: "2026-10-18T09:38:46+08:00"},
	})
	if !got.BalanceKnown || got.Balance != 5500 {
		t.Errorf("后一条没写 balance ⇒ 不该把已知的 5500 抹掉，得到 known=%v val=%d",
			got.BalanceKnown, got.Balance)
	}
	if !got.DailyKnown || got.DailyBalance != 4950 {
		t.Errorf("后一条写了 dailyBalance ⇒ 应覆盖为 4950，得到 known=%v val=%d",
			got.DailyKnown, got.DailyBalance)
	}
}
