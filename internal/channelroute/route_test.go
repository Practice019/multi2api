// route_test.go 守卫"目录里看得见的名字，一定发得出去"这条对称性。
package channelroute

import (
	"errors"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/channelstore"
)

func store(t *testing.T) *channelstore.Store {
	t.Helper()
	s, err := channelstore.New(filepath.Join(t.TempDir(), "channels.json"))
	if err != nil {
		t.Fatalf("建 store 失败: %v", err)
	}
	return s
}

func add(t *testing.T, s *channelstore.Store, id string, status int, priority int, models []string, mm map[string]string) {
	t.Helper()
	if err := s.Upsert(&channelstore.Channel{
		ID: id, Name: id, BaseURL: "https://" + id + ".example.com/v1",
		Status: status, Priority: priority, Models: models, ModelMap: mm,
	}); err != nil {
		t.Fatalf("建渠道 %s 失败: %v", id, err)
	}
}

// 没写映射 ⇒ 上游名就是请求名本身（映射只对表里列出的名字生效）。
func TestPlanPassthroughWhenUnmapped(t *testing.T) {
	s := store(t)
	add(t, s, "ch-a", channelstore.StatusEnabled, 10, []string{"plain"}, nil)
	got, err := Plan(s, "", "plain")
	if err != nil {
		t.Fatalf("应有候选: %v", err)
	}
	if len(got) != 1 || got[0].UpstreamModel != "plain" {
		t.Fatalf("未映射时上游名应原样，实际 %+v", got)
	}
}

// ★ 请求侧映射：客户端发公开名，上游必须收到**上游名**。
func TestPlanResolvesPublicNameToUpstream(t *testing.T) {
	s := store(t)
	// 公开名 pub-y → 上游名 real-x（与 new-api 的 channel.model_mapping 同向）
	add(t, s, "ch-b", channelstore.StatusEnabled, 10, []string{"real-x"}, map[string]string{"pub-y": "real-x"})
	got, err := Plan(s, "", "pub-y")
	if err != nil {
		t.Fatalf("公开名应能规划出候选: %v", err)
	}
	if len(got) != 1 || got[0].UpstreamModel != "real-x" {
		t.Fatalf("上游模型名应为 real-x（不是公开名），实际 %+v", got)
	}
	// 反向：客户端若直接发上游名，而该渠道的映射没把它列为公开名 ⇒ 上游名=它自己 ⇒ 也匹配
	got2, err := Plan(s, "", "real-x")
	if err != nil || len(got2) != 1 || got2[0].UpstreamModel != "real-x" {
		t.Fatalf("发上游名也应可用（透传），实际 %+v err=%v", got2, err)
	}
	// 渠道没声明的名字 ⇒ 无候选（默认收紧）
	if _, err := Plan(s, "", "not-declared"); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("未声明的模型应报 ErrNoCandidate，实际 %v", err)
	}
}

// 停用渠道不参与；全部停用 ⇒ 无候选（哪怕名字对）。
func TestPlanSkipsDisabledChannels(t *testing.T) {
	s := store(t)
	add(t, s, "ch-on", channelstore.StatusEnabled, 5, []string{"m"}, nil)
	add(t, s, "ch-off", channelstore.StatusManuallyDisabled, 99, []string{"m"}, nil)
	got, err := Plan(s, "", "m")
	if err != nil {
		t.Fatalf("应有候选: %v", err)
	}
	if len(got) != 1 || got[0].Channel.ID != "ch-on" {
		t.Fatalf("停用渠道不该参与（哪怕优先级更高），实际 %+v", got)
	}
	if err := s.SetStatus("ch-on", channelstore.StatusManuallyDisabled); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if _, err := Plan(s, "", "m"); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("全停用后应无候选，实际 %v", err)
	}
}

// 优先级 desc 排序、同优先级按渠道 id 稳定（同输入两次计划必须同序，否则重试语义会漂）。
func TestPlanSortsByPriorityAndIsStable(t *testing.T) {
	s := store(t)
	add(t, s, "ch-low", channelstore.StatusEnabled, 1, []string{"m"}, nil)
	add(t, s, "ch-high", channelstore.StatusEnabled, 100, []string{"m"}, nil)
	add(t, s, "ch-mid", channelstore.StatusEnabled, 50, []string{"m"}, nil)
	got, err := Plan(s, "", "m")
	if err != nil {
		t.Fatalf("应有候选: %v", err)
	}
	want := []string{"ch-high", "ch-mid", "ch-low"}
	if len(got) != 3 {
		t.Fatalf("应有 3 条候选，实际 %d", len(got))
	}
	for i, id := range want {
		if got[i].Channel.ID != id {
			t.Fatalf("第 %d 条应为 %s，实际 %s（顺序 %v）", i, id, got[i].Channel.ID, ids(got))
		}
	}
	again, _ := Plan(s, "", "m")
	for i := range again {
		if again[i].Channel.ID != got[i].Channel.ID {
			t.Fatalf("两次计划顺序不一致：%v vs %v", ids(again), ids(got))
		}
	}
}

// 坏映射（环）不该让整个模型不可用：跳过那条渠道，别的渠道仍可服务。
func TestPlanSkipsChannelWithCyclicMapping(t *testing.T) {
	s := store(t)
	add(t, s, "ch-bad", channelstore.StatusEnabled, 99, []string{"x", "y"}, map[string]string{"x": "y", "y": "x"})
	add(t, s, "ch-good", channelstore.StatusEnabled, 1, []string{"x"}, nil)
	got, err := Plan(s, "", "x")
	if err != nil {
		t.Fatalf("坏渠道不该让模型整体不可用: %v", err)
	}
	for _, c := range got {
		if c.Channel.ID == "ch-bad" {
			t.Fatalf("含环的渠道不该成为候选，实际 %v", ids(got))
		}
	}
	if len(got) != 1 || got[0].Channel.ID != "ch-good" {
		t.Fatalf("应只剩 ch-good，实际 %v", ids(got))
	}
}

func ids(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Channel.ID)
	}
	return out
}
