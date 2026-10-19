// select_test.go 守卫选路的四条判据（都对着 new-api 的 channel-ability-routing 语义写）：
//
//	① 优先级是**耗尽阶梯**：第 n 次尝试走第 n 档，高档没走完不碰低档
//	② 档内是**加权随机**：权重大的被选中的比例明显更高（比例与权重同向，不是精确 3:1 —— 随机量）
//	③ 没有渠道时返回结构化错误且 NoChannel=true（不是返回 nil 让人猜）
//	④ 多 key 只挑启用的；全坏 ⇒ 返回 ""（调用方据此判渠道不可用）
//
// 随机源用固定种子注入（同包直接改 s.rand），所以分布判据可复现、不 flaky。
package channelstore

import (
	"errors"
	"testing"
)

// ① 阶梯：优先级 10 的渠道在 attempt=0 被选中；attempt=1 才轮到优先级 5 的。
func TestSelectWalksPriorityLadder(t *testing.T) {
	s, _ := newTestStore(t)
	s.rand = newRand(1)

	hi := ch("hi", "m")
	hi.Priority, hi.Weight = 10, 1
	lo := ch("lo", "m")
	lo.Priority, lo.Weight = 5, 1
	if err := s.Upsert(hi); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(lo); err != nil {
		t.Fatal(err)
	}

	if got, err := s.Select(DefaultGroup, "m", 0); err != nil || got.ID != "hi" {
		t.Fatalf("第 0 次应选高档渠道 hi，实际 %v err=%v", getID(got), err)
	}
	if got, err := s.Select(DefaultGroup, "m", 1); err != nil || got.ID != "lo" {
		t.Fatalf("第 1 次应轮到低档渠道 lo，实际 %v err=%v", getID(got), err)
	}
	if n := s.TierCount(DefaultGroup, "m"); n != 2 {
		t.Fatalf("应有 2 个优先级档，实际 %d", n)
	}
}

// ② 加权随机：同档两渠道权重 9:1，考 300 次，重的那个应占绝对多数（>70%）。
func TestSelectWeightedWithinTier(t *testing.T) {
	s, _ := newTestStore(t)
	s.rand = newRand(42)

	heavy := ch("heavy", "m")
	heavy.Priority, heavy.Weight = 1, 9
	light := ch("light", "m")
	light.Priority, light.Weight = 1, 1
	if err := s.Upsert(heavy); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(light); err != nil {
		t.Fatal(err)
	}

	nHeavy := 0
	const draws = 300
	for i := 0; i < draws; i++ {
		got, err := s.Select(DefaultGroup, "m", 0)
		if err != nil {
			t.Fatalf("第 %d 次选路失败: %v", i, err)
		}
		if got.ID == "heavy" {
			nHeavy++
		}
	}
	// 期望 ≈ 9/10 = 0.9；给足余量只要求 >0.7（判据要能变红，但不能 flaky）
	if nHeavy*10 < draws*7 {
		t.Fatalf("权重 9:1 时重渠道应占多数（>70%%），实际 %d/%d", nHeavy, draws)
	}
	// 反向：轻的也必须被选到过（否则等于"权重把小渠道饿死"）
	if nHeavy == draws {
		t.Fatalf("权重 1 的渠道一次都没被选到（%d/%d）—— 权重不该是开关", nHeavy, draws)
	}
}

// ③ 结构化失败：没有渠道 ⇒ NoChannel=true，且 attempt 超出档数也如实报。
func TestSelectErrorsAreStructured(t *testing.T) {
	s, _ := newTestStore(t)
	s.rand = newRand(1)

	// 模型压根没有渠道
	_, err := s.Select(DefaultGroup, "不存在", 0)
	var se *SelectError
	if !errors.As(err, &se) {
		t.Fatalf("应返回 *SelectError，实际 %v", err)
	}
	if !se.NoChannel || se.Code != CodeNoChannel || se.Status != 400 {
		t.Fatalf("无渠道时语义应为 400/%s/NoChannel，实际 %d/%s/%v", CodeNoChannel, se.Status, se.Code, se.NoChannel)
	}

	// 有渠道但重试次数超过档数
	if err := s.Upsert(ch("c1", "m")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Select(DefaultGroup, "m", 5); !errors.As(err, &se) {
		t.Fatalf("超出档数应返回 *SelectError，实际 %v", err)
	}
	if se.Code != CodeTierDone {
		t.Fatalf("超出档数应为 %s，实际 %s", CodeTierDone, se.Code)
	}
}

// 停用的渠道不进候选（能力索引里它本来就没了，这里再钉一次端到端语义）。
func TestSelectSkipsDisabledChannel(t *testing.T) {
	s, _ := newTestStore(t)
	s.rand = newRand(1)
	if err := s.Upsert(ch("c1", "m")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("c1", StatusManuallyDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Select(DefaultGroup, "m", 0); err == nil {
		t.Fatal("停用渠道不该被选中")
	}
}

// ④ 多 key：只挑启用的；只坏一半时永远挑不到坏的那个；全坏返回 ""。
func TestPickKeyHonoursStatus(t *testing.T) {
	s, _ := newTestStore(t)
	s.rand = newRand(7)

	c := ch("c1", "m")
	c.Keys = []ChannelKey{
		{Key: "sk-bad", Status: StatusManuallyDisabled},
		{Key: "sk-good", Status: StatusEnabled},
	}
	if err := s.Upsert(c); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("c1")
	for i := 0; i < 20; i++ {
		if k := s.PickKey(got); k != "sk-good" {
			t.Fatalf("应只挑启用 key，实际 %q", k)
		}
	}

	got.Keys[1].Status = StatusManuallyDisabled
	if k := s.PickKey(got); k != "" {
		t.Fatalf("全坏时应返回空串，实际 %q", k)
	}

	// round_robin 模式按游标轮转
	rr := ch("c2", "m")
	rr.ChannelInfo = ChannelInfo{KeySelectionMode: "round_robin"}
	rr.Keys = []ChannelKey{{Key: "k1", Status: StatusEnabled}, {Key: "k2", Status: StatusEnabled}}
	if err := s.Upsert(rr); err != nil {
		t.Fatal(err)
	}
	g2, _ := s.Get("c2")
	first, second := s.PickKey(g2), s.PickKey(g2)
	if first == second {
		t.Fatalf("round_robin 应轮转，实际连续两次都是 %q", first)
	}
}

func getID(ch *Channel) string {
	if ch == nil {
		return "<nil>"
	}
	return ch.ID
}
