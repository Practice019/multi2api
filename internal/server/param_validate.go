// param_validate.go 对话参数的**最小必要**校验。
//
// # 为什么这里几乎什么都不校验（踩过才定的口径）
//
// 我第一版按 OpenAI 规范做了完整校验（messages 非空、role 枚举、
// temperature/top_p/max_tokens 范围），结果 **12 个既有测试立刻变红**：
//
//	它们发的 body 是 {"model":"x","messages":[]} 或不带 messages，
//	测的是路由与重试 —— 与对话内容无关。
//
// 那不只是测试写法问题。它暴露了一个判断：**网关不是参数校验器**。
//
// # 对标 new-api 的做法
//
// new-api（本仓的对标项目）在 relay 层同样**不做**这套校验：
//
//	· 它把 body 原样转给上游，只有在上游报错后才做错误映射
//	· 唯一的本地校验是「model 是否被该令牌允许」（权限），不是参数合法性
//	· temperature / top_p / max_tokens 全部直通 —— 各家取值范围本就不同
//	  （有些上游接受 temperature 到 2，有些只到 1；max_tokens 有的叫
//	   max_completion_tokens。在网关层统一裁剪等于替上游做它自己的判断）
//
// # 唯一必须本地拦的：会污染账号池的错误
//
// 用户实测的那个 400（reasoning_effort=off 被 workbuddy 拒）暴露的真问题
// 不是"参数没校验"，而是"**参数错误被当成账号故障**"：
//
//	2 个健康号被打进冷却 → 后续正常请求也跟着 503
//
// 那个修在 handler 里（400 直接回 400，不 applyErrorPolicy），不在本文件。
//
// ⇒ 本文件只保留一条规则：**max_tokens 为负**。
//   原因不是"不符合规范"，而是它有实际危害：负数被上游当"无限制"，
//   客户端以为限了长度，计费失控。其余全部交给上游。
package server

import "fmt"

// validateChatParams 只拦**有实际危害**的参数。
//
// 返回空串表示通过。
func validateChatParams(maxTokens *int64) string {
	if maxTokens != nil && *maxTokens < 0 {
		return fmt.Sprintf("max_tokens 不能为负（收到 %d）：负值会被上游当成不限长度，"+
			"而你可能以为已经限制了输出。取消限制请省略该字段", *maxTokens)
	}
	return ""
}
