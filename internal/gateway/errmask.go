// errmask.go —— 出站错误脱敏：把错误文本里的「上游是谁」抹掉。
//
// # 为什么需要它（PLAN 的 G1）
//
// 出站错误长期直接透传上游原文，实测形态：
//
//	Post "https://api.opencsg.com/v1/chat/completions?key=sk-xxxx": dial tcp 10.0.0.5:443: connect: connection refused
//
// 对公网部署，这等于把后端拓扑、厂商口径、有时还有凭据一起交给任何能构造失败请求的人。
// new-api 的对应机制是 relaykit 的 MaskSensitiveInfo。
//
// # 与 new-api 的差异（刻意简化）
//
// new-api 的 MaskSensitiveInfo 接收 upstreamURLs，只对**已知上游 host** 打码 ——
// 它这么做是为了同时给 SSRF 校验留信息（那套完整逻辑我们不要，见 PLAN §0）。
// 本仓不照搬这个前提：
//
//  1. 我们无法可靠枚举「哪些 host 是上游」。一次请求会经过 CDN、反代、重定向，
//     而错误里出现的 host 恰恰常是我们没登记过的那个（307 之后的地址）。
//  2. 出站错误里的 host 本身就是身份，与它「是不是我们登记过的上游」无关。
//
// 所以这里**无差别**对待所有 URL：带 scheme 的一律打码；裸域名按 TLD 白名单识别。
// 少一个参数，也少一类「漏打」。
//
// ⚠ 只用于**出站给客户端**的错误体。日志保留原文 —— 排障需要真实地址，
// 而日志的读者是我们自己（PLAN §4 风险表已写明这条边界）。
//
// # 取舍：保留什么（与 new-api 同款，理由是排障真的用得到）
//
//   - **port 保留**：端口是第一排障信息（443 / 8443 / 7868 一眼看出走没走网关）。
//   - **TLD 尾巴保留**：***.com 与 ***.co.uk 仍能区分自家还是外部供应商，identity 已丢。
//   - **query 参数名保留、值打码**：`alt=sse` 这个名字本身能解释「为什么是流式失败」。
//   - **路径按段打码**（/***/***，不是整段 /***）：段数保留，一眼看出打到了
//     长的 /v1/chat/completions 还是短的 /oauth/token（后者意味着错在刷新令牌）。
//   - **fragment 整体打码**：OAuth 隐式流把 access_token 放在 # 后面，它是凭据不是路径。
//
// 整体例子：
//
//	https://api.openai.com:8443/v1/chat/completions?key=x&alt=sse
//	 -> https://***.com:8443/***/***/***?key=***&alt=***
//
// # 取舍：宁可漏打，不可误伤
//
// 这个函数作用在**所有**错误文本上，一次误伤就把排障信息永久毁了
// （把 handler.go 打成 ***.go、把 12:30:45 打成 [***]）。
// 所以每条正则命中之后都跟一次**验证**，不是匹配到就算：
//
//	IPv4 / IPv6 → 必须 net.ParseIP 通过
//	裸域名      → TLD 必须在白名单里
//
// 白名单**故意**不含 .md/.sh/.so/.pl/.js/.py/.go/.mod 这类在错误文本里更像文件名的后缀。
// 用覆盖率换准确率，这里换得值。
//
// # 为什么不打码邮箱本地部分
//
// `user@example.com` 的域名会被裸域名规则打掉（-> user@***.com），这是顺带的；
// 但本地部分保留：它不是上游身份，抹掉只会让「哪个账号被限流」这类信息消失。
//
// # 已知边界（写在这里，免得后来者以为是 bug 去"修"）
//
//  1. **无 scheme 的 userinfo 不处理**（"user:pass@host" 这种裸形态）。
//     它与「凭据字段」和「邮箱」三者形状重叠，硬判会误伤 `--flag=a@b` 这类参数。
//  2. **单标签主机名不处理**（"dial tcp buildbox:8080"）。
//     `word:数字` 太像 `retry:3` / `step:2`，误伤率高；只对 localhost 开了特例。
//  3. **第三方模块路径会被一起打掉**（"github.com/x/y: not found" -> "***.com/x/y"）。
//     这是「不打码已知上游、而是打码所有域名」的必然代价 —— 我们选择了不漏打。
//     运行期错误里出现模块路径的机会极少（那是编译期的事）。
//  4. **URL 里的 percent-encoded 段不还原**：path 一律按段换成 ***，
//     所以 `/v1/%E4%B8%AD` 与 `/v1/abc` 打出来的结果一样。这没关系 ——
//     段内容本来就是我们要抹掉的东西。
package gateway

import (
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	// maskURLPattern 匹配带 scheme 的绝对 URL。
	//
	// 排除三类字符，每一类都是实测踩出来的：
	//  1. 引号/尖括号/花括号/反引号/竖线/反斜杠/插入符 —— 错误文本里的**定界符**，
	//     吃进去会把 Markdown 定界符或紧随的冒号一起吞掉，输出就变形了。
	//  2. 空白。
	//  3. **所有非 ASCII**（\x{0080} 到 \x{10FFFF}）。本仓错误文案是中文：
	//     "失败于 https://a.com/x、请重试" 里的顿号与后面的中文会被当成 URL 的一部分，
	//     于是整句后半截凭空消失（实测：只剩 "失败于 https://***.com/***/***"）。
	//     URL 里的非 ASCII 本该 percent-encode，真出现在错误里的概率远低于中文标点。
	//     ⚠ 排除**非 ASCII 整段**而不只是标点：中文正文本身也不能进 URL。
	//
	// ⚠⚠ 大小写开关只作用于 scheme（写成内联组 (?i:...)），**不能**是全局 (?i)：
	// Go 的字符类在 case-insensitive 下会做大小写折叠，而 \x{0080}-\x{10FFFF}
	// 这个区间里含 U+017F（长 s）与 U+212A（开尔文符号），它们分别与 s、k 折叠 ——
	// 于是全局 (?i) 会把 ASCII 的 s 和 k 也排除掉。实测后果：
	//
	//	".../v1/models: 502"  ->  ".../v1/model"   ← 尾部 s 被切掉，"s:" 留在 URL 外
	//
	// 这个 bug 只在带 s/k 的路径上出现，测试表里恰好有 models，才没漏过去。
	maskURLPattern = regexp.MustCompile(`(?i:\b(?:https?|wss?)://)[^\s"'<>{}|\\^\x{0080}-\x{10FFFF}]+`)

	// maskBracketedIPv6Pattern 匹配 Go 网络错误里的方括号 IPv6：
	// "dial tcp [2001:db8::1]:443"，允许带 zone（fe80::1%eth0）。
	maskBracketedIPv6Pattern = regexp.MustCompile(`\[([0-9A-Fa-f:.]+)(%[^\]\s]*)?\]`)

	// maskBareIPv6Pattern 是**候选**匹配，不是判定：至少两个冒号才可能像 IPv6，
	// 真正判定交给 net.ParseIP（"12:30:45" 能匹配到这里，但 ParseIP 会否掉）。
	maskBareIPv6Pattern = regexp.MustCompile(`[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]+`)

	// maskIPv4Pattern 同样是候选：四段数字。判定交给 net.ParseIP，
	// 于是 "999.1.1.1"（不是地址，是版本号/时间戳碎片）保持原样。
	maskIPv4Pattern = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)

	// maskDomainPattern 是候选：形如 a.b.c 的标签串。TLD 由白名单二次判定。
	maskDomainPattern = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,24}\b`)

	// maskCredentialPattern 匹配凭证式键值对：
	//   api_key: xxx / api-key=xxx / "access_token": "xxx" / password: xxx
	// 保留键名与引号冒号，只换值 —— 与 new-api 的 api_key:*** 输出一致。
	//
	// ⚠ 键名只收**明确的凭据名**：早先版本收了裸 token，于是
	// "token: expired" 这类散文会被打成 "token: ***"，把原因抹掉反而更难排查。
	maskCredentialPattern = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|client[_-]?secret|secret[_-]?key|password|passwd)(["']?\s*[:=]\s*["']?)([^\s"',;&\}\])]+)`)

	// maskBearerPattern 匹配 Authorization 头原文（上游错误体常把它回显出来）。
	// 限定 ≥6 位且字符集像 base64/点分，避免把 "Bearer auth" 这种散文打掉。
	maskBearerPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+([A-Za-z0-9._~+/=-]{6,})`)

	// maskKeyShapePattern 匹配常见 key 前缀形态（sk-/ak-/rk-/pk- + 足够长的随机串）。
	// 门槛 16 位是为了不碰 "sk-learn" 这类普通词。
	maskKeyShapePattern = regexp.MustCompile(`\b(?:sk|ak|rk|pk)-[A-Za-z0-9_-]{16,}`)

	// maskLocalhostPattern 见 maskLocalhost 的注释。包级编译，避免每次调用重新编译。
	maskLocalhostPattern = regexp.MustCompile(`(?i)\blocalhost\b`)
)

// maskTLDAllowlist 裸域名的 TLD 白名单。
//
// 只收真实会出现在上游地址里的后缀。歧义后缀（.md/.sh/.so/.pl/.js/.py/.go/.mod）
// **刻意不收** —— 见文件头「宁可漏打」。
var maskTLDAllowlist = map[string]bool{
	// 通用
	"com": true, "net": true, "org": true, "edu": true, "gov": true, "mil": true, "int": true,
	"info": true, "biz": true, "name": true, "pro": true, "mobi": true, "asia": true, "xyz": true,
	"top": true, "dev": true, "app": true, "cloud": true, "site": true, "online": true,
	"tech": true, "shop": true, "store": true, "blog": true, "page": true, "link": true,
	"live": true, "run": true, "one": true, "cat": true, "coop": true, "travel": true,
	"aero": true, "jobs": true, "tel": true, "email": true, "agency": true, "solutions": true,
	// 国别（高频）
	"cn": true, "uk": true, "io": true, "ai": true, "co": true, "de": true, "jp": true,
	"fr": true, "ru": true, "br": true, "in": true, "au": true, "ca": true, "us": true,
	"eu": true, "nl": true, "se": true, "no": true, "fi": true, "dk": true, "it": true,
	"es": true, "ch": true, "at": true, "be": true, "cz": true, "pt": true, "gr": true,
	"tr": true, "kr": true, "tw": true, "hk": true, "sg": true, "my": true, "th": true,
	"vn": true, "id": true, "ph": true, "nz": true, "za": true, "mx": true, "ar": true,
	"cl": true, "il": true, "ie": true, "cc": true, "tv": true, "ws": true, "me": true,
}

// maskSecondLevelSuffix 二级公共后缀的前半段（co.uk / com.cn 的 co / com）。
//
// 它有两个用途，缺一不可：
//  1. 打码时整体保留后缀，避免 ***.uk 把 "co" 误当成子域（那是在制造假信息）。
//  2. **幂等**：第一遍把 sub.example.co.uk 打成 ***.co.uk，第二遍会把 "co.uk"
//     自己又当成一个域名再打一遍（-> ***.***.co.uk）。收录这个集合后，
//     "co.uk" 被识别为后缀原样留下，重复脱敏才真的收敛。
var maskSecondLevelSuffix = map[string]bool{
	"co": true, "com": true, "net": true, "org": true, "gov": true, "edu": true,
	"ac": true, "or": true, "ne": true, "go": true, "mil": true, "sch": true,
	"nic": true, "mod": true,
}

// MaskErrorText 把一段**出站错误文本**里的上游身份打掉，其余内容逐字保留。
//
// 打码范围：URL 的 userinfo / host / path / query 值 / fragment、IPv4、IPv6、
// 裸域名、api_key 形态的凭据（含 Bearer/Basic 头）。port 与 query 参数名保留。
//
// 幂等：输出里不再有可被再次命中的形态，重复调用结果不变（有测试钉住）。
// 这一点不是洁癖 —— 错误会被多层包装、可能被多个出口各脱敏一次，
// 不幂等就意味着**脱敏次数**会改变对外文案。
func MaskErrorText(text string) string {
	if text == "" {
		return ""
	}
	text = maskURLs(text)
	text = maskCredentials(text)
	text = maskBearer(text)
	text = maskKeyShapePattern.ReplaceAllString(text, "***")
	text = maskBracketedIPv6(text)
	text = maskBareIPv6(text)
	text = maskIPv4Pattern.ReplaceAllStringFunc(text, func(m string) string {
		if ip := net.ParseIP(m); ip != nil && ip.To4() != nil {
			return "***.***.***.***"
		}
		return m
	})
	text = maskDomainPattern.ReplaceAllStringFunc(text, func(m string) string {
		if !isMaskableDomain(m) {
			return m
		}
		return maskDomain(m)
	})
	// localhost 必须**最后**处理：前面的 URL 掩码会把 "http://localhost:7868" 的
	// host 换成 "***"，而裸的 "localhost:7868"（Go 的 dial tcp 错误形态）没有点，
	// 域名规则碰不到它 —— 它会原样留在错误里，暴露「错误出在网关自身」。
	return maskLocalhost(text)
}

// MaskError 是 MaskErrorText 的 error 版本，省得调用方每次自己 .Error()。
//
// nil 返回空串而不是 "<nil>"：出口层拿到的 nil 就是「没有错误」，
// 让它带着字符串 <nil> 出现在响应体里是另一种信息泄漏（实现细节）。
func MaskError(err error) string {
	if err == nil {
		return ""
	}
	return MaskErrorText(err.Error())
}

// maskURLs 对每个带 scheme 的 URL 打码。
//
// 解析失败时**原样返回**：解析不出的串更可能是散文里碰巧像 URL 的东西，
// 把它改坏比漏打更糟（输出变形会让整条错误失去可读性）。
func maskURLs(s string) string {
	return maskURLPattern.ReplaceAllStringFunc(s, maskOneURL)
}

func maskOneURL(match string) string {
	body, suffix := trimURLSuffix(match)
	u, err := url.Parse(body)
	if err != nil || u.Host == "" {
		return match
	}

	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.User != nil {
		// userinfo 是凭据（basic auth、client_id:secret），与 host 无关，一律抹掉。
		b.WriteString("***@")
	}
	b.WriteString(maskHostValue(u.Hostname()))
	if port := u.Port(); port != "" {
		b.WriteString(":")
		b.WriteString(port)
	}
	b.WriteString(maskURLPath(u.Path))
	b.WriteString(maskRawQuery(u.RawQuery))
	if u.Fragment != "" {
		b.WriteString("#***")
	}
	b.WriteString(suffix)
	return b.String()
}

// trimURLSuffix 把句尾标点从 URL 上剥掉再交给解析器。
//
// 错误文本里的 URL 通常被引号或句号包围（connect to https://a.com/x.），
// 这些字符本身也是合法 URL 字符，不剥掉就会变成路径的一部分。
//
// ⚠ **全角标点必须一起剥**：本仓的错误文案是中文，形如
// 「上游返回 HTTP 502（响应体片段：Post "https://x/v1": ...）」。
// 全角括号是合法 UTF-8 字节，不会被 URL 正则的排除集挡住 —— 它会跟着进 path，
// 再被整段换成 *** 而**凭空消失**。少一个右括号不会报错，只会让文案看着被截断。
func trimURLSuffix(s string) (body, suffix string) {
	end := len(s)
	for end > 0 {
		// 先试全角（多字节），再试 ASCII 单字节 —— 顺序不能反，
		// 否则 s[end-1] 会命中全角字符的某个尾字节并把它当成标点切掉半截。
		if r, size := utf8.DecodeLastRuneInString(s[:end]); size > 0 && strings.ContainsRune(maskURLTrailingPunct, r) {
			end -= size
			continue
		}
		c := s[end-1]
		if strings.IndexByte(".,;:!?'", c) >= 0 {
			end--
			continue
		}
		// 右括号只在**不配对**时才算标点："(see https://a.com/x)" 的右括号是标点，
		// 而 "https://a.com/x_(y)" 的右括号属于路径。
		if c == ')' && strings.Count(s[:end], ")") > strings.Count(s[:end], "(") {
			end--
			continue
		}
		break
	}
	return s[:end], s[end:]
}

// maskURLTrailingPunct URL 后紧跟的中文标点。
//
// 只收**收尾类**标点（右括号、句号、逗号、顿号、分号、冒号、问感叹号），
// 不收「（」这类开头标点 —— 它们出现在 URL 前面，本来就不在匹配范围内。
var maskURLTrailingPunct = "）】，。、；：？！》」』"

// maskHostValue 打码单个 host（不含 port）。IP 抹成定长占位，域名保留后缀尾巴。
func maskHostValue(host string) string {
	if host == "" {
		return "***"
	}
	if ip := net.ParseIP(strings.TrimSuffix(host, ".")); ip != nil {
		if ip.To4() != nil {
			return "***.***.***.***"
		}
		return "[***]"
	}
	return maskDomain(host)
}

// maskDomain 把 api.openai.com 打成 ***.com、sub.domain.co.uk 打成 ***.co.uk。
//
// 与 new-api 的差异：它按标签数打多个 ***（***.***.co.uk），刻意暴露子域层级。
// 这里不打层级 —— 层级本身也是身份（internal-auth.corp.example.com 里的
// internal-auth 比域名更能说明问题），而排障只需要知道后缀。
func maskDomain(host string) string {
	host = strings.TrimSuffix(host, ".")
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return "***"
	}
	return "***." + strings.Join(maskDomainTail(parts), ".")
}

// maskDomainTail 决定保留几个后缀标签。
func maskDomainTail(parts []string) []string {
	if len(parts) < 2 {
		return parts
	}
	last, prev := parts[len(parts)-1], parts[len(parts)-2]
	if len(last) == 2 && len(prev) <= 3 && maskSecondLevelSuffix[strings.ToLower(prev)] {
		return []string{prev, last}
	}
	return []string{last}
}

// isMaskableDomain 判定候选串是不是**裸域名**。
//
// 两道判据缺一不可：
//   - TLD 在白名单里（挡住 handler.go / config.yaml 这类文件名）
//   - 不是「二级公共后缀本身」（挡住把已打码结果 ***.co.uk 再打一遍 —— 幂等）
func isMaskableDomain(cand string) bool {
	parts := strings.Split(strings.TrimSuffix(cand, "."), ".")
	if len(parts) < 2 {
		return false
	}
	last := strings.ToLower(parts[len(parts)-1])
	if !maskTLDAllowlist[last] {
		return false
	}
	if len(parts) == 2 && len(last) == 2 && maskSecondLevelSuffix[strings.ToLower(parts[0])] {
		// co.uk / com.cn 本身就是后缀，不是可识别的域名 —— 留下（见 maskSecondLevelSuffix）。
		return false
	}
	return true
}

// maskURLPath 逐段打码，**保留段数**（/v1/chat/completions -> /***/***/***）。
//
// # 为什么保留段数（测试钉住的口径）
//
// 段数本身**不是**敏感信息 —— 泄漏身份的是 host（哪家上游）与路径段名
// （"/v2/enterprises/personal/models" 直接点名了产品）。把段名换成 ***
// 之后，"/***/***/***" 只说明"这里原本有三层路径"，排障时还能看出
// 客户端请求的是 v1 风格还是嵌套 API，而看不出是谁。
//
// ⚠ 我一度改成压成一段 "/***"（怕段数是指纹），测试立刻红了 ——
// 而且更糟：压成一段会让 "/v1/models" 与 "/v2/enterprises/personal/models"
// 长得一模一样，排障时无法区分是哪个接口出错。
func maskURLPath(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if seg != "" {
			parts[i] = "***"
		}
	}
	return strings.Join(parts, "/")
}

func maskRawQuery(q string) string {
	if q == "" {
		return ""
	}
	segs := strings.Split(q, "&")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		name, _, hasValue := strings.Cut(seg, "=")
		if hasValue {
			out = append(out, name+"=***")
			continue
		}
		// 无值参数（?stream）保留原名：它本身不携带内容。
		out = append(out, name)
	}
	if len(out) == 0 {
		return "?***"
	}
	return "?" + strings.Join(out, "&")
}

// maskCredentials 抹掉凭证键值对的值。
//
// 长度门槛是**第二道闸**：`api_key: not set` 里的 "not" 只有 3 个字符，
// 不是凭据；把它打成 *** 会让人以为「这里本来有个 key」。真凭据不会这么短。
// 这条门槛同时保证了幂等（第二次跑时值已经是三个星号）。
func maskCredentials(s string) string {
	return maskCredentialPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := maskCredentialPattern.FindStringSubmatch(m)
		if len(sub) < 4 || len(sub[3]) < 6 {
			return m
		}
		return sub[1] + sub[2] + "***"
	})
}

// maskBearer 抹掉 Authorization 头里的 token，保留头名与方案名。
//
// ⚠ 不用 ReplaceAllString 的模板展开：Go 的模板会把 token 里的美元符号当引用，
// 遇到某些畸形 header 会输出字面量 <no value> —— 那比漏打更糟（错误体变形）。
func maskBearer(s string) string {
	return maskBearerPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := maskBearerPattern.FindStringSubmatch(m)
		if len(sub) < 3 || len(sub[2]) < 6 {
			return m
		}
		return sub[1] + " ***"
	})
}

// maskBracketedIPv6 处理 "dial tcp [2001:db8::1]:443" 这种带方括号的形态。
// zone（%eth0）一并抹掉 —— 网卡名也是内网拓扑。
func maskBracketedIPv6(s string) string {
	return maskBracketedIPv6Pattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := maskBracketedIPv6Pattern.FindStringSubmatch(m)
		if len(sub) < 2 || net.ParseIP(sub[1]) == nil {
			return m
		}
		return "[***]"
	})
}

// maskBareIPv6 处理不带方括号的 IPv6（"lookup 2001:db8::1: no such host"）。
//
// 候选串先剥掉句尾的 '.'/':'，再交给 net.ParseIP：一个误判会把 10:30:45
// 这种时间戳打掉，而排障时那恰恰是最需要的东西。
func maskBareIPv6(s string) string {
	return maskBareIPv6Pattern.ReplaceAllStringFunc(s, func(m string) string {
		end := len(m)
		for end > 0 && (m[end-1] == '.' || m[end-1] == ':') {
			end--
		}
		if end == 0 || net.ParseIP(m[:end]) == nil {
			return m
		}
		return "[***]" + m[end:]
	})
}

// maskLocalhost 抹掉 localhost。
//
// 它没有点，裸域名规则碰不到；但 "dial tcp localhost:7868" 明确暴露了
// 「错误发生在网关自己身上」，对攻击者是有效信息。
func maskLocalhost(s string) string {
	return maskLocalhostPattern.ReplaceAllString(s, "***")
}
