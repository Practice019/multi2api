// model_prefix.go 「严格前缀模式」的热状态：开了之后裸模型名一律拒绝。
//
// # 它解决什么
//
// 用户的原话：「调用的模型必须有上游的前缀」。
//
// 背景：model_check.go 已经拦掉**未知名**（不在目录里的裸名 ⇒ 400），
// 但对"裸名且确实在默认上游目录里"是**放行**的（`auto` 这类 legacy 客户端写法）。
// 严格模式把这条变成可执行的规则：只要没前缀就 400 —— 除非那个名字是用户**显式配过**的
// 别名/映射（见 model_check.go 里那条放行分支的注释：显式配置的不该被严格模式打死）。
//
// # 为什么是独立对象而不是一个 bool 字段
//
// 与 prompt.ArmorState 同一条纪律（那里踩过一次）：
// Handler 的 Config 是**启动时拍平**的值，设置页改完要**立即生效** ⇒ 必须让请求路径
// 每次去读一个**共享可变对象**，而不是读启动时复制进去的 bool。
// 各建一份的后果是"界面显示已开启、行为没变"——最难查的那种。
package server

import "sync"

// ModelPrefixPolicy 严格前缀策略（并发安全，热改）。
type ModelPrefixPolicy struct {
	mu     sync.RWMutex
	strict bool
}

// NewModelPrefixPolicy 用启动配置构造（默认 false = 保持既有行为）。
func NewModelPrefixPolicy(strict bool) *ModelPrefixPolicy {
	return &ModelPrefixPolicy{strict: strict}
}

// Strict 当前是否开启严格前缀模式（请求路径每次读它）。
func (p *ModelPrefixPolicy) Strict() bool {
	if p == nil {
		return false // nil = 未接线 ⇒ 不改变既有行为
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strict
}

// SetStrict 热改（设置页调用；下一次请求即生效）。
func (p *ModelPrefixPolicy) SetStrict(v bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.strict = v
	p.mu.Unlock()
}
