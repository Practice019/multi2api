// payload_test.go 请求体的**字段清单守卫**。
//
// # 为什么需要它（2026-10-26 实测漏过）
//
// 第一版实现漏了 `temperature` 与 `env.today_date` 两处 —— 而它们**不影响跑通**
// （没带也成功生成过内容），所以任何"能不能用"的测试都抓不到。
// 漏字段的表现只有一种：**我发的东西和真实客户端不一样**，而"不一样"本身
// 在一个会被风控打量的通道里就是个变量。
//
// # 参照物是真实抓包，不是我抄的那份 JS
//
// 字段清单来自 register-machine 的 `llm-requests.jsonl`（mitmproxy 抓的
// 真实桌面端请求）·并逐条与 `sources/verdent/verdent-client.mjs` 对齐。
// 两者一致，所以下面这张表有两个独立来源互相印证。
//
// # ⚠ 为什么 tools / tool_choice **不在**表里（这是刻意的）
//
// 真实桌面端**确实**发它们（tools 是 94KB 的加密工具定义）—— 但那是 agent
// 形态才需要的。register-machine 的参考客户端**没有**发，而它在 9 个号上
// 都跑通了 ⇒ 缺 tools 不会导致任何问题。
// 抄一份 94KB 的工具定义进来只会让本上游更难维护，且我们本来就不做工具调用。
// 所以这张表钉的是"**与参考客户端**一致"，不是"与桌面端逐字节一致"。
package verdent

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// refPayloadFields 参考客户端请求体的**完整**顶层字段集。
//
// ⚠ 顺序无关（下面按字典序比）：改字段名会红，加/删字段也会红。
var refPayloadFields = []string{
	"agent_name",
	"channel",
	"context_window_tokens",
	"conv_id",
	"custom_trace_metadata_tmp",
	"custom_trace_tags_tmp",
	"effort",
	"encrypt",
	"env",
	"is_auto",
	"is_eco",
	"is_free",
	"is_limit_free",
	"max_tokens",
	"messages",
	"model",
	"model_catalog_version",
	"native_api",
	"react_id",
	"react_type",
	"session_id",
	"stream",
	"system",
	"temperature",
	"thinking",
}

// TestBuildPayloadFieldSetMatchesReference 字段集必须与参考客户端**逐字段相同**。
func TestBuildPayloadFieldSetMatchesReference(t *testing.T) {
	raw, err := buildPayload("m", "enc-system", "enc-messages")
	if err != nil {
		t.Fatalf("buildPayload 失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("产出的不是 JSON 对象: %v", err)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	want := append([]string(nil), refPayloadFields...)
	sort.Strings(want)

	// 先报"少了的" —— 那是最容易漏且最难发现的方向。
	missing := diffStrings(want, got)
	if len(missing) > 0 {
		t.Errorf("请求体少了这些字段（参考客户端有，我们没有）：%s\n"+
			"⚠ 漏字段不影响能不能跑通，所以「能不能用」的测试抓不到它 —— "+
			"只能靠这条清单。补进 buildPayload，或在两个来源都确认不需要后再从表里去掉",
			strings.Join(missing, ", "))
	}
	extra := diffStrings(got, want)
	if len(extra) > 0 {
		t.Errorf("请求体多了这些字段（参考客户端没有）：%s\n"+
			"多发的字段同样是「和真实客户端不一样」，要么补进表并注明依据，要么删掉",
			strings.Join(extra, ", "))
	}
}

// TestBuildPayloadKeyValues 关键字段的**取值**也要对（不只名字）。
func TestBuildPayloadKeyValues(t *testing.T) {
	raw, err := buildPayload("mymodel", "S", "M")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["channel"] != "deck" {
		t.Errorf("channel = %v，参考客户端是 \"deck\"", m["channel"])
	}
	// ★ react_type 必须是 "Main Agent"（带空格）—— 写成 "main" 会被上游拒。
	if m["react_type"] != "Main Agent" {
		t.Errorf("react_type = %v，必须是 \"Main Agent\"（带空格）", m["react_type"])
	}
	if m["temperature"] != float64(1) {
		t.Errorf("temperature = %v，参考客户端是 1", m["temperature"])
	}
	if m["max_tokens"] != float64(64000) {
		t.Errorf("max_tokens = %v，参考客户端是 64000", m["max_tokens"])
	}
	if m["model"] != "mymodel" {
		t.Errorf("model 没透传，实得 %v", m["model"])
	}
	// 加密字段必须是**密文**，不能是明文（上游会把拿到的当密文解）。
	if m["system"] != "S" || m["messages"] != "M" {
		t.Error("system/messages 没有原样带上（它们应当是已加密的 base64 串）")
	}
	// id 前缀：写成裸 uuid 会被上游当非法 id。
	for field, prefix := range map[string]string{
		"session_id": "session_",
		"conv_id":    "conv_",
		"react_id":   "model_agent_",
	} {
		v, _ := m[field].(string)
		if !strings.HasPrefix(v, prefix) {
			t.Errorf("%s = %q，必须以 %q 开头", field, v, prefix)
		}
	}
	// env.today_date 必须是一个日期（漏了它 = 与参考客户端不一致）。
	env, _ := m["env"].(map[string]any)
	if env == nil {
		t.Fatal("env 不是对象")
	}
	if env["today_date"] == nil || env["today_date"] == "" {
		t.Error("env.today_date 缺失 —— 参考客户端每次都带它")
	}
	for _, k := range []string{"platform", "os_version", "shell"} {
		if _, ok := env[k]; !ok {
			t.Errorf("env.%s 缺失", k)
		}
	}
}

// TestBuildPayloadFreshIDs 每次调用必须产生不同的 id（否则同一"会话"被复用）。
func TestBuildPayloadFreshIDs(t *testing.T) {
	a, err := buildPayload("m", "S", "M")
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildPayload("m", "S", "M")
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Error("两次调用产出完全相同的请求体 —— id 没重新生成")
	}
}

// diffStrings 返回 a 里有、b 里没有的元素（两边都已排序）。
func diffStrings(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	for _, s := range a {
		if !inB[s] {
			out = append(out, s)
		}
	}
	return out
}
