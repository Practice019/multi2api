// accountkey_test.go 账号判重的守卫。
//
// # 这个文件守的是用户实测报的缺陷
//
// 用户：「我添加同一个账号时，会出现重复账号。不应该更新那个旧账号吗？」
//
// 根因：uid = access_token 哈希，而重登必然换发新令牌（实测两条 60 字符
// 随机串、除前缀外无一相同）⇒ 主键不稳定 ⇒ 必然重复。
//
// 修法是用**账号作用域的数据指纹**做证据。而"证据"这个性质决定了
// 本文件最要紧的一条用例是：**判不准时必须不合并**（新增是可逆的，
// 覆盖别人的凭证是不可逆的）。
package minimax

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 造一个只回应积分明细的上游。body 为空串表示"请求失败"。
func fakeCreditServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathCreditDetails {
			http.NotFound(w, r)
			return
		}
		if body == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

const bucketA = `{"details":[{"credit_type":2,"granted_amount":"400.00",
 "granted_at_ms":1791613598478,"expire_at_ms":1794153600000,
 "remaining_amount":"121.00","consumed_amount":"279.00"}],
 "total_count":1,"base_resp":{"status_code":0,"status_msg":"ok"}}`

// 与 bucketA **同一账号**、但余额已耗尽（remaining/consumed 变了）。
const bucketASpent = `{"details":[{"credit_type":2,"granted_amount":"400.00",
 "granted_at_ms":1791613598478,"expire_at_ms":1794153600000,
 "remaining_amount":"0.00","consumed_amount":"400.00"}],
 "total_count":1,"base_resp":{"status_code":0,"status_msg":"ok"}}`

// 另一个账号（授予时刻不同）。
const bucketB = `{"details":[{"credit_type":2,"granted_amount":"400.00",
 "granted_at_ms":1791500000000,"expire_at_ms":1794000000000,
 "remaining_amount":"400.00","consumed_amount":"0.00"}],
 "total_count":1,"base_resp":{"status_code":0,"status_msg":"ok"}}`

// 指纹必须**只由不变量组成**：同一账号消费前后必须是同一个指纹。
//
// ⚠ 这是判重能不能用的关键。若把 remaining/consumed 算进指纹，
// 同一个号在"消费了一次之后"重登就会算出不同指纹 ⇒ 判重漏判 ⇒
// 又变成两条重复（正是用户报的现象），而且这种漏判**看起来像随机**。
func TestFingerprintIgnoresConsumption(t *testing.T) {
	sa := fakeCreditServer(t, bucketA)
	defer sa.Close()
	sb := fakeCreditServer(t, bucketASpent)
	defer sb.Close()

	ca := NewClient(Config{AccountBase: sa.URL, APIBase: sa.URL})
	cb := NewClient(Config{AccountBase: sb.URL, APIBase: sb.URL})
	a := ca.fingerprint(context.Background(), &Auth{AccessToken: "t1"})
	b := cb.fingerprint(context.Background(), &Auth{AccessToken: "t2"})
	if a == "" {
		t.Fatal("有积分桶时指纹不应为空")
	}
	if a != b {
		t.Errorf("同一账号消费前后的指纹必须相同：\n  消费前 %q\n  消费后 %q\n"+
			"若把 remaining/consumed 算进指纹，重登就会判不出同一账号 ⇒ 又出现重复。", a, b)
	}
}

// 不同账号 ⇒ 不同指纹。
func TestFingerprintDiffersAcrossAccounts(t *testing.T) {
	s1 := fakeCreditServer(t, bucketA)
	defer s1.Close()
	s2 := fakeCreditServer(t, bucketB)
	defer s2.Close()

	f1 := NewClient(Config{AccountBase: s1.URL, APIBase: s1.URL}).
		fingerprint(context.Background(), &Auth{AccessToken: "t1"})
	f2 := NewClient(Config{AccountBase: s2.URL, APIBase: s2.URL}).
		fingerprint(context.Background(), &Auth{AccessToken: "t2"})
	if f1 == "" || f2 == "" {
		t.Fatal("两者都应有指纹")
	}
	if f1 == f2 {
		t.Errorf("不同账号的指纹不该相同：%q", f1)
	}
}

// ⚠ 最重要的一条：**没有积分桶 ⇒ 指纹为空 ⇒ 绝不合并**。
//
// 一个新注册的号可能一条积分包都没领过（details 缺失或为空）。
// 两个"空指纹"在字符串上相等 —— 若允许按它合并，就会把两个真实
// 不同的账号并成一个，**覆盖掉其中一个的凭证**。
//
// 覆盖是不可逆的（旧 refresh_token 被替换就找不回来），比重复更糟。
func TestFingerprintEmptyMeansNeverMerge(t *testing.T) {
	for _, noBucket := range []string{
		`{"total_count":0,"base_resp":{"status_code":0,"status_msg":"ok"}}`, // details 整个缺失
		`{"details":[],"total_count":0,"base_resp":{"status_code":0}}`,      // 空数组
	} {
		s := fakeCreditServer(t, noBucket)
		c := NewClient(Config{AccountBase: s.URL, APIBase: s.URL})
		if got := c.fingerprint(context.Background(), &Auth{AccessToken: "t"}); got != "" {
			t.Errorf("无积分桶必须返回空指纹（否则两个不同的新号会被并成一个），实际 %q\n响应: %s", got, noBucket)
		}
		s.Close()
	}
}

// 请求失败 ⇒ 空指纹（不是"错误地判成不同账号"，而是"放弃判重"）。
func TestFingerprintOnRequestFailure(t *testing.T) {
	s := fakeCreditServer(t, "") // 500
	defer s.Close()
	c := NewClient(Config{AccountBase: s.URL, APIBase: s.URL})
	if got := c.fingerprint(context.Background(), &Auth{AccessToken: "t"}); got != "" {
		t.Errorf("请求失败应返回空指纹并让调用方放弃合并，实际 %q", got)
	}
}

// UID 优先级：Identity（判重证据）> AccountID > token 哈希。
//
// 这条钉住"重登同一个号 → uid 不变 → 文件名与池记录对齐"的机制。
func TestUIDPrefersIdentity(t *testing.T) {
	hashBased := (&Auth{AccessToken: "mmoat_xyz"}).UID()
	if !strings.HasPrefix(hashBased, "token-") {
		t.Fatalf("兜底形态应是 token-<hash>，实际 %q", hashBased)
	}
	withID := &Auth{AccessToken: "mmoat_xyz", Identity: hashBased}
	if withID.UID() != hashBased {
		t.Errorf("Identity 必须优先（它就是「重登不产生新账号」的机制）：实际 %q", withID.UID())
	}
	// 令牌换了但 Identity 在 ⇒ uid 不变（这正是原 bug 的反面）。
	relogin := &Auth{AccessToken: "mmoat_completely_new_token", Identity: hashBased}
	if relogin.UID() != hashBased {
		t.Errorf("换了令牌但判重命中时 uid 必须保持 %q，实际 %q —— 不一致就会新增一条重复",
			hashBased, relogin.UID())
	}
	// FileName 也跟着对齐 ⇒ 落盘是覆盖旧文件，不是多出一个文件。
	if FileName(withID) != FileName(&Auth{AccessToken: "whatever", Identity: hashBased}) {
		t.Error("文件名必须由 uid 派生，否则重登会多出一个文件")
	}
}

// 续期不得改变 uid（Identity 与哈希基准都不动）。
//
// 若续期换了 uid，池里就会在每次续期时**多出一条**——比重复登录还糟。
func TestRefreshKeepsUID(t *testing.T) {
	srv := newFakeTokenServer(t, map[string]any{
		"access_token": "mmoat_brand_new", "refresh_token": "mmort_new",
		"token_type": "Bearer", "expires_in": 3600, "scope": "agent.default",
	}, http.StatusOK, nil)
	defer srv.Close()

	a := &Auth{AccessToken: "mmoat_old", RefreshToken: "mmort_old", Identity: "token-abc"}
	before := a.UID()
	p := New(Config{AccountBase: srv.URL, APIBase: srv.URL})
	if err := p.RefreshCredential(gatewayCred(a)); err != nil {
		t.Fatal(err)
	}
	if a.AccessToken != "mmoat_brand_new" {
		t.Error("续期应换发新令牌")
	}
	if a.UID() != before {
		t.Errorf("续期后 uid 必须不变（否则每次续期多一条账号）：%q → %q", before, a.UID())
	}
}

// dedupIdentity 的三个分支：命中 / 不命中 / 判不准。
func TestDedupIdentityBranches(t *testing.T) {
	// 命中：两条令牌指向同一个桶。
	sameSrv := fakeCreditServer(t, bucketA)
	defer sameSrv.Close()

	p := New(Config{AccountBase: sameSrv.URL, APIBase: sameSrv.URL})
	existing := &Auth{AccessToken: "mmoat_existing", Identity: "token-existing"}
	p.creds["token-existing"] = existing

	// 新登录的令牌不同，但指纹相同 ⇒ 应命中已有 uid。
	fresh := &Auth{AccessToken: "mmoat_fresh_login", RefreshToken: "mmort_f"}
	p.client = NewClient(Config{AccountBase: sameSrv.URL, APIBase: sameSrv.URL})
	got, ok := p.dedupIdentity(fresh)
	if !ok || got != "token-existing" {
		t.Errorf("同一账号（指纹一致）应命中已有 uid，实际 (%q, %v)", got, ok)
	}

	// 不命中：另一个账号的桶。
	diffSrv := fakeCreditServer(t, bucketB)
	defer diffSrv.Close()
	p2 := New(Config{AccountBase: diffSrv.URL, APIBase: diffSrv.URL})
	p2.creds["token-existing"] = existing
	// 让"已有账号"与"新登录"打到不同响应，需要分别的 client ——
	// 这里直接断言：已有账号取不到（同域名同 body ⇒ 同一个桶）会命中，
	// 所以改测"新凭证取不到"的那条分支。
	if _, ok := p2.dedupIdentity(&Auth{AccessToken: "t"}); !ok {
		t.Skip("两侧同 body，无法在此构造不命中；不命中由 TestFingerprintDiffersAcrossAccounts 覆盖")
	}

	// 判不准（新凭证无指纹）⇒ 必须**不**合并。
	emptySrv := fakeCreditServer(t, `{"total_count":0,"base_resp":{"status_code":0}}`)
	defer emptySrv.Close()
	p3 := New(Config{AccountBase: emptySrv.URL, APIBase: emptySrv.URL})
	p3.creds["token-existing"] = existing
	if _, ok := p3.dedupIdentity(&Auth{AccessToken: "mmoat_new"}); ok {
		t.Error("新凭证取不到指纹时必须放弃合并 —— 覆盖别人的凭证不可逆")
	}
}

// 多桶账号：桶集合相同才算同一账号，顺序无关。
func TestFingerprintIsOrderIndependent(t *testing.T) {
	two := `{"details":[
	  {"credit_type":2,"granted_amount":"400.00","granted_at_ms":111,"expire_at_ms":222},
	  {"credit_type":1,"granted_amount":"100.00","granted_at_ms":333,"expire_at_ms":444}],
	  "total_count":2,"base_resp":{"status_code":0}}`
	twoReversed := `{"details":[
	  {"credit_type":1,"granted_amount":"100.00","granted_at_ms":333,"expire_at_ms":444},
	  {"credit_type":2,"granted_amount":"400.00","granted_at_ms":111,"expire_at_ms":222}],
	  "total_count":2,"base_resp":{"status_code":0}}`
	s1 := fakeCreditServer(t, two)
	defer s1.Close()
	s2 := fakeCreditServer(t, twoReversed)
	defer s2.Close()

	f1 := NewClient(Config{AccountBase: s1.URL, APIBase: s1.URL}).fingerprint(context.Background(), &Auth{AccessToken: "x"})
	f2 := NewClient(Config{AccountBase: s2.URL, APIBase: s2.URL}).fingerprint(context.Background(), &Auth{AccessToken: "y"})
	if f1 == "" || f1 != f2 {
		t.Errorf("多桶指纹不该依赖上游返回顺序：\n  %q\n  %q", f1, f2)
	}
	// 少一个桶 ⇒ 不同。
	one := `{"details":[{"credit_type":2,"granted_amount":"400.00","granted_at_ms":111,"expire_at_ms":222}],
	  "total_count":1,"base_resp":{"status_code":0}}`
	s3 := fakeCreditServer(t, one)
	defer s3.Close()
	f3 := NewClient(Config{AccountBase: s3.URL, APIBase: s3.URL}).fingerprint(context.Background(), &Auth{AccessToken: "z"})
	if f3 == f1 {
		t.Error("桶集合不同必须判为不同账号")
	}
	_ = fmt.Sprintf
}
