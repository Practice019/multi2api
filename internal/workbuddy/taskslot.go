// taskslot.go 后台任务槽：同一时刻只允许一个全量任务。
//
// # 搬运说明（Task 3c）
//
// 本文件原先在 internal/admin/admin.go（类型名 taskSlot）。
// 它随签到/保活/旅行/成长这几条全量端点一起搬过来，判定逻辑逐字未改：
// 已有任务在跑时返回 false，调用方回 409 而**不是排队**
// （排队会让界面误以为立刻执行了）。
//
// 任务结果是 []map[string]any 而不是强类型：这个槽要同时装下
// 签到结果（调度器类型）、旅行/成长结果（本包类型），
// 而它们对外的 JSON 形状一致（uid/status/detail/credits）。
// 用后端 shape 中转，避免为一个纯展示结构引入第三份类型定义。
package workbuddy

import (
	"fmt"
	"sync"
	"time"
)

// TaskSlot 的默认实现。
type taskSlot struct {
	mu       sync.Mutex
	running  bool
	kind     string
	started  time.Time
	finished time.Time
	results  []map[string]any
	errMsg   string
}

// NewTaskSlot 建一个空的任务槽（可注入给 AdminEnv.TaskSlot）。
//
// 导出的唯一理由：cmd/server 需要把它交给调度器，
// 让 /admin/schedule+/admin/task 两条端点与签到/保活共用同一个槽。
func NewTaskSlot() TaskSlot { return newTaskSlot() }

func newTaskSlot() *taskSlot { return &taskSlot{} }

// Start 尝试占用任务槽并异步执行 fn（在调用方 goroutine 外另起一个）。
//
// panic 被捕获并记录成 error 字段而不是让进程崩：这些任务会打上游，
// 单次上游的意外不该带崩整个网关。
func (t *taskSlot) Start(kind string, fn func() []map[string]any) bool {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return false
	}
	t.running = true
	t.kind = kind
	t.started = time.Now()
	t.finished = time.Time{}
	t.results = nil
	t.errMsg = ""
	t.mu.Unlock()

	go func() {
		var results []map[string]any
		var errMsg string
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					errMsg = fmt.Sprintf("panic: %v", rec)
				}
			}()
			results = fn()
		}()
		t.mu.Lock()
		t.running = false
		t.finished = time.Now()
		t.results = results
		t.errMsg = errMsg
		t.mu.Unlock()
	}()
	return true
}

// Snapshot 当前任务槽状态。
//
// 字段名是**对外契约**（前端按它渲染），必须与改造前的 admin.taskSlot.snapshot 一致。
func (t *taskSlot) Snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]any{
		"running": t.running,
		"kind":    t.kind,
		"results": t.results,
	}
	if !t.started.IsZero() {
		out["started_at"] = t.started
	}
	if t.running {
		out["elapsed_sec"] = int64(time.Since(t.started).Seconds())
	} else if !t.finished.IsZero() {
		out["finished_at"] = t.finished
		out["duration_sec"] = int64(t.finished.Sub(t.started).Seconds())
		out["ok"] = sumStatus(t.results, statusOK)
		out["fail"] = sumStatus(t.results, "fail")
		out["already"] = sumStatus(t.results, "already")
		if t.errMsg != "" {
			out["error"] = t.errMsg
		}
	}
	return out
}

// sumStatus 统计结果里某个状态的条数（读的是中转后的 JSON 形状）。
func sumStatus(rs []map[string]any, want string) int {
	n := 0
	for _, r := range rs {
		if s, ok := r["status"].(string); ok && s == want {
			n++
		}
	}
	return n
}
