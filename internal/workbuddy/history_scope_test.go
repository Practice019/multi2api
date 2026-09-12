// history_scope_test.go 任务历史的**出口**归属（T4）。
//
// # 它补的是哪一层
//
// T1 修的是"作用域错误"的**入口**（7 处取号 + 2 处快照出口 + 1 处判据），
// 结论是"不再产生新的越界记录"。但历史文件是**只写不删**的：
// 修好之前写进去的记录仍在，而 /admin/checkin/history 原先原样返回全量。
//
// 实测（2026-09-13，18080 实例）：全量 5000 条里有 766~815 条 uid 是
// `01a08fe0`（codearts 账号），占比约 16%。也就是说用户打开「任务历史」
// 看到的每 6 条里就有 1 条属于一个他甚至没在 workbuddy 分组里添加过的账号。
//
// 所以 T4 在**出口**再收一次口 —— 与 T1 的结论同一条规则（入口/出口/判据）。
//
// # 为什么这套用例必须往池里塞一个**别家**账号
//
// 与 upstream_isolation_test.go 同一个理由：既有的历史测试
//（admin_routes_test.go 的 newHistoryProvider）只造了一个上游，
// "过滤"与"不过滤"的结果完全相同 —— 过滤写错在那里**不可见**。
// 本文件每条用例都先塞别家账号，再断言它不出现在响应里。
package workbuddy

import (
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
)

// seedHistoryWithForeign 造一个双上游池 + 一份**混着两家 uid**的历史。
//
// own 个本上游记录 + foreign 个别家上游记录，交替写入以确保排序交错
//（过滤若写成"只取前 N 条"或"按位置切"就会露馅）。
func seedHistoryWithForeign(t *testing.T, ownN, foreignN int) (*Provider, string) {
	t.Helper()
	p, foreign := seedMultiProviderPool(t, testAuthNamed("wb1", "甲"), testAuthNamed("wb2", "乙"))
	prov := newIsolationProvider(t, p)
	lg := prov.cfg.Log
	if lg == nil {
		t.Fatal("装置失效：Provider 未接历史日志")
	}

	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	n := ownN
	if foreignN > n {
		n = foreignN
	}
	for i := 0; i < n; i++ {
		if i < ownN {
			uid := "wb1"
			if i%2 == 1 {
				uid = "wb2"
			}
			lg.Append(checkinlog.Record{
				At: base.Add(time.Duration(i*2) * time.Minute), UID: uid,
				Kind: checkinlog.KindCheckin, Status: checkinlog.StatusOK,
			})
		}
		if i < foreignN {
			lg.Append(checkinlog.Record{
				At: base.Add(time.Duration(i*2+1) * time.Minute), UID: foreign.UID,
				Kind: checkinlog.KindCheckin, Status: checkinlog.StatusOK,
			})
		}
	}
	return prov, foreign.UID
}

// historyUIDs 取一页历史响应里的 uid 列表。
func historyUIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, _ := body["items"].([]any)
	out := make([]string, 0, len(items))
	for _, raw := range items {
		m, _ := raw.(map[string]any)
		uid, _ := m["uid"].(string)
		out = append(out, uid)
	}
	return out
}

// TestHistoryExcludesForeignUpstream 历史出口不得返回别家上游的记录。
//
// 这是 T4 的核心断言。去掉 History 里的 ownUIDs 过滤会让它立刻变红 ——
// 而且**不是**靠条数变红：即使别家记录只有 1 条，它也会出现在 uid 列表里。
func TestHistoryExcludesForeignUpstream(t *testing.T) {
	prov, foreignUID := seedHistoryWithForeign(t, 4, 3)
	srv := newAdminTestServer(t, prov)

	body := doHistory(t, srv, "?limit=50")
	uids := historyUIDs(t, body)
	t.Logf("返回 uid: %v（本上游应有 4 条，别家 3 条应被过滤）", uids)

	for _, u := range uids {
		if u == foreignUID {
			t.Errorf("历史返回了别家上游账号 %s 的记录 —— "+
				"这正是那 766 条脏数据的形态（入口已修，出口未收口）", u)
		}
	}
	if got := len(uids); got != 4 {
		t.Errorf("本页条数=%d，期望 4（只有本上游的记录）", got)
	}
	if got, _ := body["total"].(float64); got != 4 {
		t.Errorf("total=%v，期望 4 —— total 必须是**过滤后**的数，"+
			"否则前端算出的页数会指向不存在的页", got)
	}
}

// TestHistoryTotalIsPostFilter 钉住"total 是过滤后的数"。
//
// # 为什么单独一条（它不是上一条的重复）
//
// 上一条可以靠"条数恰好对上"通过，而那掩盖不了顺序错误：
// 若实现是"先分页再过滤"，total 会是过滤**前**的 7，但本页条数可能仍是 4。
// 前端拿 7 算页数 → 用户翻到第 2 页看到空表。这条断言把 total 单独钉住。
func TestHistoryTotalIsPostFilter(t *testing.T) {
	prov, _ := seedHistoryWithForeign(t, 5, 9)
	srv := newAdminTestServer(t, prov)

	body := doHistory(t, srv, "?limit=3&offset=0")
	if got, _ := body["total"].(float64); got != 5 {
		t.Errorf("total=%v，期望 5（过滤后：本上游 5 条）—— "+
			"若为 14 说明过滤发生在分页之后", got)
	}
	if got := len(historyUIDs(t, body)); got != 3 {
		t.Errorf("本页条数=%d，期望 3（limit 生效）", got)
	}
}

// TestHistoryPaginationOverFilteredSet 分页必须在**过滤后**的集合上做。
//
// 构造：本上游 5 条、别家 9 条，时间交错。
// 逐页取完，断言：①一页不漏不重 ②永远不出现别家 uid ③总数 == total。
//
// # 为什么这条能抓到"过滤与分页顺序颠倒"
//
// 若先切页再过滤，第一页可能整页都是别家记录 → 过滤后变成空页
//（"翻页翻着翻着没了"）；且取到的总条数会少于 total。
func TestHistoryPaginationOverFilteredSet(t *testing.T) {
	prov, foreignUID := seedHistoryWithForeign(t, 5, 9)
	srv := newAdminTestServer(t, prov)

	body := doHistory(t, srv, "?limit=2&offset=0")
	total := int(body["total"].(float64))
	if total != 5 {
		t.Fatalf("total=%d，期望 5", total)
	}

	seen := map[string]int{}
	order := []string{}
	for off := 0; off < total+2; off += 2 {
		page := doHistory(t, srv, "?limit=2&offset="+itoa(off))
		for _, u := range historyUIDs(t, page) {
			if u == foreignUID {
				t.Fatalf("offset=%d 的页里出现别家 uid %s", off, u)
			}
			seen[u]++
			order = append(order, u)
		}
	}
	if len(seen) != 2 {
		t.Errorf("遍历后出现的 uid 种类=%d，期望 2（wb1/wb2）", len(seen))
	}
	sum := 0
	for _, n := range seen {
		sum += n
	}
	if sum != total {
		t.Errorf("逐页取到的总条数=%d，期望 %d（分页有丢或重）", sum, total)
	}
	t.Logf("逐页顺序: %v", order)
}

// TestHistoryNoAccountsDegradesToUnfiltered 池未接线时不过滤（语义与 accountList 一致）。
//
// # 这是**刻意的降级**，不是漏洞
//
// ownUIDs() 在池未接线时返回 nil，语义是"不过滤"。返回空 map 反而会把
// 所有记录都藏起来（过度过滤）。本用例把这个选择钉住 ——
// 否则将来有人"顺手"把 nil 改成空 map，表现是"未配置账号池的部署里
// 历史整片面消失"，而且不报错。
func TestHistoryNoAccountsDegradesToUnfiltered(t *testing.T) {
	lg := newIsolationLog(t)
	lg.Append(checkinlog.Record{At: time.Now(), UID: "whoever", Kind: checkinlog.KindCheckin})
	// 刻意不接池。
	prov := NewWithConfig(Config{Log: lg})
	srv := newAdminTestServer(t, prov)

	body := doHistory(t, srv, "?limit=10")
	if got, _ := body["total"].(float64); got != 1 {
		t.Errorf("池未接线时 total=%v，期望 1（不过滤）—— "+
			"返回空 map 会让历史整片消失，那是过度过滤", got)
	}
}

// TestHistoryKindFilterStillWorks 服务端的 kind 过滤不能被这次改动弄丢。
func TestHistoryKindFilterStillWorks(t *testing.T) {
	p, _ := seedMultiProviderPool(t, testAuthNamed("wb1", "甲"))
	prov := newIsolationProvider(t, p)
	lg := prov.cfg.Log
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		k := checkinlog.KindCheckin
		if i%2 == 0 {
			k = checkinlog.KindTravel
		}
		lg.Append(checkinlog.Record{At: base.Add(time.Duration(i) * time.Minute), UID: "wb1", Kind: k})
	}
	srv := newAdminTestServer(t, prov)

	body := doHistory(t, srv, "?limit=30&kind=travel")
	if got, _ := body["total"].(float64); got != 5 {
		t.Errorf("kind=travel 的 total=%v，期望 5", got)
	}
	for _, raw := range body["items"].([]any) {
		if m, _ := raw.(map[string]any); m["kind"] != "travel" {
			t.Errorf("kind 过滤失效，混入 %v", m["kind"])
		}
	}
}

// TestHistoryNewestFirstAfterFilter 过滤不得打乱时间倒序。
func TestHistoryNewestFirstAfterFilter(t *testing.T) {
	prov, _ := seedHistoryWithForeign(t, 6, 5)
	srv := newAdminTestServer(t, prov)

	body := doHistory(t, srv, "?limit=20")
	var prev time.Time
	for i, raw := range body["items"].([]any) {
		m, _ := raw.(map[string]any)
		at, err := time.Parse(time.RFC3339Nano, m["at"].(string))
		if err != nil {
			t.Fatalf("解析 at 失败: %v", err)
		}
		if i > 0 && at.After(prev) {
			t.Errorf("第 %d 条比第 %d 条更新 —— 过滤后不再是时间倒序", i, i-1)
		}
		prev = at
	}
}

// itoa 小工具（避免为一个整数引入 strconv 到断言消息里）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}
