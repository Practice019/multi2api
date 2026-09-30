// checkin_all_test.go 核心跨上游签到端点的行为测试。
//
// # 为什么它必须存在
//
// 顶部那个「全部签到」的**全部**语义是"触发所有上游"。它有三种
// "看起来成功其实没做"的失效形态，三种都不报错：
//
//	① 某个上游没实现扩展点 → 它被跳过（不在 providers 里）
//	② 任务槽没注入         → 整条端点 501（前端只说"未能启动"）
//	③ 上游整趟失败         → 三个计数里看不出是哪个上游没动
//
// 本文件用**假上游**把它们逐个钉住：假上游是可控的，能断言
// "它到底被问了没有" —— 而那是真上游测不出来的（真上游的签到结果
// 取决于网络与凭证）。
package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/gateway"
)

// ⚠ 假上游必须实现**完整的** gateway.Provider（ID/Caps/Chat/Models），
// 否则注册不进 Registry。但下面那三个方法都是**空壳** —— 本文件不测它们。
//
// 空壳不是偷懒：核心的 checkinProviders() 只问 ID 与"有没有实现
// DailyCheckinExt"，给它一个能注册的载体就够了。把 Chat 写成真的
// 只会让"这个测试在测什么"变模糊。
func (f *checkinFakeProvider) Caps() gateway.Capability { return gateway.CapChat }
func (f *checkinFakeProvider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{Status: http.StatusNotImplemented, Body: io.NopCloser(strings.NewReader(""))}, nil
}
func (f *checkinFakeProvider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

// plainProvider 一个不实现任何扩展点的上游。
type plainProvider struct{ id string }

func (p *plainProvider) ID() string               { return p.id }
func (p *plainProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *plainProvider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{Status: http.StatusNotImplemented, Body: io.NopCloser(strings.NewReader(""))}, nil
}
func (p *plainProvider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}

// checkinFakeProvider 一个只实现 DailyCheckinExt 的假上游。
//
// # 为什么它只实现 DailyCheckinExt 而不实现 Provider 全部接口
//
// 本文件要测的是"核心怎么遍历与汇总"，不是"上游怎么签到"。
// 假上游越小，测试里"没被断言到的行为"就越少 —— 这是本项目
// 一直避免"测试装置抄被测对象"的同一条原则。
type checkinFakeProvider struct {
	id    string
	calls int
	mu    sync.Mutex
	// results 返回给核心的结果（模拟"签了几个"）。
	results []gateway.DailyCheckinResult
	// err 非空表示整趟失败。
	err string
	// actions 它自报的每日动作（决定核心要不要列它）。
	//
	// ⚠ 必须能表达"实现了 DailyCheckinExt 但**没有**签到动作"这个组合 ——
	// workbuddy-intl 就是它（同一 Provider 类型的第二个实例，玩法被裁剪）。
	actions []gateway.DailyAction
}

func (f *checkinFakeProvider) ID() string { return f.id }

// DailyActions 让假上游同时满足 DailyActionExt（判据需要它）。
func (f *checkinFakeProvider) DailyActions() []gateway.DailyAction { return f.actions }

func (f *checkinFakeProvider) CheckinAll(ctx context.Context) gateway.DailyCheckinReport {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != "" {
		return gateway.DailyCheckinReport{Provider: f.id, Error: f.err}
	}
	return gateway.DailyCheckinReport{Provider: f.id, Results: f.results}
}

// checkinAction 一个"有全量端点的签到动作"（大多数假上游都要带它）。
//
// # 为什么假上游必须显式带上它
//
// 核心的判据是"自报了 batch 动作才列进 providers"（见 checkinProviders）。
// 假上游不带它就会被跳过 —— 那样上面几条断言测的就变成"空清单"，
// 而不是它们声称在测的东西。
func checkinAction(provider string) []gateway.DailyAction {
	return []gateway.DailyAction{{
		ID: "checkin", Label: "签到",
		OneURL: "/admin/" + provider + "/checkin",
		AllURL: "/admin/" + provider + "/checkin/all",
		Batch:  true,
	}}
}

// fakeSlot 记录 Start 是否被调用（并立即同步执行，便于断言结果）。
type fakeSlot struct {
	mu      sync.Mutex
	started []string
	fn      func() []map[string]any
}

func (s *fakeSlot) Snapshot() map[string]any { return map[string]any{"running": false} }

func (s *fakeSlot) Start(kind string, fn func() []map[string]any) bool {
	s.mu.Lock()
	s.started = append(s.started, kind)
	s.fn = fn
	s.mu.Unlock()
	return true
}

func (s *fakeSlot) kinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.started...)
}

func (s *fakeSlot) run() []map[string]any {
	s.mu.Lock()
	fn := s.fn
	s.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// newCheckinAllHandler 建一个带两个假上游的 handler。
func newCheckinAllHandler(t *testing.T, slot adminSlotForCheckin, provs ...gateway.Provider) *Handler {
	t.Helper()
	reg := gateway.NewRegistry()
	for _, p := range provs {
		reg.Register(p)
	}
	h := New(Config{Registry: reg, TaskSlot: slot})
	return h
}

// adminSlotForCheckin 本测试对任务槽的要求（只读那部分 + Start）。
type adminSlotForCheckin = TaskSlot

// TestCheckinAllStartsTaskForEveryProvider 每个实现了扩展点的上游都被跑到。
//
// # 这是本端点最核心的断言（挡失效形态 ①）
//
// 只断言"回了 202"是不够的 —— 核心即便一个上游都没遍历，
// 只要任务槽接受就照样回 202。所以这里**跑完后台任务**，
// 再逐个假上游问"你被调了几次"。
func TestCheckinAllStartsTaskForEveryProvider(t *testing.T) {
	a := &checkinFakeProvider{id: "fake-a", actions: checkinAction("fake-a"), results: []gateway.DailyCheckinResult{
		{UID: "u1", Status: "ok"}, {UID: "u2", Status: "already"},
	}}
	b := &checkinFakeProvider{id: "fake-b", actions: checkinAction("fake-b"), results: []gateway.DailyCheckinResult{
		{UID: "u3", Status: "fail", Detail: "上游没开"},
	}}
	slot := &fakeSlot{}
	h := newCheckinAllHandler(t, slot, a, b)

	rec := httptest.NewRecorder()
	h.accountsCheckinAll(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/checkin/all", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}

	var body struct {
		Started   bool     `json:"started"`
		Providers []string `json:"providers"`
		Count     int      `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("回执不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if !body.Started {
		t.Error("started=false —— 前端会走『未能启动』分支")
	}
	if !sameSet(body.Providers, []string{"fake-a", "fake-b"}) {
		t.Errorf("providers=%v，want [fake-a fake-b] —— "+
			"这份清单是『所有上游』里『所有』的答案，漏一个就是漏跑一个上游", body.Providers)
	}
	if body.Count != 2 {
		t.Errorf("count=%d，want 2", body.Count)
	}

	// 真正跑一遍后台任务：核心的汇总逻辑在这里。
	rows := slot.run()
	if len(rows) != 3 {
		t.Fatalf("汇总结果 %d 条，want 3（a 的 2 条 + b 的 1 条）: %+v", len(rows), rows)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("两个上游的调用次数 = %d / %d，want 1 / 1 —— "+
			"少调用一个就是『某上游整片没被签』（本端点最危险的失效形态）", a.calls, b.calls)
	}
	// 失败的那条必须带着原因进结果（否则用户只看到"失败 1"不知所以）。
	found := false
	for _, r := range rows {
		if r["uid"] == "u3" && r["status"] == "fail" && strings.Contains(r["detail"].(string), "上游没开") {
			found = true
		}
	}
	if !found {
		t.Errorf("失败账号 u3 的原因没有进汇总结果: %+v", rows)
	}
}

// TestCheckinAllReportsWholeUpstreamFailure 整趟失败也要留下一条可见结果。
//
// # 挡失效形态 ③
//
// 上游列账号失败时它的 Results 是空的。若核心直接跳过，
// 汇总里就完全没有这个上游的痕迹 —— 用户看到"成功 N"，
// 而这一整个上游一个号都没动。所以核心要把它转成一条 status=fail。
func TestCheckinAllReportsWholeUpstreamFailure(t *testing.T) {
	a := &checkinFakeProvider{id: "fake-a", actions: checkinAction("fake-a"), err: "列账号失败: 目录不存在"}
	slot := &fakeSlot{}
	h := newCheckinAllHandler(t, slot, a)

	rec := httptest.NewRecorder()
	h.accountsCheckinAll(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/checkin/all", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d", rec.Code)
	}
	rows := slot.run()
	if len(rows) != 1 {
		t.Fatalf("整趟失败时应留下 1 条可见结果，实际 %d: %+v", len(rows), rows)
	}
	if rows[0]["status"] != "fail" {
		t.Errorf("整趟失败的结果 status=%v，want fail", rows[0]["status"])
	}
	if !strings.Contains(rows[0]["uid"].(string), "fake-a") {
		t.Errorf("整趟失败的结果看不出是哪个上游: uid=%v", rows[0]["uid"])
	}
}

// TestCheckinAllNoProviderIsNotImplemented 一个都没有时**不假装成功**。
func TestCheckinAllNoProviderIsNotImplemented(t *testing.T) {
	slot := &fakeSlot{}
	h := newCheckinAllHandler(t, slot) // 没有注册任何上游

	rec := httptest.NewRecorder()
	h.accountsCheckinAll(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/checkin/all", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("没有上游时期望 501，实际 %d —— "+
			"回 202 会让用户以为『已在签到』，实际什么都没跑", rec.Code)
	}
}

// TestCheckinAllNeedsWritableTaskSlot 任务槽不带 Start 时报 501。
//
// # 挡失效形态 ②（且必须**不同步执行**）
//
// 若这里改成"没有槽就同步跑一遍"，界面上按钮是能用的，
// 但浏览器会挂住几十秒、且 /admin/task 永远看不到进度 ——
// 与"顶部按钮就该走后台任务槽"这条约定相悖。宁可刺眼地 501。
func TestCheckinAllNeedsWritableTaskSlot(t *testing.T) {
	a := &checkinFakeProvider{id: "fake-a", actions: checkinAction("fake-a")}
	reg := gateway.NewRegistry()
	reg.Register(a)
	h := New(Config{Registry: reg, TaskSlot: readOnlySlot{}})

	rec := httptest.NewRecorder()
	h.accountsCheckinAll(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/checkin/all", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("只读任务槽时期望 501，实际 %d", rec.Code)
	}
	if a.calls != 0 {
		t.Errorf("没有可写任务槽时**不该**同步执行签到（实际调了 %d 次）—— "+
			"那会让浏览器挂住且进度条永远不动", a.calls)
	}
}

// readOnlySlot 只实现 Snapshot 的任务槽（装配层漏接线的形态）。
type readOnlySlot struct{}

func (readOnlySlot) Snapshot() map[string]any { return map[string]any{"running": false} }

// TestCheckinAllSkipsProvidersWithoutExt 没实现扩展点的上游**不**被列出。
//
// # 为什么它不算错误
//
// 一个纯 API Key 上游没有账号生命周期，本来就没有签到 ——
// 报成"跳过"会让用户以为漏了什么。所以核心干脆不列它。
func TestCheckinAllSkipsProvidersWithoutExt(t *testing.T) {
	a := &checkinFakeProvider{id: "fake-a", actions: checkinAction("fake-a")}
	slot := &fakeSlot{}
	h := newCheckinAllHandler(t, slot, a, &plainProvider{id: "no-checkin"})

	rec := httptest.NewRecorder()
	h.accountsCheckinAll(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/checkin/all", nil))
	var body struct {
		Providers []string `json:"providers"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !sameSet(body.Providers, []string{"fake-a"}) {
		t.Errorf("providers=%v，want 只含 fake-a —— "+
			"没实现签到的上游不该出现在清单里（它不是『漏了』）", body.Providers)
	}
}

// TestCheckinAllSkipsInstanceWithoutCheckinAction 实现了扩展点但**没有签到动作**的实例要被跳过。
//
// # 这条是隔离实例实测抓出来的谎报（不是假想）
//
// 实测（7871）：`workbuddy` 与 `workbuddy-intl` 是**同一个 Provider 类型**
// 的两个实例，两者**都**实现了 DailyCheckinExt。但海外版
// （DisableGrowthTravel）没有签到玩法 —— 它的路由里没有 /admin/checkin，
// CheckinAll 如实回空。
//
// 只看"实现了扩展点"会把它列进 providers，于是用户点完「全部签到」看到
// `providers: [workbuddy, workbuddy-intl]`，以为两个上游都签了 ——
// 而其中一个**一个号都没动**。回执说做了、实际没做，是本按钮最危险的形态。
//
// 判据：providers 清单 == "自报了 batch 动作"的那些上游
// （与前端渲染卡片头「全部签到」按钮的判据同源）。
func TestCheckinAllSkipsInstanceWithoutCheckinAction(t *testing.T) {
	withCheckin := &checkinFakeProvider{
		id: "with-checkin", actions: checkinAction("with-checkin"),
		results: []gateway.DailyCheckinResult{{UID: "u1", Status: "ok"}},
	}
	// 实现了扩展点、但**没有** batch 动作的实例（workbuddy-intl 的形态）。
	noCheckin := &checkinFakeProvider{
		id: "no-checkin",
		actions: []gateway.DailyAction{
			// 有端点但 Batch=false —— 产品决定不给全量入口（workbuddy 的保活同形）
			{ID: "keepalive", Label: "保活", OneURL: "/admin/k", AllURL: "/admin/k", Batch: false},
		},
	}

	slot := &fakeSlot{}
	h := newCheckinAllHandler(t, slot, withCheckin, noCheckin)

	rec := httptest.NewRecorder()
	h.accountsCheckinAll(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/checkin/all", nil))
	var body struct {
		Providers []string `json:"providers"`
		Count     int      `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	if !sameSet(body.Providers, []string{"with-checkin"}) {
		t.Errorf("providers=%v，want 只含 with-checkin —— "+
			"没有签到动作的实例被列进去就是**谎报**：用户以为它签了，实际一个号都没动",
			body.Providers)
	}
	if body.Count != 1 {
		t.Errorf("count=%d，want 1", body.Count)
	}

	// 真正跑一遍：那个没有签到的实例**不该被调用**。
	rows := slot.run()
	if noCheckin.calls != 0 {
		t.Errorf("没有签到动作的实例被调了 %d 次 —— "+
			"它的 CheckinAll 只会回空，调用它是纯粹的浪费与谎报", noCheckin.calls)
	}
	if len(rows) != 1 {
		t.Errorf("汇总结果 %d 条，want 1（只有有签到的那个实例产出）: %+v", len(rows), rows)
	}
}

// TestCheckinAllEndpointIsRegistered 路由真的挂着。
//
// 与 TestCheckinAllStartsTaskForEveryProvider 互补：那边测函数，
// 这边测"前端点的那个 URL 真的有东西接"。漏注册的形态是
// 前端 404，而按钮看起来一切正常。
func TestCheckinAllEndpointIsRegistered(t *testing.T) {
	h := New(Config{})
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/checkin/all", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatal("POST /admin/accounts/checkin/all 没有注册 —— 顶部按钮会 404")
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}
