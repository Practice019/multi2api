// channels_uniform_test.go 用户指令（2026-11-13）：「优先级全部去掉，全部平等」。
//
// # 判什么
//
//	① 均匀覆盖：6 条候选、权重全等 ⇒ 每条都该被选中（修之前只有最高档那 2 条会被选中）；
//	② 回归锁：**权重仍生效**（一条设成 10 倍 ⇒ 占比显著更高）；
//	③ 重试不再"降级"：失败后换**另一条没试过的**，且试过的不会再试；
//	④ 终止：所有候选都试过 ⇒ 明确"没得试"（守住防无限重试这条既有硬要求）；
//	⑤ 候选里残留 priority ⇒ 不生效（而且不报错）。
//
// ⚠ 采样用**固定随机源**（注入 `*rand.Rand`，不用全局 rand）⇒ 可复现，不是概率性假绿。
package server

import (
	"math/rand"
	"testing"

	"workbuddy2api/internal/channelroute"
	"workbuddy2api/internal/channelstore"
)

// uniformCands 造 n 条候选（权重由参数给；priority 各不相同 —— 它已不参与选路）。
func uniformCands(n int, weights ...int) []channelroute.Candidate {
	out := make([]channelroute.Candidate, 0, n)
	for i := 0; i < n; i++ {
		w := 1
		if i < len(weights) {
			w = weights[i]
		}
		id := "ch-" + string(rune('a'+i))
		out = append(out, channelroute.Candidate{
			Channel:  &channelstore.Channel{ID: id, Name: id},
			Priority: 100 - i*10, // ⚠ 故意给不同 priority：不该影响结果
			Weight:   w,
		})
	}
	return out
}

// sample 独立采样 k 次（每次都从"没试过"开始 = 模拟连续的独立请求）。
func sample(cands []channelroute.Candidate, k int, seed int64) map[string]int {
	rnd := rand.New(rand.NewSource(seed))
	seen := map[string]int{}
	for i := 0; i < k; i++ {
		c, _, ok := pickCandidateExcluding(cands, map[string]bool{}, rnd)
		if !ok {
			continue
		}
		seen[c.Channel.ID]++
	}
	return seen
}

// ① 均匀覆盖：每条候选都必须有机会被选中。
func TestChannelPickUniformCoverage(t *testing.T) {
	cands := uniformCands(6)
	seen := sample(cands, 600, 20261113)
	if len(seen) != 6 {
		t.Fatalf("只覆盖了 %d/6 条候选：%v —— 用户报的「不会轮询」就是这个", len(seen), seen)
	}
	// 权重全等 ⇒ 不该有系统性偏斜（期望各 ≈100 次）。
	min, max := 1<<30, 0
	for _, n := range seen {
		if n < min {
			min = n
		}
		if n > max {
			max = n
		}
	}
	t.Logf("采样分布（6 条、权重全等、600 次）: %v（min=%d max=%d）", seen, min, max)
	if max > min*2 {
		t.Errorf("权重全等却有系统性偏斜：min=%d max=%d（分布 %v）", min, max, seen)
	}
}

// ② 权重仍生效（用户说的是"优先级"，没说权重）。
func TestChannelPickWeightStillWorks(t *testing.T) {
	cands := uniformCands(6, 10, 1, 1, 1, 1, 1)
	seen := sample(cands, 600, 7)
	heavy := seen["ch-a"]
	if heavy*100/600 < 40 {
		t.Errorf("权重 10 的那条只占 %d/600（期望≈67%%）—— 权重被这次改动弄丢了？分布 %v", heavy, seen)
	}
	for id, n := range seen {
		if n == 0 {
			t.Errorf("候选 %s 一次都没被选中（权重 1 也应当有份额）：%v", id, seen)
		}
	}
}

// ③ 重试换的是"另一条没试过的"，且试过的不会重复。
func TestChannelRetrySwitchesToUntriedCandidate(t *testing.T) {
	cands := uniformCands(6)
	rnd := rand.New(rand.NewSource(99))
	tried := map[string]bool{}
	var order []string
	for {
		c, left, ok := pickCandidateExcluding(cands, tried, rnd)
		if !ok {
			break
		}
		if tried[c.Channel.ID] {
			t.Fatalf("候选 %s 被重复选中（试过的必须排除）", c.Channel.ID)
		}
		tried[c.Channel.ID] = true
		order = append(order, c.Channel.ID)
		if want := len(cands) - len(tried); left != want {
			t.Errorf("left（还剩几条没试）应为 %d，实际 %d（顺序 %v）", want, left, order)
		}
	}
	if len(order) != len(cands) {
		t.Errorf("应当试完全部 %d 条，实际 %d 条：%v", len(cands), len(order), order)
	}
}

// ④ 终止：全部试过 ⇒ 明确"没得试"（不是死循环、不是回到第一条）。
func TestChannelPickTerminatesWhenAllTried(t *testing.T) {
	cands := uniformCands(3)
	tried := map[string]bool{"ch-a": true, "ch-b": true, "ch-c": true}
	if _, left, ok := pickCandidateExcluding(cands, tried, rand.New(rand.NewSource(1))); ok || left != 0 {
		t.Fatalf("全部试过后必须 ok=false 且 left=0（否则就是无限重试），实际 ok=%v left=%d", ok, left)
	}
}

// ⑤ 残留 priority 不生效：不同 priority 的候选在同一池里被平等对待。
func TestChannelPickIgnoresResidualPriority(t *testing.T) {
	cands := uniformCands(4) // priority = 100 / 90 / 80 / 70
	seen := sample(cands, 400, 4242)
	if len(seen) != 4 {
		t.Fatalf("priority 仍在影响选路（只覆盖 %d/4）：%v", len(seen), seen)
	}
}
