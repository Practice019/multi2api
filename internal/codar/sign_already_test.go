package codar

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/gateway"
)

// stubSignUpstream 桩上游：/v1/daily-sign 返回指定的 HTTP 码与业务 code/message。
func stubSignUpstream(t *testing.T, httpCode int, body string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(httpCode)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	p := NewWithConfig(Config{PayBase: srv.URL, APIBase: srv.URL, Client: &Client{APIBase: srv.URL, PayBase: srv.URL}})
	p.SetCredentialSource(func(uid string) (gateway.Credential, bool) {
		return gateway.Credential{Provider: providerID, UID: uid, Secret: &Auth{Token: "t", Username: uid, UID: uid}}, true
	})
	return p
}

// TestSignAlreadyClaimedIsAlreadyNotFail ★ T-351：上游的"已签"是**中文措辞 + code!=200**，
// 必须判成 already（此前被判 fail —— 用户截图里老账号的"签到失败"就是这个）。
//
// 真源（实测）：HTTP 200 + {"code":<非200>,"message":"24小时内仅可领取一次"}
func TestSignAlreadyClaimedIsAlreadyNotFail(t *testing.T) {
	p := stubSignUpstream(t, http.StatusOK, `{"code":400,"message":"24小时内仅可领取一次"}`)
	a, _ := authOf(gateway.Credential{Secret: &Auth{Token: "t", UID: "u1"}})
	res := p.doSign(nil, a, "manual")
	if res.Status != "already" {
		t.Errorf("状态 = %q，want already（中文措辞「24小时内仅可领取一次」= 已签，不是失败）· detail=%q", res.Status, res.Detail)
	}
}

// TestSignEnglishAlreadyStillWorks 英文措辞不能被回归（T-317 探到过 "Already signed"）。
func TestSignEnglishAlreadyStillWorks(t *testing.T) {
	p := stubSignUpstream(t, http.StatusOK, `{"code":400,"message":"Already signed"}`)
	a, _ := authOf(gateway.Credential{Secret: &Auth{Token: "t", UID: "u2"}})
	if res := p.doSign(nil, a, "manual"); res.Status != "already" {
		t.Errorf("英文已签措辞判成了 %q", res.Status)
	}
}

// TestSignRealFailureStaysFail 真失败（网络/上游 5xx）仍必须是 fail —— 不能被"已签"判定吃掉。
func TestSignRealFailureStaysFail(t *testing.T) {
	p := stubSignUpstream(t, http.StatusInternalServerError, `{"code":500,"message":"服务器开小差了"}`)
	a, _ := authOf(gateway.Credential{Secret: &Auth{Token: "t", UID: "u3"}})
	if res := p.doSign(nil, a, "manual"); res.Status != "fail" {
		t.Errorf("上游 5xx 必须判 fail，实际 %q", res.Status)
	}
}

// TestSignSuccessIsOk 正常签到成功仍是 ok。
func TestSignSuccessIsOk(t *testing.T) {
	p := stubSignUpstream(t, http.StatusOK, `{"code":200,"message":"签到成功"}`)
	a, _ := authOf(gateway.Credential{Secret: &Auth{Token: "t", UID: "u4"}})
	if res := p.doSign(nil, a, "manual"); res.Status != "ok" {
		t.Errorf("签到成功应判 ok，实际 %q", res.Status)
	}
}
