// stats_db_source_test.go 钉住「统计优先读库」这条契约（2026-11-10）。
//
// # 用户报障
//
//	「总 Token 10774.1M 这个数值不对吧？」
//	「一天都有 40 亿左右，怎么可能全部只有百亿呢？」
//
// 实测确认他说得对：`/admin/stats` 只读 **JSONL 文件**，而那个文件按体积裁剪
// ⇒ 只剩 2.3 天（107.8 亿）；`logs` 表里有 8 天 / 13.6 万行（**274.2 亿**）。
// 用户原话：「有数据库之后，统一都用数据库的得了。」
//
// 本文件验证：① 库里有数据时 statsItems 走 db；② 字段映射与聚合口径一致；
// ③ 覆盖范围被如实报出（这是"这个数覆盖了多久"唯一的可读来源）。
package admin

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
	"workbuddy2api/internal/store"
)

// seedStatsStore 造一个有历史的库：N 条请求，其中 1 条失败。
func seedStatsStore(t *testing.T, rows []store.Log) {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// ⚠ 必须注册关闭（与同包其它用例同款）：`t.TempDir()` 的清理在**用例结束后**跑，
	//   而它在 Windows 上**删不掉仍被句柄占用的 gw.db** ⇒ 报
	//   「TempDir RemoveAll cleanup: ... being used by another process」⇒ 用例被判 FAIL。
	//   这个失败在 Linux 上**看不见**（unlink 打开中的文件会成功）⇒ CI(ubuntu) 全绿，
	//   只有本机 Windows 会红 —— 这类"只在一边红"的缺口最容易被漏过去。
	//   t.Cleanup 是 LIFO：本行晚于 TempDir 注册 ⇒ 先关句柄、再删目录 ✓。
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	for i := range rows {
		if err := store.AppendLog(rows[i]); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}
}

// TestStatsItemsPrefersDB 库里有数据时必须走 db，而不是文件/内存。
func TestStatsItemsPrefersDB(t *testing.T) {
	now := time.Now()
	seedStatsStore(t, []store.Log{
		{CreatedAt: now.Add(-2 * time.Hour).Unix(), ModelName: "loomy/A", AccountUID: "u1",
			TotalTokens: 1000, CompletionTokens: 100, CacheHitTokens: 900, Status: 200,
			LatencyMs: 1200, TTFBMs: 300, Provider: "loomy", Success: true},
		{CreatedAt: now.Add(-1 * time.Hour).Unix(), ModelName: "loomy/A", AccountUID: "u2",
			TotalTokens: 2000, CompletionTokens: 200, CacheMissTokens: 1800, Status: 502,
			LatencyMs: 500, TTFBMs: 100, Provider: "loomy", Success: false},
	})

	h := &Handler{}
	items, source, err := h.statsItems()
	if err != nil {
		t.Fatalf("statsItems: %v", err)
	}
	if source != "db" {
		t.Fatalf("★ 有库时 source = %q，想要 \"db\"（否则统计又在读会被裁剪的文件）", source)
	}
	if len(items) != 2 {
		t.Fatalf("条数 = %d，想要 2", len(items))
	}
	// 字段映射必须逐项到位 —— 少一个就会让某个维度静默变成 0。
	got := items[1]
	if got.Model != "loomy/A" || got.UID != "u2" || got.Provider != "loomy" {
		t.Errorf("映射错了：model=%q uid=%q provider=%q", got.Model, got.UID, got.Provider)
	}
	if got.Status != 502 || got.Tokens != 2000 || got.OutTokens != 200 {
		t.Errorf("映射错了：status=%d tokens=%d out=%d", got.Status, got.Tokens, got.OutTokens)
	}
	if got.TTFBMS != 100 || got.TotalMS != 500 {
		t.Errorf("映射错了：ttfb=%d total=%d", got.TTFBMS, got.TotalMS)
	}
	if got.CacheMissTokens != 1800 {
		t.Errorf("缓存字段映射错了：miss=%d", got.CacheMissTokens)
	}

	// 覆盖范围必须能被报出来（本次实现的核心可观测性要求）。
	cov := h.statsCoverage()
	if cov["count"] != 2 {
		t.Errorf("coverage.count = %v，想要 2", cov["count"])
	}
	if cov["from"] == "" || cov["to"] == "" {
		t.Errorf("coverage 缺起止时间：%v", cov)
	}
	if d, _ := cov["days"].(float64); d <= 0 {
		t.Errorf("coverage.days = %v，想要 >0（覆盖天数必须报出来）", cov["days"])
	}
}

// TestStatsAggregateCountsDBTokens 聚合必须把库里的 token 加全。
//
// 这是用户问的那个数：107.8 亿 vs 274.2 亿，差的正是"没被读到的那些行"。
func TestStatsAggregateCountsDBTokens(t *testing.T) {
	now := time.Now()
	var rows []store.Log
	const n = 50
	for i := 0; i < n; i++ {
		rows = append(rows, store.Log{
			CreatedAt: now.Add(-time.Duration(i) * time.Minute).Unix(),
			ModelName: "loomy/A", AccountUID: "u1", TotalTokens: 1_000_000,
			CompletionTokens: 1000, Status: 200, Provider: "loomy", Success: true,
		})
	}
	seedStatsStore(t, rows)

	h := &Handler{}
	items, _, err := h.statsItems()
	if err != nil {
		t.Fatalf("statsItems: %v", err)
	}
	agg := aggregateChatLog(items)
	tok, _ := agg["tokens"].(int64)
	if tok != n*1_000_000 {
		t.Errorf("★ 聚合 tokens = %d，想要 %d —— 库里的行没被加全", tok, int64(n)*1_000_000)
	}
}

// TestStatsFallsBackWhenDBEmpty 库为空时必须回落（未开库/新部署不该让面板空着）。
func TestStatsFallsBackWhenDBEmpty(t *testing.T) {
	// 显式不打开库：模拟"还没迁移到 SQL"的部署。
	store.DB = nil
	defer func() { store.DB = nil }()

	h := &Handler{}
	ring := logbuf.New(8)
	h.cfg.Ring = ring
	ring.Push(logbuf.Entry{At: time.Now(), Model: "m", Status: 200, Tokens: 5})

	items, source, err := h.statsItems()
	if err != nil {
		t.Fatalf("statsItems 不该因未开库报错: %v", err)
	}
	if source == "db" {
		t.Fatalf("库为 nil 时不该报 source=db")
	}
	if len(items) != 1 {
		t.Errorf("应回落到内存缓冲的 1 条，得到 %d 条", len(items))
	}
}

// TestStatsDBDerivesLegacyStatusAndProvider 历史行的 status / provider 必须被回推。
//
// # 为什么需要（2026-11-10 实测）
//
// 这两列都是「统计统一读库」时才加的 ⇒ 之前 12.8 万行是零值。不回推的后果实测：
//
//	ok=8, fail=0（total=128694）        ← status=0 落在 `>=200` 与 `>=400` 之外，两边都不计
//	by_provider 的 "(未标注)" = 128302  ← 99.5% 的用量归不到上游，整块维度不可用
//
// 回推规则必须与网关**产出**这两个字段的规则逐字一致，否则历史段与新段的口径会分叉。
func TestStatsDBDerivesLegacyStatusAndProvider(t *testing.T) {
	now := time.Now()
	seedStatsStore(t, []store.Log{
		// 成功 + 无 provider：应从 success 回推 200、从模型前缀回推 loomy。
		{CreatedAt: now.Add(-3 * time.Minute).Unix(), ModelName: "loomy/DeepSeek-V4.1-Flash",
			AccountUID: "u1", TotalTokens: 100, Status: 0, Success: true},
		// 失败 + 无 provider：status 回推 500（不是 502 —— 具体码不可考，不编归因）。
		{CreatedAt: now.Add(-2 * time.Minute).Unix(), ModelName: "workbuddy/deepseek-v4.1-flash",
			AccountUID: "u2", TotalTokens: 200, Status: 0, Success: false},
		// 渠道行：provider 必须回推成 "channel:"+渠道名（核心就是这么标的）。
		{CreatedAt: now.Add(-1 * time.Minute).Unix(), ModelName: "whatever",
			ChannelName: "ch-abc", AccountUID: "u3", TotalTokens: 300, Status: 0, Success: true},
	})

	h := &Handler{}
	items, _, err := h.statsItems()
	if err != nil {
		t.Fatalf("statsItems: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("条数 = %d，想要 3", len(items))
	}
	if items[0].Status != 200 {
		t.Errorf("成功行 status 回推 = %d，想要 200", items[0].Status)
	}
	if items[1].Status != 500 {
		t.Errorf("失败行 status 回推 = %d，想要 500（不可考的具体码不许编）", items[1].Status)
	}
	if items[0].Provider != "loomy" {
		t.Errorf("provider 回推 = %q，想要 loomy（取模型前缀）", items[0].Provider)
	}
	if items[1].Provider != "workbuddy" {
		t.Errorf("provider 回推 = %q，想要 workbuddy", items[1].Provider)
	}
	if items[2].Provider != "channel:ch-abc" {
		t.Errorf("渠道行 provider 回推 = %q，想要 channel:ch-abc", items[2].Provider)
	}

	// 回推量必须被报出来 —— 否则读者会以为 by_status 里那个 500 桶是真错误码。
	if cov := h.statsCoverage(); cov["status_derived"] != 3 {
		t.Errorf("coverage.status_derived = %v，想要 3", cov["status_derived"])
	}

	// 聚合口径必须自洽：ok+fail == total，三个维度求和都等于 total。
	agg := aggregateChatLog(items)
	tot, _ := agg["total"].(int)
	ok, _ := agg["ok"].(int64)
	fail, _ := agg["fail"].(int64)
	if tot != 3 || int(ok+fail) != tot {
		t.Errorf("aggregate: total=%d ok=%d fail=%d（ok+fail 必须等于 total）", tot, ok, fail)
	}
}
