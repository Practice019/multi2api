// Package checkinlog 签到/保活/旅行结果的持久化历史。
//
// 原始项目跑完定时任务只打日志，进程重启即丢，控制台没有「今天签到成功了吗」可看。
// 这里把每条结果追加到 data/checkin-log.json，并按保留天数裁剪。
//
// 写路径特征：低频（每次签到每账号一条）、可容忍同步 IO，因此直接整体重写文件
// 反而比追加式更安全——避免崩溃留下半行 JSON 导致整个历史不可读。
package checkinlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind 任务类型。
const (
	KindCheckin   = "checkin"
	KindKeepalive = "keepalive"
	KindTravel    = "travel"
	KindCredits   = "credits"
	// KindGrowth 成长中心动作（领任务奖励 / 连登兑换 / 补签 / 开盲盒 / 抽奖）。
	// 与 KindTravel 分开记：两者奖励来源与频率完全不同，混在一起没法按类型统计。
	KindGrowth = "growth"
)

// Status 结果状态。
const (
	StatusOK      = "ok"
	StatusAlready = "already" // 今日已签到
	StatusFail    = "fail"
	StatusSkip    = "skip"
)

// Record 一条任务结果。
type Record struct {
	At       time.Time `json:"at"`
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Kind     string    `json:"kind"`
	Status   string    `json:"status"`
	Detail   string    `json:"detail,omitempty"`
	Credits  int64     `json:"credits,omitempty"`
	Trigger  string    `json:"trigger,omitempty"` // manual | schedule
}

// Log 历史日志（内存 + 落盘）。
type Log struct {
	mu       sync.Mutex
	path     string
	keepDays int
	limit    int // 内存中最多保留条数（防无限增长）
	records  []Record
	dirty    bool
}

// New 构造并加载既有历史。keepDays<=0 取 30。
func New(path string, keepDays int) *Log {
	if keepDays <= 0 {
		keepDays = 30
	}
	l := &Log{path: path, keepDays: keepDays, limit: 5000}
	l.load()
	return l
}

func (l *Log) load() {
	raw, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var recs []Record
	if err := json.Unmarshal(raw, &recs); err != nil {
		return
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].At.Before(recs[j].At) })
	l.records = l.pruneLocked(recs)
}

// pruneLocked 去掉超期与超量的记录（保留最近的 limit 条）。调用方需持有 l.mu。
func (l *Log) pruneLocked(recs []Record) []Record {
	cut := time.Now().AddDate(0, 0, -l.keepDays)
	out := recs[:0]
	for _, r := range recs {
		if r.At.After(cut) {
			out = append(out, r)
		}
	}
	if len(out) > l.limit {
		out = out[len(out)-l.limit:]
	}
	return out
}

// Append 追加一条并落盘。落盘失败不返回错误给调用方（签到流程不应因日志写失败而中断），
// 但会在内存里标记 dirty，下次成功时一并补写。
func (l *Log) Append(r Record) {
	if r.At.IsZero() {
		r.At = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	l.records = l.pruneLocked(l.records)
	l.dirty = true
	l.saveLocked()
}

func (l *Log) saveLocked() {
	if !l.dirty {
		return
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return
	}
	raw, err := json.MarshalIndent(l.records, "", "  ")
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return
	}
	l.dirty = false
}

// SetKeepDays 运行时调整保留天数（<=0 忽略）；立即按新窗口裁剪一次。
func (l *Log) SetKeepDays(days int) {
	if days <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keepDays = days
	l.records = l.pruneLocked(l.records)
	l.dirty = true
	l.saveLocked()
}

// KeepDays 返回当前保留天数。
func (l *Log) KeepDays() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.keepDays
}

// DefaultPageSize 是任务历史等列表的默认每页条数。
// 放在后端而不是前端常量：分页语义（offset/limit/total）由后端定义，
// 默认值跟着语义走，避免前后端各写一个 30 慢慢漂移。
const DefaultPageSize = 30

// MaxPageSize 单页条数上限，与 logbuf.MaxPageSize 保持一致。
// 前端的每页条数是可输入的数字框，但接口不能依赖前端自觉 —— 上界在服务端兜住。
const MaxPageSize = 300

// Page 按 offset/limit 返回一页记录（时间倒序，最新在前），并给出过滤后的总数。
//
// 与 Recent 的区别：Recent 只回答「最近 n 条」，没有 offset，因而无法翻页。
// 历史保留 30 天、条数可能上千，必须由后端分页，而不是把全量发给前端再切。
//
// 边界：offset<0 视为 0；limit<=0 用 DefaultPageSize；limit>MaxPageSize 夹到上限；
// offset 越界返回空页不报错。
//
// # 为什么不带"上游归属过滤"参数（T4 的决定）
//
// 本包是历史文件的读写层，**不认识上游** —— 它不持有账号池，也没有 provider 字段。
// 若在这里加 `uids []string`（或 provider 名 + 反查池子），这个纯存储包就要
// 反向依赖账号池，而"一条记录归谁"的判据会分裂成两份（池子一份、这里一份）。
// 判据分散正是本项目反复出问题的地方（见 workbuddy.ownAccounts 的注释）。
//
// 所以出口过滤放在**端点层**（workbuddy.AdminHandler.History）：
// 那里本来就持有 Provider，能拿到与本上游其它路径**同一份**归属判据。
// 分页语义为它保留的接口就是 PageAll —— 见那个函数的注释。
func (l *Log) Page(offset, limit int, kind string) ([]Record, int) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	// l.records 是按追加顺序（时间正序）存的，倒着走就是最新在前。
	filtered := make([]Record, 0, len(l.records))
	for i := len(l.records) - 1; i >= 0; i-- {
		if kind != "" && l.records[i].Kind != kind {
			continue
		}
		filtered = append(filtered, l.records[i])
	}
	total := len(filtered)
	if offset >= total {
		return []Record{}, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]Record, end-offset)
	copy(out, filtered[offset:end])
	return out, total
}

// PageAll 返回**全部**记录（时间倒序，最新在前）的一个快照。
//
// # 为什么需要它（而不是让调用方自己拼 limit=MaxPageSize 翻页）
//
// 出口过滤必须在**过滤之后**分页，否则 total 会算成"过滤前"的数，
// 前端据此算出的页数会指向不存在的页（最后一页空、页码跳）。
// 而 Page 的过滤（kind）发生在它内部，外部拿不到"过滤后的全集"。
//
// 于是端点层的正确顺序是：
//
//	PageAll() → 按上游归属过滤 → kind 过滤 → 切 offset/limit
//
// 中间两步都在内存里做。为什么会这么设计而不是给 Page 加参数：
// 归属判据属于 workbuddy（只有它知道"本上游是谁"），不属于这个存储包 ——
// 见 Page 的注释。数据量上界是本包自己的 limit（5000），
// 复制一次切片完全可接受；相比把判据搬进来，这个代价更小。
//
// 返回的是**拷贝**：调用方在锁外过滤时，Append 可能正在往 l.records 追加
// （append 在容量够时原地写底层数组），直接返回内部切片会读到写一半的状态。
func (l *Log) PageAll() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, len(l.records))
	copy(out, l.records)
	return out
}

// Recent 返回最近 n 条（时间倒序，最新在前）；n<=0 返回全部。
func (l *Log) Recent(n int) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, len(l.records))
	for i := len(l.records) - 1; i >= 0; i-- {
		out = append(out, l.records[i])
		if n > 0 && len(out) >= n {
			break
		}
	}
	return out
}

// Since 返回 at 之后的记录（时间正序），用于「今日结果」。
func (l *Log) Since(at time.Time) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, 16)
	for _, r := range l.records {
		if !r.At.Before(at) {
			out = append(out, r)
		}
	}
	return out
}

// LastByUID 返回某账号 + 某类型最近一次结果（ok=false 表示没有记录）。
// 供「今日是否已签到」判定。
func (l *Log) LastByUID(uid, kind string) (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.records) - 1; i >= 0; i-- {
		if l.records[i].UID == uid && l.records[i].Kind == kind {
			return l.records[i], true
		}
	}
	return Record{}, false
}

// TodayStart 返回本地时区今天 00:00。
func TodayStart() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// NormalizeUID 统一 uid 形态（去空白），避免同账号两种写法产生两份历史。
func NormalizeUID(uid string) string { return strings.TrimSpace(uid) }
