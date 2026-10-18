package minimax

import (
	"errors"
	"fmt"
	"testing"

	"workbuddy2api/internal/anthroconv"
	"workbuddy2api/internal/gateway"
)

// ⚠ 钉住「请求体转换失败 ⇒ 出口层不换号」这条链上的**机制**（2026-10-18 加）。
//
// # 为什么需要它
//
// 出口层此前把转换失败与"网络抖动"走同一条路：
//
//	if terr != nil { … fail(acct.UID); continue }   // 换号，再来一轮
//
// ⚠ 而换号对转换失败**完全无效**：转换取决于**请求体本身**，每个账号背后是同一套
// 转换代码 ⇒ 白试 N 个号，最后还报成"所有账号不可用"（把本地错误误报成池子故障）。
// ⇒ 现在 minimax 的两处转换失败都用 `errors.Join(err, gateway.ErrConvertFailed)`
//   包一层哨兵，出口层用 `errors.Is` 认它，认到就直接回 400、不换号。
//
// ⚠ 这条测试穿过**真实**的转换函数（不是造一个假错误），
//   所以"坏 body 会不会失败"这个前提也一并被钉住了 ——
//   万一哪天 anthroconv 变得宽容（比如自动修复坏 JSON），这条测试会先红，
//   而不是让哨兵悄悄失去意义。
func TestConvertFailureCarriesSentinel(t *testing.T) {
	// 前提：坏 JSON 必须真的让转换失败（否则下面的断言会假绿）
	_, cerr := anthroconv.OpenAIToAnthropic([]byte("{"))
	if cerr == nil {
		t.Fatal("坏 JSON 本该转换失败 —— 前提变了，这条测试的结论不再可信")
	}

	// ⚠ 与 provider.go / client.go 里**同一句**包装（保持同步：改了那里也要改这里）
	wrapped := fmt.Errorf("minimax: 请求体转换失败: %w",
		errors.Join(cerr, gateway.ErrConvertFailed))

	if !errors.Is(wrapped, gateway.ErrConvertFailed) {
		t.Fatal("包装后认不出 ErrConvertFailed ⇒ 出口层会退化成「换号重试」，白试 N 个账号")
	}
	// ⚠ 同时必须仍能看出**原始**原因（Operator 排障要靠它，见 handler 里
	//   "刻意不动 lastErr：那里存着上游原文" 那段的同一取舍）
	if !errors.Is(wrapped, cerr) {
		t.Error("包装后丢了原始错误 ⇒ 排障时看不到「到底哪一步转不出来」")
	}
}
