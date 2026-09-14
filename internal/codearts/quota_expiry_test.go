// quota_expiry_test.go 钉住"额度是**每日**状态，不是永久事实"这条语义。
//
// # 这一组测试防的是什么
//
// benefit 通道的免费额度按日重置。若把"今天用完了"写成静态事实
// （例如 models.go 的 Verified=false），会造成永久隐藏 + 死锁：
//
//	隐藏 → 没有请求 → 不触发 ClearQuota → 永远不恢复
//
// 所以额度状态必须带**到期时间**，且过期判断放在读取侧（无状态、重启不丢）。
// 下面每条都对应一个会退化成长时间隐藏的具体途径。
package codearts

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// modelsForTest 用一份合法凭证取目录（Models 会先做 authOf 校验）。
func modelsForTest(t *testing.T, p *Provider) []gateway.ModelInfo {
	t.Helper()
	cred := gateway.Credential{
		Provider: ProviderID,
		UID:      "uid-test",
		Secret:   &Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"},
	}
	ms, err := p.Models(context.Background(), cred)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	return ms
}

func hasModel(ms []gateway.ModelInfo, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestQuotaMarkExpiresAtNextMidnight 确认标记带日切到期时间。
func TestQuotaMarkExpiresAtNextMidnight(t *testing.T) {
	const model = "test-model-expiry"
	defer ClearQuota(model)

	MarkQuotaExhausted(model, "insufficient quota")

	states, _ := QuotaStates()
	s, ok := states[model]
	if !ok || !s.Exhausted {
		t.Fatalf("标记后应看到额度耗尽，实际 %+v", s)
	}
	if s.ExhaustedUntil.IsZero() {
		t.Fatal("★ 标记没带到期时间 —— 模型会被永久隐藏，额度次日恢复也不会自愈")
	}

	// 到期时刻必须是"下一个本地日零点"，且在当前时刻之后。
	now := time.Now()
	y, m, d := now.Date()
	want := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	if !s.ExhaustedUntil.Equal(want) {
		t.Errorf("到期时刻 = %v，期望下一个本地日零点 %v", s.ExhaustedUntil, want)
	}
	if !s.ExhaustedUntil.After(now) {
		t.Errorf("到期时刻 %v 不晚于当前 %v —— 标记会立刻失效", s.ExhaustedUntil, now)
	}
}

// TestIsQuotaExhaustedExpiresAtRollover 是核心：过期判断发生在**读取侧**。
//
// 手工把到期时刻挪到过去，模拟跨日 —— 不需要后台任务、不需要重启，
// 读取侧立刻返回"不再耗尽"。这正是"每日额度"自愈的机制。
func TestIsQuotaExhaustedExpiresAtRollover(t *testing.T) {
	const model = "test-model-rollover"
	defer ClearQuota(model)

	MarkQuotaExhausted(model, "insufficient quota")
	if !IsQuotaExhausted(model) {
		t.Fatal("刚标记后应判定为耗尽")
	}

	// 模拟"已经到了次日"：把到期时刻推到过去。
	quotaCache.mu.Lock()
	st := quotaCache.states[model]
	st.ExhaustedUntil = time.Now().Add(-time.Second)
	quotaCache.states[model] = st
	quotaCache.mu.Unlock()

	if IsQuotaExhausted(model) {
		t.Error("★ 已过到期时刻仍判定为耗尽 —— 跨日后模型无法自动恢复，会一直隐藏")
	}
}

// TestIsQuotaExhaustedUnmarkedModel 确认未标记的模型永远不算耗尽。
func TestIsQuotaExhaustedUnmarkedModel(t *testing.T) {
	if IsQuotaExhausted("GLM-5.2") {
		t.Error("未标记过的模型不应被判定为额度耗尽")
	}
	if IsQuotaExhausted("") {
		t.Error("空模型名不应判定为耗尽")
	}
}

// TestModelsFiltersExhaustedButRecovers 钉住 /v1/models 的可逆性。
//
// 这是"当前不可用"对客户端的表达：耗尽当天不列出（客户端据此换模型），
// 日切后自动回来。**不可逆**的隐藏是本 bug 的核心危害。
func TestModelsFiltersExhaustedButRecovers(t *testing.T) {
	const model = "glm-5.3-flash"
	defer ClearQuota(model)

	p := NewWithConfig(Config{})

	// 基线：全部 7 个都在。
	base := modelsForTest(t, p)
	if !hasModel(base, model) {
		t.Fatalf("基线目录里缺少 %s", model)
	}

	// 标记额度耗尽 → 当天不列出。
	MarkQuotaExhausted(model, "insufficient quota")
	got := modelsForTest(t, p)
	if hasModel(got, model) {
		t.Error("★ 额度耗尽的模型仍被列出 —— 客户端会选中它然后必然失败")
	}
	if len(got) != len(base)-1 {
		t.Errorf("应恰好少 1 个：基线 %d，实际 %d", len(base), len(got))
	}

	// 模拟跨日 → 自动恢复列出。
	quotaCache.mu.Lock()
	st := quotaCache.states[model]
	st.ExhaustedUntil = time.Now().Add(-time.Second)
	quotaCache.states[model] = st
	quotaCache.mu.Unlock()

	back := modelsForTest(t, p)
	if !hasModel(back, model) {
		t.Error("★ 跨日后模型没回来 —— 每日额度恢复了但客户端永远看不到它")
	}
}

// TestQuotaTrackingBodyMarksOnQuotaError 覆盖**真实请求路径**上的就地标记。
//
// 这是 quota.go 注释里写明、改造前却没有任何生产调用方的那条线：
// 额度耗尽藏在 200 的流里，只有读过流才知道。
func TestQuotaTrackingBodyMarksOnQuotaError(t *testing.T) {
	const model = "glm-5.3-flash"
	defer ClearQuota(model)

	// 实测的 benefit 额度耗尽形态（HTTP 200 + 流内业务错误）。
	stream := `data:{"error_code":"InferHub.4291.200","error_msg":"insufficient quota",` +
		`"details":[{"error_msg":"modelId: glm-5.3-flash"}]}` + "\n\n"

	body := &quotaTrackingBody{
		ReadCloser: io.NopCloser(strings.NewReader(stream)),
		model:      model,
	}
	_, _ = io.ReadAll(body)

	if !IsQuotaExhausted(model) {
		t.Error("★ 真实请求撞到额度耗尽却没有标记 —— " +
			"/v1/models 不会反映，用户看到的是『模型在列但恒失败』")
	}
}

// TestQuotaTrackingBodyClearsOnSuccess 覆盖**自愈点**。
//
// 额度次日恢复后，第一条成功请求必须解除标记 ——
// 否则模型会一直从目录里消失（隐藏 → 无请求 → 永不解除的死锁）。
func TestQuotaTrackingBodyClearsOnSuccess(t *testing.T) {
	const model = "glm-5.3-flash"
	defer ClearQuota(model)

	MarkQuotaExhausted(model, "insufficient quota")
	if !IsQuotaExhausted(model) {
		t.Fatal("前置条件失败：应处于耗尽状态")
	}

	// 一次正常回复（第一帧就带 choices）。
	stream := `data:{"id":"c1","model":"glm-5.3-flash",` +
		`"choices":[{"index":0,"delta":{"content":"你好"}}]}` + "\n\n"

	body := &quotaTrackingBody{
		ReadCloser: io.NopCloser(strings.NewReader(stream)),
		model:      model,
	}
	_, _ = io.ReadAll(body)

	if IsQuotaExhausted(model) {
		t.Error("★ 成功回复后额度标记未清除 —— 模型会一直隐藏，形成死锁")
	}
}

// TestQuotaTrackingBodyIgnoresUnrelatedErrors 确认不误标。
//
// 误标的代价：模型被隐藏，而真正的原因（并发上限/参数非法）与额度无关。
func TestQuotaTrackingBodyIgnoresUnrelatedErrors(t *testing.T) {
	const model = "GLM-5.2"
	defer ClearQuota(model)

	stream := `data:{"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)"}` + "\n\n"

	body := &quotaTrackingBody{
		ReadCloser: io.NopCloser(strings.NewReader(stream)),
		model:      model,
	}
	_, _ = io.ReadAll(body)

	if IsQuotaExhausted(model) {
		t.Error("并发上限不是额度耗尽，不应标记")
	}
}
