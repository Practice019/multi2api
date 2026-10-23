// urlguard.go —— 渠道 base_url 的 SSRF 防护：挡住指向本机 / 内网 / 云元数据的地址。
//
// # 为什么需要它（PLAN 的 G7）
//
// 渠道的 base_url 是**用户填的**（internal/admin/channels.go 的表单），而网关会
// 拿它去发携带凭据的请求。这构成一条教科书式的 SSRF 链：
//
//	POST /admin/channels {"base_url":"http://169.254.169.254/latest/meta-data/"}
//	→ 网关带着自己的网络身份去访问云元数据 → 实例凭据被写进"上游响应"
//
// 同样的手法可以打 127.0.0.1 上的本机管理面、10.0.0.0/8 里的内网服务、
// 或者任何"只允许内网访问"的端点。判据不是"用户会不会这么干"，
// 而是"只要存在这个地址，它就一定会被扫到" —— 公网部署的网关每天都被扫。
//
// new-api 的对应机制是 common/ssrf_protection.go 的 ValidateURL +
// service/http_client.go 里的 validateURLWithCurrentFetchSetting。
// 本文件是它的**简化移植**：只保留"按解析后的主机判定 + 主机/CIDR 白名单"，
// 不搬域名/IP 双黑名单模式、端口白名单、以及运行时 fetch_setting 热更新
// （本仓没有那套设置中心，配置在装配层一次性注入）。
//
// # 为什么在「解析后」校验，而不是查字符串
//
// 字符串匹配在 URL 上一定会被绕过 —— 同一个地址有无穷多种写法：
//
//	http://127.0.0.1/          http://127.1/            http://2130706433/
//	http://0x7f.0.0.1/         http://0177.0.0.1/       http://[::ffff:127.0.0.1]/
//	http://127.0.0.1./         http://LOCALHOST/        http://127.0.0.1.nip.io/
//
// 前六种在 glibc 的 inet_aton / Winsock 里都是 127.0.0.1（见 parseLooseIPv4 的注释）。
// 所以判据必须是「先按 RFC 3986 解析出 Hostname，再判定这个主机」。这也是
// new-api 的做法（ValidateURL 里先 url.Parse 再取 Hostname）。
//
// # ⚠ 本文件的边界：它**不能**替代拨号层的校验（DNS 重绑定）
//
// 域名到 IP 的映射在**拨号那一刻**才发生，而本文件在**保存/发起请求之前**执行。
// 一个攻击者可控的域名可以在两次解析之间返回不同答案：
//
//	保存渠道时  api.evil.com -> 8.8.8.8      （通过校验）
//	真正请求时  api.evil.com -> 127.0.0.1    （打到本机）
//
// 这类攻击（DNS rebinding）只能在拨号层解决 —— 拿到**即将连接的那个 IP** 再判一次。
// 所以本文件提供 ValidateResolvedHostIP(host, ip)：装配层可以把它挂进
// net.Dialer.Control 或自定义 DialContext，用**同一套判据**在拨号时复核。
// 只做前者是"挡住手滑与字面量扫描"，做了后者才是"挡住有动机的攻击者"。
//
// # 为什么错误信息要写清"为什么拒"
//
// 这个校验的错误会直接回到填表的人面前（管理台 / API 调用方）。只回
// "invalid base_url" 的后果是：填 192.168.1.10 的自建上游用户不知道
// 是"地址写错了"还是"内网被禁"，于是反复改端口、改路径、找我们报 bug。
// 所以每个拒绝都带上：哪个主机、属于哪一类地址、以及**怎么放行**（白名单）。
// new-api 的 ipAccessError 同样是 "private IP address not allowed: %s resolves to %s"
// 这个形状。
//
// # 与 errmask 的关系
//
// 本文件产出的错误里**只出现 scheme://host:port**，不含 userinfo / path / query
// （见 redactGuardURL 的注释）—— 所以它可以直接出现在管理台或日志里，
// 不需要再过一遍 MaskErrorText。这是刻意的：SSRF 拒绝发生在**入站**，
// 而 errmask 处理的是**出站**错误，两者的脱敏口径不该混在一起。
package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// URLGuardReason 拒绝原因分类。
//
// # 为什么要有分类，而不是只给一段字符串
//
// 三个下游都要按原因分支，而它们都不能去匹配中文文案：
//
//	管理台表单      内网地址 → 提示"加入白名单"；非 http scheme → 提示"只支持 http/https"
//	装配层/告警     区分"有人在扫内网"（metadata/loopback）与"用户填错了"（malformed）
//	集成测试        断言被拒的**依据**，而不是断言某句文案 —— 文案会改，依据不会
//
// 所以 Error() 的中文正文可以随便润色，而 Reason 一旦发布就不再改（同 errcode.go 的取向）。
type URLGuardReason string

const (
	// URLGuardReasonMalformed 地址本身不合法：没有 scheme、没有主机名、端口越界、
	// 或者主机名是"纯数字却不是合法 IPv4"这类解析器口径不一致的形态。
	URLGuardReasonMalformed URLGuardReason = "malformed"

	// URLGuardReasonScheme 协议不在 http/https 之内（file / gopher / ftp / ws / unix …）。
	//
	// 为什么必须单独判：SSRF 的老牌利用链（如 gopher 打 Redis、file 读本地文件）
	// 走的正是"非 http 协议"这条路。base_url 只该是 HTTP 上游。
	URLGuardReasonScheme URLGuardReason = "scheme"

	// URLGuardReasonLoopback 回环：127.0.0.0/8、::1、localhost。
	//
	// 这类地址最危险 —— 它指向**网关自己**（管理面、metrics、admin 端口）。
	URLGuardReasonLoopback URLGuardReason = "loopback"

	// URLGuardReasonPrivate 私网：10/8、172.16/12、192.168/16，以及 CGNAT 100.64/10。
	URLGuardReasonPrivate URLGuardReason = "private"

	// URLGuardReasonLinkLocal 链路本地：169.254.0.0/16、fe80::/10。
	URLGuardReasonLinkLocal URLGuardReason = "link_local"

	// URLGuardReasonULA IPv6 唯一本地地址 fc00::/7。
	//
	// 单列而不并进 private：IPv6 的内网段与 IPv4 不是同一套心智模型，
	// 排障时"fd00::/8 是 ULA"这条信息比"私网"精确。
	URLGuardReasonULA URLGuardReason = "ula"

	// URLGuardReasonMetadata 云厂商实例元数据服务（IMDS）。
	//
	// 单列的理由：它不是"内网"的一种普通情况，而是**凭据泄露**路径 ——
	// 命中它意味着请求方在尝试读这台机器的云身份。运维看到这条应当警觉，
	// 而看到"private"只会以为是配置写错。
	URLGuardReasonMetadata URLGuardReason = "metadata"

	// URLGuardReasonReserved 保留/特殊用途：0.0.0.0/8、组播、TEST-NET、
	// 240.0.0.0/4、文档段、NAT64 等。
	//
	// 它们连不通，但**不该连**：一个能连上的"保留"地址通常意味着
	// 网络里有奇怪的劫持/隧道（如 6to4、NAT64 把请求转进内网）。
	URLGuardReasonReserved URLGuardReason = "reserved"

	// URLGuardReasonInternalName 明显的内网主机名：单标签主机、.local/.internal/
	// .lan/.home.arpa，以及 nip.io / lvh.me 这类"域名即 IP"的通配服务。
	//
	// # 为什么域名也要挡（"字面量 IP 挡完不就够了"）
	//
	// 不够。以下三种都不含 IP 字面量，却直通内网：
	//
	//	http://buildbox:8080        单标签名，只能由内网 DNS / hosts 解析
	//	http://db.local/            RFC 6762 mDNS 名字
	//	http://127.0.0.1.nip.io/    nip.io 把域名里的 IP 原样解析回去
	//
	// 前两种是"用户自己的内网"（合理需求，走白名单），后一种是"公开的绕过服务"
	// （不该有合法用途）。三种一律拒，理由统一，放行方式统一（白名单）。
	URLGuardReasonInternalName URLGuardReason = "internal_name"

	// URLGuardReasonDNSAlias "域名即 IP" 的通配解析服务（nip.io / sslip.io / lvh.me …）。
	//
	// # 为什么它必须有独立的 reason（而不是并进 internal_name）
	//
	// 两者的处置**不同**：internal_name 在 AllowPrivate 下会被放行
	// （自建内网上游就是靠单标签名 / .lan 访问的，那是合理需求）；
	// 而这类服务在任何配置下都不放行 —— 它们没有任何"内网上游"的用途，
	// 唯一作用是**把字面量 IP 藏进域名**绕开检查。若并进 internal_name，
	// 那么打开 allow_private 之后就等于打开了 http://127.0.0.1.nip.io/。
	URLGuardReasonDNSAlias URLGuardReason = "dns_alias"
)

// String 返回给人看的中文标签（进错误正文与管理台）。
func (r URLGuardReason) String() string {
	switch r {
	case URLGuardReasonMalformed:
		return "地址格式不合法"
	case URLGuardReasonScheme:
		return "协议不被支持"
	case URLGuardReasonLoopback:
		return "回环地址"
	case URLGuardReasonPrivate:
		return "私网地址"
	case URLGuardReasonLinkLocal:
		return "链路本地地址"
	case URLGuardReasonULA:
		return "IPv6 唯一本地地址（ULA）"
	case URLGuardReasonMetadata:
		return "云厂商元数据地址"
	case URLGuardReasonReserved:
		return "保留/特殊用途地址"
	case URLGuardReasonInternalName:
		return "内网主机名"
	case URLGuardReasonDNSAlias:
		return "「域名即 IP」的通配解析服务"
	}
	return string(r)
}

// ErrBaseURLBlocked 所有 SSRF 拒绝的哨兵。
//
// # 为什么需要一个哨兵（错误已经是 *URLGuardError 了）
//
// 调用方要能**不关心细节**地做一件事：把"被安全策略拒了"翻译成一个 400 + 明确的
// error code（见 errcode.go 的 CodeInvalidRequest）。要求每个调用点都
// errors.As 到具体类型，等于把"这层是安全拒绝"这件事散落到各处。
//
//	errors.Is(err, gateway.ErrBaseURLBlocked)   → 400 invalid_request
//	errors.As(err, &guardErr)                   → 取 Reason 做更细的分支/提示
var ErrBaseURLBlocked = errors.New("base_url 被安全策略拒绝（SSRF 防护）")

// URLGuardError 一次具体的拒绝。
//
// 字段全部导出：它会进管理台的错误提示、也可能被上层序列化成诊断信息。
type URLGuardError struct {
	// Input 用户填的地址，**已去掉 userinfo / path / query**（见 redactGuardURL）。
	Input string `json:"input,omitempty"`
	// Host 被判定为不安全的主机（IP 字面量或域名）。
	Host string `json:"host,omitempty"`
	// Reason 拒绝分类（见 URLGuardReason）。
	Reason URLGuardReason `json:"reason"`
	// Detail 一句话说清"这个地址属于哪一类、为什么不行"。
	Detail string `json:"detail"`
	// Hint 怎么才能放行（只在真的能放行时出现，例如白名单 / 换 scheme）。
	Hint string `json:"hint,omitempty"`
}

// Error 拼出**给人看**的文案。
//
// 形状：base_url 被拒绝（SSRF 防护）：http://127.0.0.1:8080 —— 主机 "127.0.0.1" 属于回环地址：…；…
//
// 顺序刻意是「先是什么地址、再为什么、最后怎么办」：填表的人第一眼要认出的
// 是**自己填的那个地址**（否则他不知道是哪条渠道出错），第二眼才是原因。
func (e *URLGuardError) Error() string {
	if e == nil {
		return ErrBaseURLBlocked.Error()
	}
	var b strings.Builder
	b.WriteString("base_url 被拒绝（SSRF 防护）：")
	if e.Input != "" {
		b.WriteString(e.Input)
		b.WriteString("；")
	}
	b.WriteString("主机 ")
	b.WriteString(quoteHost(e.Host))
	b.WriteString(" 属于")
	b.WriteString(e.Reason.String())
	if e.Detail != "" {
		b.WriteString("（")
		b.WriteString(e.Detail)
		b.WriteString("）")
	}
	if e.Hint != "" {
		b.WriteString("；")
		b.WriteString(e.Hint)
	}
	return b.String()
}

// Unwrap 让 errors.Is(err, ErrBaseURLBlocked) 成立。
func (e *URLGuardError) Unwrap() error { return ErrBaseURLBlocked }

// quoteHost 空主机不输出一对空引号（"主机 """ 这种）。
func quoteHost(h string) string {
	if h == "" {
		return "（缺失）"
	}
	return strconv.Quote(h)
}

// URLGuardConfig base_url 校验的配置（装配层从配置文件解析后传进来）。
//
// 刻意做成扁平结构 + json tag：它会被直接嵌进本仓的 config 结构体
// （cmd/server/config.go 那套 `json:"..."` 风格），不需要额外的转换层。
type URLGuardConfig struct {
	// AllowPrivate 放开**所有**私网/链路本地/ULA/保留地址（对应 new-api 的 allowPrivateIp）。
	//
	// # 什么场景该开
	//
	// 单租户私有部署：网关与上游都在同一个 VPC 里，base_url 写 10.x 是常态，
	// 逐个白名单维护不动。此时 SSRF 面由"内网还有谁能访问什么"承担，不由网关承担。
	//
	// # ⚠ 开了它仍然挡两件事（刻意的偏离）
	//
	//	回环 127.0.0.0/8 / ::1 / localhost   —— 那指向网关**自己**的管理面，
	//	                                        任何"内网上游"都不该是网关自身；
	//	云 metadata（169.254.169.254 等）    —— 那不是"上游"，是这台机器的云身份。
	//
	// 两者仍可用 AllowHosts **逐条**显式放行 —— 显式总比一刀切好。
	// new-api 的 allowPrivateIp 是连 metadata 一起放开的（它只判 isPrivateIP），
	// 这里刻意更严：把"放行内网"与"允许读实例凭据"拆成两个决定。
	//
	// 放开范围见 relaxableByAllowPrivate：私网 / 链路本地 / ULA / 保留段，
	// **以及内网主机名**（单标签名、.local、.internal 等）—— 后者是真实部署里
	// 最常见的自建上游写法（http://buildbox:8000），只放松 IP 不够用。
	AllowPrivate bool `json:"allow_private"`

	// AllowHosts 主机白名单：精确主机名、IP 字面量、CIDR，或 *.example.com。
	//
	// # 为什么必须提供它（否则这个功能会被整体关掉）
	//
	// "自建内网上游"是完全合理的部署形态（vLLM/ollama/私有网关都在内网）。
	// 一个不能放行的 SSRF 校验，最后一定会被运维用"整体禁用"来绕过 ——
	// 那等于没有校验。白名单把"要不要开这个洞"变成一次**逐个主机的显式决定**，
	// 这正是 new-api 的 DomainList / IpList 白名单模式。
	//
	// 支持的写法：
	//
	//	"api.example.com"     精确匹配（大小写不敏感，末尾的点会被忽略）
	//	"*.corp.example.com"  匹配 corp.example.com 及其所有子域（与 new-api 一致，
	//	                      *.x 也匹配 x 本身 —— 少一个例外就少一类"配了没用"的困惑）
	//	"10.0.0.5"            单个地址（等价于 /32，对字面量 IP 生效）
	//	"192.168.0.0/16"      网段（对字面量 IP 生效；域名要等拨号层的
	//	                      ValidateResolvedHostIP 才用得上 CIDR，因为这里不做 DNS）
	//
	// ⚠ 条目里不要写端口（"10.0.0.5:8080"）：白名单判的是主机，端口不参与。
	// 带端口的条目会在 NewURLGuard 里被当成配置错误拒掉，而不是"静默不生效" ——
	// 静默不生效的配置是这个功能最容易踩的坑。
	AllowHosts []string `json:"allow_hosts"`
}

// guardHostRule 白名单里的一条**主机名**规则。
//
// exact 与 suffix 恰好有一个非空（构造期保证）；用两个字段而不是一个"带模式"
// 字符串，是为了匹配时不跑正则、也不需要在热路径上解析。
type guardHostRule struct {
	exact  string // 精确主机（已 normalize）
	suffix string // *.suffix 的后缀（已 normalize，不含 "*."）
}

// match 报告主机名是否命中本规则。
func (r guardHostRule) match(host string) bool {
	if r.exact != "" {
		return host == r.exact
	}
	if r.suffix == "" {
		return false
	}
	if host == r.suffix {
		return true
	}
	return strings.HasSuffix(host, "."+r.suffix)
}

// URLGuard base_url 校验器。
//
// # 并发安全
//
// 构造后**不可变**，因此可被任意多个 goroutine 同时调用（校验发生在每个
// 建渠道请求上，也在每个拨号上）。刻意不提供 setter：白名单中途变化会让
// "同一条渠道时好时坏"，比不支持热更新难排查得多 —— 改配置就重建。
//
// # 零值也可用
//
// URLGuard 的零值（以及 nil 接收者）等价于"默认策略"：拒内网、无白名单。
// 这不是随手写的便利，而是防御性设计 —— 装配层忘了注入配置时（或测试里
// 直接 new 一个），行为应当**最严**，而不是"什么都不查"（fail-open）。
type URLGuard struct {
	allowPrivate bool
	hostRules    []guardHostRule
	prefixes     []netip.Prefix
}

// NewURLGuard 编译配置。条目不合法时返回错误（而不是静默忽略）。
//
// # 为什么非法条目要报错
//
// 白名单是这个功能唯一的"关掉它"的正规通道。一条写错的条目（"10.0.0.5:8080"、
// "*"、"ftp://x"）如果被静默跳过，用户看到的是"我加白了但还是被拒" ——
// 然后他会去关掉整个功能。所以宁可让装配期（启动/保存配置）失败一次。
func NewURLGuard(cfg URLGuardConfig) (*URLGuard, error) {
	g := &URLGuard{allowPrivate: cfg.AllowPrivate}
	for _, raw := range cfg.AllowHosts {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "://") {
			return nil, fmt.Errorf("url_guard.allow_hosts 条目 %q 不能带 scheme（只写主机名或 IP/CIDR）", raw)
		}
		// ① CIDR / 单个 IP
		if pfx, err := netip.ParsePrefix(entry); err == nil {
			g.prefixes = append(g.prefixes, pfx.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(entry); err == nil {
			addr = addr.WithZone("").Unmap()
			g.prefixes = append(g.prefixes, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		// ② 通配
		if suffix, ok := strings.CutPrefix(entry, "*."); ok {
			suffix = normalizeGuardHost(suffix)
			if suffix == "" {
				return nil, fmt.Errorf("url_guard.allow_hosts 条目 %q 的通配后缀为空", raw)
			}
			if strings.Contains(suffix, "*") {
				return nil, fmt.Errorf("url_guard.allow_hosts 条目 %q 含多个 *（只支持 *.example.com 这种前缀通配）", raw)
			}
			g.hostRules = append(g.hostRules, guardHostRule{suffix: suffix})
			continue
		}
		if strings.Contains(entry, "*") {
			return nil, fmt.Errorf("url_guard.allow_hosts 条目 %q 含 *（只支持 *.example.com 这种前缀通配）", raw)
		}
		// ③ 精确主机名
		if strings.Contains(entry, ":") {
			return nil, fmt.Errorf("url_guard.allow_hosts 条目 %q 带端口（白名单只判主机，端口不参与）", raw)
		}
		host := normalizeGuardHost(entry)
		if host == "" {
			return nil, fmt.Errorf("url_guard.allow_hosts 条目 %q 为空", raw)
		}
		g.hostRules = append(g.hostRules, guardHostRule{exact: host})
	}
	return g, nil
}

// DefaultURLGuard 返回默认策略的校验器（拒内网、无白名单）。
//
// 包级共享一个实例：它不可变，不需要每次新建。
func DefaultURLGuard() *URLGuard { return defaultURLGuard }

var defaultURLGuard = &URLGuard{}

// AllowsPrivate 报告是否放开了私网（管理台展示 / 诊断用）。
func (g *URLGuard) AllowsPrivate() bool {
	if g == nil {
		return false
	}
	return g.allowPrivate
}

// allowsPrivate 是 AllowPrivate 的**nil 安全**读法。
//
// 为什么要专门一个方法：所有校验路径都可能在 nil 接收者上跑
// （var g *URLGuard; g.ValidateBaseURL(...)），而直接读 g.allowPrivate
// 会 panic —— 一次 panic 在 HTTP handler 里就是整个进程的 500。
func (g *URLGuard) allowsPrivate() bool {
	if g == nil {
		return false
	}
	return g.allowPrivate
}

// CheckBaseURL 用默认策略校验一条 base_url（没有白名单的便捷入口）。
//
// 装配层注入了配置的场合请用 (*URLGuard).ValidateBaseURL —— 否则白名单不生效，
// 用户会觉得"加了白名单还是被拒"。
func CheckBaseURL(raw string) error { return defaultURLGuard.ValidateBaseURL(raw) }

// ValidateBaseURL 校验一条**渠道 base_url**。
//
// 校验顺序（先命中先返回，顺序即优先级）：
//
//	① 地址非空 / 能解析成 URL 吗  —— 解析不出就无从判定，按 malformed 拒
//	② scheme 是 http/https 吗      —— 先于主机判定：file:///etc/passwd 的"主机"是空的，
//	                                  报 scheme 比报"缺少主机"有用
//	③ 有主机名、端口合法吗
//	④ 主机是不是内网/回环/metadata/保留
//
// ⚠ 不做 DNS 解析（见文件头"本文件的边界"）：在保存时解析一次再放行，
// 是把裁决权交给一个我们**不控制**的 DNS 答案，而它随时会变。
// 真正的裁决在拨号层（ValidateResolvedHostIP）。这里只负责
// "字面量层面一定不放过"。
func (g *URLGuard) ValidateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return g.block("", "", URLGuardReasonMalformed, "地址为空", guardHint(URLGuardReasonMalformed))
	}

	u, err := url.Parse(raw)
	if err != nil {
		// ⚠ 不把 err.Error() 拼进文案：net/url 的错误会把**原始输入**整个回显
		// （parse "http://user:pw@h:abc": invalid port），而原始输入里可能有 userinfo。
		return g.block(redactRawURL(raw), "", URLGuardReasonMalformed,
			"URL 解析失败（检查 scheme、主机名与端口是否完整合法）", guardHint(URLGuardReasonMalformed))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return g.block(redactGuardURL(u), "", URLGuardReasonMalformed,
			"缺少 scheme（必须以 http:// 或 https:// 开头）", guardHint(URLGuardReasonMalformed))
	}
	if scheme != "http" && scheme != "https" {
		return g.block(redactGuardURL(u), "", URLGuardReasonScheme,
			fmt.Sprintf("协议 %q 不被支持", scheme), guardHint(URLGuardReasonScheme))
	}
	// ⚠ opaque 检查放在 scheme 之后：这样 "javascript:alert(1)" 会报 scheme
	// （更准确），而 "http:example.com" 会落到这里报 malformed。
	if u.Opaque != "" {
		// "http:example.com" 这种 opaque 形态：没有 authority，Hostname() 为空，
		// 但 net/http 仍可能把它当相对地址处理。直接判非法。
		return g.block(redactRawURL(raw), "", URLGuardReasonMalformed,
			"不支持不带 // 的 opaque 形态地址", guardHint(URLGuardReasonMalformed))
	}

	host := u.Hostname()
	if host == "" {
		return g.block(redactGuardURL(u), "", URLGuardReasonMalformed,
			"缺少主机名", guardHint(URLGuardReasonMalformed))
	}
	// 端口：net/url 只保证字符集合法，不保证范围。超范围端口在拨号时才炸，
	// 那会让一条错误配置在"保存成功"之后才以连接失败的形式出现 —— 这里先拦。
	if port := u.Port(); port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return g.block(redactGuardURL(u), host, URLGuardReasonMalformed,
				fmt.Sprintf("端口 %q 不在 1-65535 之内", port), guardHint(URLGuardReasonMalformed))
		}
	}

	// ⚠ userinfo（http://user:pass@host/）不在本次判定范围内：它属于
	// "凭据该不该写进 base_url"的问题，不是 SSRF。唯一的副作用是错误文案里
	// 可能带出凭据 —— 这一点由 redactGuardURL 处理（它不输出 userinfo）。

	reason, detail, blocked := g.checkHost(host)
	if !blocked {
		return nil
	}
	return g.block(redactGuardURL(u), host, reason, detail, guardHint(reason))
}

// ValidateResolvedHostIP 在**拨号层**复核"即将连接的这个 IP"。
//
// # 这是防 DNS 重绑定的唯一位置
//
// host 是 URL 里的主机名（用于白名单匹配），ip 是解析结果。
// 装配方式（示意，集成由主脑做）：
//
//	dialer := &net.Dialer{Control: func(network, address string, c syscall.RawConn) error {
//	    h, _, _ := net.SplitHostPort(address)
//	    return guard.ValidateResolvedHostIP(urlHost, net.ParseIP(h))
//	}}
//
// 传入的 host 必须是**用户填的那一个**，而不是解析后的 IP —— 否则白名单
// （按主机名授权）在拨号时失效，被放行的内网上游会在这里被拒。
//
// # 为什么白名单要在这里再判一次
//
// 因为白名单的语义是"这个**主机**可以连内网"。只按 IP 判的话，
// 放行 api.corp.example.com -> 10.0.0.5 就必须放行整个 10.0.0.0/8，
// 那是把逐主机授权退化成整段放行。
func (g *URLGuard) ValidateResolvedHostIP(host string, ip net.IP) error {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return g.block("", host, URLGuardReasonMalformed, "解析结果不是合法 IP", guardHint(URLGuardReasonMalformed))
	}
	h := normalizeGuardHost(host)

	if h != "" && g.hostAllowed(h, addr) {
		return nil
	}
	reason, detail, blocked := g.judgeAddr(h, addr)
	if !blocked {
		return nil
	}
	return g.block("", h, reason, detail, guardHint(reason))
}

// block 造一条拒绝错误（把 hint 的注入集中在一处，避免各处漏传）。
func (g *URLGuard) block(input, host string, reason URLGuardReason, detail, hint string) error {
	return &URLGuardError{Input: input, Host: host, Reason: reason, Detail: detail, Hint: hint}
}

// guardHint 针对不同原因给出"怎么才能放行"。
//
// # 为什么 hint 要按原因分（而不是统一一句"请检查配置"）
//
// 每个原因的补救动作完全不同，写错 hint 比不写更糟：
//
//	malformed  → 去改地址格式（加白名单没用）
//	scheme     → 换 http/https（加白名单没用 —— 白名单不绕过 scheme 检查）
//	metadata   → 确认这不是误配；真要放行必须显式写进白名单
//	其它内网类 → 加白名单（这才是正确的补救动作）
func guardHint(reason URLGuardReason) string {
	switch reason {
	case URLGuardReasonMalformed:
		return "请填写形如 https://api.example.com/v1 的完整地址"
	case URLGuardReasonScheme:
		return "base_url 只支持 http:// 或 https://"
	case URLGuardReasonMetadata:
		return "该地址只应由网关自身访问；确认确需放行时请写入 url_guard.allow_hosts"
	case URLGuardReasonLoopback:
		return "如需放行本机自建上游，请把该主机写入 url_guard.allow_hosts"
	case URLGuardReasonPrivate, URLGuardReasonLinkLocal, URLGuardReasonULA,
		URLGuardReasonReserved, URLGuardReasonInternalName:
		return "如需放行自建内网上游，请把该主机（或网段）写入 url_guard.allow_hosts，或打开 url_guard.allow_private"
	case URLGuardReasonDNSAlias:
		return "该服务的域名会被解析回域名里携带的 IP，请直接填写真实主机"
	}
	return ""
}

// checkHost 判定一个（已解析出的）主机名或 IP 字面量。
//
// 返回 blocked=true 时前两个返回值是原因与说明。
func (g *URLGuard) checkHost(host string) (URLGuardReason, string, bool) {
	host = normalizeGuardHost(host)
	if host == "" {
		return URLGuardReasonMalformed, "主机名为空", true
	}

	// ① 字面量 IP：netip 的规范形态（含 ::ffff:1.2.3.4、带 zone 的 fe80::1%eth0）
	if addr, err := netip.ParseAddr(host); err == nil {
		return g.judgeAddr(host, addr.WithZone(""))
	}
	// ② 字面量 IP：inet_aton 的宽松形态（127.1 / 0x7f.1 / 2130706433 / 0177.0.0.1）
	//
	// ⚠ 这一条不是洁癖。Go 在 cgo 解析器 / Windows（GetAddrInfo）下会把这类
	// 字符串**原样交给系统解析器**，而 glibc 的 inet_aton 与 Winsock 都认这些写法：
	//
	//	127.1         -> 127.0.0.1
	//	2130706433    -> 127.0.0.1
	//	0x7f.0.0.1    -> 127.0.0.1
	//	0177.0.0.1    -> 127.0.0.1   （前导 0 = 八进制）
	//
	// 而 net.ParseIP / netip.ParseAddr 全部返回 nil。只看规范形态 = 留一条
	// 明牌的绕过路径（这类绕过在 SSRF 里是被实际利用的第一梯队）。
	if addr, ok := parseLooseIPv4(host); ok {
		return g.judgeAddr(host, addr)
	}

	// ③ 显式白名单：按主机名
	if g.hostAllowed(host, netip.Addr{}) {
		return "", "", false
	}

	// ④ 明显的内网 / 本地 / 通配域名
	if reason, detail, blocked := judgeHostName(host); blocked {
		// allow_private 也要覆盖内网主机名（单标签名 / .lan / .internal …）：
		// "自建内网上游"在真实部署里往往写成 http://buildbox:8000 而不是 IP。
		// 若只放松 IP，打开 allow_private 的人会在这里莫名其妙被拒，
		// 然后去把整个校验关掉 —— 那比放过一个单标签名糟得多。
		// 回环 / metadata / DNS 别名不在放松之列（理由见各自常量）。
		if g.allowsPrivate() && relaxableByAllowPrivate(reason) {
			return "", "", false
		}
		return reason, detail, true
	}

	// ⑤ 纯数字但不是合法 IPv4（如 999.1.1.1、1.2.3.4.5）
	//
	// 这类主机名在不同解析器里行为不一致（有的直接失败，有的截断，有的按
	// DNS 通配记录解析）。它们不可能是一个真实的上游域名 ⇒ 直接拒。
	if isAllNumericHost(host) {
		return URLGuardReasonMalformed,
			fmt.Sprintf("%q 是纯数字串但不是合法的 IPv4 地址（不同解析器对它的理解不一致）", host), true
	}

	return "", "", false
}

// judgeAddr 对**已确定的 IP** 套策略：白名单 → 分类 → AllowPrivate。
func (g *URLGuard) judgeAddr(host string, addr netip.Addr) (URLGuardReason, string, bool) {
	if g.hostAllowed(host, addr) {
		return "", "", false
	}
	reason, detail, blocked := classifyAddr(addr)
	if !blocked {
		return "", "", false
	}
	// metadata 与回环**不受 AllowPrivate 影响**（理由见 URLGuardConfig.AllowPrivate）：
	// 它们不是"某台内网上游"，而是"网关自己"和"这台机器的云身份"。
	if g.allowsPrivate() && relaxableByAllowPrivate(reason) {
		return "", "", false
	}
	return reason, detail, true
}

// hostAllowed 主机名/IP 是否命中白名单。
func (g *URLGuard) hostAllowed(host string, addr netip.Addr) bool {
	if g == nil {
		return false
	}
	for _, r := range g.hostRules {
		if r.match(host) {
			return true
		}
	}
	if !addr.IsValid() {
		return false
	}
	addr = addr.WithZone("").Unmap()
	for _, p := range g.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// normalizeGuardHost 归一化主机名：去空白、去末尾的根点、去 IPv6 zone、小写。
//
// # 为什么每一条都要做
//
//	去空白      配置里手抄的地址常带空格
//	去末尾点    "example.com." 是合法 FQDN，与 "example.com" 是同一台机器；
//	            不归一化就会让白名单"写了有用、实际没命中"
//	去 zone     "[fe80::1%25eth0]" 解析出的 Hostname 带 %eth0；zone 只与本机网卡有关，
//	            与"它是不是链路本地"这个判定无关
//	小写        DNS 大小写不敏感；白名单不该因为大小写不同而失效
func normalizeGuardHost(h string) string {
	h = strings.TrimSpace(h)
	// ⚠ 方括号（IPv6 字面量在 URL 里的形态）一起剥掉：net/url 的 Hostname() 已经
	// 去过一次，但白名单条目与调用方传进来的 host 可能仍带括号，不统一就会漏命中。
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	h = strings.TrimRight(h, ".")
	if i := strings.LastIndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

// judgeHostName 判定"不含 IP 字面量"的主机名是否明显指向内网/本机。
func judgeHostName(host string) (URLGuardReason, string, bool) {
	// localhost 及其子域：RFC 6761 明确要求解析到回环。
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return URLGuardReasonLoopback,
			fmt.Sprintf("%q 是 localhost，按 RFC 6761 必须解析到回环地址", host), true
	}

	// 私有/本地命名空间（RFC 6762 的 .local、ICANN 保留的 .internal，以及企业内网常见的几个）。
	for _, suffix := range guardReservedNameSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return URLGuardReasonInternalName,
				fmt.Sprintf("%q 属于 .%s 私有命名空间（只能由内网 DNS / hosts 解析）", host, suffix), true
		}
	}

	// "域名即 IP" 的通配服务：nip.io 会把自己域名里出现的 IP 原样解析回去，
	// 于是 http://127.0.0.1.nip.io/ 绕开所有字面量 IP 检查。lvh.me / localtest.me
	// 固定指向 127.0.0.1。它们没有任何合法的上游用途（见 URLGuardReasonDNSAlias）。
	for _, suffix := range guardDNSAliasSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return URLGuardReasonDNSAlias,
				fmt.Sprintf("%q 属于 %s 这类「域名即 IP」的通配解析服务，可绕开字面量 IP 检查", host, suffix), true
		}
	}

	// 单标签主机名（无点）：公网 DNS 里不存在单标签名字（根区没有这样的 TLD），
	// 能解析的只可能是内网 DNS / hosts / search domain。
	if !strings.Contains(host, ".") {
		return URLGuardReasonInternalName,
			fmt.Sprintf("%q 是不带域名的单标签主机（只能由内网 DNS / hosts 解析）", host), true
	}

	return "", "", false
}

// guardReservedNameSuffixes 只可能解析到内网的私有命名空间。
//
// 收录依据：
//
//	local, localhost  RFC 6762（mDNS）/ RFC 6761
//	home.arpa         RFC 8375（住宅网络专用）
//	internal          ICANN 2024 保留给"私有使用"的 TLD
//	intranet, lan,
//	localdomain,
//	corp, private     企业内网通行约定（Windows AD 的 .corp/.lan 尤其常见）
//
// ⚠ 刻意**不**收录 .test / .invalid / .example：前两者是占位符（RFC 6761 要求
// 永不解析），把它们拒掉只会让"用假的基址跑测试"的人多踩一脚；
// 而 example.com 正是本仓测试与文档里在用的公网示例域。
var guardReservedNameSuffixes = []string{
	"local", "localhost", "home.arpa", "internal", "intranet", "lan", "localdomain", "corp", "private",
}

// guardDNSAliasSuffixes "域名即 IP" 的通配解析服务（都可被用来绕过字面量 IP 检查）。
var guardDNSAliasSuffixes = []string{
	"nip.io", "sslip.io", "xip.io", "lvh.me", "localtest.me", "traefik.me",
}

// guardIPv4Rules IPv4 的拒绝表。
//
// 依据 IANA IPv4 Special-Purpose Address Registry，与 new-api 的 privateIPv4Nets
// 逐项对齐（少一项就等于少挡一类地址）。顺序只是"报哪个原因"的优先级，
// 各段本身互不重叠。
var guardIPv4Rules = []struct {
	prefix netip.Prefix
	reason URLGuardReason
	label  string
}{
	// 0.0.0.0/8 归到 loopback 而不是 reserved：在 Linux 上 connect 到本段内任何
	// 地址都等价于连 127.0.0.1（内核把它当"本网络"）；归 reserved 的话，
	// AllowPrivate 会把它一起放行 —— 而"放行内网"绝不该等于"放行本机"。
	{mustGuardPrefix("0.0.0.0/8"), URLGuardReasonLoopback, "「本网络」段 0.0.0.0/8（Linux 上 connect 到本段等价于连本机）"},
	{mustGuardPrefix("10.0.0.0/8"), URLGuardReasonPrivate, "私网地址段 10.0.0.0/8"},
	{mustGuardPrefix("100.64.0.0/10"), URLGuardReasonPrivate, "运营商级 NAT 地址段 100.64.0.0/10"},
	{mustGuardPrefix("127.0.0.0/8"), URLGuardReasonLoopback, "回环地址段 127.0.0.0/8"},
	{mustGuardPrefix("169.254.0.0/16"), URLGuardReasonLinkLocal, "链路本地地址段 169.254.0.0/16"},
	{mustGuardPrefix("172.16.0.0/12"), URLGuardReasonPrivate, "私网地址段 172.16.0.0/12"},
	{mustGuardPrefix("192.0.0.0/24"), URLGuardReasonReserved, "IETF 协议分配段 192.0.0.0/24"},
	{mustGuardPrefix("192.0.2.0/24"), URLGuardReasonReserved, "文档示例段 192.0.2.0/24（TEST-NET-1）"},
	{mustGuardPrefix("192.168.0.0/16"), URLGuardReasonPrivate, "私网地址段 192.168.0.0/16"},
	{mustGuardPrefix("198.18.0.0/15"), URLGuardReasonReserved, "基准测试段 198.18.0.0/15"},
	{mustGuardPrefix("198.51.100.0/24"), URLGuardReasonReserved, "文档示例段 198.51.100.0/24（TEST-NET-2）"},
	{mustGuardPrefix("203.0.113.0/24"), URLGuardReasonReserved, "文档示例段 203.0.113.0/24（TEST-NET-3）"},
	{mustGuardPrefix("224.0.0.0/4"), URLGuardReasonReserved, "组播地址段 224.0.0.0/4"},
	{mustGuardPrefix("240.0.0.0/4"), URLGuardReasonReserved, "保留地址段 240.0.0.0/4（含受限广播 255.255.255.255）"},
}

// guardIPv6Rules IPv6 里无法用 netip 的便捷方法判定的特殊段。
//
// 回环 ::1、链路本地 fe80::/10、组播 ff00::/8、ULA fc00::/7 用 netip 的
// 方法判（见 classifyAddr），这里只列剩下的。
var guardIPv6Rules = []struct {
	prefix netip.Prefix
	reason URLGuardReason
	label  string
}{
	{mustGuardPrefix("100::/64"), URLGuardReasonReserved, "Discard-Only 段 100::/64"},
	{mustGuardPrefix("2001::/23"), URLGuardReasonReserved, "IETF 协议分配段 2001::/23（含 Teredo）"},
	{mustGuardPrefix("2001:db8::/32"), URLGuardReasonReserved, "文档示例段 2001:db8::/32"},
	{mustGuardPrefix("2002::/16"), URLGuardReasonReserved, "6to4 隧道段 2002::/16"},
}

// guardEmbeddedV4Prefixes 内嵌一个 IPv4 的 IPv6 前缀。
//
// NAT64（64:ff9b::/96）与已废弃的 IPv4-compatible（::/96）都能把请求
// "翻译"到一个 IPv4 目标上。若内嵌的 IPv4 是内网，那么这串 IPv6 同样能打到内网 ——
// 所以判定要**递归到内嵌地址**，而不是只看外层前缀。
var guardEmbeddedV4Prefixes = []netip.Prefix{
	mustGuardPrefix("64:ff9b::/96"),
	mustGuardPrefix("::/96"),
}

// guardMetadataIPv4 云厂商元数据服务的 IPv4 端点。
//
// 为什么要一张具名表而不是只靠 169.254.0.0/16：命中它们意味着"有人在试着偷这台
// 机器的云身份"，这是安全事件而不是配置错误 —— 值得单独一个 reason
// （URLGuardReasonMetadata）让告警能据此升级。
//
// 除了 169.254.169.254（AWS/GCP/Azure/OpenStack/阿里云的经典地址），
// 还有几个现实中存在、但不在 169.254/16 里的端点。
var guardMetadataIPv4 = map[netip.Addr]string{
	netip.MustParseAddr("169.254.169.254"): "AWS/GCP/Azure/OpenStack 的实例元数据服务（IMDSv1，可直接读到实例凭据）",
	netip.MustParseAddr("169.254.169.253"): "AWS VPC 内的 DNS/RRSA 端点（可被用作内网探测跳板）",
	netip.MustParseAddr("169.254.170.2"):   "AWS ECS 任务角色凭据端点",
	netip.MustParseAddr("100.100.100.200"): "阿里云 ECS 元数据服务",
}

// guardMetadataIPv6 元数据的 IPv6 端点。
//
// fd00:ec2::254 是 AWS IMDS 的 IPv6 地址，落在 ULA 里 —— 但 ULA 会被
// AllowPrivate 放开，所以必须在这里单独拦（它属于 metadata 而非"内网"）。
var guardMetadataIPv6 = map[netip.Addr]string{
	netip.MustParseAddr("fd00:ec2::254"): "AWS IMDS 的 IPv6 端点",
}

// classifyAddr 判定一个 IP 是否属于"不该由网关访问"的地址，并说清属于哪一类。
//
// 返回 blocked=false 表示放行（公网地址）。
func classifyAddr(addr netip.Addr) (URLGuardReason, string, bool) {
	if !addr.IsValid() {
		return URLGuardReasonMalformed, "无效的 IP 地址", true
	}
	addr = addr.WithZone("")
	// IPv4-mapped（::ffff:10.0.0.1）先还原成 IPv4：否则 IPv6 的各个判据都碰不到它，
	// 而它在拨号时就是一条 IPv4 连接。这正是 SSRF 里最常见的绕过写法之一。
	if addr.Is4In6() {
		addr = addr.Unmap()
	}

	if addr.Is4() {
		if label, ok := guardMetadataIPv4[addr]; ok {
			return URLGuardReasonMetadata, fmt.Sprintf("%s 是%s", addr, label), true
		}
		for _, r := range guardIPv4Rules {
			if r.prefix.Contains(addr) {
				return r.reason, fmt.Sprintf("%s 属于%s", addr, r.label), true
			}
		}
		return "", "", false
	}

	// ── IPv6 ────────────────────────────────────────────────────
	if label, ok := guardMetadataIPv6[addr]; ok {
		return URLGuardReasonMetadata, fmt.Sprintf("%s 是%s", addr, label), true
	}
	if addr.IsLoopback() {
		return URLGuardReasonLoopback, fmt.Sprintf("%s 是 IPv6 回环地址（::1）", addr), true
	}
	if addr.IsUnspecified() {
		// 同 0.0.0.0/8：connect 到 :: 等价于连本机，所以按 loopback 归类
		// （否则 AllowPrivate 会把它一起放行）。
		return URLGuardReasonLoopback, fmt.Sprintf("%s 是未指定地址（::），拨号时会回落成本机", addr), true
	}
	if addr.IsLinkLocalUnicast() {
		return URLGuardReasonLinkLocal, fmt.Sprintf("%s 属于 IPv6 链路本地段 fe80::/10", addr), true
	}
	if addr.IsMulticast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() {
		return URLGuardReasonReserved, fmt.Sprintf("%s 属于 IPv6 组播段", addr), true
	}
	if addr.IsPrivate() {
		return URLGuardReasonULA, fmt.Sprintf("%s 属于 IPv6 唯一本地地址段 fc00::/7", addr), true
	}
	for _, r := range guardIPv6Rules {
		if r.prefix.Contains(addr) {
			return r.reason, fmt.Sprintf("%s 属于%s", addr, r.label), true
		}
	}
	// 内嵌 IPv4 的翻译前缀：递归判定内嵌地址（见 guardEmbeddedV4Prefixes）。
	for _, p := range guardEmbeddedV4Prefixes {
		if !p.Contains(addr) {
			continue
		}
		b := addr.As16()
		inner := netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		if reason, detail, blocked := classifyAddr(inner); blocked {
			return reason, fmt.Sprintf("%s 属于 %s，它内嵌的 IPv4 %s 同样被拒（%s）", addr, p, inner, detail), true
		}
	}
	return "", "", false
}

// isAllNumericHost 报告主机名是否"只由数字和点组成"。
//
// 用来拦 999.1.1.1 / 1.2.3.4.5 这类"看着像 IP 但不是"的主机名 ——
// 它们不可能是一个可用的公网上游，而不同解析器对它们的行为又不一致。
func isAllNumericHost(host string) bool {
	if host == "" {
		return false
	}
	hasDigit := false
	for _, r := range host {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '.':
		default:
			return false
		}
	}
	return hasDigit
}

// parseLooseIPv4 按 glibc inet_aton 的语义解析"宽松 IPv4 字面量"。
//
// # 为什么非要有它（这是本文件最容易被删掉、也最不该删的一段）
//
// 下列写法在 glibc / Winsock 里都是 127.0.0.1，但 Go 的 net.ParseIP 与
// netip.ParseAddr **全部返回 nil**：
//
//	127.1             两段：a.b  -> a(8bit) . b(24bit)
//	2130706433        单段：直接当 32 位数
//	0x7f.0.0.1        每段允许 0x 前缀（十六进制）
//	0177.0.0.1        前导 0 = 八进制
//	127.0.1           三段：a(8) . b(8) . c(16)
//
// Go 的纯 Go 解析器不认这些写法，但 **cgo 解析器与 Windows 的原生
// GetAddrInfo 会把原始字符串交给系统**，系统认。于是
// "http://127.1/" 在 Linux(cgo) 与 Windows 上直连 127.0.0.1，而只查规范形态的
// 校验器会放行它 —— 一条明牌的 SSRF 绕过。
//
// 取舍：按 inet_aton 语义**多判**是安全的（判错的唯一后果是某个怪异字符串
// 被当成 IP，而它本来也不可能是可用域名）；漏判则是一条可利用的洞。
func parseLooseIPv4(host string) (netip.Addr, bool) {
	host = strings.TrimRight(host, ".")
	if host == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		v, ok := parseInetAtom(p)
		if !ok {
			return netip.Addr{}, false
		}
		vals[i] = v
	}

	var b [4]byte
	switch len(vals) {
	case 1:
		if vals[0] > 0xFFFFFFFF {
			return netip.Addr{}, false
		}
		b[0] = byte(vals[0] >> 24)
		b[1] = byte(vals[0] >> 16)
		b[2] = byte(vals[0] >> 8)
		b[3] = byte(vals[0])
	case 2:
		if vals[0] > 0xFF || vals[1] > 0xFFFFFF {
			return netip.Addr{}, false
		}
		b[0] = byte(vals[0])
		b[1] = byte(vals[1] >> 16)
		b[2] = byte(vals[1] >> 8)
		b[3] = byte(vals[1])
	case 3:
		if vals[0] > 0xFF || vals[1] > 0xFF || vals[2] > 0xFFFF {
			return netip.Addr{}, false
		}
		b[0] = byte(vals[0])
		b[1] = byte(vals[1])
		b[2] = byte(vals[2] >> 8)
		b[3] = byte(vals[2])
	case 4:
		for i, v := range vals {
			if v > 0xFF {
				return netip.Addr{}, false
			}
			b[i] = byte(v)
		}
	}
	return netip.AddrFrom4(b), true
}

// parseInetAtom 解析 inet_aton 的一个"数字段"（支持 0x 十六进制与前导 0 八进制）。
//
// 与 strconv.ParseUint(s, 0, 64) 的区别：那个会把 "08" 判成非法（八进制里的 8），
// 而 glibc 的 strtol 也是同样行为 —— 所以这里刻意与它保持一致，不"宽容"。
func parseInetAtom(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	base := 10
	switch {
	case len(s) > 2 && (strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X")):
		base = 16
		s = s[2:]
	case len(s) > 1 && s[0] == '0':
		base = 8
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	// ParseUint 不接受符号，正好挡住了 "+127" / "-1" 这类形态。
	v, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// redactRawURL 对**解析失败**的原始输入做最小化脱敏：抹掉 userinfo 位置 + 截断。
//
// 解析失败时拿不到 *url.URL，只能按字符串处理。要防的是"把凭据回显进错误文案/日志"：
// 形如 http://user:pass@host 的前半段直接抹掉。解析不出来的形态本身也无法可靠
// 判定哪段是 userinfo —— 所以这一步是"尽力而为"，真正的保证来自
// "解析失败时只说最少的话 + 截断"。
func redactRawURL(raw string) string {
	const maxBytes = 120 // 按字节截断足够：它只用于展示，且截断后仍以 UTF-8 边界为限
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		if at := strings.Index(s[i+3:], "@"); at >= 0 {
			s = s[:i+3] + "***@" + s[i+3+at+1:]
		}
	}
	if len(s) > maxBytes {
		// 按 rune 边界退一步，避免把一个多字节 UTF-8 字符切成乱码（U+FFFD）。
		cut := maxBytes
		for cut > 0 && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// redactGuardURL 把解析成功的 URL 渲染成"只含 scheme://host:port"的形态。
//
// # 为什么不让它带 path / query / userinfo
//
// 这个字符串会原样进错误消息（管理台弹窗、API 响应、日志）：
//
//	userinfo  就是凭据（http://user:pass@host/）
//	query     常放 key（?api_key=...）
//	path      可能含租户/项目标识
//
// 而说明"为什么被拒"只需要 host —— 用户看得懂自己填的主机。少输出三类东西，
// 就少三条泄漏路径，也不需要调用方记得再过一遍 MaskErrorText。
func redactGuardURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	var b strings.Builder
	if u.Scheme != "" {
		b.WriteString(strings.ToLower(u.Scheme))
		b.WriteString("://")
	}
	b.WriteString(strings.ToLower(u.Hostname()))
	if p := u.Port(); p != "" {
		b.WriteString(":")
		b.WriteString(p)
	}
	return b.String()
}

// relaxableByAllowPrivate 报告某个拒绝原因能否被 AllowPrivate 整体放开。
//
// # 为什么是"白名单式"的判断而不是散在各处的 != 比较
//
// 这条策略决定"打开 allow_private 到底放过了什么"，是安全边界的定义处。
// 写成多处 != 比较时，新增一个 reason 的默认行为是**被放过**（因为没人去加比较）
// —— 那是 fail-open。白名单写法让新 reason 默认**继续拒绝**，需要显式放行。
func relaxableByAllowPrivate(reason URLGuardReason) bool {
	switch reason {
	case URLGuardReasonPrivate, URLGuardReasonLinkLocal, URLGuardReasonULA,
		URLGuardReasonReserved, URLGuardReasonInternalName:
		return true
	}
	// loopback / metadata / dns_alias / scheme / malformed 一律不放。
	return false
}

// mustGuardPrefix 解析固定的 CIDR，写错就在 init 阶段炸掉。
//
// 与 errmask 里 regexp.MustCompile 的取法一致：这些字符串是**代码常量**，
// 打错是编程错误而不是运行期输入。让它尽早失败（测试第一轮就会跑到），
// 好过在运行期静默少挡一个网段 —— 那是最难发现的一类安全回归。
func mustGuardPrefix(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		panic("gateway: urlguard 内置 CIDR 非法: " + s + ": " + err.Error())
	}
	return p
}
