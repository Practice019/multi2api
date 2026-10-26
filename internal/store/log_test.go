package store

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openLogTestDB 本文件的测试库入口（独立文件名/独立符号）。
//
// ⚠ 刻意**不复用**别的测试文件里的 openTestDB：同一个包里多个切片并行写测试时，
// 文件隔离只保证"文件不重叠"，不保证"符号名不撞车"。让 log 的门禁依赖别人的
// 测试助手，等于让"别人改个名字"变成我的门禁变红。log.go 只依赖 db.go 的
// Open/Migrate/Close 与 models.go 的 Log —— 那是冻结契约。
func openLogTestDB(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gw.db")
	if err := Open(path); err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("Migrate(): %v", err)
	}
	t.Cleanup(func() { _ = Close() })
}

// TestWindowAggregateCounts T8 验收第 ② 条：3 条（2 成 1 败）⇒ reqs=3 succ=2。
func TestWindowAggregateCounts(t *testing.T) {
	openLogTestDB(t)
	now := time.Now().Unix()
	for _, e := range []Log{
		{CreatedAt: now, Success: true, LatencyMs: 120, Type: "consume", TotalTokens: 10},
		{CreatedAt: now, Success: true, LatencyMs: 200, Type: "consume", TotalTokens: 20},
		{CreatedAt: now, Success: false, ErrKind: "timeout", Type: "error"},
	} {
		if err := AppendLog(e); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}

	reqs, succ, avg, err := WindowAggregate(now-300, now+1)
	if err != nil {
		t.Fatalf("WindowAggregate: %v", err)
	}
	if reqs != 3 || succ != 2 {
		t.Errorf("应 reqs=3 succ=2，得到 reqs=%d succ=%d", reqs, succ)
	}
	if math.Abs(avg-160) > 0.001 {
		t.Errorf("avg_latency 应为 AVG(120,200)=160，得到 %v", avg)
	}
	if got := SuccessRate(reqs, succ); math.Abs(got-66.666666) > 0.01 {
		t.Errorf("成功率应为 66.67，得到 %v", got)
	}
}

// TestWindowAggregateAvgIgnoresZeroLatency 失败请求往往没记延迟（latency=0），
// 把它们算进分母会把平均延迟**假性拉低**——照 hyxapi 的 use_time > 0 排除。
func TestWindowAggregateAvgIgnoresZeroLatency(t *testing.T) {
	openLogTestDB(t)
	now := time.Now().Unix()
	if err := AppendLog(Log{CreatedAt: now, Success: true, LatencyMs: 100, Type: "consume"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendLog(Log{CreatedAt: now, Success: false, LatencyMs: 0, Type: "error"}); err != nil {
		t.Fatal(err)
	}
	reqs, succ, avg, err := WindowAggregate(now-60, now+1)
	if err != nil {
		t.Fatalf("WindowAggregate: %v", err)
	}
	if reqs != 2 || succ != 1 {
		t.Errorf("reqs/succ 应 2/1，得到 %d/%d", reqs, succ)
	}
	if math.Abs(avg-100) > 0.001 {
		t.Errorf("只有一条 latency=100 的样本，avg 应为 100，得到 %v", avg)
	}
}

// TestAggregatesEmptyTable 验收第 ③ 条：空表三个聚合全 0，
// 且 success_rate **不是 NaN**（NaN 会让 JSON 序列化成 null）。
func TestAggregatesEmptyTable(t *testing.T) {
	openLogTestDB(t)
	now := time.Now().Unix()

	reqs, tokens, cost, err := TodayAggregate(now - 86400)
	if err != nil {
		t.Fatalf("TodayAggregate: %v", err)
	}
	if reqs != 0 || tokens != 0 || cost != 0 {
		t.Errorf("空表今日聚合应全 0，得到 %d/%d/%d", reqs, tokens, cost)
	}
	wReqs, wSucc, avg, err := WindowAggregate(now-300, now)
	if err != nil {
		t.Fatalf("WindowAggregate: %v", err)
	}
	if wReqs != 0 || wSucc != 0 || avg != 0 {
		t.Errorf("空表窗口聚合应全 0，得到 %d/%d/%v", wReqs, wSucc, avg)
	}
	rate := SuccessRate(wReqs, wSucc)
	if rate != 0 {
		t.Errorf("无样本的成功率必须是 0，得到 %v", rate)
	}
	if math.IsNaN(rate) {
		t.Error("成功率成了 NaN —— 序列化成 JSON 会变成 null")
	}
}

// TestTodayAggregate 今日口径：**只在 dayStart 之后**，且 tokens 是入+出两段之和。
func TestTodayAggregate(t *testing.T) {
	openLogTestDB(t)
	now := time.Now().Unix()
	dayStart := now - now%86400 // 随便一个"本地日 0 点"的替身，只要它在测试里稳定

	rows := []Log{
		{CreatedAt: dayStart + 10, Success: true, Type: "consume", PromptTokens: 100, CompletionTokens: 50, CostQuota: 7},
		{CreatedAt: dayStart + 20, Success: true, Type: "consume", PromptTokens: 30, CompletionTokens: 20, CostQuota: 3},
		// 昨天的那条**不该**被算进来
		{CreatedAt: dayStart - 1, Success: true, Type: "consume", PromptTokens: 999, CompletionTokens: 999, CostQuota: 999},
	}
	for _, e := range rows {
		if err := AppendLog(e); err != nil {
			t.Fatalf("AppendLog: %v", err)
		}
	}

	reqs, tokens, cost, err := TodayAggregate(dayStart)
	if err != nil {
		t.Fatalf("TodayAggregate: %v", err)
	}
	if reqs != 2 {
		t.Errorf("今日请求数应为 2（不含昨天那条），得到 %d", reqs)
	}
	if tokens != 200 {
		t.Errorf("今日 tokens 应为 200（100+50+30+20），得到 %d", tokens)
	}
	if cost != 10 {
		t.Errorf("今日消费应为 10（7+3），得到 %d", cost)
	}
}

// TestWindowAggregateZeroWidth 验收第 ⑥ 条：零宽/倒挂窗口 ⇒ 不 panic、返回 0。
func TestWindowAggregateZeroWidth(t *testing.T) {
	openLogTestDB(t)
	now := time.Now().Unix()
	if err := AppendLog(Log{CreatedAt: now, Success: true, LatencyMs: 5, Type: "consume"}); err != nil {
		t.Fatal(err)
	}
	for _, w := range [][2]int64{{now, now}, {now, now - 1}} {
		reqs, succ, avg, err := WindowAggregate(w[0], w[1])
		if err != nil {
			t.Fatalf("窗口 %v: %v", w, err)
		}
		if reqs != 0 || succ != 0 || avg != 0 {
			t.Errorf("零宽窗口 %v 应返回全 0，得到 %d/%d/%v", w, reqs, succ, avg)
		}
	}
	if got := RPMOf(10, now, now); got != 0 {
		t.Errorf("零宽窗口的 RPM 应为 0，得到 %v", got)
	}
}

// TestRPMOfClampsToOneMinute RPM 的分母**下限 1 分钟**（hyxapi 同款）：
// 否则 5 秒窗口里的 1 个请求会被算成 12 RPM，页面数字随刷新剧烈跳动。
func TestRPMOfClampsToOneMinute(t *testing.T) {
	base := int64(1_700_000_000)
	if got := RPMOf(1, base, base+5); got != 1 {
		t.Errorf("5 秒窗口应夹到 1 分钟 ⇒ 1 RPM，得到 %v", got)
	}
	if got := RPMOf(60, base, base+60); got != 60 {
		t.Errorf("60 秒窗口应为 60 RPM，得到 %v", got)
	}
	if got := RPMOf(300, base, base+300); got != 60 {
		t.Errorf("5 分钟窗口应为 60 RPM，得到 %v", got)
	}
}

// TestLogAggregateUsesIndex 是"聚合不能全表扫"的门禁。
//
// 造 1000 行、把时间铺开一年，只查其中一个月的小窗口——这时索引是明显更优的
// 选择，EXPLAIN QUERY PLAN 会说出它用了哪个索引。断言计划文本比断言耗时稳
// （CI 上偶发的一次调度抖动不该让门禁变红），所以这里两者都做：
// 计划必须命中索引，耗时另设一个宽松上限。
func TestLogAggregateUsesIndex(t *testing.T) {
	openLogTestDB(t)
	base := int64(1_700_000_000)
	rows := make([]Log, 0, 1000)
	for i := 0; i < 1000; i++ {
		rows = append(rows, Log{
			CreatedAt: base + int64(i)*86400, // 每天一条，铺满约 3 年
			Success:   i%10 != 0,
			LatencyMs: int64(50 + i%50),
			Type:      "consume",
		})
	}
	if err := DB.CreateInBatches(&rows, 200).Error; err != nil {
		t.Fatalf("批量造数据: %v", err)
	}

	from, to := base+100*86400, base+130*86400
	var plan string
	r := DB.Raw("EXPLAIN QUERY PLAN SELECT COUNT(*) FROM logs WHERE created_at >= ? AND created_at <= ?", from, to).Row()
	if err := r.Scan(&plan); err != nil {
		// EXPLAIN 每一行有多列，Scan 单列会失败；改用 Rows 取最后一段文本
		rows, err := DB.Raw("EXPLAIN QUERY PLAN SELECT COUNT(*) FROM logs WHERE created_at >= ? AND created_at <= ?", from, to).Rows()
		if err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("扫描计划: %v", err)
			}
			for _, v := range vals {
				if s, ok := v.(string); ok {
					plan += s + " "
				}
			}
		}
	}
	if !strings.Contains(plan, "idx_logs") {
		t.Errorf("时间窗查询没有走索引: %s", plan)
	}

	// 索引确实存在（三个：created_at / type / success）
	idx := map[string]bool{}
	ir, err := DB.Raw("SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'logs'").Rows()
	if err != nil {
		t.Fatalf("读索引: %v", err)
	}
	defer ir.Close()
	for ir.Next() {
		var name string
		if err := ir.Scan(&name); err != nil {
			t.Fatalf("扫描索引名: %v", err)
		}
		idx[name] = true
	}
	// created_at 是窗口过滤列、type 是今日消费的过滤列 —— 这两个必须有。
	//
	// ⚠ success **没有独立索引**（models.go 现在只有 idx_log_created / idx_log_type）：
	// 它在聚合里只出现在 SUM(CASE WHEN success THEN 1 ELSE 0 END) 里，不参与过滤，
	// 单列索引对这条查询没有帮助（区分度只有 2 档）。这条已经回报给主脑，
	// 由 models.go 的属主决定要不要加——我不在这里断言一个别人文件里的东西。
	for _, suffix := range []string{"created_at", "type"} {
		found := false
		for name := range idx {
			if strings.Contains(name, suffix) {
				found = true
			}
		}
		if !found {
			t.Errorf("logs 表缺 %s 的索引（现有 %v）", suffix, idx)
		}
	}

	// 先热身一次，再取 5 次的最小值：TestLogAggregateUsesIndex 的耗时门禁
	// 是"量级"判据（< 20ms），不是精确基准。
	if _, _, _, err := WindowAggregate(from, to); err != nil {
		t.Fatalf("WindowAggregate: %v", err)
	}
	best := time.Hour
	for i := 0; i < 5; i++ {
		start := time.Now()
		if _, _, _, err := WindowAggregate(from, to); err != nil {
			t.Fatalf("WindowAggregate: %v", err)
		}
		if d := time.Since(start); d < best {
			best = d
		}
	}
	if best > 20*time.Millisecond {
		t.Errorf("1000 行下窗口聚合最快一次 %v，超过 20ms 门槛（疑似全表扫）", best)
	}
}
