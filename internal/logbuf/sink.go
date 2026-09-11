// sink.go 请求日志落盘：JSON Lines 追加 + 按天数/体积裁剪。
//
// 为什么选 JSON Lines 而不是整体重写 JSON 数组：
// 请求日志是高频追加（每个 chat 请求一条），整体重写会在每次请求时产生
// 「读全量 → 序列化 → 写临时 → rename」的开销，且并发下容易丢数据。
// 追加式只在末尾写一行，天然并发安全（配合单写者锁），崩溃最多丢最后一行。
//
// 裁剪（Prune）是低频动作：启动时一次 + 体积超阈值时一次，重写整个文件。
package logbuf

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 落盘默认值。
const (
	DefaultKeepDays = 7
	DefaultMaxBytes = 8 << 20 // 8 MiB：约 4~6 万条，足够回溯一周的普通用量
)

// Sink 请求日志的磁盘落点。
type Sink struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	keepDays int
	maxBytes int64
	size     int64
}

// OpenSink 打开（或创建）日志文件。keepDays<=0 取 7 天；maxBytes<=0 取 8 MiB。
func OpenSink(path string, keepDays int, maxBytes int64) (*Sink, error) {
	if keepDays <= 0 {
		keepDays = DefaultKeepDays
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Sink{path: path, keepDays: keepDays, maxBytes: maxBytes}
	if fi, err := os.Stat(path); err == nil {
		s.size = fi.Size()
	}
	if err := s.openLocked(); err != nil {
		return nil, err
	}
	s.Prune()
	return s, nil
}

func (s *Sink) openLocked() error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s.f = f
	return nil
}

// Append 追加一条（JSON Lines）。落盘失败不向上抛：日志写失败不该影响请求本身，
// 内存环形缓冲里那条仍然在，UI 的实时视图不受影响。
func (s *Sink) Append(e Entry) {
	if s == nil {
		return
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return
	}
	n, err := s.f.Write(append(raw, '\n'))
	if err != nil {
		return
	}
	s.size += int64(n)
	// 超过体积上限 → 异步式裁剪（仍在本锁内，但只在跨阈值那一刻发生一次）。
	if s.size > s.maxBytes {
		s.pruneLocked()
	}
}

// SetKeepDays 运行时调整保留天数，并立即按新窗口裁剪一次
// （否则调小保留天数后旧记录要等到下次体积超限才消失，界面上看不出变化）。
func (s *Sink) SetKeepDays(days int) {
	if s == nil || days <= 0 {
		return
	}
	s.mu.Lock()
	s.keepDays = days
	s.pruneLocked()
	s.mu.Unlock()
}

// SetMaxBytes 运行时调整体积上限。
func (s *Sink) SetMaxBytes(n int64) {
	if s == nil || n <= 0 {
		return
	}
	s.mu.Lock()
	s.maxBytes = n
	s.mu.Unlock()
}

// Prune 按保留天数与体积上限裁剪文件。
func (s *Sink) Prune() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
}

func (s *Sink) pruneLocked() {
	cut := time.Now().AddDate(0, 0, -s.keepDays)
	entries, err := s.readAllLocked()
	if err != nil {
		return
	}
	kept := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.At.After(cut) {
			kept = append(kept, e)
		}
	}
	// 按天裁剪后仍超体积：从最旧的开始丢，保证文件不会无限增长。
	if approxSize(kept) > s.maxBytes {
		for len(kept) > 0 && approxSize(kept) > s.maxBytes {
			kept = kept[len(kept)/10+1:] // 每次丢 10%，避免逐条 O(n²)
		}
	}
	if err := s.rewriteLocked(kept); err != nil {
		return
	}
	s.size = approxSize(kept)
}

func approxSize(es []Entry) int64 {
	var n int64
	for _, e := range es {
		n += int64(len(e.Model) + len(e.UID) + len(e.Mode) + 96)
	}
	return n
}

// rewriteLocked 用 kept 重写文件（tmp + rename，保持原子性）。
func (s *Sink) rewriteLocked(kept []Entry) error {
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		_ = s.openLocked()
		return err
	}
	w := bufio.NewWriterSize(f, 256<<10)
	for _, e := range kept {
		raw, err := json.Marshal(e)
		if err != nil {
			continue
		}
		_, _ = w.Write(append(raw, '\n'))
	}
	_ = w.Flush()
	_ = f.Close()
	if err := os.Rename(tmp, s.path); err != nil {
		_ = s.openLocked()
		return err
	}
	return s.openLocked()
}

func (s *Sink) readAllLocked() ([]Entry, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// DefaultPageSize 与 checkinlog.DefaultPageSize 保持一致：日志类列表统一每页 30 条。
// 两处各定义一份是刻意的 —— logbuf 不应反向依赖 checkinlog（那是业务历史包），
// 但值必须相同，否则「统一的每页条数」就名不副实。
const DefaultPageSize = 30

// MaxPageSize 单页条数上限，与 checkinlog.MaxPageSize 保持一致。
//
// 为什么需要它：Page 的实现是「整体读出文件再切片」，limit 会直接决定这次读的规模。
// 前端虽然是数字框且有 300 的夹紧，但 HTTP 接口对谁都开放（curl 一个 limit=1e9 就来），
// 所以上界必须由服务端自己兜住，不能只靠前端。
const MaxPageSize = 300

// Page 按 offset/limit 返回一页记录（时间倒序，最新在前），并返回文件内总条数。
//
// 与 checkinlog.Log.Page 同一套契约，便于前端用同一个分页组件接两个数据源。
// 实现上先整体读出再切片：文件已由 maxBytes(8MiB) + keepDays(7) 双重约束，
// 规模可控；若将来上界大幅放宽，这里需要改成倒序游标读取。
func (s *Sink) Page(offset, limit int) ([]Entry, int, error) {
	if s == nil {
		return nil, 0, nil
	}
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	all, err := s.readAllLocked()
	if err != nil {
		return nil, 0, err
	}
	total := len(all)
	if offset >= total {
		return []Entry{}, total, nil
	}
	// all 是追加顺序（时间正序），从尾部往前取即「最新在前」。
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]Entry, 0, end-offset)
	for i := offset; i < end; i++ {
		out = append(out, all[total-1-i])
	}
	return out, total, nil
}

// LoadRecent 返回最近 n 条（时间倒序，最新在前）。
// n<=0 时返回全部。用于「历史」视图。
func (s *Sink) LoadRecent(n int) ([]Entry, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAllLocked()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if n > 0 && len(out) >= n {
			break
		}
		out = append(out, all[i])
	}
	return out, nil
}

// MaxSeq 返回落盘日志中的最大序号（文件为空或不存在时返回 0）。
//
// 用途：请求序号计数器是进程级的，重启会从 1 重来，于是同一个落盘文件里会出现
// 重复 seq，「序号依次变大」只在单次进程生命周期内成立。启动时用它播种计数器，
// 就能让序号跨重启继续增长。
//
// 取「最大」而不是「最后一条」：文件可能被外部追加过或存在乱序，
// 扫一遍求最大值更稳，代价也只是启动时一次顺序读（受 maxBytes 上界约束）。
func (s *Sink) MaxSeq() (int64, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.readAllLocked()
	if err != nil {
		return 0, err
	}
	var max int64
	for _, e := range all {
		if e.Seq > max {
			max = e.Seq
		}
	}
	return max, nil
}

// Stats 返回落盘日志的概况（条数、字节、时间范围），供设置页展示。
func (s *Sink) Stats() map[string]any {
	if s == nil {
		return map[string]any{"enabled": false}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, _ := s.readAllLocked()
	out := map[string]any{
		"enabled":   true,
		"path":      s.path,
		"keep_days": s.keepDays,
		"max_bytes": s.maxBytes,
		"size":      s.size,
		"count":     len(all),
	}
	if len(all) > 0 {
		out["from"] = all[0].At
		out["to"] = all[len(all)-1].At
	}
	return out
}

// Close 关闭文件句柄。
func (s *Sink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
