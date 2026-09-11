// Package logbuf 进程内请求日志环形缓冲。
//
// 动机：网关原本只把「每请求一行表格日志」printf 到 stdout，进程内没有任何留存，
// 控制台无法回看。这里把同一份数据同时写进定长环形缓冲，供 /admin/logs 增量拉取。
//
// 设计约束：写路径必须极轻（每个 chat 请求都会走），因此只做一次 mutex 保护的
// 定长切片写入，不做 IO、不做 JSON 序列化；序列化交给读侧。
package logbuf

import (
	"sync"
	"time"
)

// DefaultCapacity 默认保留条数。2000 条足以覆盖最近几小时的排障窗口。
const DefaultCapacity = 2000

// Entry 一条请求日志（字段刻意扁平，便于直接 JSON 出去）。
type Entry struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Model   string    `json:"model"`
	Mode    string    `json:"mode"`   // stream | sync
	Status  int       `json:"status"` // 对客户端返回的状态码
	UID     string    `json:"uid"`    // 完整 uid，展示层自行截断
	TTFBMS  int64     `json:"ttfb_ms"`
	Tokens  int       `json:"tokens"` // -1 表示 usage 缺失
	TotalMS int64     `json:"total_ms"`
}

// Ring 定长环形缓冲。零值不可用，必须经 New 构造。
type Ring struct {
	mu    sync.Mutex
	buf   []Entry
	next  int
	full  bool
	total int64 // 累计写入条数，同时充当递增序列号

	// sink 可选的落盘目标（nil = 只留在内存）。写盘刻意放在锁外：
	// 文件 IO 不能拖着所有请求一起等。
	sink *Sink
}

// New 构造容量为 capacity 的环形缓冲；capacity<=0 时取 DefaultCapacity。
func New(capacity int) *Ring {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Ring{buf: make([]Entry, capacity)}
}

// SetSink 挂上落盘目标（可在启动后调用）。
func (r *Ring) SetSink(s *Sink) {
	r.mu.Lock()
	r.sink = s
	r.mu.Unlock()
}

// Sink 返回当前落盘目标（可能为 nil）。
func (r *Ring) Sink() *Sink {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sink
}

// Push 写入一条；返回分配的递增序列号。
func (r *Ring) Push(e Entry) int64 {
	r.mu.Lock()
	r.total++
	e.Seq = r.total
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	sink := r.sink
	r.mu.Unlock()

	if sink != nil {
		sink.Append(e)
	}
	return e.Seq
}

// Snapshot 返回 seq > sinceSeq 的全部条目（按时间正序），以及当前最大 seq。
// sinceSeq<=0 时返回缓冲内全部。读侧轮询用 sinceSeq 做增量游标。
func (r *Ring) Snapshot(sinceSeq int64) ([]Entry, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	max := r.total
	if sinceSeq >= max {
		return nil, max
	}
	// 已滚出缓冲的最老 seq 下界：满了就是 total-cap+1，没满就是 1。
	lowest := int64(1)
	if r.full {
		lowest = r.total - int64(len(r.buf)) + 1
	}
	if sinceSeq < lowest-1 {
		sinceSeq = lowest - 1
	}

	n := int(max - sinceSeq)
	out := make([]Entry, 0, n)
	// 已写入的条目总数为 written = min(total, cap)，逻辑索引 0 对应 seq = total-written+1。
	written := r.total
	if int64(len(r.buf)) < written {
		written = int64(len(r.buf))
	}
	start := r.next - int(written)
	if start < 0 {
		start += len(r.buf)
	}
	for i := 0; i < int(written); i++ {
		e := r.buf[(start+i)%len(r.buf)]
		if e.Seq > sinceSeq {
			out = append(out, e)
		}
	}
	return out, max
}

// Len 返回当前缓冲内的条目数。
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return len(r.buf)
	}
	return r.next
}

// Cap 返回容量。
func (r *Ring) Cap() int { return len(r.buf) }
