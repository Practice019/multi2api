package loomy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件：`accountTasksView.FetchedAt` / `accountInviteView.FetchedAt` 的守门用例。
//
// # 为什么它值得单独一个文件
//
// `FetchedAt` 是**分层过期判定**（见 docs/cache-refresh-design.md）唯一的输入 ——
// 判定错一次，后果是两种都很糟的方向：
//   - 它被**漏盖**（零值）⇒ 那一行永远被当作"过期"⇒ 每次兜底扫描都优先刷它，
//     而不变的那些号（邀请码）被反复查。
//   - 它在**失败**时也被盖上 ⇒ "查失败"看起来像"刚查过"，于是它进不了重试队列。
//
// ⚠ 这两条都不会崩、不会报错、单测不打它就是绿的 —— 所以必须专门钉住。

// tasksStubFixture 造一个**两个端点都能回**的假上游：
// `/onboarding/tasks` 回任务表，其余路径回积分账本（`p.Balance` 走这里）。
//
// ⚠ 照 `points_budget_test.go` 里那个假上游的形状写（AGENTS 第 8 条：能抄则抄）——
// 那里已经踩过"tasks 必须是 **map**"这个坑：给数组会让 `Tasks` 报"回执里没有 tasks 字段"，
// 于是测试的语义会从"时间戳盖没盖"悄悄变成"回执形状错了"，判据就不再判别了。
func tasksStubFixture(t *testing.T, uid, session string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("token") != session {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/onboarding/tasks" || r.URL.Path == "/onboarding/tasks" {
			fmt.Fprint(w, `{"code":"000000","desc":"ok","data":{"tasks":{"configure_remote":true}}}`)
			return
		}
		fmt.Fprint(w, `{"code":"000000","desc":"ok","data":{"balance":100,"dailyBalance":50,"availableBalance":150}}`)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	writeFile(t, dir, "loomy-x.json", `{"session":"`+session+`","userid":"`+uid+`"}`)
	return NewWithConfig(Config{Client: NewWithBase(srv.URL), AuthDir: dir})
}

// TestTasksViewForStampsFetchedAtOnSuccess 成功时必须盖时间戳，**且缓存里那份也要带上**。
func TestTasksViewForStampsFetchedAtOnSuccess(t *testing.T) {
	const uid, sess = "260101010101010101", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := tasksStubFixture(t, uid, sess)

	v := p.tasksViewFor(accountRef{UID: uid, Session: sess})
	if !v.OK {
		t.Fatalf("假上游可用时应当查成功，实际 Error=%q", v.Error)
	}
	if v.FetchedAt.IsZero() {
		t.Error("★ 成功查询必须盖上 FetchedAt —— 没有它这一行永远被当成'过期'，" +
			"于是每一轮兜底扫描都优先刷它（而不变的那些号被反复查）")
	}

	// ⚠ 关键：**缓存里那份**也要有。只给返回值盖、忘了在 `set` 之前盖，
	//   是本改动第一版的真实错误（当时把赋值写在了读取路径上，改的是副本）。
	cached, ok := p.tasksPanel().get(uid)
	if !ok {
		t.Fatal("成功查询必须写进缓存（否则前端永远显示「正在读取…」）")
	}
	if cached.FetchedAt.IsZero() {
		t.Error("★ 缓存里的那份也必须带 FetchedAt —— " +
			"赋值写在了读取路径（改的是副本）时就是这个症状：返回值有、缓存没有")
	}
	if cached.FetchedAt.Unix() != v.FetchedAt.Unix() {
		t.Errorf("缓存里与返回值的时间戳应当是同一次（%v vs %v）", cached.FetchedAt, v.FetchedAt)
	}
}

// TestTasksViewForFailureKeepsFetchedAtZero 失败时：**不写缓存**，且时间戳保持零值。
func TestTasksViewForFailureKeepsFetchedAtZero(t *testing.T) {
	const uid, sess = "260101010101010101", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// ⚠ 用 freeQuotaFixture：它只认 `/points/records`，于是 `p.Tasks` 拿不到任务
	//   ⇒ 正好构造"查失败"。复用已有 fixture 而不是再造一个坏上游。
	p := freeQuotaFixture(t, uid, sess, `{"balance":8000}`)

	v := p.tasksViewFor(accountRef{UID: uid, Session: sess})
	if v.OK {
		t.Fatal("假上游不提供 tasks 端点时不该查成功")
	}
	if v.Error == "" {
		t.Error("失败必须留下 Error 文本（界面靠它显示「查询失败」而不是永远「正在读取…」）")
	}
	if !v.FetchedAt.IsZero() {
		t.Error("★ 失败时时间戳必须保持零值 —— 零值的语义是'从来没读到过'；" +
			"盖上就等于说'刚查过'，这一行反而进不了重试队列")
	}
	if _, ok := p.tasksPanel().get(uid); ok {
		t.Error("★ 失败的结果**不能**进缓存：那会让'上次抖了一下'在下次打开页面时" +
			"被当成已确认的事实显示出来（tasksViewFor 的文档注释就是这条契约）")
	}
}
