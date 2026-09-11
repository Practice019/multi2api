package logbuf

import (
	"path/filepath"
	"testing"
	"time"
)

// 背景（侦察结论）：请求序号其实有**两个**计数器，且都会在重启时归零：
//
//  1. server 包的 chatSeq —— 只用于 stdout 表格的 #%03d 显示；
//  2. Ring.total —— Push 时覆盖 Entry.Seq，落盘与 /admin/logs 序号列都用它。
//
// 所以要真正让「序号依次变大」跨重启成立，必须给 Ring 一个播种入口，
// 否则落盘文件里会出现重复 seq。

func TestSeedSeqContinuesNumbering(t *testing.T) {
	r := New(4)
	if got := r.Push(Entry{At: time.Now()}); got != 1 {
		t.Fatalf("首个 seq=%d，期望 1", got)
	}

	// 模拟重启：新建一个 Ring，用上次的最大 seq 播种。
	r2 := New(4)
	if err := r2.SeedSeq(10); err != nil {
		t.Fatalf("SeedSeq: %v", err)
	}
	if got := r2.Push(Entry{At: time.Now()}); got != 11 {
		t.Errorf("播种 10 之后首个 seq=%d，期望 11", got)
	}
}

func TestSeedSeqZeroIsNoop(t *testing.T) {
	// 首次启动（落盘为空）时 MaxSeq 返回 0，播种 0 必须保持从 1 开始，
	// 不能把计数器设成 0 导致首个 seq 变成 1 之后又回退。
	r := New(4)
	if err := r.SeedSeq(0); err != nil {
		t.Fatalf("SeedSeq(0): %v", err)
	}
	if got := r.Push(Entry{At: time.Now()}); got != 1 {
		t.Errorf("播种 0 后首个 seq=%d，期望 1", got)
	}
}

// 播种值比当前计数小（例如人工把日志文件截短过）时不能倒退，
// 否则会与已有记录重号。
func TestSeedSeqNeverGoesBackwards(t *testing.T) {
	r := New(4)
	r.Push(Entry{At: time.Now()}) // total=1
	r.Push(Entry{At: time.Now()}) // total=2

	if err := r.SeedSeq(1); err != nil {
		t.Fatalf("SeedSeq: %v", err)
	}
	if got := r.Push(Entry{At: time.Now()}); got != 3 {
		t.Errorf("倒退播种后 seq=%d，期望 3（不能重号）", got)
	}
}

func TestSeedSeqRejectsNegative(t *testing.T) {
	r := New(4)
	if err := r.SeedSeq(-5); err == nil {
		t.Error("负数应报错")
	}
	if got := r.Push(Entry{At: time.Now()}); got != 1 {
		t.Errorf("拒绝非法播种后 seq=%d，期望仍从 1 开始", got)
	}
}

// 端到端：落盘 + 重新打开 + 播种，验证文件里没有重复 seq。
// 这是「序号保证依次变大」的实际验收场景。
func TestRingSeedThenPersistHasNoDuplicateSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.jsonl")
	now := time.Now()

	r1 := New(4)
	s1, err := OpenSink(path, 7, 0)
	if err != nil {
		t.Fatalf("OpenSink#1: %v", err)
	}
	r1.SetSink(s1)
	r1.Push(Entry{At: now, Model: "m", Mode: "stream", Status: 200})
	r1.Push(Entry{At: now, Model: "m", Mode: "stream", Status: 200})
	s1.Close()

	// 重启：读最大 seq 播种新 Ring
	s2, err := OpenSink(path, 7, 0)
	if err != nil {
		t.Fatalf("OpenSink#2: %v", err)
	}
	defer s2.Close()
	seed, err := s2.MaxSeq()
	if err != nil {
		t.Fatalf("MaxSeq: %v", err)
	}
	r2 := New(4)
	r2.SetSink(s2)
	if err := r2.SeedSeq(seed); err != nil {
		t.Fatalf("SeedSeq: %v", err)
	}
	r2.Push(Entry{At: now.Add(time.Second), Model: "m", Mode: "stream", Status: 200})

	all, err := s2.LoadRecent(0)
	if err != nil {
		t.Fatalf("LoadRecent: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("记录数=%d，期望 3", len(all))
	}
	seen := map[int64]bool{}
	for _, e := range all {
		if seen[e.Seq] {
			t.Errorf("seq %d 重复 —— 跨重启没有接续", e.Seq)
		}
		seen[e.Seq] = true
	}
	if !seen[3] {
		t.Errorf("重启后应从 3 继续，实际序号集合=%v", seen)
	}
}
