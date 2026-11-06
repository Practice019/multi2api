package codearts

// models_upstream_test.go —— T-236 的守卫：模型目录必须**从上游拉**，且**拉不到要降级**。
//
// # 本文件要钉住的风险
//
//  1. 目录回退成"读快照"（用户实测的那条 503 就是快照多列了账号没有的模型）；
//  2. 上游拉失败时**报错而不是降级** —— 那会让整个上游在客户端面板里消失，
//     比多列/少列几条严重得多；
//  3. 上游返回空目录时同样要降级；
//  4. 上游有、快照没有的新模型必须**照样出现**（否则用户永远等不到新模型）；
//  5. 额度耗尽的过滤不能被上游路径绕过（这是 T-196 那套可逆表达的命门）。
//
// 全程用 mock upstream（`newMockClient`），**不碰网络、不需要真实凭证**。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// offlineModelsClient 造一个**绝不出网**的 client：出口指向一个必然拒绝连接的地址。
//
// 给"只想测快照目录 + 额度过滤"的既有测试用（T-236 之后 Models() 会拉上游清单，
// 若不注入离线 client，那些单元测试会去打真实网络）。
func offlineModelsClient() *Client {
	return &Client{
		HTTP:          &http.Client{Timeout: 300 * time.Millisecond},
		ChatHTTP:      &http.Client{Timeout: 300 * time.Millisecond},
		EngineBase:    "http://127.0.0.1:1",
		STSBase:       "http://127.0.0.1:1",
		HeaderTimeout: 300 * time.Millisecond,
	}
}

// upstreamModelJSON 造一条 /v1/model/builtin 里的模型项（字段名按上游原文）。
func upstreamModelJSON(id, name string, enable, display bool) string {
	b, _ := json.Marshal(map[string]any{
		"model_id": id, "model_name": name, "model_category": "文本生成",
		"enable": enable, "display_enabled": display, "model_desc": "…",
	})
	return string(b)
}

// newModelsProvider 造一个 Provider，出口指向一个只实现 /v1/model/builtin 的 mock。
//
//   - `body`：清单端点要回的 JSON（"" 表示回 500，用来测降级）
//   - 返回的 `hits` 记录清单端点被调了几次（证明"真的打上游了"）
func newModelsProvider(t *testing.T, body string) (*Provider, *int, func()) {
	t.Helper()
	hits := 0
	engine := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ModelListPath) {
			hits++
			if body == "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error_code":"boom"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		// 别的路径一律 404：本测试只关心清单端点。
		w.WriteHeader(http.StatusNotFound)
	}
	c, closeFn := newMockClient(t, engine, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	p := NewWithConfig(Config{Client: c})
	return p, &hits, closeFn
}

func credForTest() gateway.Credential {
	return gateway.Credential{
		Provider: ProviderID,
		UID:      "uid-models",
		Secret:   &Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"},
	}
}

func idsOf(ms []gateway.ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// ① 上游返回 11 个 ⇒ 目录就是这 11 个（不再返回 7 个快照）
// ---------------------------------------------------------------------------

func TestModelsComeFromUpstream(t *testing.T) {
	// 上游真实形态（T-236 实测的 11 个，按原文抄；顺序也照上游）。
	ids := []string{
		"GLM-5.2", "openpangu-2.0-flash", "openpangu-2.0-pro", "glm-5.3",
		"glm-5.3-flash", "glm-5.2-sft-harmony", "deepseek-v4.1-flash",
		"deepseek-v4.1-flash-1017", "kimi-k3", "kimi-k3.1-pro", "speed",
	}
	var items []string
	for _, id := range ids {
		items = append(items, upstreamModelJSON(id, id, true, true))
	}
	body := `{"count":11,"builtinModels":[` + strings.Join(items, ",") + `]}`
	p, hits, closeFn := newModelsProvider(t, body)
	defer closeFn()

	ms, err := p.Models(context.Background(), credForTest())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if *hits == 0 {
		t.Fatal("★ Models() 没有请求上游清单端点 —— 回退成读快照了（这正是用户那 503 的根因）")
	}
	if len(ms) != 11 {
		t.Fatalf("应有 11 个（上游清单），实际 %d 个: %v", len(ms), idsOf(ms))
	}
	for _, id := range ids {
		if !hasModel(ms, id) {
			t.Errorf("上游有的模型 %q 没出现在目录里", id)
		}
	}
	// ★ 反向：快照多列的那两个**必须消失**
	for _, stale := range []string{"deepseek-v4-flash-0731", "deepseek-v4-pro-0813"} {
		if hasModel(ms, stale) {
			t.Errorf("★ 快照里的 %q 仍被列出 —— 它会以 TM.000010051「无权调用」的形态失败", stale)
		}
	}
}

// ---------------------------------------------------------------------------
// ② 上游失败 ⇒ 降级到快照（不报错）
// ---------------------------------------------------------------------------

func TestModelsFallsBackOnUpstreamError(t *testing.T) {
	p, hits, closeFn := newModelsProvider(t, "") // "" = 500
	defer closeFn()

	ms, err := p.Models(context.Background(), credForTest())
	if err != nil {
		t.Fatalf("★ 上游失败时 Models 必须降级而不是报错（报错会让整个上游在面板里消失）: %v", err)
	}
	if *hits == 0 {
		t.Fatal("没有真的请求上游（那这条测试测不到降级）")
	}
	if len(ms) != len(KnownModels()) {
		t.Fatalf("降级后应等于快照（%d 个），实际 %d 个: %v", len(KnownModels()), len(ms), idsOf(ms))
	}
	if !hasModel(ms, "GLM-5.2") {
		t.Error("降级目录里缺 GLM-5.2")
	}
}

// ---------------------------------------------------------------------------
// ③ 上游返回空目录 ⇒ 同样降级（有些账号可能接口不给数据）
// ---------------------------------------------------------------------------

func TestModelsFallsBackOnEmptyUpstream(t *testing.T) {
	p, hits, closeFn := newModelsProvider(t, `{"count":0,"builtinModels":[]}`)
	defer closeFn()

	ms, err := p.Models(context.Background(), credForTest())
	if err != nil {
		t.Fatalf("空目录时应降级，不是报错: %v", err)
	}
	if *hits == 0 {
		t.Fatal("没有真的请求上游")
	}
	if len(ms) == 0 {
		t.Fatal("★ 空目录直接返回 0 个 —— 客户端会以为这个上游没有模型")
	}
	if len(ms) != len(KnownModels()) {
		t.Errorf("应降级到快照（%d 个），实际 %d 个", len(KnownModels()), len(ms))
	}
}

// ---------------------------------------------------------------------------
// ④ 上游有、快照没有的模型 ⇒ 照样列出，且带保守默认值
// ---------------------------------------------------------------------------

func TestModelsIncludesUnknownUpstreamModels(t *testing.T) {
	body := `{"count":3,"builtinModels":[` + strings.Join([]string{
		upstreamModelJSON("kimi-k3", "Kimi-K3", true, true),
		upstreamModelJSON("GLM-5.2", "GLM-5.2", true, true),
		upstreamModelJSON("brand-new-2030", "Brand New", true, true),
	}, ",") + `]}`
	p, _, closeFn := newModelsProvider(t, body)
	defer closeFn()

	ms, err := p.Models(context.Background(), credForTest())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if !hasModel(ms, "brand-new-2030") {
		t.Fatal("★ 上游有、快照没有的模型必须照样列出（否则用户永远等不到新模型）")
	}
	// 未知名：保守默认（输出上限必须是实测的 65536，不能是 0 —— 0 会被当成"无上限"）
	for _, m := range ms {
		if m.ID == "brand-new-2030" {
			if m.MaxOutputTokens != int(defaultLearnedMaxOutput) {
				t.Errorf("未知名应带保守输出上限 %d，实际 %d", defaultLearnedMaxOutput, m.MaxOutputTokens)
			}
			if m.ContextWindow != int(defaultLearnedContextWindow) {
				t.Errorf("未知名应带保守上下文 %d，实际 %d", defaultLearnedContextWindow, m.ContextWindow)
			}
		}
		// 已知名：用快照的实测值（不是默认值）
		if m.ID == "GLM-5.2" && m.MaxOutputTokens != 65536 {
			t.Errorf("GLM-5.2 应取快照实测的 65536，实际 %d", m.MaxOutputTokens)
		}
	}
	// 新模型必须进"裁剪表"，否则客户端的大 max_tokens 会以"上游 400"的形态暴露
	if got := MaxTokensTable()["brand-new-2030"]; got != int(defaultLearnedMaxOutput) {
		t.Errorf("新模型应被登记进 MaxTokensTable（裁剪用），实际 %d", got)
	}
}

// ---------------------------------------------------------------------------
// ⑤ 额度耗尽的过滤不能被上游路径绕过
// ---------------------------------------------------------------------------

func TestUpstreamModelsStillFilterExhausted(t *testing.T) {
	const model = "kimi-k3"
	body := `{"count":2,"builtinModels":[` + strings.Join([]string{
		upstreamModelJSON(model, "Kimi-K3", true, true),
		upstreamModelJSON("GLM-5.2", "GLM-5.2", true, true),
	}, ",") + `]}`
	p, _, closeFn := newModelsProvider(t, body)
	defer closeFn()
	defer ClearQuota(model)

	base, err := p.Models(context.Background(), credForTest())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if !hasModel(base, model) {
		t.Fatalf("基线应包含 %s", model)
	}

	MarkQuotaExhausted(model, "insufficient quota")
	got, err := p.Models(context.Background(), credForTest())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if hasModel(got, model) {
		t.Error("★ 额度耗尽的模型仍被列出 —— 客户端会选中它然后必然失败")
	}
	if len(got) != len(base)-1 {
		t.Errorf("应恰好少 1 个：基线 %d，实际 %d", len(base), len(got))
	}
}

// ---------------------------------------------------------------------------
// 附带：上游标记为未开通（enable=false）的不列
// ---------------------------------------------------------------------------

func TestUpstreamDisabledModelNotListed(t *testing.T) {
	body := `{"count":2,"builtinModels":[` + strings.Join([]string{
		upstreamModelJSON("GLM-5.2", "GLM-5.2", true, true),
		upstreamModelJSON("disabled-model", "Disabled", false, true),
	}, ",") + `]}`
	p, _, closeFn := newModelsProvider(t, body)
	defer closeFn()

	ms, err := p.Models(context.Background(), credForTest())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if hasModel(ms, "disabled-model") {
		t.Error("上游标 enable=false 的模型不该列出（列了就是必然失败的一条）")
	}
}
