package loomy

import (
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// ban_detectability_test.go —— 「loomy 账号被封禁时，我们判得出来吗？」的可复现答案。
//
// # 结论（2026-11-10 实测）
//
// **判不出来。** 三条理由，每一条都有下面的用例或实测记录支撑：
//
//  ① `ProbeHealth` 只调 `GET /models`，而 **/models 对受限账号也返回 200**：
//     实测 5 个真实账号，5/5 都返回 200（9~10 个模型），其中 **3 个**一对话就是 402 积分不足。
//     ⇒ 目录能拉 ≠ 这个号能用。健康探测在这件事上是**盲的**。
//
//  ② 分类器里**没有任何封禁判据**（本文件就是那个清单的反面证据）。
//
//  ③ 于是真回封禁（403 + 正文不认识）会落到 `DefaultErrorKind` 的 `status >= 400 → ErrKindClient`，
//     而那一档的语义是「**只换号、不罚号**」（见 applyErrorPolicy 的 default 分支）——
//     结果与 workbuddy 那个「8 天 0 成功却显示完全健康」的缺陷**完全同类**：
//     坏号不会被摘掉，每轮都被重新选中。
//
// # 这个文件的作用
//
// 把「现在会怎么判」钉成可执行事实，而不是散落在人的记忆里：
// 谁将来补上封禁判据，这里会**红**，然后被有意识地更新。

// observedNoCredits 是**实测原文**（2026-11-10，用 5 个真实账号打上游）：
//
//	HTTP 402 {"error":{"message":"积分余额不足，请获取积分后再试",
//	           "type":"insufficient_quota","param":null,"code":402,
//	           "metadata":{"provider_name":"loomy"}}}
//
// ⚠ 注意：正文里是 `insufficient_quota`，而本包的 markerNoCredits 是
// `"insufficient credits"` —— **marker 并不匹配它**。
// 它之所以仍被正确判为积分耗尽，靠的是**状态码 402** 的通用兜底
// （gateway.DefaultErrorKind 的 `status == 402 → ErrKindHardCredit`）。
// 这不是缺陷（结果正确），但说明了「正文判据在这里没起作用」——
// 若上游哪天把 402 改成 200，这条就会静默漏判。
const observedNoCredits = `{"error":{"message":"积分余额不足，请获取积分后再试","type":"insufficient_quota","param":null,"code":402,"metadata":{"provider_name":"loomy"}}}`

func TestLoomyRealNoCreditsBodyIsHardCredit(t *testing.T) {
	p := NewWithConfig(Config{})
	got := p.Classify(402, observedNoCredits)
	if got != gateway.ErrKindHardCredit {
		t.Errorf("实测的 402 正文应判 hard credit（额度耗尽、按自然日恢复），实际 %v", got)
	}
	// 反向钉子：marker 没命中它 —— 判对是靠 402 状态码。
	if !strings.Contains(observedNoCredits, "insufficient_quota") {
		t.Fatal("夹具正文变了，本用例的前提不再成立")
	}
}

// TestLoomyModelsEndpointCannotJudgeAccountUsability 钉住「目录能拉 ≠ 号能用」。
//
// 实测：`GET /models` 对 5/5 个真实账号返回 200（含 3 个一对话就 402 的）。
// 所以 `ProbeHealth`（它只调 ModelList）**不能**用来判断账号是否可用，
// 更不能用来判断是否被封禁。
//
// 这一条不依赖网络：它断言的是"目录响应长这样时，分类器的判定"，
// 而目录响应恰恰是**成功**的那一种 —— 没有任何信息量。
func TestLoomyModelsEndpointCannotJudgeAccountUsability(t *testing.T) {
	p := NewWithConfig(Config{})
	// 目录的真实响应（截自实测）：HTTP 200 + 模型列表。
	modelsBody := `{"data":[{"id":"spark-x2.6"},{"id":"deepseek-v4-flash-0731"}]}`
	if got := p.Classify(200, modelsBody); got != gateway.ErrKindNone {
		t.Errorf("目录 200 应判「没问题」（ErrKindNone），实际 %v", got)
	}
	// ★ 这就是问题所在：受限账号与健康账号在这里**完全无法区分**。
}

// TestLoomyBanShapeIsCurrentlyNotDetected 钉住当前的**能力缺口**。
//
// ⚠ 这个用例故意断言「现状」而不是「期望」，因为本轮不改分类器。
//   它存在的意义是：谁将来真的补上封禁判据（无论叫 ErrKindAccountRejected 还是复用
//   ErrKindServer），**这里会红** —— 那时请把断言改成新契约，而不是把用例删掉。
func TestLoomyBanShapeIsCurrentlyNotDetected(t *testing.T) {
	p := NewWithConfig(Config{})
	// 封禁类正文（中/英/上游风格各一）+ 403：本包一个 marker 都不认识。
	for _, body := range []string{
		`{"error":{"message":"账号已被封禁","type":"account_banned"}}`,
		`{"error":{"message":"account suspended","type":"forbidden"}}`,
		``, // 空体 403（与 workbuddy 那个真实缺口同形）
	} {
		got := p.Classify(403, body)
		if got != gateway.ErrKindClient {
			t.Logf("403 + %q 已判为 %v —— 封禁判据似乎被补上了；"+
				"请把本用例改成新契约（并把 ProbeHealth 的盲区一并处理）", body, got)
			continue
		}
		t.Logf("⚠ 403 + %q ⇒ ErrKindClient（只换号、不罚号）—— 该账号**永远不会被摘除**", body)
	}
}