package codearts

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChannelMapping 锁定 7 个模型与两个服务通道的对应关系。
//
// 这是实测得出的**互斥**分组，走错通道会报
// `InferHub.002002009 The model is not registered`，
// 看起来像"账号未开通"，实则只是少了/多了 maas_type 头。
//
// 之前把这个错误当成账号权限问题，导致 3 个本可用的模型被误标为不可用。
func TestChannelMapping(t *testing.T) {
	wantDefault := []string{
		"GLM-5.2",
		"glm-5.2-sft-harmony",
		"openpangu-2.0-pro",
		"openpangu-2.0-flash",
	}
	wantBenefit := []string{
		"deepseek-v4-flash-0731",
		"deepseek-v4-pro-0813",
		"glm-5.3-flash",
	}

	for _, id := range wantDefault {
		if got := ChannelFor(id); got != ChannelDefault {
			t.Errorf("ChannelFor(%q) = %v, 期望 default", id, got)
		}
	}
	for _, id := range wantBenefit {
		if got := ChannelFor(id); got != ChannelBenefit {
			t.Errorf("ChannelFor(%q) = %v, 期望 benefit", id, got)
		}
	}

	// 未知模型必须回落到 default：多送一个不被接受的头
	// 会触发更难诊断的 "unsupported model"。
	if got := ChannelFor("no-such-model"); got != ChannelDefault {
		t.Errorf("未知模型应回落 default，实际 %v", got)
	}
	if got := ChannelFor(""); got != ChannelDefault {
		t.Errorf("空模型名应回落 default，实际 %v", got)
	}
}

// TestChannelHeader 校验两个通道各自的头。
func TestChannelHeader(t *testing.T) {
	if h := ChannelDefault.Header(); h != nil {
		t.Errorf("默认通道不应附加任何头，实际 %v", h)
	}
	h := ChannelBenefit.Header()
	if h == nil {
		t.Fatal("benefit 通道必须附加 maas_type 头")
	}
	if h["maas_type"] != "benefit" {
		t.Errorf(`maas_type = %q, 期望 "benefit"`, h["maas_type"])
	}
}

// TestKnownModelsExposesEveryVerifiedModel 确认"对外暴露 == Verified"这条不变式。
//
// # 这个测试改过两次，第二次是**纠错**
//
// 第一版断言"7 个模型全部可用并全部暴露"，依据是"通道修对之后都能跑"。
// 实测推翻了它：3 个 benefit 通道模型报
// `InferHub.4004.200 benefit not found` / `InferHub.4291.200 insufficient quota`。
//
// 第二版据此把它们的 Verified 改成 false 并从 /v1/models 摘掉 ——
// **这一步是错的**。那些模型在本账号上**存在且可用**，只是 benefit 通道的
// **每日免费额度**当天用完，次日自动重置。
//
// 用静态的 Verified 表达"今天额度用完了"有三个后果：
//  1. 模型永久消失（其实第二天就回来）；
//  2. ProbeAllQuota 跳过未验证模型 → 永远发现不了它已恢复；
//  3. 没有请求 → 不触发 ClearQuota → "越藏越久"的死锁。
//
// 所以是第三版：**7 个全部 Verified**（静态事实），
// "此刻是否因额度不可用"由 quotaCache 表达 —— 见 quota.go 的
// QuotaState.ExhaustedUntil 与 Provider.Models 的过滤，那是**可逆**的。
func TestKnownModelsExposesEveryVerifiedModel(t *testing.T) {
	all := AllModels()
	if len(all) != 7 {
		t.Fatalf("模型表总数 = %d, 期望 7", len(all))
	}

	// 默认通道 4 个 + benefit 通道 3 个，全部是本账号上真实存在的模型。
	wantIDs := map[string]bool{
		"GLM-5.2":                true,
		"glm-5.2-sft-harmony":    true,
		"openpangu-2.0-pro":      true,
		"openpangu-2.0-flash":    true,
		"deepseek-v4-flash-0731": true,
		"deepseek-v4-pro-0813":   true,
		"glm-5.3-flash":          true,
	}

	for _, m := range all {
		if !wantIDs[m.ID] {
			t.Errorf("意外的模型 %q（模型表结构变了？）", m.ID)
			continue
		}
		if !m.Verified {
			t.Errorf("%s 未标记 Verified —— 它在本账号上确实存在；"+
				"不该用 Verified 表达『今天额度用完』这种**临时**状态", m.ID)
		}
	}

	// KnownModels 是**静态事实**层：不因额度而增减。
	if got := len(KnownModels()); got != len(all) {
		t.Errorf("KnownModels 暴露 %d 个但表里 %d 个 —— "+
			"额度过滤只应发生在 Provider.Models（那才是可逆的展示层）", got, len(all))
	}
	exposed := map[string]bool{}
	for _, m := range KnownModels() {
		exposed[m.ID] = true
	}
	for id := range wantIDs {
		if !exposed[id] {
			t.Errorf("%s 已验证却未暴露", id)
		}
	}
}

// TestModelOf 覆盖从请求体提取模型名。
func TestModelOf(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"model":"GLM-5.2","messages":[]}`, "GLM-5.2"},
		{`{"model":"deepseek-v4-pro-0813","stream":true}`, "deepseek-v4-pro-0813"},
		{`{"messages":[]}`, ""},
		{`not json`, ""},
		{`{}`, ""},
	}
	for _, c := range cases {
		if got := modelOf([]byte(c.body)); got != c.want {
			t.Errorf("modelOf(%q) = %q, 期望 %q", c.body, got, c.want)
		}
	}
}

// TestChannelHeadersReachTheWire 用 mock 上游验证通道头真的发出去了。
//
// 纯函数测试（上面的 TestChannelHeader）只能证明"算得对"，
// 证明不了"发得对" —— 头必须在**签名之前**并入 extra，
// 否则会因 SignedHeaders 与实际发送的头不一致而 401。
func TestChannelHeadersReachTheWire(t *testing.T) {
	for _, tc := range []struct {
		model      string
		wantHeader bool
	}{
		{"GLM-5.2", false},
		{"deepseek-v4-flash-0731", true},
	} {
		t.Run(tc.model, func(t *testing.T) {
			// 只关心 chat 请求的头。
			//
			// 注意不能取"最后一个请求" —— ChatStream 会先发 heartbeat(busy)、
			// 结束后再发 heartbeat(idle)，所以最后到达的是 idle 心跳，
			// 它不带 maas_type。必须按路径过滤。
			var chatHeaders map[string]string
			srv := newTestServer(t, func(path string, headers map[string]string) {
				if strings.Contains(path, "chat/completions") {
					chatHeaders = headers
				}
			})
			defer srv.Close()

			c := New()
			c.EngineBase = srv.URL
			a := &Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"}

			body, _ := json.Marshal(map[string]any{
				"model":    tc.model,
				"messages": []map[string]string{{"role": "user", "content": "hi"}},
			})
			rc, _, _, err := c.ChatStream(a, body)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			if rc != nil {
				rc.Close()
			}
			if chatHeaders == nil {
				t.Fatal("未捕获到 chat 请求")
			}

			got := chatHeaders["maas_type"]
			if tc.wantHeader && got != "benefit" {
				t.Errorf("应发 maas_type=benefit，实际 %q（头：%v）", got, keysOf(chatHeaders))
			}
			if !tc.wantHeader && got != "" {
				t.Errorf("默认通道不应发 maas_type，实际 %q", got)
			}
			// 签名必须存在（通道头并入后仍要正确签名）
			if !strings.HasPrefix(chatHeaders["authorization"], "SDK-HMAC-SHA256 Access=") {
				t.Errorf("Authorization 头异常: %q", chatHeaders["authorization"])
			}
			// 通道头必须被纳入 SignedHeaders
			if tc.wantHeader && !strings.Contains(chatHeaders["authorization"], "maas_type") {
				t.Errorf("maas_type 未纳入 SignedHeaders: %q", chatHeaders["authorization"])
			}
		})
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
