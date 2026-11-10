// reqdump.go 失败请求的**完整请求体**落盘（排障专用，不进请求日志）。
//
// # 为什么需要它（用户实测的一个死结）
//
// 用户报「手动调用模型全是 200，但 DSH 调用就是 403」。为了查这个，日志已经补了两层：
//
//	upstream_err —— 上游说了什么（错误原文）
//	upstream_req —— 我们发了什么（**截断到 8 KiB** 的头尾）
//
// 于是排障卡在一个具体的死结上：**截断丢掉的那部分恰好可能是触发点**。
// 那一轮我把 body 的各种组合逐项直连复测（tools / max_completion_tokens=384000 /
// armor 2 万字 system / im_start 标记 / 各种 UA / 到 2.4 MB 的 body / 并发）
// **全部 200** ⇒ 说明差异在某个我还没构造出来的组合上，而它就在被截掉的那 8 KB 之外。
//
// 所以需要一个"**完整** body"的落点。但它绝不能进请求日志：
//
//	· body 可达数百 KB（整段对话），进 JSONL 会让日志体积翻几十倍；
//	· 内存环形缓冲（/admin/logs 的数据源）也会被它撑爆。
//
// 因此单独成文件：一次失败一个文件，按天数 + 总配额双重裁剪。
//
// # 边界（安全）
//
//	· 只在**非 2xx** 时写（成功请求不写：正常业务流量，无排障价值）
//	· 写入前过 gateway.MaskErrorText（与日志同一脱敏口径：不带 host/IP/key）
//	· 目录 0700、文件 0600（含用户对话内容，不能给别人读）
//	· 按天保留 + 总配额上限（超限先删最旧的，不是停止写入 —— 停止写入会让
//	  "最新的那次失败"恰好丢失，那是最不该丢的）
package logbuf

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ReqDump 失败请求体的落盘器。零值不可用，必须经 NewReqDump 构造。
//
// nil 接收者安全：所有方法在 nil 上都是 no-op —— "不落盘"是合法部署形态
// （例如磁盘紧张时），调用方不必到处判空。
type ReqDump struct {
	mu sync.Mutex
	// dir 落盘目录。空 = 未启用（no-op）。
	dir string
	// maxFiles 最多保留多少个文件（按 mtime 删最旧）。
	maxFiles int
	// maxBytes 目录总字节上限（超限继续删最旧）。
	maxBytes int64
	// written 已写文件数（用于低频触发裁剪，避免每次失败都扫目录）。
	written int
}

// 落盘默认值。
const (
	// DefaultReqDumpFiles 最多保留 200 次失败请求。
	//
	// 取值理由：排障通常只需要"最近几次"，而 200 个 × 典型 20 KB ≈ 4 MB，
	// 对磁盘无感；同时够覆盖"复现失败 → 回来查"的时间窗。
	DefaultReqDumpFiles = 200
	// DefaultReqDumpBytes 目录总上限 32 MiB。
	//
	// 单条最大 8 MiB（= server.max_body_mb 之外的另一重保护）：
	// 超大 body 只写前 8 MiB + 截断标记，避免一次写入吃掉整个配额。
	DefaultReqDumpBytes = 32 << 20
	// ReqDumpMaxBody 单个文件最多写多少字节。
	ReqDumpMaxBody = 8 << 20
)

// NewReqDump 构造落盘器。dir 为空 ⇒ 返回 nil（调用方不必判空，nil 是 no-op）。
//
// 为什么"空目录返回 nil"而不是"返回一个禁用的实例"：
// 装配层用 `if cfg.ReqDumpDir != ""` 表达"要不要这个功能"，
// 返回 nil 让调用链上的判空成本降到零（方法集全部 nil-safe）。
func NewReqDump(dir string, maxFiles int, maxBytes int64) (*ReqDump, error) {
	if dir == "" {
		return nil, nil
	}
	if maxFiles <= 0 {
		maxFiles = DefaultReqDumpFiles
	}
	if maxBytes <= 0 {
		maxBytes = DefaultReqDumpBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	d := &ReqDump{dir: dir, maxFiles: maxFiles, maxBytes: maxBytes}
	d.pruneLocked() // 启动即裁一次：上次运行可能留下超量文件
	return d, nil
}

// Enabled 报告是否真的会落盘（nil / 空目录 ⇒ false）。
func (d *ReqDump) Enabled() bool { return d != nil && d.dir != "" }

// Write 落一份失败请求体。meta 里的键值会写成文件头注释行（便于对照）。
//
// 文件名：<时间戳>-<模型/上游短哈希>.txt
//
//	为什么带时间戳在前：ls 按名字排序即按时间排序，找"最近一次"不用再看 mtime。
//	为什么带哈希：同一秒可能有多个失败（并发），哈希让名字不冲突且能看出"是不是同一个模型"。
//
// 返回落盘路径（未启用时返回空串，便于调用方决定要不要记日志）。
func (d *ReqDump) Write(model, provider, status string, meta map[string]string, body []byte) string {
	if !d.Enabled() {
		return ""
	}
	// 脱敏在调用方做（那一层才有 gateway 依赖，见 server 侧的调用点）。
	// 这里只做长度保护。
	truncated := false
	if len(body) > ReqDumpMaxBody {
		body = body[:ReqDumpMaxBody]
		truncated = true
	}

	now := time.Now()
	h := sha1.Sum([]byte(model + "\x00" + provider))
	name := fmt.Sprintf("%s-%s.txt", now.Format("20060102-150405.000"), hex.EncodeToString(h[:4]))
	path := filepath.Join(d.dir, name)

	var b []byte
	b = append(b, []byte("# 失败请求体（排障用；已脱敏）\n")...)
	b = append(b, []byte("# at="+now.Format(time.RFC3339Nano)+"\n")...)
	b = append(b, []byte("# model="+model+"  provider="+provider+"  status="+status+"\n")...)
	// meta 排序输出：同一份失败两次，diff 出来应该只有时间差。
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b = append(b, []byte("# "+k+"="+meta[k]+"\n")...)
	}
	if truncated {
		b = append(b, []byte(fmt.Sprintf("# ⚠ 已截断：只保留前 %d 字节\n", ReqDumpMaxBody))...)
	}
	b = append(b, []byte("# ---- body ----\n")...)
	b = append(b, body...)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		b = append(b, '\n')
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// 0600：含用户对话内容，不能给同机其他用户读。
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return ""
	}
	d.written++
	// 低频裁剪：每写 20 个才扫一次目录（扫目录是 O(n) 系统调用，
	// 每次失败都扫会让排障本身变成负担）。
	if d.written%20 == 0 {
		d.pruneLocked()
	}
	return path
}

// Prune 手动触发一次裁剪（测试与运维用）。
func (d *ReqDump) Prune() {
	if !d.Enabled() {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pruneLocked()
}

// pruneLocked 按 (数量, 总字节) 双上限裁掉最旧的文件。调用方持锁。
func (d *ReqDump) pruneLocked() {
	ents, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	type fi struct {
		path string
		mod  time.Time
		size int64
	}
	files := make([]fi, 0, len(ents))
	var total int64
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fi{filepath.Join(d.dir, e.Name()), info.ModTime(), info.Size()})
		total += info.Size()
	}
	if len(files) <= d.maxFiles && total <= d.maxBytes {
		return
	}
	// 最旧的先删。
	//
	// ⚠ 用**索引**而不是 `for _, f := range files` + `files = files[1:]`：
	//   range 在循环开始时就把切片头复制好了，循环体里改 files 不会影响
	//   range 的推进 —— 那种写法会边删边跳过元素，也可能重复删同一个文件。
	n := len(files)
	for i := 0; i < n; i++ {
		if n-i <= d.maxFiles && total <= d.maxBytes {
			break
		}
		if err := os.Remove(files[i].path); err == nil {
			total -= files[i].size
		}
	}
}
