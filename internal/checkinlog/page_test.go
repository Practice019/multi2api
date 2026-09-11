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
