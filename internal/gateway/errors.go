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

// ErrCredentialRefreshRejected 上游**拒绝了这次续期凭据**，但这不等于账号失效。
//
// # 为什么必须与「凭据已废」分开（2026-11-14，依据官方客户端源码）
//
// 官方桌面端 sidecar 的续期入口（`tH`，偏移 @99948835）拿到
// `400 invalid_grant` 时的动作**不是**报"该重新登录"，而是：
//
//	① 先把这次拒绝表达成 `null`
//	   （`Pd3` 里 `if (status===400 && code==="invalid_grant") return null`）
//	② 调用方立刻**重读共享凭证库**，看同一 `sub` 有没有更新的 token
//	   （`if (!J) return o44(await _(), q, O())`）—— 有就采用，本次续期算成功
//	③ 只有重读也拿不到，才回一个**可重试**的错误：
//	   `Po` = 503 `OAUTH_REFRESH_UNAVAILABLE`，文案是「登录服务暂时不可用，
//	   请稍后重试；**当前登录信息已保留**」
//
// 关键差别在**计不计账**：`invalid_grant` 最常见的成因是 refresh_token
// **被另一个端轮换掉了**（网关与桌面端共用同一账号时必然发生，见
// docs/agent2api-蒸馏报告.md §L2），这类账号的凭据**下一秒就可能重新可用**。
// 一旦按"凭据废了"计一次失败、累计三次就永久禁用，**一次轮换竞态就能把
// 一个健康号打成死号** —— 2026-11-14 现场的两个号就是这么被禁掉的。
//
// ⚠ 出口层的动作：**可重试、不计入"账号层失效"、不因此禁用账号**。
// 各上游把自己的哨兵 `%w` 包装在这个标记上即可（见 phanthywork.ErrRefreshRejected）。
var ErrCredentialRefreshRejected = errors.New(
	"gateway: 上游拒绝了续期凭据（常见成因是被另一个端轮换掉了）——可重试，不应计为账号失效")
