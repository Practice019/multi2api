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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestRefreshExpiredCarriesUpstreamBody 400 + invalid_grant 必须**保留上游原文**。
//
// # 判据的两面
//
//	① 分类不变：errors.Is(err, ErrRefreshExpired) 仍为真（终态、不重试）
//	② 可诊断：err.Error() 里带上游的 code 与 message
//
// ★★ 2026-11-14 语义变更（依据官方客户端源码）：`invalid_grant` **不是终态**。
//
// 旧口径把它判成 `ErrRefreshExpired`（终态）⇒ 核心计一次失败 ⇒ 连续 3 次永久禁用。
// 官方 sidecar 的 `Pd3`（@99945774）对同一条响应是
// `if (status===400 && code==="invalid_grant") return null;` —— **返回 null 不抛错**，
// 然后 `tH`（@99948835）拿 null 去**重读共享凭证库**（看是不是被别的端轮换掉了），
// 真拿不到才回可重试的 503 `OAUTH_REFRESH_UNAVAILABLE`。
//
// 所以本判据现在有三条**互相制约**的断言，缺一条都会放过一类回归：
//
//	① 必须判成"被拒"（可重试）        ← 否则号被无谓禁用
//	② **不得**判成"凭据已废"（终态）    ← 否则退回旧 bug（这条是核心）
//	③ 出口层认得出这个标记              ← 否则 server 层照样计数
//
// 只满足 ③ 不满足 ②，就是"计数字段换了名字但行为照旧"，本判据专门拦它。
func TestRefreshRejectedCarriesUpstreamBody(t *testing.T) {
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
	// ① 判成"被拒"（可重试）
	if !IsRefreshRejected(err) {
		t.Fatalf("invalid_grant 应判为 ErrRefreshRejected（可重试），实际 %v\n"+
			"后果：按终态处理会让一次轮换竞态把健康号打成死号", err)
	}
	// ② ★ 不得判成终态 —— 这条是防"退回旧 bug"的主判据
	if IsRefreshExpired(err) {
		t.Fatalf("invalid_grant **不得**判成 ErrRefreshExpired（终态）：%v\n"+
			"后果：核心按「账号层失效」计一次失败，连续 3 次永久禁用；\n"+
			"而 invalid_grant 最常见的成因只是 refresh_token 被另一个端轮换走了"+
			"（下一秒就可能重新可用）。2026-11-14 现场两个号就是这么死的。", err)
	}
	// ③ 出口层能识别（server 层靠它决定不计 refreshFails）
	if !errors.Is(err, gateway.ErrCredentialRefreshRejected) {
		t.Errorf("必须包装 gateway.ErrCredentialRefreshRejected，否则 server 层照样计数：%v", err)
	}
	// ④ 可诊断：带上游 code 与 message
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("错误文本里必须带上游 code（invalid_grant）：%v\n"+
			"否则「被轮换」与「client_id 配错」在日志里长得一模一样，\n"+
			"只能靠手工 curl 才能分辨 —— 2026-11-14 就是这么卡住的", err)
	}
	if !strings.Contains(err.Error(), "invalid or expired") {
		t.Errorf("错误文本里必须带上游 message：%v", err)
	}
}

// TestMissingRefreshTokenIsTheOnlyTerminal 真终态只剩一种：这份凭证连 refresh_token 都没有。
//
// 这是 ErrRefreshExpired 收窄后的**边界**：它不能再被当成"续期失败的统称"，
// 否则 invalid_grant 会顺着它重新流回"永久禁用"那条路。
func TestMissingRefreshTokenIsTheOnlyTerminal(t *testing.T) {
	a := newTestAuth("u-nort", "")
	_, err := NewWithBase("http://127.0.0.1:1").RefreshTokens(context.Background(), a)
	if !IsRefreshExpired(err) {
		t.Fatalf("没有 refresh_token 才是真终态，应判 ErrRefreshExpired：%v", err)
	}
	if IsRefreshRejected(err) {
		t.Errorf("「没有 refresh_token」不该同时被判成「被拒」——两者动作不同。"+
			"被拒要等下一轮，没 token 只能重登：%v", err)
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
