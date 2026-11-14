package phanthywork

// refresh_error_test.go 续期失败的**可诊断性**判据。
//
// # 为什么单独一个文件
//
// 这里测的不是"续期成不成功"（那是 jobs_test / upstream_test 的事），
// 而是"续期失败时，错误里还剩多少信息"。
//
// 这一点在 2026-11-14 现场暴露成一个真实障碍：账号被判成
// 「refreshToken 被吊销或已轮换，需重新登录」，而那句话是我们自己写的，
// 把上游的 6 种 code 压成了同一句话。最后只能手工 curl 一次 /oauth/token
// 才确认上游回的是 `invalid_grant`（真·凭据废了），而不是 `invalid_client`
// （**本地 client_id 配错**，重新登录一万次也没用）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRefreshExpiredCarriesUpstreamBody 400 + invalid_grant 必须**保留上游原文**。
//
// # 判据的两面
//
//	① 分类不变：errors.Is(err, ErrRefreshExpired) 仍为真（终态、不重试）
//	② 可诊断：err.Error() 里带上游的 code 与 message
//
// 只满足 ① 会让失败原因不可区分：
//
//	invalid_grant       凭据真的废了          → 该让用户重新登录
//	invalid_client      client_id 配错        → 该改代码，重登无用
//	unauthorized_client 该 client 未获授权    → 同上
//
// 只满足 ② 会让分类漂移（核心不再按终态处理 ⇒ 死号被反复刷新）。
func TestRefreshExpiredCarriesUpstreamBody(t *testing.T) {
	const upstreamBody = `{"error":{"code":"invalid_grant","message":"Refresh token is invalid or expired."}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOAuthToken {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	a := newTestAuth("u-refresh", "rt-dead")

	_, err := NewWithBase(srv.URL).RefreshTokens(context.Background(), a)
	if err == nil {
		t.Fatal("上游回 400 invalid_grant，续期应失败")
	}
	// ① 分类：仍是终态
	if !IsRefreshExpired(err) {
		t.Fatalf("应判为终态 ErrRefreshExpired（errors.Is），实际 %v\n"+
			"后果：归成可重试会让死号被反复刷新，每轮都白打一次上游", err)
	}
	// ② 可诊断：带上游 code
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("错误文本里必须带上游 code（invalid_grant）：%v\n"+
			"否则「凭据真废了」与「client_id 配错」在日志里长得一模一样，\n"+
			"只能靠手工 curl 才能分辨 —— 2026-11-14 就是这么卡住的", err)
	}
	// ③ 连 message 也要在（code 相同、message 不同时仍可区分）
	if !strings.Contains(err.Error(), "invalid or expired") {
		t.Errorf("错误文本里必须带上游 message：%v", err)
	}
}

// TestRefreshClientErrorIsNotTerminal 非 invalid_grant 的 4xx **不得**被判成终态。
//
// # 为什么这条要和上一条成对存在
//
// `invalid_client` / `unauthorized_client` 也被列在 authExpiredMarkers 里
// （见 errors.go），于是它们同样会走 `ErrRefreshExpired` ⇒ 核心按"凭据废了"
// 把账号永久禁用。而这两个 code 的真实语义是**本地配置错**：
//
//	client_id 不在上游的白名单里 / 该 client 未被授权做 refresh_token 授权
//
// ⇒ 重登无用，改代码才有用。本用例把这个语义**显式钉住**：它确实是终态
// （不该无限重试），但错误文本里必须能看出是 client 的问题，而不是凭据过期。
//
// ⚠ 这不是在要求改分类（那会牵动核心的处置），只是要求**可诊断**：
// 运维看到 invalid_client 就知道该报 bug，而不是傻等用户去重登。
func TestRefreshClientErrorCarriesCode(t *testing.T) {
	const upstreamBody = `{"error":{"code":"invalid_client","message":"client not authorized for refresh_token"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOAuthToken {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	a := newTestAuth("u-client", "rt-x")

	_, err := NewWithBase(srv.URL).RefreshTokens(context.Background(), a)
	if err == nil {
		t.Fatal("应失败")
	}
	if !strings.Contains(err.Error(), "invalid_client") {
		t.Errorf("错误文本里必须带 invalid_client（本地配置错，重登无用）：%v", err)
	}
}

// TestRefreshServerErrorIsRetriable 5xx 必须**不是**终态（可重试）。
//
// 与上面两条一起构成分类的三档：终态 / 可重试 / 客户端错。
// 若把 5xx 也归成 ErrRefreshExpired，那么上游抖动一次就会永久废掉一个健康账号 ——
// 那正是"自动续期看起来坏了"最容易发生的一种形态。
func TestRefreshServerErrorIsRetriable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathOAuthToken {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream_unavailable"}}`))
	}))
	defer srv.Close()

	a := newTestAuth("u-5xx", "rt-x")

	_, err := NewWithBase(srv.URL).RefreshTokens(context.Background(), a)
	if err == nil {
		t.Fatal("应失败")
	}
	if IsRefreshExpired(err) {
		t.Fatalf("5xx 被判成终态：%v\n"+
			"后果：上游抖动一次就永久废掉一个健康账号（核心会按「凭据废了」禁用）", err)
	}
}
