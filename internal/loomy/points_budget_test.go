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

// 源码级守：「每个账号一份独立预算」+「限流并发」两条都不能少。
//
// # ⚠ 这条判据**随结构演进而改过一次**（2026-10-15）
//
//	改造前  withPointsTimeout / fanOutConcurrent 直接写在 handler 体内
//	改造后  两者搬进了 tasksViewFor / inviteViewFor（handler 只负责分页、
//	        缓存与 HTTP 形态）
//
// 于是原来"在 handler 里找 withPointsTimeout"的判据不再成立 —— 而**缺陷
// 本身仍然可能回来**（谁把取数函数里的 withPointsTimeout 去掉，面板就会
// 重演"前几个账号把 20 秒吃光"）。
//
// ⚠ 这次守卫之所以有价值，正是因为它**在我重构之后立刻变红了** —— 它挡住了
// "改了结构、判据失效、于是缺陷可以悄悄回来"这条路。改编据时要按**职责**找，
// 不要按位置找，否则下次搬代码它又失效。
//
//	handler      → 必须限流并发（fanOutConcurrent），且不许出现共用的 pointsCtx()
//	取数函数      → 必须每个账号一份独立预算（withPointsTimeout）
func TestFanOutHandlersDoNotShareOneBudget(t *testing.T) {
	src, err := readLoomyGo(t, "adminpoints.go")
	if err != nil {
		t.Fatal(err)
	}

	bodyOf := func(mark string) string {
		i := strings.Index(src, mark)
		if i < 0 {
			return ""
		}
		body := src[i:]
		if end := strings.Index(body, "\n}\n"); end > 0 {
			body = body[:end]
		}
		return body
	}

	// ① 三个扇出 handler：必须限流并发，且不许出现循环外共用的 pointsCtx()。
	for _, fn := range []string{"handleTasksList", "handleInviteList", "handleTasksAutoAll"} {
		body := bodyOf("func (p *Provider) " + fn + "(")
		if body == "" {
			t.Errorf("找不到 %s（改名了？那这条守卫要跟着改，否则它会静默失效）", fn)
			continue
		}
		if !strings.Contains(body, "fanOutConcurrent") {
			t.Errorf("★ %s 里没有 fanOutConcurrent ——\n"+
				"它遍历多个账号，必须限流并发。\n"+
				"串行跑 35 个账号（每个 2 次上游请求，单次 0.2~23.7 秒）要 3~4 分钟，\n"+
				"面板会一直显示「加载中…」，用户的评价是「看起来坏了」。\n"+
				"并发上限见 loomyPanelConcurrency（实测 12 条并发上游就开始 502）。", fn)
		}
		if strings.Contains(body, "ctx, cancel := pointsCtx()") {
			t.Errorf("★ %s 里出现了循环外共用的 `ctx, cancel := pointsCtx()` ——\n"+
				"那就是本次缺陷本身：那 20 秒是**所有账号加起来**的预算，\n"+
				"上游一慢，预算在前 3~5 个账号就被吃光，其余全部\n"+
				"`context deadline exceeded`（用户看到的就是「几乎全部查询失败」）。", fn)
		}
	}

	// ② 两个取数函数：必须**每个账号一份独立预算**。
	for _, fn := range []string{"tasksViewFor", "inviteViewFor"} {
		body := bodyOf("func (p *Provider) " + fn + "(")
		if body == "" {
			t.Errorf("找不到 %s —— 若它被删/改名，这条守卫会静默失效，必须同步改", fn)
			continue
		}
		if !strings.Contains(body, "withPointsTimeout") {
			t.Errorf("★ %s 里没有 withPointsTimeout ——\n"+
				"共用一份预算 ⇒ 前面的账号把 20 秒吃光 ⇒ 后面全部报「查询失败」，\n"+
				"而**手动单查同一个账号却完全正常** —— 用户实测的正是这个形态。", fn)
		}
	}
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
