package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// egressFakeProvider 实现了 EgressExt + EgressCheckExt 的桩上游。
//
// ⚠ probes 写死、且**不发网络** —— 本测试要验的是「核心有没有真的遍历并如实汇总」，
// 不是「探测本身准不准」（那两条在 gateway / monkeycode 的单测里）。
type egressFakeProvider struct {
	id     string
	info   gateway.EgressInfo
	probes []gateway.EgressProbe
	calls  int
}

func (p *egressFakeProvider) ID() string               { return p.id }
func (p *egressFakeProvider) Caps() gateway.Capability { return gateway.CapChat }
func (p *egressFakeProvider) Chat(context.Context, gateway.Credential, []byte) (gateway.ChatStream, error) {
	return gateway.ChatStream{}, nil
}
func (p *egressFakeProvider) Models(context.Context, gateway.Credential) ([]gateway.ModelInfo, error) {
	return nil, nil
}
func (p *egressFakeProvider) Egress() gateway.EgressInfo { return p.info }
func (p *egressFakeProvider) EgressCheck(context.Context) []gateway.EgressProbe {
	p.calls++
	return p.probes
}

// TestEgressCheckRequiresImplementation 一个都没实现时**不假装成功**。
//
// 501 + 说清缺哪个扩展点：否则用户点了按钮看到「已启动」，而后台什么都没跑。
func TestEgressCheckRequiresImplementation(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(plainNoNameProvider{id: "plain"}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true, Registry: reg, TaskSlot: &fakeSlot{}})

	rec := httptest.NewRecorder()
	h.accountsEgressCheck(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/egress-check", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("期望 501，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "EgressCheckExt") {
		t.Errorf("回执应点名缺哪个扩展点，实际 %s", rec.Body.String())
	}
}

// TestEgressCheckRunsEveryProbeAndReportsFacts 每个实现的探测都被跑到，且失败带类别与下一步。
//
// 只断言「回了 202」不够：核心即便一条探测都没跑，只要任务槽接受就照样 202
// （与 checkin_all 的测试同一条理由）。所以这里**跑完后台任务**再逐条对。
func TestEgressCheckRunsEveryProbeAndReportsFacts(t *testing.T) {
	a := &egressFakeProvider{
		id:   "fake-a",
		info: gateway.EgressInfo{Direct: true, Proxies: []string{"http://127.0.0.1:10808"}, Note: "两站前提相反"},
		probes: []gateway.EgressProbe{
			{Name: "直连", Target: "https://a.example/x", Via: "直连", OK: true, MS: 12},
			{Name: "走代理", Target: "https://a.example/x", Via: "代理 http://127.0.0.1:10808",
				OK: false, Kind: gateway.EgressKindRefused, Hint: "对方端口没有服务在听：本地代理没起来？",
				Detail: "dial tcp 127.0.0.1:10808: connectex: actively refused", MS: 3},
		},
	}
	slot := &fakeSlot{}
	h := newCheckinAllHandler(t, slot, a)

	rec := httptest.NewRecorder()
	h.accountsEgressCheck(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/egress-check", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("期望 202，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Started   bool     `json:"started"`
		Providers []string `json:"providers"`
		Count     int      `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("回执不是 JSON: %v", err)
	}
	if !body.Started || body.Count != 1 || len(body.Providers) != 1 || body.Providers[0] != "fake-a" {
		t.Errorf("回执 = %+v，want started=true count=1 providers=[fake-a]", body)
	}

	rows := slot.run()
	if a.calls != 1 {
		t.Errorf("上游被调用 %d 次，want 1 —— 少调一次就是「按钮点了没测」", a.calls)
	}
	if len(rows) != 2 {
		t.Fatalf("结果行 = %d，want 2（一条探测一行；吞掉行会让用户看不到某条出口的状态）", len(rows))
	}
	// 每条都以「上游 · 探测名」开头：任务面板是按行渲染的，这一列是给人看的主键。
	first, _ := rows[0]["uid"].(string)
	if !strings.HasPrefix(first, "fake-a · ") {
		t.Errorf("结果行主键 = %q，want 前缀 \"fake-a · \"", first)
	}
	if got := rows[0]["status"]; got != "ok" {
		t.Errorf("通的那条 status = %v，want ok", got)
	}
	failRow := rows[1]
	if failRow["status"] != "fail" {
		t.Errorf("不通的那条 status = %v，want fail", failRow["status"])
	}
	detail, _ := failRow["detail"].(string)
	for _, want := range []string{"代理 http://127.0.0.1:10808", "代理没起来", "actively refused"} {
		if !strings.Contains(detail, want) {
			t.Errorf("失败行 detail 缺 %q（那是用户动手的依据）：%q", want, detail)
		}
	}
}

// TestEgressReachesProvidersEndpoint 出口要真的下发到 /admin/providers，且**没实现的不发键**。
//
// 前半：卡片头上的「出口 代理 http://…」靠这个字段，不下发就等于没做。
// 后半：omitempty 的落点 —— 既有部署（上游都没实现）升级后 manifest 必须与改造前一致。
func TestEgressReachesProvidersEndpoint(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&egressFakeProvider{id: "withegress", info: gateway.EgressInfo{Direct: true, Proxies: []string{"http://127.0.0.1:10808"}}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(plainNoNameProvider{id: "noegress"}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true, Registry: reg})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminReq(http.MethodGet, "/admin/providers"))
	var raw struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, p := range raw.Providers {
		byID[p["id"].(string)] = p
	}
	eg, ok := byID["withegress"]["egress"].(map[string]any)
	if !ok {
		t.Fatal("实现了 EgressExt 的上游，/admin/providers 里应带 egress 字段")
	}
	if direct, _ := eg["direct"].(bool); !direct {
		t.Errorf("egress.direct = %v，want true", eg["direct"])
	}
	proxies, _ := eg["proxies"].([]any)
	if len(proxies) != 1 || proxies[0] != "http://127.0.0.1:10808" {
		t.Errorf("egress.proxies = %v，want [http://127.0.0.1:10808]", eg["proxies"])
	}
	if _, has := byID["noegress"]["egress"]; has {
		t.Error("没实现 EgressExt 的上游不该带 egress 键（omitempty：界面必须与改造前逐字节相同）")
	}
}
