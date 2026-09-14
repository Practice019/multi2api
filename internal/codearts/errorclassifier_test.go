package codearts

// errorclassifier_test.go —— codearts 的错误分类扩展点，钉住 P2 缺口的**两条危害**。
//
// # 本文件是本次修复的主闸门
//
// 每条测试都对应一个"改造前必然发生"的具体后果：
//
//	① insufficient quota → 不得被判成 none（额度耗尽必须被识别）
//	② 响应体含 "12153"    → 不得被判成 session_dead（不得永久禁用）
//	③ 类型映射           → codearts.ErrAuth 必须映到 ErrKindAuth，不是 SessionDead
//
// 每条都在文件里写明了**变异验证**（把修复去掉后哪条断言会红）。
//
// ⚠ 这些测试**不打任何上游**：Classify 是纯函数，
// 只吃 (status, body) 两个参数。

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// realInsufficientQuotaBody 是 codearts 额度耗尽的**实测原文**
// （取自 quota.go 的注释，benefit 通道免费额度耗尽）：
//
//	HTTP 200 + data:{"error_code":"InferHub.4291.200",
//	                 "error_msg":"insufficient quota",
//	                 "details":[{"error_msg":"modelId: glm-5.3-flash"}, ...]}
//
// ⚠ 关键：它是 **HTTP 200** —— 额度错误塞在 SSE 流里，不是 HTTP 错误码。
// 这正是"按状态码分类"会漏判的原因。
const realInsufficientQuotaBody = `data: {"error_code":"InferHub.4291.200","error_msg":"insufficient quota","details":[{"error_msg":"modelId: glm-5.3-flash"}]}`

// ---------------------------------------------------------------------------
// 危害 ①：额度漏判
// ---------------------------------------------------------------------------

// TestClassifyRecognizesInsufficientQuota 是**危害 ① 的主闸门**。
//
// # 判据
//
// codearts 的 `insufficient quota` 必须被判成 ErrKindHardCredit
// （core 侧 → CooldownUntilNextReset，额度耗尽的长冷却）。
//
// # 为什么这条测试是必须的（实测数字）
//
// 改造前 core 用 workbuddy 的 `upstream.Classify` 判这个 body，得到 `none`：
//
//	upstream.Classify(200, realInsufficientQuotaBody) → none
//
// 因为 workbuddy 的 hardMarkers 是
//
//	"insufficient credit" / "no credit" / "quota exceeded" / "quota exhaust" /
//	"额度不足" / "余额不足" / ...
//
// **没有一条匹配 "insufficient quota"**（注意 `insufficient credit`
// 与 `insufficient quota` 是两个不同的串）。
//
// 后果：额度耗尽被完全忽略 —— 账号不进冷却、不被标记，
// 下一轮照旧选中它，把额度白烧到底，而客户端只看到一次次失败。
//
// # 变异验证（必须红）
//
// 把 `Provider.Classify` 的第一段（DetectQuotaExhausted）删掉，
// 只留 `toGatewayKind(Classify(status, body))`：
//
//	codearts.Classify 的 hardMarkers 含 "insufficient"（子串）→ 这一条**仍绿**
//
// ⚠ 所以本测试**单独**不足以夹住"复用 DetectQuotaExhausted"这个决定 ——
// 它夹住的是"额度耗尽必须被识别"这个**行为**。
// 真正夹住"必须走 DetectQuotaExhausted"的是
// TestClassifyRecognizesQuotaByDetectQuotaExhausted（用 InferHub.4291
// 这个只有 DetectQuotaExhausted 才认的串）。
//
// 若把整个第一段去掉**且** codearts.Classify 的 hardMarkers 也改成
// workbuddy 那一套（模拟"仍然用 upstream.Classify"）：
//
//	→ 得到 gateway.ErrKindNone → 本测试红 ✓
func TestClassifyRecognizesInsufficientQuota(t *testing.T) {
	p := NewWithConfig(Config{})

	got := p.Classify(200, realInsufficientQuotaBody)
	if got != gateway.ErrKindHardCredit {
		t.Fatalf("codearts 的 insufficient quota 被判成 %v，want hard_credit。\n"+
			"★ 这正是 P2 缺口 ①：改造前用 workbuddy 的 upstream.Classify 判它，\n"+
			"  它的 hardMarkers 含 \"insufficient credit\"/\"quota exceeded\"\n"+
			"  但**没有** \"insufficient quota\" → 判成 none → 额度耗尽被完全忽略。\n"+
			"  body=%s", got, realInsufficientQuotaBody)
	}
}

// TestClassifyRecognizesQuotaByDetectQuotaExhausted 夹住"必须复用
// codearts.DetectQuotaExhausted"这个决定。
//
// # 为什么单独需要这一条
//
// `InferHub.4291` 这个**错误码**只有 `DetectQuotaExhausted` 认识；
// `codearts.Classify` 的 hardMarkers 只认关键词，不认错误码。
//
// 所以：错误码在、关键词不在时，只有"先走 DetectQuotaExhausted"才判得对。
// 这是本测试与上一条的**分工**：上一条管行为，这一条管实现路径。
//
// # 变异验证（必须红）
//
// 把 `Provider.Classify` 的第一段删掉（只留 toGatewayKind(Classify(...)))：
//
//	body = `{"error_code":"InferHub.4291.200"}`（无关键词）
//	→ codearts.Classify 的 hardMarkers 一条都不命中
//	→ 200 → ErrNone → ErrKindNone
//	→ 本测试**红** ✓
func TestClassifyRecognizesQuotaByDetectQuotaExhausted(t *testing.T) {
	p := NewWithConfig(Config{})

	// 只有错误码、没有关键词 —— 刻意构造，用来区分两条识别路径。
	codeOnly := `data: {"error_code":"InferHub.4291.200","details":[{"error_msg":"modelId: glm-5.3-flash"}]}`

	if _, ok := DetectQuotaExhausted(codeOnly); !ok {
		t.Fatal("前提失效：DetectQuotaExhausted 应当认得 InferHub.4291 错误码")
	}
	if got := p.Classify(200, codeOnly); got != gateway.ErrKindHardCredit {
		t.Fatalf("只有 InferHub.4291 错误码时被判成 %v，want hard_credit ——\n"+
			"  说明 Provider.Classify 没有复用 DetectQuotaExhausted\n"+
			"  （它认错误码，而 codearts.Classify 只认关键词）。body=%s", got, codeOnly)
	}
}

// TestClassifyNeverReturnsNoneForQuotaVariants 额度耗尽的多种形态都要被识别。
//
// # 为什么列这么多种
//
// 额度耗尽的**文案随通道与语言变化**（benefit 免费额度 / 积分 / 余额）。
// 只要有一种漏判，对应的那类账号就会被静默烧穿。
//
// 每一行都标注了它由哪条路径识别（DetectQuotaExhausted 或 Classify），
// 让"哪个判据在起作用"一眼可见。
//
// # 变异验证（必须红）
//
// 去掉 Provider.Classify 的 DetectQuotaExhausted 那一段 →
// `insufficient quota` 与 `insufficient balance` 两行会靠 codearts.Classify
// 的 "insufficient" 子串侥幸通过，但**语义上已退化**（见上一条的分工说明）。
// 若同时删掉两段 → 全部红。
func TestClassifyNeverReturnsNoneForQuotaVariants(t *testing.T) {
	p := NewWithConfig(Config{})
	cases := []struct {
		name string
		body string
	}{
		{"benefit 免费额度（实测原文）", realInsufficientQuotaBody},
		{"只有错误码", `{"error_code":"InferHub.4291.200"}`},
		{"insufficient quota（裸串）", `{"error_msg":"insufficient quota"}`},
		{"insufficient balance", `{"error_msg":"insufficient balance"}`},
		{"余额不足（中文）", `{"msg":"余额不足"}`},
		{"额度不足（中文）", `{"msg":"额度不足"}`},
		{"insufficient credit（workbuddy 口径，也须识别）", `{"msg":"insufficient credit"}`},
		{"quota exceeded", `{"msg":"quota exceeded"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.Classify(200, c.body)
			if got != gateway.ErrKindHardCredit {
				t.Errorf("body=%q 被判成 %v，want hard_credit —— "+
					"额度耗尽**任何一种文案**漏判都意味着那类账号被静默烧穿", c.body, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 危害 ②：反向误伤（健康的号被永久禁用）
// ---------------------------------------------------------------------------

// TestClassifyNeverReturnsSessionDead 是**危害 ② 的主闸门**。
//
// # 判据
//
// **任何** (status, body) 组合下，codearts 的分类器都不得返回
// gateway.ErrKindSessionDead。
//
// # 为什么这是硬判据（后果最重）
//
// ErrKindSessionDead 在 core 侧映射到 `Pool.Disable` —— **永久禁用**，
// 需人工重登。而 workbuddy 的 `sessionDeadMarkers` 里有一个**裸数字 "12153"**：
//
//	upstream.Classify(200, `{"error":"InferHub.4005.200 12153 something"}`) → session_dead
//
// codearts 的错误体只要碰巧含这段子串，一个**健康**的账号就被永久打掉，
// 而界面上只会显示"已禁用"，没有任何线索指向真正的原因。
//
// # codearts 为什么根本没有 session dead 这个概念
//
// 它的凭证失效是 `ErrAuth`（401/403，securityToken 过期）——
// **可以自动续期恢复**（STS 凭证仅约 2 小时，401 是常见路径而非异常，
// 见 client.go 的 MaxAuthRetry 与 credentialrefresher.go）。
// 与 workbuddy 的"必须人工重登"是**完全不同的恢复路径**。
//
// # 变异验证（必须红）
//
// 把 `Provider.Classify` 换成 `upstream.Classify` 的等价判据
// （或让 `toGatewayKind` 把 ErrAuth 映到 ErrKindSessionDead）：
//
//	body 含 "12153" → session_dead → **本测试红** ✓
func TestClassifyNeverReturnsSessionDead(t *testing.T) {
	p := NewWithConfig(Config{})

	// 覆盖：workbuddy 的判据串、codearts 的可能误命中形态、各类状态码。
	bodies := []string{
		"12153",
		`{"code":12153,"msg":"Offline user session not found"}`, // workbuddy 的 session dead 原文
		`Offline user session not found`,
		`{"error":"InferHub.4005.200 12153 unsupported model"}`,
		`{"error_code":"InferHub.4005.200","error_msg":"12153"}`,
		`{"details":[{"error_msg":"modelId: 12153"}]}`,
		"",
		`{"error_msg":"insufficient quota"}`,
	}
	statuses := []int{200, 400, 401, 403, 404, 429, 500, 502, 503}

	for _, st := range statuses {
		for _, b := range bodies {
			if got := p.Classify(st, b); got == gateway.ErrKindSessionDead {
				t.Fatalf("Classify(%d, %q) = session_dead ——\n"+
					"★ 这正是 P2 缺口 ②：core 的 ErrKindSessionDead 分支调用\n"+
					"  Pool.Disable（**永久禁用**，需人工重登）。codearts 没有\n"+
					"  session dead 这个概念，它的凭证失效是 ErrKindAuth（可自动续期）。\n"+
					"  一个健康的账号会因为一个无关的数字被永久打掉。", st, b)
			}
		}
	}
}

// TestClassifyAuthIsNotSessionDead codearts 的 401/403 映射到 ErrKindAuth。
//
// # 为什么 Auth ≠ SessionDead（两者在 core 侧后果完全不同）
//
//	ErrKindSessionDead → Pool.Disable（永久禁用）
//	ErrKindAuth        → default 分支：只换号不罚（可自动续期恢复）
//
// # 变异验证（必须红）
//
// 把 `toGatewayKind` 里 `case ErrAuth:` 改成 `return gateway.ErrKindSessionDead`：
// → 本测试红 ✓（且 TestClassifyNeverReturnsSessionDead 也会红）
func TestClassifyAuthIsNotSessionDead(t *testing.T) {
	p := NewWithConfig(Config{})

	// 401 与 403 都是 codearts 的凭证失效（securityToken 过期）。
	for _, st := range []int{401, 403} {
		got := p.Classify(st, `{"error_msg":"securityToken expired"}`)
		if got != gateway.ErrKindAuth {
			t.Errorf("Classify(%d, ...) = %v，want auth。\n"+
				"  codearts 的凭证失效是**可自动续期恢复**的（STS 约 2 小时），\n"+
				"  必须走 core 的\"只换号不罚\"分支，而不是永久禁用。", st, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 状态码判据仍然生效（两步是叠加，不是替代）
// ---------------------------------------------------------------------------

// TestClassifyStatusCodesStillWork 正文认不出时，状态码判据必须兜住。
//
// # 为什么需要这一条
//
// 新加的第一步（额度识别）如果写成"命中就返回，否则返回 none"，
// 就会把 codearts 自己的状态码判据整个盖掉 —— 那会让
// 429 不再触发软冷却、5xx 不再喂熔断。这是"修一个弄坏另一个"的典型。
//
// 两步必须是**叠加**：先额度，再状态码。
//
// # 变异验证（必须红）
//
// 把 `return toGatewayKind(Classify(status, body))` 改成
// `return gateway.ErrKindNone`（即只保留第一步）：
// → 本测试全部红 ✓
func TestClassifyStatusCodesStillWork(t *testing.T) {
	p := NewWithConfig(Config{})
	cases := []struct {
		status int
		body   string
		want   gateway.ErrorKind
	}{
		{429, "", gateway.ErrKindSoftRate},
		{429, `{"error_msg":"too many requests"}`, gateway.ErrKindSoftRate},
		{404, "", gateway.ErrKindNotFound},
		{500, "", gateway.ErrKindServer},
		{502, "bad gateway", gateway.ErrKindServer},
		{400, `{"error_msg":"bad request"}`, gateway.ErrKindClient},
		{402, "", gateway.ErrKindHardCredit},
		{200, "", gateway.ErrKindNone},
		{200, "ok", gateway.ErrKindNone},
	}
	for _, c := range cases {
		if got := p.Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q) = %v，want %v —— "+
				"额度判据之外的路径必须继续按 codearts 的状态码事实分类",
				c.status, c.body, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 类型映射（枚举漂移防护）
// ---------------------------------------------------------------------------

// TestToGatewayKindMapsEveryCodeartsKind 逐个锁定 codearts.ErrKind → gateway.ErrorKind。
//
// # 为什么要显式列举而不是裸类型转换
//
// 裸转换（`gateway.ErrorKind(k)`）依赖"两个枚举顺序永远一致"这个隐含约定，
// 任一边插一个新常量就会**静默错位**（ErrSessionDead 变成 ErrHardCredit）。
// 显式 switch + 本测试把它变成编译期/测试期可见的失败。
//
// # 变异验证（必须红）
//
// 改任意一行的映射目标（例如 ErrAuth → ErrKindSessionDead）：
// → 本测试红 ✓
func TestToGatewayKindMapsEveryCodeartsKind(t *testing.T) {
	want := map[ErrKind]gateway.ErrorKind{
		ErrNone:       gateway.ErrKindNone,
		ErrHardCredit: gateway.ErrKindHardCredit,
		ErrSoftRate:   gateway.ErrKindSoftRate,
		ErrAuth:       gateway.ErrKindAuth, // ⚠ 刻意不是 SessionDead
		ErrNotFound:   gateway.ErrKindNotFound,
		ErrServer:     gateway.ErrKindServer,
		ErrClient:     gateway.ErrKindClient,
	}
	for k, w := range want {
		if got := toGatewayKind(k); got != w {
			t.Errorf("toGatewayKind(%v) = %v，want %v", k, got, w)
		}
	}

	// 覆盖检查：上面必须列全 codearts.ErrKind 的所有取值。
	// 值域是 0..ErrClient（见 client.go 的常量声明）。
	all := []ErrKind{ErrNone, ErrHardCredit, ErrSoftRate, ErrAuth, ErrNotFound, ErrServer, ErrClient}
	for _, k := range all {
		if _, ok := want[k]; !ok {
			t.Errorf("codearts.ErrKind %v 没有在映射表里被覆盖 —— "+
				"新增常量必须显式映射，否则会落到 default（静默吞掉）", k)
		}
	}
	if len(want) != len(all) {
		t.Errorf("映射表有 %d 项，但 codearts.ErrKind 有 %d 个取值 —— 有遗漏",
			len(want), len(all))
	}
}

// TestToGatewayKindDefaultIsNone 未知值落到 ErrKindNone（不给惩罚性语义）。
//
// # 为什么 default 不能是惩罚性分类
//
// 若 default 返回 ErrKindSessionDead（"最严重"直觉上像"安全"），
// 一次枚举错位就会演变成**批量永久禁用**。
// 保守方向是明确的：未知 = 不认识 = 不惩罚（只换号）。
//
// # 变异验证（必须红）
//
// 把 `toGatewayKind` 的 default 改成 `return gateway.ErrKindSessionDead`：
// → 本测试红 ✓
func TestToGatewayKindDefaultIsNone(t *testing.T) {
	unknown := ErrKind(999)
	if got := toGatewayKind(unknown); got != gateway.ErrKindNone {
		t.Errorf("toGatewayKind(未知值) = %v，want none —— "+
			"未知值**绝不能**映射到惩罚性分类（尤其不能是 session_dead=永久禁用）", got)
	}
}

// TestClassifyIsPureFunction 分类是纯函数：同一输入恒返回同一结果。
//
// # 为什么需要（实现约束）
//
// gateway.ErrorClassifier 明确要求纯函数（不发网络、不改状态）。
// 它在出站循环的每次非 2xx 上被调用。若实现里混入了"改共享状态"，
// 并发调用下会数据竞争，而那是最难查的一类问题。
//
// 这条测试用**并发调用 + 结果一致性**来夹住它：
// 一个非纯实现（例如带内部计数器并据它决策）会在并发下露出不一致。
//
// ⚠ 它不能证明"没发网络"（那需要更重的机制），但能夹住"结果依赖于
// 调用历史/顺序/共享可变状态"—— 那是最常见的纯度破坏形态。
func TestClassifyIsPureFunction(t *testing.T) {
	p := NewWithConfig(Config{})
	const body = realInsufficientQuotaBody
	want := p.Classify(200, body)

	done := make(chan gateway.ErrorKind, 64)
	for i := 0; i < 64; i++ {
		go func() { done <- p.Classify(200, body) }()
	}
	for i := 0; i < 64; i++ {
		if got := <-done; got != want {
			t.Fatalf("并发调用结果不一致: %v vs %v —— "+
				"Classify 必须是纯函数（不得依赖共享可变状态）", got, want)
		}
	}
}
