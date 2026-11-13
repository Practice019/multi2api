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
		c, err := pickCandidate(zero)
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
		c, _ := pickCandidate(skew)
		seen[c.Channel.ID]++
	}
	if seen["heavy"] <= seen["light"] {
		t.Fatalf("权重 1000:1 时重者应明显更多，实际 %v", seen)
	}
}

// ② ⚠ 2026-11-13 **重写**（旧断言 `TestPickCandidateAttemptSwitchesTier` 编码的是被用户否掉的语义）：
//
//	用户指令：「不要设置上游或者渠道优先级 **全部去掉，全部平等**」
//	⇒ 原来是"attempt=0 只从最高优先级档选；attempt=1 换到下一档"；
//	  现在**没有档**：`Priority` 不再参与任何判据，全部候选在一个池里。
//
//	⚠ 旧断言里被保留的**那一半**："空候选要报 errChannelNoCandidate" 见下一条。
func TestPickCandidateIgnoresPriority(t *testing.T) {
	cs := []channelroute.Candidate{
		cand("high", 100, 1),
		cand("low", 1, 1),
	}
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		c, err := pickCandidate(cs)
		if err != nil {
			t.Fatalf("应当能选出候选: %v", err)
		}
		seen[c.Channel.ID]++
	}
	if len(seen) != 2 {
		t.Fatalf("priority 100 与 1 必须都被选中（全部平等），实际 %v", seen)
	}
}

// ③ ⚠ 2026-11-13 **重写**（旧的"档位用尽"已随优先级一起消失）：空候选要**明确失败**，
//
//	而且**不得**是 errNoChannelMode —— 那条是"渠道模式未开启"，入口会据此悄悄回落账号池
//	（Key 型渠道与账号上游的分界，见 channels_forward.go 文件头）。这条**必须守住**。
func TestPickCandidateNoCandidateFailsClosed(t *testing.T) {
	if _, err := pickCandidate(nil); !errors.Is(err, errChannelNoCandidate) {
		t.Fatalf("空候选应报 errChannelNoCandidate，实际 %v", err)
	}
	if _, err := pickCandidate(nil); errors.Is(err, errNoChannelMode) {
		t.Fatalf("★ 这条**不该**被当成「渠道模式未开启」（入口会据此回落到账号池），实际 %v", err)
	}
}
