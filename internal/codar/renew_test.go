package codar

// renew_test.go —— T-382：token 续期的「**续期后回验**」护栏。
//
// # 为什么需要回验（新增的安全网）
//
// 上游只在 token 真临期时才发新令牌（实测 POST /v1/token/refresh → 400
// "Token is not close to expiration"），所以"提前轮换踢掉桌面端"这个风险在机制上不存在。
// 但**另一件事从没验证过**：续期返回的新令牌是否真的能用。
// 万一它形式合法而服务端不认，直接落盘就把用户从"快过期但能用"变成"完全不能用" ——
// 这是不可接受的方向（续期的目的是保可用，不是制造不可用）。
//
// ⇒ 落盘前用新令牌打一次最便宜的只读端点（/v1/promotion/info：只读、幂等、返回体小，
//    且 invite 面板本来就在用它），成功才替换。
//
// 为新建文件而不是塞进 contract_hermetic_test.go 的理由：那个文件是"跨上游契约"层，
// 这里测的是**本上游自己的续期流程**（含失败路径与不替换保证），属于 codar 包内单元测试。

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRenewOneKeepsOldTokenWhenVerifyFails 回验失败 ⇒ **绝不替换**旧令牌。
func TestRenewOneKeepsOldTokenWhenVerifyFails(t *testing.T) {
	old := makeJWT(time.Now().Add(-6*24*time.Hour).Unix(), time.Now().Add(2*time.Hour).Unix())
	fresh := makeJWT(time.Now().Unix(), time.Now().Add(30*24*time.Hour).Unix())
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		switch path {
		case "/v1/token/refresh":
			return 0, `{"code":200,"message":"ok","success":true,"data":{"token":"` + fresh + `"}}`
		case "/v1/promotion/info":
			// 新令牌**不被服务端认**（回验必须挡住）
			return http.StatusUnauthorized, `{"code":401,"message":"invalid token","success":false}`
		default:
			return 0, `{"code":200,"data":{}}`
		}
	})
	a := &Auth{Token: old, Username: "user_test_1", ExpiresAt: jwtExpiry(old)}

	err := p.renewOne(context.Background(), a)
	if err == nil {
		t.Fatal("回验失败时 renewOne 应当返回错误")
	}
	if !strings.Contains(err.Error(), "回验失败") {
		t.Errorf("错误信息应点出「回验失败」，得到 %q", err.Error())
	}
	if a.Token != old {
		t.Errorf("回验失败却替换了令牌 ✗（旧令牌必须原样保留）")
	}
	if a.ExpiresAt != jwtExpiry(old) {
		t.Errorf("ExpiresAt 也不该变：%d → %d", jwtExpiry(old), a.ExpiresAt)
	}
}

// TestRenewOneReplacesTokenAfterVerify 回验成功 ⇒ 替换，且 ExpiresAt 从**新**令牌解出。
func TestRenewOneReplacesTokenAfterVerify(t *testing.T) {
	old := makeJWT(time.Now().Add(-6*24*time.Hour).Unix(), time.Now().Add(2*time.Hour).Unix())
	freshExp := time.Now().Add(30 * 24 * time.Hour).Unix()
	fresh := makeJWT(time.Now().Unix(), freshExp)
	verified := false
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		switch path {
		case "/v1/token/refresh":
			return 0, `{"code":200,"message":"ok","success":true,"data":{"token":"` + fresh + `"}}`
		case "/v1/promotion/info":
			verified = true
			return 0, `{"code":200,"message":"ok","success":true,"data":{"invite_code":"X"}}`
		default:
			return 0, `{"code":200,"data":{}}`
		}
	})
	a := &Auth{Token: old, Username: "user_test_1", ExpiresAt: jwtExpiry(old)}

	if err := p.renewOne(context.Background(), a); err != nil {
		t.Fatalf("续期应当成功：%v", err)
	}
	if !verified {
		t.Error("落盘前必须回验一次（没看到回验请求）")
	}
	if a.Token != fresh {
		t.Error("回验成功后应当换成新令牌")
	}
	if a.ExpiresAt != freshExp {
		t.Errorf("ExpiresAt = %d，want %d（应从新令牌解出）", a.ExpiresAt, freshExp)
	}
}

// TestRenewOneKeepsOldTokenWhenVerifyErrors 回验**本身**出错（网络/超时）⇒ 同样不替换。
//
// 宁可这次不续、下次再来 —— 旧令牌还在有效期内，不替换永远是更安全的方向。
func TestRenewOneKeepsOldTokenWhenVerifyErrors(t *testing.T) {
	old := makeJWT(time.Now().Add(-6*24*time.Hour).Unix(), time.Now().Add(2*time.Hour).Unix())
	fresh := makeJWT(time.Now().Unix(), time.Now().Add(30*24*time.Hour).Unix())
	p := stubInviteUpstreamFn(t, func(path, body string, _ int) (int, string) {
		switch path {
		case "/v1/token/refresh":
			return 0, `{"code":200,"message":"ok","success":true,"data":{"token":"` + fresh + `"}}`
		default:
			return http.StatusInternalServerError, `boom`
		}
	})
	a := &Auth{Token: old, Username: "user_test_1", ExpiresAt: jwtExpiry(old)}

	if err := p.renewOne(context.Background(), a); err == nil {
		t.Fatal("回验出错时应当返回错误")
	}
	if a.Token != old {
		t.Error("回验出错却替换了令牌 ✗")
	}
}
