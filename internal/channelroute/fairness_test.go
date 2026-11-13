// fairness_test.go 「均匀 / 偏斜」判据的确定性判据（2026-11-13 T-909 补）。
//
// # 为什么要重做这个判据
//
//	第一版写的是 `max > min×3 ⇒ 偏斜`。6 条等权候选、n=60 次采样时，
//	期望各 10 次、标准差 ≈2.9 ⇒ 实测出现 3~14 次**只是约 2.4σ 的正常波动**，
//	却被判成"偏斜" ⇒ 用户会看到**假的**"轮询坏了"（CI 上就是这么红的）。
//
// # 新判据（算式写在 explain.go 的 applyFairness 注释里）
//
//	· 期望按**权重**折算：p_i = w_i / Σw、μ_i = n·p_i、σ_i = √(n·p_i·(1−p_i))；
//	· 判据：任一条 |x_i − μ_i| > 3σ_i ⇒ 偏斜（3σ ⇒ 单条误报率 ≈0.27%）；
//	· **小样本不判定**：n < k×minPerChannel（10）⇒ 报「样本不足，暂不判定」。
//
// ⚠ 全是**确定性**用例（直接喂计数，不掷骰子）⇒ 不 flaky。
package channelroute

import (
	"strings"
	"testing"
)

// viewWith 造一个 k 条等权候选的诊断视图（只填判据要用的字段）。
func viewWith(k int) ExplainView {
	v := ExplainView{Total: k}
	for i := 0; i < k; i++ {
		v.Candidates = append(v.Candidates, CandidateExplain{
			ChannelID: "c" + string(rune('a'+i)), Weight: 1, WeightUsed: 1,
		})
	}
	return v
}

// ① 正常波动**不许**判成偏斜（就是 CI 上那条假红：5~14 次 / 60 次）。
func TestFairnessNormalFluctuationIsNotSkewed(t *testing.T) {
	v := viewWith(6)
	// 实测撞到过的那一组：60 次、6 条、最少 5 最多 14（期望 10、σ≈2.89）。
	got := ApplyCounts(v, map[string]int{"ca": 14, "cb": 12, "cc": 10, "cd": 10, "ce": 9, "cf": 5})
	if strings.Contains(got.Fairness, "偏斜") {
		t.Errorf("2.4σ 的正常波动被误判成偏斜（用户会以为轮询坏了）：%q", got.Fairness)
	}
	if !strings.Contains(got.Fairness, "均匀") {
		t.Errorf("应当报均匀，实际 %q", got.Fairness)
	}
}

// ② 样本不足 ⇒ 明说"暂不判定"（不许猜）。
func TestFairnessTooFewSamplesSaysUnknown(t *testing.T) {
	v := viewWith(6)
	if got := ApplyCounts(v, nil); !strings.Contains(got.Fairness, "还没有采样") {
		t.Errorf("0 次采样应说「还没有采样」，实际 %q", got.Fairness)
	}
	// 6 条 × 10 = 60 次是下限；59 次仍不足（哪怕分布看起来很不均）。
	got := ApplyCounts(v, map[string]int{"ca": 40, "cb": 19})
	if !strings.Contains(got.Fairness, "样本不足") {
		t.Errorf("59 次（< 6×10）应当说「样本不足，暂不判定」，实际 %q", got.Fairness)
	}
	if strings.Contains(got.Fairness, "偏斜") {
		t.Errorf("样本不足时**不许**下偏斜结论（那是猜的）：%q", got.Fairness)
	}
}

// ③ 正向对照：**故意不均匀**必须报偏斜（防"为了绿把判据删了"）。
func TestFairnessDeliberateSkewIsCaught(t *testing.T) {
	v := viewWith(6)
	// 120 次里前两条吃掉 100 次（期望各 20、σ≈4.1 ⇒ 偏离远超 3σ）。
	got := ApplyCounts(v, map[string]int{"ca": 50, "cb": 50, "cc": 5, "cd": 5, "ce": 5, "cf": 5})
	if !strings.Contains(got.Fairness, "偏斜") {
		t.Errorf("明显不均匀（50/50/5/5/5/5）必须报偏斜，实际 %q", got.Fairness)
	}
}

// ④ 一条都没被选中 ⇒ 大样本下必须报偏斜（"看起来没参与"的形态）。
func TestFairnessZeroPickIsCaughtWithEnoughSamples(t *testing.T) {
	v := viewWith(6)
	got := ApplyCounts(v, map[string]int{"ca": 24, "cb": 24, "cc": 24, "cd": 24, "ce": 24, "cf": 0})
	if !strings.Contains(got.Fairness, "偏斜") {
		t.Errorf("120 次里有一条 0 次必须报偏斜，实际 %q", got.Fairness)
	}
}

// ⑤ 权重不等时：**按权重折算**的期望才算对（重的被选更多是正常的）。
func TestFairnessRespectsWeights(t *testing.T) {
	v := ExplainView{Total: 2}
	v.Candidates = append(v.Candidates,
		CandidateExplain{ChannelID: "heavy", Weight: 9, WeightUsed: 9},
		CandidateExplain{ChannelID: "light", Weight: 1, WeightUsed: 1})
	// 100 次里 90 / 10 —— 与权重 9:1 完全一致 ⇒ **不许**报偏斜。
	got := ApplyCounts(v, map[string]int{"heavy": 90, "light": 10})
	if strings.Contains(got.Fairness, "偏斜") {
		t.Errorf("9:1 权重下 90/10 是**预期**，不该报偏斜：%q", got.Fairness)
	}
}
