package loomy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// ── 凭证解析与加载 ──────────────────────────────────────────────────────

// TestParseClientShapedCredential 手工抄的登录态必须能被直接读入。
//
// 这是**主路径**：手册第 5 节教用户从 Local Storage 里抄 session，
// 抄出来的就是客户端那份 JSON（含 phone/maskedPhone/session/userid/loggedInAt）。
// 如果只认网关自己写盘的形态，用户就得先把 JSON 改造成我们的格式才能用 ——
// 那正是"手工能建、程序建的不认"那种别扭的不对称。
func TestParseClientShapedCredential(t *testing.T) {
	// 手册第 2.2 节的原样实录
	raw := []byte(`{
      "phone": "138****0000",
      "maskedPhone": "138****0000",
      "session": "0123456789abcdef0123456789abcdef",
      "userid": "100000000000000001",
      "loggedInAt": "2026-09-11T09:35:22.593Z"
    }`)
	a, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("解析客户端形态失败: %v", err)
	}
	if a.Session != "0123456789abcdef0123456789abcdef" {
		t.Errorf("session = %q", a.Session)
	}
	// UID 必须取 userid（而不是派生值）—— 否则"先派生后拿到 userid"会让
	// 同一个号在池子里变成两个账号。
	if a.UID != "100000000000000001" {
		t.Errorf("uid = %q，期望取 userid", a.UID)
	}
	if a.Nickname != "138****0000" {
		t.Errorf("nickname = %q，期望取掩码手机号", a.Nickname)
	}
	if a.LoggedInAt != "2026-09-11T09:35:22.593Z" {
		t.Errorf("loggedInAt = %q", a.LoggedInAt)
	}
}

// TestParseCredentialUIDFallbacks 钉住 UID 的取值优先级。
//
// 顺序错乱的后果不是报错，而是**池子里出现幽灵账号**（同一份凭证两个 uid），
// 那种问题只在多账号部署下才显形，且表现为"并发上限被虚增"这种间接症状。
func TestParseCredentialUIDFallbacks(t *testing.T) {
	t.Run("userid 优先于文件里已有的 uid", func(t *testing.T) {
		a, err := ParseCredential([]byte(`{"session":"` + fixtureSession + `","userid":"U1","uid":"STALE"}`))
		if err != nil {
			t.Fatal(err)
		}
		if a.UID != "U1" {
			t.Errorf("uid = %q，期望 U1（userid 应胜过文件里已有的投影字段）", a.UID)
		}
	})
	t.Run("没有 userid 时用文件里的 uid", func(t *testing.T) {
		a, err := ParseCredential([]byte(`{"session":"` + fixtureSession + `","uid":"KEEP"}`))
		if err != nil {
			t.Fatal(err)
		}
		if a.UID != "KEEP" {
			t.Errorf("uid = %q，期望 KEEP", a.UID)
		}
	})
	t.Run("两者都没有则按 session 派生，且确定性", func(t *testing.T) {
		raw := []byte(`{"session":"` + fixtureSession + `"}`)
		a1, err := ParseCredential(raw)
		if err != nil {
			t.Fatal(err)
		}
		a2, _ := ParseCredential(raw)
		if a1.UID == "" {
			t.Fatal("派生 uid 为空 —— 每次启动都会多一个账号")
		}
		if a1.UID != a2.UID {
			t.Errorf("派生 uid 不稳定: %q vs %q", a1.UID, a2.UID)
		}
		if a1.UID == fixtureSession {
			t.Error("派生 uid 不该直接把 session 当标识（那会让凭证出现在日志/界面里）")
		}
	})
	t.Run("缺少 session 必须报错", func(t *testing.T) {
		if _, err := ParseCredential([]byte(`{"userid":"U1"}`)); err == nil {
			t.Error("没有 session 的凭证应当报错（它是唯一鉴权材料）")
		}
	})
	t.Run("非法 JSON 报错而不是 panic", func(t *testing.T) {
		if _, err := ParseCredential([]byte(`{not json`)); err == nil {
			t.Error("非法 JSON 应当报错")
		}
	})
}

// TestLooksLikeSessionIsAdvisoryNotBlocking 钉住"非 32 位 hex 只告警不拒绝"。
//
// # 为什么不 fail fast（这条刻意反直觉，所以要有测试说明）
//
// 上游随时可能改 session 的形态。硬校验的后果是一个**仍然可用**的凭证
// 被网关自己拒之门外，而用户看到的是"启动就报错"——那种错误最难归因：
// 明明是上游格式变了，却像用户抄错了。所以只在加载时告警。
func TestLooksLikeSessionIsAdvisoryNotBlocking(t *testing.T) {
	cases := []struct {
		name    string
		session string
		want    bool
	}{
		{"手册实录的 32 位小写 hex", fixtureSession, true},
		{"大写 hex（形态可能变）", strings.ToUpper(fixtureSession), false},
		{"长度不足", "abc123", false},
		{"带前缀", "Bearer-" + fixtureSession, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeSession(c.session); got != c.want {
				t.Errorf("looksLikeSession(%q) = %v，期望 %v", c.session, got, c.want)
			}
			// 无论形态如何，都必须能被**接受**（只是打一行告警）
			if _, err := ParseCredential([]byte(`{"session":"` + c.session + `"}`)); err != nil {
				t.Errorf("形态可疑的 session 不该被拒绝: %v", err)
			}
		})
	}
}

// TestLoadDir 钉住目录加载的语义（空目录非错误、坏文件跳过、同 uid 取较新）。
func TestLoadDir(t *testing.T) {
	t.Run("空目录不是错误", func(t *testing.T) {
		got, err := LoadDir(t.TempDir())
		if err != nil {
			t.Fatalf("空目录不该报错: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("空目录应当返回空，得到 %d 条", len(got))
		}
	})
	t.Run("空 dir 参数直接返回（不做相对 CWD 的 Glob）", func(t *testing.T) {
		// 这条防的是 codearts 那边踩过的坑：Glob("loomy*.json") 会相对
		// **进程 CWD** 匹配，把当前目录下任意凭证文件读进来。
		got, err := LoadDir("")
		if err != nil {
			t.Fatalf("空 dir 不该报错: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("空 dir 应当返回空，得到 %d 条", len(got))
		}
	})
	t.Run("坏文件跳过、好文件保留", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "loomy-good.json", `{"session":"`+fixtureSession+`","userid":"GOOD"}`)
		writeFile(t, dir, "loomy-bad.json", `{not json`)
		writeFile(t, dir, "loomy-nosession.json", `{"userid":"X"}`)
		// 非 loomy 前缀的文件必须**不被**读到（按上游分目录的判据）
		writeFile(t, dir, "codearts-other.json", `{"session":"`+fixtureSession+`","userid":"OTHER"}`)

		got, err := LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].UID != "GOOD" {
			t.Fatalf("期望只有 GOOD 一条，实际 %d 条: %+v", len(got), uidsOf(got))
		}
	})
	t.Run("同一 uid 多份取 LoggedInAt 较新者", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "loomy-old.json",
			`{"session":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","uid":"SAME","loggedInAt":"2026-09-11T09:35:22.593Z"}`)
		writeFile(t, dir, "loomy-new.json",
			`{"session":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","uid":"SAME","loggedInAt":"2026-09-14T08:15:00.000Z"}`)
		got, err := LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("同一 uid 应当只保留一份，得到 %d 份", len(got))
		}
		if !strings.HasPrefix(got[0].Session, "bbbb") {
			t.Errorf("应当保留较新的那份，实际 session=%q", got[0].Session)
		}
	})
}

// TestMarshalAuthFileRoundTrip 写盘形态必须能被自己读回且字段不丢。
func TestMarshalAuthFileRoundTrip(t *testing.T) {
	src := &Auth{
		Session:    fixtureSession,
		UID:        "U1",
		Nickname:   "138****0000",
		Phone:      "138****0000",
		UserID:     "U1",
		LoggedInAt: "2026-09-11T09:35:22.593Z",
	}
	raw, err := MarshalAuthFile(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.Session != src.Session || got.UID != src.UID || got.Nickname != src.Nickname ||
		got.Phone != src.Phone || got.LoggedInAt != src.LoggedInAt {
		t.Errorf("往返后字段不一致:\n 写 %+v\n 读 %+v", src, got)
	}
	if !strings.HasSuffix(string(raw), "\n") {
		t.Error("落盘文件应当以换行结尾（人会打开看，diff 也友好）")
	}
}

// TestFileNameSanitized 文件名不得让 uid 里的非法字符逃到路径里。
func TestFileNameSanitized(t *testing.T) {
	// 路径分隔符绝不能出现在文件名里（否则会写到别的目录）
	if got := FileName(&Auth{UID: "a/b:c"}); strings.ContainsAny(got, `/\:`) {
		t.Errorf("FileName 未清洗路径分隔符: %q", got)
	}
	if got := FileName(&Auth{UID: "U1"}); got != "loomy-U1.json" {
		t.Errorf("FileName = %q，期望 loomy-U1.json", got)
	}
	if got := FileName(&Auth{UID: ""}); !strings.Contains(got, "unknown") {
		t.Errorf("空 uid 应当退化为一个稳定的占位名，得到 %q", got)
	}
}

// ── 模型目录 ────────────────────────────────────────────────────────────

// TestCatalogShape 钉住目录的几个关键事实（来自手册第 4.1 节的实测表）。
func TestCatalogShape(t *testing.T) {
	all := KnownModels()
	if len(all) != 12 {
		t.Errorf("手册实录 12 个模型，表里有 %d 个", len(all))
	}
	avail := AvailableModels()
	if len(avail) != 10 {
		t.Errorf("实测可用 10 个（2 个生图模型 404），可用表里有 %d 个", len(avail))
	}
	for _, m := range avail {
		if m.ID == "" {
			t.Error("可用表里出现了空 ID")
		}
		if m.ContextWindow == 0 {
			t.Errorf("%s 缺上下文窗口（实时目录缺失时会用它回填）", m.ID)
		}
	}
	// 推荐模型与最大输出上限（手册第 4.1 节表格里最大的两个）
	if m, _, ok := ResolveModel("MiniMax-M3"); !ok || m.MaxOutputTokens != 512000 {
		t.Errorf("MiniMax-M3 的输出上限应为 512000")
	}
	if m, _, ok := ResolveModel("deepseek-v4-flash-0731"); !ok || m.MaxOutputTokens != 384000 {
		t.Errorf("deepseek-v4-flash-0731 的输出上限应为 384000")
	}
}

// TestAliasResolution 钉住别名（手册第 4.1 节：服务端会把别名解析成规范名）。
//
// ⚠ 别名**不需要我们改写请求体** —— 上游自己认它。
// 本表只在"按模型查输出上限"时用来归一。这一点值得写清楚，
// 否则后来者会以为漏了一处改写而去加一段多余的代码。
func TestAliasResolution(t *testing.T) {
	m, viaAlias, ok := ResolveModel("deepseek-v4.1-flash")
	if !ok {
		t.Fatal("别名应当能解析")
	}
	if !viaAlias {
		t.Error("应当报告这是经别名命中（供诊断区分）")
	}
	if m.ID != "deepseek-v4-flash-0731" {
		t.Errorf("别名应当解析到 deepseek-v4-flash-0731，得到 %q", m.ID)
	}
	if MaxOutputFor("deepseek-v4.1-flash") != 384000 {
		t.Error("别名也应当能查到输出上限（否则别名请求不会被裁剪 max_tokens）")
	}
	if _, _, ok := ResolveModel("不存在的模型"); ok {
		t.Error("未知模型不该被解析出来")
	}
	if _, _, ok := ResolveModel(""); ok {
		t.Error("空模型名不该被解析出来")
	}
}

// TestPrepareBodyForcesStreamAndClamps 出站改写：只动该动的。
func TestPrepareBodyForcesStreamAndClamps(t *testing.T) {
	t.Run("小值不动", func(t *testing.T) {
		out := string(prepareBody([]byte(`{"model":"GLM-5.3-Flash","max_tokens":100}`)))
		if !strings.Contains(out, `"max_tokens":100`) {
			t.Errorf("未超限的 max_tokens 不该被改写: %s", out)
		}
	})
	t.Run("max_completion_tokens 同样被夹", func(t *testing.T) {
		out := string(prepareBody([]byte(`{"model":"Kimi-k2.6","max_completion_tokens":999999}`)))
		if !strings.Contains(out, `"max_completion_tokens":65536`) {
			t.Errorf("max_completion_tokens 应被夹到 65536: %s", out)
		}
	})
	t.Run("未知模型不裁剪", func(t *testing.T) {
		out := string(prepareBody([]byte(`{"model":"whatever","max_tokens":999999}`)))
		if !strings.Contains(out, `"max_tokens":999999`) {
			t.Errorf("未知模型的上限未知，不该猜一个去裁剪: %s", out)
		}
		if !strings.Contains(out, `"stream":true`) {
			t.Errorf("无论如何都要强制 stream: %s", out)
		}
	})
	t.Run("非 JSON 原样返回", func(t *testing.T) {
		in := []byte("这不是 JSON")
		if out := prepareBody(in); string(out) != string(in) {
			t.Errorf("非 JSON 应当原样返回，得到 %q", out)
		}
	})
	t.Run("空体原样返回", func(t *testing.T) {
		if out := prepareBody(nil); len(out) != 0 {
			t.Errorf("空体应当原样返回，得到 %q", out)
		}
	})
}

// TestModelOf 从请求体取模型名。
func TestModelOf(t *testing.T) {
	if got := modelOf([]byte(`{"model":"spark-x"}`)); got != "spark-x" {
		t.Errorf("modelOf = %q", got)
	}
	if got := modelOf([]byte(`{}`)); got != "" {
		t.Errorf("无 model 字段应为空，得到 %q", got)
	}
	if got := modelOf([]byte(`not json`)); got != "" {
		t.Errorf("非法 JSON 应为空，得到 %q", got)
	}
}

// ── 错误分类 ────────────────────────────────────────────────────────────

// TestClassify 钉住错误分类的每一条判据。
//
// # 最关键的两条
//
//  1. 鉴权失败的响应是 **200**（手册第 3 节）—— 所以"先看状态码"的实现
//     会把 `登录已失效` 当成成功，失效的号既不冷却也不禁用，
//     每轮都重新选中、每轮都白跑。
//  2. `缺少 token` 是**我们自己的**缺陷，绝不能惩罚账号。
func TestClassify(t *testing.T) {
	p := NewWithConfig(Config{})
	cases := []struct {
		name   string
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{
			// ⚠ 200 + 正文判据 —— 本表里最重要的一行
			name: "session 失效（HTTP 200）", status: 200,
			body: `{"code":"100003","desc":"登录已失效，请重新登录"}`,
			want: gateway.ErrKindSessionDead,
		},
		{
			name: "缺少 token（HTTP 200）", status: 200,
			body: `{"code":"100002","desc":"缺少 token"}`,
			want: gateway.ErrKindClient, // 我们的头用错了，不是账号的问题
		},
		{
			name: "积分耗尽", status: 200,
			body: `{"error":{"message":"You have insufficient credits to make this request..."}}`,
			want: gateway.ErrKindHardCredit,
		},
		{
			name: "模型名不对", status: 400,
			body: `{"error":{"message":"Model \"foo\" is not supported on this endpoint"}}`,
			want: gateway.ErrKindClient,
		},
		{
			name: "生图模型已下架", status: 404,
			body: `{"code":"404","desc":"该模型暂未开放"}`,
			want: gateway.ErrKindNotFound,
		},
		{
			// 正文没有认识的东西 → 按状态码走通用兜底
			name: "裸 429 走状态码兜底", status: 429,
			body: `{"desc":"too many requests"}`,
			want: gateway.ErrKindSoftRate,
		},
		{
			name: "裸 402 走状态码兜底", status: 402,
			body: `{}`,
			want: gateway.ErrKindHardCredit,
		},
		{
			name: "裸 503 走状态码兜底", status: 503,
			body: `<html>bad gateway</html>`,
			want: gateway.ErrKindServer,
		},
		{
			name: "200 且正文不认识 → none（不惩罚）", status: 200,
			body: `{"id":"x"}`,
			want: gateway.ErrKindNone,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.Classify(c.status, c.body); got != c.want {
				t.Errorf("Classify(%d, %q) = %v(%d)，期望 %v(%d)",
					c.status, c.body, got, got, c.want, c.want)
			}
		})
	}
}

// TestClassifyDoesNotBorrowOtherUpstreamsMarkers 守住"不借用别家判据"。
//
// 这是 gateway.ErrorClassifier 存在的理由（曾经拿 workbuddy 的错误码表
// 去判所有上游，导致额度漏判 + 健康账号被裸数字 12153 永久禁用）。
// 所以这里故意喂 workbuddy 的特征串，断言 loomy **不**认它们。
func TestClassifyDoesNotBorrowOtherUpstreamsMarkers(t *testing.T) {
	p := NewWithConfig(Config{})
	for _, body := range []string{
		`{"code":"12153","msg":"Offline user session not found"}`, // workbuddy 的 session dead
		`{"error_msg":"insufficient quota"}`,                      // codearts 的额度耗尽
		`额度不足`, `余额不足`,                                            // workbuddy 的中文判据
	} {
		got := p.Classify(200, body)
		if got != gateway.ErrKindNone {
			t.Errorf("loomy 不该用别家上游的判据解释 %q（得到 %v）—— "+
				"这正是 gateway.ErrorClassifier 要防的「用 A 的事实回答 B 的问题」",
				body, got)
		}
	}
}

// TestResetAtIsNextMidnightCST 额度恢复时刻取"下一个 CST 自然日"。
func TestResetAtIsNextMidnightCST(t *testing.T) {
	p := NewWithConfig(Config{})
	got, ok := p.ResetAt(gateway.Credential{Provider: providerID, UID: "u1"})
	if !ok {
		t.Fatal("loomy 的日额度按自然日重置（手册第 4.3 节实测），应当回答恢复时刻")
	}
	if !got.After(time.Now()) {
		t.Errorf("恢复时刻必须在将来（早于 now 会让硬冷却退化成无冷却）: %v", got)
	}
	// 必须落在 CST 的 00:00，且不超过 24h
	if got.In(cstZone).Hour() != 0 || got.In(cstZone).Minute() != 0 || got.In(cstZone).Second() != 0 {
		t.Errorf("恢复时刻应当是 CST 00:00，得到 %v", got.In(cstZone))
	}
	if d := time.Until(got); d <= 0 || d > 24*time.Hour {
		t.Errorf("距今 %v，应当落在 (0, 24h]", d)
	}
}

// TestNextMidnightCSTBoundary 边界：23:59:59 → 几秒后；00:00:00 → 次日。
func TestNextMidnightCSTBoundary(t *testing.T) {
	almost := time.Date(2026, 9, 14, 23, 59, 59, 0, cstZone)
	if got := nextMidnightCST(almost); got.Day() != 15 || got.Hour() != 0 {
		t.Errorf("23:59:59 的下一个 CST 零点应当是 09-15 00:00，得到 %v", got)
	}
	exact := time.Date(2026, 9, 14, 0, 0, 0, 0, cstZone)
	if got := nextMidnightCST(exact); got.Day() != 15 {
		t.Errorf("恰好零点时应当返回**次日**零点，得到 %v", got)
	}
}

// ── Provider 契约的其余部分 ─────────────────────────────────────────────

// TestCapsAndID 能力位必须如实（声明了就必须实现）。
func TestCapsAndID(t *testing.T) {
	p := NewWithConfig(Config{})
	if p.ID() != "loomy" {
		t.Errorf("ID = %q", p.ID())
	}
	caps := p.Caps()
	if !caps.Has(gateway.CapChat) {
		t.Error("必须声明 CapChat")
	}
	if !caps.Has(gateway.CapModels) {
		t.Error("必须声明 CapModels（实测 /models 返回 12 条）")
	}
	// CapQuotaProbe：本轮补上的 —— 额度来自**本机客户端缓存的积分摘要**
	//（不是上游端点，实测 17 条候选路径全 404，见 quota_ext.go）。
	// 声明它的代价是契约要求实现 AdminExt（unverifiableCaps），
	// 那条由 TestProviderDoesNotImplementUnwantedExtensions 的另一半守住。
	if !caps.Has(gateway.CapQuotaProbe) {
		t.Error("必须声明 CapQuotaProbe —— 额度能从本机客户端缓存读到" +
			"（不声明的话账号行不会出现「额度」按钮，见 provider.go 的 Caps 注释）")
	}
	// 这四项 loomy 确实没有 —— 声明任何一个都是"声明了没实现"
	for _, c := range []gateway.Capability{
		gateway.CapCheckin, gateway.CapGrowth, gateway.CapTravel, gateway.CapWelfare,
	} {
		if caps.Has(c) {
			t.Errorf("loomy 没有 %s 能力，不该声明", gateway.String(c))
		}
	}
}

// TestAuthOfRejectsBadCredentials 凭证类型不符要返回**可诊断**的错误，且不 panic。
//
// 契约覆盖了"不 panic"，但不覆盖"错误信息是否可诊断"—— 而后者才是这条规则的
// 实际价值：拿到一句"期望 *loomy.Auth，实际 *auth.Auth"就知道该改哪里。
func TestAuthOfRejectsBadCredentials(t *testing.T) {
	p := NewWithConfig(Config{})
	ctx := context.Background()
	cases := []struct {
		name   string
		secret any
	}{
		{"nil", nil},
		{"错误类型", "not-a-loomy-auth"},
		{"错误指针类型", &struct{ X int }{}},
		{"nil 指针", (*Auth)(nil)},
		{"session 为空", &Auth{UID: "u1"}},
		{"session 只有空白", &Auth{Session: "   "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cred := gateway.Credential{Provider: providerID, UID: "u", Secret: c.secret}
			cs, err := p.Chat(ctx, cred, []byte(`{}`))
			if err == nil {
				t.Errorf("Chat 对 %s 应返回错误，实际 err=nil（status=%d）", c.name, cs.Status)
			}
			if cs.Body != nil {
				_ = cs.Body.Close()
			}
			if _, err := p.Models(ctx, cred); err == nil {
				t.Errorf("Models 对 %s 应返回错误，实际 err=nil", c.name)
			}
		})
	}
	// 具体错误信息要能指向 *loomy.Auth
	_, err := p.Chat(ctx, gateway.Credential{Secret: "x"}, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "*loomy.Auth") {
		t.Errorf("错误信息应当点名期望的类型，实际: %v", err)
	}
}

// TestChatRespectsCancelledContext 已取消的 ctx 必须立刻返回。
//
// 契约有这条检查，这里再钉一次并断言**返回的是 ctx 的错误**：
// 前端断开后继续跑完一次上游往返，会白占上游的并发额度。
func TestChatRespectsCancelledContext(t *testing.T) {
	// 指向一个不会有人监听的地址：若实现忽略了 ctx，这里会挂到超时。
	p := NewWithConfig(Config{Client: NewWithBase("http://127.0.0.1:1")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := p.Chat(ctx, newContractCredential("u1"), []byte(`{}`))
	if err == nil {
		t.Fatal("已取消的 ctx 应当返回错误")
	}
	if !errorsIs(err, context.Canceled) {
		t.Errorf("期望 context.Canceled，得到 %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("已取消的 ctx 应当立刻返回，实际耗时 %v", d)
	}
}

// TestLoadCredentialsWithSecretsSameSource 投影与 secret 必须同源。
//
// # 为什么这条值得单独测
//
// gateway.CredentialSecretLoader 的注释写明：实现者必须与 CredentialLoader
// 同源，否则会造出两份 `*loomy.Auth`，池里那份与这里这份分叉。
// 分叉的后果是"手工拷一份凭证再点重载，池里出现了账号但 secret 是 nil"
// —— 每次请求都失败，而界面上一切正常。
func TestLoadCredentialsWithSecretsSameSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "loomy-a.json", `{"session":"`+fixtureSession+`","uid":"A"}`)
	writeFile(t, dir, "loomy-b.json",
		`{"session":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","uid":"B"}`)

	p := NewWithConfig(Config{AuthDir: dir})

	plain, err := p.LoadCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	withSecret, err := p.LoadCredentialsWithSecrets("")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != len(withSecret) || len(plain) != 2 {
		t.Fatalf("两个加载器应当给出同样的 %d 条，得到 %d / %d", 2, len(plain), len(withSecret))
	}
	for i := range plain {
		if plain[i].UID != withSecret[i].UID {
			t.Errorf("第 %d 条 uid 不一致: %q vs %q", i, plain[i].UID, withSecret[i].UID)
		}
		// secret 必须是本包自己的类型，且 session 内容与投影同源
		a, ok := withSecret[i].Secret.(*Auth)
		if !ok {
			t.Fatalf("第 %d 条的 Secret 类型 = %T，期望 *loomy.Auth", i, withSecret[i].Secret)
		}
		if a.UID != plain[i].UID {
			t.Errorf("secret 里的 uid 与投影不一致: %q vs %q", a.UID, plain[i].UID)
		}
		if strings.TrimSpace(a.Session) == "" {
			t.Error("secret 里的 session 为空 —— 池子拿着它必然发不出请求")
		}
	}
	// 显式入参优先于装配注入的目录
	if got, _ := p.LoadCredentials(dir); len(got) != 2 {
		t.Errorf("显式 dir 应当生效，得到 %d 条", len(got))
	}
}

// TestAuthDirForReload 上游自报凭证目录（gateway.AuthDirExt）。
func TestAuthDirForReload(t *testing.T) {
	p := NewWithConfig(Config{AuthDir: "auths/loomy"})
	if got := p.AuthDir(); got != "auths/loomy" {
		t.Errorf("AuthDir = %q", got)
	}
	// 未注入时返回空串（核心据此回落到默认 AuthDir）
	p2 := NewWithConfig(Config{})
	if got := p2.AuthDir(); got != "" {
		t.Errorf("未注入时应当返回空串，得到 %q", got)
	}
}

// TestProviderDoesNotImplementUnwantedExtensions 钉住"没实现的那几个扩展点"。
//
// # 为什么这条测试有价值
//
// 包注释列了**刻意不实现**的扩展点，每条都有理由。但"不实现"这件事
// 在代码里是**缺席**——缺席的东西没有任何编译期信号，后来者很容易顺手加一个
// 空实现（比如 CredentialRefresher 返回 nil）就以为"补齐了能力"。
//
// 而空实现是有害的：它会让核心以为"这个上游的凭证能自动恢复"，
// 从而把「换号重试」当成有效处置 —— 而 loomy 的 session 失效只能人工重登。
//
// # 本轮订正（这不是"放宽断言"，是跟着事实走）
//
// 上一轮这条测试还把 LoginFlow / AdminExt 列在"不该实现"里，理由是
// "登录在桌面客户端里、网关无法发起"与"没有可用的管理端点"。
// 本轮实测发现客户端把登录态与积分摘要**明文缓存在本机**（见 clientstore.go），
// 于是那两条理由的**前提消失了**：能读到登录态就能实现页内添加账号，
// 有一条能自证的数据通道就能给出管理端点。
//
// 所以下面那两条断言**反向**了 —— 但它们守的命题没变：
// **声明的能力必须真的做到**。"不实现"与"实现"都只有在对得上事实时才是对的。
func TestProviderDoesNotImplementUnwantedExtensions(t *testing.T) {
	p := NewWithConfig(Config{})

	// 不实现：加一个空实现会让核心误判处置方式
	if _, ok := gateway.ExtOf[gateway.CredentialRefresher](p); ok {
		t.Error("loomy 没有可刷新的凭证（无 refresh token），不该实现 CredentialRefresher")
	}
	if _, ok := gateway.ExtOf[gateway.RefreshSkewExt](p); ok {
		t.Error("没有续期就没有「多早算该刷」，不该实现 RefreshSkewExt")
	}
	if _, ok := gateway.ExtOf[gateway.SoftRateExt](p); ok {
		t.Error("上游没有给出「限流何时解除」，不该实现 SoftRateExt")
	}
	if _, ok := gateway.ExtOf[gateway.JobExt](p); ok {
		t.Error("loomy 没有任何要定时做的事，不该实现 JobExt")
	}
	//  CredentialExpiryExt 也**不该**实现 —— 这是本轮唯一"反向"没变的一条。
	//
	// 它没有可报的到期时刻（session 不绑时间）。实现一个恒返回 (0,false)
	// 的版本不会造成伤害，但它会让"loomy 走不走时刻这条路径"变成需要读代码
	// 才能否定的问题，并把正确答案（「永久」）藏在另一个扩展点里。
	// 正确的表达是 CredentialLifetimeExt，见下面"应当实现"那张表。
	if _, ok := gateway.ExtOf[gateway.CredentialExpiryExt](p); ok {
		t.Error("loomy 没有可报的到期时刻，不该实现 CredentialExpiryExt —— " +
			"它的答案是「永久」，走 CredentialLifetimeExt")
	}

	// 实现的那几个必须**在**（缺席会让对应功能静默降级）
	for name, ok := range map[string]bool{
		"AuthDirExt":             extOK[gateway.AuthDirExt](p),
		"CredentialLoader":       extOK[gateway.CredentialLoader](p),
		"CredentialSecretLoader": extOK[gateway.CredentialSecretLoader](p),
		"ErrorClassifier":        extOK[gateway.ErrorClassifier](p),
		"ResetPolicyExt":         extOK[gateway.ResetPolicyExt](p),
		// ---- 本轮补上的五个（+ 一个可选加强版）----
		//
		// 每一个都对应用户报过的一格空白：
		//
		//	AccountColumnsExt     去掉「Token」「今日签到」两列不适用的
		//	QuotaExt              「额度」那一格
		//	CredentialLifetimeExt 「Token 到期」那一格（答案是「永久」）
		//	LoginFlow             「添加账号」按钮
		//	AdminExt              上面几条的**事实来源**（诊断端点），
		//	                      同时是 CapQuotaProbe 的契约要求
		"AccountColumnsExt":     extOK[gateway.AccountColumnsExt](p),
		"QuotaExt":              extOK[gateway.QuotaExt](p),
		"CredentialLifetimeExt": extOK[gateway.CredentialLifetimeExt](p),
		"LoginFlow":             extOK[gateway.LoginFlow](p),
		"AdminExt":              extOK[gateway.AdminExt](p),
	} {
		if !ok {
			t.Errorf("loomy 应当实现 %s（否则对应功能会静默降级）", name)
		}
	}
}

// TestClassName 分类名与 gateway 的拼写逐字一致（便于跨上游 grep 日志）。
func TestClassName(t *testing.T) {
	p := NewWithConfig(Config{})
	if got := p.Classify(200, `{"desc":"登录已失效，请重新登录"}`).String(); got != "session_dead" {
		t.Errorf("session 失效的分类名 = %q", got)
	}
	if got := p.Classify(200, `insufficient credits`).String(); got != "hard_credit" {
		t.Errorf("积分耗尽的分类名 = %q", got)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func uidsOf(list []*Auth) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.UID)
	}
	return out
}

// extOK 是 gateway.ExtOf 的薄封装，让"应当实现"的断言写成表驱动。
func extOK[T any](p gateway.Provider) bool {
	_, ok := gateway.ExtOf[T](p)
	return ok
}

// errorsIs 是 errors.Is 的薄封装（避免为一个断言引入 import 冲突）。
func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestSummarizeIsRuneSafe 错误摘要必须按字符边界截断。
//
// 按字节切会把一个汉字劈成两半，产出 \ufffd 乱码
// （本仓库在 handler.go 的 contentBlockMsg 上踩过这个坑）。
func TestSummarizeIsRuneSafe(t *testing.T) {
	long := strings.Repeat("中文", 400)
	got := summarize([]byte(long))
	if strings.Contains(got, "\ufffd") {
		t.Error("摘要里出现了替换字符 —— 说明按字节切断了多字节字符")
	}
	if len([]rune(got)) > 301 {
		t.Errorf("摘要过长: %d 个字符", len([]rune(got)))
	}
	if summarize([]byte("   ")) != "(空)" {
		t.Errorf("空白体应当显示为 (空)，得到 %q", summarize([]byte("   ")))
	}
}

// TestFakeUpstreamActuallyEnforces 是"负向对照"的自检：
// 假上游必须真的会拦住错头，否则上面的鉴权测试全是 fail-open。
//
// 这条单独写出来，是因为它的对象是**测试夹具**而不是被测代码 ——
// 夹具失效时，测试会以"全部绿灯"的形式骗人。
func TestFakeUpstreamActuallyEnforces(t *testing.T) {
	srv, _ := fakeUpstream(t, fakeAuth{session: fixtureSession})
	resp, err := http.Get(srv.URL + ModelsPath) // 不带任何鉴权头
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("鉴权失败的状态码应当是 200（照抄实测），得到 %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("鉴权失败响应不是合法 JSON: %v", err)
	}
	if got["desc"] != "缺少 token" {
		t.Errorf("desc = %v，期望「缺少 token」", got["desc"])
	}
	if got["code"] != "100002" {
		t.Errorf("code = %v，期望字符串 \"100002\"", got["code"])
	}
}
