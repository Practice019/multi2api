// sanitize_gaps_test.go 脱敏层四处盲区的回归测试（借鉴 workbuddy2api-panel 补上）。
//
// # 为什么这四条各自需要一条测试
//
// 它们的共同失败模式是**静默漏出**：请求照发、上游回 400 code=11128、
// 网关把它当业务错误换号重试，用户只看到"这个客户端时好时坏"。
// 没有任何一条会报错，也没有任何一条会在日志里留下"指纹漏了"的痕迹。
//
// 因此每条盲区都要有一个"**输入含指纹 → 输出不含**"的断言，
// 而 11128 那条还要额外断言**预检闸门确实打开**（见 TestEveryRewriteReachable）。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	// 桌面版（Agent SDK）身份句：逗号接继，与 CLI 版的句号收尾不同。
	ccIdentityDesktop = "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	codexIdentity     = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant."
	ccFeedback        = "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
)

// TestDesktopIdentitySentenceRewritten 盲区②：不带结尾标点的匹配串必须同时覆盖逗号形态。
//
// 反向判别力：若匹配串写成带句号的整句（改造前的形态），
// 本用例的输入不含 "…for Claude." 子串 → 一条规则都不中 → 原样返回 → 失败。
func TestDesktopIdentitySentenceRewritten(t *testing.T) {
	out := sanitizeText(ccIdentityDesktop)
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("桌面版身份句未被改写（指纹会原样发上游 → 400 code=11128）：%q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude") {
		t.Errorf("改写缺失：%q", out)
	}
	// 替换串不带标点 ⇒ 原有的逗号必须原样保留，后继内容不被吃掉。
	if !strings.Contains(out, "running within the Claude Agent SDK.") {
		t.Errorf("改写吃掉了后继内容：%q", out)
	}
}

// TestCLIIdentityStillRewritten 回归：去掉标点后 CLI 形态（句号收尾）仍要被改写。
func TestCLIIdentityStillRewritten(t *testing.T) {
	out := sanitizeText(ccIdentity) // sanitize_test.go 里的 CLI 形态（句号收尾）
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("CLI 身份句未被改写：%q", out)
	}
}

// TestCodexIdentityRewritten 盲区③：Codex CLI 的模板句。
func TestCodexIdentityRewritten(t *testing.T) {
	out := sanitizeText(codexIdentity)
	if strings.Contains(out, "running in the Codex CLI,") {
		t.Errorf("Codex 身份句未被改写：%q", out)
	}
	if !strings.Contains(out, "running in the Codex CLI tool,") {
		t.Errorf("改写缺失：%q", out)
	}
}

// TestFeedbackSentenceRewritten 盲区③附：带 Anthropic 仓库链接的反馈句必须整句改。
//
// 实测只留链接或只留半边均不拦——上游按**整句**匹配，所以必须整句替换。
func TestFeedbackSentenceRewritten(t *testing.T) {
	out := sanitizeText(ccFeedback)
	if strings.Contains(out, "To give feedback") {
		t.Errorf("反馈句未被改写：%q", out)
	}
	if !strings.Contains(out, "To provide feedback") {
		t.Errorf("改写缺失：%q", out)
	}
	// 链接本身保留（语义不变，只是动词换了个词）。
	if !strings.Contains(out, "https://github.com/anthropics/claude-code/issues") {
		t.Errorf("链接被误删：%q", out)
	}
}

// TestRaw11128Neutralized 盲区④：裸数字 11128 必须被打断。
//
// 上游的反探测口径是"请求体里出现这串数字就整单拦截"，与该数字的上下文无关。
// 因此断言的是**输出里不再有连续 11128**，而不是"某个句子被改了"。
func TestRaw11128Neutralized(t *testing.T) {
	cases := []string{
		"code=11128",
		"裸的 11128",
		"错误码 11128 是什么意思",
		"Code=11128: unmarshal failed",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			out := sanitizeText(in)
			if strings.Contains(out, "11128") {
				t.Errorf("裸数字 11128 未被打断：%q -> %q", in, out)
			}
			if !strings.Contains(out, "11-128") {
				t.Errorf("应改写为 11-128（保留可读性与指代）：%q", out)
			}
		})
	}
}

// TestAdjacentErrorCodesNotTouched 反向：相邻错误码不得被误伤。
//
// 上游只拦 11128，11148 / 11101 / 11115 / 99999 都放行。
// 改写范围如果放宽（例如改成拦 "1112"），会把正常回显上游错误码的请求改坏。
func TestAdjacentErrorCodesNotTouched(t *testing.T) {
	for _, in := range []string{"11148", "11101", "11115", "99999"} {
		if out := sanitizeText(in); out != in {
			t.Errorf("%s 不该被改写：%q -> %q", in, in, out)
		}
	}
}

// TestEveryRewriteReachable 每条改写规则都必须能通过预检闸门。
//
// # 这是本文件最重要的一条（判别力说明）
//
// sanitizeText 的第一句是 `if !hasFingerprint(text) { return text }`。
// 预检串表 sanitizeFeatures 与改写表 sanitizeRewrites 是**两张独立的表**，
// 加规则时漏补预检串，结果是：
//
//	规则确实写了、编译通过、直接调 sanitizeText 的单测也过
//	但线上任何真正含该指纹的请求都会在 hasFingerprint 处短路，规则永不触发
//
// 本用例对每条 rewrite 的**源串**断言 hasFingerprint(source) == true，
// 从而把"两张表必须同步"这件事变成一条可执行的约束。
func TestEveryRewriteReachable(t *testing.T) {
	if len(sanitizeRewrites) == 0 {
		t.Fatal("改写表为空，本用例失去意义")
	}
	for _, rw := range sanitizeRewrites {
		src, dst := rw[0], rw[1]
		if !hasFingerprint(src) {
			t.Errorf("改写规则 %q 无法通过预检 —— sanitizeFeatures 缺对应项；\n"+
				"  这条规则**永远不会触发**（hasFingerprint 会先短路返回）", src)
		}
		// 顺带钉住：改写必须真的改变文本（同串替换是无效规则）。
		if src == dst {
			t.Errorf("改写规则 %q 的替换串与源串相同，等于没改", src)
		}
	}
}

// TestToolCallsSanitizedWhenContentNull 盲区①：content 缺失/null 时 tool_calls 仍必须被净化。
//
// # 这是四条盲区里后果最隐蔽的一条
//
// 只带 tool_calls 的 assistant 消息（工具调用轮的常见形态）可能没有 content 键。
// 改造前 sanitizeMessages 的形状是：
//
//	c, ok := m["content"]; if !ok { continue }
//
// 于是这类消息被整条跳过，tool_calls 里字符串化的 arguments
// （文件名、命令、要写入的内容——用户与 CLI 都会把任意文本放进去）
// 完全不被净化，原样发上游。
//
// ⚠ 两种"没有 content"的形态后果不同，必须分开覆盖：
//
//	content = null  → 键存在，ok=true → 旧代码**不会**跳过（这条一直是对的）
//	content 缺失    → ok=false       → 旧代码跳过，**这才是漏网那条**
//
// 保留 null 子用例是为了防"修缺失时把 null 分支改坏"。
//
// 反向判别力：把 sanitizeMessages 改回 content 缺失即 continue，
// 「content缺失」子用例必然失败（已实测验证）。
func TestToolCallsSanitizedWhenContentNull(t *testing.T) {
	// 两种"没有 content"的形态都要覆盖：
	//   null  —— 显式 null（键存在）
	//   缺失  —— 字段干脆不存在（真正的漏网形态）
	nullContent := map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{
			map[string]any{
				"id":   "call_1",
				"type": "function",
				"function": map[string]any{
					"name": "write_file",
					// 指纹藏在字符串化的 JSON 参数里（这正是 arguments 的真实形态）
					"arguments": `{"path":"/tmp/a.txt","content":"` + ccIdentity + `"}`,
				},
			},
		},
	}
	missingContent := map[string]any{
		"role": "assistant",
		"tool_calls": []any{
			map[string]any{
				"id":   "call_2",
				"type": "function",
				"function": map[string]any{
					"name":      "run_shell",
					"arguments": `{"cmd":"echo 11128"}`,
				},
			},
		},
	}

	for name, msg := range map[string]map[string]any{
		"content=null": nullContent,
		"content缺失":    missingContent,
	} {
		t.Run(name, func(t *testing.T) {
			msgs := []any{msg, map[string]any{"role": "user", "content": "go on"}}
			if !sanitizeMessages(msgs) {
				t.Fatalf("tool_calls 里的指纹未被净化（content 缺失时整条消息被跳过）")
			}
			args, _ := msgs[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
			if strings.Contains(args, "official CLI for Claude") || strings.Contains(args, "11128") {
				t.Errorf("arguments 里仍残留指纹：%q", args)
			}
		})
	}
}

// TestToolCallsSanitizedThroughFullPipeline 盲区①的端到端形态。
//
// 单测 sanitizeMessages 只能证明"函数本身对"；这一条走 complete PrepareBodyOpt，
// 证明"出站线上真的干净"——包括 sanitize 开关、序列化往返都不破坏它。
func TestToolCallsSanitizedThroughFullPipeline(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "write_file",
							"arguments": `{"content":"` + ccIdentity + ` 11128"}`,
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out := PrepareBodyOpt(body, true)
	if strings.Contains(string(out), "11128") || strings.Contains(string(out), "official CLI for Claude") {
		t.Errorf("出站 wire body 仍含指纹：%s", out)
	}
	// 开关关上时必须原样保留（证明 sanitize=true 是它被清掉的**原因**）。
	off := PrepareBodyOpt(body, false)
	if !strings.Contains(string(off), "11128") {
		t.Error("sanitize=false 时应保留指纹；保不住说明清理不是 sanitize 干的")
	}
}

// TestToolCallsImageAndNonTextPartsUntouched 净化 tool_calls 不得动其他字段。
//
// 防止"为了修 arguments 顺手把 id/name/type 也改了"——
// 那会让上游无法把 tool_calls 与后续 tool 结果消息对应起来。
func TestToolCallsImageAndNonTextPartsUntouched(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				map[string]any{
					"id":   "call_keep_me",
					"type": "function",
					"function": map[string]any{
						"name":      "write_file",
						"arguments": `{"content":"` + ccIdentity + `"}`,
					},
				},
			},
		},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("expected change")
	}
	call := msgs[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["id"] != "call_keep_me" {
		t.Errorf("tool_call id 被改动：%v", call["id"])
	}
	if call["type"] != "function" {
		t.Errorf("tool_call type 被改动：%v", call["type"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "write_file" {
		t.Errorf("function.name 被改动：%v", fn["name"])
	}
}
