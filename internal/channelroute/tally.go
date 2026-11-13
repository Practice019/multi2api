// tally.go 选路计数的**进程内**记账（诊断用，2026-11-13 T-909 收尾）。
//
// # 它给谁用
//
//	`Explain` 回答"这个模型会走哪些渠道"，而用户真正要看见的是"轮询**真的**均匀了"
//	（原话："我发现他调用的时候不会轮询这些渠道"）⇒ 必须有"各被选中多少次"这个数。
//
// # 三条纪律
//
//  1. **进程内、重启清零**（主脑裁定）：这是排障视图，不是统计系统 ⇒ **不落盘**；
//  2. **只读接口不许改选路状态**：`Note` 只由转发路径调用；`Snapshot` 只读快照；
//  3. **不许按上游名分叉**：键是 (模型名, 渠道 id)，与上游无关。
//
// ⚠ 内存边界：键数 = 模型名 × 渠道数（本仓量级：几十 × 几十）⇒ 不需要淘汰策略；
//
//	真要防"恶意模型名把 map 撑爆"，上限见 maxTallyModels（超了就不再记新模型）。
package channelroute

import "sync"

// maxTallyModels 记账的模型名上限（防"随便报模型名 ⇒ map 无限长"）。
//
// ⚠ 取 64：真实部署里一个渠道页会看的模型名远少于这个数；超了只影响诊断的完整性，
//
//	**绝不影响选路**（`Note` 在超限后直接返回）。
const maxTallyModels = 64

// Tally 进程内的"各模型各渠道被选中次数"。
//
// ⚠ 零值可用（`var t Tally`）—— 与 `sync.Once` 相比少一个"忘了初始化"的坑；
//
//	指针为 nil 时所有方法都安全（诊断端点的"没接过任何请求"形态）。
type Tally struct {
	mu sync.Mutex
	m  map[string]map[string]int
}

// Note 记一次选中。⚠ 只由转发路径调用（不是"谁想看就调"）。
func (t *Tally) Note(model, channelID string) {
	if t == nil || model == "" || channelID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]map[string]int{}
	}
	per, ok := t.m[model]
	if !ok {
		if len(t.m) >= maxTallyModels {
			return // 超上限：宁可少记，也不让 map 无限长
		}
		per = map[string]int{}
		t.m[model] = per
	}
	per[channelID]++
}

// Snapshot 取某模型当前的计数快照（拷贝 ⇒ 调用方拿到后随便用，不持有锁）。
func (t *Tally) Snapshot(model string) map[string]int {
	if t == nil || model == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	per := t.m[model]
	if len(per) == 0 {
		return nil
	}
	out := make(map[string]int, len(per))
	for k, v := range per {
		out[k] = v
	}
	return out
}

// Reset 清空（测试用；生产不调）。
func (t *Tally) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m = nil
}
