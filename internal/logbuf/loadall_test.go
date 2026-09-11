package logbuf

import (
	"path/filepath"
	"testing"
	"time"
)

// LoadAll 是给**内部聚合**用的旁路：不受 Page 的分页上界约束。
//
// 背景（线上实测到的 bug）：调用统计调 sink.Page(0, 1<<20) 想取全量，
// 但 Page 里 `limit > MaxPageSize` 会把它夹到 300 —— 于是一份 1855 行的
// 日志只聚合了最近 300 条，界面上的「共 N 条」「成功率」「平均 TTFB」
// 全部只覆盖那一小段，且**静默无报错**。
//
// 语义划分：
//
//	Page    对外（HTTP 分页），必须有上界，防止一次拉爆
//	LoadAll 对内（聚合/统计），文件本身已由 maxBytes+keepDays 约束，不需要上界
func TestLoadAllReturnsEverything(t *testing.T) {
	const n = MaxPageSize + 137 // 明显超过分页上界
	s := newPagedSink(t, n)

	all, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Errorf("LoadAll 应返回全部 %d 条，得到 %d —— 说明仍受分页上界约束", n, len(all))
	}
}

// 时间倒序（最新在前），与 Page / LoadRecent 的契约一致，
// 否则聚合方对「最近」的理解会与展示层不一致。
func TestLoadAllNewestFirst(t *testing.T) {
	s := newPagedSink(t, 5)
	all, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Seq <= all[i].Seq {
			t.Errorf("应为最新在前：all[%d].Seq=%d 不大于 all[%d].Seq=%d",
				i-1, all[i-1].Seq, i, all[i].Seq)
		}
	}
}

// 空文件/不存在都返回空切片且不报错。
func TestLoadAllEmpty(t *testing.T) {
	s, err := OpenSink(filepath.Join(t.TempDir(), "empty.jsonl"), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	all, err := s.LoadAll()
	if err != nil {
		t.Fatalf("空文件不应报错: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("空文件应为 0 条，得到 %d", len(all))
	}
}

// nil sink 安全：与 Page/LoadRecent 一致，调用方不必判空。
func TestLoadAllNilSink(t *testing.T) {
	var s *Sink
	all, err := s.LoadAll()
	if err != nil || all != nil {
		t.Errorf("nil sink 应返回 (nil, nil)，得到 (%v, %v)", all, err)
	}
}

// 反证：Page 仍然受上界约束 —— LoadAll 是旁路，不是把 Page 的上界去掉。
//
// 这条很重要：如果修 bug 的方式是"把 MaxPageSize 调大或删掉"，
// 那 HTTP 侧就重新暴露在一次拉爆的风险里。这里钉死两者的语义差别。
func TestLoadAllIsBypassNotUnboundedPage(t *testing.T) {
	s := newPagedSink(t, MaxPageSize+50)

	// Page 仍应被夹紧
	items, _, err := s.Page(0, MaxPageSize+1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != MaxPageSize {
		t.Errorf("Page 仍应被夹到 %d，得到 %d —— 分页上界被误删了",
			MaxPageSize, len(items))
	}

	// 而 LoadAll 应拿到全部
	all, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != MaxPageSize+50 {
		t.Errorf("LoadAll 应拿到全部 %d 条，得到 %d", MaxPageSize+50, len(all))
	}
}

// 与 LoadRecent 的分工：后者是"最近 N"，n<=0 时才等于全部。
// LoadAll 恒为全部，语义更明确，聚合方不必知道"传 0 表示全部"这个约定。
func TestLoadAllVsLoadRecent(t *testing.T) {
	s := newPagedSink(t, 20)
	all, _ := s.LoadAll()
	recent, _ := s.LoadRecent(5)
	if len(all) != 20 {
		t.Errorf("LoadAll 应 20 条，得到 %d", len(all))
	}
	if len(recent) != 5 {
		t.Errorf("LoadRecent(5) 应 5 条，得到 %d", len(recent))
	}
	// 两者的"最新在前"应当一致
	if all[0].Seq != recent[0].Seq {
		t.Errorf("最新一条应相同：LoadAll[0].Seq=%d LoadRecent[0].Seq=%d",
			all[0].Seq, recent[0].Seq)
	}
}

// 灌入带时间戳的数据，确认 LoadAll 不改变 Entry 内容（聚合依赖字段完整性）。
func TestLoadAllPreservesFields(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSink(filepath.Join(dir, "r.jsonl"), 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		s.Append(Entry{
			Seq: int64(i + 1), At: base.Add(time.Duration(i) * time.Minute),
			Model: "m1", Mode: "stream", Status: 200, TotalMS: 100, TTFBMS: 10,
			Tokens: 42, UID: "u1",
		})
	}
	all, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("应 3 条，得到 %d", len(all))
	}
	last := all[0] // 最新
	if last.Model != "m1" || last.Status != 200 || last.Tokens != 42 || last.UID != "u1" {
		t.Errorf("字段在 LoadAll 后应完整保留，得到 %+v", last)
	}
}
