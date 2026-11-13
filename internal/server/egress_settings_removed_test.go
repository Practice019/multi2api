// egress_settings_removed_test.go 设置页不再渲染「MonkeyCode 出口」整块（2026-11-13 用户要求）。
//
// # 用户原话
//
//	「这个 MonkeyCode 出口 立即生效 / 国际站代理（monkeycode-ai.net）**也不需要**」
//
// # 删的是界面，不是能力（两条边界，本文件一并钉住）
//
//	· `config.json` 的 `monkeycode.proxy` / `monkeycode.direct_proxy` **照旧生效**（后端一行未改）；
//	· 卡片头上那条只读的「出口 xxx」（`egressHTML`）**保持**。
//
// # 为什么用"读源码判定"
//
//	本仓没有可执行的 DOM 环境（执行式守卫只有邀请面板那一处、靠 node 跑片段）。
//	这条只判"设置页的控件与绑定都不在了"，属于**结构式**判据：
//	它抓不到"运行时会不会抛"，但足够钉住"这两个控件不许回来"（配合 `node --check` 守语法）。
package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMonkeyCodeEgressBlockRemoved(t *testing.T) {
	htmlBytes, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html: %v", err)
	}
	jsBytes, err := os.ReadFile(filepath.Join("ui", "app.js"))
	if err != nil {
		t.Fatalf("读 ui/app.js: %v", err)
	}
	html, js := string(htmlBytes), string(jsBytes)

	// ① 控件与绑定都不许在（判据取"真代码形态"，避开我们留在注释里的说明文字）。
	for _, bad := range []struct{ where, needle string }{
		{"webui.html", `id="setMcProxy"`},
		{"webui.html", `id="setMcDirectProxy"`},
		{"webui.html", "MonkeyCode 出口 <span"},
		{"webui.html", "国内站强制代理"},
		{"ui/app.js", "['setMcProxy'"},
		{"ui/app.js", "['setMcDirectProxy'"},
	} {
		src := html
		if bad.where == "ui/app.js" {
			src = js
		}
		if strings.Contains(src, bad.needle) {
			t.Errorf("%s 里还有 %q —— 「MonkeyCode 出口」整块没删干净（用户明确不要它）", bad.where, bad.needle)
		}
	}
	// ② 悬挂引用的反面：SET_FIELDS 里不许留"没有对应控件的绑定"。
	for _, id := range []string{"setMcProxy", "setMcDirectProxy"} {
		if strings.Contains(js, "'"+id+"'") && !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("SET_FIELDS 里还有 %s 的绑定，而 HTML 里没有对应控件 —— 这就是「悬空引用」", id)
		}
	}

	// ③ 正向对照（防"删多了"）：同页其它设置块与卡片头的只读出口展示必须仍在。
	for _, need := range []string{"网络与容错", "出站请求安全", "setUpTimeout"} {
		if !strings.Contains(html, need) {
			t.Errorf("webui.html 里少了 %q —— 删超出范围了", need)
		}
	}
	for _, need := range []string{"'setArmorMode'", "'setStrictModelPrefix'", "function egressHTML("} {
		if !strings.Contains(js, need) {
			t.Errorf("ui/app.js 里少了 %q —— 删超出范围了（破甲/前缀设置与卡片头出口展示都要保留）", need)
		}
	}
}
