// chattool_router.go 装配层为出口层提供的**对话工具**适配器。
//
// # 为什么单独一个文件
//
// multiprovider.go 已经是 600+ 行、承载了 registryRouter 的全部出站能力。
// 本组方法（工具定义 / 工具执行）是**本轮新增的一类能力**，
// 与既有的 chat/quota/refresh 各占一段的写法一致 ——
// 单开文件让"这次加了什么"在 diff 里一眼可见，也避免与后续改动冲突。
//
// # 三处都靠接口断言发现可选能力
//
// 出口层不认识任何具体上游，只问"你能不能给我工具 / 能不能执行它"。
// 没实现的上游在这三处都被静默跳过 —— 行为与改造前逐字节一致。
package main

import (
	"context"
	"encoding/json"

	"workbuddy2api/internal/gateway"
)

// ChatTools 返回某上游可注入对话的工具。
//
// # 为什么作用域天然是"本上游的"（用户的要求）
//
// 用户原话：「我希望只能在使用 Loomy 上游任意一个模型时，启用该上游对应的工具。」
//
// 出口层拿的是**被选中的那个上游 ID**，本方法只问那一个 Provider。
// 所以"用 loomy 的模型对话 → 只有 loomy 的工具"是靠**路由**成立的，
// 不是靠核心里的 `if provider == "loomy"` —— 加新上游时核心零改动。
//
// ok=false 表示该上游没实现 ChatToolExt（不注入任何工具，
// 其对话行为与改造前逐字节一致）。
func (r registryRouter) ChatTools(id string) ([]gateway.ChatTool, bool) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return nil, false
	}
	ext, ok := gateway.ExtOf[gateway.ChatToolExt](pv)
	if !ok {
		return nil, false
	}
	return ext.ChatTools(), true
}

// ExecuteChatTool 代出口层执行一次工具调用。
//
// 三值语义（ok 报告"该上游能否执行这个工具"）：
//
//	ok=false   未注册 / 没实现 ChatToolExt → 出口层据此判定"不支持"
//	ok=true    上游执行了（err 可能非 nil，见下）
//
// ⚠ err 只在**调用方用法错误**（未知工具名）时非 nil —— 那是核心的 bug，
// 必须显式暴露而不是伪装成"工具执行失败"。
// 工具的**业务失败**（上游 5xx、积分不足）由上游写进
// ChatToolResult.Content（OK=false），让**模型**去向用户解释 ——
// 那比网关回一个 502 友好得多，而且模型知道上下文。
func (r registryRouter) ExecuteChatTool(ctx context.Context, id, uid, name string, args json.RawMessage) (gateway.ChatToolResult, bool, error) {
	pv, ok := r.reg.Get(id)
	if !ok {
		return gateway.ChatToolResult{}, false, nil
	}
	ext, ok := gateway.ExtOf[gateway.ChatToolExt](pv)
	if !ok {
		return gateway.ChatToolResult{}, false, nil
	}
	res, err := ext.ExecuteChatTool(ctx, uid, name, args)
	return res, true, err
}
