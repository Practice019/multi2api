// sqlbackend_test.go —— B1 的五类验收（SQL 后端的行为守卫）。
//
//	① 写入走 SQL：Upsert 之后能**从库里**读回同样的行（不是内存缓存）
//	② 能力行同事务重建：把能力行删掉 ⇒ 选路为空；再 Upsert 一次 ⇒ 恢复（A3 的反例路径）
//	③ 明文边界：List() 只回掩码（绝不含明文）；Get() 带明文（转发/reveal 依赖它，
//	   见 store.go 文件头；**HTTP 响应面**的判据在 internal/admin 的黑盒守卫里）
//	④ 并发 SetKeyStatus 不丢状态（读-改-写必须在 A1 的 per-channel 锁里）
//	⑤ 历史 JSON 回填幂等（跑两次行数不变）
package channelstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/store"
)

// ① 写入走 SQL：按外部 id 从库里读回的行必须与写进去的一致。
func TestSQLIsAuthoritativeForWrites(t *testing.T) {
	s, _ := newTestStore(t)
	ch := &Channel{
		ID: "ch-abc123", Name: "写库验证", Type: 0,
		BaseURL: "https://dbcheck.example.com/v1",
		Models:  []string{"m-a", "m-b"},
		Groups:  []string{"default", "vip"},
		Keys:    []ChannelKey{{Key: "sk-db-check-0001", Status: StatusEnabled}},
		ChannelInfo: ChannelInfo{
			KeySelectionMode: "round_robin",
			ContextLength:    200000,
			MaxOutputTokens:  8192,
		},
	}
	if err := s.Upsert(ch); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	row, ok, err := store.GetChannelByExternalID("ch-abc123")
	if err != nil || !ok || row == nil {
		t.Fatalf("SQL 里应有 ch-abc123（ok=%v err=%v）", ok, err)
	}
	if row.Name != "写库验证" || row.Group != "default,vip" || row.Models != "m-a,m-b" {
		t.Fatalf("行内容不符: name=%q group=%q models=%q", row.Name, row.Group, row.Models)
	}
	if row.BaseURL == nil || *row.BaseURL != "https://dbcheck.example.com/v1" {
		t.Fatalf("base_url 不符: %v", row.BaseURL)
	}
	// 能力行：2 个模型 × 2 个组 = 4 行（笛卡尔积）。
	abs, err := store.ListAbilities()
	if err != nil {
		t.Fatalf("ListAbilities: %v", err)
	}
	if len(abs) != 4 {
		t.Fatalf("能力行应 4 条（2 模型 × 2 组），实际 %d", len(abs))
	}
	// 往返：Get 出来的对象与写进去的语义一致（含 extras 与选择模式）。
	got, ok := s.Get("ch-abc123")
	if !ok {
		t.Fatal("Get 取不到")
	}
	if got.ChannelInfo.ContextLength != 200000 || got.ChannelInfo.MaxOutputTokens != 8192 {
		t.Fatalf("extras 往返丢了: %+v", got.ChannelInfo)
	}
	if got.ChannelInfo.KeySelectionMode != "round_robin" {
		t.Fatalf("选择模式往返错: %q（应为 round_robin ⇄ polling 翻译）", got.ChannelInfo.KeySelectionMode)
	}
	if len(got.Groups) != 2 {
		t.Fatalf("Groups 往返应为 2 个，实际 %v", got.Groups)
	}
}

// ② 能力行同事务重建（A3 给的反例形态）：删掉能力行 ⇒ 选路为空；Upsert 后恢复。
func TestAbilitiesRebuiltOnUpsert(t *testing.T) {
	s, _ := newTestStore(t)
	ch := ch("ch-ab", "m-a")
	ch.BaseURL = "https://ab.example.com/v1"
	if err := s.Upsert(ch); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := s.Select(DefaultGroup, "m-a", 0); err != nil {
		t.Fatalf("前置：应能选到渠道，实际 %v", err)
	}
	row, _, _ := store.GetChannelByExternalID("ch-ab")
	if err := store.DeleteAbilities(nil, row.ID); err != nil {
		t.Fatalf("删能力行: %v", err)
	}
	if _, err := s.Select(DefaultGroup, "m-a", 0); err == nil {
		t.Fatal("能力行被删后选路必须为空（否则说明读的不是能力表）")
	}
	if err := s.Upsert(ch); err != nil {
		t.Fatalf("二次 Upsert: %v", err)
	}
	if _, err := s.Select(DefaultGroup, "m-a", 0); err != nil {
		t.Fatalf("二次 Upsert 应在同一事务里重建能力行，实际 %v", err)
	}
}

// ③ 明文边界：List 只回掩码（绝不含明文）；Get 带明文（明文入口）。
func TestPlaintextBoundary(t *testing.T) {
	s, _ := newTestStore(t)
	const secret = "sk-boundary-secret-0001"
	ch := ch("ch-plain", "m-a")
	ch.Keys = []ChannelKey{{Key: secret, Status: StatusEnabled}}
	if err := s.Upsert(ch); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// List：**必须**只有掩码。这是被 30 秒轮询的接口，明文漏出去等于每次轮询发一遍凭据。
	list := s.List()
	if len(list) != 1 {
		t.Fatalf("List 应 1 条，实际 %d", len(list))
	}
	for _, k := range list[0].Keys {
		if k.Key == secret || strings.Contains(k.Key, "secret") {
			t.Fatalf("★ List 回出了明文: %q", k.Key)
		}
		if k.Key == "" {
			t.Fatal("List 的掩码不该是空串（界面要显示 N 个 · sk-a…xyz）")
		}
	}
	// KeysView：同样只回掩码（它是 Key 管理面板的数据源）。
	views, err := s.KeysView("ch-plain")
	if err != nil {
		t.Fatalf("KeysView: %v", err)
	}
	if len(views) != 1 || views[0].Mask == secret {
		t.Fatalf("KeysView 必须只回掩码，实际 %+v", views)
	}

	// Get：带明文 —— 这是 reveal / keys?reveal=1 / 自动发现三条显式路径唯一依赖的入口。
	got, ok := s.Get("ch-plain")
	if !ok {
		t.Fatal("Get 取不到")
	}
	if len(got.Keys) != 1 || got.Keys[0].Key != secret {
		t.Fatalf("Get 应带明文（转发要靠它发请求），实际 %+v", got.Keys)
	}
}

// ④ 并发 SetKeyStatus 不丢状态：每个 goroutine 只动自己那把 key，结束时**两把都必须是改过的状态**。
//
// 这条守的是"读-改-写是否在锁里"：不加锁时两个并发请求会互相覆盖，
// 表现是"我明明禁了两把，刷新后只有一把是禁的"（丢了更新，且不会有任何报错）。
func TestConcurrentSetKeyStatusKeepsBothChanges(t *testing.T) {
	s, _ := newTestStore(t)
	ch := ch("ch-conc", "m-a")
	ch.Keys = []ChannelKey{
		{Key: "sk-conc-0001", Status: StatusEnabled},
		{Key: "sk-conc-0002", Status: StatusEnabled},
	}
	if err := s.Upsert(ch); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := s.SetKeyStatus("ch-conc", idx, StatusManuallyDisabled, ReasonManual); err != nil {
				t.Errorf("SetKeyStatus(%d): %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
	views, err := s.KeysView("ch-conc")
	if err != nil {
		t.Fatalf("KeysView: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("key 数应为 2，实际 %d", len(views))
	}
	for i, v := range views {
		if v.Status != StatusManuallyDisabled || v.Reason != ReasonManual {
			t.Errorf("第 %d 把 key 的状态丢了（并发覆盖）: status=%d reason=%q", i, v.Status, v.Reason)
		}
	}
}

// ⑤ 历史 JSON 回填幂等：第一次 New 导入，第二次 New 不再导入（行数不变）。
func TestBackfillIsIdempotent(t *testing.T) {
	openTestDB(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "channels.json")
	// 用真实的历史落盘形状（snapshot）造两条渠道。
	snap := map[string]any{
		"version": 1,
		"channels": []map[string]any{
			{"id": "ch-legacy-a", "name": "历史A", "type": 0, "base_url": "https://a.legacy.invalid/v1",
				"models": []string{"m-a"}, "keys": []map[string]any{{"key": "sk-legacy-a", "status": 1}}},
			{"id": "ch-legacy-b", "name": "历史B", "type": 0, "base_url": "https://b.legacy.invalid/v1",
				"models": []string{"m-b"}, "keys": []map[string]any{{"key": "sk-legacy-b", "status": 1}}},
		},
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("造历史文件: %v", err)
	}
	if err := os.WriteFile(legacy, raw, 0o600); err != nil {
		t.Fatalf("写历史文件: %v", err)
	}

	s1, err := New(legacy)
	if err != nil {
		t.Fatalf("首次 New: %v", err)
	}
	n1, err := store.CountChannels()
	if err != nil {
		t.Fatalf("CountChannels: %v", err)
	}
	if n1 != 2 {
		t.Fatalf("回填后应有 2 行，实际 %d", n1)
	}
	if _, ok := s1.Get("ch-legacy-a"); !ok {
		t.Fatal("回填后按外部 id 应能取到（原样保留 ch-* id）")
	}
	// 第二次 New（模拟重启）：表非空 ⇒ 跳过回填，行数不变（不是"又导一遍"）。
	if _, err := New(legacy); err != nil {
		t.Fatalf("二次 New: %v", err)
	}
	n2, err := store.CountChannels()
	if err != nil {
		t.Fatalf("CountChannels: %v", err)
	}
	if n2 != n1 {
		t.Fatalf("回填不幂等：第一次 %d 行，第二次 %d 行", n1, n2)
	}
}
