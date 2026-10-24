// security_leak_test.go 钉住用户实测的三个泄露。
//
// # 为什么这些必须是测试而不是注释
//
// 泄露是"看起来一切正常"的失效：没有报错、功能也对、测试也绿 ——
// 只是把后端是谁告诉了客户端。而且它们彼此能串起来：
//
//	403 列出白名单 → 拿到上游真名 → 用真名直调 → 响应回显真名
//
// 一步泄露就让整套 chatcmpl-gw-* 改写与 model 别名回填失去意义。
package server

import (
	"encoding/json"
	"testing"
)

// TestNormalizeReasoningEffortOff 出站前 off/none 必须被删除。
//
// # 用户实测的缺陷 1
//
// /v1/models 声明六模型都支持 off，而 workbuddy 系收到 off 回 400：
//
//	003 workbuddy       off -> 上游 400
//	004 workbuddy-intl  off -> 上游 400
//
// 而传 none（不在声明列表里）反而 200。真正表达关闭的是**省略该参数**。
//
// 400 的副作用比错误本身更坏：它被算作账号失败，把 2 个健康号打进冷却，
// 于是后续正常请求也跟着 503。
func TestNormalizeReasoningEffortOff(t *testing.T) {
	for _, v := range []string{"off", "none"} {
		body := []byte(`{"model":"x","reasoning_effort":"` + v + `"}`)
		out, changed := normalizeReasoningEffortForUpstream(body)
		if !changed {
			t.Fatalf("reasoning_effort=%s 应当被归一化", v)
		}
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("归一化后不是合法 JSON: %v", err)
		}
		if _, has := obj["reasoning_effort"]; has {
			t.Errorf("reasoning_effort=%s 应当被**删除**（省略才是各上游都接受的关闭写法）", v)
		}
		if obj["model"] != "x" {
			t.Error("其它字段不能被改动")
		}
	}
}

// TestNormalizeReasoningEffortKeepsOtherValues 其他档位必须原样保留。
//
// 只有 off/none 需要归一化 —— low/high/max 是各上游都认的合法值，
// 删掉它们等于把"我要更强推理"这个意图丢了。
func TestNormalizeReasoningEffortKeepsOtherValues(t *testing.T) {
	for _, v := range []string{"low", "high", "max"} {
		body := []byte(`{"model":"x","reasoning_effort":"` + v + `"}`)
		out, changed := normalizeReasoningEffortForUpstream(body)
		if changed {
			t.Errorf("reasoning_effort=%s 不该被改动", v)
		}
		if string(out) != string(body) {
			t.Errorf("未改动时必须**原样返回**（不重新编码，保证字节一致）")
		}
	}
}

// TestNormalizeReasoningEffortNoFieldNoReencode 没这个字段时字节必须原样。
//
// 既有客户端不带 reasoning_effort —— 那条路径必须与改造前逐字节一致。
// 重新序列化会改变键顺序与数字格式，那是无谓的回归面。
func TestNormalizeReasoningEffortNoFieldNoReencode(t *testing.T) {
	body := []byte(`{"model":"x","temperature":0.50,"seed":1}`)
	out, changed := normalizeReasoningEffortForUpstream(body)
	if changed {
		t.Error("没有 reasoning_effort 时不该判定为改动")
	}
	if string(out) != string(body) {
		t.Error("★ 无意改动却重编码了 —— 键顺序与 0.50 这类字面量会变")
	}
}

// TestValidateChatParamsOnlyBlocksNegativeMaxTokens 只拦 max_tokens 为负。
//
// # 为什么几乎不校验（踩过才定的口径）
//
// 我第一版按 OpenAI 规范做了完整校验，12 个既有测试立刻变红 ——
// 它们发 messages:[] 测路由与重试，与对话内容无关。
//
// 那不是测试写法问题，是个判断：**网关不是参数校验器**。对标项目
// new-api 在 relay 层同样只做「model 是否被令牌允许」，参数直通上游。
//
// 唯一保留的是 max_tokens 为负：它会被上游当"无限制"，
// 客户端以为限了长度 → 计费失控。这是有实际危害的，与合规无关。
func TestValidateChatParamsOnlyBlocksNegativeMaxTokens(t *testing.T) {
	neg := int64(-1)
	if validateChatParams(&neg) == "" {
		t.Error("★ max_tokens=-1 必须被拒 —— 它会被上游当无限制，计费失控")
	}
	zero := int64(0)
	if validateChatParams(&zero) != "" {
		t.Error("max_tokens=0 不拦（各家语义不同，交给上游）")
	}
	pos := int64(100)
	if validateChatParams(&pos) != "" {
		t.Error("正数必须放行")
	}
	if validateChatParams(nil) != "" {
		t.Error("省略该字段必须放行")
	}
}

// TestIsAliasForm 别名形式判定（数字前缀）。
func TestIsAliasForm(t *testing.T) {
	cases := map[string]bool{
		"006/deepseek-v4.1-flash-1017": true,
		"001/x":                        true,
		"workbuddy/x":                  false,
		"monkeycode/monkeycode-basic/deepseek-flash": false,
		"glm-5.2": false,
		"":        false,
	}
	for in, want := range cases {
		if got := isAliasForm(in); got != want {
			t.Errorf("isAliasForm(%q) = %v, want %v", in, got, want)
		}
	}
}
