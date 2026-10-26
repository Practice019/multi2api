// sqlsink.go logs 表**双写**（PLAN-hyxapi-alignment.md §4 Wave 1 · T06）。
//
// # 要解决什么
//
// 请求日志当前只落 JSON Lines（sink.go）。系统状态页的三个业务指标
// （今日请求/Token/消费、近 5 分钟 RPM/成功率/AVG 延迟）需要 SQL 聚合 ——
// 在 jsonl 上只能全量扫盘（PLAN-sql-unify.md:53 G2）。所以同一份 Entry 要
// **同时**进 jsonl 与 logs 表。
//
// # 三条硬约束（接口语义的一部分，缺一条都会把日志变成请求的负担）
//
//  1. **异步**：Push 只做一次非阻塞入队，落库在后台 goroutine 里做。
//     SQLite 慢、锁竞争、磁盘抖动都不允许传导到请求路径上。
//  2. **有界**：队列固定长度，满了**丢弃并计数**（丢日志不丢请求）。
//     无界队列会把内存换成"迟早 OOM"，那个失败更晚也更难查。
//  3. **失败隔离**：单条写失败只记一行日志 + 计数，**不重试、不中断队列**。
//     重试会在库不可用时制造写放大（每个请求都重试一次最坏的东西）。
//
// # 为什么这里只定义接口，不 import internal/store
//
// logbuf 是写路径上的基础设施（每个 chat 请求都会走），它不该知道 SQL 层。
// 真正的适配（logbuf.Entry → store.Log 的字段映射 + store.AppendLog）由装配层
// （cmd/server，见 §4 T11）注入 —— 那里同时知道两边的类型。
// 参考映射写在 sqllog_equivalence_test.go 里，T11 直接照抄那一份即可。
package logbuf

import (
	"log"
	"sync"
	"sync/atomic"
)

// SQLWriter 落库目标。WriteLog 由**后台 goroutine** 调用，因此：
//   - 它可以慢（不阻塞请求），但必须**并发安全**（队列只有一个消费者，
//     真正的并发来自换绑/关闭时的旧 worker，边界很窄）；
//   - 返回 error 表示这一条没落成，调用方只记日志 + 计数，不重试。
type SQLWriter interface {
	WriteLog(Entry) error
}

// DefaultSQLQueue 双写队列长度。
//
// 4096 条 ≈ 几分钟的突发流量：正常写入（单条 INSERT，微秒级）根本追不满；
// 能追满只有两种情况 —— 库卡住/不可用，或流量远超设计。两种都该丢日志保请求。
const DefaultSQLQueue = 4096

// SQLStats 双写计数快照。
//
// ⚠ 三个数都必须能被看见：只报 written 的话，"日志少了 300 条"永远查不出
// 是队列丢的还是写失败的（两者的处置完全不同：前者要加长队列/修慢查询，
// 后者要看 DB 错误）。
type SQLStats struct {
	// Written 成功落库的条数。
	Written uint64 `json:"written"`
	// Failed 落库返回 error 的条数（不重试）。
	Failed uint64 `json:"failed"`
	// Dropped 队列满被丢弃的条数（Push 侧计数）。
	Dropped uint64 `json:"dropped"`
	// Queued 当前仍在队列里等待的条数（近似值，读的是 len(chan)）。
	Queued int `json:"queued"`
}

// sqlJob 队列元素。done != nil 时表示这是一个**屏障**（FlushSQL 用）：
// worker 处理到它时关闭 done，于是"done 关闭"蕴含"此前所有条目都已处理完"。
type sqlJob struct {
	e    Entry
	done chan struct{}
}

// sqlSink 一个有界队列 + 一个后台消费者。
type sqlSink struct {
	w    SQLWriter
	ch   chan sqlJob
	stop chan struct{}

	stopOnce sync.Once

	written atomic.Uint64
	failed  atomic.Uint64
	dropped atomic.Uint64
}

func newSQLSink(w SQLWriter, queue int) *sqlSink {
	if queue <= 0 {
		queue = DefaultSQLQueue
	}
	s := &sqlSink{
		w:    w,
		ch:   make(chan sqlJob, queue),
		stop: make(chan struct{}),
	}
	go s.run()
	return s
}

// enqueue 非阻塞入队。队列满 ⇒ 丢弃计数，**绝不阻塞调用方**。
//
// 这里刻意不用 select + default 之外的任何花样（比如"满了就同步写一条"）：
// 同步兜底会把最坏情况的延迟直接搬回请求路径，正是本文件头第 2 条要避免的。
func (s *sqlSink) enqueue(e Entry) {
	select {
	case s.ch <- sqlJob{e: e}:
	default:
		s.dropped.Add(1)
	}
}

// run 消费者：直到 stop 关闭，再把已入队的排空（尽力而为）后退出。
func (s *sqlSink) run() {
	for {
		select {
		case j := <-s.ch:
			s.handle(j)
		case <-s.stop:
			for {
				select {
				case j := <-s.ch:
					s.handle(j)
				default:
					return
				}
			}
		}
	}
}

// handle 处理一条：成功计数，失败**只记日志 + 计数**，两种都不影响后续条目。
func (s *sqlSink) handle(j sqlJob) {
	// 屏障（FlushSQL 入队的那个）**不落库**：它只用来标记"此前的条目都已处理完"。
	// 漏掉这个分支的后果是每次 FlushSQL 都凭空多写一条零值日志（实测踩过：
	// failed=21/应 20、written 比推入数多 1）。
	if j.done != nil {
		close(j.done)
		return
	}
	if err := s.w.WriteLog(j.e); err != nil {
		s.failed.Add(1)
		log.Printf("logbuf: logs 表双写失败（已丢弃该条，不影响请求）: %v", err)
	} else {
		s.written.Add(1)
	}
	if j.done != nil {
		close(j.done)
	}
}

// flush 入队一个屏障并等它被处理 —— 返回时此前入队的条目都已处理完。
//
// 只给测试与关停用，**不在写路径上**：它会阻塞调用方，而请求路径不允许阻塞。
// 队列满时这里允许阻塞等待（flush 的调用方本来就接受等待）。
func (s *sqlSink) flush() {
	done := make(chan struct{})
	select {
	case s.ch <- sqlJob{done: done}:
	case <-s.stop:
		return // 已停：worker 不再消费，屏障永远不会被处理
	}
	<-done
}

// close 停掉消费者（幂等）。先由 run 排空已入队条目，再返回。
func (s *sqlSink) close() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *sqlSink) stats() SQLStats {
	return SQLStats{
		Written: s.written.Load(),
		Failed:  s.failed.Load(),
		Dropped: s.dropped.Load(),
		Queued:  len(s.ch),
	}
}

// ── Ring 上的公开 API ──────────────────────────────────────────────────────

// SetSQLWriter 挂上（或换掉/摘掉）落库目标。
//
//	SetSQLWriter(w)   → 启动后台消费者；此后每条 Push 都会双写
//	SetSQLWriter(nil) → 摘掉（等价于 StopSQL）
//
// 换绑时旧 worker 会被停止：它会先把已入队的条目排空再退出，**不丢已经收下的日志**。
func (r *Ring) SetSQLWriter(w SQLWriter) {
	r.mu.Lock()
	old := r.sql
	if w == nil {
		r.sql = nil
	} else {
		r.sql = newSQLSink(w, DefaultSQLQueue)
	}
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
}

// SQLWriter 返回当前落库目标（可能为 nil）。
func (r *Ring) SQLWriter() SQLWriter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sql == nil {
		return nil
	}
	return r.sql.w
}

// FlushSQL 等双写队列排空后返回（测试与关停用，见 sqlSink.flush 的说明）。
func (r *Ring) FlushSQL() {
	r.mu.Lock()
	s := r.sql
	r.mu.Unlock()
	if s != nil {
		s.flush()
	}
}

// StopSQL 停掉后台消费者并摘掉落库目标（幂等）。
func (r *Ring) StopSQL() {
	r.mu.Lock()
	s := r.sql
	r.sql = nil
	r.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// SQLStats 返回双写计数快照；未挂落库目标时返回全 0。
func (r *Ring) SQLStats() SQLStats {
	r.mu.Lock()
	s := r.sql
	r.mu.Unlock()
	if s == nil {
		return SQLStats{}
	}
	return s.stats()
}
