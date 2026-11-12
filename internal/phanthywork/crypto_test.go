// crypto_test.go 把报告里**可离线断言**的那些坑逐条钉死。
//
// 这是本任务的核心验收：报告 §3 的 12 个坑里，凡是"纯函数能判"的，
// 都必须有一条失败的测试在守着 —— 因为它们在真实环境里的失败表现
// 全都**指不到根因**：
//
//	坑① 报"请更新或重新打开客户端"（实际是时钟）
//	坑② 报"网络繁忙"（实际是 UA）
//	坑③ 报"套餐不含这个模型"（实际是 id 多了前缀）
//	坑⑦⑩⑫ 报 desktop_binding_invalid / request_not_authorized（实际是段序/分隔符/哪把私钥）
//
// 所以这些用例的价值不是"覆盖代码"，而是"把不可诊断的错误变成可诊断的".
package phanthywork

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// ── 坑①：时钟偏移 ───────────────────────────────────────────────────────

// TestClockSkewNormalization 单位口径 + 幂等 + 符号方向。
//
// 实测两个数字（-66,996 与 -68,047）相差约 1000ms，说明**同一个概念在
// 两处用了不同的单位口径**。本包定死毫秒，并对"缺了整秒"的输入做规整。
func TestClockSkewNormalization(t *testing.T) {
	cases := []struct {
		in, want int64
		why      string
	}{
		{0, 0, "零偏移原样"},
		{-68047, -68047, "实测偏移（2api 报告 §3①）必须**原样保留** —— 它是真实时钟偏差"},
		{-66996, -66996, "auth 报告 §8 的另一个实测值同样原样保留"},
		{66996, 66996, "正的 67 秒偏移也是真实偏移（本机快），不许顺手拉回"},
		{-3600_001, -1, "超过 1 小时 ⇒ 判为**单位/来源错**，取余拉回亚秒区间"},
		{3600_001, 1, "同上（正方向）"},
		{-66996996, -996, "18 小时的偏移不可能是时钟 —— 这正是误把秒当毫秒的量级"},
	}
	for _, c := range cases {
		if got := NormalizeClockSkewMS(c.in); got != c.want {
			t.Errorf("NormalizeClockSkewMS(%d) = %d, want %d（%s）", c.in, got, c.want, c.why)
		}
		// 幂等：反复规整结果不变（否则"每次请求都改一点"，最难查）
		if got := NormalizeClockSkewMS(NormalizeClockSkewMS(c.in)); got != NormalizeClockSkewMS(c.in) {
			t.Errorf("NormalizeClockSkewMS 不幂等：f(f(%d)) = %d", c.in, got)
		}
	}
}

// TestServerNowUsesSkew 服务端口径的时间戳 = 本机 - 偏移。
//
// 实测 -68,047（本机**慢**）⇒ 服务端时间比本机**大** 68047ms。
// 方向搞反的话，签名时间戳会偏离两个偏移量（136 秒），一样是 403。
func TestServerNowUsesSkew(t *testing.T) {
	local := int64(1794444224000)
	if got := serverNowMS(local, -68047); got != local+68047 {
		t.Errorf("本机慢 68 秒时，服务端时间应比本机大：got %d want %d", got, local+68047)
	}
	if got := serverNowMS(local, 0); got != local {
		t.Errorf("零偏移应原样返回：got %d", got)
	}
}

// TestAuthCalibratesFromServerTime 用 server_time 校准（坑①的落地）。
func TestAuthCalibratesFromServerTime(t *testing.T) {
	a := &Auth{}
	localMS := int64(1794444224000)
	// server_time 比本机"现在"晚 68 秒 ⇒ 本机慢 68 秒
	server := time.UnixMilli(localMS + 68047).UTC().Format(time.RFC3339Nano)
	if !a.applyServerTime(server, localMS) {
		t.Fatal("applyServerTime 应当接受 RFC3339 时间")
	}
	if got := a.skew(); got != -68047 {
		t.Errorf("校准后的偏移 = %d, want -68047", got)
	}
	// 未校准 + 无基线 ⇒ 0（不做任何猜测）
	b := &Auth{}
	if got := b.skew(); got != 0 {
		t.Errorf("未校准且无基线时应为 0，得到 %d", got)
	}
}

// ── 坑②：UA ─────────────────────────────────────────────────────────────

// TestDefaultUAIsBun 钉住 DefaultUA 的**逐字**值。
//
// ⚠ 不是 bun/* 通配：报告 §3② 明确"这条值是具体版本"。
// 改成通配或改版本号 ⇒ 403 TM.00000001「网络繁忙」，而重试与换号都没用。
func TestDefaultUAIsBun(t *testing.T) {
	if DefaultUA != "Bun/1.3.14" {
		t.Errorf("DefaultUA = %q，必须逐字等于 Bun/1.3.14（坑②：否则 403 TM.00000001）", DefaultUA)
	}
	// 空 UA 也必须回落（"没配"不等于"不发"）
	c := New()
	c.UA = ""
	if got := c.userAgent(); got != DefaultUA {
		t.Errorf("UA 为空时应回落 %q，得到 %q", DefaultUA, got)
	}
	// 自定义值优先（自建反代场景）
	c.UA = "custom/1"
	if got := c.userAgent(); got != "custom/1" {
		t.Errorf("显式 UA 应生效，得到 %q", got)
	}
}

// ── 坑③：模型 id 前缀 ───────────────────────────────────────────────────

// TestStripProviderPrefix 只剥自己的前缀，且只剥一层。
//
// 实测对照：phanthy/Phanthy-fast → 403 model_not_allowed；
// Phanthy-fast → 200。所以"裸 id"是硬要求。
func TestStripProviderPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"phanthywork/Phanthy-fast", "Phanthy-fast"},
		{"Phanthy-fast", "Phanthy-fast"},
		{"phanthywork/deepseek-v4.1-flash-1017", "deepseek-v4.1-flash-1017"},
		// 别家的前缀不能被剥（核心按 provider 前缀路由，剥错就是换了个模型）
		{"cline/foo", "cline/foo"},
		{"workbuddy/auto", "workbuddy/auto"},
		// 模型名自身含斜杠：只切第一个（gateway.SplitModel 的约定）
		{"phanthywork/a/b", "a/b"},
		// 云端目录给的就是裸 id（报告 §5 实测 id 不带前缀）
		{"glm-5.3-flash", "glm-5.3-flash"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripProviderPrefix(c.in); got != c.want {
			t.Errorf("stripProviderPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPrepareBodyStripsPrefix 请求体里发出去的 model 是**裸 id**。
func TestPrepareBodyStripsPrefix(t *testing.T) {
	in := []byte(`{"model":"phanthywork/Phanthy-fast","stream":true,"max_tokens":16,
		"messages":[{"role":"user","content":"hi"}]}`)
	out, err := PrepareBody(in)
	if err != nil {
		t.Fatalf("PrepareBody 报错: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("产物不是 JSON: %v", err)
	}
	if got, _ := obj["model"].(string); got != "Phanthy-fast" {
		t.Errorf("发出去的 model = %q，必须是裸 id（坑③）", got)
	}
	// Anthropic 转换的产物必须带 messages（协议换了，但内容还在）
	if _, ok := obj["messages"]; !ok {
		t.Error("转换后的 body 缺少 messages")
	}
}

// TestPrepareBodyKeepsBareModelUntouched 没有前缀时不重新序列化。
//
// ⚠ 这条守的是坑⑧的另一半：**能不动就不动**。多一次 Marshal 就多一次
// "签名与发送的字节不一致"的机会。
func TestPrepareBodyKeepsBareModelUntouched(t *testing.T) {
	in := []byte(`{"model":"Phanthy-fast","messages":[{"role":"user","content":"hi"}]}`)
	out, err := PrepareBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if got, _ := obj["model"].(string); got != "Phanthy-fast" {
		t.Errorf("裸 id 不该被改，得到 %q", got)
	}
}

// ── 坑④：推理令牌 TTL ──────────────────────────────────────────────────

// TestInferenceTokenTTLAndMargin 冻结常量与余量判据。
func TestInferenceTokenTTLAndMargin(t *testing.T) {
	if InferenceTokenTTL != 180*time.Second {
		t.Errorf("InferenceTokenTTL = %v，必须逐字等于 180s（坑④ 实测 exp-iat = 180）", InferenceTokenTTL)
	}
	if InferenceTokenMargin != 30*time.Second {
		t.Errorf("InferenceTokenMargin = %v，必须等于 30s（报告 §3④ 的判据）", InferenceTokenMargin)
	}
	now := int64(1794444224000)
	a := &Auth{}
	// 余量 10 秒 < 30 秒 ⇒ 无效（要换新）
	a.storeInferenceToken("tok", now+10_000, now)
	if a.inferenceTokenValid(now) {
		t.Error("余量只有 10 秒时必须判为需要换新（坑④：不换就 403）")
	}
	// 余量 60 秒 ⇒ 有效
	a.storeInferenceToken("tok", now+60_000, now)
	if !a.inferenceTokenValid(now) {
		t.Error("余量 60 秒时应判为有效")
	}
	// 已过期 ⇒ 无效
	a.storeInferenceToken("tok", now-1, now)
	if a.inferenceTokenValid(now) {
		t.Error("已过期的令牌必须判为无效")
	}
	// 空令牌 ⇒ 无效
	a.clearInferenceToken()
	if a.inferenceTokenValid(now) {
		t.Error("空令牌必须判为无效")
	}
}

// ── 坑⑤：refreshToken 轮换 ⇒ 并发闸的键必须跨轮换稳定 ──────────────────

// TestGateKeyStableAcrossRotation 键**不随令牌轮换变化**。
func TestGateKeyStableAcrossRotation(t *testing.T) {
	a := newTestAuth("u-1", "refresh-old")
	k1 := a.refreshKey()
	// 模拟一次成功的轮换（原地更新）
	a.OAuth.RefreshToken = "refresh-new"
	k2 := a.refreshKey()
	if k1 != k2 {
		t.Errorf("键必须跨轮换稳定（坑⑤）：轮换前 %q，轮换后 %q", k1, k2)
	}
	// 不同账号必须不同键（换号隔离）
	b := newTestAuth("u-2", "refresh-old")
	if b.refreshKey() == k1 {
		t.Error("不同账号必须有不同的闸键（否则 A 号的刷新会挡住 B 号）")
	}
	// 键里**不能**出现 refreshToken 原文（长期凭据不该出现在键/日志里）
	if strings.Contains(k1, "refresh-old") || strings.Contains(k2, "refresh-new") {
		t.Errorf("闸键里不该出现 refreshToken 原文：%q", k1)
	}
}

// TestGateMergesInFlight 同键的并发调用共享同一次执行（只跑一次）。
//
// ⚠ 第一条断言就守着一个**实测暴露的 TOCTOU**：第一版把"查表"与"登记"拆成
// 两个方法（tryMerge + begin），8 个并发实测跑出 **2 次**真实刷新。
// acquire 把两件事放进同一个临界区，这里断言"第二个调用者拿到的是同一个 call、
// 且它**不是**发起者"。
func TestGateMergesInFlight(t *testing.T) {
	var g refreshGate
	key := "k"
	first, initiator := g.acquire(key, 1000, true)
	if !initiator {
		t.Fatal("首次调用必须是发起者")
	}
	second, initiator2 := g.acquire(key, 1001, true)
	if initiator2 {
		t.Fatal("进行中的同键调用**不该**拿到发起权（那正是并发刷两次的成因）")
	}
	if second != first {
		t.Fatal("必须复用同一个 call（这是「N 个并发只打一次」的全部机制）")
	}
	// 完成并广播
	g.finish(key, first, "tok", nil)
	<-first.done
	if first.value != "tok" || first.err != nil {
		t.Errorf("finish 后应能读到结果，得到 (%q, %v)", first.value, first.err)
	}
}

// TestGateReplaysRecentSuccess 成功结果在回放窗口内被复用（不再打上游）。
func TestGateReplaysRecentSuccess(t *testing.T) {
	var g refreshGate
	key := "k"
	call, _ := g.acquire(key, 1000, true)
	g.finish(key, call, "tok", nil)
	<-call.done

	// 窗口内 → 复用（且不是发起者）
	replay, initiator := g.acquire(key, 1000+GateReplayWindow.Milliseconds()-1, true)
	if initiator || replay != call {
		t.Fatal("回放窗口内必须复用成功结果（否则并发请求会拿旧 token 再刷一次 ⇒ 互踢）")
	}
	// 窗口外 → 允许发起新的刷新
	_, initiator = g.acquire(key, 1000+GateReplayWindow.Milliseconds()+1, true)
	if !initiator {
		t.Error("超出回放窗口后应当允许发起新的刷新")
	}
}

// TestGateNoReplayWhenDisabled 换推理令牌那条路**不许**回放（replayOK=false）。
//
// ⚠ 这是实测暴露的第二处：180 秒的令牌"上一次成功"完全可能已经过期，
// 回放它等于把一次必然 403 的请求发出去（回放的初衷是省一次往返，
// 在这里省掉的是正确性）。
func TestGateNoReplayWhenDisabled(t *testing.T) {
	var g refreshGate
	key := "k"
	call, _ := g.acquire(key, 1000, false)
	g.finish(key, call, "old-token", nil)
	<-call.done
	// 同一个时刻、同一个键，但 replayOK=false ⇒ 必须是新的发起者
	next, initiator := g.acquire(key, 1001, false)
	if !initiator || next == call {
		t.Fatal("replayOK=false 时不该回放已完成的旧结果（旧令牌可能已过期）")
	}
	// 但**进行中**的仍然要合并（那是并发去重，与回放无关）
	merged, initiator2 := g.acquire(key, 1002, false)
	if initiator2 || merged != next {
		t.Fatal("进行中的请求必须合并（只合并、不回放）")
	}
}

// TestGateFailureCooldown 失败在短冷却内复用、之后允许重试。
func TestGateFailureCooldown(t *testing.T) {
	var g refreshGate
	key := "k"
	call, _ := g.acquire(key, 1000, true)
	g.finish(key, call, "", errTestBoom)
	<-call.done
	if _, initiator := g.acquire(key, 1000+GateFailureCooldown.Milliseconds()-1, true); initiator {
		t.Error("冷却期内应复用失败结果（避免打爆上游）")
	}
	if _, initiator := g.acquire(key, 1000+GateFailureCooldown.Milliseconds()+1, true); !initiator {
		t.Error("冷却期外应允许重试")
	}
}

// ── 坑⑥：409 是终态 ────────────────────────────────────────────────────

// TestRegistrationConflictClassifiedAsSessionDead 409 判终态。
func TestRegistrationConflictClassifiedAsSessionDead(t *testing.T) {
	body := `{"error":{"code":"desktop_installation_conflict","message":"Desktop installation identifier does not match its public key."}}`
	if got := Classify(409, body); got != gateway.ErrKindSessionDead {
		t.Errorf("409 desktop_installation_conflict 必须判 ErrKindSessionDead，得到 %v", got)
	}
	// 即使状态码不是 409，只要正文是这个码，也要判终态（判定顺序：正文优先）
	if got := Classify(400, body); got != gateway.ErrKindSessionDead {
		t.Errorf("正文命中冲突码时应判终态（正文优先），得到 %v", got)
	}
	// 裸 409（正文里没有我们认识的码）同样保守判终态
	if got := Classify(409, "conflict"); got != gateway.ErrKindSessionDead {
		t.Errorf("裸 409 应保守判终态，得到 %v", got)
	}
}

// TestRegistrationCooldownBlocksRetry 熔断期内不再发请求（坑⑥的落地）。
func TestRegistrationCooldownBlocksRetry(t *testing.T) {
	now := nowMS()
	a := newTestAuth("u-1", "rt")
	a.Reg.ConflictAtMS = now - time.Minute.Milliseconds()
	p := NewProvider(Config{})
	err := p.ensureInstallation(context.Background(), a)
	if err == nil {
		t.Fatal("熔断期内必须报错（不能重发注册 ⇒ 否则持续触发设备风控）")
	}
	if !IsRegistrationConflict(err) {
		t.Errorf("熔断期内的错误应能被 IsRegistrationConflict 认出来，得到 %v", err)
	}
}

// ── 坑⑦：canonical 分隔符是 LF ───────────────────────────────────────────

// TestCanonicalUsesLFNotSpace 三条 canonical 的分隔符**逐字节**是 0x0A。
//
// ⚠ 这条是本文件里最"笨"也最重要的一条：渲染工具会把 0x0A 显示成「点」，
// 肉眼读会读成点号 —— 只有按字节断言才拦得住。
func TestCanonicalUsesLFNotSpace(t *testing.T) {
	dev := DeviceCanonical("scope", "di_x", "pubI", "pubR", "1700000000000", "nonce")
	v1 := V1Canonical("POST", "/p", "1700000000000", "nonce", []byte("{}"), "")
	inf := InferenceCanonical("POST", "/v1/messages", "1700000000000", "nonce", []byte("{}"),
		"Bearer at", "tok", "rid", "", "", "")

	for name, s := range map[string]string{"device": dev, "v1": v1, "inference": inf} {
		if !strings.Contains(s, "\n") {
			t.Errorf("%s canonical 里没有 LF（分隔符写错了）", name)
		}
		if strings.Contains(s, " ") && name != "inference" {
			// device/v1 的段里没有空格；出现空格说明用了空格当分隔符
			t.Errorf("%s canonical 里出现了空格（坑⑦：分隔符必须是 LF）: %q", name, s)
		}
		if strings.Contains(s, "|") || strings.Contains(s, ":v1") {
			t.Errorf("%s canonical 用了冒号或竖线当分隔符: %q", name, s)
		}
		// 逐字节确认第一处分隔符是 0x0A
		i := strings.IndexByte(s, '\n')
		if i <= 0 {
			t.Fatalf("%s canonical 的段结构不对: %q", name, s)
		}
		if s[i] != 0x0A {
			t.Errorf("%s canonical 的分隔符字节 = 0x%02X（必须是 0x0A）", name, s[i])
		}
	}

	// 段数：device 7 / v1 7 / inference 12（少一段就是另一个串）
	if n := len(strings.Split(dev, "\n")); n != 7 {
		t.Errorf("device canonical 段数 = %d, want 7", n)
	}
	if n := len(strings.Split(v1, "\n")); n != 7 {
		t.Errorf("v1 canonical 段数 = %d, want 7", n)
	}
	if n := len(strings.Split(inf, "\n")); n != 12 {
		t.Errorf("inference canonical 段数 = %d, want 12", n)
	}
}

// TestCanonicalEmptyTrailingSegmentsV1 坑⑪：空幂等键**不能省分隔符**。
func TestCanonicalEmptyTrailingSegmentsV1(t *testing.T) {
	withKey := V1Canonical("POST", "/p", "1", "n", []byte("{}"), "register:di_x")
	without := V1Canonical("POST", "/p", "1", "n", []byte("{}"), "")
	if withKey == without {
		t.Fatal("带幂等键与不带幂等键必须是两个不同的串")
	}
	if !strings.HasSuffix(without, "\n") {
		t.Errorf("没有幂等键时第 7 段是**空串**，分隔符不能省（坑⑪）: %q", without)
	}
	if !strings.HasSuffix(withKey, "\nregister:di_x") {
		t.Errorf("幂等键应作为第 7 段出现: %q", withKey)
	}
}

// TestInferenceCanonicalAlwaysTwelveSegments 12 段恒成立（缺项也要占位）。
func TestInferenceCanonicalAlwaysTwelveSegments(t *testing.T) {
	full := InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"),
		"Bearer at", "tok", "rid", "sid", "eid", "skill1,skill2")
	empty := InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"),
		"Bearer at", "tok", "rid", "", "", "")
	if len(strings.Split(full, "\n")) != 12 || len(strings.Split(empty, "\n")) != 12 {
		t.Fatal("inference canonical 必须恒为 12 段（缺项写空串，分隔符不能省）")
	}
	if full == empty {
		t.Error("可选的 3 段不同 ⇒ 串必须不同")
	}
}

// ── 坑⑧：请求体逐字节透传 ───────────────────────────────────────────────

// TestBodyHashIsOverFinalBytes 第 6 段哈希的是**最终发出的字节**。
func TestBodyHashIsOverFinalBytes(t *testing.T) {
	final := []byte(`{"model":"Phanthy-fast","a":1}`)
	// 重新序列化一份"语义相同但字节不同"的 body
	var obj map[string]any
	_ = json.Unmarshal(final, &obj)
	reencoded, _ := json.MarshalIndent(obj, "", "  ") // 缩进不同 ⇒ 字节不同

	h1 := sha256B64URL(RequestBodyForHash(final))
	h2 := sha256B64URL([]byte(reencoded))
	if h1 == h2 {
		t.Fatal("本用例的前提失效：两份字节应当不同")
	}
	// 签名必须用 final 那份的哈希
	canonical := InferenceCanonical("POST", "/v1/messages", "1", "n", RequestBodyForHash(final),
		"Bearer at", "tok", "rid", "", "", "")
	if !strings.Contains(canonical, h1) {
		t.Error("canonical 里的 body 哈希必须来自最终发出的字节（坑⑧）")
	}
	if strings.Contains(canonical, h2) {
		t.Error("canonical 里出现了重新序列化后的哈希（坑⑧：中途不能重新序列化）")
	}
}

// ── 坑⑨：x-phanthy-desktop-session 放的是令牌本身 ──────────────────────

// TestSessionHeaderCarriesTokenItself 头名像 sessionId，实际要放**令牌本身**。
//
// 坑⑨ 的失败形态是 401/403 而看不出为什么（名字骗人）。这条用假上游
// 做**端到端**断言：发的那个头必须**逐字等于**刚刚下发的推理令牌。
func TestSessionHeaderCarriesTokenItself(t *testing.T) {
	srv, fake := startFakeUpstream(t)
	p := newFakeProvider(t, srv)
	cred := newChatCredential("u-1")
	if _, _, err := chatOnce(t, p, cred, `{"model":"phanthywork/Phanthy-fast","messages":[]}`); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	got, issued := fake.lastSessionHeader, fake.lastInferTokenValue
	fake.mu.Unlock()
	if issued == "" {
		t.Fatal("假上游没下发过推理令牌（用例前提失效）")
	}
	if got != issued {
		t.Fatalf("x-phanthy-desktop-session = %q，应当**逐字等于**下发的推理令牌 %q（坑⑨）", got, issued)
	}
	if !strings.HasPrefix(got, "eyJ") {
		t.Errorf("这个头放的应当是令牌本身（JWT 以 eyJ 开头），得到 %q", got)
	}
}

// ── 坑⑩：device canonical 第 5 段是推理公钥 ─────────────────────────────

// TestDeviceCanonicalFifthSegmentIsInferenceKey 第 5 段是**推理公钥**。
//
// 早期实现传的是 identity 公钥又写一遍（record.publicKey），签名必然对不上。
func TestDeviceCanonicalFifthSegmentIsInferenceKey(t *testing.T) {
	identity := "AAA_identity_pub"
	inference := "BBB_inference_pub"
	s := DeviceCanonical("scope", "di_x", identity, inference, "1700000000000", "nonce")
	segs := strings.Split(s, "\n")
	if len(segs) != 7 {
		t.Fatalf("段数 = %d, want 7", len(segs))
	}
	if segs[3] != identity {
		t.Errorf("第 4 段应是 identity 公钥，得到 %q", segs[3])
	}
	if segs[4] != inference {
		t.Errorf("第 5 段必须是**推理公钥**（坑⑩），得到 %q", segs[4])
	}
	if segs[4] == segs[3] {
		t.Error("第 5 段与第 4 段相同 —— 这正是坑⑩ 的形态（identity 又写一遍）")
	}
}

// ── 坑⑪：activities/summary 不发 idempotency-key ────────────────────────

// TestSummaryHasNoIdempotencyKey 空幂等键 ⇒ canonical 第 7 段空串。
//
// ⚠ 这条只断言"传空串时的产物形状"。真正的判据（那条请求**必须**传空串）
// 在 client_test.go 里用假上游断言。
func TestSummaryHasNoIdempotencyKey(t *testing.T) {
	body := []byte(`{"inference_public_key":"pk"}`)
	withKey := V1Canonical("POST", "/api/oauth/activities/summary", "1", "n", body, "summary:x")
	noKey := V1Canonical("POST", "/api/oauth/activities/summary", "1", "n", body, "")
	if withKey == noKey {
		t.Fatal("加了幂等键会改变 canonical（这正是坑⑪ 说的「签名立刻失败」）")
	}
	if strings.HasSuffix(noKey, "summary:x") {
		t.Error("不带幂等键的串不该出现键值")
	}
	// register 那条则**要**带 register:<installationId>
	reg := V1Canonical("POST", "/api/oauth/desktop-installations/register", "1", "n", []byte("{}"),
		registerIdempotencyKey("di_x"))
	if !strings.HasSuffix(reg, "register:di_x") {
		t.Errorf("register 的幂等键格式应为 register:<installationId>，得到 %q", reg)
	}
}

// ── 坑⑫：method 在两条串里的大小写规则**相反** ──────────────────────────

// TestMethodCaseRulesDiffer 一条 toUpperCase，一条保持原值。
func TestMethodCaseRulesDiffer(t *testing.T) {
	v1 := V1Canonical("get", "/p", "1", "n", nil, "")
	if !strings.HasPrefix(v1, "v1\nget\n") {
		t.Errorf("v1 串必须**保持原值**（坑⑫），得到 %q", v1)
	}
	inf := InferenceCanonical("get", "/p", "1", "n", nil, "a", "t", "r", "", "", "")
	if !strings.HasPrefix(inf, "desktop-inference-v2\nGET\n") {
		t.Errorf("inference 串必须**大写**（坑⑫），得到 %q", inf)
	}
	// 大写输入下两条串都不该被改坏
	if !strings.HasPrefix(V1Canonical("POST", "/p", "1", "n", nil, ""), "v1\nPOST\n") {
		t.Error("v1 串对 POST 应原样保留")
	}
}

// ── installationId 的三条被否证写法 ─────────────────────────────────────

// TestInstallationIDDerivation 派生公式与三条被否证的错写法。
func TestInstallationIDDerivation(t *testing.T) {
	seed, pub, err := generateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	id, err := DeriveInstallationID(pub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "di_") {
		t.Errorf("前缀必须是 di_（不是 di:，不是空串）：%q", id)
	}
	if len(id) != 3+43 {
		t.Errorf("长度应是 di_ + 43 = 46 字符（实测 46），得到 %d: %q", len(id), id)
	}
	// 确定性：同一个公钥必然同一个 id
	again, _ := DeriveInstallationID(pub)
	if again != id {
		t.Error("派生必须确定性（同一公钥同一 id）—— 否则每次启动都换设备")
	}
	// 错写法①：哈希整段 SPKI（44 字节）→ 与正确值不同
	der, _ := publicKeyB64ToDER(pub)
	wrongFull := "di_" + sha256B64URL(der)
	if wrongFull == id {
		t.Error("错写法②（哈希整段 SPKI）不该得到同一个 id")
	}
	// 错写法②：没有前缀
	if strings.TrimPrefix(id, "di_") == id {
		t.Error("错写法③（不加前缀）不该成立")
	}
	// 换一把公钥 ⇒ 换一个 id
	_, pub2, _ := generateEd25519()
	id2, _ := DeriveInstallationID(pub2)
	if id2 == id {
		t.Error("不同公钥必须派生不同 installationId")
	}
	_ = seed
}

// TestInstallationIDRejectsBadInput 非法公钥不 panic、返回错误。
func TestInstallationIDRejectsBadInput(t *testing.T) {
	for _, bad := range []string{"", "not-base64!!", b64url([]byte("short"))} {
		if _, err := DeriveInstallationID(bad); err == nil {
			t.Errorf("非法公钥 %q 应当报错", bad)
		}
	}
}

// ── 三把密钥：互不相同 + 角色正确 ───────────────────────────────────────

// TestDeviceKeyDistinctFromIdentity IDENTITY_POLICY 的 deviceKeyDistinct。
//
// 写成同一把 ⇒ 服务端回 desktop_binding_invalid（看不出原因）。
func TestDeviceKeyDistinctFromIdentity(t *testing.T) {
	a := newTestAuth("u-1", "rt")
	identity, err := a.identityPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	device, err := a.devicePublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if identity == device {
		t.Fatal("device 公钥必须与 identity 公钥不同（IDENTITY_POLICY.deviceKeyDistinct）")
	}
	// 确定性：同一个 identity 种子永远派生出同一把 device 公钥
	device2, _ := a.devicePublicKey()
	if device2 != device {
		t.Error("device 公钥的派生必须确定性（跨重启不变）")
	}
	// 换 identity ⇒ 换 device
	b := newTestAuth("u-2", "rt")
	bDevice, _ := b.devicePublicKey()
	if bDevice == device {
		t.Error("不同 identity 应派生出不同 device 公钥")
	}
}

// TestKeyRolesAreNotInterchangeable signer 角色必须分开（用错就签不对）。
func TestKeyRolesAreNotInterchangeable(t *testing.T) {
	a := newTestAuth("u-1", "rt")
	canonical := DeviceCanonical("scope", "di_x", "pi", "pr", "1", "n")

	// device canonical 必须用 **device** 私钥签（bindingSigner = device）
	sig, err := a.signDeviceCanonical(canonical)
	if err != nil {
		t.Fatal(err)
	}
	devicePub, _ := a.devicePublicKey()
	if !verifyEd25519(devicePub, []byte(canonical), sig) {
		t.Error("device canonical 的签名必须能用 device 公钥验证")
	}
	identityPub, _ := a.identityPublicKey()
	if verifyEd25519(identityPub, []byte(canonical), sig) {
		t.Error("device canonical 的签名**不该**能用 identity 公钥验证（说明用错了私钥）")
	}

	// v1 canonical 必须用 **identity** 私钥签（headerSigner = identity）
	v1 := V1Canonical("POST", "/p", "1", "n", nil, "")
	vsig, err := a.signV1Canonical(v1)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyEd25519(identityPub, []byte(v1), vsig) {
		t.Error("v1 canonical 的签名必须能用 identity 公钥验证")
	}
	if verifyEd25519(devicePub, []byte(v1), vsig) {
		t.Error("v1 canonical 的签名**不该**能用 device 公钥验证")
	}

	// inference canonical 必须用 **推理** 私钥签
	inf := InferenceCanonical("POST", "/v1/messages", "1", "n", nil, "a", "t", "r", "", "", "")
	isig, err := a.signInferenceCanonical(inf)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyEd25519(a.Infer.PublicKey, []byte(inf), isig) {
		t.Error("inference canonical 的签名必须能用推理公钥验证")
	}
	if verifyEd25519(identityPub, []byte(inf), isig) {
		t.Error("inference canonical 的签名**不该**能用 identity 公钥验证")
	}
}

// TestSignatureCoverageChangesWithEachSegment 任一段被改 ⇒ 签名失效。
//
// ⚠ 这条是"段序/段数写错"的通用拦截器：只验一个固定向量会漏掉
// "某一段被写进了错误的字段"。
func TestSignatureCoverageChangesWithEachSegment(t *testing.T) {
	a := newTestAuth("u-1", "rt")
	base := InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"), "Bearer at", "tok", "rid", "", "", "")
	sig, err := a.signInferenceCanonical(base)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyEd25519(a.Infer.PublicKey, []byte(base), sig) {
		t.Fatal("基准签名应当可验证")
	}
	// 逐段篡改：每一处改动都必须让签名失效
	mutations := []string{
		InferenceCanonical("GET", "/v1/messages", "1", "n", []byte("{}"), "Bearer at", "tok", "rid", "", "", ""),
		InferenceCanonical("POST", "/v1/messages/", "1", "n", []byte("{}"), "Bearer at", "tok", "rid", "", "", ""),
		InferenceCanonical("POST", "/v1/messages", "2", "n", []byte("{}"), "Bearer at", "tok", "rid", "", "", ""),
		InferenceCanonical("POST", "/v1/messages", "1", "n2", []byte("{}"), "Bearer at", "tok", "rid", "", "", ""),
		InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{} "), "Bearer at", "tok", "rid", "", "", ""),
		InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"), "Bearer at2", "tok", "rid", "", "", ""),
		InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"), "Bearer at", "tok2", "rid", "", "", ""),
		InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"), "Bearer at", "tok", "rid2", "", "", ""),
		InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"), "Bearer at", "tok", "rid", "sid", "", ""),
		InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"), "Bearer at", "tok", "rid", "", "eid", ""),
		InferenceCanonical("POST", "/v1/messages", "1", "n", []byte("{}"), "Bearer at", "tok", "rid", "", "", "sk"),
	}
	for i, m := range mutations {
		if verifyEd25519(a.Infer.PublicKey, []byte(m), sig) {
			t.Errorf("第 %d 个变体（改了某一段）的签名仍然有效 —— 说明该段没进 canonical", i)
		}
	}
}

// ── 纯函数分层（crypto.go 零 IO）────────────────────────────────────────

// TestCryptoLayerHasNoNetworkOrIO crypto.go **不得** import net/http、os、log。
//
// 报告 §7.2 把"零 IO、不发请求、不认识端点"列为值得照抄的分层。
// 这条用**源码文本**断言（Go 的 import 图无法断言"某个文件"）。
func TestCryptoLayerHasNoNetworkOrIO(t *testing.T) {
	src := mustReadFile(t, "crypto.go")
	for _, banned := range []string{`"net/http"`, `"net"`, `"os"`, `"log"`, `"path/filepath"`} {
		if strings.Contains(src, banned) {
			t.Errorf("crypto.go 不得 import %s（纯函数层：零 IO、不发请求、不认识端点）", banned)
		}
	}
	// 也不该认识任何端点路径（路径必须由调用方传入）
	//
	// ⚠ 判据里的引号用**原始串**表达（`"`），避免在这一行里再写转义引号 ——
	// 本文件已经被一次转义写坏过，所以宁可多一个常量。
	const q = `"`
	for _, p := range []string{"/v1/messages", "/api/oauth/", "/oauth/token"} {
		if strings.Contains(src, q+p+q) {
			t.Errorf("crypto.go 里出现了端点字面量 %q —— 端点知识不属于纯函数层", p)
		}
	}
}

// TestClientLayerKnowsOnlyDeclaredEndpoints client.go 的端点字面量逐个核对。
//
// 这条把"我们到底打了哪些上游端点"变成可断言的事实：多一个端点
// （例如有人顺手加了个写操作）会在这里红。
func TestClientLayerKnowsOnlyDeclaredEndpoints(t *testing.T) {
	src := mustReadFile(t, "client.go")
	allowed := []string{
		pathProfile, pathRegister, pathActivitiesSummary, pathModels, pathOAuthToken,
	}
	for _, p := range allowed {
		if !strings.Contains(src, p) {
			t.Errorf("client.go 里没有出现端点 %q（声明清单与实现对不上）", p)
		}
	}
	// 本包**不**做注册（报告 §6.9 明令别写成注册可协议化）：
	// 不许出现任何注册页/注册 action 的路径。
	for _, forbidden := range []string{"Next-Action", "phoneLoginAction", "sendPhoneLoginCodeAction", "inviteCode", "referral_state"} {
		if strings.Contains(src, forbidden) {
			t.Errorf("client.go 里出现了注册相关字面量 %q —— 本上游不做注册", forbidden)
		}
	}
}

// ── 环境断言 ─────────────────────────────────────────────────────────────

// TestNoNewDependencies 本包只依赖标准库与 internal/（依赖铁律：go.mod 直接依赖只有两条）。
func TestNoNewDependencies(t *testing.T) {
	for _, f := range []string{"crypto.go", "provider.go", "client.go", "credential.go", "models.go", "errors.go", "login.go", "platform.go", "adminroute.go", "credentialext.go"} {
		src := mustReadFile(t, f)
		// ⚠ 判据必须**只看 import 块**，而且只认"整行就是一个引号包裹的路径"。
		// 第一版写成「任何以引号开头的行」，于是 map 字面量里的字符串键
		// （形如 `"uid":`）被当成依赖 —— 那是一个会误报的检查，比没有更糟。
		for _, dep := range importPaths(src) {
			if strings.HasPrefix(dep, "workbuddy2api/") {
				continue
			}
			// 标准库：第一个路径段不含 . （域名形态的就是第三方）
			first := strings.SplitN(dep, "/", 2)[0]
			if strings.Contains(first, ".") {
				t.Errorf("%s 引入了第三方依赖 %q —— 依赖铁律不许新增依赖", f, dep)
			}
		}
	}
}

// importPaths 抽取 Go 源文件 import 块里的**单行路径**。
//
// 只认"整行（去空白后）就是一个引号包裹的路径"这一种形状；
// 带别名的 import、块内注释、以及**任何在 import 块之外的字符串**都不参与 ——
// 这个检查的目的是"发现新增的第三方依赖"，误报会让它失去意义。
func importPaths(src string) []string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case l == "import (":
			inBlock = true
			continue
		case inBlock && l == ")":
			inBlock = false
			continue
		case strings.HasPrefix(l, "import "):
			l = strings.TrimSpace(strings.TrimPrefix(l, "import "))
		default:
			if !inBlock {
				continue
			}
		}
		if len(l) >= 2 && l[0] == '"' && l[len(l)-1] == '"' {
			out = append(out, l[1:len(l)-1])
		}
	}
	return out
}

// ── 测试辅助 ─────────────────────────────────────────────────────────────

// errTestBoom 一个稳定的测试错误。
var errTestBoom = cryptoErr("boom")

// newTestAuth 造一份可用于签名的测试凭证（uid + refreshToken 可控）。
func newTestAuth(uid, refreshToken string) *Auth {
	identitySeed, _, _ := generateEd25519()
	inferSeed, inferPub, _ := generateEd25519()
	return &Auth{
		Provider:     providerID,
		UIDValue:     uid,
		AccountScope: "scope-" + uid,
		OAuth: OAuthTokens{
			AccessToken:  fakeJWT(uid, 1794444224+1800),
			RefreshToken: refreshToken,
			ExpiresAtMS:  (1794444224 + 1800) * 1000,
		},
		Device: DeviceKey{IdentitySeed: identitySeed},
		Infer:  InferenceKey{Seed: inferSeed, PublicKey: inferPub},
	}
}

// fakeJWT 造一个 payload 里带 sub/iat/exp 的假 JWT（**不签名**，只用于本地解析）。
//
// ⚠ iat 必须让**寿命恰好 1800 秒**（= 报告实测的 exp-iat）：
// 本包的 RefreshSkew 复用 gateway 的「剩余 ≤ 寿命一半」策略，而寿命是从
// iat/exp 算出来的 —— 第一版这里把 iat 写成了一个固定旧时间戳，于是寿命
// 被算成两千万秒、窗口跟着变成"永远都该刷"，直接把并发的合并用例逼出
// 「续期被合并，但令牌仍未刷新」（一个假数据造出来的假故障）。
func fakeJWT(sub string, exp int64) string {
	return jwtWithTimes(sub, exp-1800, exp)
}

// mustReadFile 读本目录下的源文件（用于分层/依赖断言）。
func mustReadFile(t *testing.T, name string) string {
	t.Helper()
	b, err := readFileForTest(name)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", name, err)
	}
	return b
}

// readFileForTest 读本包目录下的文件（测试专用的小包装）。
func readFileForTest(name string) (string, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
