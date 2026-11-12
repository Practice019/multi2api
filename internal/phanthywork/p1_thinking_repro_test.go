// p1_thinking_repro_test.go ★ P1 取证：思考-only 流经"我 → 出口层"之后到底产出什么。
//
// # 这条用例是**证据**，不是回归
//
// 背景（真机实测，2026-11-12）：
//
//	glm-5.3-flash        200 / 6.7s / usage 输出 220（reasoning 192）/ **content = ""**
//	deepseek-v4.1-flash  200 / 1.2s / usage 输出  25（reasoning  23）/ **content = ""**
//
// 两个模型的共同点：**输出预算几乎全花在思考上**。所以这里用一份"只有 thinking 块、
// 没有 text 块"的 Anthropic SSE 复现它，把三件事分别看清楚：
//
//	① 本包（provider.Chat）会不会把它弄坏 —— 它只转发原始流，不解析
//	② anthroconv（出口层转换）怎么映射这些块 —— thinking → reasoning_content
//	③ wire.Aggregate（非流式聚合）产出什么 —— content 为空、reasoning 有值、usage 如何
//
// 结论写在本文件末尾的注释里（那是给报告用的口径），断言只锁"当前行为"，
// 这样将来转换层改了映射，这条会红 —— 那时必须回来更新结论。
package phanthywork

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/anthroconv"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/wire"
)

// thinkingOnlySSE 一份"只有思考块"的 Anthropic 流（字段名与事件名照 Anthropic 协议）。
//
// ⚠ usage 只出现在 message_delta 里（message_start 只报输入）—— 这是 Anthropic 的形态，
// 也正是"输出 token 会不会被算进去"的关键处。
func thinkingOnlySSE() string {
	frames := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","model":"glm-5.3-flash","usage":{"input_tokens":11,"output_tokens":0}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"让我想想…"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}` + "\n\n",
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
	return strings.Join(frames, "")
}

// TestThinkingOnlyStreamYieldsEmptyContent 取证：思考-only ⇒ content 为空、reasoning 有值。
//
// 这条把"用户看到空回复"这件事**归因**清楚：不是链路坏了（200、SSE 正常、
// 思考内容完整送达），而是"模型把预算全花在思考上，没有产生 text 块"。
func TestThinkingOnlyStreamYieldsEmptyContent(t *testing.T) {
	var out strings.Builder
	if err := anthroconv.AnthropicSSEToOpenAI(strings.NewReader(thinkingOnlySSE()), &out); err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	converted := out.String()
	if !strings.Contains(converted, "reasoning_content") {
		t.Errorf("思考内容没有被映射成 reasoning_content —— 转换层丢了它: %s", converted)
	}
	if strings.Contains(converted, `"content":"让我想想`) {
		t.Error("思考内容被当成了 content（语义错：它是推理过程，不是回答）")
	}

	resp, err := wire.Aggregate(strings.NewReader(converted))
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	msg, _ := resp["choices"].([]any)
	if len(msg) == 0 {
		t.Fatalf("聚合结果没有 choices: %#v", resp)
	}
	first, _ := msg[0].(map[string]any)
	inner, _ := first["message"].(map[string]any)
	content, _ := inner["content"].(string)
	if content != "" {
		t.Errorf("本条取证的形态应当是**空 content**（这说明我没复现出来），实际 %q", content)
	}
	// 推理内容必须完整保留（它没丢 —— 这一点决定了"该报错还是该透传"的取舍）
	if rc, _ := inner["reasoning_content"].(string); !strings.Contains(rc, "让我想想") {
		t.Errorf("reasoning_content 应当保留思考内容，实际 %q", rc)
	}
	// usage：这是 tok=0 那条症状的判据所在
	usage, _ := resp["usage"].(map[string]any)
	t.Logf("取证结果：content=%q reasoning=%v usage=%v", content, inner["reasoning_content"] != nil, usage)
}

// TestClientSawEmptyContentThroughChat 端到端（本包真实 Chat + 出口层聚合）：客户端拿到什么。
//
// ⚠ 本用例 2026-11-12 **改过一次口径**，原因值得记（改前它自己把缺陷遮住了）：
//
//	改前：断言「本包把上游 SSE **原样**带回来」，然后**手工**调一次 anthroconv 再聚合。
//	     于是生产链上那一步缺失（谁都不转换）永远测不到 —— 真机 7 次全部 content=""、
//	     reasoning_content 字段根本不存在，而这条用例一直绿。
//	改后：**不再手工转换**，直接把 Chat 的返回喂给核心的聚合层（= 非流式出口的真实链路）。
//
// 现实依据（同一模型、同一提示词，网关侧 vs 探针侧）：网关侧 7 次 content 全空；
// 探针侧（相隔数分钟）256/1024 各拿到 content 长 1（reasoning 247/105）——
// 「同一请求、两条路、一个有正文一个没有」正是这条链曾经断掉的证据。
func TestClientSawEmptyContentThroughChat(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(pathMessages, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(thinkingOnlySSE()))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newFakeProvider(t, srv)
	a := newTestAuth("u-1", "rt")
	a.Reg.RegisteredAtMS = nowMS() // 已注册：跳过写操作
	now := nowMS()
	a.storeInferenceToken("infer-token", now+120_000, now) // 令牌够新：跳过换令牌
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}

	cs, err := p.Chat(context.Background(), cred, []byte(
		`{"model":"phanthywork/glm-5.3-flash","messages":[{"role":"user","content":"1+1等于几？"}],"max_tokens":512}`))
	if err != nil {
		t.Fatalf("Chat 报错: %v", err)
	}
	if cs.Status != 200 {
		t.Fatalf("状态码 = %d, want 200", cs.Status)
	}
	raw, _ := io.ReadAll(cs.Body)
	_ = cs.Body.Close()
	// ① 交给核心的必须是 **OpenAI 形态**（核心只认 choices[].delta.*）。
	//    ⚠ 改前这里是 Anthropic 帧（message_start / content_block_delta）—— 那一版本断言必红。
	if strings.Contains(string(raw), "message_start") || strings.Contains(string(raw), "content_block_delta") {
		t.Errorf("交出去的仍是 Anthropic 帧 ⇒ 核心解析不到，客户端会拿到空 content。前 300 字节: %.300s", raw)
	}
	if !strings.Contains(string(raw), "chat.completion.chunk") {
		t.Errorf("交出去的应当是 OpenAI SSE 分片，实际前 300 字节: %.300s", raw)
	}
	// ② 核心的聚合层（非流式出口走的就是它）**直接**吃这份流 —— 不手工转换。
	resp, err := wire.Aggregate(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("聚合结果没有 choices: %#v", resp)
	}
	first, _ := choices[0].(map[string]any)
	inner, _ := first["message"].(map[string]any)
	// ③ 本条夹具只有 thinking 块、没有 text 块 ⇒ content 为空**是对的**（不是缺陷）。
	if c, _ := inner["content"].(string); c != "" {
		t.Errorf("本条夹具没有 text 块，content 应为空，实际 %q", c)
	}
	// ④ 但**思考必须投递到客户端** —— 改前这里字段压根不存在（这就是本次修的那一格）。
	if rc, _ := inner["reasoning_content"].(string); !strings.Contains(rc, "让我想想") {
		t.Errorf("思考内容必须投递到客户端（reasoning_content），实际 %q；message=%#v", rc, inner)
	}
	// ⑤ 附带信号：有推理、无正文 ⇒ 必须带 gateway_notice（wire.Aggregate 的判据）。
	if n, _ := inner["gateway_notice"].(string); strings.TrimSpace(n) == "" {
		t.Errorf("「有推理、无正文」必须带 gateway_notice，实际 message=%#v", inner)
	}
	t.Logf("端到端：content=%q reasoning_content=%q gateway_notice=%q usage=%v",
		inner["content"], inner["reasoning_content"], inner["gateway_notice"], resp["usage"])
}

// textAndThinkingSSE 一份「既有 thinking 又有 text」的 Anthropic 流。
//
// 为什么要它：真机上「同一模型、同一提示词」在两条路上一次有正文一次没有 ——
// 所以"投递"这件事必须对**两种块同时存在**的形态成立，而不只是 thinking-only。
func textAndThinkingSSE() string {
	frames := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_2","model":"glm-5.3-flash","usage":{"input_tokens":11,"output_tokens":0}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"让我想想"}}` + "\n\n",
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"1+1=2"}}` + "\n\n",
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":25}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
	return strings.Join(frames, "")
}

// TestChatDeliversBothTextAndReasoning 上游同时给 thinking 与 text ⇒ 两条都必须到客户端。
//
// 这是「去掉手工转换」之后**最直接**的那条断言：
//
//	content           == "1+1=2"
//	reasoning_content == "让我想想"
//
// 改前两者都拿不到（content 是空串、reasoning_content 字段不存在 ⇒ 第二个断言必红）；
// 这条同时覆盖 `wire.Aggregate` 的 turns/usage 归并，以及本包那次 Anthropic→OpenAI 转换。
func TestChatDeliversBothTextAndReasoning(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(pathMessages, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(textAndThinkingSSE()))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newFakeProvider(t, srv)
	a := newTestAuth("u-2", "rt")
	a.Reg.RegisteredAtMS = nowMS()
	now := nowMS()
	a.storeInferenceToken("infer-token", now+120_000, now)
	cred := gateway.Credential{Provider: providerID, UID: a.UID(), Secret: a}

	cs, err := p.Chat(context.Background(), cred, []byte(
		`{"model":"phanthywork/glm-5.3-flash","messages":[{"role":"user","content":"1+1等于几？"}],"max_tokens":512}`))
	if err != nil {
		t.Fatalf("Chat 报错: %v", err)
	}
	defer cs.Body.Close()
	resp, err := wire.Aggregate(cs.Body)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("聚合结果没有 choices: %#v", resp)
	}
	inner, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if c, _ := inner["content"].(string); c != "1+1=2" {
		t.Errorf("正文必须投递到客户端，实际 content=%q（message=%#v）", c, inner)
	}
	if rc, _ := inner["reasoning_content"].(string); rc != "让我想想" {
		t.Errorf("思考也必须投递，实际 reasoning_content=%q", rc)
	}
	if n, _ := inner["gateway_notice"].(string); n != "" {
		t.Errorf("有正文时不该出现 gateway_notice，实际 %q", n)
	}
}

// ── 结论（写在这里，供报告引用）──────────────────────────────────────────
//
// 1 空回复的**责任面**（⚠ 2026-11-12 改过一次结论 —— 原判"责任不在本包"是错的）：
//
//	上游行为   模型把输出预算全花在 thinking 块上，整条流没有 text 块（真机 220 输出里 192 是推理）
//	本包       **原判错了**：它当时只把请求转成 Anthropic，**响应却原样透传**，
//	           而核心只认 OpenAI 帧 ⇒ 整条 payload 都没被解析到（连正文一起丢，见探针证据）。
//	           真机 7 次 content="" 的直接原因就是这一格；**已修**：Chat 成功分支现在把
//	           响应转成 OpenAI SSE（与 minimax / zcode / monkeycode 同形）。
//	出口层     thinking → reasoning_content 的映射**是对的**（没有丢内容）—— 前提是有人调用它。
//
// 2 "别静默"这条落在**出口层**（wire.Aggregate）：reasoning_content 非空 + content 为空 ⇒
//   不是失败，但会给一条 `gateway_notice`（人话、可操作）。**两件事是同一条链上的两格**：
//   本包不投递 ⇒ 出口层连判据都凑不齐（没有 reasoning_content 就判不出"有推理无正文"）。
//
// 3 max_tokens 的取舍：两个模型的输出都用完了预算（220/25），说明"预算被思考吃掉"
//   是**用户 side 参数**与**模型行为**的交互，不是本包少传了参数 —— 报告里**没有任何**
//   thinking/reasoning 相关的口径（我全文搜过：零命中），所以本包**不**凭空加
//   thinking:{type:disabled}（那属于"照抄一份没有依据的协议"）。
