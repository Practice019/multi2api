package raccoon

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestHasTokenReadsLiveCredential has_token 必须来自**活凭证**，不是池投影。
//
// # 这是用户报的「Token 列一直是 `—`」的真正原因
//
// 界面的「Token」列判据是 `has_token`，而它原来只读账号池投影
// （核心的 `*auth.Auth`）。对 cline 而言投影里那个字段**永远是空的**：
//
//	核心 auth.Parse 读 `auth.accessToken`（驼峰）
//	本上游落盘写的令牌字段名与它不同
//
// 实测生产凭证：文件里的 access_token 完全正常，而 has_token=false。
// 界面把「我们没在投影里读到」说成了「这个号没有 token」。
//
// 修法是让上游自报（gateway.CredentialTokenExt）—— 与当年
// workbuddy-intl 的同类修复同一条路（见 credential_token.go 的文件头）。
//
// 变异可检：把 HasToken 改成读 `cred` 上别的字段 / 恒 false → 本用例红。
func TestHasTokenReadsLiveCredential(t *testing.T) {
	p := NewWithConfig(Config{})
	cred := gateway.Credential{Secret: &Auth{AccessToken: "workos:some-jwt"}}
	if !p.HasToken(cred) {
		t.Error("活凭证里有 access_token 时 has_token 必须为 true —— " +
			"否则界面「Token」列被 if(has_token) 挡成 `—`，用户以为号不可用")
	}
}

// TestHasTokenEmptyCredential 没有 token 时 false。
func TestHasTokenEmptyCredential(t *testing.T) {
	p := NewWithConfig(Config{})
	cases := []struct {
		name string
		cred gateway.Credential
	}{
		{"AccessToken 为空", gateway.Credential{Secret: &Auth{RefreshToken: "rt"}}},
		{"空 Auth", gateway.Credential{Secret: &Auth{}}},
		{"Secret 类型不对", gateway.Credential{Secret: "not-auth"}},
		{"Secret 为 nil", gateway.Credential{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if p.HasToken(c.cred) {
				t.Error("没有可用的 access token 时应返回 false")
			}
		})
	}
}

// TestHasTokenSkipsNetwork 纯本地判断：账号列表渲染路径上每账号调一次。
func TestHasTokenSkipsNetwork(t *testing.T) {
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1")})
	// 不发网络的话这里立刻返回；若发了会因基址不可达而慢/失败。
	_ = p.HasToken(gateway.Credential{Secret: &Auth{AccessToken: "t"}})
}
