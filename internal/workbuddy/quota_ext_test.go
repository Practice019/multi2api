// quota_ext_test.go `gateway.QuotaExt` 在 workbuddy 上的实现（RefreshQuota）。
//
// # 这组测试守的是两个"改一个字就静默出错"的判据
//
// 判据 a：HasData 必须取 res.HasQuota，**不能**取 res.Credits != 0
// 判据 b：不能直接把 RefreshCredits 的第二个返回值（claimed）当成 ok
//
// 两条都**不会**让编译失败、不会 panic、不会打错误日志 —— 表现只是
// 界面上某一格显示 `—` 或某个计数多一个。这正是它们需要显式用例的原因：
// 变异验证（改回错误写法）时，必须有一条**恰好**红。
//
// # ⚠ 变异验证实测结论（务必读，它推翻了一个想当然的假设）
//
// 判据 a 的守门用例（测试 2）经实测**确实**能杀掉变异：把判据换成
// `res.Credits == 0` 后它单独变红，其余全绿。
//
// 判据 b 的守门用例（测试 3）能杀掉的变异是**"失败分支被当成成功"**
// （例如把 Status 判据删掉、直接 `return gateway.CreditsQuota(...), claimed`）——
// 此时它因 HasData=true 而红。
//
// 但它**杀不掉** `return gateway.UnknownQuota(), claimed` 与
// `return gateway.UnknownQuota(), true` 这两写的差异：
// 在 AuthByUID==nil 那条分支上 claimed 恰好是 **true**（history.go:53），
// 两个表达式是同一个值。而 ok=false 的那两支 claimed 本来就是 false，
// 同样分不出来。也就是说 **"claimed 能不能当 ok 用"在本包现有 API 下不可观测**。
// 测试 3 因此钉的是"失败时 HasData=false 且不动池"，不是"ok 的取值"。
//
// 这也是为什么下面每条用例都显式构造触发条件，而不是"顺手过"：
//
//	判据 a 只在 `remain == 0` 时与错误写法分叉
//	判据 b 只在失败分支被当成成功时分叉（而非在 claimed 的取值上分叉）
//
// 一个"正常账号有额度"的用例对两者都无感 —— 它会在两种写法下都绿。
package workbuddy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// userResourceBody 造一个 get-user-resource 的上游成功响应。
//
// 用 CycleCapacity* 那一组字段：UserResource 的取值顺序是
// `CycleCapacitySize > 0` 优先（upstream/client.go:449），
// 所以这条路径最直接地钉住"上游说了多少就是多少"。
//
// 传 0 时上游是**明确地**回答"还剩 0"（不是"没回答"）——
// 这正是判据 a 要区分的那种情况。
//
// 数字拼装复用同包 history_scope_test.go 的 itoa（避免再引入一个重复工具，
// 也避免为一个整数把 strconv 拖进断言消息里）。
func userResourceBody(remain int) string {
	n := itoa(remain)
	return `{"code":0,"msg":"OK","data":{"Response":{"Data":{"Accounts":[{` +
		`"PackageName":"p","CapacitySize":100,"CapacityRemain":` + n + `,` +
		`"CapacityUsed":0,"CycleCapacitySize":100,` +
		`"CycleCapacityRemain":` + n + `,"CycleCapacityUsed":0}]}}}}`
}

// quotaStubUpstream 起一个只回答 get-user-resource 的假上游。
func quotaStubUpstream(t *testing.T, body string, status int) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Header().Set("Content-Type", "application/json")
			if status != 0 && status != http.StatusOK {
				w.WriteHeader(status)
			}
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/get-dosage-notify"):
			// 故障路径会顺带查告警文案（history.go:65）。
			// 这里刻意**不给**告警：本文件的用例与文案无关，
			// 给它反而会让 Detail 混入噪音（那部分已由 dosage_wiring_test.go 覆盖）。
			_, _ = w.Write([]byte(`{"code":0,"data":{"dosageNotifyCode":0}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
}

// quotaProviderWithPool 造一个接了**给定池**的 Provider（额度路径用）。
//
// 与 newCreditsProvider 的区别：那个写死了"池里只有一个 u1"，
// 而判据 b 需要造"池里有号但 AuthByUID 返回 nil"这种池，
// 判据 a 需要控制上游返回的具体数额。所以池与上游都由用例给。
func quotaProviderWithPool(t *testing.T, p *pool.Pool, up *upstream.Client) *Provider {
	t.Helper()
	return NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up})
}

// nilAuthPool 本身已经是 AccountPool 的实现，直接当池递进去即可 ——
// 与 quotaProviderWithPool 的其他调用点不同，这里没有 *pool.Pool 可包。
func quotaProviderWithNilAuthPool(t *testing.T, np *nilAuthPool, up *upstream.Client) *Provider {
	t.Helper()
	return NewWithConfig(Config{Pool: np, Client: up})
}

// ---------------------------------------------------------------------------
// 用例 1：正常路径
// ---------------------------------------------------------------------------

// TestRefreshQuotaOK 正常账号必须被判为"能被认到、且有数据"。
//
// 这是被 refreshQuotas 认到的**前提**：ok=true 才不会被记进 Failed，
// HasData=true 才会走到 pool.SetQuota（quota_refresh.go:113）。
//
// 也就是说这条同时是那个 bug 的回归测试 —— 在加本文件之前，
// workbuddy 根本没有 RefreshQuota 方法，core 连问都不会问它。
func TestRefreshQuotaOK(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Nickname: "测试号"})
	prov := quotaProviderWithPool(t, p, quotaStubUpstream(t, userResourceBody(42), 0))

	qv, ok := prov.RefreshQuota("u1")

	if !ok {
		t.Fatalf("账号存在且属于本上游时必须 ok=true（否则会被记成 Failed 并静默跳过）")
	}
	if !qv.HasData {
		t.Fatalf("上游明确返回了余额，必须 HasData=true，得到 %+v", qv)
	}
	if qv.Kind != gateway.QuotaKindCredits {
		t.Errorf("workbuddy 的额度是单值积分，期望 kind=%q 得到 %q",
			gateway.QuotaKindCredits, qv.Kind)
	}
	if qv.Remaining != 42 {
		t.Errorf("Remaining=%d want 42（上游说了多少就该是多少）", qv.Remaining)
	}
}

// ---------------------------------------------------------------------------
// 用例 2：判据 a —— remain=0 是"查到了，确实是 0"
// ---------------------------------------------------------------------------

// TestRefreshQuotaZeroRemainIsDataNotUnknown 【判据 a 的守门用例】
//
// 上游明确回答 `remain=0` 时，返回必须是
// `HasData=true, Remaining=0` —— **不是** Unknown。
//
// # 为什么这条最要紧
//
// 两种写法在这条上分叉，而它们的用户可见后果**方向相反**：
//
//	正确（取 res.HasQuota）→ 界面显示 `0`   → 用户知道这号空了
//	错误（取 Credits != 0）→ 界面显示 `—`   → 用户以为额度未知，
//	                                          于是继续把请求打给它
//
// 后者更糟：显示 0 只是难看，显示 `—` 会让人**误以为还能用**。
//
// # 它与 codearts 那条判据是同一个不变式
//
// codearts 用 `sub.Status == ""` 判"上游没给套餐块"（quota_ext.go:89），
// 明确写了"比 CreditTotal==0 更可靠 —— 真实存在总额为 0 的合法套餐"。
// 本包对应的权威字段就是 CreditsResult.HasQuota（history.go:71 上
// Credits 与 HasQuota 一起被赋值）。
//
// 变异验证：把 quota_ext.go 里的 `!res.HasQuota` 改成 `res.Credits == 0`，
// 本用例必须红。
func TestRefreshQuotaZeroRemainIsDataNotUnknown(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-zero", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Nickname: "空额度号"})
	// ⚠ 前置：这条路径必须真的走"上游成功"那一支。
	// 否则本用例会因为失败而 HasData=false，看起来"实现是对的"。
	prov := quotaProviderWithPool(t, p, quotaStubUpstream(t, userResourceBody(0), 0))

	// 先直接验一遍前置事实：RefreshCredits 必须报 OK 且 HasQuota=true。
	// 这两条一起才构成"上游查到了 0" —— 缺任一条本用例都会假绿。
	res, claimed := prov.RefreshCredits("u-zero", quotaRefreshTrigger)
	if !claimed || res.Status != checkinlog.StatusOK {
		t.Fatalf("测试装置失效：上游应返回成功，得到 claimed=%v status=%s detail=%s",
			claimed, res.Status, res.Detail)
	}
	if !res.HasQuota {
		t.Fatal("测试装置失效：成功路径必须带 HasQuota=true（history.go:71）")
	}
	if res.Credits != 0 {
		t.Fatalf("测试装置失效：本用例要的是 remain=0，得到 %d", res.Credits)
	}

	qv, ok := prov.RefreshQuota("u-zero")

	if !ok {
		t.Fatal("号存在且属于本上游，ok 必须为 true")
	}
	if !qv.HasData {
		t.Fatalf("上游查到了 0 就该报「有数据、值是 0」，而不是未知：%+v\n"+
			"（若这里红了：判据被写成了 Credits != 0。那会让界面显示 `—`，"+
			"用户以为额度未知而继续用这个号 —— 比显示 0 更糟）", qv)
	}
	if qv.Remaining != 0 {
		t.Errorf("Remaining=%d want 0（0 是一个合法答案，不该被改写成别的数）", qv.Remaining)
	}
	if qv.Kind != gateway.QuotaKindCredits {
		t.Errorf("kind=%q want %q（未知额度是 HasData=false 的空视图，不该有别 kind）",
			qv.Kind, gateway.QuotaKindCredits)
	}
}

// ---------------------------------------------------------------------------
// 用例 3：判据 b —— 认领了但拿不到额度，必须 ok=true
// ---------------------------------------------------------------------------

// nilAuthPool 是一个"号报告存在、但取不到凭证"的池。
//
// # 为什么必须手搓这个装置（而不是用真池）
//
// 真池（pool.Pool）里 `Has(uid)` 与 `AuthByUID(uid) != nil` 是同源的：
// 账号在 byUID 里，AuthByUID 就返回它的 a。要让两者分叉只能直接造
// 这个组合 —— 而它**不是**测试为了凑用例编出来的状态，它对应真实场景：
// 池里登记了账号、但该账号的凭证此刻不可用（凭证被撤/待重新登录）。
//
// 这正是 history.go:53 那条分支，也是全仓唯一能把它走出来的入口 ——
// 生产代码里没有任何路径会构造出它，所以这条判据只有在这里被证明。
type nilAuthPool struct {
	// uids 报告"存在"的 uid 集合（Has 返回 true）。
	uids map[string]bool
	// setCredits 记录谁写过余额 —— 用例据此断言"取不到时不动池"。
	setCredits []string
	// reenabled 记录谁被解冻过。
	reenabled []string
}

func newNilAuthPool(uids ...string) *nilAuthPool {
	m := make(map[string]bool, len(uids))
	for _, u := range uids {
		m[u] = true
	}
	return &nilAuthPool{uids: m}
}

func (n *nilAuthPool) List() []Account             { return nil }
func (n *nilAuthPool) ListFor(string) []Account    { return nil }
func (n *nilAuthPool) Has(uid string) bool         { return n.uids[uid] }
func (n *nilAuthPool) AuthByUID(string) *auth.Auth { return nil } // ⚠ 这就是 history.go:53
func (n *nilAuthPool) SetCredits(uid string, _ int64) {
	n.setCredits = append(n.setCredits, uid)
}
func (n *nilAuthPool) ReenableIfUsable(uid string, _ bool, _ QuotaView) {
	n.reenabled = append(n.reenabled, uid)
}
func (n *nilAuthPool) Disable(string, string) {}

var _ AccountPool = (*nilAuthPool)(nil)

// TestRefreshQuotaClaimedButNoCredentialIsUnknownNotFailed 【判据 b 的守门用例】
//
// 池里有这个号、但 AuthByUID 返回 nil（history.go:53）时：
//
//	claimed = true（"这条 uid 我认领了"）
//	Status  = fail（没取到）
//
// 正确答案是 `(UnknownQuota(), **true**)`。
//
// # 为什么必须是 ok=true
//
// 契约里 ok 的语义是"这条 uid 我服务不了"（gateway/quota_ext.go:58），
// 不是"这次取值成功没成功"。号存在、也属于本上游，所以 ok 必须是 true。
//
// 若某天它变成 false，调用方（quota_refresh.go:104）会把它记进 out.Failed。
// 那是**错的分类**：这个号没有问题，只是当下拿不到额度，
// 正确的归类是 out.Unknown（界面显示 `—`，属于正常情况而非故障）。
// 把 Unknown 记成 Failed 会让"上游偶发不可达"看起来像故障并被误修，
// 正是 quota_refresh.go:44 那段注释明确要避免的事。
//
// ⚠ 但请注意：**这条 ok=true 断言本身杀不掉任何变异**，
//
//	因为 history.go:53 返回的 claimed 恰好就是 true。
//	详见下面「本用例边界」一节 —— 不要把它当成判据 b 的证明。
//
// # 为什么它同时是一条"不写池"的断言
//
// 因为 refreshQuotas 在 HasData=false 时**不调 SetQuota**
// （quota_refresh.go:108）—— 适配层必须给出 HasData=false 才能触发它。
// 这里顺带钉住"没有凭证就没有余额可写"，免得将来有人为了
// "让界面有点东西"而在失败路径上填 0。
//
// 变异验证：把 quota_ext.go 里 `res.Status != checkinlog.StatusOK` 那条判据
// 删掉、改成"直接落到返回有数据那一支"（`return gateway.CreditsQuota(res.Credits), claimed`），
// 本用例必须红 —— 断言 HasData 会失败，因为 claimed 在这条分支上是 **true**。
//
// # ⚠ 一条实测出来的、关于本用例边界的事实（别把它写成它证明不了的东西）
//
// 我最初以为这条用例也钉住了"`return claimed` 与 `return true` 的区别"。
// **变异实测证明它钉不住**：把失败支那句改成 `return gateway.UnknownQuota(), claimed`，
// 本用例仍然**全绿**。
//
// 原因是 history.go:53 那条分支返回的 claimed 恰好就是 **true**
// （"这条 uid 我认领了，只是没拿到凭证"）—— 所以在这个用例里
// `claimed` 与字面量 `true` 是**同一个值**，断言无法区分二者。
// 真正与 `claimed` 分叉的是 ok=false 那两支（池未接线 / 号不存在），
// 而那两支的 claimed 本来就是 false，同样区分不出来。
//
// 结论：**在本包现有 API 下，"claimed 能不能当 ok 用"在 AuthByUID==nil 这条
// 分支上不可观测**。这条用例能钉住的是另外两件确凿的事：
//
//  1. 失败时**不能报告有额度数据**（HasData=false）—— 这条能杀变异（见上）
//  2. 失败时**不动池**（不 SetCredits / 不 ReenableIfUsable）
//
// 所以本用例的价值是"HasData 与写池在这条分支上的行为"，不是"ok 的取值"。
// 把它写成后者会是**假安全感** —— 而假安全感比没有守卫更糟。
func TestRefreshQuotaClaimedButNoCredentialIsUnknownNotFailed(t *testing.T) {
	np := newNilAuthPool("u-nocred")
	// 上游给一个**有效**的响应：本用例要证明的是"没有凭证时根本不会去问上游"。
	// 若实现漏判了凭证，它会拿这个 42 一路报"有数据"。
	prov := quotaProviderWithNilAuthPool(t, np, quotaStubUpstream(t, userResourceBody(42), 0))

	// 前置：这条分支确实落在 history.go:53（claimed=true、Status=fail）。
	// 没有这一步，下面的断言可能在一条完全不同的分支上"通过"。
	res, claimed := prov.RefreshCredits("u-nocred", quotaRefreshTrigger)
	if !claimed {
		t.Fatal("测试装置失效：AuthByUID==nil 这条分支在 history.go:53 返回 claimed=true")
	}
	if res.Status == checkinlog.StatusOK {
		t.Fatalf("测试装置失效：本用例要的是失败分支，得到 status=%s", res.Status)
	}
	if res.HasQuota {
		t.Fatal("测试装置失效：失败分支不该带 HasQuota（history.go:60 那条 return 只赋 Status/Detail）")
	}

	qv, ok := prov.RefreshQuota("u-nocred")

	if !ok {
		t.Fatalf("号在池里、只是没有可用凭证，ok 必须是 true（ok 的语义是"+
			"「这条 uid 我服务不了」）—— 返回 false 会被 core 记进 Failed，"+
			"而正确的归类是 Unknown。得到 qv=%+v", qv)
	}
	if qv.HasData {
		t.Fatalf("没有可用凭证时不该报告有额度数据（那会显示成一个凭空的数）：%+v\n"+
			"（这里红了通常意味着 Status 判据被删掉、失败被当成了成功）", qv)
	}
	if qv.Remaining != 0 {
		t.Errorf("未知额度的 Remaining 应为 0（HasData=false 时该字段无意义）：%d", qv.Remaining)
	}
	if qv.Kind != "" {
		t.Errorf("未知额度不该带 kind（gateway.UnknownQuota 是空视图）：%q", qv.Kind)
	}
	if len(np.setCredits) != 0 {
		t.Errorf("取不到额度时不该写池，实际写了 %v", np.setCredits)
	}
	if len(np.reenabled) != 0 {
		t.Errorf("取不到额度时不该解冻账号，实际解冻了 %v", np.reenabled)
	}
}

// ---------------------------------------------------------------------------
// 用例 4：契约对齐 —— 服务不了的 uid 必须 ok=false
// ---------------------------------------------------------------------------

// TestRefreshQuotaMissingAccountReportsNotOK 号不存在时 ok=false。
//
// 调用方据此跳过/回 404（gateway/quota_ext.go:60），
// 而不是把一个"问不到"当成"额度是 0"。
//
// 与 codearts 的 TestRefreshQuota_NoAccountReportsNotOK 对称。
func TestRefreshQuotaMissingAccountReportsNotOK(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	prov := quotaProviderWithPool(t, p, quotaStubUpstream(t, userResourceBody(7), 0))

	qv, ok := prov.RefreshQuota("不存在的-uid")

	if ok {
		t.Errorf("号不存在时应 ok=false，得到 ok=true qv=%+v", qv)
	}
	if qv.HasData {
		t.Errorf("号不存在时不该报告有额度数据：%+v", qv)
	}
}

// TestRefreshQuotaPoolNotWiredReportsNotOK 账号池未接线时 ok=false。
//
// 与"号不存在"同一条契约（都是"我服务不了"），但走的是
// history.go:46 那一支 —— 部署里池为 nil 是合法状态（New() 就是），
// 不能 panic，也不能报"有额度"。
func TestRefreshQuotaPoolNotWiredReportsNotOK(t *testing.T) {
	// NewWithConfig(Config{}) 不接池 —— 与 New() 同一形态，
	// 但返回具体类型 *Provider（New 返回 gateway.Provider 接口，
	// 在它上面调不到 QuotaExt 的方法）。
	prov := NewWithConfig(Config{})

	qv, ok := prov.RefreshQuota("any-uid")

	if ok {
		t.Errorf("池未接线时应 ok=false，得到 ok=true qv=%+v", qv)
	}
	if qv.HasData {
		t.Errorf("池未接线时不该报告有额度数据：%+v", qv)
	}
}

// ---------------------------------------------------------------------------
// 用例 5：trigger 值域与历史落点
// ---------------------------------------------------------------------------

// TestQuotaRefreshTriggerMatchesLegacyEndpoint 钉住 trigger == "manual"。
//
// # 为什么不新造一个值（比如 "quota-ext"）
//
// 因为 checkinlog.Record.Trigger 的消费方是**二元显示**：
// webui.html:4380 只认 "manual" → "手动"，其余一律 "定时"。
// 新造值不会更精确，只会把一次人工点击显示成"定时"。
//
// 旧端点用的就是 "manual"（adminendpoints.go:228）——
// 同一个用户动作在两个入口下必须落同一个值，否则历史表里同一件事
// 会因为走的哪个按钮而显示成两种来源。
//
// 本用例同时断言**历史确实被写了一条**：适配层转调 RefreshCredits，
// 而它会 record（history.go:75）。这是"刷新额度会出现在任务历史里"的保证。
func TestQuotaRefreshTriggerMatchesLegacyEndpoint(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Nickname: "测试号"})
	up := quotaStubUpstream(t, userResourceBody(9), 0)
	lg := newIsolationLog(t)
	prov := NewWithConfig(Config{Pool: testPoolAdapter{p: p}, Client: up, Log: lg})

	if _, ok := prov.RefreshQuota("u1"); !ok {
		t.Fatal("前置：正常账号应 ok=true")
	}

	items, _ := lg.Page(0, 10, "")
	if len(items) == 0 {
		t.Fatal("刷新额度应写一条任务历史（RefreshCredits 内含 record）")
	}
	if got := items[0].Trigger; got != "manual" {
		t.Errorf("trigger=%q want %q —— webui 只把 \"manual\" 显示成「手动」，"+
			"其余都会落进 else 分支被显示成「定时」（webui.html:4380）",
			got, "manual")
	}
	if items[0].Kind != checkinlog.KindCredits {
		t.Errorf("历史 kind=%q want %q", items[0].Kind, checkinlog.KindCredits)
	}
	if items[0].Status != checkinlog.StatusOK {
		t.Errorf("历史 status=%q want %q", items[0].Status, checkinlog.StatusOK)
	}
}

// ---------------------------------------------------------------------------
// 用例 6：可被 core 的 registry 发现（与 codearts 对称）
// ---------------------------------------------------------------------------

// TestQuotaExtIsDiscoverable 扩展点必须能被 gateway.ExtOf 认出。
//
// core 的 refreshQuotas 就是靠 `gateway.ExtOf[gateway.QuotaExt](p)`
// 找到上游的额度实现的（quota_refresh.go:76）。断言方法集没写错，
// 比"编译过了"更强一层：接口多一个方法这里就会红。
func TestQuotaExtIsDiscoverable(t *testing.T) {
	ext, ok := gateway.ExtOf[gateway.QuotaExt](New())
	if !ok {
		t.Fatal("workbuddy 实现了 QuotaExt，却不能被 ExtOf 发现 —— " +
			"core 会静默跳过它（这正是本次要修的 bug 的形态）")
	}
	if ext == nil {
		t.Fatal("ExtOf 返回了 nil 实现")
	}
	// 真调一次，确认返回的确实接到同一个方法上。
	qv, ok := ext.RefreshQuota("any")
	if ok {
		t.Errorf("池未接线的实例不该服务任何 uid，得到 %+v", qv)
	}
}
