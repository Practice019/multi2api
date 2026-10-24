package server

import (
	"testing"
)

// 轮询：N 个候选必须严格按 1,2,3,...,N,1,2,... 分发。
//
// 用户原话：「它不是有几个上游吗？那就轮询就行了」。
//
// # 为什么不用随机
//
// 随机在高并发下会出现「某个上游 5 分钟内一次没轮到」，现象是
// 「额度没变但就是不走那条」—— 排查时极难判断是配置问题还是运气。
// 轮询可预测，均匀性也直观。
func TestPickAliasTargetRoundRobin(t *testing.T) {
	resetAliasRoundRobin()
	h := &Handler{}
	cands := []string{"a/1", "b/2", "c/3"}
	pub := "merged"

	// 连续 6 次（两轮）：应依次是 a,b,c,a,b,c
	want := []string{"a/1", "b/2", "c/3", "a/1", "b/2", "c/3"}
	for i, w := range want {
		got, order := h.pickAliasTarget(pub, cands)
		if got != w {
			t.Fatalf("第 %d 次 = %q，期望 %q（轮询顺序不对）", i+1, got, w)
		}
		// order 必须从选中的那个开始绕一圈
		if len(order) != 3 || order[0] != w {
			t.Fatalf("第 %d 次尝试顺序 = %v，期望从 %q 开始", i+1, order, w)
		}
	}
}

// 不同对外名各自独立轮询，互不干扰。
func TestPickAliasTargetIndependentPerName(t *testing.T) {
	resetAliasRoundRobin()
	h := &Handler{}
	a := []string{"x/1", "x/2"}
	b := []string{"y/1", "y/2"}

	// 交替调用两个名字，各自都应该从第一个开始
	if got, _ := h.pickAliasTarget("A", a); got != "x/1" {
		t.Errorf("A 第一次 = %q，期望 x/1", got)
	}
	if got, _ := h.pickAliasTarget("B", b); got != "y/1" {
		t.Errorf("B 第一次 = %q，期望 y/1（不该被 A 的游标影响）", got)
	}
	if got, _ := h.pickAliasTarget("A", a); got != "x/2" {
		t.Errorf("A 第二次 = %q，期望 x/2", got)
	}
}

// 单候选时不做轮询（顺序恒等于它自己）。
func TestPickAliasTargetSingle(t *testing.T) {
	resetAliasRoundRobin()
	h := &Handler{}
	for i := 0; i < 3; i++ {
		got, order := h.pickAliasTarget("solo", []string{"only/one"})
		if got != "only/one" || len(order) != 1 {
			t.Fatalf("单候选应恒返回 only/one，实际 %q / %v", got, order)
		}
	}
}

// 空候选不 panic。
func TestPickAliasTargetEmpty(t *testing.T) {
	resetAliasRoundRobin()
	h := &Handler{}
	if got, order := h.pickAliasTarget("x", nil); got != "" || order != nil {
		t.Errorf("空候选应返回空，实际 %q / %v", got, order)
	}
}

// aliasAttempts：去重 + 保持顺序 + 空时退化成当前值。
func TestAliasAttempts(t *testing.T) {
	cases := []struct {
		name    string
		order   []string
		current string
		want    []string
	}{
		{"空顺序退化成当前值", nil, "cur", []string{"cur"}},
		{"正常顺序原样", []string{"a", "b", "c"}, "a", []string{"a", "b", "c"}},
		{"重复项去掉", []string{"a", "a", "b"}, "a", []string{"a", "b"}},
		{"空串去掉", []string{"a", "", "b"}, "a", []string{"a", "b"}},
		{"全是空串则退化成当前值", []string{"", ""}, "cur", []string{"cur"}},
	}
	for _, c := range cases {
		got := aliasAttempts(c.order, c.current)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

// aliasCandidates：命中别名返回全部候选；未命中返回原名。
func TestAliasCandidates(t *testing.T) {
	// 这个测试只验证"未命中走原名"这条 —— 命中路径需要构造 key+分组，
	// 由 TestModelsListSurvivesDoubleRename 那类端到端测试覆盖。
	h := &Handler{}
	got := h.aliasCandidates(nil, "plain/model")
	if len(got) != 1 || got[0] != "plain/model" {
		t.Errorf("nil 请求应原样返回，实际 %v", got)
	}
}
