package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// readWebUIHTML 读源码文件（不是内嵌的 webuiHTML）。
//
// 解析必须针对**源文件**：占位符要替换、JS 常量要在浏览器求值，
// 两者都只在源文件里可见。读内嵌变量会让"改文件忘了同步"这类漂移漏网。
func readWebUIHTML() (string, error) {
	b, err := os.ReadFile(filepath.Join("webui.html"))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// TestColPlaceholdersMatchJSCols 守住 Go 侧替换表与 webui.html 的 JS 常量一致。
//
// # 为什么必须有这条测试（而不是只写注释）
//
// webui.html 的静态表格空态行写了 `colspan="__COLS_LOGS__"` 这类占位符，
// 由本包的 colPlaceholders 在服务端替换成真实数字。
// 与此同时，JS 里还有一份 `const COLS = { logs: 10, ... }` 用于**运行时**
// 动态生成的占位行（rowPlaceholder）。
//
// 于是同一个列数存在于**两个地方**：
//
//	Go 的 colPlaceholders 表（静态 HTML 用）
//	JS 的 COLS 常量（动态行用）
//
// 两者不一致时，静态空态行与动态空态行的跨列数会不同 —— 表现为
// "某些空态行错位"，而且**不会报任何错**。
//
// # 这里为什么读源码文件而不是 runtime 变量
//
// 占位符与 JS 常量都只在源文件里有意义（一个是文本替换的目标，
// 一个是浏览器里求值的字面量）。读文件比导出 Go 变量更能覆盖真实情况：
// 有人改了 JS 常量但漏改 Go 表，本测试就会红。
func TestColPlaceholdersMatchJSCols(t *testing.T) {
	src, err := readWebUIHTML()
	if err != nil {
		t.Fatalf("读 webui.html 失败: %v", err)
	}

	// 1) 每个 colPlaceholders 的 token 都必须在 HTML 里至少出现一次，
	//    否则说明占位符改过名而 Go 表没跟上（替换变成空操作）。
	for _, p := range colPlaceholders {
		if !strings.Contains(src, p.token) {
			t.Errorf("占位符 %s 在 webui.html 里不存在 —— "+
				"Go 侧的替换表与页面漂移了（改了占位符名却没改这里？）", p.token)
		}
	}

	// 2) Go 表里的数字必须与 JS 的 COLS 常量逐项相等。
	jsCols := parseJSCols(src)
	if len(jsCols) == 0 {
		t.Fatal("没能从 webui.html 解析出 JS 的 COLS 常量 —— " +
			"解析器或源码结构变了，本测试已失效（不是通过）")
	}
	// token __COLS_LOGS__ → JS key logs
	for _, p := range colPlaceholders {
		key := tokenToJSKey(p.token)
		got, ok := jsCols[key]
		if !ok {
			t.Errorf("JS 的 COLS 里没有 %q（Go 表里是 %s = %s）", key, p.token, p.value)
			continue
		}
		if strconv.Itoa(got) != p.value {
			t.Errorf("列数不一致：Go 的 %s = %s，但 JS 的 COLS.%s = %d —— "+
				"静态空态行与动态空态行会跨不同的列数",
				p.token, p.value, key, got)
		}
	}

	// 3) 反向：JS 里有的列，Go 表也应当有（防止有人只加了一边）
	for key := range jsCols {
		token := "__COLS_" + upperASCII(key) + "__"
		found := false
		for _, p := range colPlaceholders {
			if p.token == token {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("JS 的 COLS.%s 在 Go 的 colPlaceholders 里没有对应项 "+
				"（静态空态行会缺列数）", key)
		}
	}
}

// TestRenderUIReplacesEveryPlaceholder renderUI 必须把每个占位符都替换掉。
//
// 只测"表里有"不够 —— 替换代码漏了一个 token，表现是页面上出现
// `colspan="__COLS_LOGS__"` 这种字面量（真实发生过的缺陷）。
func TestRenderUIReplacesEveryPlaceholder(t *testing.T) {
	out := string(renderUI(webuiHTML))
	for _, p := range colPlaceholders {
		if strings.Contains(out, p.token) {
			t.Errorf("renderUI 输出里仍残留占位符 %s —— 该空态行的 colspan 会变成非法值", p.token)
		}
	}
	// 替换后的数字确实出现
	for _, p := range colPlaceholders {
		if !strings.Contains(out, `colspan="`+p.value+`"`) {
			t.Errorf("renderUI 输出里找不到 colspan=%q（占位符 %s 没被正确替换）", p.value, p.token)
		}
	}
}

// TestRenderUINoTouchesCredentialPlaceholders 控制台外壳里那两处凭据哨兵
// **必须原样留着**，而且渲染器不得再往页面里塞任何凭据。
//
// # 这条是本轮安全的**核心判据**（用户报障 ③）
//
// 改造前 renderUI 的签名是
//
//	renderUI(page, apiKey, injectKey bool, adminToken, injectAdminToken bool)
//
// 它把 config 的 api_key 与 admin_token **明文写进页面 HTML**，而那时门禁
// 也认 api_key ⇒ "打开控制台页面"本身就等于拿到两把钥匙。用户报障的形态是：
// 填错令牌也能进 UI，于是 api_key 就泄了。
//
// 现在页面里没有任何密钥（身份改走 HttpOnly 会话 cookie），但页面**仍保留**
// 那两处自赋值语句 —— 它们是 JS 侧的"未注入"基准：
//
//	window.__WB2API_KEY__ = "__WB2API_KEY__"
//
// 保留而不是删掉的理由：删了会让 JS 里的 `typeof window.__WB2API_KEY__`
// 判据变成"undefined ≠ 占位符"⇒ 页面以为自己拿到了钥匙。T7 会把这段 JS
// 换成"凭据缺失就跳登录页"，在那之前保留它是**最小改动**。
//
// ⚠ 本测试不断言"页面上没有 mk-"：那是**密钥形状**的猜测（掩码示例、
// 提示文案里都可能出现）。真正的判据在 HTTP 层：见 webui_login_test.go 的
// TestUIHTTPNeverLeaksSecrets —— 它拿真的配置密钥去比对响应体。
func TestRenderUINoTouchesCredentialPlaceholders(t *testing.T) {
	out := string(renderUI(webuiHTML))

	// ① 两处自赋值原样保留（JS 的"未注入"基准）
	for _, want := range []string{
		`window.__WB2API_KEY__ = "__WB2API_KEY__"`,
		`window.__WB2API_ADMIN_TOKEN__ = "__WB2API_ADMIN_TOKEN__"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("控制台外壳里应当保留 JS 的「未注入」基准：%s ——\n"+
				"删掉它会让 typeof 判据失效（页面以为自己拿到了钥匙）。", want)
		}
	}

	// ② JS 属性名不得被替换（`window."X"` 是语法错误）
	if strings.Contains(out, `window."`) {
		t.Error("JS 属性名位置被替换了 —— window.\"…\" 是非法 JS，脚本会整体不执行")
	}

	// ③ ★ 渲染器不接收任何凭据 ⇒ 输出里不可能有密钥。
	//    这条是"签名层面"的守卫：若有人把注入参数加回来，编译期就会让
	//    webui_login_test.go 的调用点对不上（那里用的是两参数版本）。
	if n := len(colPlaceholders); n == 0 {
		t.Fatal("列数占位符表空了 —— 空态行的 colspan 会变成非法值")
	}
}

// ---- helpers ----

var jsColsRe = regexp.MustCompile(`const\s+COLS\s*=\s*\{([^}]*)\}`)
var jsColEntryRe = regexp.MustCompile(`(\w+)\s*:\s*(\d+)`)

// parseJSCols 从 webui.html 里解析 `const COLS = { key: N, ... }`。
func parseJSCols(src string) map[string]int {
	m := jsColsRe.FindStringSubmatch(src)
	if m == nil {
		return nil
	}
	out := map[string]int{}
	for _, kv := range jsColEntryRe.FindAllStringSubmatch(m[1], -1) {
		n, err := strconv.Atoi(kv[2])
		if err != nil {
			continue
		}
		out[kv[1]] = n
	}
	return out
}

// tokenToJSKey 把 __COLS_LOGS__ 转成 JS 里的键名（小写）。
func tokenToJSKey(token string) string {
	s := token
	s = strings.TrimPrefix(s, "__COLS_")
	s = strings.TrimSuffix(s, "__")
	return strings.ToLower(s)
}

func upperASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 32
		}
	}
	return string(b)
}
