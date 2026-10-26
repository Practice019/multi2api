package logbuf

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingWriter 记录收到的每一条（并发安全）。
type recordingWriter struct {
	mu      sync.Mutex
	got     []Entry
	err     error
	release chan struct{} // 非 nil 时阻塞，直到关闭（测"不阻塞写路径"）
}

func (w *recordingWriter) WriteLog(e Entry) error {
	if w.release != nil {
		<-w.release
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, e)
	return w.err
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.got)
}

func (w *recordingWriter) seqs() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]int64, 0, len(w.got))
	for _, e := range w.got {
		out = append(out, e.Seq)
	}
	return out
}

// 正常路径：每条 Push 最终都落库，且顺序与 seq 一致（队列是 FIFO、单消费者）。
func TestSQLDoubleWriteDeliversEveryEntry(t *testing.T) {
	r := New(64)
	w := &recordingWriter{}
	r.SetSQLWriter(w)
	defer r.StopSQL()

	const n = 50
	for i := 0; i < n; i++ {
		r.Push(Entry{Model: "m", Status: 200})
	}
	r.FlushSQL()

	if got := w.count(); got != n {
		t.Fatalf("应双写 %d 条，实际 %d 条", n, got)
	}
	if st := r.SQLStats(); st.Written != n || st.Failed != 0 || st.Dropped != 0 {
		t.Errorf("计数应为 written=%d failed=0 dropped=0，得到 %+v", n, st)
	}
	for i, seq := range w.seqs() {
		if seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq 应为 %d（FIFO），得到 %d", i, i+1, seq)
		}
	}
}

// 第 1 条硬约束：写路径**不阻塞**。落库方卡住时 Push 仍要立刻返回。
func TestSQLDoubleWriteDoesNotBlockPush(t *testing.T) {
	r := New(64)
	gate := make(chan struct{})
	w := &recordingWriter{release: gate}
	r.SetSQLWriter(w)
	defer r.StopSQL()

	const n = 200
	start := time.Now()
	for i := 0; i < n; i++ {
		r.Push(Entry{Status: 200})
	}
	elapsed := time.Since(start)

	close(gate)
	r.FlushSQL()

	if elapsed > time.Second {
		t.Errorf("落库方卡住时 %d 次 Push 花了 %v —— 写路径被落库拖住了", n, elapsed)
	}
	if got := w.count(); got != n {
		t.Errorf("放行后应全部落库 %d 条，实际 %d 条（stats=%+v）", n, got, r.SQLStats())
	}
}

// 第 3 条硬约束：单条写失败只计数 + 记日志，不重试、不中断队列。
func TestSQLWriteFailureIsIsolatedAndCounted(t *testing.T) {
	r := New(64)
	w := &recordingWriter{err: errors.New("mock: database is locked")}
	r.SetSQLWriter(w)
	defer r.StopSQL()

	const n = 20
	for i := 0; i < n; i++ {
		if seq := r.Push(Entry{Status: 500}); seq != int64(i+1) {
			t.Fatalf("写库失败不该影响 Push 的返回值：第 %d 次得到 seq=%d", i, seq)
		}
	}
	r.FlushSQL()

	st := r.SQLStats()
	if st.Failed != n || st.Written != 0 {
		t.Errorf("应 failed=%d written=0，得到 %+v", n, st)
	}
	if got := w.count(); got != n {
		t.Errorf("失败也要逐条尝试过，实际尝试 %d 条", got)
	}
}

// 第 2 条硬约束：队列有界，满了丢日志（并计数），绝不阻塞请求。
// 不变量：写入 + 丢弃 == 推入总数（一条都不会凭空消失）。
func TestSQLQueueOverflowDropsAndCounts(t *testing.T) {
	r := New(64)
	gate := make(chan struct{})
	w := &recordingWriter{release: gate}
	r.SetSQLWriter(w)
	defer r.StopSQL()

	total := DefaultSQLQueue + 200
	start := time.Now()
	for i := 0; i < total; i++ {
		r.Push(Entry{Status: 200})
	}
	elapsed := time.Since(start)

	close(gate)
	r.FlushSQL()

	st := r.SQLStats()
	if elapsed > 3*time.Second {
		t.Errorf("队列写满时 %d 次 Push 花了 %v —— 队列没有兜住背压", total, elapsed)
	}
	if st.Dropped == 0 {
		t.Errorf("队列容量 %d、推入 %d 条且消费者被卡住，应该有丢弃；得到 %+v", DefaultSQLQueue, total, st)
	}
	if st.Written+st.Dropped != uint64(total) {
		t.Errorf("不变量被破坏：written(%d)+dropped(%d) != 推入(%d)", st.Written, st.Dropped, total)
	}
}

// 未挂落库目标时行为与改动前完全一致（零回归），且摘掉后不再投递。
func TestSQLWriterNilIsNoOpAndStopStopsDelivery(t *testing.T) {
	r := New(8)
	r.Push(Entry{Status: 200})
	r.FlushSQL() // 无 writer：不应 panic、不应阻塞
	if st := r.SQLStats(); st != (SQLStats{}) {
		t.Errorf("未挂落库目标时计数应为零值，得到 %+v", st)
	}
	if r.SQLWriter() != nil {
		t.Error("未挂落库目标时 SQLWriter() 应为 nil")
	}

	w := &recordingWriter{}
	r.SetSQLWriter(w)
	if r.SQLWriter() == nil {
		t.Fatal("挂上之后 SQLWriter() 不该是 nil")
	}
	r.Push(Entry{Status: 201})
	r.FlushSQL()
	if got := w.count(); got != 1 {
		t.Fatalf("挂上之后应落 1 条，实际 %d", got)
	}

	r.StopSQL()
	r.StopSQL() // 幂等
	r.Push(Entry{Status: 202})
	r.FlushSQL()
	if got := w.count(); got != 1 {
		t.Errorf("StopSQL 之后不该再投递，实际共 %d 条", got)
	}
	if r.SQLWriter() != nil {
		t.Error("StopSQL 之后 SQLWriter() 应回到 nil")
	}
}

// SetSQLWriter(nil) 等价于 StopSQL（摘掉目标）。
func TestSetSQLWriterNilDetaches(t *testing.T) {
	r := New(8)
	w := &recordingWriter{}
	r.SetSQLWriter(w)
	r.Push(Entry{Status: 200})
	r.FlushSQL()

	r.SetSQLWriter(nil)
	r.Push(Entry{Status: 200})
	r.FlushSQL()

	if got := w.count(); got != 1 {
		t.Errorf("摘掉目标后不该再投递，实际 %d 条", got)
	}
}
