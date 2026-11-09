package server

import (
	"os"
	"strings"
	"testing"
)

// TestStatusPageHasNoMeaninglessCards 状态页**不许再出现**那三张无意义的卡（T-441）。
//
// # 用户原话
//
//	「系统状态展示的那些信息，你看看，是不是有哪些东西根本就不需要展示的呢。
//	  也就是说没有意义的。比如说**今日消费**是什么意思啊？……
//	  后面就是什么**依赖健康**里的这几个信息和**缓存容量**这几个信息，是不是都没有意义呢？」
//
// # 为什么这三张卡必须不回来（逐条给依据，不是"用户说过"就算）
//
//	「今日消费」   —— 单位不明（quota？美元？），fmtCost 固定两位小数会显示成
//	                  「21132.00」；而且它和「今日 Token = 67072」表达同一件事
//	「渠道亲和缓存」—— 本仓**没有** HybridCache 亲和缓存：后端返回的是
//	                  {"applicable":false,"enabled":false,"note":"…该项不适用"}
//	                  ⇒ 整张卡在说"这东西不存在"
//	「磁盘缓存」   —— **全仓搜不到实现**，后端压根不返回这个字段 ⇒ 永远是空卡
//
// # 判据是"整页文本里没有这三个词"而不是"某个函数不存在"
//
// 因为真正的目标（用户要的）是**界面上看不到**。函数改名/重构后仍然不能回来 ⇒
// 词级断言比结构断言更贴近需求，也更能挡住"换个写法又渲染出来"。
//
// ⚠ 空匹配即失败：读不到文件要 t.Fatalf，**不能**静默通过
// （历史上"空匹配即通过"的守卫在包改名后会静默变绿 —— 那是假绿）。
func TestStatusPageHasNoMeaninglessCards(t *testing.T) {
	b, err := os.ReadFile("webui-status.html")
	if err != nil {
		t.Fatalf("读 webui-status.html 失败（它必须存在 —— 空匹配不算通过）: %v", err)
	}
	page := string(b)
	if strings.TrimSpace(page) == "" {
		t.Fatal("webui-status.html 读到空内容 —— 守卫失效（fail-open）")
	}
	for _, gone := range []string{"今日消费", "渠道亲和缓存", "磁盘缓存"} {
		if strings.Contains(page, gone) {
			t.Errorf("状态页里仍有「%s」——"+
				"它是无意义的展示（单位不明/本仓无此实现/后端不返回），用户明确要求删掉。"+
				"若确实要加回来，先回答「它表达的信息和已有的哪张卡重复了」。", gone)
		}
	}
}
