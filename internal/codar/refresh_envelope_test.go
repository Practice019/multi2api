// refresh_envelope_test.go codar 续期「上游错误信封」的判据。
//
// # 实测现场（2026-11-16，用户贴的状态页）
//
//	codar-token-refresh  出错  codar: 续期响应里找不到令牌字段
//	                            （code=401 message="Token has expired"；响应键：code,message）
//
// 上游实际返回的是 **HTTP 200 + {"code":401,"message":"Token has expired"}** ——
// 状态码正常，内容是一次**拒绝**。
//
// 改造前这段被一路走到"找不到令牌字段" ⇒ 把「上游明确说令牌过期了」
// 报成「我们的解析出错」，**指向完全错误的方向**：人会去查解析代码，
// 而真正要做的是去官方端重新登录。
//
// 三个必须钉住的行为：
//
//	① `{"code":401,...}` 必须被认成**错误信封**（不是"没有令牌字段"）
//	② 它必须映射到 `ErrRefreshTokenExpired`（终态哨兵，调用方可区分"要重登"与"可重试"）
//	③ 错误串里必须**带上上游原话**（"Token has expired"）—— 那是唯一能定性的证据
package codar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ★ ① 错误信封识别。
func TestCodarErrEnvelope(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
		wantCde int
	}{
		{"实测那条", `{"code":401,"message":"Token has expired"}`, true, 401},
		{"被停用", `{"code":100003,"message":"账号已被停用"}`, true, 100003},
		{"字符串 code", `{"code":"401","message":"Token has expired"}`, true, 401},
		{"嵌在 data 下", `{"data":{"code":403,"message":"forbidden"}}`, true, 403},
		{"成功响应（无 code）", `{"token":"eyJhbGciOi…"}`, false, 0},
		{"成功响应（code=0）", `{"code":0,"token":"x"}`, false, 0},
		// ⚠ 上游把 code 当 HTTP 状态用：code=200 是**成功**。
		//   我第一版按"有 code 就是错误信封"判，把这种成功响应判成拒绝，
		//   两条既有判据当场红了（续期永远失败）。
		{"成功信封（code=200）", `{"code":200,"message":"ok"}`, false, 200},
		{"成功信封（code=201）", `{"code":201,"data":{"token":"x"}}`, false, 201},
		{"非 JSON", `<html>502</html>`, false, 0},
	}
	for _, c := range cases {
		code, _, isErr := codarErrEnvelope([]byte(c.raw))
		if isErr != c.wantErr {
			t.Errorf("%s：isErr = %v，want %v（%s）", c.name, isErr, c.wantErr, c.raw)
		}
		if isErr && code != c.wantCde {
			t.Errorf("%s：code = %d，want %d", c.name, code, c.wantCde)
		}
	}
}

// ★ ② 哪些信封算「令牌已失效」（终态）。
func TestIsExpiredEnvelope(t *testing.T) {
	yes := []struct {
		code int
		msg  string
	}{
		{401, "Token has expired"},
		{403, "forbidden"},
		{500, "token expired"},
		{500, "登录已失效"},
		{500, "unauthorized"},
		{500, "请重新登录"},
	}
	for _, c := range yes {
		if !isExpiredEnvelope(c.code, c.msg) {
			t.Errorf("code=%d msg=%q 应判为令牌失效", c.code, c.msg)
		}
	}
	no := []struct {
		code int
		msg  string
	}{
		{500, "internal error"},
		{429, "too many requests"},
		{400, "bad request"},
	}
	for _, c := range no {
		if isExpiredEnvelope(c.code, c.msg) {
			t.Errorf("code=%d msg=%q 不该判为令牌失效（那不是要人重登的问题）", c.code, c.msg)
		}
	}
}

// ★ ③ 端到端：上游回 200 + code=401 ⇒ 必须是 ErrRefreshTokenExpired，
// **不是**「找不到令牌字段」，且错误串里要带上游原话。
func TestRenewOneMapsExpiredEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 关键：HTTP **200**，但内容是拒绝信封。这正是实测形态。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":401,"message":"Token has expired"}`))
	}))
	defer srv.Close()

	p := NewWithConfig(Config{
		PayBase: srv.URL,
		Client:  &Client{APIBase: srv.URL, PayBase: srv.URL},
	})
	a := &Auth{UID: "user_15083583411_7985", Username: "user_15083583411_7985", Token: "old-token"}

	err := p.renewOne(context.Background(), a)
	if err == nil {
		t.Fatal("上游回拒绝信封时必须返回错误")
	}
	if !errors.Is(err, ErrRefreshTokenExpired) {
		t.Fatalf("★ 应映射到 ErrRefreshTokenExpired（终态：要人重登），实际 %v", err)
	}
	if strings.Contains(err.Error(), "找不到令牌字段") {
		t.Fatalf("★ 仍报「找不到令牌字段」—— 那是把上游的拒绝说成我们的解析错误：%v", err)
	}
	if !strings.Contains(err.Error(), "Token has expired") {
		t.Fatalf("★ 错误串必须带上游原话（那是唯一能定性的证据）：%v", err)
	}
	if !strings.Contains(err.Error(), "重新登录") {
		t.Fatalf("★ 必须告诉人**该做什么**（重新登录），否则只会看到「失败」：%v", err)
	}
}

// ④ 非鉴权类的错误信封（如 429）不该被判成"令牌失效"。
func TestRenewOneMapsOtherEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":429,"message":"too many requests"}`))
	}))
	defer srv.Close()

	p := NewWithConfig(Config{PayBase: srv.URL, Client: &Client{APIBase: srv.URL, PayBase: srv.URL}})
	err := p.renewOne(context.Background(), &Auth{UID: "u", Token: "t"})
	if err == nil {
		t.Fatal("拒绝信封必须返回错误")
	}
	if errors.Is(err, ErrRefreshTokenExpired) {
		t.Fatalf("429 是可重试的，不该判成终态失效：%v", err)
	}
	if !strings.Contains(err.Error(), "被上游拒绝") {
		t.Fatalf("应说清是上游拒绝：%v", err)
	}
}

// ⑤ 「未临期」仍然是**静默跳过**（上游护栏，不是错误）。
//
// 这条防的是"修 ① 时把护栏弄坏"—— 那是每天一条假错误的回归。
func TestRenewOneNotCloseToExpirationStaysSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"message":"Token is not close to expiration"}`))
	}))
	defer srv.Close()

	p := NewWithConfig(Config{PayBase: srv.URL, Client: &Client{APIBase: srv.URL, PayBase: srv.URL}})
	if err := p.renewOne(context.Background(), &Auth{UID: "u", Token: "t"}); err != nil {
		t.Fatalf("「未临期」必须静默跳过（它是上游护栏，不是错误），实际 %v", err)
	}
}
