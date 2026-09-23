// webui_manual_login_test.go 钉住登录弹窗里 manual 粘贴框代码块的**位置**。
//
// # 为什么按字节序断言（回归自一次真实的"绿测试白页面"事故）
//
// manualBox 块插入时锚点选了 `$('loginBody').innerHTML = \“ —— 它的第一次
// 出现**在 start 的 catch 块里**（失败提示那行），块因此落进了 catch：
// 主模板读不到 manualBox → 每次登录弹窗在 start 成功后抛 ReferenceError，
// 界面永远停在"正在向上游申请授权链接…"。Go 全量测试照样绿 —— 没有任何
// 测试跑这段 JS。这里用最小断言把"定义先于使用"钉成红线。
package server

import (
	"strings"
	"testing"
)

func TestWebUIManualLoginBlockPlacement(t *testing.T) {
	src := string(webuiHTML)
	start := strings.Index(src, "r = await admin('/admin/login/start'")
	catchFail := strings.Index(src, "申请失败：")
	define := strings.Index(src, "const mEntry")
	hasAuth := strings.Index(src, "const hasAuthURL")
	use := strings.Index(src, "${manualBox}")
	if start < 0 || catchFail < 0 || define < 0 || hasAuth < 0 || use < 0 {
		t.Fatalf("锚点缺失: start=%d catchFail=%d define=%d hasAuth=%d use=%d", start, catchFail, define, hasAuth, use)
	}
	if !(start < catchFail && catchFail < define && define < hasAuth && hasAuth < use) {
		t.Errorf("manual 粘贴框块必须位于 start 的 catch 之后、hasAuthURL 判定之前（定义先于模板使用）：start=%d catchFail=%d define=%d hasAuth=%d use=%d",
			start, catchFail, define, hasAuth, use)
	}
}
