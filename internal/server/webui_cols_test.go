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
	out := string(renderUI([]byte(webuiHTML), "test-key", true))
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

// TestRenderUIKeyInjection 密钥注入只在明确要求时发生，且不动 JS 守卫里的哨兵。
//
// # 为什么单独测这条
//
// 上一轮后端评审怀疑过"注入是死代码"。实测**注入正常**，但它猜错的理由
// 是对的担忧：Go 只替换**带双引号**的那一处（赋值语句），
// 而 JS 守卫里 `!== '__WB2API_KEY__'`（单引号）必须**保持原样** ——
// 它正是用来判断"是否已注入"的比对基准。替换了它反而会让 INJECTED 失真。
func TestRenderUIKeyInjection(t *testing.T) {
	// 注入开启：赋值处变成真 key
	out := string(renderUI([]byte(webuiHTML), "SECRET-AAA", true))
	if !strings.Contains(out, `window.__WB2API_KEY__ = "SECRET-AAA"`) {
		t.Error("开启注入时，赋值语句没有被换成真实密钥")
	}
	// 守卫里的单引号哨兵必须保留（它是对比基准，不是待替换值）
	if !strings.Contains(out, `!== '__WB2API_KEY__'`) {
		t.Error("JS 守卫里的单引号哨兵被替换了 —— 那会让 INJECTED 判断失真")
	}

	// 注入关闭（非本机访问）：哨兵保持原样，页面按"需手输 Key"处理
	off := string(renderUI([]byte(webuiHTML), "SECRET-AAA", false))
	if !strings.Contains(off, `window.__WB2API_KEY__ = "__WB2API_KEY__"`) {
		t.Error("关闭注入时，赋值语句不应被替换")
	}
	if strings.Contains(off, "SECRET-AAA") {
		t.Error("关闭注入时页面上不应出现密钥明文")
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
