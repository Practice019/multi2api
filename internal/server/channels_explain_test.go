// channels_explain_test.go 渠道选路诊断的接线判据（2026-11-13 T-909 收尾）。
//
// # 判什么
//
//	① 计数接线：跑 N 次**真实**选路 ⇒ 各渠道被选中次数被记下来，且**均匀覆盖**（6 条全中）；
//	② 只读接口 `GET /ui/channels/explain`：身份判据与 `/ui/channels` **同一套**（未认证 ⇒ 401）、
//	   **不触发任何上游请求**（假上游命中计数不变）、返回页面要的字段。
//
// ⚠ 只读性由「假上游命中计数不变」来判 —— 比「看代码没发请求」硬。
package server

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/openaicompat"
)

// explainResp 诊断接口的响应形状（页面按它渲染）。
type explainResp struct {
	Ready bool   `json:"ready"`
	Model string `json:"model"`
	Error string `json:"error"`
	Text  string `json:"text"`
	View  struct {
		Total      int      `json:"total"`
		Conclusion string   `json:"conclusion"`
		Fairness   string   `json:"fairness"`
		Notes      []string `json:"notes"`
		Candidates []struct {
			ChannelID string `json:"channel_id"`
			Priority  int    `json:"priority"`
			Weight    int    `json:"weight"`
			Regular   bool   `json:"regular"`
			Picked    int    `json:"picked"`
		} `json:"candidates"`
	} `json:"view"`
}

// explainHandler 与 retryHandler 同款，但**带上 admin token** ——
// ⚠ 不带的话 `?token=` 必然 401，那条"身份判据一致"的断言就测不到真东西（只是测了 401）。
func explainHandler(s *channelstore.Store) *Handler {
	return NewHandler(Config{
		Channels:   s,
		AdminToken: testAdminToken,
		ChannelProvider: func(id, baseURL string) gateway.Provider {
			return openaicompat.NewOpenAICompat(id, baseURL)
		},
	})
}

// explainReq 发一个诊断请求。
//
// ⚠ 身份用**本仓既有的**那条：`?token=<admin token>`（见 webui_login_test.go:131 的表驱动用例），
//
//	不新造一套测试鉴权 —— 否则「身份判据与 /ui/channels 一致」这条断言就失去意义。
func explainReq(t *testing.T, h *Handler, model string, authed bool) (int, string) {
	t.Helper()
	path := "/ui/channels/explain?model=" + url.QueryEscape(model)
	if authed {
		path += "&token=" + url.QueryEscape(testAdminToken)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// ② 未认证必须拒绝（与 /ui/channels 同一套判据）。
func TestChannelsExplainRequiresAuth(t *testing.T) {
	s := retryStore(t, []spec{{id: "ch-a", prio: 10, weight: 1}})
	h := explainHandler(s)
	code, body := explainReq(t, h, "m", false)
	if code != http.StatusUnauthorized {
		t.Fatalf("未认证应 401（与 /ui/channels 同一套判据），实际 %d: %s", code, body)
	}
}

// ② 诊断**不发任何上游请求**：假上游命中计数必须一次都不变。
func TestChannelsExplainIsReadOnlyNoUpstream(t *testing.T) {
	s := retryStore(t, []spec{{id: "ch-a", prio: 10, weight: 1}})
	h := explainHandler(s)
	up := newRetryUpstream(t)
	up.add(t, s, "ch-a", false, 0)
	before := up.hitCount("ch-a")

	code, body := explainReq(t, h, "m", true)
	if code != http.StatusOK {
		t.Fatalf("诊断应 200，实际 %d: %s", code, body)
	}
	if after := up.hitCount("ch-a"); after != before {
		t.Fatalf("★ 诊断打了上游（命中 %d → %d）—— 它必须是纯只读的", before, after)
	}
	var out explainResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("诊断响应不是预期 JSON: %v\n%s", err, body)
	}
	if !out.Ready || out.View.Total != 1 || len(out.View.Candidates) != 1 {
		t.Errorf("诊断应报出 1 条候选，实际 ready=%v total=%d\n%s", out.Ready, out.View.Total, body)
	}
	if out.View.Conclusion == "" || !strings.Contains(out.Text, "候选") {
		t.Errorf("诊断必须带结论与可读文本：%s", body)
	}
}

// ① 计数接线 + 均匀覆盖：跑真实选路 N 次 ⇒ 6 条都该被记到。
//
// ⚠ priority 故意各不相同（60/50/…/10）：它已不参与选路，所以不该影响覆盖。
func TestChannelsExplainCountsCoverAllCandidates(t *testing.T) {
	specs := []spec{
		{id: "c1", prio: 60, weight: 1}, {id: "c2", prio: 50, weight: 1},
		{id: "c3", prio: 40, weight: 1}, {id: "c4", prio: 30, weight: 1},
		{id: "c5", prio: 20, weight: 1}, {id: "c6", prio: 10, weight: 1},
	}
	s := retryStore(t, specs)
	h := explainHandler(s)
	up := newRetryUpstream(t)
	for _, sp := range specs {
		up.add(t, s, sp.id, false, 0)
	}
	// ⚠ 固定随机源（T-909 补）：否则"均匀性"断言会 flaky —— 同样的种子必然同样的分布。
	h.pickRand = rand.New(rand.NewSource(20261113))
	// 采样次数要**超过判据下限**（6 条 × 10 = 60），留点余量取 90。
	for i := 0; i < 90; i++ {
		if _, err := h.forwardViaChannel(context.Background(), "m", []byte(`{"model":"m"}`)); err != nil {
			t.Fatalf("第 %d 次转发失败: %v", i, err)
		}
	}
	_, body := explainReq(t, h, "m", true)
	var out explainResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析诊断失败: %v\n%s", err, body)
	}
	if len(out.View.Candidates) != 6 {
		t.Fatalf("应当 6 条候选，实际 %d\n%s", len(out.View.Candidates), body)
	}
	uncovered := 0
	for _, c := range out.View.Candidates {
		if c.Picked == 0 {
			uncovered++
		}
	}
	if uncovered > 0 {
		t.Errorf("★ 有 %d/6 条候选一次都没被选中（60 次转发）—— 计数没接上或选路仍不均：%s",
			uncovered, body)
	}
	if !strings.Contains(out.View.Fairness, "均匀") {
		t.Errorf("均匀性结论应当说「均匀」，实际 %q", out.View.Fairness)
	}
	t.Logf("诊断文本:\n%s", out.Text)
}
