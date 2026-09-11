package logbuf

import (
	"path/filepath"
	"testing"
	"time"
)

// MaxSeq 用于启动时播种请求序号计数器。
// 背景：计数器是进程级的，重启会从 1 重来，于是同一个落盘文件里会出现重复 seq，
// 「序号依次变大」只在单次进程生命周期内成立。启动时读最大 seq 才能真正连续。

func TestMaxSeqEmptyFileReturnsZero(t *testing.T) {
	// 文件还不存在（首次启动）时必须返回 0，而不是报错或负数 ——
	// 否则播种会把计数器设成负数，序号从 -1 开始增长。
	s, err := OpenSink(filepath.Join(t.TempDir(), "nope.jsonl"), 7, 0)
	if err != nil {
		t.Fatalf("OpenSink: %v", err)
	}
	defer s.Close()

	got, err := s.MaxSeq()
	if err != nil {
		t.Fatalf("MaxSeq: %v", err)
	}
	if got != 0 {
		t.Errorf("空文件 MaxSeq=%d，期望 0", got)
	}
}

func TestMaxSeqReturnsLargest(t *testing.T) {
	s, err := OpenSink(filepath.Join(t.TempDir(), "r.jsonl"), 7, 0)
	if err != nil {
		t.Fatalf("OpenSink: %v", err)
	}
	defer s.Close()

	now := time.Now()
	for _, seq := range []int64{1, 2, 3, 7, 4} { // 故意乱序：取最大而非最后一条
		s.Append(Entry{Seq: seq, At: now, Model: "m", Mode: "stream", Status: 200, TotalMS: 1})
	}

	got, err := s.MaxSeq()
	if err != nil {
		t.Fatalf("MaxSeq: %v", err)
	}
	if got != 7 {
		t.Errorf("MaxSeq=%d，期望 7（取最大而不是最后一条）", got)
	}
}

// 关键回归：模拟「重启后接着写」，验证新旧 seq 不会撞号。
func TestMaxSeqContinuesAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	now := time.Now()

	s1, err := OpenSink(path, 7, 0)
	if err != nil {
		t.Fatalf("OpenSink#1: %v", err)
	}
	for _, seq := range []int64{1, 2, 3} {
		s1.Append(Entry{Seq: seq, At: now, Model: "m", Mode: "stream", Status: 200, TotalMS: 1})
	}
	s1.Close()

	// 重新打开 = 模拟进程重启
	s2, err := OpenSink(path, 7, 0)
	if err != nil {
		t.Fatalf("OpenSink#2: %v", err)
	}
	defer s2.Close()

	seed, err := s2.MaxSeq()
	if err != nil {
		t.Fatalf("MaxSeq: %v", err)
	}
	if seed != 3 {
		t.Fatalf("重开后 MaxSeq=%d，期望 3", seed)
	}

	// 播种后从 4 继续，不应与已有记录重号
	s2.Append(Entry{Seq: seed + 1, At: now.Add(time.Second), Model: "m", Mode: "stream", Status: 200, TotalMS: 1})

	recent, err := s2.LoadRecent(0)
	if err != nil {
		t.Fatalf("LoadRecent: %v", err)
	}
	if len(recent) != 4 {
		t.Fatalf("记录数=%d，期望 4", len(recent))
	}
	seen := map[int64]bool{}
	for _, e := range recent {
		if seen[e.Seq] {
			t.Errorf("seq %d 重复 —— 重启后序号没有接续", e.Seq)
		}
		seen[e.Seq] = true
	}
	if !seen[4] {
		t.Errorf("未写入接续的 seq=4，实际=%v", seen)
	}
}
