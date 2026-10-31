package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestToastSharedLayerInjected 通知框是**一套实现**（外壳 + 令牌页逐字相同）。
//
// # 它守的是什么（2026-11-01 用户报障的同一类问题的第二处）
//
// 用户原话：「右上角如果出现多个消息 多交替显示 这也是bug」。
// 外壳因此改成了堆叠式（逐条列出 / 重复文案合并 ×N / 超限折叠），而令牌页还留着
// 旧实现 —— `el.textContent = msg` + `.toast on`：**后一条覆盖前一条**。
// 同一个产品里两种通知语义：令牌页批量轮换一次发十几条结果时，用户仍然只读得到最后一条。
//
// 判据三条：
//
//	① 两页都带 START/END 标记块（共享层真的注入了）
//	② 两块**逐字相同** —— 这才是「统一」本身；只断言「都存在」会让两份实现各改各的
//	③ 块里有那几条语义（上限、队列在元素上、各自截止时间、折叠行、点击关闭），
//	   且**旧的单行实现不再出现**（防「注入了但没替换掉旧的」这种半截改动）
func TestToastSharedLayerInjected(t *testing.T) {
	blocks := map[string]string{}
	for _, page := range []string{"webui.html", "webui-tokens.html"} {
		body := readPage(t, page)
		b, ok := sliceMarkerBlock(body, "WB2API:TOAST:START", "WB2API:TOAST:END")
		if !ok {
			t.Fatalf("%s 里没有 TOAST 共享层标记块 —— 通知实现又各写一份了", page)
		}
		blocks[page] = b
	}
	if blocks["webui.html"] != blocks["webui-tokens.html"] {
		t.Error("外壳与令牌页的 toast 实现**不一致** —— 这正是本次要消灭的分叉（同一个产品两种通知语义）")
	}
	// ⚠ 判据前先**剥注释**：共享层的头部注释里为了说明来历，原文引用了旧实现那两行；
	//   不剥注释会把「解释错误做法」当成「还在用错误做法」（本仓在别处踩过同类）。
	blk := stripCodeComments(blocks["webui.html"])
	for _, want := range []struct{ what, needle string }{
		{"上限常量在函数内（模块级变量会让门禁抽出来的那份 ReferenceError）", "TOAST_MAX = 5"},
		{"队列挂在元素上", "el._lines"},
		{"每条各自截止时间", "deadline"},
		{"超限折叠成一行", "…还有 "},
		{"重复文案合并成 ×N", "toastn"},
		{"点一行关一行", "closest('.toastline')"},
		{"容器类名保持旧口径", "el.className = 'notice '"},
	} {
		if !strings.Contains(blk, want.needle) {
			t.Errorf("共享块里缺「%s」（找 %q）", want.what, want.needle)
		}
	}
	// 旧实现的两条特征（任一还在 = 半截改动）
	for _, dead := range []string{"el.textContent = msg", "'toast on'"} {
		if strings.Contains(blk, dead) {
			t.Errorf("共享块里还有旧的单行实现特征 %q —— 注入了但没替换干净", dead)
		}
	}
}

// TestToastNoPerPageStyles 页面**不许**再自带一份 toast 定位（那会让两页又漂开）。
//
// 位置与外观的唯一真相是主题层（theme-max.css 的 .notices/.notice/#toast .toastline）；
// 令牌页原来那句 `.toast{position:fixed;left:50%;bottom:24px}` 是左下角的单行框，
// 与外壳的右上堆叠完全不是一回事 —— 这条判据防它被复制回来。
func TestToastNoPerPageStyles(t *testing.T) {
	for _, page := range []string{"webui.html", "webui-tokens.html"} {
		// 同样先剥注释：页面里那段「地雷提示」**故意**写了旧规则的形态。
		body := stripCodeComments(readPage(t, page))
		if strings.Contains(body, ".toast { position: fixed") || strings.Contains(body, ".toast{position:fixed") {
			t.Errorf("%s 里又出现页面自带的 .toast 定位 —— 通知的位置/外观只该在主题层定义一次", page)
		}
	}
}

// stripCodeComments 剥掉 JS/CSS 注释（保留换行，行号不漂）。
//
// 判据要的是**代码**；本仓注释里大量出现「错误写法原文」（为了记录踩坑），
// 不剥注释的判据会把文档当成实现。
func stripCodeComments(s string) string {
	// 块注释要按**跨行**处理：主题块里那段说明就是 /* … */ 跨了十几行，
	// 而它里面故意写了旧规则的形态（`.toast{position:fixed;…}`）——
	// 只按行首判断会把这段散文当成代码（第一版就是这样，两页都被误判）。
	var out strings.Builder
	inBlock := false
	for _, line := range strings.Split(s, "\n") {
		var keep strings.Builder
		for i := 0; i < len(line); i++ {
			if inBlock {
				if i+1 < len(line) && line[i] == '*' && line[i+1] == '/' {
					inBlock = false
					i++
				}
				continue
			}
			if i+1 < len(line) && line[i] == '/' && line[i+1] == '*' {
				inBlock = true
				i++
				continue
			}
			if i+1 < len(line) && line[i] == '/' && line[i+1] == '/' {
				break // 行尾注释：本行剩余全是散文
			}
			keep.WriteByte(line[i])
		}
		out.WriteString(keep.String())
		out.WriteString("\n")
	}
	return out.String()
}

func readPage(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".", name))
	if err != nil {
		t.Fatalf("读 %s 失败: %v", name, err)
	}
	return string(b)
}

// sliceMarkerBlock 取 START..END 之间的内容（含标记行，便于逐字比对）。
func sliceMarkerBlock(body, start, end string) (string, bool) {
	i := strings.Index(body, start)
	j := strings.Index(body, end)
	if i < 0 || j <= i {
		return "", false
	}
	return body[i : j+len(end)], true
}
