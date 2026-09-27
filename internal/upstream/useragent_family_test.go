package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/wire"
)

// newHdrReqForUA 造一个用于 UA 断言的请求。
func newHdrReqForUA(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestUAModelFamiliesSelectsByModel 按模型族选 UA（参照 product.ts 的协议事实）。
//
// # 为什么需要它
//
// 国际版与国内版**共用同一后端协议，但模型池分属不同产品线**，
// 而腾讯后台按出站 UA 归因「使用端」—— 只用一个全局 UA 会让其中一类
// 模型的账单显示成 `-`。
//
// 参照 intl 表（逐字）：
//
//	gpt- / gemini- / claude-     → WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2
//	glm- / hy / kimi- / minimax- → WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2
//
// ⚠ 变异可检：把 UAModelFamilies 置空 → 本用例红（两个模型得到同一个 UA）。
func TestUAModelFamiliesSelectsByModel(t *testing.T) {
	const intlUA = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	const cnUA = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"

	c := New()
	c.ClientVersion = "5.5.2"
	c.CliVersion = "5.5.2"
	c.UAModelFamilies = []wire.UAModelFamilyRule{
		{Match: "gpt-", UA: intlUA},
		{Match: "gemini-", UA: intlUA},
		{Match: "claude-", UA: intlUA},
		{Match: "glm-", UA: cnUA},
		{Match: "hy", UA: cnUA},
		{Match: "kimi-", UA: cnUA},
		{Match: "minimax-", UA: cnUA},
	}

	for _, tc := range []struct {
		model string
		want  string
	}{
		{"gpt-6-astra", intlUA},
		{"claude-4.5", intlUA},
		{"glm-5.2", cnUA},
		{"hy3", cnUA},
		{"kimi-k3", cnUA},
		{"minimax-m3", cnUA},
		// ⚠ 无命中时用**默认档**。这里刻意用显式配的 DefaultUA 而不是
		// `defaultWorkBuddyUA()` —— 见 TestDefaultUADiffersFromReferenceIntl 的说明：
		// 我们的默认三段式是 CN 形态（改造前的行为），而参照的默认档是 INTL 形态。
		// 两者不同是**已知且刻意**的（不能为了对齐参照而改默认，
		// 那会破坏"未配置部署 UA 逐字节不变"这条硬约束）。
		{"unknown-model", "DEFAULT-UA"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			req := newHdrReqForUA(t)
			c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "", tc.model)
			want := tc.want
			if want == "DEFAULT-UA" {
				want = c.defaultWorkBuddyUA()
			}
			if got := req.Header.Get("User-Agent"); got != want {
				t.Errorf("model=%q UA=%q\nwant %q", tc.model, got, want)
			}
		})
	}
}

// TestDefaultUADiffersFromReferenceIntl **如实记录**一处已知差异。
//
// # 差异是什么
//
//	参照 intl 的默认档（product.userAgent）  = WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2
//	我们的 defaultWorkBuddyUA()             = WorkBuddy/5.5.2 WorkBuddy/5.5.2    CLI/5.5.2
//	                                                    ^^^^^^^^^^^ 差在这里
//
// # 为什么不改
//
// 我们的三段式默认值是**改造前就在生产跑的形态**，而"未配置的部署
// 升级后 UA 必须逐字节不变"是一条硬约束（headers.go 的三级优先注释、
// TestEmptyUAModelFamiliesIsByteIdenticalToBefore 都在守它）。
//
// 改默认会让**所有既有部署**的出站 UA 突然变化 —— 上游侧归因、风控、
// 限流都可能受影响，而那是用户没要求、也无法预期的变更。
//
// 正确的做法是**把差异显式化**：想对齐参照的部署在配置里写明
// `UAModelFamilies`（含一条兜底的 `{Match:"", UA: INTL形态}` 或直接配
// `UserAgent`），而不是我们替所有人改默认。
//
// 本测试的作用是**让这个差异可见**：将来若有人"顺手把默认改成参照的值"，
// 它会红，并指向这里解释为什么不能改。
func TestDefaultUADiffersFromReferenceIntl(t *testing.T) {
	c := New()
	c.ClientVersion = "5.5.2"
	c.CliVersion = "5.5.2"

	ours := c.defaultWorkBuddyUA()
	referenceIntl := "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"

	if ours == referenceIntl {
		t.Error("我们的默认三段式与参照 intl 默认档相同了 —— " +
			"若这是有意的改动，请一并更新本注释与 headers.go 的三级优先说明，" +
			"并确认既有部署的 UA 变化已被评估（那会牵动上游归因/风控）")
	}
	// 我们的默认必须是 CN 形态（第一段与第三段与参照一致，品牌段无 AI）。
	if !strings.Contains(ours, "WorkBuddy/5.5.2 WorkBuddy/5.5.2") {
		t.Errorf("默认三段式 = %q，期望 CN 形态（品牌段无 `AI`）", ours)
	}
}

// TestEmptyUAModelFamiliesIsByteIdenticalToBefore 未配置分档表时 UA **逐字节不变**。
//
// # 这是本次改动最重要的约束
//
// 分档是**新增能力**，默认必须关闭 —— 既有部署升级后发出的 UA
// 不能有任何变化（否则上游侧归因、风控、限流都可能受影响）。
//
// 判据：空表 + 非空 ClientVersion 时，UA 必须等于三段式默认值
//（defaultWorkBuddyUA），与加这个特性之前**完全相同**。
func TestEmptyUAModelFamiliesIsByteIdenticalToBefore(t *testing.T) {
	c := New()
	c.ClientVersion = "5.5.2"
	c.CliVersion = "5.5.2"
	// UAModelFamilies 留空

	want := c.defaultWorkBuddyUA()
	for _, model := range []string{"", "gpt-6", "glm-5.2", "whatever"} {
		req := newHdrReqForUA(t)
		c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "", model)
		if got := req.Header.Get("User-Agent"); got != want {
			t.Errorf("model=%q 空分档表下 UA=%q\nwant %q（必须与改造前逐字节相同）",
				model, got, want)
		}
	}
}

// TestUserAgentVerbatimOverrideBeatsFamilies UserAgent 逐字覆盖**压过分档**。
//
// 第 ① 级是"配置完全接管"，分档是第 ② 级的细化 —— 分档不该越过它。
// 若越过，用户显式配的 user_agent 会在某些模型上静默失效。
func TestUserAgentVerbatimOverrideBeatsFamilies(t *testing.T) {
	c := New()
	c.UserAgent = "MyAgent/9.9"
	c.ClientVersion = "5.5.2"
	c.UAModelFamilies = []wire.UAModelFamilyRule{{Match: "glm-", UA: "CN-UA"}}

	req := newHdrReqForUA(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "", "glm-5.2")
	if got := req.Header.Get("User-Agent"); got != "MyAgent/9.9" {
		t.Errorf("UA=%q，want MyAgent/9.9 —— "+
			"user_agent 是最高优先级的逐字覆盖，分档不得越过它", got)
	}
}

// TestNoClientVersionKeepsCLIForm 未配 client_version 时仍是 CLI 形态（第三级）。
//
// 三级优先的第 ③ 条是"改造前的行为"：未配置的部署必须发出与升级前
// 完全相同的 UA。分档只在第 ② 级生效，不该影响这一级。
func TestNoClientVersionKeepsCLIForm(t *testing.T) {
	c := New()
	// ClientVersion 与 UserAgent 都留空
	c.UAModelFamilies = []wire.UAModelFamilyRule{{Match: "glm-", UA: "CN-UA"}}

	req := newHdrReqForUA(t)
	c.ChatHeaders(req, &auth.Auth{AccessToken: "t", UID: "u"}, "", "glm-5.2")
	if got := req.Header.Get("User-Agent"); got != clientUA {
		t.Errorf("UA=%q，want %q —— "+
			"未配 client_version 时应保持 CLI 形态（改造前行为），分档不影响这一级",
			got, clientUA)
	}
}

// TestChatStreamWithIPPassesModelToUA 真实 chat 路径要把 body 里的 model 传进 UA 选择。
//
// # 为什么这条必须走真路径
//
// 上面几条直接调 ChatHeaders（手工传 model）。但生产路径是
// `ChatStreamWithIP(a, body, ip)` —— 它得**自己从 body 里取出 model**
// 再传给 ChatHeaders。若那里漏传（或传了空串），分档在生产里**永不生效**，
// 而上面几条测试全都照绿。
//
// 这正是"单元测试全绿但功能没接上"的典型形态，所以这里打真路径。
func TestChatStreamWithIPPassesModelToUA(t *testing.T) {
	const cnUA = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"
	const intlUA = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"

	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	newClient := func() *Client {
		c := New()
		c.ChatBaseCN = srv.URL
		c.ClientVersion = "5.5.2"
		c.CliVersion = "5.5.2"
		c.UAModelFamilies = []wire.UAModelFamilyRule{
			{Match: "glm-", UA: cnUA},
			{Match: "gpt-", UA: intlUA},
		}
		return c
	}

	// 国内系模型 → 国内形态
	c := newClient()
	rc, _, _, err := c.ChatStreamWithIP(
		&auth.Auth{AccessToken: "t", UID: "u", Channel: auth.ChannelCN},
		[]byte(`{"model":"glm-5.2","messages":[]}`), "")
	if err != nil {
		t.Fatalf("ChatStreamWithIP: %v", err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	if gotUA != cnUA {
		t.Errorf("glm-5.2 的 UA = %q\nwant %q —— "+
			"说明 ChatStreamWithIP 没把 body 里的 model 传给 UA 选择（分档在生产里永不生效）",
			gotUA, cnUA)
	}

	// 国际系模型 → 国际形态
	c2 := newClient()
	rc2, _, _, err := c2.ChatStreamWithIP(
		&auth.Auth{AccessToken: "t", UID: "u", Channel: auth.ChannelCN},
		[]byte(`{"model":"gpt-6-astra","messages":[]}`), "")
	if err != nil {
		t.Fatalf("ChatStreamWithIP: %v", err)
	}
	if rc2 != nil {
		_ = rc2.Close()
	}
	if gotUA != intlUA {
		t.Errorf("gpt-6-astra 的 UA = %q\nwant %q", gotUA, intlUA)
	}
}
