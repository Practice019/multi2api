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
func TestAPIKeyModelsUIIsWired(t *testing.T) {
	src := readWebUI(t)

	need := []struct {
		what   string
		needle string
	}{
		{"表头「可用模型」", "<th>可用模型</th>"},
		{"列渲染函数", "function apiKeyModelsCell("},
		{"单元格里调用它", "${apiKeyModelsCell(k)}"},

		// 弹窗本体
		{"弹窗容器", `id="akModal"`},
		{"名称输入", `id="akName"`},
		{"额度输入", `id="akLimit"`},
		{"限速输入", `id="akRpm"`},
		{"「全部允许」开关", `id="akAll"`},
		{"搜索框", `id="akSearch"`},
		{"模型列表容器", `id="akModelList"`},
		{"全选/清空", `id="akSelAll"`},

		// 行为
		{"打开弹窗", "function openAPIKeyModal("},
		{"保存", "function saveAPIKeyModal("},
		{"拉模型目录", "async function ensureAKCatalog("},
		{"目录来源是 /v1/models", "api('/v1/models')"},
		{"渲染可勾选列表", "function renderAKModelList("},
		{"搜索过滤", "includes(kw)"},
		{"新建按钮打开弹窗", "$('btnApiKeyNew').onclick = () => openAPIKeyModal('create')"},
		{"行内「编辑」打开弹窗", "openAPIKeyModal('edit', cur)"},
	}
	for _, n := range need {
		if !strings.Contains(src, n.needle) {
			t.Errorf("webui.html 缺少%s（%q）—— 后端会照发，而那一列/那个按钮不会出现，"+
				"且**没有任何报错**", n.what, n.needle)
		}
	}

	// 不允许再回到 prompt 那条路（它会同时丢掉搜索、回显与"取消 ≠ 0"）。
	if strings.Contains(src, "function apiKeyCreate(") {
		t.Error("apiKeyCreate（prompt 四连问）应当已被弹窗取代 —— " +
			"它同时丢掉了搜索、字段回显，以及「取消 ≠ 输入 0」这条判据")
	}
}

// 「隐藏 ≠ 取消勾选」：搜着勾两个、再搜别的勾一个，三个都该留下。
//
// 这是搜索式多选最容易写错的地方 —— 重渲染时把被过滤掉的那些
// 从选择集里丢掉了，而用户完全看不出来（他以为自己勾了三个）。
func TestAPIKeyPickerKeepsHiddenSelections(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, "akHiddenChecked") {
		t.Fatal("弹窗缺少 akHiddenChecked —— 搜索会隐藏部分行，" +
			"而「隐藏」**不等于**「取消勾选」：把隐藏的那些从选择集里丢掉，" +
			"用户会得到一个比他勾的少得多的白名单，且看不出来")
	}
	if !strings.Contains(src, "function stashAKChecked(") {
		t.Error("重渲染前必须先 stash 当前勾选（否则隐藏行会被丢掉）")
	}
}

// 「不是全部允许、又没选任何模型」必须在保存前拦下。
//
// 不拦的话会存成一个**空名单**，而后端把空名单读作"不限制" ——
// 与用户此刻的意图（他刚取消勾选"全部允许"）恰好相反。
// 这种"看起来设了限制其实没设"是白名单最危险的失败形态。
func TestAPIKeyPickerRejectsEmptySelection(t *testing.T) {
	src := readWebUI(t)
	if !strings.Contains(src, "要么勾「全部允许」，要么至少选一个模型") {
		t.Error("空选择必须在保存前拦下 —— 后端把空名单读作「不限制」，" +
			"与用户取消勾选「全部允许」的意图相反")
	}
}

// API key **永久可看**：默认打码、点一下显示完整值并自动复制。
//
// 用户原话：「我不要那个 API key 只在第一次创建的时候显示，
// 它永久都可以显示。但是默认的时候…中间部分是要屏蔽一下，就打个码。
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
