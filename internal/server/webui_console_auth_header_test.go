package server

import (
	"regexp"
	"strings"
	"testing"
)

// hasGlobalKeyAssign 判断源码里有没有"给**全局 KEY** 赋值"。
//
// ⚠ Go 的 regexp（RE2）**不支持后顾**（`(?<!CONSOLE_)` 会 panic，我第一版就这么炸的），
// 所以手写等价判据：逐个子串 `KEY`，看它后面（跳过空白）是不是 `=`，且前面 8 个字符不是 `CONSOLE_`。
// 这样 `CONSOLE_KEY = r.admin_key_full`（期望写法）不会误报，`KEY = r.admin_key_full`（旧写法）会被抓住。
func hasGlobalKeyAssign(s string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], "KEY")
		if j < 0 {
			return false
		}
		j += i
		rest := s[j+3:]
		k := 0
		for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t') {
			k++
		}
		if k < len(rest) && rest[k] == '=' {
			if j < 8 || s[j-8:j] != "CONSOLE_" {
				return true
			}
		}
		i = j + 3
	}
}

// TestWebUIConsoleRequestsCarryNoV1Bearer 控制台面的请求不得带 /v1 的 Bearer，且 401 不得自毁重载。
//
// # 事故链（线上实测：10 秒 58 次导航、52 次 401 /status）
//
//	① boot 调 ensureConsoleKey()（不 await）⇒ admin('/admin/apikeys') ⇒ `KEY = admin_key_full`
//	   —— 而 KEY 的本意只是"用户在顶栏粘的**对话**密钥"
//	② api(path) 的 headers 无条件并 `hdr()` ⇒ KEY 非空后**每个** api() 都带 Bearer <api_key>
//	③ 服务端凭据优先级 Bearer > ?token= > Cookie
//	   ⇒ bearer 是 api_key（不是 admin_token）⇒ 校验失败 ⇒ 401
//	   ⇒ 且 Identify 在"cred 非空但校验失败"时**不会**退回 cookie（那个 cookie 本来有效）
//	④ api() 见 401 且路径是 /admin/* 或 /status ⇒ toLogin() ⇒ location.replace('/ui')
//	⑤ 重载后又 ensureConsoleKey… ⇒ **无限自毁**
//
// 真值：api_key == admin_token 的部署把它掩盖了；两者不同必炸。
//
// # 五条判据
//
//	1 api() 不得无条件并 hdr()，必须由 opt.v1 === true 显式开关
//	2 /v1/models 与 /v1/chat/completions 的调用点必须带 v1: true（否则对话面反而坏）
//	3 ensureConsoleKey 体内不得给全局 KEY 赋值（只能写 CONSOLE_KEY）
//	4 toLogin 必须有 60 秒窗口熔断（打断 401 → 重载 → 又 401）
//	5 全文不得再有 `KEY = r.admin_key_full`（旧写法）
func TestWebUIConsoleRequestsCarryNoV1Bearer(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)

	// 1
	if strings.Contains(html, "adminHdr(), hdr(), opt.headers") {
		t.Error("api() 仍**无条件**并入 hdr() ⇒ 每个控制台请求都会带上 Bearer <api_key>，" +
			"压掉本来有效的 admin_token cookie（服务端 Bearer 优先级更高，且 Identify 不 fall-through）⇒ 401 → 重载 → 自毁")
	}
	if !strings.Contains(html, "opt.v1 === true ? hdr() : {}") {
		t.Error("api() 缺少显式开关写法 `opt.v1 === true ? hdr() : {}`（只有传了 v1 才该带对话密钥）")
	}
	// 2
	for _, call := range []string{"api('/v1/models')", "api('/v1/chat/completions'"} {
		if strings.Contains(html, call) && !strings.Contains(html, call+" , {") && !strings.Contains(html, call+", {") {
			t.Errorf("调用点 %s 没带 v1: true（对话面会拿不到密钥）", call)
		}
	}
	if strings.Contains(html, "api('/v1/models')") {
		t.Error("api('/v1/models') 仍无 opt ⇒ 应写成 api('/v1/models', { v1: true })")
	}
	if !strings.Contains(html, "v1: true") {
		t.Error("全文找不到 v1: true（判据失效或没改）")
	}
	// 3
	gi := strings.Index(html, "async function ensureConsoleKey")
	if gi < 0 {
		t.Fatal("找不到 ensureConsoleKey")
	}
	body := html[gi:]
	if j := strings.Index(body, "\n  function "); j > 0 {
		body = body[:j]
	}
	// ⚠ 判据要用**负向后顾**：`CONSOLE_KEY = r.admin_key_full` 里也含子串 "KEY ="
	//（第一版就是这么误报的）。意图是"不许给**全局 KEY** 赋值"，所以排除 CONSOLE_ 前缀。
	if hasGlobalKeyAssign(body) {
		t.Error("ensureConsoleKey 仍给全局 KEY 赋值 ⇒ 它会把对话密钥偷偷加到所有控制台请求上（自毁链的源头）")
	}
	if !strings.Contains(body, "CONSOLE_KEY") {
		t.Error("ensureConsoleKey 应只写 CONSOLE_KEY（仅供 /v1 调用点用）")
	}
	// 4
	ti := strings.Index(html, "function toLogin(why)")
	if ti < 0 {
		t.Fatal("找不到 toLogin")
	}
	tbody := html[ti:]
	if j := strings.Index(tbody, "\n  async function "); j > 0 {
		tbody = tbody[:j]
	}
	if !strings.Contains(tbody, "wb2api.lastReloginAt") {
		t.Error("toLogin 缺 60 秒窗口熔断（wb2api.lastReloginAt）⇒ 一个接口写错就能让控制台无限重载自毁")
	}
	if !strings.Contains(tbody, "60000") {
		t.Error("toLogin 的熔断窗口应为 60000 毫秒（60 秒）")
	}
	// 5
	// 同上：`CONSOLE_KEY = r.admin_key_full` 是**期望写法**，不能用子串判据把它也抓了。
	// ⚠ 这里只能判"配置里的 api_key 写进**全局** KEY"这一种旧写法：
	//   文件里还有两处 `KEY = $('key').value…`（用户手粘对话密钥）是**设计内**的，
	//   用通用的 hasGlobalKeyAssign(html) 会把它们一起抓了（第一版就误报过）。
	if strings.Contains(html, "KEY = r.admin_key_full") && !strings.Contains(html, "CONSOLE_KEY = r.admin_key_full") {
		t.Error("仍有旧写法 `KEY = r.admin_key_full`（全局 KEY 被配置里的 api_key 覆盖 = 自毁链第 ② 步）")
	}
	// 额外：CONSOLE_KEY 必须先声明（hdr() 里会读它）
	di := strings.Index(html, "let CONSOLE_KEY")
	hi := strings.Index(html, "const hdr = ()")
	if di < 0 {
		t.Error("缺 `let CONSOLE_KEY = ''` 声明")
	} else if hi >= 0 && di > hi {
		t.Error("CONSOLE_KEY 的声明必须排在 hdr() 之前（hdr 会读它）")
	}
	if hi >= 0 && !strings.Contains(html[hi:hi+160], "CONSOLE_KEY") {
		t.Error("hdr() 应优先用户手粘的 KEY，否则回落 CONSOLE_KEY")
	}
}

// TestWebUIApiCallV1FlagIsComplete 每个 api() 调用点的 v1 开关都要**正反都对**（T-376②）。
//
// # 为什么不能只盯字面两处
//
// 上一版的护栏只断言 `/v1/models` 与 `/v1/chat/completions` 这两串字面存在 v1: true。
// 谁新加一个 api() 调用（或把 hdr() 用到别处），护栏都抓不到 —— 而这两个方向都是**线上事故**：
//
//	· /v1/* 少了 v1: true ⇒ 请求不带 Bearer ⇒ 对话面/模型目录默默坏掉
//	· 控制台面（/admin/*、/status）多带了 v1: true ⇒ 带上 Bearer <api_key> ⇒ 压掉 cookie 身份
//	  ⇒ 401 ⇒ toLogin ⇒ 重载 ⇒ 无限自毁（P0 的原形）
//
// ⇒ 判据：扫出**所有** api('…') 调用，按 path 前缀正反双向判定。
func TestWebUIApiCallV1FlagIsComplete(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)
	re := regexp.MustCompile(`api\(\s*['"]([^'"]+)['"]\s*(,\s*\{[^}]*\})?`)
	ms := re.FindAllStringSubmatch(html, -1)
	if len(ms) < 2 {
		t.Fatalf("只扫到 %d 个 api() 调用 ⇒ 判据失效（正则或写法变了）", len(ms))
	}
	v1Count, ctrlCount := 0, 0
	for _, m := range ms {
		path, opt := m[1], m[2]
		hasV1 := strings.Contains(opt, "v1: true")
		if strings.HasPrefix(path, "/v1/") {
			v1Count++
			if !hasV1 {
				t.Errorf("api('%s') 是 /v1 面却**没**带 v1: true ⇒ 请求不会带 Bearer ⇒ 该功能默默坏掉", path)
			}
		} else {
			ctrlCount++
			if hasV1 {
				t.Errorf("api('%s') 是控制台面却带了 v1: true ⇒ 会带上 Bearer <api_key> 压掉 cookie 身份 ⇒ 401 → 重载 → 自毁（P0 复发）", path)
			}
		}
	}
	if v1Count == 0 || ctrlCount == 0 {
		t.Errorf("没有同时覆盖两类调用（/v1 面 %d 个、控制台面 %d 个）⇒ 判据退化", v1Count, ctrlCount)
	}
}

// TestWebUIBearerHeaderOnlyInHdr 请求头键形式的 Authorization 只允许出现在 hdr() 里（T-377①修订）。
//
// # 为什么上一版会误报（启发式必然脆）
//
// 上一版试图用"上一非空行含 card(" 猜续行、用 card/curl/-H/textContent 做豁免 ——
// 那些都是**猜测**：真正的请求头与给人看的展示文本，其实在**语法形态**上就不同：
//
//	真·请求头（对象字面量的键）：'Authorization': 'Bearer ' + …      ← 冒号在**引号外**
//	假·展示文本（整串字符串）：  'Authorization: Bearer ' + …        ← 冒号在**引号内**
//
// ⇒ 一条正则即可区分，不需要任何豁免、续行猜测或行号白名单：
//
//	找 `['"]Authorization['"]\s*:`（键式），而不是 `Authorization:\s*Bearer`（串式）。
func TestWebUIBearerHeaderOnlyInHdr(t *testing.T) {
	b, err := readFrontendSource(t)
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)
	if !strings.Contains(html, "const hdr =") {
		t.Fatal("找不到 hdr() 定义 ⇒ 判据失效")
	}
	reHeaderKey := regexp.MustCompile(`['"]Authorization['"]\s*:`)
	lines := strings.Split(html, "\n")
	bad := []string{}
	for i, l := range lines {
		if !reHeaderKey.MatchString(l) {
			continue
		}
		// 唯一允许处：hdr() 自己的定义体（按**内容**判，不按行号）
		if strings.Contains(l, "const hdr =") {
			continue
		}
		bad = append(bad, "L"+itoa(i+1)+": "+strings.TrimSpace(l))
	}
	if len(bad) > 0 {
		t.Errorf("hdr() 之外出现**请求头键**形式的 Authorization（键式：引号内是键名、冒号在引号外）——\n"+
			"那等于绕过 hdr() 手写鉴权头（P0 就是这么来的）。应一律走 hdr()：\n  %s", strings.Join(bad, "\n  "))
	}
	// 存活检查：hdr() 必须是"Bearer + 键"的唯一构造点
	if !strings.Contains(html, "'Bearer ' + (KEY || CONSOLE_KEY)") {
		t.Error("hdr() 定义体里看不到 Bearer 的构造 ⇒ 判据失效")
	}
	// 反向存活检查：展示文本（冒号在引号内）必须**不**被键式正则命中
	if reHeaderKey.MatchString("'Authorization: Bearer '") {
		t.Error("键式正则把展示文本也命中了 ⇒ 判据失效（形态区分不成立）")
	}
}
