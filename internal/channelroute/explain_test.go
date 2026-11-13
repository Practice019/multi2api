// explain_test.go 只读诊断 `Explain` 的判据（2026-11-13 用户报障「不会轮询这些渠道」）。
//
// # 用户原话
//
//	"一个模型名对应了多个渠道，但是我发现他调用的时候**不会轮询**这些渠道" / "只要映射的都要轮询"
//
// # 本轮只做"可判性"（主脑裁定：**不许改选路语义**）
//
//	现状（已核实）：同档内**确实**按权重轮转；但常规请求只取**第 0 档**（最高优先级），
//	其余各档要等失败重试才进入 ⇒ 6 条候选分属 3 档时，常规只有 2 条在轮。
//	⇒ 先把这件事变成**可见事实**（这份诊断），再决定要不要改路由。
//
// ⚠ 诊断必须**只读**：不发上游请求、不改优先级/权重、不写库（见 TestExplainIsReadOnly）。
package channelroute

import (
	"strings"
	"testing"

	"workbuddy2api/internal/channelstore"
)

// explainFixture 造"6 条候选、分属 3 档"的夹具（每档 2 条，档内权重不同）。
//
// ⚠ 三档的优先级取 30 / 20 / 10（越大越先用）⇒ 期望：第 0 档 = 30 那两条。
func explainFixture(t *testing.T) *channelstore.Store {
	t.Helper()
	s := store(t)
	addKeyed(t, s, "ch-a1", 30, 70, 2, 2) // 第 0 档
	addKeyed(t, s, "ch-a2", 30, 30, 1, 1) // 第 0 档
	addKeyed(t, s, "ch-b1", 20, 50, 1, 1) // 第 1 档
	addKeyed(t, s, "ch-b2", 20, 50, 1, 1) // 第 1 档
	addKeyed(t, s, "ch-c1", 10, 50, 1, 0) // 第 2 档（key 全停用！）
	addKeyed(t, s, "ch-c2", 10, 50, 1, 1) // 第 2 档
	return s
}

// addKeyed 建一条能服务 "pub-x" 的渠道：total 个 key，其中 usable 个启用。
func addKeyed(t *testing.T, s *channelstore.Store, id string, prio, weight, total, usable int) {
	t.Helper()
	keys := make([]channelstore.ChannelKey, 0, total)
	for i := 0; i < total; i++ {
		st := channelstore.StatusEnabled
		if i >= usable {
			st = channelstore.StatusManuallyDisabled // ⚠ 停用值**不是 0**：0 在存储层等价于"启用"（见 convert.go:108）
		}
		keys = append(keys, channelstore.ChannelKey{Key: "sk-" + id + "-" + string(rune('a'+i)), Status: st})
	}
	if err := s.Upsert(&channelstore.Channel{
		ID: id, Name: id, BaseURL: "https://" + id + ".example.com/v1",
		Status: channelstore.StatusEnabled, Priority: prio, Weight: weight,
		Models: []string{"up-x"}, ModelMap: map[string]string{"pub-x": "up-x"},
		Keys: keys,
	}); err != nil {
		t.Fatalf("建渠道 %s 失败: %v", id, err)
	}
}

// ① ⚠ 2026-11-13 **重写**：单池 —— 6 条候选**全部**参与常规轮转（旧断言是"只有第 0 档”）。
//
//	这条同时是"用户能看见轮询"的判据：结论里必须点出"一个池/都参与"。
func TestExplainShowsSinglePool(t *testing.T) {
	s := explainFixture(t)
	v, err := Explain(s, "", "pub-x")
	if err != nil {
		t.Fatalf("Explain 失败: %v", err)
	}
	if v.Total != 6 || len(v.Candidates) != 6 {
		t.Fatalf("候选应当 6 条全部可见，实际 total=%d len=%d", v.Total, len(v.Candidates))
	}
	if v.RegularCount != 6 {
		t.Errorf("参与常规轮转的必须是**全部 6 条**，实际 %d", v.RegularCount)
	}
	for _, c := range v.Candidates {
		if !c.Regular {
			t.Errorf("候选 %s 不在常规池里（单池之后不许有「旁观者」）", c.ChannelID)
		}
	}
	if !strings.Contains(v.Conclusion, "一个池") && !strings.Contains(v.Conclusion, "都参与") {
		t.Errorf("结论里没点出「全部候选同一个池、都参与轮询」：%q", v.Conclusion)
	}
	// 配置里还写着 priority ⇒ 必须**明说它已不生效**（否则用户以为它还在起作用）。
	if v.PrioritiesInConfig != 6 {
		t.Errorf("应当报出 6 条配置里写着 priority，实际 %d", v.PrioritiesInConfig)
	}
	joined := strings.Join(v.Notes, " ")
	if !strings.Contains(joined, "已不参与选路") {
		t.Errorf("备注里必须说明 priority 已不生效：%v", v.Notes)
	}
}

// ② "某条候选 key 全停用" ⇒ 必须**明确标注**（不是静默少一条）。
func TestExplainFlagsFullyDisabledKeys(t *testing.T) {
	s := explainFixture(t)
	v, err := Explain(s, "", "pub-x")
	if err != nil {
		t.Fatalf("Explain 失败: %v", err)
	}
	var found *CandidateExplain
	for i := range v.Candidates {
		if v.Candidates[i].ChannelID == "ch-c1" {
			found = &v.Candidates[i]
		}
	}
	if found == nil {
		t.Fatal("ch-c1（key 全停用）在诊断里**消失**了 —— 那正是「看起来没参与」最容易被误判的成因；它必须出现并被标注")
	}
	if found.KeysUsable != 0 || found.KeysTotal != 1 {
		t.Errorf("ch-c1 的 key 数应为 0/1，实际 %d/%d", found.KeysUsable, found.KeysTotal)
	}
	if found.SkipReason == "" {
		t.Error("key 全停用必须给出 SkipReason（明确标注），不能只靠数字让用户自己发现")
	}
	if !strings.Contains(v.Conclusion, "1 条没有可用 key") && !strings.Contains(strings.Join(v.Notes, " "), "没有可用 key") {
		t.Errorf("结论/备注里应指出有候选没有可用 key：conclusion=%q notes=%v", v.Conclusion, v.Notes)
	}
}

// ④ 只读性：跑一遍诊断，`Plan` 的结果必须逐项相等（不许有副作用）。
func TestExplainIsReadOnly(t *testing.T) {
	s := explainFixture(t)
	before, err := Plan(s, "", "pub-x")
	if err != nil {
		t.Fatalf("Plan 失败: %v", err)
	}
	if _, err := Explain(s, "", "pub-x"); err != nil {
		t.Fatalf("Explain 失败: %v", err)
	}
	after, err := Plan(s, "", "pub-x")
	if err != nil {
		t.Fatalf("Plan（第二次）失败: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("诊断改变了候选数：%d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Channel.ID != after[i].Channel.ID ||
			before[i].Priority != after[i].Priority ||
			before[i].Weight != after[i].Weight ||
			before[i].UpstreamModel != after[i].UpstreamModel {
			t.Errorf("第 %d 条候选被诊断改了：%+v → %+v", i, before[i], after[i])
		}
	}
	// 渠道本身的 priority/weight/status 也不许被改（诊断绝不写库）。
	for _, ch := range s.List() {
		if ch.Status != channelstore.StatusEnabled {
			t.Errorf("渠道 %s 的 status 被诊断改了：%d", ch.ID, ch.Status)
		}
	}
}

// ⑥ 可判性："各被选中多少次 / 当前是否均匀"必须是一句话能看出来的结论。
//
// ⚠ 只读：`ApplyCounts` 是**纯函数**（给它一份计数快照，返回新的诊断值），
//
//	不许因为"要看统计"而改动选路状态。
func TestExplainFairnessIsJudgeable(t *testing.T) {
	s := explainFixture(t)
	v, err := Explain(s, "", "pub-x")
	if err != nil {
		t.Fatalf("Explain 失败: %v", err)
	}

	// 还没采样 ⇒ 明确说「还没有数据」（而不是画成"均匀"）。
	if got := ApplyCounts(v, nil); !strings.Contains(got.Fairness, "还没有采样") {
		t.Errorf("没有采样数据时应当直说，实际 %q", got.Fairness)
	}

	// ⚠ 2026-11-13 补：判据改成"按权重折算期望 + 3σ + **小样本不判定**"之后，
	//   这里的采样次数必须**超过下限**（5 条可用候选 × 10 = 50 次）否则会正确地报"样本不足"。
	//   两条断言因此各抬到 ≥50 次（同时保留"零命中要报偏斜"这个形态）。
	lopsided := map[string]int{}
	for _, c := range v.Candidates {
		if c.ChannelID == "ch-c1" { // ch-c1 的 key 全停用（本来就选不中）
			continue
		}
		lopsided[c.ChannelID] = 15
	}
	lopsided["ch-c2"] = 0 // 可用但一次都没被选中（用户报障的形态）
	got := ApplyCounts(v, lopsided)
	if !strings.Contains(got.Fairness, "偏斜") {
		t.Errorf("有可用候选零命中（60 次采样）必须报偏斜，实际 %q", got.Fairness)
	}
	// 计数也要落到每条候选上（用户能逐条看）。
	for _, c := range got.Candidates {
		if c.ChannelID == "ch-a1" && c.Picked != 15 {
			t.Errorf("ch-a1 的 picked 应为 15，实际 %d", c.Picked)
		}
	}

	// 均匀 ⇒ 说"均匀"（5 条可用候选 × 12 = 60 次，超过下限）。
	even := map[string]int{}
	for _, c := range v.Candidates {
		even[c.ChannelID] = 12
	}
	if got := ApplyCounts(v, even); !strings.Contains(got.Fairness, "均匀") {
		t.Errorf("各 12 次（60 次采样）应当判为均匀，实际 %q", got.Fairness)
	}
}
