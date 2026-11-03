// channels_discover_ui_test.go 钉住「从上游拉取」按钮发出的**字段名**。
//
// # 这是用户实测报障的回归钉
//
// 现象：新建渠道、填好密钥与 Base URL，点②「从上游拉取」必报
//
//	拉取失败: 没有可用的 key: 该渠道没有启用状态的 key，且请求里也没带 key
//
// 根因不是后端：`/admin/channels/discover` 的契约是
// `discoverReq{base_url, key string}`（见 internal/admin/channels_discover.go），
// 而前端把字段写成了 `keys`（那是**保存渠道**接口的名字，被误用到这里）。
//
// 之所以难查：JSON 解码会**静默忽略**未知字段 —— 后端看到的只是"没带 key"，
// 没有任何线索指向"前端字段名写错了"。所以这条守卫按字段名钉死，
// 而不是按行为（行为用例在 internal/admin/channels_discover_test.go）。
package server

import (
	"os"
	"strings"
	"testing"
)

// TestChannelsDiscoverUIUsesKeyField pullModels 必须用 `key`（字符串），不得用 `keys`。
func TestChannelsDiscoverUIUsesKeyField(t *testing.T) {
	b, err := os.ReadFile("webui-channels.html")
	if err != nil {
		t.Fatalf("读 webui-channels.html: %v", err)
	}
	html := string(b)

	start := strings.Index(html, "function pullModels()")
	if start < 0 {
		t.Fatal("★ webui-channels.html 里找不到 pullModels —— 「从上游拉取」按钮的实现被改名/删除了？")
	}
	// 取到下一个顶层 function 为止（函数体足够短，用固定窗口更稳：
	// 避免依赖"下一个 function"的写法在嵌套函数上失准）。
	end := start + 2600
	if end > len(html) {
		end = len(html)
	}
	body := html[start:end]

	if strings.Contains(body, "body.keys") {
		t.Error("★ pullModels 不得用 body.keys：discover 只认 `key`（字符串）。\n" +
			"  发 keys 会被后端静默忽略，表现为「没有可用的 key」—— 用户实测报障的那条。")
	}
	if !strings.Contains(body, "body.key") {
		t.Error("★ pullModels 必须把表单里的密钥以 `key` 字段发出，否则新建渠道时无法试查上游模型。")
	}
	if !strings.Contains(body, "body.id") {
		t.Error("★ 编辑已有渠道时应同时带 `id`：密钥框为空时后端回落到该渠道已启用的 key。")
	}
}
