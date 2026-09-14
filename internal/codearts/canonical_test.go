// canonical_test.go 钉住 CodeArts 侧的模型名规范化。
//
// # 为什么需要这一组
//
// 本包的模型 ID 必须与服务端注册名**逐字符一致**（见 models.go 的 knownModels
// 注释：写错大小写会得到 `InferHub.002002009 The model is not registered`，
// 看起来像"账号没开通"）。
//
// 而客户端不保证遵守大小写。实测 `codearts/glm-5.2` 这种请求**不会**得到 4xx：
// 上游用 200 + 流内错误信封拒绝它，网关改造前会把它洗成一个空的成功响应。
// 规范化是从源头消除这类失败。
package codearts

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

// TestCanonicalModelCaseInsensitive 覆盖规范化映射本身。
func TestCanonicalModelCaseInsensitive(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		// 精确匹配（绝大多数请求走这条）
		{"GLM-5.2", "GLM-5.2", true},
		{"glm-5.2-sft-harmony", "glm-5.2-sft-harmony", true},
		{"openpangu-2.0-pro", "openpangu-2.0-pro", true},
		// 大小写不符 → 折叠匹配到规范名
		{"glm-5.2", "GLM-5.2", true},
		{"Glm-5.2", "GLM-5.2", true},
		{"GLM-5.2-SFT-HARMONY", "glm-5.2-sft-harmony", true},
		{"OpenPangu-2.0-Pro", "openpangu-2.0-pro", true},
		{"DEEPSEEK-V4-FLASH-0731", "deepseek-v4-flash-0731", true},
		// 不是本上游的模型 → 必须原样转发，让上游给出它自己的错误
		{"no-such-model", "", false},
		{"glm-6", "", false},
		{"", "", false},
	}

	for _, c := range cases {
		got, ok := CanonicalModel(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("CanonicalModel(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestKnownModelsNoCaseCollision 守住折叠匹配的**前提**。
//
// CanonicalModel 用 EqualFold 兜底，前提是表里不存在"只有大小写之差"的两个 ID。
// 一旦出现，折叠匹配会命中多个 → 返回 ok=false → 规范化静默失效
// （表现为"小写请求又开始报错"，而原因极难定位）。这个守卫把它变成一次响亮的失败。
func TestKnownModelsNoCaseCollision(t *testing.T) {
	seen := map[string]string{}
	for _, m := range AllModels() {
		key := strings.ToLower(m.ID)
		if prev, dup := seen[key]; dup {
			t.Errorf("%q 与 %q 只有大小写之差 —— EqualFold 折叠匹配会产生歧义，"+
				"必须改掉其中一个 ID", m.ID, prev)
		}
		seen[key] = m.ID
	}
}

// TestChannelForCaseInsensitive 确认通道判定也走规范化。
//
// ⚠ 这一条比"能跑"更重要：不做规范化时，"名字不对"与"通道不对"
// 会**同时**发生，而报错只体现后者（模型未注册），排查时极易误判成账号没开通。
func TestChannelForCaseInsensitive(t *testing.T) {
	if got := ChannelFor("glm-5.2"); got != ChannelDefault {
		t.Errorf("ChannelFor(glm-5.2) = %v，期望 default（规范化后应命中 GLM-5.2）", got)
	}
	if got := ChannelFor("GLM-5.2"); got != ChannelDefault {
		t.Errorf("ChannelFor(GLM-5.2) = %v，期望 default", got)
	}
	if got := ChannelFor("DEEPSEEK-V4-FLASH-0731"); got != ChannelBenefit {
		t.Errorf("ChannelFor(DEEPSEEK-V4-FLASH-0731) = %v，期望 benefit", got)
	}
	// 未知模型仍回落 default（多送一个不被接受的头会触发更难诊断的
	// "unsupported model"）。
	if got := ChannelFor("some-unknown-model"); got != ChannelDefault {
		t.Errorf("未知模型的通道应回落 default，实际 %v", got)
	}
}

// TestChatStreamCanonicalizesModelOnTheWire 钉住**出站请求体**里的模型名被改写。
//
// 纯函数测试（上面的 TestCanonicalModelCaseInsensitive）只证明"算得对"，
// 证明不了"发得对" —— 规范名必须真的出现在发给上游的 body 里，
// 否则上游照旧按未注册模型拒绝。
func TestChatStreamCanonicalizesModelOnTheWire(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "chat/completions") {
			raw, _ := io.ReadAll(r.Body)
			var obj map[string]any
			if json.Unmarshal(raw, &obj) == nil {
				gotModel, _ = obj["model"].(string)
			}
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data:[DONE]\n\n")
	}))
	defer srv.Close()

	c := New()
	c.EngineBase = srv.URL
	a := &Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"}

	body, _ := json.Marshal(map[string]any{
		"model":    "glm-5.2",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	rc, _, _, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if rc != nil {
		rc.Close()
	}

	if gotModel != "GLM-5.2" {
		t.Errorf("出站 model=%q，期望规范名 %q\n"+
			"★ 未规范化 → 上游按未注册模型拒绝（200 + 流内错误信封），\n"+
			"  客户端只会看到一个空回复。", gotModel, "GLM-5.2")
	}
}

// TestMaxTokensTableClampsOverLimit 钉住"超限 max_tokens 必须被裁剪到表值"。
//
// 这条是 `InferHub.001001005.400 The request param is invalid` 的第一道闸门：
// 客户端（尤其 DSH）会发远超模型上限的 max_tokens，裁剪层必须在出站前收窄。
func TestMaxTokensTableClampsOverLimit(t *testing.T) {
	limits := MaxTokensTable()
	limit, ok := limits["GLM-5.2"]
	if !ok || limit <= 0 {
		t.Fatalf("MaxTokensTable 未给出 GLM-5.2 的上限：%v", limits)
	}

	body := []byte(`{"model":"GLM-5.2","messages":[],"max_tokens":384000}`)
	out := upstream.PrepareBodyOptWithLimits(body, false, nil, limits)

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("改写后不是合法 JSON: %v (%s)", err, out)
	}
	got, _ := obj["max_tokens"].(float64)
	if int(got) != limit {
		t.Errorf("max_tokens=%v，期望被裁剪到 %d（超限值必须收窄，否则上游直接 400）", got, limit)
	}
}

// TestModelMaxTokensMatchMeasuredCeiling 把表值与**实测上限**绑在一起。
//
// # 为什么需要它（这条是真踩过的）
//
// knownModels 里 GLM-5.2 原写 MaxTokens=131072。而实测（见
// probe_real_test.go 的 TestProbeMaxTokensCeiling）：
//
//	max_tokens=65536  → 接受
//	max_tokens=131072 → 被拒（InferHub.001001005.400）
//
// 表值比真实上限高一倍，后果是**裁剪层把超限值裁到了一个依然非法的值** ——
// 表现与完全不裁剪一模一样：外部客户端恒 503，而裁剪日志连一条都不打
// （因为 131072 <= 131072，判据认为"没超限"）。
//
// 所以表值必须按实测钉住。改动这里之前请先跑 probe 重新测。
func TestModelMaxTokensMatchMeasuredCeiling(t *testing.T) {
	// 实测确认可用的最大 max_tokens（默认通道 4 个模型，
	// 见 probe_real_test.go 的 TestProbeMaxTokensCeiling：
	// 65536 全部接受，70000 起被 InferHub.001001005.400 拒绝）。
	const measured = 65536

	limits := MaxTokensTable()
	for _, id := range []string{
		"GLM-5.2", "glm-5.2-sft-harmony",
		"openpangu-2.0-pro", "openpangu-2.0-flash",
	} {
		got, ok := limits[id]
		if !ok || got <= 0 {
			t.Errorf("%s 在 MaxTokensTable 里没有上限 —— 超限的 max_tokens 不会被裁剪", id)
			continue
		}
		if got > measured {
			t.Errorf("%s 的表值 %d > 实测上限 %d\n"+
				"★ 表值高于实测会让裁剪**（裁到仍非法的值）**，"+
				"表现为外部客户端恒 503 而裁剪日志一条都不打。", id, got, measured)
		}
	}
}

// TestChatStreamKeepsUnknownModelVerbatim 是对照组：
// 非本上游的模型名**不得**被悄悄替换。
//
// 替换成某个默认模型会把"客户端写错了模型名"变成"用别的模型跑了一次" ——
// 那是比报错更糟的行为。
func TestChatStreamKeepsUnknownModelVerbatim(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "chat/completions") {
			raw, _ := io.ReadAll(r.Body)
			var obj map[string]any
			if json.Unmarshal(raw, &obj) == nil {
				gotModel, _ = obj["model"].(string)
			}
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data:[DONE]\n\n")
	}))
	defer srv.Close()

	c := New()
	c.EngineBase = srv.URL
	a := &Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"}

	body, _ := json.Marshal(map[string]any{
		"model":    "some-unknown-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	rc, _, _, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if rc != nil {
		rc.Close()
	}

	if gotModel != "some-unknown-model" {
		t.Errorf("未知模型被改成了 %q —— 必须原样转发让上游报错", gotModel)
	}
}
