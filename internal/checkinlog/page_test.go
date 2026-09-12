package checkinlog

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Page 是「30 条一页」的基础能力：既有 offset 也有总数。
//
// 之所以必须做在后端而不是前端切数组：历史保留 30 天，条数可能上千，
// 一次性把全量发给前端再切页，等于把分页想解决的问题又搬回了浏览器。
//
// 契约（与其余日志类接口保持一致）：
//   - 返回顺序为**时间倒序（最新在前）**；
//   - total 是**过滤后**的总条数（不是本页条数），前端据此算总页数；
//   - offset 超出范围返回空页而不是报错；
//   - limit<=0 时用默认页大小。

func newPagedLog(t *testing.T, n int) *Log {
	t.Helper()
	l := New(filepath.Join(t.TempDir(), "h.json"), 30)
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		kind := KindCheckin
		if i%3 == 0 {
			kind = KindGrowth
		}
		l.Append(Record{
			At:      base.Add(time.Duration(i) * time.Minute),
			UID:     fmt.Sprintf("u%d", i),
			Kind:    kind,
			Status:  StatusOK,
			Credits: int64(i),
		})
	}
	return l
}

func TestPageReturnsNewestFirst(t *testing.T) {
	l := newPagedLog(t, 5)
	items, total := l.Page(0, 3, "")
	if total != 5 {
		t.Errorf("total=%d，期望 5（过滤后总数，不是本页条数）", total)
	}
	if len(items) != 3 {
		t.Fatalf("本页条数=%d，期望 3", len(items))
	}
	// 最新那条是 i=4（base+4min）
	if items[0].UID != "u4" {
		t.Errorf("首条=%s，期望 u4（时间倒序，最新在前）", items[0].UID)
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].At.Before(items[i].At) {
			t.Errorf("顺序错误：第 %d 条比第 %d 条更旧", i-1, i)
		}
	}
}

func TestPageOffsetWalksThroughAll(t *testing.T) {
	l := newPagedLog(t, 7)

	seen := map[string]bool{}
	for offset := 0; offset < 7; offset += 3 {
		items, total := l.Page(offset, 3, "")
		if total != 7 {
			t.Fatalf("offset=%d total=%d，期望恒为 7", offset, total)
		}
		for _, r := range items {
			if seen[r.UID] {
				t.Errorf("翻页出现重复记录 %s", r.UID)
			}
			seen[r.UID] = true
		}
	}
	if len(seen) != 7 {
		t.Errorf("翻完所有页只覆盖 %d 条，期望 7", len(seen))
	}
}

func TestPageLastPageIsShort(t *testing.T) {
	l := newPagedLog(t, 7)
	items, total := l.Page(6, 3, "")
	if total != 7 {
		t.Errorf("total=%d", total)
	}
	if len(items) != 1 {
		t.Errorf("末页条数=%d，期望 1（7 条按 3 条一页，末页剩 1 条）", len(items))
	}
}

func TestPageOffsetBeyondEndReturnsEmpty(t *testing.T) {
	// 前端在多标签/慢轮询下很容易算出越界 offset，
	// 这时必须返回空页而不是报错或 panic。
	l := newPagedLog(t, 3)
	items, total := l.Page(100, 30, "")
	if total != 3 {
		t.Errorf("total=%d 应仍为 3", total)
	}
	if len(items) != 0 {
		t.Errorf("越界 offset 应返回空页，得到 %d 条", len(items))
	}
}

func TestPageKindFilterAffectsTotal(t *testing.T) {
	// 20 条里 i%3==0 的（i=0,3,6,9,12,15,18）共 7 条是 growth
	l := newPagedLog(t, 20)
	items, total := l.Page(0, 30, KindGrowth)
	if total != 7 {
		t.Errorf("growth 过滤后 total=%d，期望 7", total)
	}
	if len(items) != 7 {
		t.Errorf("本页条数=%d，期望 7", len(items))
	}
	for _, r := range items {
		if r.Kind != KindGrowth {
			t.Errorf("过滤失效，混入了 kind=%s", r.Kind)
		}
	}
}

func TestPageDefaultLimitWhenNonPositive(t *testing.T) {
	l := newPagedLog(t, 40)
	items, _ := l.Page(0, 0, "")
	if len(items) != DefaultPageSize {
		t.Errorf("limit<=0 时本页条数=%d，期望默认 %d", len(items), DefaultPageSize)
	}
}

// 与 logbuf.MaxPageSize 对应：上界由服务端兜住，不依赖前端的数字框自觉。
func TestPageClampsLimitToMax(t *testing.T) {
	l := newPagedLog(t, MaxPageSize+50)
	for _, limit := range []int{MaxPageSize + 1, 1000, 1 << 20} {
		items, total := l.Page(0, limit, "")
		if len(items) != MaxPageSize {
			t.Errorf("limit=%d 本页=%d，期望夹到 %d", limit, len(items), MaxPageSize)
		}
		if total != MaxPageSize+50 {
			t.Errorf("limit=%d total=%d，期望 %d（total 是全量）", limit, total, MaxPageSize+50)
		}
	}
}

func TestPageLimitExactlyMax(t *testing.T) {
	l := newPagedLog(t, MaxPageSize+10)
	items, _ := l.Page(0, MaxPageSize, "")
	if len(items) != MaxPageSize {
		t.Errorf("limit=MaxPageSize 本页=%d，期望 %d（边界值不应被夹紧）", len(items), MaxPageSize)
	}
}

func TestPageClampedLimitKeepsOffsetAndFilter(t *testing.T) {
	const n = MaxPageSize + 50
	l := newPagedLog(t, n)
	// 夹紧 limit 后，offset 仍按原语义生效。
	// newPagedLog 写入的 UID 是 u0..u(n-1)，倒序后第 k 条对应 u(n-1-k)。
	items, _ := l.Page(5, 1<<20, "")
	if len(items) != MaxPageSize {
		t.Fatalf("本页=%d，期望 %d", len(items), MaxPageSize)
	}
	want := fmt.Sprintf("u%d", n-1-5)
	if items[0].UID != want {
		t.Errorf("offset=5 时首条=%s，期望 %s", items[0].UID, want)
	}
	// 本页最后一条 = u(n-1-5-(MaxPageSize-1))
	wantLast := fmt.Sprintf("u%d", n-1-5-(MaxPageSize-1))
	if items[len(items)-1].UID != wantLast {
		t.Errorf("本页末条=%s，期望 %s", items[len(items)-1].UID, wantLast)
	}
}

func TestPageNegativeOffsetTreatedAsZero(t *testing.T) {
	l := newPagedLog(t, 5)
	items, _ := l.Page(-10, 2, "")
	if len(items) != 2 {
		t.Fatalf("负 offset 应视为 0，得到 %d 条", len(items))
	}
	if items[0].UID != "u4" {
		t.Errorf("负 offset 未从最新开始：首条=%s", items[0].UID)
	}
}

// ---------------------------------------------------------------------------
// PageAll：给"出口过滤"用的全量快照（T4）
// ---------------------------------------------------------------------------

// TestPageAllReturnsEverythingNewestFirst 钉住 PageAll 的契约。
//
// # 它为什么会存在
//
// 出口过滤必须发生在**分页之前**（否则 total 会算成过滤前的数，
// 前端据此算出的页数指向不存在的页）。而 Page 的过滤（kind）在它内部做，
// 外部拿不到"过滤后的全集"，所以需要 PageAll。
//
// # 为什么它必须返回**时间正序**
//
// 端点层的过滤保持顺序不变，末了再倒着切页（与 Page 的倒序遍历一致）。
// 若 PageAll 按倒序返回，端点层就会再倒一次 —— 两边各以为对方负责顺序，
// 表现是"历史面板的时间顺序反了"，而这不会报任何错。
// l.records 本身是正序，PageAll 就是它的拷贝，所以这里直接断言正序。
func TestPageAllReturnsEverythingNewestFirst(t *testing.T) {
	l := newPagedLog(t, 7)
	all := l.PageAll()
	if len(all) != 7 {
		t.Fatalf("PageAll 返回 %d 条，期望 7（全部）", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].At.Before(all[i-1].At) {
			t.Errorf("PageAll 第 %d 条比第 %d 条更旧 —— 期望时间正序"+
				"（端点层负责倒序切页，两边都倒会让顺序反掉）", i, i-1)
		}
	}
	if all[0].UID != "u0" || all[len(all)-1].UID != "u6" {
		t.Errorf("首尾=%s..%s，期望 u0..u6", all[0].UID, all[len(all)-1].UID)
	}
}

// TestPageAllIsACopy 钉住"PageAll 返回拷贝"。
//
// # 为什么这条不是形式主义
//
// 端点层在**锁外**过滤（拿 ownUIDs() 要问账号池，不能在 checkinlog 的锁里做）。
// 若 PageAll 返回内部切片，并发的 Append 会在容量足够时**原地改写底层数组** ——
// 过滤读到的是写了一半的状态，表现为"偶尔少一条/多一条记录"，无法复现。
//
// 判据不靠"读代码看有没有 copy"：直接改返回值，再确认内部状态没被改。
func TestPageAllIsACopy(t *testing.T) {
	l := newPagedLog(t, 3)
	all := l.PageAll()
	all[0].UID = "被篡改"
	all[0].Kind = "被篡改"

	again := l.PageAll()
	if again[0].UID == "被篡改" || again[0].Kind == "被篡改" {
		t.Errorf("改 PageAll 的返回值影响到了内部状态 —— 返回的是内部切片而不是拷贝"+
			"（端点层在锁外过滤时会读到写一半的数据）实际: %+v", again[0])
	}
}
