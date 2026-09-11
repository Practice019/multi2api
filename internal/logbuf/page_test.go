package logbuf

import (
	"path/filepath"
	"testing"
	"time"
)

// Sink.Page 与 checkinlog.Log.Page 保持**同一套分页契约**（这是「统一」的要求）：
//   - 返回顺序为时间倒序（最新在前）；
//   - total 是文件内总条数，不是本页条数；
//   - offset 越界返回空页不报错；limit<=0 用 DefaultPageSize。

func newPagedSink(t *testing.T, n int) *Sink {
	t.Helper()
	s, err := OpenSink(filepath.Join(t.TempDir(), "r.jsonl"), 7, 0)
	if err != nil {
		t.Fatalf("OpenSink: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		s.Append(Entry{
			Seq:     int64(i + 1),
			At:      base.Add(time.Duration(i) * time.Minute),
			Model:   "m",
			Mode:    "stream",
			Status:  200,
			TotalMS: int64(i),
		})
	}
	return s
}

func TestSinkPageReturnsNewestFirst(t *testing.T) {
	s := newPagedSink(t, 5)
	items, total, err := s.Page(0, 3)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if total != 5 {
		t.Errorf("total=%d，期望 5", total)
	}
	if len(items) != 3 {
		t.Fatalf("本页条数=%d，期望 3", len(items))
	}
	if items[0].Seq != 5 {
		t.Errorf("首条 seq=%d，期望 5（最新在前）", items[0].Seq)
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].At.Before(items[i].At) {
			t.Errorf("顺序错误：第 %d 条比第 %d 条更旧", i-1, i)
		}
	}
}

func TestSinkPageWalksThroughAllWithoutDuplicates(t *testing.T) {
	s := newPagedSink(t, 7)
	seen := map[int64]bool{}
	for offset := 0; offset < 7; offset += 3 {
		items, total, err := s.Page(offset, 3)
		if err != nil {
			t.Fatalf("Page(%d): %v", offset, err)
		}
		if total != 7 {
			t.Fatalf("offset=%d total=%d，期望恒为 7", offset, total)
		}
		for _, e := range items {
			if seen[e.Seq] {
				t.Errorf("翻页重复 seq=%d", e.Seq)
			}
			seen[e.Seq] = true
		}
	}
	if len(seen) != 7 {
		t.Errorf("翻完覆盖 %d 条，期望 7", len(seen))
	}
}

func TestSinkPageOffsetBeyondEnd(t *testing.T) {
	s := newPagedSink(t, 3)
	items, total, err := s.Page(100, 30)
	if err != nil {
		t.Fatalf("越界 offset 不应报错: %v", err)
	}
	if total != 3 {
		t.Errorf("total=%d 应仍为 3", total)
	}
	if len(items) != 0 {
		t.Errorf("越界应返回空页，得到 %d 条", len(items))
	}
}

func TestSinkPageDefaultLimit(t *testing.T) {
	s := newPagedSink(t, 40)
	items, _, err := s.Page(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != DefaultPageSize {
		t.Errorf("limit<=0 本页=%d，期望 %d", len(items), DefaultPageSize)
	}
}

func TestSinkPageEmptyFile(t *testing.T) {
	s, err := OpenSink(filepath.Join(t.TempDir(), "empty.jsonl"), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items, total, err := s.Page(0, 30)
	if err != nil {
		t.Fatalf("空文件不应报错: %v", err)
	}
	if total != 0 || len(items) != 0 {
		t.Errorf("空文件应为 0 条，得到 total=%d len=%d", total, len(items))
	}
}

func TestSinkPageNegativeOffset(t *testing.T) {
	s := newPagedSink(t, 5)
	items, _, err := s.Page(-10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("负 offset 应视为 0，得到 %d 条", len(items))
	}
	if items[0].Seq != 5 {
		t.Errorf("负 offset 未从最新开始：首条 seq=%d", items[0].Seq)
	}
}
