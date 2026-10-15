// points_budget_test.go 「超时预算必须按账号切分」的守卫。
//
// # 这个缺陷的形态（用户截图）
//
// 新手任务/邀请码面板里 35 个账号**只有前 3 个**有数据，其余全是
// 「查询失败」+ 余额 `—`。而**单独手动打那几个"失败"的账号完全正常** ——
// 因为手动那一次拿到了完整的 20 秒。
//
// 根因：面板在循环**外**建一次 ctx、整个循环共用：
//
//	ctx, cancel := pointsCtx()   // 20 秒，但这 20 秒是**所有账号加起来**的
//	for _, a := range accts {    // 35 个账号 × 每个 2 次请求 = 70 次
//		tasks, err := p.Tasks(ctx, a.Session)
//	}
//
// 上游单次 0.2~23.7 秒 ⇒ 预算在前 3~5 个账号就被吃光 ⇒ 其余全部
// `context deadline exceeded`。
package loomy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// ⚠⚠ **行为守卫**：N 个账号、每个都要一点时间 ⇒ 必须**全部**查到。
//
// 判别性来自"总耗时 > 预算"：`pointsTimeout` 压到 300ms，10 个账号每个延迟
// 60ms（合计 600ms）。共用一份预算的旧实现会在第 ~5 个账号之后全部超时；
// 按账号切分的实现 10 个全过。
//
// 这条判据是"跑得出来"的 —— 换用真实的 20 秒去重现同一个形态要跑一分钟，
// 那种测试没人会跑，等于没有。
func TestTasksListGivesEachAccountItsOwnBudget(t *testing.T) {
	old := pointsTimeout
	pointsTimeout = 300 * time.Millisecond
	defer func() { pointsTimeout = old }()

	const n = 10
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 每个账号的查询都要花点时间：让总耗时超过一份预算。
		time.Sleep(60 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/onboarding/tasks"):
			// ⚠ tasks 必须是 **map**（`map[string]bool`），给数组会让
			// Tasks 返回「回执里没有 tasks 字段」——那会把这条测试的语义
			// 从"超时预算"变成"回执形状错了"，判据就不再判别了。
			fmt.Fprint(w, `{"code":"000000","desc":"ok","data":{"tasks":{"configure_remote":true}}}`)
		default:
			fmt.Fprint(w, `{"code":"000000","desc":"ok","data":{"balance":100,"dailyBalance":50,"availableBalance":150}}`)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("2609%014d", i)
		sess := fmt.Sprintf("%032x", i)
		writeFile(t, dir, fmt.Sprintf("loomy-%d.json", i),
			fmt.Sprintf(`{"session":"%s","userid":"%s"}`, sess, uid))
	}
	p := NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir})

	r := httptest.NewRequest(http.MethodGet, "/admin/loomy/tasks", nil)
	w := httptest.NewRecorder()
	p.handleTasksList(w, r)

	var resp struct {
		Accounts []struct {
			UID   string `json:"uid"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("回执不是合法 JSON: %v（原文 %s）", err, w.Body.String()[:min(200, w.Body.Len())])
	}
	if len(resp.Accounts) != n {
		t.Fatalf("回执行数 = %d，期望 %d", len(resp.Accounts), n)
	}
	okN := 0
	for _, a := range resp.Accounts {
		if a.OK {
			okN++
			continue
		}
		t.Errorf("★ 账号 %s 查询失败: %s", a.UID, a.Error)
	}
	if okN != n {
		t.Errorf("★ %d/%d 个账号查到了（期望全部 %d）——\n"+
			"\n"+
			"这是"+"「超时预算被所有账号共用」"+"的形态：\n"+
			"`ctx, cancel := pointsCtx()` 建在循环**外** ⇒ 那 20 秒是\n"+
			"**所有账号加起来**的预算 ⇒ 上游一慢，预算在前几个账号就被吃光 ⇒\n"+
			"其余全部 `context deadline exceeded`。\n"+
			"\n"+
			"用户实测：35 个账号只有前 3 个有数据，界面显示「查询失败」+ 余额 —，\n"+
			"而**手动单查那几个账号完全正常**（它拿到了完整的预算）。\n"+
			"\n"+
			"修法：每个账号一份独立预算 —— 见 withPointsTimeout。",
			okN, n, n)
	}
}

// 源码级守：扇出型 handler 不许在循环外建 ctx。
//
// 行为测试覆盖的是 `handleTasksList`；这条覆盖**同型的另外两处**
// （`handleInviteList`、`handleTasksAutoAll`）—— 它们不是可以靠
// "记得改"来保证的：少改一处，那个面板就会重演同一个形态，
// 而且不报任何错，只是列表里一片"查询失败"。
func TestFanOutHandlersDoNotShareOneBudget(t *testing.T) {
	raw, err := readLoomyGo(t, "adminpoints.go")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(raw, "\r\n", "\n")

	for _, fn := range []string{"handleTasksList", "handleInviteList", "handleTasksAutoAll"} {
		start := strings.Index(src, "func (p *Provider) "+fn+"(")
		if start < 0 {
			t.Errorf("找不到 %s（改名了？那这条守卫要跟着改）", fn)
			continue
		}
		body := src[start:]
		if end := strings.Index(body, "\n}\n"); end > 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "withPointsTimeout") {
			t.Errorf("★ %s 里没有 withPointsTimeout ——\n"+
				"它遍历多个账号，必须在**每个账号**上建一份超时预算。\n"+
				"共用一份 ⇒ 前面的账号把预算吃光 ⇒ 后面全部报「查询失败」，\n"+
				"而手动单查同一个账号却正常（用户实测的形态）。", fn)
		}
		if strings.Contains(body, "ctx, cancel := pointsCtx()") {
			t.Errorf("★ %s 里出现了循环外共用的 `ctx, cancel := pointsCtx()` ——\n"+
				"那就是本次缺陷本身。请改用 withPointsTimeout。", fn)
		}
		// ⚠ 扇出还必须**限流并发**。
		//
		// 串行的实测代价：35 个账号 × 2 次请求，而上游单次 0.2~23.7 秒
		//（同一账号同一端点，差 90 倍）⇒ 串行总和 272 秒，面板一直显示
		//「加载中…」，用户以为坏了。
		//
		// ⚠ 这条判据是对"串行"的**回归**：把 fanOutConcurrent 去掉会让面板
		// 重新变成 4 分半，而功能上"仍然正确"—— 所以只有这条源码级判据能抓到它。
		if !strings.Contains(body, "fanOutConcurrent") {
			t.Errorf("★ %s 里没有 fanOutConcurrent ——\n"+
				"串行跑 35 个账号（每个 2 次上游请求，单次 0.2~23.7 秒）要 3~4 分钟，\n"+
				"面板会一直显示「加载中…」。用户的评价是「看起来坏了」。\n"+
				"并发上限见 loomyPanelConcurrency（实测 12 条并发上游就开始 502）。", fn)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// readLoomyGo 读本包的一个源文件（源码级判据用）。
//
// ⚠ 归一化行尾再比对：工作树里是 CRLF，而锚点按 LF 写 ——
// 不归一化的话 strings.Contains 会静默不命中（看起来像"函数被改名了"）。
func readLoomyGo(t *testing.T, name string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n"), nil
}
