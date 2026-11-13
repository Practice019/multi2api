// channels_explain_ui_test.go 渠道页那段「路由诊断」的结构守卫（2026-11-13 T-909 收尾）。
//
// # 为什么用读源码判定
//
//	本仓没有可执行的 DOM 环境（执行式守卫只有邀请面板那处、靠 node 跑片段）。
//	这条判"诊断块存在 + 可折叠 + 从后端拿数 + 不自己拼字段"，属于**结构式**判据；
//	配合 `go test ./internal/server/`（接口侧那条只读断言）一起才完整。
package server

import (
	"strings"
	"testing"
)

func TestChannelsPageHasReadonlyExplainBlock(t *testing.T) {
	src := string(channelsUIPage)
	// ① 可折叠：沿用本页既有的 <details class="more"> + <summary> 形态（不新造交互）。
	if !strings.Contains(src, `id="ch-explain"`) {
		t.Error("页面没有路由诊断块（id=ch-explain）")
	}
	if !strings.Contains(src, `<summary class="tb">路由诊断</summary>`) {
		t.Error("诊断块必须是 <details><summary> 形态（可折叠）—— 别自创交互")
	}
	// ② 取数走只读端点，且**不自己拼字段**（渲染后端给的 text）。
	if !strings.Contains(src, "'/ui/channels/explain?model=") {
		t.Error("诊断块没接 /ui/channels/explain（与 /ui/channels 同一套身份判据的那个端点）")
	}
	if !strings.Contains(src, "j.text") {
		t.Error("页面应当直接渲染后端给的 text（结论/均匀性只有一处出处），而不是自己拼字段")
	}
	// ③ 只读性写在页面上（用户看得见"它不会动任何东西"）。
	if !strings.Contains(src, "不发上游请求") {
		t.Error("诊断块上应写明「只读」（不发上游请求、不改配置）—— 否则用户不敢点")
	}
	// ④ 不许按上游名分叉（本仓的横切纪律）。
	for _, bad := range []string{"monkeycode", "loomy", "qoder"} {
		if strings.Contains(strings.ToLower(src), bad) && strings.Contains(src, bad+":") {
			t.Errorf("诊断块里出现了按上游名分叉的迹象（%s:）", bad)
		}
	}
}
