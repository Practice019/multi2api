// channels_pick_test.go pickCandidate 的专属守卫（上一轮我标注过它没有测试）。
//
// 三条对应三段语义，都是**纯单元**（直接喂候选，不建 store、不打 HTTP）：
//
//	① 同档：权重 <=0 视为 1（不让"没填权重"变成"永远选不中"），权重相差悬殊时偏重者
//	② 档位：attempt=0 只从最高优先级档里选；attempt=1 换到下一档
//	③ 档位用尽：明确报错，且**不是** errNoChannelMode（否则入口会把它当"模式未开启"而悄悄回落账号池）
package server

import (
	"errors"
	"testing"

	"workbuddy2api/internal/channelroute"
	"workbuddy2api/internal/channelstore"
)

func cand(id string, prio, weight int) channelroute.Candidate {
	return channelroute.Candidate{
		Channel:       &channelstore.Channel{ID: id, Name: id},
		UpstreamModel: "m",
		Priority:      prio,
		Weight:        weight,
	}
}

// ① 同档加权：权重写 0 与写 1 等价（都能被选中），权重悬殊时偏重者明显更多。
func TestPickCandidateWeightSemantics(t *testing.T) {
	// 两个都写 0 ⇒ 都视为 1 ⇒ 两者都必须出现过（否则"没填权重"就成了静默屏蔽）
	zero := []channelroute.Candidate{cand("a", 10, 0), cand("b", 10, 0)}
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		c, err := pickCandidate(zero, 0)
		if err != nil {
			t.Fatalf("同档应能选出候选: %v", err)
		}
		seen[c.Channel.ID]++
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Fatalf("权重均为 0 时应等价于 1（两条都能被选中），实际 %v", seen)
	}

	// 权重悬殊：重的必须明显更多（用 200 次抽样，阈值取得很宽松，避免偶发翻红）
	skew := []channelroute.Candidate{cand("heavy", 10, 1000), cand("light", 10, 1)}
	seen = map[string]int{}
	for i := 0; i < 200; i++ {
		c, _ := pickCandidate(skew, 0)
		seen[c.Channel.ID]++
	}
	if seen["heavy"] <= seen["light"] {
		t.Fatalf("权重 1000:1 时重者应明显更多，实际 %v", seen)
	}
}

// ② 档位：attempt=0 只从最高优先级档选；attempt=1 换到下一档。
func TestPickCandidateAttemptSwitchesTier(t *testing.T) {
	cs := []channelroute.Candidate{
		cand("high", 100, 1),
		cand("low", 1, 1),
	}
	for i := 0; i < 50; i++ {
		c, err := pickCandidate(cs, 0)
		if err != nil || c.Channel.ID != "high" {
			t.Fatalf("attempt=0 应只从最高档选，实际 %v err=%v", c.Channel.ID, err)
		}
	}
	for i := 0; i < 50; i++ {
		c, err := pickCandidate(cs, 1)
		if err != nil || c.Channel.ID != "low" {
			t.Fatalf("attempt=1 应换到下一档，实际 %v err=%v", c.Channel.ID, err)
		}
	}
	// 同档多条 + attempt=1：仍只在该档内选
	multi := []channelroute.Candidate{
		cand("h1", 100, 1), cand("h2", 100, 1), cand("l1", 5, 1),
	}
	for i := 0; i < 50; i++ {
		c, err := pickCandidate(multi, 1)
		if err != nil || c.Channel.ID != "l1" {
			t.Fatalf("attempt=1 应整档跳到低档（不是同档第 2 条），实际 %v", c.Channel.ID)
		}
	}
}

// ③ 档位用尽：明确报错、不回退，且**不得**是 errNoChannelMode（否则入口会悄悄回落账号池）。
func TestPickCandidateExhaustedTiersFailsClosed(t *testing.T) {
	cs := []channelroute.Candidate{cand("only", 100, 1)}
	_, err := pickCandidate(cs, 1)
	if err == nil {
		t.Fatal("档位用尽应报错")
	}
	if errors.Is(err, errNoChannelMode) {
		t.Fatalf("★ 这**不该**被当成'渠道模式未开启'（入口会据此回落到账号池），实际 %v", err)
	}
	if errors.Is(err, errChannelNoCandidate) {
		t.Fatalf("档位用尽与'索引里没有这个模型'是两件事，不该混用同一个错误，实际 %v", err)
	}
	// 负 attempt 按 0 处理（防御性）
	if c, err := pickCandidate(cs, -3); err != nil || c.Channel.ID != "only" {
		t.Fatalf("负 attempt 应按 0 处理，实际 %v err=%v", c.Channel.ID, err)
	}
	// 空候选：报"无候选"（与入口的 404 分支对齐）
	if _, err := pickCandidate(nil, 0); !errors.Is(err, errChannelNoCandidate) {
		t.Fatalf("空候选应报 errChannelNoCandidate，实际 %v", err)
	}
}
