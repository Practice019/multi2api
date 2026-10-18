// Package gateway 的错误哨兵（本地错误）。
//
// ⚠ 这里只放**本地产生**的错误哨兵 —— 上游返回的错误走 ErrorClassifier
// （按 status + body 分类，见 errorclassifier.go），两者不是一回事。
package gateway

import "errors"

// ErrConvertFailed 本地**请求体转换失败**（把 OpenAI 体转成上游协议时出错）。
//
// # 为什么需要它（2026-10-18 加）
//
// 这类错误在出口层此前与"网络抖动"**走同一条路**：
//
//	if terr != nil { … fail(acct.UID); continue }   // 换号，再来一轮
//
// ⚠ 而换号对它是**完全无效**的：转换失败取决于**请求体本身**，
// 每个账号背后是同一套转换代码 ⇒ 换 N 个号就是白跑 N 次，
// 最后还把结果报成"所有账号不可用"，把一个本地错误误报成池子故障。
//
// 这正是 new-api 用 `types.ErrOptionWithSkipRetry()` 显式声明的那件事
// （见 relay/convert_request_error.go：转换失败一律带 SkipRetry，并配专门错误码）。
//
// ⚠ 出口层的动作：**不换号、不惩罚账号**，直接把 400 返回给客户端
// （请求体是客户端给的，它该知道这份 body 转不出来）。
var ErrConvertFailed = errors.New("gateway: 请求体转换失败（本地错误，换号无意义）")
