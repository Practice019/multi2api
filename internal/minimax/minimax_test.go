// minimax_test.go 协议判据的守卫。
//
// # 为什么每条都值得单独立一条
//
// 本上游的协议事实来自开源参照项目的**逐行提取**，其中好几条是
// "错误实现能在已知样本上得到相同结果"的形态 —— 那种缺陷**无法被
// 普通断言发现**（参照项目为此专门写了"必须构造让两者分叉的样本"）。
//
// 所以这里刻意记录**每条判据是被哪个观测到的分歧逼出来的**。
package minimax

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseCreditBalance 必须取 `Σ details[].remaining_amount`，
// **不是** `total_count`。
//
// # ⚠ 这条用例的全部价值在于"让两者分叉"
//
// 参照项目在这里踩过一个真实的坑：初版写成 `total = total_count`，
// 而 `total_count` 是 `details[]` 的**记录条数**。
//
// 它俩在"余额为 0"时**偶然相等**（都是 0）—— 所以任何只看 0 的断言
// 都是**同义反复**：它只能证明"0 还是 0"，无法区分两个语义。
// 缺陷只有在**领取积分后**才暴露（条数 1 / 余额 800），
// 那时用户界面显示"1 积分"。
//
// 所以下面这个样本是**故意构造的**：一条 800 的记录 ⇒ 期望 800 而非 1。
// 把 assert 改成 `total_count` 会让它立刻变红（见 mutation 验证）。
func TestParseCreditBalanceTakesRemainingAmountNotTotalCount(t *testing.T) {
	// 实测形态（照抄参照项目注释里的真实报文）：
	// remaining_amount 是**字符串**，total_count 是**记录条数**。
	raw := `{"details":[{"remaining_amount":"800.00","consumed_amount":"0.00",
	          "granted_amount":"800.00","credit_type":2}],
	         "total_count":1,"base_resp":{"status_code":0,"status_msg":"ok"}}`
	var top map[string]any
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	got, ok := parseCreditBalance(top)
	if !ok {
		t.Fatal("应解析出余额")
	}
	if got != 800 {
		t.Errorf("余额 = %v，期望 800 ——\n"+
			"`total_count` 是 details[] 的**记录条数**（这里是 1），"+
			"真实余额是 Σ remaining_amount。\n"+
			"两者在「余额为 0」时偶然相等，所以只有这种样本能分辨它们。", got)
	}
	// 多桶求和。
	raw2 := `{"details":[{"remaining_amount":"800.00"},{"remaining_amount":"150.50"}],
	          "total_count":2}`
	var top2 map[string]any
	_ = json.Unmarshal([]byte(raw2), &top2)
	if got2, _ := parseCreditBalance(top2); got2 != 950.5 {
		t.Errorf("多桶应合计 950.5，实际 %v", got2)
	}
}

// `details` 缺失 ⇒ 余额 **0**（有效结果），不是"查询失败"。
//
// # 为什么这条与"失败"必须分开
//
// 参照项目实测：账号余额为 0 时 `details` **整个字段都不出现**
// （不是空数组，是缺键）。把它当失败会让界面显示 `—`（"没查到"），
// 而真相是"这个号确实没有积分" —— 两者对用户的含义完全不同。
func TestParseCreditBalanceMissingDetailsMeansZero(t *testing.T) {
	// 有 total_count 但没 details ⇒ 0（有效）。
	var a map[string]any
	_ = json.Unmarshal([]byte(`{"total_count":0,"base_resp":{"status_code":0}}`), &a)
	got, ok := parseCreditBalance(a)
	if !ok {
		t.Error("有 total_count 时应当是**有效结果**（余额 0），不是失败")
	}
	if got != 0 {
		t.Errorf("余额应为 0，实际 %v", got)
	}

	// details 与 total_count **都缺** ⇒ 这才是不像余额响应 ⇒ 失败。
	var b map[string]any
	_ = json.Unmarshal([]byte(`{"something":"else"}`), &b)
	if _, ok2 := parseCreditBalance(b); ok2 {
		t.Error("details 与 total_count 都缺 ⇒ 应判失败（不编造 0）")
	}
}

// `remaining_amount` 是**字符串**，空串必须**先挡掉**。
//
// `strconv.ParseFloat("")` 会报错（好），但若实现用
// `Number(”)`-style 的语义就会得到 0 —— 那会把"缺字段"读成"0 积分"。
func TestParseCreditBalanceStringAmounts(t *testing.T) {
	var a map[string]any
	// 一条字符串、一条空串（空串那条必须被跳过而不是算 0）。
	_ = json.Unmarshal([]byte(`{"details":[
	   {"remaining_amount":"12.34"},{"remaining_amount":""},{"remaining_amount":"5.66"}
	 ],"total_count":3}`), &a)
	got, ok := parseCreditBalance(a)
	if !ok {
		t.Fatal("应解析出余额")
	}
	if got != 18 {
		t.Errorf("余额 = %v，期望 18（空串那条应跳过，不是算作 0 影响不了和 —— "+
			"但它也**不能**被当成有效值）", got)
	}
}

// 过期时刻解析：毫秒与秒都要认。
//
// ⚠ 只看毫秒会让"秒级那份"被当成 1970 年（账号永远显示"已过期"）；
// 只看秒会让毫秒份被放大成几万年后的未来。
func TestParseExpiryMillisAcceptsSecondsAndMillis(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"1791258329000", 1791258329000}, // 毫秒
		{"1791258329", 1791258329000},    // 秒 → ×1000
		{"", 0},                          // 空 = 不知道
		{"abc", 0},                       // 非数字 = 不知道
		{"-1", 0},                        // 负 = 不知道
		{"1791258329.5", 0},              // 带小数（只认纯数字串）
	}
	for _, c := range cases {
		if got := parseExpiryMillis(c.in); got != c.want {
			t.Errorf("parseExpiryMillis(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// 签到：幂等判据是 `claim_result`，**不是** HTTP 状态码。
//
//	1 = 真领到 → ok
//	2 = 已领过 → already（**必须**与"领到"区分）
//	其他/缺失/类型不对 → **失败**（绝不虚报成功）
//
// ⚠ "虚报成功"是最坏的一类：用户以为 +了积分而实际 +0
// （参照项目明确记过这个形态）。
func TestSigninClaimResultUsesClaimResultNotHTTPStatus(t *testing.T) {
	// 真领到：注意 points=800 / bonus_points=400 时**只能是 800**。
	out, err := signinClaimResult(map[string]any{
		"claim_result": float64(1), "points": float64(800), "bonus_points": float64(400),
	})
	if err != nil {
		t.Fatalf("claim_result=1 应成功: %v", err)
	}
	if out.Status != "ok" {
		t.Errorf("claim_result=1 应是 ok，实际 %q", out.Status)
	}
	if out.Credit != 800 {
		t.Errorf("积分 = %d，期望 800 ——\n"+
			"`bonus_points` 是 `points` 的**子集**，不是额外加量。\n"+
			"相加（1200）会让展示金额虚高一倍（参照项目里用户亲自纠正过）。",
			out.Credit)
	}

	// 已领过：必须与"领到"区分。
	out2, err2 := signinClaimResult(map[string]any{"claim_result": float64(2)})
	if err2 != nil {
		t.Fatalf("claim_result=2 不该报错: %v", err2)
	}
	if out2.Status != "already" {
		t.Errorf("claim_result=2 应是 already，实际 %q —— 报成 ok 会让用户"+
			"以为积分又加了一次", out2.Status)
	}

	// 缺失/类型不对/越界 ⇒ 一律失败（不虚报成功）。
	for _, bad := range []any{
		nil, "1", float64(0), float64(9), float64(3),
	} {
		m := map[string]any{"claim_result": bad}
		if bad == nil {
			m = map[string]any{}
		}
		if _, err3 := signinClaimResult(m); err3 == nil {
			t.Errorf("claim_result=%v 应判失败（缺失/类型不对/越界都不能虚报成功）", bad)
		}
	}
}

// 上下文窗口取**档位表最大档**，不是 `limit.context`。
//
// ⚠ 参照项目为这条专门写了理由：M3.1 的 `limit.context` 是 512000，
// 而 `context_window_options` 是 [512000, 1000000]。
// 填 512K 会让客户端**远早于官方能力**触发上下文压缩。
func TestNormalizeModelTakesMaxContextWindowOption(t *testing.T) {
	var m remoteModel
	raw := `{"name":"M3.1-Flash-Preview",
	         "limit":{"context":512000,"output":128000},
	         "context_window_options":[512000,1000000],
	         "thinking_config":{"mode":"forced_on"}}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	e := normalizeModel("MiniMax-M3.1-Flash-Preview", m)
	if e.ContextWin != 1_000_000 {
		t.Errorf("窗口 = %d，期望 1000000（档位表最大档）——\n"+
			"用 limit.context（512000）会让客户端远早于官方能力触发压缩。", e.ContextWin)
	}
	if e.MaxTokens != 128_000 {
		t.Errorf("输出上限 = %d，期望 128000", e.MaxTokens)
	}
	if e.ThinkingMode != thinkingModeForcedOn {
		t.Errorf("thinking mode = %q，期望 forced_on", e.ThinkingMode)
	}

	// 档位表缺失时才回退 limit.context（那是有据的回落）。
	var m2 remoteModel
	_ = json.Unmarshal([]byte(`{"name":"X","limit":{"context":200000,"output":8000}}`), &m2)
	if e2 := normalizeModel("MiniMax-M2.7", m2); e2.ContextWin != 200_000 {
		t.Errorf("无档位表时应回退 limit.context=200000，实际 %d", e2.ContextWin)
	}
}

// 图片能力取自 `modalities.input` 含 `image`。
func TestNormalizeModelImageSupport(t *testing.T) {
	var m remoteModel
	_ = json.Unmarshal([]byte(`{"name":"A","modalities":{"input":["text","image"]}}`), &m)
	if e := normalizeModel("A", m); !e.SupportsImage {
		t.Error("modalities.input 含 image ⇒ 应支持图片")
	}
	var m2 remoteModel
	_ = json.Unmarshal([]byte(`{"name":"B","modalities":{"input":["text"]}}`), &m2)
	if e := normalizeModel("B", m2); e.SupportsImage {
		t.Error("modalities.input 不含 image ⇒ 不该声明支持图片")
	}
}

// M3.1 必须发 adaptive（前缀判据），其余不用。
//
// ⚠ 判据是**模型名前缀** `MiniMax-M3.1`，不是等值比较 ——
// 将来出现 `MiniMax-M3.1-x` 的兄弟模型时它同样要求 adaptive。
func TestRequiresAdaptiveThinkingIsPrefixBased(t *testing.T) {
	for _, yes := range []string{
		"MiniMax-M3.1-Flash-Preview", "MiniMax-M3.1", "MiniMax-M3.1-Future-Sibling",
	} {
		if !requiresAdaptiveThinking(yes) {
			t.Errorf("%q 必须要求 adaptive thinking（前缀判据）", yes)
		}
	}
	for _, no := range []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed", ""} {
		if requiresAdaptiveThinking(no) {
			t.Errorf("%q 不该要求 adaptive", no)
		}
	}
}

// 兜底模型表必须含 M3.1 —— 客户端内置表漏了它。
//
// ⚠ 参照项目实测：客户端 `config.js` 的内置表只有 3 个
// （M3 / M2.7-highspeed / M2.7），而远端有 **4** 个 —— 恰好漏掉
// `MiniMax-M3.1-Flash-Preview`，而它是客户端界面上被选中的那个。
// 若兜底表也漏掉它，远端一失败用户就**看不到自己在用的模型**。
func TestFallbackModelsIncludeM31(t *testing.T) {
	list := fallbackModels()
	if len(list) != 4 {
		t.Fatalf("兜底表应是 4 个（远端实测数量），实际 %d", len(list))
	}
	var found bool
	for _, e := range list {
		if e.ID == "MiniMax-M3.1-Flash-Preview" {
			found = true
			// 只有它有档位（其余三个远端没有 effort_options 字段）。
			if len(e.EffortOptions) != 6 {
				t.Errorf("M3.1 应有 6 个档位，实际 %v", e.EffortOptions)
			}
			if e.DefaultEffort != "default" {
				t.Errorf("M3.1 的默认档位应是 default，实际 %q", e.DefaultEffort)
			}
		} else if len(e.EffortOptions) != 0 {
			t.Errorf("%s 不该有档位（远端没有 effort_options 字段 —— 编档位就是猜测）", e.ID)
		}
	}
	if !found {
		t.Error("兜底表必须含 MiniMax-M3.1-Flash-Preview —— 客户端内置表漏了它，" +
			"兜底再漏掉用户就看不到自己在用的模型")
	}
}

// 目录响应：`models` 是**对象**（键即 id），且按 `model_order` 排序。
func TestParseModelsShapeAndOrder(t *testing.T) {
	raw := `{"providers":[{"providerId":"minimax","config":{
	   "models":{
	     "MiniMax-M2.7":{"name":"M2.7"},
	     "MiniMax-M3.1-Flash-Preview":{"name":"M3.1-Flash-Preview"}
	   },
	   "model_order":["MiniMax-M3.1-Flash-Preview","MiniMax-M2.7"]}}]}`
	list, err := parseModels([]byte(raw))
	if err != nil {
		t.Fatalf("应能解析: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 个模型，实际 %d", len(list))
	}
	if list[0].ID != "MiniMax-M3.1-Flash-Preview" {
		t.Errorf("应按 model_order 排序（第一个应是 M3.1），实际 %q", list[0].ID)
	}
	// 键即 id。
	if list[0].Name != "M3.1-Flash-Preview" {
		t.Errorf("name 应取条目里的短名，实际 %q", list[0].Name)
	}

	// 不是 minimax 的 provider ⇒ 报错（不静默返回空）。
	if _, err2 := parseModels([]byte(`{"providers":[{"providerId":"other","config":{"models":{"x":{}}}}]}`)); err2 == nil {
		t.Error("providerId 不是 minimax 时应报错（静默空目录会让界面少一堆模型）")
	}
	// 非 JSON ⇒ 报错。
	if _, err3 := parseModels([]byte(`not json`)); err3 == nil {
		t.Error("非 JSON 应报错")
	}
}

// 导入：认 camelCase（客户端登录态文件的形状）。
//
// ⚠ 参照项目实测客户端 `auth.json` 的字段是
// `accessToken` / `refreshToken` / `expiresAtMs`（毫秒**数字**），
// 而本插件的形状是 snake_case。用户很可能把客户端那份直接粘进来 ——
// 不认 camelCase 会让他以为"导入坏了"，而我们只是没读那个键。
func TestImportAcceptsCamelCaseClientShape(t *testing.T) {
	p := &Provider{}
	got, err := p.ImportCredentials(
		`{"accessToken":"mmoat_x","refreshToken":"mmort_y","tokenType":"Bearer","expiresAtMs":1791258329000}`)
	if err != nil {
		t.Fatalf("应认得客户端 camelCase 形状: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应导入 1 条，实际 %d", len(got))
	}
	if got[0].UID == "" {
		t.Error("uid 不能为空")
	}
}

// 导入：多个独立对象（用户一次粘 N 个文件）+ 裸 token。
func TestImportMultipleObjectsAndBareToken(t *testing.T) {
	p := &Provider{}
	got, err := p.ImportCredentials(
		"{\"access_token\":\"mmoat_a\"}\n{\"access_token\":\"mmoat_b\"}")
	if err != nil {
		t.Fatalf("多个独立对象应能导入: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应导入 2 条，实际 %d", len(got))
	}

	// 裸 token（每行一个）。
	got2, err2 := p.ImportCredentials("mmoat_c\nmmoat_d")
	if err2 != nil {
		t.Fatalf("裸 token 应能导入: %v", err2)
	}
	if len(got2) != 2 {
		t.Fatalf("裸 token 应导入 2 条，实际 %d", len(got2))
	}

	// 缺 access_token ⇒ 报错。
	if _, err3 := p.ImportCredentials(`{"no_token":true}`); err3 == nil {
		t.Error("缺 access_token 时应报错（不静默成功）")
	}
}

// AccountColumns 必须报**规范列 id**，且含 success 列。
//
// ⚠ 本仓有两条独立的守卫会检查这个（跨上游不变量测试 + 前端跳过未知 id），
// 但这里再钉一次本上游自己的：报中文标题会让**那几列静默消失**。
func TestAccountColumnsAreCanonicalAndIncludeSuccess(t *testing.T) {
	p := New(Config{})
	cols := p.AccountColumns()
	if len(cols) == 0 {
		t.Fatal("列集不能为空")
	}
	for _, c := range cols {
		switch c {
		case "provider", "nickname", "uid", "quota", "status", "token",
			"token_expiry", "checkin", "welfare", "success", "breaker",
			"in_flight", "ops":
		default:
			t.Errorf("列 id %q 不在规范词汇表里 —— 前端会**静默跳过**该列", c)
		}
	}
	joined := strings.Join(cols, ",")
	if !strings.Contains(joined, "success") {
		t.Error("必须含 success 列（用户明确要求「每一个上游都需要有成功列」）")
	}
	for _, banned := range []string{"breaker", "in_flight"} {
		if strings.Contains(joined, banned) {
			t.Errorf("不该报 %q 列 —— 用户明确要求账号池不显示熔断/在途", banned)
		}
	}
}

// 基础形状：UID 派生稳定、Usable/Refreshable/ExpiresAt 的边界。
func TestAuthBasics(t *testing.T) {
	a := &Auth{AccessToken: "mmoat_x"}
	if a.UID() == "" {
		t.Error("UID 必须能从 token 派生（真实凭据里没有可用的账号 id）")
	}
	// 同一个 token 永远得到同一个 uid（幂等）。
	if b := (&Auth{AccessToken: "mmoat_x"}).UID(); b != a.UID() {
		t.Errorf("同一个 token 的 uid 必须稳定：%q vs %q", a.UID(), b)
	}
	if !a.Usable() || a.Refreshable() {
		t.Error("有 token 就算可用；没有 refresh_token 就不可续期")
	}
	if a.Expired() {
		t.Error("没有过期信息时应保守视为**未**过期（不是已过期）")
	}
	// 显式给了过去的时刻 ⇒ 已过期。
	old := &Auth{AccessToken: "x", ExpiresAt: "1000000000001"}
	if !old.Expired() {
		t.Error("2001 年的时刻（毫秒）应判已过期")
	}
}
