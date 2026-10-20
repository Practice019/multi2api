// context_exceeded_test.go 上下文超限必须判 FatalClient（2026-10-20，issue #2 观察②）
//
// # 这个文件钉的是什么
//
// issue #2 的第二个观察：
//
//	「`context_length_exceeded` 仍会轮换全部账号。上游 400 `fatal_client`
//	 （"prompt is too long: 100001 tokens > 100000 maximum"）实测仍把账号池
//	  轮一遍，最后返回笼统的 503 `budget_exhausted:global`，客户端看不到上游原因。」
//
// 查证结果：workbuddy 侧（`internal/upstream/client.go` 的 `fatalClientMarkers`）
// 早就覆盖了这些形态；**loomy 侧完全没有上下文超限判定** —— 它落到
// `gateway.DefaultErrorKind(status, body)` 的 `status >= 400 → ErrKindClient`，
// 而那一档的语义是"只换号不罚"。
//
// ⇒ 一次"这次请求太长"会把 loomy 的 46 个号全轮一遍（每个号收到的 body 一样长，
//
//	换到哪都必然超限），纯白烧 46 次往返，最后报成笼统的 503。
//
// 所以这批 marker 加在 loomy 的 classifier 里。
package loomy

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestContextExceededIsFatalClient ★ 核心：issue 报的原文必须判 FatalClient。
func TestContextExceededIsFatalClient(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// ← issue #2 报告的原文，一字不改
			"issue 原文",
			`{"error":{"message":"prompt is too long: 100001 tokens > 100000 maximum","type":"invalid_request_error","code":400}}`,
		},
		{"OpenAI 标准 code", `{"error":{"code":"context_length_exceeded","message":"too big"}}`},
		{"maximum context length", `{"error":{"message":"This model's maximum context length is 128000 tokens"}}`},
		{"exceeds the maximum", `{"error":{"message":"input exceeds the maximum allowed size"}}`},
		{"context overflow", `{"error":{"message":"context overflow detected"}}`},
		{"too many tokens", `{"error":{"message":"too many tokens in request"}}`},
		{"中文 超出上下文", `{"error":{"message":"超出上下文限制"}}`},
		{"中文 上下文长度", `{"error":{"message":"上下文长度超限"}}`},
	}
	for _, c := range cases {
		got := (&Provider{}).Classify(400, c.body)
		if got != gateway.ErrKindFatalClient {
			t.Fatalf("★ %s：Classify = %v，期望 ErrKindFatalClient —— "+
				"判成 Client 会让 loomy 的 46 个号全轮一遍（每个号收到的 body 一样长，必然都超限）",
				c.name, got)
		}
	}
}

// TestContextExceededNotClient 反证：不能因为"看起来像 400"就漏判成 Client。
func TestContextExceededNotClient(t *testing.T) {
	body := `{"error":{"message":"prompt is too long: 100001 tokens > 100000 maximum"}}`
	if got := (&Provider{}).Classify(400, body); got == gateway.ErrKindClient {
		t.Fatal("★ 上下文超限被判成 ErrKindClient —— 那正是 issue #2 报的缺陷")
	}
}

// TestFatalDoesNotPunishAccount 判 FatalClient 时不该顺带把账号判死。
//
// ⚠ 这条防的是过度修复：上下文超限**不是账号的错**（每个号收到的 body 一样长），
// 所以它绝不能落进任何"罚账号"的类别（HardCredit / SessionDead / SoftRate）。
func TestFatalDoesNotPunishAccount(t *testing.T) {
	body := `{"error":{"message":"prompt is too long: 100001 tokens > 100000 maximum"}}`
	got := (&Provider{}).Classify(400, body)
	punishing := map[gateway.ErrorKind]string{
		gateway.ErrKindHardCredit:  "长冷却到额度重置",
		gateway.ErrKindSessionDead: "永久禁用等人重登",
		gateway.ErrKindSoftRate:    "软冷却",
	}
	if what, bad := punishing[got]; bad {
		t.Fatalf("★ 上下文超限被判成 %v（%s）—— 那会把账号错误地冷却/禁用，而账号在这件事上无过错", got, what)
	}
}

// TestInvalidRequestStillFatal 回归：原有 invalid_request 判定不能被新 case 挤掉。
func TestInvalidRequestStillFatal(t *testing.T) {
	if got := (&Provider{}).Classify(400, `{"error":{"type":"invalid_request_error","message":"请求参数错误"}}`); got != gateway.ErrKindFatalClient {
		t.Fatalf("invalid_request_error 仍应判 FatalClient，实际 %v", got)
	}
	if got := (&Provider{}).Classify(400, `{"error":{"message":"请求参数错误"}}`); got != gateway.ErrKindFatalClient {
		t.Fatalf("中文「请求参数错误」仍应判 FatalClient，实际 %v", got)
	}
}

// TestOtherMarkersUnaffected 回归：其余 marker 的判定不受新 case 影响。
func TestOtherMarkersUnaffected(t *testing.T) {
	cases := []struct {
		body string
		want gateway.ErrorKind
	}{
		{`{"error":{"message":"insufficient credits"}}`, gateway.ErrKindHardCredit},
		{`{"error":{"message":"登录已失效，请重新登录"}}`, gateway.ErrKindSessionDead},
		{`{"error":{"message":"该模型暂未开放"}}`, gateway.ErrKindNotFound},
		{`{"error":{"message":"model is not supported on this endpoint"}}`, gateway.ErrKindClient},
		{`{"error":{"message":"缺少 token"}}`, gateway.ErrKindClient},
	}
	for _, c := range cases {
		if got := (&Provider{}).Classify(400, c.body); got != c.want {
			t.Fatalf("Classify(%q) = %v，期望 %v —— 新加的上下文判定挤掉了原有分类", c.body, got, c.want)
		}
	}
}
