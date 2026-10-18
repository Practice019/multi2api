package loomy

import (
	"reflect"
	"testing"
	"time"
)

// 本文件：分层 TTL 的"过期集合"判定。见 panelttl.go 与 docs/cache-refresh-design.md。
//
// ⚠ 这些断言全是**纯逻辑**（不碰网络、不碰锁）⇒ 它们能覆盖到"刷不刷"这条决策的
// 每一个角落，而那是本轮改动里最贵的一个决策：算错的方向有两个，
// 一个白刷上游、一个永远显示旧数据，**两种都不会崩**。

type ttlRow struct {
	fetched time.Time
	ok      bool
}

func ttlFixture(now time.Time) map[string]ttlRow {
	return map[string]ttlRow{
		"fresh":    {fetched: now.Add(-1 * time.Minute), ok: true},  // 1 分钟前 ⇒ 新鲜
		"edge-in":  {fetched: now.Add(-9 * time.Minute), ok: true},  // TTL 边界内
		"edge-out": {fetched: now.Add(-11 * time.Minute), ok: true}, // TTL 边界外
		"stale":    {fetched: now.Add(-2 * time.Hour), ok: true},    // 很久以前 ⇒ 过期
		"never":    {fetched: time.Time{}, ok: true},                // ⚠ 从来没读到过（零值）
		"failed":   {fetched: now.Add(-2 * time.Hour), ok: false},   // ⚠ 失败行（不该进过期集合）
	}
}

func ttlCall(rows map[string]ttlRow, now time.Time, ttl time.Duration, force bool) []string {
	return expiredRows(rows, now, ttl, force,
		func(v ttlRow) time.Time { return v.fetched },
		func(v ttlRow) bool { return v.ok })
}

// TestExpiredRowsHonoursTTL TTL 内的不返回，TTL 外的返回；零值与失败行都不进集合。
func TestExpiredRowsHonoursTTL(t *testing.T) {
	now := time.Now()
	got := ttlCall(ttlFixture(now), now, 10*time.Minute, false)
	// ⚠ 按 uid 升序（expiredRows 保证了稳定顺序，所以这里敢直接比切片）
	want := []string{"edge-out", "stale"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("过期集合 = %v，want %v\n"+
			"⚠ 三个必须**不出现**的：fresh/edge-in（TTL 内 ⇒ 白刷上游）、"+
			"never（零值 = 从没读到过 ⇒ 该走补数那条路）、failed（失败行 ⇒ 该重试不是刷新）", got, want)
	}
}

// TestExpiredRowsZeroValueIsNotExpired ⚠ 「从来没读到过」绝不能算过期。
//
// 混进去的后果：一批从未查过的号每一轮都被优先刷，而真正过期的排在后面（饿死）。
func TestExpiredRowsZeroValueIsNotExpired(t *testing.T) {
	now := time.Now()
	rows := map[string]ttlRow{"never": {fetched: time.Time{}, ok: true}}
	if got := ttlCall(rows, now, time.Nanosecond, false); len(got) != 0 {
		t.Errorf("零值 FetchedAt 不该进过期集合（它是'从没读到过'，由补数那条路负责），实际 %v", got)
	}
}

// TestExpiredRowsFailureNotExpired 失败行不按 TTL 处理。
func TestExpiredRowsFailureNotExpired(t *testing.T) {
	now := time.Now()
	rows := map[string]ttlRow{"failed": {fetched: now.Add(-100 * time.Hour), ok: false}}
	if got := ttlCall(rows, now, time.Nanosecond, false); len(got) != 0 {
		t.Errorf("失败行不该按 TTL 进过期集合（它需要的是重试），实际 %v", got)
	}
}

// TestExpiredRowsForceIgnoresTTL force=true 用于"手动刷新"—— 忽略 TTL，但仍排除零值与失败行。
func TestExpiredRowsForceIgnoresTTL(t *testing.T) {
	now := time.Now()
	got := ttlCall(ttlFixture(now), now, 24*time.Hour, true)
	want := []string{"edge-in", "edge-out", "fresh", "stale"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("force 时的集合 = %v，want %v（⚠ never / failed 仍须排除）", got, want)
	}
}

// TestExpiredRowsEmptyAndNil 空集合不 panic、不返回 nil 之外的意外值。
func TestExpiredRowsEmptyAndNil(t *testing.T) {
	now := time.Now()
	if got := ttlCall(nil, now, time.Minute, false); len(got) != 0 {
		t.Errorf("空 map 应当返回空，实际 %v", got)
	}
	if got := ttlCall(map[string]ttlRow{}, now, time.Minute, true); len(got) != 0 {
		t.Errorf("空 map + force 应当返回空，实际 %v", got)
	}
}

// TestTTLLayeringIsOrdered 钉住分层的相对次序 —— 它是整份设计的支点。
//
// ⚠ 如果谁把邀请码的 TTL 改到比额度还短，那"分层"就名存实亡了（额度会永远旧）。
func TestTTLLayeringIsOrdered(t *testing.T) {
	if !(TTLQuota < TTLTasks && TTLTasks < TTLInvite) {
		t.Errorf("分层次序必须是 额度(%v) < 任务(%v) < 邀请码(%v)：\n"+
			"额度变得最快（每次对话都动，还喂给选号权重）⇒ 必须最短；"+
			"邀请码几乎不变 ⇒ 必须最长。v 反了就等于没有分层。",
			TTLQuota, TTLTasks, TTLInvite)
	}
}
