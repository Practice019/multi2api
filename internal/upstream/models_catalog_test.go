package upstream

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// realCatalogBody 是 /v3/config 的真实形状（字段与实测一致，模型取真实样本里的几个）。
// 故意保留 data.agents / data.productFeatures —— 上游确实返回它们，
// 夹具少了这一段就测不出「多余字段不会让解析失败」。
const realCatalogBody = `{
  "code": 0,
  "msg": "OK",
  "requestId": "req-abc-123",
  "data": {
    "models": [
      {"id":"deepseek-v4-pro","name":"DeepSeek V4 Pro","credits":"x0.51 credits",
       "maxInputTokens":131072,"supportsToolCall":true,"supportsImages":false,
       "supportsReasoning":true,"vendor":"deepseek","tags":["cli","reasoning"]},
      {"id":"hy4-preview-f","name":"HY4 Preview F","credits":"x0.00 credits",
       "maxInputTokens":131072,"supportsToolCall":true,"supportsImages":false,
       "supportsReasoning":false,"vendor":"tencent","tags":["cli"]},
      {"id":"gpt-5.2","name":"GPT 5.2","credits":"x5.00 credits",
       "maxInputTokens":400000,"supportsToolCall":true,"supportsImages":true,
       "supportsReasoning":true,"vendor":"openai","tags":["cli","vision"]},
      {"id":"claude-4.7-sonnet","name":"Claude 4.7 Sonnet","credits":"x2.00 credits",
       "maxInputTokens":200000,"supportsToolCall":true,"supportsImages":true,
       "supportsReasoning":true,"vendor":"anthropic","tags":["cli"]}
    ],
    "agents": [{"name":"cli","models":["deepseek-v4-pro","gpt-5.2"]}],
    "productFeatures": {"foo":true}
  }
}`

// TestFetchModelCatalogHappyPath 正常路径：路径/方法/三个必需头 + 解析结果。
func TestFetchModelCatalogHappyPath(t *testing.T) {
	var gotPath, gotMethod, gotUA, gotAccept, gotAuth string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotAuth = r.Header.Get("Authorization")
		return jsonResp(200, realCatalogBody), nil
	})

	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("FetchModelCatalog: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method=%s want GET", gotMethod)
	}
	if gotPath != "/v3/config" {
		t.Errorf("path=%s want /v3/config", gotPath)
	}
	// 这三个头是实测必需：缺任一个上游直接拒。钉住它们，防止将来「顺手精简」。
	if gotAuth != "Bearer at" {
		t.Errorf("Authorization=%q want Bearer at", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept=%q want application/json", gotAccept)
	}
	if gotUA != clientUA {
		t.Errorf("User-Agent=%q want %q", gotUA, clientUA)
	}
	if len(cat.Models) != 4 {
		t.Fatalf("models=%d want 4", len(cat.Models))
	}

	m := cat.Models[0]
	if m.ID != "deepseek-v4-pro" || m.Name != "DeepSeek V4 Pro" || m.Vendor != "deepseek" {
		t.Errorf("字段解析错误: %+v", m)
	}
	if m.CreditsRaw != "x0.51 credits" {
		t.Errorf("CreditsRaw=%q want x0.51 credits", m.CreditsRaw)
	}
	if m.Multiplier != 0.51 {
		t.Errorf("Multiplier=%v want 0.51", m.Multiplier)
	}
	if m.MaxInputTokens != 131072 {
		t.Errorf("MaxInputTokens=%d want 131072", m.MaxInputTokens)
	}
	if !m.SupportsToolCall || m.SupportsImages || !m.SupportsReasoning {
		t.Errorf("能力位解析错误: %+v", m)
	}
	if len(m.Tags) != 2 || m.Tags[1] != "reasoning" {
		t.Errorf("tags 解析错误: %+v", m.Tags)
	}
	// 多余的 agents / productFeatures 不能把解析搞崩，也不能混进 Models。
	for _, x := range cat.Models {
		if x.ID == "" || x.ID == "cli" {
			t.Errorf("混入了非模型条目: %+v", x)
		}
	}
}

// TestFetchModelCatalogAllMultipliers 四个真实系数样本都要解析对，
// 尤其是带 0 的那个（"x0.00 credits" 必须得到 0，而不是「没解析」的 0 —— 两者在本任务里等价，
// 但 Raw 串必须保留原样，排查时才分得清）。
func TestFetchModelCatalogAllMultipliers(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, realCatalogBody), nil
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"deepseek-v4-pro":   0.51,
		"hy4-preview-f":     0.00,
		"gpt-5.2":           5.00,
		"claude-4.7-sonnet": 2.00,
	}
	got := cat.MultiplierTable()
	for id, w := range want {
		if w == 0 {
			continue // 0 系数不进表，下面单独断言
		}
		if got[id] != w {
			t.Errorf("%s multiplier=%v want %v", id, got[id], w)
		}
	}
	if _, ok := got["hy4-preview-f"]; ok {
		t.Error("0 系数不应进入 MultiplierTable")
	}
	// 单查接口也要能区分「不存在」和「存在但 0 系数」。
	if _, ok := cat.Multiplier("hy4-preview-f"); ok {
		t.Error("0 系数模型 Multiplier 应返回 ok=false")
	}
	if _, ok := cat.Multiplier("no-such-model"); ok {
		t.Error("不存在的模型应返回 ok=false")
	}
	if v, ok := cat.Multiplier("gpt-5.2"); !ok || v != 5.0 {
		t.Errorf("gpt-5.2 = (%v,%v) want (5,true)", v, ok)
	}
}

// TestFetchModelCatalogSort 排序：系数降序，同系数按 id 升序（结果必须可复现）。
func TestFetchModelCatalogSort(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, realCatalogBody), nil
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	cat.SortByMultiplierDesc()
	wantOrder := []string{"gpt-5.2", "claude-4.7-sonnet", "deepseek-v4-pro", "hy4-preview-f"}
	for i, w := range wantOrder {
		if cat.Models[i].ID != w {
			t.Errorf("排序第 %d 位 = %s want %s", i, cat.Models[i].ID, w)
		}
	}
	// 同系数时按 id 升序：再排一次必须完全不变（稳定）。
	cat.SortByMultiplierDesc()
	if cat.Models[0].ID != "gpt-5.2" {
		t.Error("重复排序结果不稳定")
	}
}

// TestParseCreditsMultiplier 解析器的边界表。这是本任务最容易出错的地方。
func TestParseCreditsMultiplier(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		// —— 真实样本 ——
		{"x0.00 credits", 0},
		{"x0.51 credits", 0.51},
		{"x2.00 credits", 2},
		{"x5.00 credits", 5},

		// —— 空白 ——
		{"  x0.51 credits  ", 0.51},
		{"x0.51   credits", 0.51},
		{"\tx1.5 credits\n", 1.5},

		// —— 大小写 ——
		{"X0.51 CREDITS", 0.51},
		{"x1.5 Credits", 1.5},

		// —— 后缀缺失（宽容）——
		{"x0.51", 0.51},
		{"x12", 12},

		// —— 空 / 垃圾 ——
		{"", 0},
		{"   ", 0},
		{"x", 0},
		{"X", 0},
		{"credits", 0},
		{"0.51 credits", 0},   // 缺 x 前缀
		{"0.51", 0},           // 缺 x 前缀
		{"credits x0.51", 0},  // 顺序颠倒
		{"xx0.51 credits", 0}, // 双 x
		{"x0.51 credits extra", 0},
		{"xabc credits", 0},
		{"x0.5.1 credits", 0},
		{"x∞ credits", 0},

		// —— 负数：格式合法但语义无效，一律归零 ——
		{"x-0.51 credits", 0},
		{"x-5", 0},

		// —— 极大值：不溢出、不变 Inf ——
		{"x1e308 credits", 1e308},
		{"x1e309 credits", 0}, // ParseFloat 溢出 → err → 0
		// 20 个 9 ≈ 1e20：**在 float64 量程内**，ParseFloat 正常解析，不是溢出。
		// 先前这里写成 want 0 是测试自己算错了 —— 真溢出需要 ≥1e309（见上一行）。
		{"x99999999999999999999 credits", 1e20},
	}
	for _, c := range cases {
		got := ParseCreditsMultiplier(c.in)
		if got != c.want {
			t.Errorf("ParseCreditsMultiplier(%q)=%v want %v", c.in, got, c.want)
		}
		// 铁律：任何输入都不许产出 NaN/Inf（math.IsInf 判正负两个方向）。
		if got != got || math.IsInf(got, 0) {
			t.Errorf("ParseCreditsMultiplier(%q) 产出非法值: %v", c.in, got)
		}
	}
}

// TestParseCreditsMultiplierNeverNaNAndNoPanic 穷举一批病态输入，
// 只断言「不 panic 且结果是有限非负数」——解析错误一律退化成 0。
func TestParseCreditsMultiplierNeverNaNAndNoPanic(t *testing.T) {
	inputs := []string{
		"nan", "xnan credits", "xNaN", "xinf", "xInf credits", "x+Inf",
		"x-Inf", "x1e999", "x.--", "x..", "x  ", "\x00", "x0,51 credits",
		"ｘ0.51 credits", // 全角 x：不是 ASCII 'x'，应归零
		"x０.５１ credits", // 全角数字
		strings.Repeat("x", 1000),
		strings.Repeat("9", 5000) + " credits",
	}
	for _, in := range inputs {
		got := ParseCreditsMultiplier(in)
		if got < 0 {
			t.Errorf("ParseCreditsMultiplier(%q)=%v 负数不合法", in, got)
		}
		if got != got { // NaN
			t.Errorf("ParseCreditsMultiplier(%q)=NaN", in)
		}
		if math.IsInf(got, 0) {
			t.Errorf("ParseCreditsMultiplier(%q)=Inf", in)
		}
	}
}

// TestParseCreditsMultiplierFullwidthX 明确钉住全角 x 的行为（归零，不是 0.51）。
func TestParseCreditsMultiplierFullwidthX(t *testing.T) {
	if got := ParseCreditsMultiplier("ｘ0.51 credits"); got != 0 {
		t.Errorf("全角 x 应归零，得到 %v", got)
	}
}

// TestFetchModelCatalogBusinessError code != 0 → *Error，且不能返回半个目录。
func TestFetchModelCatalogBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10001,"msg":"invalid params","requestId":"r1","data":null}`), nil
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("code != 0 应报错")
	}
	if cat != nil {
		t.Errorf("出错时不应返回目录: %+v", cat)
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Status != 200 {
		t.Errorf("status=%d want 200（HTTP 层是成功的）", ue.Status)
	}
	if !strings.Contains(ue.Msg, "10001") {
		t.Errorf("Msg 应含业务码: %q", ue.Msg)
	}
}

// TestFetchModelCatalogHTTP500 HTTP 500 → *Error，Kind=ErrServer。
func TestFetchModelCatalogHTTP500(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `{"code":500,"msg":"internal error"}`), nil
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("HTTP 500 应报错")
	}
	if cat != nil {
		t.Errorf("出错时不应返回目录: %+v", cat)
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Status != 500 || ue.Kind != ErrServer {
		t.Errorf("got status=%d kind=%v want 500/%v", ue.Status, ue.Kind, ErrServer)
	}
}

// TestFetchModelCatalogTransportError 网络层失败（连接被拒/超时）应是普通 error，
// 由调用方忽略；绝不能 panic，也不能伪装成空目录。
func TestFetchModelCatalogTransportError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("传输层失败应报错")
	}
	if cat != nil {
		t.Errorf("出错时不应返回目录: %+v", cat)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("错误应透传原因: %v", err)
	}
}

// TestFetchModelCatalogTimeout 真·超时：用 httptest 挂住响应，客户端 50ms 超时，
// 断言返回错误且没有 panic / 没有返回目录。
func TestFetchModelCatalogTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // 永不响应，直到测试结束
	}))
	defer srv.Close()
	defer close(block)

	c := &Client{
		HTTP:       &http.Client{Timeout: 50 * time.Millisecond},
		ChatBaseCN: srv.URL,
	}
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("超时应报错")
	}
	if cat != nil {
		t.Errorf("超时时不应返回目录: %+v", cat)
	}
}

// TestFetchModelCatalogViaHTTPServer 用真实 httptest server（而非 RoundTripper 桩）
// 走一遍完整的 HTTP 栈，确认路径拼接与 headers 在真实连接上也成立。
func TestFetchModelCatalogViaHTTPServer(t *testing.T) {
	var seenPath, seenUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realCatalogBody))
	}))
	defer srv.Close()

	c := &Client{HTTP: &http.Client{Timeout: 5 * time.Second}, ChatBaseCN: srv.URL}
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModelCatalog: %v", err)
	}
	if seenPath != "/v3/config" {
		t.Errorf("server 收到 path=%s want /v3/config", seenPath)
	}
	if seenUA != clientUA {
		t.Errorf("server 收到 UA=%q want %q", seenUA, clientUA)
	}
	if len(cat.Models) != 4 {
		t.Fatalf("models=%d want 4", len(cat.Models))
	}
}

// TestFetchModelCatalogEmptyModels data.models 为空 → 返回空目录而不是错误，
// 也不 panic；MultiplierTable 返回 nil。
func TestFetchModelCatalogEmptyModels(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"models":[],"agents":[]}}`), nil
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("空列表不应报错: %v", err)
	}
	if cat == nil || len(cat.Models) != 0 {
		t.Fatalf("want 空目录, got %+v", cat)
	}
	if tbl := cat.MultiplierTable(); tbl != nil {
		t.Errorf("空目录的 MultiplierTable 应为 nil, got %v", tbl)
	}
	cat.SortByMultiplierDesc() // 不能 panic
}

// TestFetchModelCatalogNilData data 整个缺失（上游只回 code/msg）时应得到空目录，
// 而不是解析错误 —— 与 GrowthClaim 的「缺 data 按无事发生」保持同一套语义。
func TestFetchModelCatalogNilData(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK"}`), nil
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("缺 data 不应报错: %v", err)
	}
	if cat == nil || len(cat.Models) != 0 {
		t.Fatalf("want 空目录, got %+v", cat)
	}
}

// TestFetchModelCatalogMalformedCredits 上游给出坏 credits 时，
// 目录本身仍要成功返回，坏条目退化为 Multiplier=0（保住其它模型的信息）。
func TestFetchModelCatalogMalformedCredits(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"good","name":"Good","credits":"x3.00 credits","maxInputTokens":1},
			{"id":"bad1","name":"Bad1","credits":"","maxInputTokens":1},
			{"id":"bad2","name":"Bad2","credits":"3.00 credits","maxInputTokens":1},
			{"id":"bad3","name":"Bad3","credits":"x-1.00 credits","maxInputTokens":1}
		]}}`), nil
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("坏 credits 不应让整个目录失败: %v", err)
	}
	if len(cat.Models) != 4 {
		t.Fatalf("models=%d want 4", len(cat.Models))
	}
	tbl := cat.MultiplierTable()
	if tbl["good"] != 3.0 {
		t.Errorf("good=%v want 3", tbl["good"])
	}
	for _, id := range []string{"bad1", "bad2", "bad3"} {
		if _, ok := tbl[id]; ok {
			t.Errorf("%s 坏系数不应进表", id)
		}
	}
	// 原样字符串必须保留，排查用。
	for _, m := range cat.Models {
		if m.ID == "bad2" && m.CreditsRaw != "3.00 credits" {
			t.Errorf("CreditsRaw 应原样保留, got %q", m.CreditsRaw)
		}
	}
}

// TestFetchModelCatalogNotJSON 响应不是 JSON 时返回解析错误，不 panic。
func TestFetchModelCatalogNotJSON(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `<html>gateway timeout</html>`), nil
	})
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("非 JSON 响应应报错")
	}
	if cat != nil {
		t.Errorf("出错时不应返回目录: %+v", cat)
	}
}

// TestModelCatalogNilReceivers 方法要能吃 nil 接收者（调用方写
// `cat, _ := Fetch(...); tbl := cat.MultiplierTable()` 而不判空是常态）。
func TestModelCatalogNilReceivers(t *testing.T) {
	var cat *ModelCatalog
	if tbl := cat.MultiplierTable(); tbl != nil {
		t.Errorf("nil 接收者应返回 nil 表, got %v", tbl)
	}
	if _, ok := cat.Multiplier("x"); ok {
		t.Error("nil 接收者应返回 ok=false")
	}
	cat.SortByMultiplierDesc() // 不能 panic
}

// TestFetchModelCatalogDoesNotBreakMainFlow 约束 3 的可测形态：
// 即使 Fetch 全失败，调用方拿到 (nil, err) 后照样能继续 —— 这里模拟一次
// 「失败后回退到静态认知」的调用路径，断言没有任何 panic 且零值可用。
func TestFetchModelCatalogDoesNotBreakMainFlow(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("boom")
	})
	var multiplier float64 = -1
	cat, err := c.FetchModelCatalog(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("want error")
	}
	if cat != nil {
		t.Fatal("want nil catalog")
	}
	if v, ok := cat.Multiplier("deepseek-v4-pro"); ok {
		multiplier = v
	} else {
		multiplier = 0 // 降级：没有系数信息
	}
	if multiplier != 0 {
		t.Errorf("降级路径得到 %v want 0", multiplier)
	}
}

// ---------------------------------------------------------------------------
// 评审（Reviewer agent）发现的三处缺陷的回归测试。
// 每条都先在评审报告里被实测指出，这里钉死修复后的契约。
// ---------------------------------------------------------------------------

// 负零必须归 0，不能返回 -0。
//
// 评审实测："x-0" → -0 且 math.Signbit(-0)=true。
// 成因：IEEE-754 规定 -0.0 < 0 为 false，所以只判 `v < 0` 会让负零漏过。
// 影响：-0 在任何 `> 0` 判断下都等价于 0，但它会以 `-0` 形式写进 JSON，
// 且 math.Signbit 为真 —— 看着像 bug，也容易让下游的符号判断出错。
func TestParseCreditsMultiplierNegativeZero(t *testing.T) {
	for _, in := range []string{"x-0", "x-0.0", "x-0.00 credits", "x-0e5"} {
		got := ParseCreditsMultiplier(in)
		if got != 0 {
			t.Errorf("%q = %v want 0", in, got)
		}
		if math.Signbit(got) {
			t.Errorf("%q 返回了负零（Signbit=true）", in)
		}
	}
}

// MultiplierTable 的空结果契约必须统一：一律 nil。
//
// 评审实测：空切片目录 → nil；有模型但系数全 0 → **非 nil 空 map**。
// 两个同样"空"的结果对 `if tbl == nil` 含义不同，调用方容易写出
// 只在其中一种情况下正确的分支。现在按**结果**判空。
func TestMultiplierTableEmptyContractIsNil(t *testing.T) {
	cases := map[string]*ModelCatalog{
		"nil 接收者":    nil,
		"空切片":        {Models: []ModelCatalogEntry{}},
		"系数全为 0":     {Models: []ModelCatalogEntry{{ID: "a"}, {ID: "b"}}},
		"id 为空":      {Models: []ModelCatalogEntry{{ID: "", Multiplier: 3}}},
		"系数为负（异常输入）": {Models: []ModelCatalogEntry{{ID: "a", Multiplier: -1}}},
	}
	for name, mc := range cases {
		if tbl := mc.MultiplierTable(); tbl != nil {
			t.Errorf("%s: want nil, got %v（空结果应统一为 nil）", name, tbl)
		}
	}
	// 有有效条目时必须非 nil 且内容正确
	mc := &ModelCatalog{Models: []ModelCatalogEntry{{ID: "a", Multiplier: 2}, {ID: "b"}}}
	tbl := mc.MultiplierTable()
	if tbl == nil || len(tbl) != 1 || tbl["a"] != 2 {
		t.Errorf("非空结果错误: %v", tbl)
	}
}

// NaN 不得让排序产生不确定顺序。
//
// 评审实测：`less` 在 NaN 下不是严格弱序 —— 同时满足 less(a,b) 与 less(b,a)，
// 排序结果不确定。正常路径构造不出 NaN（解析器拒绝它），
// 但 Multiplier 是导出字段，兜底后 NaN 视为最小、且仍按 id 定序。
func TestSortByMultiplierDescNaNDeterministic(t *testing.T) {
	build := func() *ModelCatalog {
		return &ModelCatalog{Models: []ModelCatalogEntry{
			{ID: "a", Multiplier: math.NaN()},
			{ID: "b", Multiplier: 1},
			{ID: "c", Multiplier: math.NaN()},
			{ID: "d", Multiplier: 2},
		}}
	}
	first := build()
	first.SortByMultiplierDesc()
	// 同一输入多次排序必须得到同一顺序
	for i := 0; i < 20; i++ {
		again := build()
		again.SortByMultiplierDesc()
		for j := range first.Models {
			if again.Models[j].ID != first.Models[j].ID {
				t.Fatalf("排序不确定：第 %d 次得到 %v，首次为 %v",
					i, ids(again), ids(first))
			}
		}
	}
	// 有限值必须仍按降序排在 NaN 前面
	got := ids(first)
	if got[0] != "d" || got[1] != "b" {
		t.Errorf("有限值应排前且降序: %v", got)
	}
	// NaN 项按 id 升序
	if got[2] != "a" || got[3] != "c" {
		t.Errorf("NaN 项应按 id 升序: %v", got)
	}
}

func ids(mc *ModelCatalog) []string {
	out := make([]string, 0, len(mc.Models))
	for _, m := range mc.Models {
		out = append(out, m.ID)
	}
	return out
}

// 评审 L1：解析器接受了若干未在文档中声明的形态。
// 这里把它们**明确钉成支持**（而不是"意外通过"），并补上文档。
func TestParseCreditsMultiplierAcceptedForms(t *testing.T) {
	cases := map[string]float64{
		"x0.51 credits":   0.51,
		"x0.51":           0.51,
		"X0.51 CREDITS":   0.51, // 大小写不敏感
		"  x0.51 credits": 0.51, // 前后空白
		"x 0.51":          0.51, // 名字与数字之间有空白（评审指出未声明）
		"x.5":             0.5,
		"x5.":             5,
	}
	for in, want := range cases {
		if got := ParseCreditsMultiplier(in); got != want {
			t.Errorf("%q = %v want %v", in, got, want)
		}
	}
}
