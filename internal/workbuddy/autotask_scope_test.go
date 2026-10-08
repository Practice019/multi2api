// autotask_scope_test.go 「一键完成」的作用域守卫。
//
// # 守的是用户的一个质疑
//
// 用户："workbuddy 一键完成任务 —— 我选择了一个账号，
//
//	他不能把所有的账号都跑一遍吧！？"
//
// 这个质疑必须能用**测试**回答，而不是靠"我读过代码，没问题"。
// 因为一旦真的跑成全部账号，后果是**不可撤销**的：那是对每个账号
// 各发一轮真实动作（含专家链的真实对话，实测单账号 ~180s），
// 上游侧已经发生的事没法回退。
//
// # 判据为什么落在 mode 字段上
//
// `AutoAll` / `SchoolRun` 都是"先定作用域、再交给后台任务槽"：
//
//	if body.UID != "" { ... Start(... 只跑这一个 uid ...)
//	                    响应 {"mode":"one","uid":<uid>} }
//	else              { ... Start(... 遍历 ownAccounts() ...)
//	                    响应 {"mode":"all","accounts":N} }
//
// `mode` 是**分支本身**的直接输出，而且它在 goroutine 启动后立刻写出 ——
// 所以断言它不需要等那 180s 的流水线跑完，也不会真的往上游发请求。
//
// ⚠ 这里断言的是"选了哪个分支"，不是"流水线跑得对不对"（那是别的测试的事）。
// 两者分开是刻意的：把作用域断言绑到流水线完成上，会让这条测试
// 慢到没人愿意跑，而它守的恰恰是最要紧的那件事。
package workbuddy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newScopeTestHandler 造一个挂 2 个账号的 Provider + 管理端点宿主。
//
// 客户端指向一个**一切返回 404** 的假上游：
//   - 保证不会有任何请求打到真腾讯（这条测试是 hermetic 的）
//   - 流水线步骤会快速失败，后台 goroutine 不会挂很久
func newScopeTestHandler(t *testing.T, uids ...string) (*AdminHandler, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented in scope test", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	prov := NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up})
	return NewAdminHandler(prov, AdminEnv{}), p
}

// postJSON 发一个 POST 并把响应解成 map。
func postJSON(t *testing.T, h func(http.ResponseWriter, *http.Request), body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// TestAutoAllSelectedUIDTakesSingleAccountBranch 用户质疑的那一条。
//
// 池里放 **2 个账号**，请求只带其中**一个** uid —— 响应必须是单账号模式。
//
// 为什么池里要有 2 个而不是 1 个：只有 1 个账号时，"单账号"与"全池"
// 跑出来是一样的，这条断言就失去意义（那正是最容易写出假绿的形态）。
func TestAutoAllSelectedUIDTakesSingleAccountBranch(t *testing.T) {
	h, _ := newScopeTestHandler(t, "acc-a", "acc-b")

	code, resp := postJSON(t, h.AutoAll, `{"uid":"acc-a"}`)
	if code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d：%v", code, resp)
	}
	if got := resp["mode"]; got != "one" {
		t.Errorf("选了 acc-a 却走了 %q 模式（期望 one）——\n"+
			"池里有 acc-a/acc-b 两个账号，这是**全池**模式的形态。\n"+
			"后果：用户只选了一个账号，网关却对每个账号各发一轮真实动作，"+
			"而上游侧已经发生的事无法回退。\n响应: %v", got, resp)
	}
	if got := resp["uid"]; got != "acc-a" {
		t.Errorf("响应里的 uid 应是选中的 acc-a，实际 %v", got)
	}
	// 全池模式才会带 accounts 字段 —— 它的出现本身就是走错分支的信号。
	if _, ok := resp["accounts"]; ok {
		t.Errorf("单账号模式不该带 accounts 字段，响应: %v", resp)
	}
}

// 空 uid 才是「全部账号」—— 这条与上一条成对。
//
// 没有这条的话，"把 mode 恒写成 one"也能让上一条通过，
// 而那会让「全部账号」这个功能彻底失效（点了只跑一个）。
func TestAutoAllEmptyUIDTakesPoolBranch(t *testing.T) {
	h, _ := newScopeTestHandler(t, "acc-a", "acc-b")

	code, resp := postJSON(t, h.AutoAll, `{"uid":""}`)
	if code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d：%v", code, resp)
	}
	if got := resp["mode"]; got != "all" {
		t.Errorf("空 uid 应走全池模式（mode=all），实际 %q —— "+
			"「全部账号」功能失效了。响应: %v", got, resp)
	}
	if got := resp["accounts"]; got != float64(2) {
		t.Errorf("全池模式的 accounts 应是池里的 2 个，实际 %v", got)
	}
}

// 请求体缺 uid 字段（老前端/curl 裸 POST）也走全池 —— 与空串同一条分支。
//
// 这条钉住"省略 = 全池"这个**既有契约**（注释里写明了）：
// 有人后来把 `if body.UID != ""` 改成"必须带 uid 否则 400"，
// 会让既有的 curl/脚本调用全部失效，而那是静默的破坏。
func TestAutoAllMissingUIDStaysPoolBranch(t *testing.T) {
	h, _ := newScopeTestHandler(t, "acc-a", "acc-b")

	code, resp := postJSON(t, h.AutoAll, `{}`)
	if code != http.StatusAccepted {
		t.Fatalf("期望 202（省略 uid = 全池），实际 %d：%v", code, resp)
	}
	if got := resp["mode"]; got != "all" {
		t.Errorf("省略 uid 应与空串同义（全池），实际 %q。响应: %v", got, resp)
	}
}

// school/run（一键完成的第二步）必须同样认 uid。
//
// # 为什么两步都要测
//
// 「一键完成」= auto-all → 等 → school/run。**两步各自独立解析 uid**，
// 所以只守住第一步是不够的：第二步漏了 uid 的话，用户选了一个账号，
// 成长待办只跑它、而开学季仍然跑全部 —— 那半程越界更难发现
// （用户在进度面板看到的是"开学季"在跑，很难意识到它跑了几个号）。
func TestSchoolRunSelectedUIDTakesSingleAccount(t *testing.T) {
	h, _ := newScopeTestHandler(t, "acc-a", "acc-b")

	code, resp := postJSON(t, h.SchoolRun, `{"uid":"acc-a"}`)
	if code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d：%v", code, resp)
	}
	if got := resp["accounts"]; got != float64(1) {
		t.Errorf("选了 acc-a 时开学季应只跑 1 个账号，实际 accounts=%v ——\n"+
			"后果同 AutoAll：半程越界。响应: %v", got, resp)
	}
}

// school/run 空 uid = 全池（与上一条成对）。
func TestSchoolRunEmptyUIDTakesWholePool(t *testing.T) {
	h, _ := newScopeTestHandler(t, "acc-a", "acc-b")

	code, resp := postJSON(t, h.SchoolRun, `{"uid":""}`)
	if code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d：%v", code, resp)
	}
	if got := resp["accounts"]; got != float64(2) {
		t.Errorf("空 uid 时开学季应跑全部 2 个账号，实际 accounts=%v", got)
	}
}

// 选中一个**不存在**的 uid 必须报 404，不能"找不到就退化成全池"。
//
// # 为什么这条很要紧
//
// "找不到就全池"是一种很自然的兜底写法，而它的后果正是用户担心的那件事：
// 用户选了一个刚被删掉的账号（界面上还留着旧选项）→ 网关把**全部账号**
// 跑了一遍。这条断言把这个兜底方向钉死。
func TestSchoolRunUnknownUIDDoesNotFallBackToPool(t *testing.T) {
	h, _ := newScopeTestHandler(t, "acc-a", "acc-b")

	code, resp := postJSON(t, h.SchoolRun, `{"uid":"already-deleted"}`)
	if code == http.StatusAccepted {
		t.Errorf("不存在的 uid 不该被接受（那意味着它退化成全池了），响应: %v", resp)
	}
	if code != http.StatusNotFound {
		t.Errorf("不存在的 uid 应回 404，实际 %d：%v", code, resp)
	}
}
