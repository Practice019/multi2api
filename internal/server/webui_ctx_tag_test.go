// webui_ctx_tag_test.go 守 chip 上的上下文窗口标注。
//
// 用户定的口径：
//
//	能拿到 -> 填上
//	拿不到 -> 不写，或写占位
//	写清楚 -> 让人知道「不知道」不等于「是 0」
//
// 为什么占位而不是留白：留白时用户分不清「这个模型窗口很小」与
// 「我们不知道它多大」。前者影响他选哪个模型，后者不影响。
//
// 但也不能编数字：ctx 决定客户端要不要提前压缩历史。报大了会塞太多
// 被上游截断，报小了会过早 compact。所以占位必须是问号。
package server

import (
	"os"
	"strings"
	"testing"
)

func readWebUIForCtx(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}
	return string(raw)
}

// jsFuncSource 截出某个 JS 函数的源码。
func jsFuncSource(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("找不到 %s", sig)
	}
	body := src[i:]
	if j := strings.Index(body, "\n  }"); j > 0 {
		body = body[:j]
	}
	return body
}

// TestWebUICtxTagShowsPlaceholderWhenUnknown 未知时必须显示 ctx ? 占位。
func TestWebUICtxTagShowsPlaceholderWhenUnknown(t *testing.T) {
	src := readWebUIForCtx(t)
	body := jsFuncSource(t, src, "function ctxTag(m)")
	if !strings.Contains(body, "ctx ?") {
		t.Error("未知时必须显示 ctx ? 占位 —— 留白会让「窗口小」与「不知道」看起来一样")
	}
	if !strings.Contains(body, "ctx unknown") {
		t.Error("未知态要有单独的 class（虚线边框），否则与真实数字只靠颜色区分")
	}
}

// TestWebUICtxTagExplainsWhyUnknown title 必须说清「不是 0」。
func TestWebUICtxTagExplainsWhyUnknown(t *testing.T) {
	src := readWebUIForCtx(t)
	body := jsFuncSource(t, src, "function ctxTag(m)")
	for _, need := range []string{"上游没有提供", "不代表它是 0"} {
		if !strings.Contains(body, need) {
			t.Errorf("未知态的 title 必须写明 %q —— 否则用户会以为窗口真是 0", need)
		}
	}
}

// TestWebUICtxChipWired chip 渲染必须真的调用 ctxTag。
//
// 破了会怎样：函数写好了但没接上 —— 页面上一片安静，看起来像功能没做。
func TestWebUICtxChipWired(t *testing.T) {
	src := readWebUIForCtx(t)
	body := jsFuncSource(t, src, "function renderModelGroup(")
	if !strings.Contains(body, "ctxTag(m)") {
		t.Error("renderModelGroup 的 chip 没有调用 ctxTag —— 标注不会显示")
	}
}

// TestWebUICtxItemsCarryMeta groupModelsByOwner 必须把元数据带进 items。
//
// 破了会怎样：renderModels 拿到的 items 只有 id/fullId/owner，
// ctx 在分组这一步就丢了 —— 渲染层再正确也显示不出来。
func TestWebUICtxItemsCarryMeta(t *testing.T) {
	src := readWebUIForCtx(t)
	i := strings.Index(src, "g.items.push(")
	if i < 0 {
		t.Fatal("找不到 g.items.push")
	}
	body := src[i:]
	if j := strings.Index(body, ");"); j > 0 {
		body = body[:j]
	}
	for _, need := range []string{"ctx:", "maxOut:"} {
		if !strings.Contains(body, need) {
			t.Errorf("items 里缺 %q —— 元数据在分组这一步丢了", need)
		}
	}
}

// TestWebUICtxFmtTreatsZeroAsUnknown fmtCtx 必须把 0 当成「没有」。
//
// 失效形态是「显示成 ctx 0」—— 从源码就能看出来。
func TestWebUICtxFmtTreatsZeroAsUnknown(t *testing.T) {
	src := readWebUIForCtx(t)
	body := jsFuncSource(t, src, "function fmtCtx(n)")
	if !strings.Contains(body, "n <= 0") {
		t.Error("必须把 0 与负数当成「没有」—— 否则会显示 ctx 0")
	}
}
