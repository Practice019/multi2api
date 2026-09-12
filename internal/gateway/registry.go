package gateway

import (
	"fmt"
	"sort"
	"sync"
)

// Registry 已注册上游的集合。核心只通过它按 ID 取 Provider。
//
// 线程安全：注册发生在启动期，读取发生在请求路径，用 RWMutex 让读无争用。
type Registry struct {
	mu    sync.RWMutex
	byID  map[string]Provider
	order []string // 注册顺序，给"默认上游"回落用
}

// NewRegistry 建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]Provider)}
}

// Register 注册一个上游。
//
// 拒绝的情况（都有测试）：
//   - p 为 nil
//   - ID() 为空或格式非法（含 '/'、大写、空白等会破坏模型前缀解析的字符）
//   - ID 已被占用
//
// **重复注册直接报错而不是覆盖** —— 两个上游抢同一个 ID 时，
// 静默覆盖会让其中一个永远用不上且无人察觉。
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return fmt.Errorf("gateway: 不能注册 nil Provider")
	}
	id := p.ID()
	if !validProviderID(id) {
		return fmt.Errorf("gateway: Provider ID %q 非法（要求 ^[a-z][a-z0-9-]*$，"+
			"因为要作为模型名前缀出现在 \"%s/model\" 里）", id, id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byID[id]; dup {
		return fmt.Errorf("gateway: Provider ID %q 已被注册（重复注册会静默覆盖，故拒绝）", id)
	}
	r.byID[id] = p
	r.order = append(r.order, id)
	return nil
}

// Get 按 ID 取 Provider。
func (r *Registry) Get(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byID[id]
	return p, ok
}

// IDs 返回全部 ID（**字典序**，顺序稳定便于测试与展示）。
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// All 返回全部 Provider（按 ID 字典序，顺序稳定）。
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Provider, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.byID[id])
	}
	return out
}

// Len 已注册上游数量。
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// First 返回注册顺序里的第一个 ID。用于 default_provider 缺省时的回落。
//
// 为什么用注册顺序而不是字典序：注册顺序由 cmd/server 决定，是可预期的；
// 字典序会让"谁当默认"取决于字母表，改个名字就变了默认上游 —— 太脆。
func (r *Registry) First() (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) == 0 {
		return "", false
	}
	return r.order[0], true
}
