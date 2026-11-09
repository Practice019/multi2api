package codar

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/gateway"
)

// stubUpstream 起桩上游（T-348 的 seam 测试用）。
func stubUpstream(t *testing.T, routes map[string]string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := routes[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"stub: no such route"}`))
	}))
	t.Cleanup(srv.Close)
	p := NewWithConfig(Config{
		PayBase: srv.URL, APIBase: srv.URL,
		Client: &Client{APIBase: srv.URL, PayBase: srv.URL},
	})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{Provider: providerID, UID: uid, Secret: &Auth{Token: "t", Username: uid, UID: uid}}, true
	})
	return p
}

// TestQuotaTotalSumsThreeBuckets 三桶求和（T-348 定稿口径）。
//
// 期望值来自 T-347 消费实验的真源：老账号 agent=1706、gettoken={token:2500, agent_token:1706, rewrite:0}。
// 手算：1706+2500+0 = 4206。
// ★ RED 依据（真实历史）：T-346 修订版只认 agent_balance ⇒ 会得 1706 ≠ 4206（已部署实测过 1706）。
func TestQuotaTotalSumsThreeBuckets(t *testing.T) {
	p := stubUpstream(t, map[string]string{
		"/v1/agent/balance": `{"code":200,"data":{"agent_balance":1706}}`,
		"/v1/gettoken":      `{"code":200,"token":2500,"agent_token":1706,"rewrite_token":0}`,
	})
	q, ok := p.RefreshQuota("u1")
	if !ok {
		t.Fatalf("RefreshQuota 应当成功")
	}
	if q.Remaining != 4206 {
		t.Errorf("总额 = %v，want 4206（1706 agent + 2500 general）", q.Remaining)
	}
}

// TestQuotaNewAccountTotalIs500 新账号总额 = 500（用户问的"怎么可能没有额度"）。
//
// 真源：新账号 agent=0、gettoken={token:500}。手算 0+500=500。
// ★ RED 依据：T-346 修订版会得 0 ≠ 500。
func TestQuotaNewAccountTotalIs500(t *testing.T) {
	p := stubUpstream(t, map[string]string{
		"/v1/agent/balance": `{"code":200,"data":{"agent_balance":0}}`,
		"/v1/gettoken":      `{"code":200,"token":500,"agent_token":0,"rewrite_token":0}`,
	})
	q, ok := p.RefreshQuota("u2")
	if !ok || q.Remaining != 500 {
		t.Fatalf("新账号总额应当 = 500：ok=%v remaining=%v", ok, q.Remaining)
	}
}

// TestQuotaPartialWhenGetTokenFails gettoken 失败 ⇒ 只报 agent 桶 + partial=true + general 为 null。
//
// 手算：agent=1706 ⇒ total 1706（不能把缺的桶当 0 相加，那会显示成"余额变少了"）。
func TestQuotaPartialWhenGetTokenFails(t *testing.T) {
	p := stubUpstream(t, map[string]string{
		"/v1/agent/balance": `{"code":200,"data":{"agent_balance":1706}}`,
	})
	q, ok := p.RefreshQuota("u3")
	if !ok || q.Remaining != 1706 {
		t.Fatalf("gettoken 失败时应只报 agent 桶：ok=%v remaining=%v", ok, q.Remaining)
	}
	w := httptest.NewRecorder()
	p.handleQuota(w, httptest.NewRequest(http.MethodGet, "/admin/codar/quota?uid=u3", nil))
	body := w.Body.String()
	if !contains(body, `"partial":true`) {
		t.Errorf("部分数据必须标 partial=true：%s", body)
	}
	if !contains(body, `"general":{"balance":null}`) {
		t.Errorf("缺的桶必须是 null（不是 0）：%s", body)
	}
}

// TestQuotaUnknownWhenBalanceFails agent/balance 失败 ⇒ UnknownQuota（不拿 gettoken 或 0 顶替）。
func TestQuotaUnknownWhenBalanceFails(t *testing.T) {
	p := stubUpstream(t, map[string]string{
		"/v1/gettoken": `{"code":200,"token":2500,"agent_token":1706,"rewrite_token":0}`,
	})
	q, ok := p.RefreshQuota("u4")
	if ok || q.HasData {
		t.Fatalf("余额查询失败必须是 UnknownQuota：ok=%v q=%+v", ok, q)
	}
}

// TestQuotaDetailHasNoUsedOrLimit 防回归：used / limit 不许回来（被 T-347 实验否决）。
//
// 依据：`limit - balance` 对新账号会算出假的"已用 500"（它从未消费）。
func TestQuotaDetailHasNoUsedOrLimit(t *testing.T) {
	p := stubUpstream(t, map[string]string{
		"/v1/agent/balance": `{"code":200,"data":{"agent_balance":1706}}`,
		"/v1/gettoken":      `{"code":200,"token":2500,"agent_token":1706,"rewrite_token":0}`,
	})
	w := httptest.NewRecorder()
	p.handleQuota(w, httptest.NewRequest(http.MethodGet, "/admin/codar/quota?uid=u5", nil))
	body := w.Body.String()
	for _, bad := range []string{`"used"`, `"limit"`} {
		if contains(body, bad) {
			t.Errorf("响应里不该出现 %s（T-347 实验否决了上限/已用口径）：%s", bad, body)
		}
	}
	for _, want := range []string{`"total":4206`, `"agent":{"balance":1706}`, `"general":{"balance":2500}`, `"partial":false`} {
		if !contains(body, want) {
			t.Errorf("quota_detail 缺少 %s：%s", want, body)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
