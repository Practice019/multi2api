// localfile.go 粘性绑定的**本地文件兜底**（2026-10-20，issue #2 观察①）
//
// # 解决什么
//
// 没有 Redis（`redis_mode: noop`）时，`session.Router` 的绑定表**纯内存** ——
// 重启后所有长会话都被判"未绑定"，重新分配账号。而上游 prompt cache 按账号隔离，
// 换号 = **整段对话重新预填充**。
//
// issue #2 实测两笔：`msgs=1146` 花 5.59 credit、`msgs=753` 花 1.76 credit，
// **仅仅因为重启了一次**。正常请求是 0.1~0.3 credit。
//
// # 为什么这不是"有 Redis 就够了"
//
// Redis 是可选依赖。绝大多数自建部署（包括这一份）跑的是 `noop` ——
// 于是 session.go 顶部注释第 9 点写的"防重启丢粘性"在**默认配置下从未生效过**。
// 设计意图与默认行为背道而驰，这是本条要修的实质。
//
// # 保真策略
//
//   - **原子替换**：先写 `<path>.tmp`，再 `os.Rename` 覆盖。
//     ⚠ Windows 上 Rename 到已存在文件会失败，所以先 Remove 再 Rename。
//     这仍是"整文件替换"语义，不是"截断后重写" —— 后者中途崩溃会留下半个 JSON。
//   - **节流 500ms**：`SetBind` 每轮请求结束都调一次，每次都落盘会把盘打满。
//     攒一批写一次；最坏情况丢 500ms 内的绑定（代价是那一两个会话重新分配，
//     不是数据损坏）。
//   - **只存 key→uid**：TTL 不落盘。过期由 `session.Router` 的 lastActive 判定，
//     重启后第一次 Resolve 按既有 TTL 逻辑处理。把 TTL 也存进去会让
//     "为什么重启后立刻过期"变成更难解释的事。
package redisstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// flushInterval 批量落盘的间隔。见文件头「节流」。
const flushInterval = 500 * time.Millisecond

// LocalFile 把粘性绑定持久化到一个本地 JSON 文件。
//
// 它是 `Noop` 的替代品：接口语义完全一致，但重启后能恢复。
// 用 `NewLocalFile(path)` 构造；构造时即尝试加载，`LoadBinds` 返回加载结果。
type LocalFile struct {
	path string

	mu    sync.Mutex
	mem   map[string]string // key → uid
	state []byte            // SaveState 写入的原始字节（本项目当前不使用，保留接口语义）

	dirty  bool
	stopCh chan struct{}
	once   sync.Once
}

// NewLocalFile 构造并**立即加载**磁盘上的绑定。
//
// ⚠ 加载失败（文件不存在/损坏）不报错，返回空表：粘性是优化不是契约，
// 起步时没有绑定等价于"首次运行"，比因为这个不让网关启动好得多。
func NewLocalFile(path string) *LocalFile {
	lf := &LocalFile{
		path:   path,
		mem:    map[string]string{},
		stopCh: make(chan struct{}),
	}
	lf.load()
	go lf.flushLoop()
	return lf
}

// Close 停止后台落盘协程，并做最后一次同步落盘。
func (lf *LocalFile) Close() {
	lf.once.Do(func() { close(lf.stopCh) })
	lf.flush()
}

// SetBind 记录一条绑定（异步落盘，最多延迟 flushInterval）。
func (lf *LocalFile) SetBind(key, uid string, _ time.Duration) {
	if key == "" || uid == "" {
		return
	}
	lf.mu.Lock()
	lf.mem[key] = uid
	lf.dirty = true
	lf.mu.Unlock()
}

// DelBind 删除一条绑定。
func (lf *LocalFile) DelBind(key string) {
	lf.mu.Lock()
	if _, ok := lf.mem[key]; ok {
		delete(lf.mem, key)
		lf.dirty = true
	}
	lf.mu.Unlock()
}

// LoadBinds 返回当前绑定表（启动时 session.Router 用它恢复内存）。
func (lf *LocalFile) LoadBinds() map[string]string {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	out := make(map[string]string, len(lf.mem))
	for k, v := range lf.mem {
		out[k] = v
	}
	return out
}

// SaveState 保存任意状态字节（接口要求的通用能力，本项目当前未使用）。
func (lf *LocalFile) SaveState(b []byte) {
	lf.mu.Lock()
	lf.state = b
	lf.dirty = true
	lf.mu.Unlock()
}

// LoadState 返回 SaveState 存过的字节。
func (lf *LocalFile) LoadState() ([]byte, bool) {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if lf.state == nil {
		return nil, false
	}
	return lf.state, true
}

// ---------------------------------------------------------------- 内部

// bindingsFile 是落盘的 JSON 结构。
//
// ⚠ 带 version 字段：将来改结构时能识别旧文件并安全忽略，
// 而不是静默地把旧格式读成空表（那种 bug 极难查）。
type bindingsFile struct {
	Version int               `json:"version"`
	Binds   map[string]string `json:"binds"`
}

const bindingsVersion = 1

func (lf *LocalFile) load() {
	raw, err := os.ReadFile(lf.path)
	if err != nil {
		return // 首次运行：文件不存在，空表即可
	}
	var f bindingsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return // 损坏：当作没有，下次落盘会覆盖掉
	}
	if f.Version != bindingsVersion {
		return // 版本不认识：宁可不用，也不要按错的格式解释
	}
	lf.mu.Lock()
	for k, v := range f.Binds {
		if k != "" && v != "" {
			lf.mem[k] = v
		}
	}
	lf.mu.Unlock()
}

func (lf *LocalFile) flushLoop() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-lf.stopCh:
			return
		case <-t.C:
			lf.flush()
		}
	}
}

// flush 把内存表原子写到磁盘。没有变更时直接返回（避免空转 IO）。
func (lf *LocalFile) flush() {
	lf.mu.Lock()
	if !lf.dirty {
		lf.mu.Unlock()
		return
	}
	snapshot := make(map[string]string, len(lf.mem))
	for k, v := range lf.mem {
		snapshot[k] = v
	}
	stateCopy := lf.state
	lf.dirty = false
	lf.mu.Unlock()

	buf, err := json.Marshal(bindingsFile{Version: bindingsVersion, Binds: snapshot})
	if err != nil {
		return
	}
	_ = stateCopy // 本项目未使用 SaveState；保留快照避免将来误读脏值
	lf.writeAtomic(buf)
}

// writeAtomic 原子替换文件。见文件头「原子替换」。
func (lf *LocalFile) writeAtomic(b []byte) {
	dir := filepath.Dir(lf.path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	tmp := lf.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	// ⚠ Windows：Rename 到已存在文件会失败，先删目标再改名。
	// 这中间有个极短窗口目标不存在 —— 可接受（绑定表丢了等于重新分配，
	// 而截断后重写会留下半个 JSON，那是更糟的失败模式）。
	_ = os.Remove(lf.path)
	if err := os.Rename(tmp, lf.path); err != nil {
		_ = os.Remove(tmp)
	}
}
