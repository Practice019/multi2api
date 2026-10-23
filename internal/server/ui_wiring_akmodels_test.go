// ui_wiring_akmodels_test.go API Key 界面在 webui.html 里的接线守卫。
//
// # 这些守卫守的是什么（本会话反复出现的形态）
//
// 前端**嵌在二进制里**（webui.html 走 go:embed），而界面与后端之间只靠约定
// 连着：后端返回 `k.models` ←→ 前端读 `k.models`；`data-act="edit"` ←→
// 事件委托里有那个分支。任何一处改名，另一处**不会报错** ——
// 页面上那一列空着、那个按钮点了没反应。
//
// 所以每一种"用户点名要的界面元素"都在这里逐个钉住。
//
// # ⚠ 这批用例改过一次，因为界面被重做了（记下来免得下次又困惑）
//
// 用户原话：「创建 API 的过程太简陋了。因为这个模型不应该让我们输入，
// 而应该是选择。我们不是有模型列表吗？要从模型列表里面选，还要支持搜索」。
//
// 改造前是 `window.prompt` 四连问（名称/额度/限速/模型手打），
// 所以老断言钉的是 `parseModelList` 与那句提示文案。
// 现在改钉弹窗与可搜索多选 —— 老断言留着会**保护一个已经不存在的界面**。
package server

import (
	"os"
	"strings"
	"testing"
)

// readWebUI 读前端源码（多处守卫共用）。
func readWebUI(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	return string(raw)
}

// 模型必须**从目录里选**（多选 + 搜索），不能手打。
//
// 手打的失败形态是"设了限制却一个都调不了"，而保存时**完全看不出来**
// （保存成功、列表里也显示着那个拼错的模型名）。
// ---- 模型选择器相关的守卫已删除 ----
//
// 原来的四条（TestAPIKeyModelsUIIsWired / TestAPIKeyPickerKeepsHiddenSelections /
// TestAPIKeyPickerRejectsEmptySelection，以及 webui_apikey_selected_test.go 里的两条）
// 守的都是「API Key 弹窗里的模型多选器」—— 那个控件已被移除。
//
// 为什么整体删掉而不是改断言：
//
// 它们守的语义在新设计下**不存在了**。比如「取消勾选全部允许但一个都不选 ⇒ 拦下」——
// 那是白名单时代的危险（空名单 = 不限制，与用户意图相反）。现在 key 只选分组，
// 「一个分组都不选」就是明确的「不限制」，没有歧义可守。
//
// 新的守卫在 groups_ui_test.go 的 TestAPIKeyModalSelectsGroupsNotModels：
// 它断言旧控件确实消失、分组控件确实存在、提交体确实带 groups。

// 但是你可以再点击一下那个 API key，点击之后它就显示出来了，
// 并且自动复制到那个剪切板里面去。」
func TestAPIKeyRevealAndCopyWired(t *testing.T) {
	src := readWebUI(t)

	for _, n := range []struct{ what, needle string }{
		{"打码单元格渲染", "function apiKeyCellHTML("},
		{"单元格里调用它", "${apiKeyCellHTML(k)}"},
		{"带上完整值", "data-akkey="},
		{"带上掩码", "data-akmask="},
		{"点击后显示完整值", "kb.textContent = full"},
		{"点击后自动复制", "copyText(full, 'API Key')"},
		{"事件委托（行会重建，不能挂行上）", "closest('button[data-akkey]')"},
	} {
		if !strings.Contains(src, n.needle) {
			t.Errorf("webui.html 缺少%s（%q）—— key 列会一直打码、点了没反应", n.what, n.needle)
		}
	}

	// 打码必须**保留首尾**（纯省略号看不出是同一把 key）。
	if !strings.Contains(src, "slice(0, 8)") && !strings.Contains(src, "k.masked") {
		t.Error("打码应保留首尾（后端已给 masked 字段，前端优先用它）")
	}
}
