package qoder

import (
	"sync"

	"workbuddy2api/internal/gateway"
)

// consumed.go 从对话回执里读「这次花了多少 credits」，并在跨过整数时上报。
//
// # 为什么需要"累计 + 跨整数才上报"（这是 qoder 特有的复杂度）
//
// 实测：`usage.credits` = `0.006352698571428571`/次，而池子里的显示值是
// `int64(Total)` —— **整数 credits**（且用户明确要求"显示原始值不换算"）。
//
// 所以两条直路都不通：
//
//	把回执里的数直接写进池子 → 写进去的是 0（截断），等于没写
//	每次从显示值里减它       → 减 0，永远扣不动
//
// 正确做法是**在内部累计小数、只在跨过整数时上报整数** ——
// 于是核心不需要知道任何上游的刻度，显示值也保持上游自己的单位。
const creditsScale = 1_000_000 // 1 credit = 1e6 微 credits

// consumedAcc 每个 uid 的"未上报微 credits 余量"。
//
// ⚠ 并发安全：`Chat` 在转发路径上会被并发调用（同一个 Provider 服务所有账号）。
type consumedAcc struct {
	mu sync.Mutex
	m  map[string]int64
}

func newConsumedAcc() *consumedAcc { return &consumedAcc{m: map[string]int64{}} }

// add 累加 scaled 微 credits，返回**本次可以上报的整数 credits**（可能为 0）。
//
// ⚠ **余量必须保留**（不能把累计整个抹掉）：抹掉的话每次不足 1 credit 的部分
// 都丢了，长期少扣 —— 累积起来就是"额度看起来比实际多"，而那正是用户最不
// 想要的形态（以为还有免费额度，于是继续用）。
func (a *consumedAcc) add(uid string, scaled int64) int64 {
	if uid == "" || scaled <= 0 {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[uid] += scaled
	whole := a.m[uid] / creditsScale
	if whole > 0 {
		a.m[uid] -= whole * creditsScale
	}
	return whole
}

// reset 清空某个 uid 的累计。
//
// ⚠ **成功查过一次余额之后必须调它**：那次查询拿到的值**已经包含**了我们
// 尚未上报的消耗，再上报就会**多扣**（把一个还有额度的账号显示成用完）。
// 见 `gateway.QuotaConsumedSink` 文件头那段"一个有界且自愈的偏差"。
func (a *consumedAcc) reset(uid string) {
	if uid == "" {
		return
	}
	a.mu.Lock()
	delete(a.m, uid)
	a.mu.Unlock()
}

// quotaTapSpec qoder 的额度旁听声明。
//
// ⚠ `Mode=UsageConsumed`：`usage.credits` 是**消耗掉的额度**（与余额同单位），
// 不是 `cline` 那种"成本/费用"。声明错的代价是编数据 —— 所以 Mode 是必填项
// （`gateway.UsageTapSpec.Validate` 会拦住缺省值）。
func quotaTapSpec() gateway.UsageTapSpec {
	return gateway.UsageTapSpec{
		Key:   "credits",
		Scale: creditsScale,
		Mode:  gateway.UsageConsumed,
	}
}
