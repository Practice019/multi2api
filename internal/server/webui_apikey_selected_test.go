// webui_apikey_selected_test.go 钉住「已选模型要置顶可见」。
//
// # 用户报的问题
//
// 「这选择的模型需要显示在一个额外的地方 不然用户看不见呢 要么置顶」
//
// 原来只有一个「已选 9 个模型」的数量，具体是哪 9 个要滚进下面
// 那个 280px 的滚动列表里逐个找勾 —— 列表有 670 个模型，实际上不可能核对。
//
// 这不只是不好看，是**配置不可验证**：改完白名单无法确认授权了什么，
// 只能相信那行数字。而这个配置恰好决定「谁能调什么模型」。
package server

import (
	"strings"
	"testing"
)

func TestWebUIShowsSelectedModelsAtTop(t *testing.T) {
	src := string(webuiHTML)

	// ① 必须有一个**独立的**已选容器（不是靠滚动列表里的勾表示已选）。
	if !strings.Contains(src, `id="akSelectedChips"`) {
		t.Fatal(`找不到已选模型容器 akSelectedChips —— 模型清单只报数量，用户无法核对授权了什么`)
	}

	// ② 它必须在模型列表**之前**（置顶），否则仍要滚动才看得见。
	boxAt := strings.Index(src, `id="akSelectedBox"`)
	listAt := strings.Index(src, `id="akModelList"`)
	if boxAt < 0 || listAt < 0 {
		t.Fatalf(`锚点缺失: box=%d list=%d`, boxAt, listAt)
	}
	if boxAt > listAt {
		t.Error(`★ 已选清单在模型列表之后 —— 用户仍要滚动才能看见，与「要么置顶」的要求相反`)
	}

	// ③ 每个已选项必须可点掉（否则取消一个模型又要回去找）。
	if !strings.Contains(src, `data-akunchk`) {
		t.Error(`已选项没有取消入口 —— 取消某个模型还得回滚动列表里找`)
	}

	// ④ 空清单必须说清后果，不能留白。
	if !strings.Contains(src, `将无法调用任何模型`) {
		t.Error(`空清单没有说明后果 —— 用户会以为「没选就是不限」，而实际是「一个都不能调」`)
	}
}

// TestWebUISelectedChipsUseDelegation chip 的点击必须用事件委托。
//
// # 为什么这条不能省
//
// chip 的 innerHTML 每次 updateAKCount 都会重建。若把 onclick 绑在元素上，
// 处理器会随元素一起被丢掉 —— 表现是「第一次能点、之后点了没反应」，
// 而且**不报任何错**。这类失效很难靠手测发现（要连续点两次）。
func TestWebUISelectedChipsUseDelegation(t *testing.T) {
	src := string(webuiHTML)
	if !strings.Contains(src, `$('akSelectedChips').addEventListener('click'`) {
		t.Error(`★ chip 点击没用事件委托 —— innerHTML 重建后处理器会丢失，表现为「点一次就不响应」且不报错`)
	}
}
