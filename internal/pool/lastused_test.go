package pool

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// mustParseTime 测试用的固定时刻解析。⚠ 用固定字符串而不是 time.Now()：
// 这一组断言的是"字段/键名/round-trip"，与"现在几点"无关 ——
// 把 Now() 引进来只会让它在某些钟点表现不同（本仓已经吃过一次这种 flaky 的亏）。
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("测试自带的时刻写错了: %v", err)
	}
	return v
}

// 本文件：`Status.LastUsed` 的导出契约 —— 额度刷新靠它排队（见 admin 的 RefreshAllQuotasNow）。
//
// ⚠ 为什么值得单独钉住：这个字段是**给另一个包用的**（admin 排序），
//   所以它的"键名/语义"一旦改了，症状是**额度刷新的顺序悄悄变回乱序** ——
//   admin 那边不会报错，只会把最该刷的排在后面，表现为"刚用过的号额度还是旧的"。

// TestStatusJSONKeyIsLastUsed 键名必须是 `last_used`（下划线）。
//
// ⚠ 防的是"改了字段名/标签，而调用方还在读旧键"这类静默失配 ——
// 与 `AccountView.FreeQuota` 那条 `TestFreeQuotaJSONKeyIsStable` 同一条纪律。
func TestStatusJSONKeyIsLastUsed(t *testing.T) {
	b, err := json.Marshal(Status{UID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	// ⚠⚠ **零值也会下发**，键名同样是 `last_used`。
	//
	// 这不是我想要的（我第一版断言"零值不该出现"，然后被这条测试自己打了脸）：
	// ⚠ **Go 的 `omitempty` 对 `time.Time` 不生效** —— `time.Time` 是 struct，
	//   而 omitempty 只对"空值"（0/""/nil/长度 0 的容器）生效，struct 永远不算空。
	//   ⇒ 同一个 `Status` 里的 `until` / `last_success` / `last_err` / `breaker_until`
	//     **全都是这个行为**（看上面那次失败输出里那一排 `0001-01-01T00:00:00Z`）。
	//
	// ⚠ 所以这里**对齐既有行为**而不是让 `last_used` 变成一个例外 ——
	//   一个结构体里"四个时间字段里有一个特殊"比"四个都一样"更容易出错。
	// ⚠ 而它**无害**：零值 = 1970 年（实际上是最小时间）⇒ admin 那边按
	//   `After()` 排序时它排**最后**，正好是"从没被选中过"该有的位置。
	//   ⚠ 前提是那边用 `After()` 比较而不是"判断字段在不在"—— 后者会把它当成
	//     "刚被用过"，那才是真的错。这条测试与 admin 的排序实现共同钉住这一点。
	if !strings.Contains(string(b), `"last_used":"0001-01-01T00:00:00Z"`) {
		t.Errorf("零值应当照常下发 `last_used`（与 last_success/last_err/until 同行为："+
			"Go 的 omitempty 对 time.Time 不生效），实际 %s", b)
	}

	// 非零值：键名必须是 `last_used`（admin 排序读的就是它）。
	b2, err := json.Marshal(Status{UID: "u1", LastUsed: mustParseTime(t, "2026-10-18T23:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), `"last_used":"2026-10-18T23:00:00Z"`) {
		t.Errorf("非零值必须以 `last_used` 为键下发（admin 排序读的就是它），实际 %s", b2)
	}
}

// TestStatusLastUsedZeroSortsLast 零值（从没被选中过）排序时必须排**最后**。
//
// ⚠ 这是 admin 那条"优先刷最近用过的"排序依赖的唯一性质。
//
//	若哪天有人把 `After()` 换成"字段非零优先"之类，零值会跑到**最前面** ——
//	于是每轮都先刷一批"从没被用过"的号，而真正该刷的被挤到后面。
func TestStatusLastUsedZeroSortsLast(t *testing.T) {
	zero := Status{UID: "never-used"}
	used := Status{UID: "used", LastUsed: mustParseTime(t, "2026-10-18T23:00:00Z")}

	// ⚠ 复现 admin 的判据：更晚的排前面。
	usedFirst := used.LastUsed.After(zero.LastUsed)
	if !usedFirst {
		t.Errorf("被用过的号必须排在'从没被选中过'的前面（%v vs %v）—— "+
			"否则每轮都先刷一批从没用过的号，而真正该刷的被挤后面",
			used.LastUsed, zero.LastUsed)
	}
	if zero.LastUsed.After(used.LastUsed) {
		t.Error("零值不该被判定为'更晚'")
	}
}

// TestStatusLastUsedCarriesThroughJSON 值要能原样 round-trip。
func TestStatusLastUsedCarriesThroughJSON(t *testing.T) {
	want := mustParseTime(t, "2026-10-18T23:00:00Z")
	b, err := json.Marshal(Status{UID: "u1", LastUsed: want})
	if err != nil {
		t.Fatal(err)
	}
	var got Status
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.LastUsed.Equal(want) {
		t.Errorf("LastUsed 往返后 = %v，want %v", got.LastUsed, want)
	}
}
