package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/wire"
)

// toolLoopParams 一次工具循环的入参。
type toolLoopParams struct {
	ext        toolCapable
	providerID string
	uid        string
	body       []byte
	// stream 客户端是否要事件流（决定最后的发射形态）。
	stream bool
}

// runToolLoop 驱动一次完整的工具调用循环并写出响应。
//
// 返回值语义：
//
//	(done=true,  err=nil)  已写出响应（含状态码），调用方直接收工
//	(done=false, err≠nil)  本轮没跑成，调用方按普通失败处理（分类 + 换号）
//
// # 为什么用 (done, err) 两值而不是单 error
//
// "写过了响应"与"失败了"是**互斥**的两种收场，且调用方的动作完全不同：
// 前者必须立刻 return（再写一次会 "superfluous WriteHeader"），
// 后者要继续换号。用一个 error 表达不了三态，用一个 bool 又丢了原因。
func (h *Handler) runToolLoop(w http.ResponseWriter, p toolLoopParams) (bool, error) {
	tools := h.chatToolsOf(p.providerID)
	if len(tools) == 0 {
		return false, nil
	}

	body, err := injectTools(p.body, tools)
	if err != nil {
		// 注入失败只可能是请求体不是 JSON —— 但那在更早的 peek 阶段就该发现。
		// 走到这里说明 body 在中间被改坏了：明确报错，不要静默降级成
		// "工具没生效"（那会让用户以为功能坏了却查不出原因）。
		return false, fmt.Errorf("注入工具失败: %w", err)
	}

	collected := make([]gateway.Artifact, 0)
	var final map[string]any

	for round := 1; round <= maxToolRounds; round++ {
		// 每轮都问上游（非流式 —— 循环内部需要完整响应才能判断有没有 tool_calls）。
		raw, status, herr := h.chatOnceNonStream(p.uid, body)
		if herr != nil {
			return false, herr
		}
		if status >= 400 {
			// 上游拒绝：把它的错误交给调用方走普通失败路径（分类 + 换号）。
			// 不在这里自己回错 —— 那会绕过既有的分类与重试逻辑。
			return false, &chatError{
				Kind:   h.classifyErr(p.providerID, status, raw),
				Status: status,
				Msg:    clipForLog(string(raw)),
			}
		}

		resp, perr := wire.Aggregate(strings.NewReader(string(raw)))
		if perr != nil {
			// 流内错误信封：与普通路径同一条处理（分类 + 换号）。
			var inband *wire.InBandError
			if errors.As(perr, &inband) {
				return false, &chatError{
					Kind:   h.classifyErr(p.providerID, http.StatusOK, []byte(inband.Body)),
					Status: http.StatusOK,
					Msg:    inband.Message,
				}
			}
			return false, fmt.Errorf("解析上游响应失败: %w", perr)
		}

		msg := firstMessage(resp)
		calls := parseToolCalls(msg)
		if len(calls) == 0 {
			// 模型不再调工具 → 这就是最终回复。
			final = resp
			break
		}

		if round == maxToolRounds {
			// 到顶了还在调工具：不再执行（避免无限烧积分），
			// 把已完成的结果交给模型做最后一次收尾 —— 且**不带工具**，
			// 它就没有再调的余地，只会说话。
			//
			// 比"直接报错"好：用户至少拿到已经生成好的东西 + 一句解释。
			log.Printf("chat tools: 达到轮次上限 %d，强制收尾 uid=%s provider=%s",
				maxToolRounds, p.uid, p.providerID)
			final = resp
			break
		}

		// 执行本轮的所有工具调用。
		results := make([]toolExecResult, 0, len(calls))
		for _, call := range calls {
			results = append(results, h.execOneTool(p, call.ID, call.Name, call.Args))
		}
		// 收集产物（图片/搜索）。
		//
		// ⚠ markdown 要**塞进 artifact 里**（键 `markdown`）——
		// `markdownForArtifacts` 就是按那个键取出来追加到回复末尾的。
		//
		// 我第一版把 `res.markdown` 存进了 toolExecResult 却**没往下传**，
		// 于是图片 URL 只出现在结构化字段里、回复正文一句图都没有 ——
		// 用户要求的是"写进回复内容（markdown）"，那条要求就落空了。
		// 一个字段被赋值但没人读，是这类漏接线的典型形态（编译器不会报）。
		for _, r := range results {
			arts := r.artifacts
			if len(arts) == 0 && r.markdown == "" {
				continue
			}
			if len(arts) == 0 {
				// 只有 markdown（没有结构化产物）：造一条只带 markdown 的记录。
				arts = []gateway.Artifact{{"type": "note"}}
			}
			if r.markdown != "" {
				// 复制一份再挂，避免改到上游返回的 map（那是它的对象）。
				withMD := make([]gateway.Artifact, 0, len(arts))
				for i, a := range arts {
					if i == 0 {
						merged := make(gateway.Artifact, len(a)+1)
						for k, v := range a {
							merged[k] = v
						}
						merged["markdown"] = r.markdown
						withMD = append(withMD, merged)
						continue
					}
					withMD = append(withMD, a)
				}
				arts = withMD
			}
			collected = append(collected, arts...)
		}

		// 把 tool_calls + 结果回喂，进入下一轮。
		var obj map[string]any
		if jerr := json.Unmarshal(body, &obj); jerr != nil {
			return false, fmt.Errorf("工具循环内解析请求体失败: %w", jerr)
		}
		if aerr := appendToolMessages(obj, msg, results); aerr != nil {
			return false, aerr
		}
		body, err = json.Marshal(obj)
		if err != nil {
			return false, fmt.Errorf("工具循环内重编码请求体失败: %w", err)
		}
	}

	if final == nil {
		return false, fmt.Errorf("工具循环未能取得最终回复")
	}

	if aerr := appendArtifactsToResponse(final, collected); aerr != nil {
		return false, aerr
	}

	// 按客户端要的形态发射。
	if p.stream {
		writeAsSSE(w, final)
	} else {
		writeJSON(w, http.StatusOK, final)
	}
	return true, nil
}

// execOneTool 执行一次工具调用，把它包成 toolExecResult。
//
// # 为什么所有失败都在这里转成"给模型看的文本"
//
// 工具失败（上游 5xx、积分不足、参数畸形）**不该**让整个请求失败 ——
// 那会把"这次图没生成成功"报成"网关挂了"。正确形态是回给模型，
// 由它向用户解释（它知道上下文，比网关的错误页友好）。
//
// 只有 err（未知工具名 = 核心传错了名字）才是真错误：那是本仓的 bug。
func (h *Handler) execOneTool(p toolLoopParams, callID, name string, args json.RawMessage) (res toolExecResult) {
	res = toolExecResult{callID: callID, name: name}

	ctx, cancel := context.WithTimeout(context.Background(), toolExecTimeout)
	defer cancel()

	result, ok, err := p.ext.ExecuteChatTool(ctx, p.providerID, p.uid, name, args)
	if err != nil {
		// 未知工具名等**编程错误**：不把整个请求打成 500（那对用户毫无意义），
		// 而是把这件事告诉模型，让它换个方式回答。
		//
		// ⚠ 同时记日志：这是本仓的 bug（注入的工具名与执行的不符），
		// 只有日志能让人发现它。
		log.Printf("chat tools: 执行工具 %q 出错（可能是本仓 bug）uid=%s: %v", name, p.uid, err)
		res.content = fmt.Sprintf("工具 %s 执行出错：%v。请直接用你的能力回答用户。", name, err)
		return res
	}
	if !ok {
		log.Printf("chat tools: 上游 %s 无法执行工具 %q（未实现？）", p.providerID, name)
		res.content = fmt.Sprintf("工具 %s 不可用。请直接用你的能力回答用户。", name)
		return res
	}

	res.content = result.Content
	res.markdown = result.Markdown
	res.artifacts = result.Artifacts
	return res
}

// firstMessage 取响应里第一条 choice 的 message。
func firstMessage(resp map[string]any) map[string]any {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	c0, _ := choices[0].(map[string]any)
	if c0 == nil {
		return nil
	}
	msg, _ := c0["message"].(map[string]any)
	return msg
}

// clipForLog 把上游错误体裁短（日志用）。
func clipForLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 300 {
		return s
	}
	return s[:300] + "…"
}
