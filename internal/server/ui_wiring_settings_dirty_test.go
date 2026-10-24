// ui_wiring_settings_dirty_test.go 设置页「（有未保存的修改）」的接线守卫。
//
// # 用户报障（2026-10-25）
//
// 「设置页顶部一直显示（有未保存的修改），点保存设置也清不掉。」
//
// # 根因（前端，后端无关）
//
// 脏标记的置位靠 `#content` 上的两条**委托**监听，而回调不看事件源：
//
//	$('content').addEventListener('input',  () => markDirty(true));
//	$('content').addEventListener('change', () => markDirty(true));
//
// `#content` 是**整个内容区**，里面装着所有面板（对话测试、成长计划、API Key
// 弹窗、日志筛选……）。于是在任何一个面板动一下控件都会把设置页标脏；而清脏
// 只有一条路 —— `fillSettings` 末尾那次 `markDirty(false)`，它只在
// 「切到设置页」与「保存成功」时跑。结果就是提示永远挂着，保存多少次都清不掉。
//
// 后半条：点「保存设置」时焦点若还在数字框里，浏览器会先派发那个框的 `change`
// 再派发按钮的 `click`；只要这次事件落在 PUT 返回之后，顶部就会被重新标脏
// —— 那一刻服务端其实已经存好了，提示在**说反话**。
//
// # 这些断言为什么是字符串级
//
// 前端嵌在二进制里（webui.html 走 go:embed），界面与逻辑之间只靠约定连着：
// 把 `closest(...)` 的判据删掉、把 `markDirty(false)` 从 fillSettings 里挪走，
// **编译照样过、页面照样打开** —— 只是用户又会看到那个清不掉的提示。
// 本项目的既有做法就是这种静态守卫（见 ui_wiring_akmodels_test.go）。
package server

import (
	"strings"
	"testing"
)

// settingsSectionHTML 设置页在 webui.html 里的锚点。
//
// 判据（closest）与锚点（data-key）是**一对**：任何一边改名，另一边会静默
// 失配 —— closest 永远返回 null ⇒ 脏标记再也不会出现（"功能消失"），
// 而不是报错。所以下面有一条专门钉这个锚点。
const settingsSectionHTML = `<section data-key="settings">`

// lastStmt 返回函数体里**最后一条非空、非注释**的语句（用于断言"末尾那句是它"）。
func lastStmt(body string) string {
	lines := strings.Split(body, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" || s == "}" || strings.HasPrefix(s, "//") {
			continue
		}
		return s
	}
	return ""
}

// TestSettingsDirtyClearedAtEndOfFillSettings 回填完成必须清脏。
//
// 这条守的是"清脏的那一下还在不在、是不是在末尾"：
// 只要 `markDirty(false)` 被挪到中间（或删掉），后面任何一次赋值/渲染都可能
// 把它盖回去 —— 而那正是用户看到的现象。
func TestSettingsDirtyClearedAtEndOfFillSettings(t *testing.T) {
	src := readWebUI(t)

	body := sliceFuncBody(t, src, "function fillSettings(")
	if body == "" {
		t.Fatal("webui.html 里找不到 fillSettings —— 回填逻辑被改名/删掉了")
	}
	// 先证明切片真的是那个函数（防止 sliceFuncBody 锚点失配后拿空串假通过）。
	if !strings.Contains(body, "savedSettings = s") {
		t.Fatalf("切到的不是 fillSettings 的函数体（前 120 字节）：%q", body[:120])
	}
	if got := lastStmt(body); got != "markDirty(false);" {
		t.Errorf("fillSettings 末尾必须是 markDirty(false)，实际是 %q —— "+
			"回填完不清脏，设置页会一直挂着「有未保存的修改」", got)
	}
}

// TestSettingsDirtyListenerIsScoped 委托监听必须限定在设置页内。
//
// 这是本次报障的**根因**：裸的 `#content` 全域监听会把别的面板的输入
// 也算成"设置改了"。
func TestSettingsDirtyListenerIsScoped(t *testing.T) {
	src := readWebUI(t)

	// ① 锚点存在（判据与锚点是一对，改名会静默失效）
	if !strings.Contains(src, settingsSectionHTML) {
		t.Fatalf("webui.html 里找不到 %s —— 范围判据会永远失配，"+
			"脏标记再也不会出现（功能静默消失）", settingsSectionHTML)
	}
	// ② 判据存在
	if !strings.Contains(src, `closest('section[data-key="settings"]')`) {
		t.Error("缺少 closest('section[data-key=\"settings\"]') —— " +
			"监听没有范围判断，任何面板的输入都会把设置页标脏")
	}
	// ③ 两条委托都把事件源交给判据
	for _, kind := range []string{"input", "change"} {
		want := `addEventListener('` + kind + `', (ev) => { if (inSettingsPanel(ev.target)) markDirty(true); });`
		if !strings.Contains(src, want) {
			t.Errorf("%s 委托没有按设置页范围过滤（期望含 %q）", kind, want)
		}
	}
	// ④ 反向断言：裸的（无 ev 参数、无条件）形态一条都不许留。
	//    ⚠ 用带行首缩进的完整字面量匹配，避免命中注释里那段"改前形态"的引用。
	for _, kind := range []string{"input", "change"} {
		bad := "addEventListener('" + kind + "', () => markDirty(true))"
		for i, line := range strings.Split(src, "\n") {
			if strings.Contains(line, bad) && !strings.Contains(strings.TrimSpace(line), "//") {
				t.Errorf("webui.html:%d 仍有裸的 %q —— 这就是本 bug 的形态（全域标脏）",
					i+1, bad)
			}
		}
	}
}

// TestSettingsDirtyNotSetWhileSaving 保存在途不标脏，保存成功由回填清脏。
//
// 守的是"点保存 → 焦点框先派发 change → PUT 返回"这条时序：
// 少了 settingsSaving 这道闸，慢请求下顶部会被重新点亮，而服务端其实已经存好了。
func TestSettingsDirtyNotSetWhileSaving(t *testing.T) {
	src := readWebUI(t)

	markBody := sliceFuncBody(t, src, "function markDirty(")
	if markBody == "" {
		t.Fatal("webui.html 里找不到 markDirty")
	}
	if !strings.Contains(markBody, "if (on && settingsSaving) return;") {
		t.Error("markDirty 缺少保存在途守卫（if (on && settingsSaving) return;）—— " +
			"慢请求下保存完仍会被标脏")
	}

	saveBody := sliceFuncBody(t, src, "async function saveSettings(")
	if saveBody == "" {
		t.Fatal("webui.html 里找不到 saveSettings")
	}
	// 保存成功 → 回填 → 回填末尾清脏。断言回填确实在 PUT 之后。
	putAt := strings.Index(saveBody, "method: 'PUT'")
	fillAt := strings.Index(saveBody, "fillSettings(r.settings, r)")
	if putAt < 0 {
		t.Fatal("saveSettings 里找不到 PUT /admin/settings —— 保存路径被改了")
	}
	if fillAt < 0 || fillAt < putAt {
		t.Error("saveSettings 必须在 PUT 成功之后调 fillSettings(r.settings, r) —— " +
			"只有它末尾的 markDirty(false) 能把提示清掉")
	}
	// settingsSaving 必须置位且在 finally 里复位（漏了复位 = 之后永远不标脏）。
	if !strings.Contains(saveBody, "settingsSaving = true;") {
		t.Error("saveSettings 未置位 settingsSaving —— 保存在途仍会被标脏")
	}
	if !strings.Contains(saveBody, "settingsSaving = false;") {
		t.Error("saveSettings 未在 finally 里复位 settingsSaving —— 之后所有改动都不会再提示")
	}
}

// TestSettingsDirtyFailureKeepsDirty 保存失败必须保持脏。
//
// 失败时界面上的值确实还没落盘 —— 这时候清掉提示才是骗人（本项目的纪律：
// 提示必须与事实一致）。这条防的是"为了让它消失干脆一律清脏"那种修法。
func TestSettingsDirtyFailureKeepsDirty(t *testing.T) {
	src := readWebUI(t)

	saveBody := sliceFuncBody(t, src, "async function saveSettings(")
	if saveBody == "" {
		t.Fatal("webui.html 里找不到 saveSettings")
	}
	catchAt := strings.Index(saveBody, "} catch (e) {")
	if catchAt < 0 {
		t.Fatal("saveSettings 里找不到 catch 分支")
	}
	catchBody := saveBody[catchAt:]
	// catch 分支里**不许**出现 markDirty(false)：那等于"保存失败也说没改动"。
	if strings.Contains(catchBody, "markDirty(false)") {
		t.Error("saveSettings 的 catch 分支里清了脏标记 —— 保存失败时值还没落盘，" +
			"清掉提示就是在骗用户")
	}
	if !strings.Contains(catchBody, "toast('保存失败：'") {
		t.Error("saveSettings 失败分支应如实播报「保存失败：…」")
	}
}
