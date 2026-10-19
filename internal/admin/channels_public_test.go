// channels_public_test.go 管理端「对外模型」视角的守卫。
//
// 与 /v1/models 的关系：同一套规则（internal/modelvis），但**不叠 key 白名单** ——
// 管理页问的是"我配的渠道让外面看见什么"，不该取决于管理台手里那把 key 恰好是什么。
package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"workbuddy2api/internal/channelstore"
)

// addChannel 建一条渠道（直通 store，不走 HTTP）。
func addChannel(t *testing.T, s *channelstore.Store, id, name string, status, priority int, models []string, mm map[string]string) {
	t.Helper()
	if err := s.Upsert(&channelstore.Channel{
		ID: id, Name: name, BaseURL: "https://" + id + ".example.com/v1",
		Status: status, Priority: priority, Models: models, ModelMap: mm,
	}); err != nil {
		t.Fatalf("建渠道 %s 失败: %v", id, err)
	}
}

type publicResp struct {
	Models []struct {
		Public   string   `json:"public"`
		Upstream string   `json:"upstream"`
		Channels []string `json:"channels"`
	} `json:"models"`
	PublicCount     int `json:"public_count"`
	AbilityCount    int `json:"ability_count"`
	EnabledChannels int `json:"enabled_channels"`
	TotalChannels   int `json:"total_channels"`
}

func getPublic(t *testing.T, h *Handler) publicResp {
	t.Helper()
	code, body := doJSON(t, h, http.MethodGet, "/admin/channels/public-models", "")
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", code, body)
	}
	var out publicResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, body)
	}
	return out
}

// 映射生效：对外名与上游名分开显示，且能看出"谁提供的"。
func TestPublicModelsShowsPublicAndUpstream(t *testing.T) {
	s := chanStore(t)
	// 公开名 pub-y → 上游名 real-x
	addChannel(t, s, "ch-a", "渠道A", channelstore.StatusEnabled, 10, []string{"real-x"}, map[string]string{"pub-y": "real-x"})
	addChannel(t, s, "ch-b", "渠道B", channelstore.StatusEnabled, 50, []string{"plain"}, nil)
	h := chanHandler(t, s)

	got := getPublic(t, h)
	if got.PublicCount != 2 || got.AbilityCount != 2 {
		t.Fatalf("应 2 条对外模型（两条能力各一条），实际 public=%d ability=%d", got.PublicCount, got.AbilityCount)
	}
	byName := map[string]string{}
	owners := map[string][]string{}
	for _, m := range got.Models {
		byName[m.Public] = m.Upstream
		owners[m.Public] = m.Channels
	}
	if _, ok := byName["pub-y"]; !ok {
		t.Fatalf("对外名应是 pub-y（不是上游名 real-x），实际 %v", byName)
	}
	if byName["pub-y"] != "real-x" {
		t.Fatalf("应同时回上游名便于排查，实际 %q", byName["pub-y"])
	}
	if byName["plain"] != "plain" {
		t.Fatalf("未映射的模型两个名字应相同，实际 %q", byName["plain"])
	}
	if len(owners["pub-y"]) != 1 || owners["pub-y"][0] != "渠道A" {
		t.Fatalf("应标出来源渠道，实际 %v", owners["pub-y"])
	}
}

// 停用渠道不产生能力 ⇒ 不出现；计数要能看出"停了几条"（这是看不见模型的常见原因）。
func TestPublicModelsExcludesDisabledAndCounts(t *testing.T) {
	s := chanStore(t)
	addChannel(t, s, "ch-on", "开着", channelstore.StatusEnabled, 10, []string{"m1"}, nil)
	addChannel(t, s, "ch-off", "停着", channelstore.StatusManuallyDisabled, 99, []string{"m2"}, nil)
	h := chanHandler(t, s)

	got := getPublic(t, h)
	if got.PublicCount != 1 {
		t.Fatalf("停用渠道的模型不该出现，实际 %d 条: %+v", got.PublicCount, got.Models)
	}
	if got.EnabledChannels != 1 || got.TotalChannels != 2 {
		t.Fatalf("计数应体现 1 启用 / 2 总数（看不见模型的常见原因一眼可见），实际 %d/%d",
			got.EnabledChannels, got.TotalChannels)
	}
}

// 同一条对外模型被两条渠道提供 ⇒ 来源按优先级降序（与选路顺序一致）。
func TestPublicModelsOwnerOrderByPriority(t *testing.T) {
	s := chanStore(t)
	addChannel(t, s, "ch-low", "低优先", channelstore.StatusEnabled, 1, []string{"m"}, nil)
	addChannel(t, s, "ch-high", "高优先", channelstore.StatusEnabled, 99, []string{"m"}, nil)
	h := chanHandler(t, s)

	got := getPublic(t, h)
	if len(got.Models) != 1 {
		t.Fatalf("两条渠道提供同一模型 ⇒ 对外只列一次，实际 %d", len(got.Models))
	}
	ch := got.Models[0].Channels
	if len(ch) != 2 || ch[0] != "高优先" {
		t.Fatalf("来源应按优先级降序（与选路一致），实际 %v", ch)
	}
}

// 未启用渠道功能 ⇒ 未注册（404）。
func TestPublicModelsNotRegisteredWithoutStore(t *testing.T) {
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true})
	code, _ := doJSON(t, h, http.MethodGet, "/admin/channels/public-models", "")
	if code != http.StatusNotFound {
		t.Fatalf("未注入存储时路由不该存在，实际 %d", code)
	}
}
