// apikey_publicname_test.go 对外名映射的**幂等性**：/v1/models 会改名两遍。
//
// # 用户报的真实现象（2026-10-21）
//
//	「我给 API key 加了 workbuddy/deepseek-v4.1-flash，对外公布成 006/deepseek-v4.1-flash，
//	 但客户端拿到的模型列表里没有 006/…」
//
// # 根因
//
// /v1/models 的组装是 **filterModelsForRequest → appendChannelModels → applyPublicNames**。
// 第一步已经把 id 换成了对外名（006/…），第三步又对同一个 id 跑一遍 publicNameFor：
//
//	精确匹配：modelMap["006/…"] 是键不是值 ⇒ 不命中
//	裸名匹配：SplitModel("006/deepseek-v4.1-flash") = "deepseek-v4.1-flash"，
//	          而 map 里 **004/005/006 三个对外名的 target 裸名全是它**
//	          ⇒ 遍历顺序随机，006 被改成 004 或 005，随后 seen[pub] 去重把条目吃掉
//
// 于是"配了三条对外名，列表里只剩两条、且名字是别人的"。这条测试把幂等性钉住。
package server

import (
	"path/filepath"
	"workbuddy2api/internal/groupstore"
	"testing"
)

// aliasFixture 单值表：给 publicNameFor 这类"只要一个答案"的测试用。
func aliasFixture() map[string]string {
	return map[string]string{
		"004/deepseek-v4.1-flash": "qiniuyun/deepseek-v4.1-flash",
		"005/deepseek-v4.1-flash": "senseaudio/deepseek-v4.1-flash",
		"006/deepseek-v4.1-flash": "workbuddy/deepseek-v4.1-flash",
	}
}

// aliasFixtureMulti 一对多表：给 groupstore.Create 用（2026-10-24 起分组存列表）。
//
// 与 aliasFixture 内容等价，只是每个对外名包成单元素列表 —— 这个测试关心的是
// "三个别名各自独立"，不是合并，所以每条一个内部名。
func aliasFixtureMulti() map[string][]string {
	out := make(map[string][]string, 3)
	for pub, in := range aliasFixture() {
		out[pub] = []string{in}
	}
	return out
}

// publicNameFor 必须是幂等的：传进去已经是对外名，就原样返回。
func TestPublicNameForIsIdempotent(t *testing.T) {
	m := aliasFixture()
	for _, pub := range []string{"004/deepseek-v4.1-flash", "005/deepseek-v4.1-flash", "006/deepseek-v4.1-flash"} {
		if got := publicNameFor(m, pub); got != pub {
			t.Errorf("对外名 %q 被二次改名成 %q —— "+
				"/v1/models 会先 filter（已改名）再 applyPublicNames（又改名），"+
				"用户配置的对外名就这样被别的上游抢走", pub, got)
		}
	}
}

// 内部名仍必须映射到它**自己**的对外名（精确匹配优先，不受同裸名的其它别名干扰）。
func TestPublicNameForMapsInternalToOwnAlias(t *testing.T) {
	m := aliasFixture()
	cases := map[string]string{
		"qiniuyun/deepseek-v4.1-flash":   "004/deepseek-v4.1-flash",
		"senseaudio/deepseek-v4.1-flash": "005/deepseek-v4.1-flash",
		"workbuddy/deepseek-v4.1-flash":  "006/deepseek-v4.1-flash",
	}
	for in, want := range cases {
		if got := publicNameFor(m, in); got != want {
			t.Errorf("publicNameFor(%q) = %q，want %q", in, got, want)
		}
	}
}

// 裸名兜底只在**唯一**命中时使用：多个别名共享同一裸名时不猜（返回原值）。
func TestPublicNameForBareNameNeedsUniqueHit(t *testing.T) {
	m := aliasFixture()
	// 三个 target 的裸名都相同 ⇒ 不唯一 ⇒ 不改名（保持原值，而不是随机挑一个）
	if got := publicNameFor(m, "某上游/deepseek-v4.1-flash"); got != "某上游/deepseek-v4.1-flash" {
		t.Errorf("裸名多命中时必须返回原值（不能猜），实际 %q", got)
	}
	// 唯一命中时才用裸名兜底
	m2 := map[string]string{"007/x": "some/deepseek-v9-flash"}
	if got := publicNameFor(m2, "other/deepseek-v9-flash"); got != "007/x" {
		t.Errorf("裸名唯一命中时应返回该别名，实际 %q", got)
	}
}

// 端到端：filter → applyPublicNames 两遍之后，三条对外名都得在，且不重复。
// 端到端：filter → applyPublicNames 两遍之后，三条对外名都得在，且不重复。
//
// ⚠ 2026-10-24 更新：别名从 API Key 移到了**分组**（用户要求
// 「对外模型不要在这里设置 在分组里面设置」）。所以这里要把别名配在分组上、
// 让 key 引用那个分组 —— 判据本身（两遍改名不能吃掉别名）一个字没变。
func TestModelsListSurvivesDoubleRename(t *testing.T) {
	keys := newKeyStore(t)
	gs := newGroupStore(t)
	mm := aliasFixtureMulti()
	allow := []string{"qiniuyun/deepseek-v4.1-flash", "senseaudio/deepseek-v4.1-flash", "workbuddy/deepseek-v4.1-flash"}
	g, err := gs.Create("别名组", "", allow, mm)
	if err != nil {
		t.Fatalf("建分组: %v", err)
	}
	k, err := keys.CreateOwnedWithGroups("", "666", 0, 0, nil, []string{g.ID})
	if err != nil {
		t.Fatalf("建 key: %v", err)
	}
	h := &Handler{cfg: Config{APIKeys: keys, Groups: gs}}
	r := reqAs(t, k.ID)

	list := []map[string]any{
		{"id": "qiniuyun/deepseek-v4.1-flash"},
		{"id": "senseaudio/deepseek-v4.1-flash"},
		{"id": "workbuddy/deepseek-v4.1-flash"},
	}
	got := h.applyPublicNames(r, h.filterModelsForRequest(r, list))
	seen := map[string]bool{}
	for _, m := range got {
		id, _ := m["id"].(string)
		seen[id] = true
	}
	for _, want := range []string{"004/deepseek-v4.1-flash", "005/deepseek-v4.1-flash", "006/deepseek-v4.1-flash"} {
		if !seen[want] {
			t.Errorf("列表里缺少 %q（实际 %v）—— 两遍改名把分组配的对外名吃掉了", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("应有 3 条，实际 %d 条: %v", len(got), got)
	}
}

// newGroupStore 建一个临时分组存储（测试用）。
func newGroupStore(t *testing.T) *groupstore.Store {
	t.Helper()
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatalf("建分组存储: %v", err)
	}
	return gs
}
