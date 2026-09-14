// token_lifetime_test.go —— 「这份凭证不会过期」这条第三态的守卫。
//
// # 这条第三态为什么必须存在（用户实测报的缺口）
//
// `AccountView` 原来只有两个指针表达过期信息：`token_expire_sec`（有到期时刻）
// 与"字段不出现"（不知道）。而 loomy 的 session **两者都不是** ——
// 它是服务端持久化的登录态，不绑时间。
//
// 只用两个指针，它只能落进"不知道"，界面显示 `—`，用户把它读成
// "这功能没做"。所以有了 `token_never_expires`（gateway.CredentialLifetimeExt）。
//
// # 这里守的两件事
//
//  1. **三态各自有对应输出**：只实现时刻 / 只实现永久 / 都不实现 ——
//     三种桩的行为必须**互不相同**（否则某一态被另一态吃掉，测试却全绿）。
//  2. **顺序**：先问时刻、拿不到才问永久。顺序反了会把一个有明确失效时刻的
//     凭证（codearts 的 2 小时 STS）渲染成「永久」—— 那是更强、更错的断言。
package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// ---------------------------------------------------------------------------
// 桩：三种形态
// ---------------------------------------------------------------------------

// bothStub **同时**实现了两个扩展点，且两者都给出肯定的答案。
//
// 这是唯一能检验"顺序"的桩：只实现一个的桩无论顺序怎么写都看不出来。
type bothStub struct {
	stubProvider
	// at 到期时刻（>0 时 TokenExpiry 回 (at,true)）
	at int64
	// never 为 true 时 NeverExpires 回 true
	never bool
}

func (s *bothStub) TokenExpiry(cred gateway.Credential) (int64, bool) {
	if s.at <= 0 {
		return 0, false
	}
	return s.at, true
}

func (s *bothStub) NeverExpires(cred gateway.Credential) bool { return s.never }

// lifetimeOnlyStub 只实现 CredentialLifetimeExt（= loomy 的形态）。
//
// 刻意**不**实现 CredentialExpiryExt：两个扩展点是独立发现的
// （`ExtOf` 各判一次），只有实现了后者才能被问到时刻。
type lifetimeOnlyStub struct {
	stubProvider
}

func (s *lifetimeOnlyStub) NeverExpires(cred gateway.Credential) bool { return true }

// neverStub 说"永久"但**没有** secret —— 核心必须按未知处理。
//
// # 为什么这条另开一个桩
//
// "永久"是一个**关于某个凭证**的断言。拿不到凭证（secret 缺失）时，
// 上游的实现根本没被调用过 —— 若核心在那时也填 true，就是在替一个
// 不存在的凭证断言永久有效。
type neverStub struct {
	stubProvider
}

func (s *neverStub) NeverExpires(cred gateway.Credential) bool { return true }

// ---------------------------------------------------------------------------
// 三态各自的输出
// ---------------------------------------------------------------------------

// TestAccountsReportsNeverExpires 只实现"永久"的上游 → token_never_expires=true。
func TestAccountsReportsNeverExpires(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&lifetimeOnlyStub{
		stubProvider: stubProvider{id: "lmy", caps: gateway.CapChat | gateway.CapQuotaProbe},
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("lmy", &auth.Auth{UID: "ly-1"}, "不透明的 secret")

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "lmy"}))[0]
	if !v.TokenNeverExpires {
		t.Fatal("★ 上游说「设计上不过期」，核心却没下发 token_never_expires —— " +
			"界面只能显示 `—`，用户会读成「这功能没做」")
	}
	if v.TokenExpireSec != nil || v.TokenExpireAt != nil {
		t.Errorf("「永久」与「有到期时刻」互斥，不该同时填（sec=%v at=%v）",
			v.TokenExpireSec, v.TokenExpireAt)
	}
}

// TestAccountsNeverExpiresWinsOnlyWhenNoExpiry ★ 顺序：时刻优先于「永久」。
//
// # 反向验证
//
// 把 accountViews 里那两段的顺序对调（先问永久、后问时刻），这条会红：
// 一个有明确失效时刻的凭证会被渲染成「永久」。
// codearts 的 STS 只活约 2 小时 —— 把它说成永久，用户就不会去换号。
func TestAccountsNeverExpiresWinsOnlyWhenNoExpiry(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&bothStub{
		stubProvider: stubProvider{id: "both", caps: gateway.CapChat},
		at:           time.Now().Add(time.Hour).Unix(),
		never:        true, // 故意同时说"永久"（上游自相矛盾）
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("both", &auth.Auth{UID: "b-1"}, "不透明的 secret")

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "both"}))[0]
	if v.TokenExpireSec == nil {
		t.Fatal("有到期时刻时必须以时刻为准")
	}
	if v.TokenNeverExpires {
		t.Fatalf("★ 同时给出时刻与「永久」时，核心填了永久（sec=%d）—— "+
			"顺序反了：一个 1 小时后失效的凭证被说成永不过期",
			*v.TokenExpireSec)
	}
}

// TestAccountsNeverExpiresFalseWhenNoSecret 拿不到 secret 时不许说「永久」。
//
// 上游实现返回 true，但它**没被调用过**（因为核心拿不到凭证）。
// 输出必须是未知 —— 与"没实现扩展点"完全一样。
func TestAccountsNeverExpiresFalseWhenNoSecret(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&neverStub{
		stubProvider: stubProvider{id: "ns", caps: gateway.CapChat},
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	// secret 传 nil（= 手工拷凭证但没走 WithSecrets 那个缺口）
	p.AddFor("ns", &auth.Auth{UID: "ns-1"}, nil)

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "ns"}))[0]
	if v.TokenNeverExpires {
		t.Error("★ 拿不到凭证时说「永久」—— 那是替一个不存在的凭证做断言")
	}
}

// TestAccountsPlainProviderHasNeither 两个都没实现 → 两个字段都不出现。
//
// 这是**向后兼容**那一条：workbuddy 与 codearts 的行为必须逐字节不变。
func TestAccountsPlainProviderHasNeither(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&plainStub{stubProvider{id: "wb", caps: gateway.CapChat}}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("wb", &auth.Auth{UID: "wb-1"}, nil)

	v := accountsFor(t, New(Config{Pool: p, Registry: reg, DefaultProvider: "wb"}))[0]
	if v.TokenNeverExpires {
		t.Error("没实现任何过期扩展点时不该下发 token_never_expires")
	}
	if v.TokenExpireSec != nil || v.TokenExpireAt != nil {
		t.Error("没实现时不该下发到期字段")
	}
}

// TestTokenNeverExpiresIsOmittedFromJSON 字段用 omitempty（老前端读到 undefined）。
//
// 与 token_expire_* 同一手法：字段**不出现**是老前端的下线形状，
// 它的 `if (a.token_never_expires === true)` 在 undefined 时走原有分支。
func TestTokenNeverExpiresIsOmittedFromJSON(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&plainStub{stubProvider{id: "wb", caps: gateway.CapChat}}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("wb", &auth.Auth{UID: "wb-1"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	New(Config{Pool: p, Registry: reg, DefaultProvider: "wb"}).ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "token_never_expires") {
		t.Errorf("false 时该字段不该出现在线格式里（omitempty）：\n%s", body)
	}
}

// TestCredentialNeverExpiresRefusesUnknownProvider 三种"拿不到"都安全返回 false。
//
// 任何一条 panic 都会让**整张账号表**打不出来（handler 崩在渲染路径上）。
func TestCredentialNeverExpiresRefusesUnknownProvider(t *testing.T) {
	reg := gateway.NewRegistry()
	if err := reg.Register(&lifetimeOnlyStub{
		stubProvider: stubProvider{id: "lmy", caps: gateway.CapChat | gateway.CapQuotaProbe},
	}); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.AddFor("lmy", &auth.Auth{UID: "ly-1"}, "s")
	h := New(Config{Pool: p, Registry: reg, DefaultProvider: "lmy"})

	cases := []struct{ name, uid, provider string }{
		{"账号不存在", "没有这个 uid", "lmy"},
		{"上游未注册", "ly-1", "ghostnet"},
		{"provider 为空", "ly-1", ""},
	}
	for _, c := range cases {
		if h.credentialNeverExpires(c.uid, c.provider) {
			t.Errorf("%s：应当返回 false（不确定不能说「永久」）", c.name)
		}
	}
}
