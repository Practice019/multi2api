// Package auditlog 事件审计落盘 —— 照 dsh-purge 的 lib/audit.js 移植。
//
// # 原实现的语义（逐条抄自 lib/audit.js）
//
//	AUDIT_FILE      "audit.jsonl"      一行一条 JSON
//	AUDIT_MAX_BYTES 1 MiB              超过就轮转
//	AUDIT_KEEP      2                  保留 .1 / .2 两份历史
//	行形状          {at, event, pid, ...data}（at 是 ISO 时间）
//	写入失败        **绝不打断主流程**；只警告一次（否则每写失败一次刷一行日志）
//
// # 为什么要单独一个包，而不是塞进请求日志
//
// 请求日志（internal/logbuf）是一张**会话表**：字段固定、被面板按列读、按天/字节裁剪。
// 审计是**事件流**：事件名 + 任意附字段，写法与保留策略都不同。
// 混进去会让两边都变形 —— 用户要的是"照它那样做"，不是"找个地方塞下"。
package auditlog

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// MaxBytes 超过它就把当前文件轮转掉（原实现 AUDIT_MAX_BYTES）。
	MaxBytes = 1024 * 1024
	// Keep 保留的历史份数（原实现 AUDIT_KEEP）。
	Keep = 2
)

// Log 一个审计文件。
//
// 零值不可用：用 Open 构造。**并发安全**（写入加锁）—— 原实现靠 O_APPEND 原子追加，
// 我们在进程内加锁更直接，也避免轮转与写入互相踩。
type Log struct {
	path string
	mu   sync.Mutex
	// warned 只警告一次（原实现 warnedAuditFailure）。
	warned bool
}

// Open 打开（不存在则首次写入时创建）。
//
// ⚠ 不在这里创建文件：原实现也是"写的时候才 open"—— 开了审计但一直没事件时，
//
//	磁盘上不该多一个空文件（那会让人以为"审计在用"）。
func Open(path string) *Log { return &Log{path: path} }

// Path 审计文件路径（供启动日志与排障）。
func (l *Log) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Log 追加一条审计记录。返回是否写入成功。
//
// ⚠ data 里**不要放密钥/凭据**（原实现的注释同样写着 "别放密钥"）。
func (l *Log) Log(event string, data map[string]any) bool {
	if l == nil || l.path == "" || event == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		l.warnOnce(err)
		return false
	}
	l.rotateLocked()

	row := map[string]any{
		"at":    time.Now().Format(time.RFC3339Nano),
		"event": event,
		"pid":   os.Getpid(),
	}
	for k, v := range data {
		if k == "at" || k == "event" || k == "pid" {
			continue // 保留字段不许被覆盖（否则时间/事件名会被附字段顶掉）
		}
		row[k] = v
	}
	line, err := json.Marshal(row)
	if err != nil {
		l.warnOnce(err)
		return false
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		l.warnOnce(err)
		return false
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		l.warnOnce(err)
		return false
	}
	return true
}

// rotateLocked 超过 MaxBytes 就轮转：base→.1、.1→.2，丢掉最旧的（原实现的 for 循环）。
func (l *Log) rotateLocked() {
	fi, err := os.Stat(l.path)
	if err != nil || fi.Size() < MaxBytes {
		return
	}
	for i := Keep; i >= 1; i-- {
		from := l.path
		if i > 1 {
			from = l.path + "." + itoa(i-1)
		}
		_ = os.Rename(from, l.path+"."+itoa(i))
	}
}

func (l *Log) warnOnce(err error) {
	if l.warned {
		return
	}
	l.warned = true
	// 只警告一次：审计写不进去不能变成刷屏源（原实现 warnedAuditFailure 同理）。
	log.Printf("auditlog: 审计写入失败（后续同类失败不再重复提示）: %v", err)
}

// itoa 小整数转字符串（避免为两位数引入 strconv 的可读性代价）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
