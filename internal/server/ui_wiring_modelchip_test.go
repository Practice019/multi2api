package server

import (
	"strings"
	"testing"
)

// ui_wiring_modelchip_test.go 「可用模型」卡片点击复制的接线守卫。
//
// # 守的是什么
//
// 前端嵌在二进制里（webui.html 走 go:embed），卡片由 JS 拼字符串生成、
// 点击靠事件委托。任何一环改名都**不会报错**：
//
//	生成处少写 data-id  → 点击时 dataset.id 是 undefined → 复制空串
//	点击处理被删/改回  → 卡片变成纯装饰，点了没反应
//
// 两种都只在用户手上才暴露，所以逐个钉住。

// TestModelChipCarriesFullId 卡片必须带完整模型 id。
//
// 用户要求「点击一个卡片自动复制完整的模型名称 例如 loomy/auto」——
// 「完整」是字面要求：卡片上显示的文字可能被截断，data-id 必须是原值。
//
// ⚠ 生成处有两处（正常分支与「无可用账号」分支），漏一处就会出现
//
//	「有的分组能复制、有的不能」这种最难查的形态。
func TestModelChipCarriesFullId(t *testing.T) {
	src := readWebUI(t)
	// 两处都必须是 data-id="${esc(m.fullId)}"。
	n := strings.Count(src, `data-id="${esc(m.fullId)}"`)
	if n < 2 {
		t.Errorf("只有 %d 处 chip 带 data-id=\"${esc(m.fullId)}\"，应为 2 处"+
			"（正常分支 + 无可用账号分支）—— 漏的那一处点了复制不出东西", n)
	}
}

// TestModelChipClickCopies 点击卡片必须**复制**模型名。
//
// 这条是本次改动的核心判据：改造前点击只把值塞进对话测试的下拉，
// 用户要从面板上**手抄**模型名（而卡片文字是截断的，必然抄错）。
func TestModelChipClickCopies(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, "copyText(id, '模型名')") {
		t.Error("chip 的点击处理里没有 copyText(id, '模型名') —— " +
			"卡片会变回纯装饰，用户还是得手抄模型名")
	}
	// 复制必须用 data-id 而不是可见文字：可见文字经过 esc() 且可能被截断。
	if !strings.Contains(src, "const id = c.dataset.id || ''") {
		t.Error("点击处理没有从 c.dataset.id 取 id —— 复制到的会是截断后的显示文字")
	}
}

// TestModelChipKeepsDropdownSync 点击仍要填对话测试的下拉（既有行为不能丢）。
//
// 复制是新增的副作用，不该顺手删掉原来的「点一下就选中」——
// 那会让「先点模型、再点对话测试」这条路径断掉。
//
// ⚠ 但填之前要确认选项真的存在：面板里的模型未必在当前筛选的下拉里
//
//	（下拉按上游筛过）。直接赋值会得到「选了但看不到」的状态。
func TestModelChipKeepsDropdownSync(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, "sel.value = id") {
		t.Error("点击不再填对话测试的下拉 —— 既有路径断了")
	}
	if !strings.Contains(src, "Array.from(sel.options || []).some(o => o.value === id)") {
		t.Error("填下拉前没有检查选项是否存在 —— 会得到「选了但看不到」的状态")
	}
}

// TestModelChipHasPointerCursor 卡片必须看起来可点。
//
// 复制是**无声**操作：没有手型光标、按下也没位移时，用户点完不知道
// 到底成没成（toast 会飘走，之后再想确认只能再点一次）。
func TestModelChipHasPointerCursor(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, ".chips .chip{cursor:pointer}") {
		t.Error("缺少 .chips .chip{cursor:pointer} —— 卡片看起来不可点")
	}
}
