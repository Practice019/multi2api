// urlguard_test.go —— base_url SSRF 校验的表驱动测试。
//
// # 这张表为什么以「拒绝」为主，还专门另立一条「放行」表
//
// SSRF 校验的两种失败代价**不对称**：
//
//	漏拒（false negative）  内网被打穿、云凭据被读走 —— 一次就够出事
//	误拒（false positive）  用户填不进自己的上游 —— 他会去把校验整个关掉
//
// 所以两类断言都要有，且都必须逐条钉住**依据**（Reason），而不是只钉"有没有报错"。
// 只断言 "err != nil" 的行为等价于：任何人把 metadata 误判成 malformed 都不会被发现，
// 而两者的告警等级完全不同（见 URLGuardReason 的注释）。
//
// # 为什么大量用例是「IP 字面量的变形写法」
//
// 因为 SSRF 的真实绕过几乎都发生在**解析口径差异**上，而不是"没想到 10.0.0.0/8"：
// 127.1 / 2130706433 / 0x7f.0.0.1 / 0177.0.0.1 / [::ffff:127.0.0.1] 全都是回环，
// 而它们没有一个能通过 net.ParseIP。表里逐条钉住，是为了让"顺手删掉
// parseLooseIPv4"这种改动立刻变红。
package gateway

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// guardCase 一条「应当被拒」的用例。
type guardCase struct {
	name string
	url  string
	want URLGuardReason
	why  string
}

// guardRejectCases 主拒绝表。
var guardRejectCases = []guardCase{
	// ── 回环 ────────────────────────────────────────────────────
	{name: "IPv4回环", url: "http://127.0.0.1:8080/v1", want: URLGuardReasonLoopback,
		why: "指向网关自身的管理面，是最经典的目标"},
	{name: "IPv4回环_段内任意地址", url: "http://127.255.255.254/", want: URLGuardReasonLoopback,
		why: "整个 127/8 都是回环，不是只有 127.0.0.1"},
	{name: "IPv4回环_两段简写", url: "http://127.1/", want: URLGuardReasonLoopback,
		why: "inet_aton：a.b ⇒ a(8bit).b(24bit)；glibc 与 Winsock 都认，net.ParseIP 不认"},
	{name: "IPv4回环_单整数", url: "http://2130706433/", want: URLGuardReasonLoopback,
		why: "2130706433 = 0x7F000001 = 127.0.0.1"},
	{name: "IPv4回环_十六进制段", url: "http://0x7f.0.0.1/", want: URLGuardReasonLoopback,
		why: "inet_aton 允许每段 0x 前缀"},
	{name: "IPv4回环_八进制段", url: "http://0177.0.0.1/", want: URLGuardReasonLoopback,
		why: "前导 0 = 八进制，0177 = 127"},
	{name: "IPv4回环_三段简写", url: "http://127.0.1/", want: URLGuardReasonLoopback,
		why: "inet_aton：a.b.c ⇒ a(8).b(8).c(16)"},
	{name: "IPv6回环", url: "http://[::1]:8080/", want: URLGuardReasonLoopback,
		why: "IPv6 的回环是 ::1，且 URL 里必须带方括号"},
	{name: "IPv4映射到IPv6的回环", url: "http://[::ffff:127.0.0.1]/", want: URLGuardReasonLoopback,
		why: "拨号时它就是一条 IPv4 回环连接；不 Unmap 就地绕过全部 IPv4 判据"},
	{name: "未指定地址_IPv4", url: "http://0.0.0.0:8080/", want: URLGuardReasonLoopback,
		why: "Linux 上 connect 到 0.0.0.0 等价于连本机，且 0/8 整段如此"},
	{name: "未指定地址_IPv4整数", url: "http://0/", want: URLGuardReasonLoopback,
		why: "单段 0 = 0.0.0.0，同一件事的另一种写法"},
	{name: "未指定地址_IPv6", url: "http://[::]/", want: URLGuardReasonLoopback,
		why: ":: 同 0.0.0.0，拨号会回落本机"},
	{name: "localhost", url: "http://localhost:7868/v1", want: URLGuardReasonLoopback,
		why: "RFC 6761 要求解析到回环；常见于「想打自己」的手滑配置"},
	{name: "localhost_子域", url: "http://api.localhost/", want: URLGuardReasonLoopback,
		why: "*.localhost 同样按 RFC 6761 解析到回环"},

	// ── 私网 ────────────────────────────────────────────────────
	{name: "私网10", url: "http://10.0.0.5:8000/v1", want: URLGuardReasonPrivate,
		why: "RFC 1918：10.0.0.0/8"},
	{name: "私网10_上界", url: "http://10.255.255.255/", want: URLGuardReasonPrivate,
		why: "段边界也要挡住 —— 按 /8 判而不是按前缀字符串判"},
	{name: "私网172_16", url: "http://172.16.0.1/", want: URLGuardReasonPrivate,
		why: "RFC 1918：172.16.0.0/12 的下界"},
	{name: "私网172_31", url: "http://172.31.255.254/", want: URLGuardReasonPrivate,
		why: "172.16.0.0/12 的上界；写成 /16 就会漏掉这一半"},
	{name: "私网192_168", url: "http://192.168.1.10/", want: URLGuardReasonPrivate,
		why: "RFC 1918：192.168.0.0/16"},
	{name: "CGNAT", url: "http://100.64.0.1/", want: URLGuardReasonPrivate,
		why: "100.64.0.0/10 是运营商级 NAT 段，云上常被用作内部服务地址"},
	{name: "CGNAT_上界", url: "http://100.127.255.254/", want: URLGuardReasonPrivate,
		why: "100.64.0.0/10 的上界（/10 而不是 /8）"},

	// ── 链路本地 / ULA ──────────────────────────────────────────
	{name: "链路本地", url: "http://169.254.1.1/", want: URLGuardReasonLinkLocal,
		why: "169.254.0.0/16：链路本地（APIPA），同一链路上就能被直接访问"},
	{name: "IPv6链路本地", url: "http://[fe80::1]/", want: URLGuardReasonLinkLocal,
		why: "fe80::/10"},
	{name: "IPv6链路本地_带zone", url: "http://[fe80::1%25eth0]:80/", want: URLGuardReasonLinkLocal,
		why: "带 zone 的形态在 URL 里必须写成 %25；zone 不改变「它是链路本地」这个结论"},
	{name: "IPv6_ULA", url: "http://[fc00::1]/", want: URLGuardReasonULA,
		why: "fc00::/7 是 IPv6 的内网段，与 10/8 等价"},
	{name: "IPv6_ULA_fd", url: "http://[fd12:3456::1]/", want: URLGuardReasonULA,
		why: "fd00::/8 是 fc00::/7 的一半，实际部署里更常见"},

	// ── 云 metadata（单列的原因：这是凭据泄露路径）────────────────
	{name: "metadata_经典IMDS", url: "http://169.254.169.254/latest/meta-data/", want: URLGuardReasonMetadata,
		why: "AWS/GCP/Azure/OpenStack/阿里云共用的实例元数据地址 —— 必须报 metadata 而不是 link_local"},
	{name: "metadata_AWS_ECS", url: "http://169.254.170.2/v2/credentials", want: URLGuardReasonMetadata,
		why: "ECS 任务角色凭据端点，能直接换出临时 AK"},
	{name: "metadata_阿里云", url: "http://100.100.100.200/latest/meta-data/", want: URLGuardReasonMetadata,
		why: "不在 169.254/16 里，只靠链路本地规则挡不住"},
	{name: "metadata_AWS_V6", url: "http://[fd00:ec2::254]/latest/meta-data/", want: URLGuardReasonMetadata,
		why: "AWS IMDS 的 IPv6 地址落在 ULA 里，会被 allow_private 放开 ⇒ 必须单独判"},

	// ── 保留 / 特殊用途 ─────────────────────────────────────────
	{name: "保留_组播", url: "http://224.0.0.1/", want: URLGuardReasonReserved,
		why: "224.0.0.0/4 组播"},
	{name: "保留_受限广播", url: "http://255.255.255.255/", want: URLGuardReasonReserved,
		why: "在 240.0.0.0/4 里；广播地址不该被当成上游"},
	{name: "保留_240段", url: "http://240.0.0.1/", want: URLGuardReasonReserved,
		why: "240.0.0.0/4 保留段"},
	{name: "保留_TEENET1", url: "http://192.0.2.1/", want: URLGuardReasonReserved,
		why: "文档示例段；能连通说明网络里有劫持"},
	{name: "保留_TEENET2", url: "http://198.51.100.7/", want: URLGuardReasonReserved,
		why: "文档示例段"},
	{name: "保留_TEENET3", url: "http://203.0.113.9/", want: URLGuardReasonReserved,
		why: "文档示例段"},
	{name: "保留_基准测试段", url: "http://198.18.0.1/", want: URLGuardReasonReserved,
		why: "198.18.0.0/15 基准测试段"},
	{name: "保留_192_0_0", url: "http://192.0.0.8/", want: URLGuardReasonReserved,
		why: "IETF 协议分配段"},
	{name: "保留_文档IPv6", url: "http://[2001:db8::1]/", want: URLGuardReasonReserved,
		why: "2001:db8::/32 文档段"},
	{name: "保留_6to4", url: "http://[2002::1]/", want: URLGuardReasonReserved,
		why: "6to4 隧道段可把请求转进任意 IPv4 目标"},
	{name: "保留_NAT64内嵌私网", url: "http://[64:ff9b::a00:1]/", want: URLGuardReasonPrivate,
		why: "NAT64 会把内嵌的 IPv4 目标翻译出来：a00:1 = 10.0.0.1 ⇒ 必须递归判内嵌地址"},
	{name: "IPv6组播", url: "http://[ff02::1]/", want: URLGuardReasonReserved,
		why: "ff00::/8 组播"},

	// ── 内网主机名 ──────────────────────────────────────────────
	{name: "单标签主机名", url: "http://buildbox:8080/v1", want: URLGuardReasonInternalName,
		why: "根区没有单标签 TLD，能解析的只可能是内网 DNS / hosts"},
	{name: "mDNS的local", url: "http://nas.local/", want: URLGuardReasonInternalName,
		why: "RFC 6762 的 .local 只在本地链路上可解析"},
	{name: "保留TLD_internal", url: "http://vault.internal/", want: URLGuardReasonInternalName,
		why: "ICANN 保留给私有使用的 TLD"},
	{name: "企业内网_corp", url: "http://git.corp/", want: URLGuardReasonInternalName,
		why: ".corp 是 Windows AD 的常见内网 TLD（不在公网根区里）"},
	{name: "企业内网_lan", url: "http://fileserver.lan/", want: URLGuardReasonInternalName,
		why: ".lan 同样是内网约定后缀"},
	{name: "IP即域名_nip.io", url: "http://127.0.0.1.nip.io/", want: URLGuardReasonDNSAlias,
		why: "nip.io 把域名里携带的 IP 原样解析回去 ⇒ 藏住字面量 IP 的公开绕过服务"},
	{name: "IP即域名_lvh.me", url: "http://lvh.me:3000/", want: URLGuardReasonDNSAlias,
		why: "lvh.me 固定指向 127.0.0.1"},

	// ── 非法形态 ────────────────────────────────────────────────
	{name: "空地址", url: "", want: URLGuardReasonMalformed,
		why: "边界：表单没填就提交"},
	{name: "纯文本", url: "not a url", want: URLGuardReasonMalformed,
		why: "没有 scheme；补全猜测（加 https://）是危险的：它会把内网地址「猜」成合法的"},
	{name: "缺主机名", url: "https:///v1/chat", want: URLGuardReasonMalformed,
		why: "有 scheme 没有 authority"},
	{name: "opaque形态", url: "http:example.com/v1", want: URLGuardReasonMalformed,
		why: "没有 // 的 opaque 形态：Hostname() 为空，但 net/http 可能当相对地址处理"},
	{name: "伪IP_越界段", url: "http://999.1.1.1/", want: URLGuardReasonMalformed,
		why: "纯数字但不是合法 IPv4 —— 不同解析器行为不一致，不可能是一个真实上游"},
	{name: "伪IP_五段", url: "http://1.2.3.4.5/", want: URLGuardReasonMalformed,
		why: "同上"},
	{name: "端口越界", url: "http://api.example.com:99999/v1", want: URLGuardReasonMalformed,
		why: "net/url 只校验端口字符集，不校验范围；不拦就会在拨号时才炸"},

	// ── 非 http(s) scheme ──────────────────────────────────────
	{name: "scheme_file", url: "file:///etc/passwd", want: URLGuardReasonScheme,
		why: "读本地文件是 SSRF 的经典后果之一"},
	{name: "scheme_gopher", url: "gopher://127.0.0.1:6379/_INFO", want: URLGuardReasonScheme,
		why: "gopher 可以构造任意 TCP 载荷（打 Redis / 内网 SMTP）"},
	{name: "scheme_ftp", url: "ftp://files.example.com/pub", want: URLGuardReasonScheme,
		why: "base_url 只该是 HTTP 上游"},
	{name: "scheme_ws", url: "wss://api.example.com/v1", want: URLGuardReasonScheme,
		why: "本仓的上游调用走 HTTP；ws 不在支持范围内（但注意它不是「不安全」，只是不支持）"},
	{name: "scheme_javascript", url: "javascript:alert(1)", want: URLGuardReasonScheme,
		why: "scheme 检查先于主机检查，所以这类串报 scheme 而不是「缺少主机」"},
	{name: "scheme_大写也拦", url: "FILE:///etc/passwd", want: URLGuardReasonScheme,
		why: "scheme 大小写不敏感，白名单比对前必须小写化"},
}

// TestURLGuardRejects 主拒绝表：逐条钉住「被拒 + 拒绝依据」。
func TestURLGuardRejects(t *testing.T) {
	g := DefaultURLGuard()
	for _, tc := range guardRejectCases {
		t.Run(tc.name, func(t *testing.T) {
			err := g.ValidateBaseURL(tc.url)
			if err == nil {
				t.Fatalf("应当被拒但放行了: %q\n  依据: %s", tc.url, tc.why)
			}
			if !errors.Is(err, ErrBaseURLBlocked) {
				t.Fatalf("错误没有包装 ErrBaseURLBlocked（调用方无法统一翻译成 400）: %v", err)
			}
			var ge *URLGuardError
			if !errors.As(err, &ge) {
				t.Fatalf("错误不是 *URLGuardError: %T", err)
			}
			if ge.Reason != tc.want {
				t.Fatalf("拒绝依据不符\n  url:  %s\n  got:  %s\n  want: %s\n  理由: %s\n  文案: %s",
					tc.url, ge.Reason, tc.want, tc.why, err.Error())
			}
			// 文案必须说清「为什么」：只有 "invalid base_url" 会让用户反复试错。
			if ge.Detail == "" {
				t.Errorf("Detail 为空 —— 错误信息必须说清拒绝原因: %v", err)
			}
			if !strings.Contains(err.Error(), string(tc.want)) && !strings.Contains(err.Error(), tc.want.String()) {
				t.Errorf("文案里既没有 reason 值也没有中文标签: %s", err.Error())
			}
		})
	}
}

// guardAllowCase 一条「应当放行」的用例。
type guardAllowCase struct {
	name string
	url  string
	why  string
}

// guardAllowCases 放行表（默认策略）。
//
// 这张表的存在理由：任何回归都会让它变红，而它大红的方式是"误拒" ——
// 误拒比漏拒更容易被用户发现，但也更容易被"放宽校验"错误地修掉。
var guardAllowCases = []guardAllowCase{
	{name: "公网域名", url: "https://api.openai.com/v1", why: "最常见形态"},
	{name: "公网域名_带端口", url: "https://api.example.com:8443/v1", why: "非标准端口本身不是问题"},
	{name: "公网域名_带路径与查询", url: "https://gw.example.com/openai/v1?region=cn", why: "path / query 不参与 SSRF 判定"},
	{name: "公网IP", url: "http://8.8.8.8/v1", why: "字面量公网 IP 是合法的自建上游写法"},
	{name: "公网IP_1_1_1_1", url: "https://1.1.1.1/", why: "同上"},
	{name: "公网IPv6", url: "https://[2606:4700:4700::1111]/v1", why: "IPv6 公网地址必须放行 —— 一刀切拒 IPv6 是错误做法"},
	{name: "172段之外", url: "http://172.32.0.1/", why: "172.32 开始已经是公网；写成 172/8 之类会误伤整个公网"},
	{name: "172段之下", url: "http://172.15.255.255/", why: "172.16/12 的下界之外"},
	{name: "192段之外", url: "http://192.167.255.255/", why: "192.168/16 之外"},
	{name: "192段之上", url: "http://192.169.0.1/", why: "同上"},
	{name: "100段之下", url: "http://100.63.255.255/", why: "100.64/10 之外是公网"},
	{name: "100段之上", url: "http://100.128.0.1/", why: "100.64/10 是 /10，只覆盖 64-127"},
	{name: "10段之外", url: "http://9.255.255.255/", why: "10/8 之外"},
	{name: "10段之上", url: "http://11.0.0.1/", why: "同上"},
	{name: "域名末尾根点", url: "https://api.example.com./v1", why: "FQDN 的根点是合法写法，不该因此被拒"},
	{name: "大写字", url: "HTTPS://API.EXAMPLE.COM/V1", why: "scheme 与主机名都大小写不敏感"},
}

// TestURLGuardAllowsPublic 放行表：合法公网地址一个字都不许拦。
func TestURLGuardAllowsPublic(t *testing.T) {
	g := DefaultURLGuard()
	for _, tc := range guardAllowCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := g.ValidateBaseURL(tc.url); err != nil {
				t.Fatalf("不应被拒\n  url: %s\n  理由: %s\n  err: %v", tc.url, tc.why, err)
			}
		})
	}
}

// TestURLGuardAllowlist 白名单：自建内网上游是合理需求，必须能逐条放行。
func TestURLGuardAllowlist(t *testing.T) {
	type allowlistCase struct {
		name  string
		hosts []string
		url   string
		ok    bool
		why   string
	}
	cases := []allowlistCase{
		{name: "精确IP放行", hosts: []string{"10.0.0.5"}, url: "http://10.0.0.5:8000/v1", ok: true,
			why: "逐台授权：网段 10/8 里只放行这一台"},
		{name: "同网段其它IP仍拒", hosts: []string{"10.0.0.5"}, url: "http://10.0.0.6/", ok: false,
			why: "白名单不是网段通配 —— 精确条目只放行它自己"},
		{name: "CIDR放行", hosts: []string{"192.168.0.0/16"}, url: "http://192.168.1.10/", ok: true,
			why: "整段内网上游（K8s / VPC 场景）"},
		{name: "CIDR之外的私网仍拒", hosts: []string{"192.168.0.0/16"}, url: "http://10.0.0.1/", ok: false,
			why: "掩码必须真的生效：放行 192.168/16 不该顺带放行其它私网段"},
		{name: "通配子域放行", hosts: []string{"*.corp.example.com"}, url: "https://api.corp.example.com/v1", ok: true,
			why: "内网上游常有一组同域主机"},
		{name: "通配也匹配裸域", hosts: []string{"*.corp.example.com"}, url: "https://corp.example.com/v1", ok: true,
			why: "与 new-api 的 isDomainListed 一致：少一个例外就少一类「配了没用」的困惑"},
		{name: "条目大小写与根点归一", hosts: []string{"API.Example.COM."}, url: "https://api.example.com/v1", ok: true,
			why: "手抄配置的大小写/根点差异不该让白名单静默失效"},
		{name: "单标签主机放行", hosts: []string{"buildbox"}, url: "http://buildbox:8080/v1", ok: true,
			why: "自建上游最常写成内网主机名而不是 IP"},
		{name: "metadata可显式放行", hosts: []string{"169.254.169.254"}, url: "http://169.254.169.254/latest/meta-data/", ok: true,
			why: "白名单是唯一正规的「我知道我在做什么」通道；有能力显式放行，才不会被整体关掉"},
		{name: "回环可显式放行", hosts: []string{"127.0.0.1"}, url: "http://127.0.0.1:11434/v1", ok: true,
			why: "同机部署的本地推理服务（ollama 等）是真实需求"},
		{name: "IPv6字面量放行", hosts: []string{"fe80::1"}, url: "http://[fe80::1]/v1", ok: true,
			why: "IPv6 内网上游同样要能配"},
		{name: "白名单不放行非http协议", hosts: []string{"localhost"}, url: "file://localhost/etc/passwd", ok: false,
			why: "scheme 检查先于主机检查：白名单授权的是「主机」，不是「任意协议」"},
		{name: "白名单不放行伪IP", hosts: []string{"api.example.com"}, url: "http://999.1.1.1/", ok: false,
			why: "白名单不该顺手把非法形态也放过（那是配置错误，不是授权）"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := NewURLGuard(URLGuardConfig{AllowHosts: tc.hosts})
			if err != nil {
				t.Fatalf("配置应当合法: %v", err)
			}
			err = g.ValidateBaseURL(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("白名单 %v 应放行 %q，但被拒: %v\n  理由: %s", tc.hosts, tc.url, err, tc.why)
			}
			if !tc.ok && err == nil {
				t.Fatalf("白名单 %v 不应放行 %q\n  理由: %s", tc.hosts, tc.url, tc.why)
			}
		})
	}
}

// TestURLGuardHostRuleMatch 白名单后缀匹配必须锚在**点边界**上。
//
// # 为什么不放在白名单集成表里测
//
// 因为那个表测的是「整体放行/拒绝」，而被绕过的后缀匹配往往**不影响最终结论**：
// evilcorp.example.com 本来就是个公网域名，默认策略也放行 —— 于是"匹配错了"
// 这件事在集成层面完全不可见。这类"错误被别的正确决定掩盖"的洞只能拆开测。
func TestURLGuardHostRuleMatch(t *testing.T) {
	rule := guardHostRule{suffix: "corp.example.com"}
	cases := []struct {
		host string
		want bool
		why  string
	}{
		{"corp.example.com", true, "*.x 也匹配 x 本身（与 new-api 一致）"},
		{"api.corp.example.com", true, "正常子域"},
		{"a.b.corp.example.com", true, "多级子域同样命中"},
		{"x.corp.example.com.evil.net", false, "后缀必须锚在点边界：否则 evil.net 可伪装成子域"},
		{"evilcorp.example.com", false, "前缀粘连不是子域"},
		{"corp.example.com.evil.net", false, "域名在后缀之前出现不算"},
		{"other.example.com", false, "不同子域"},
		{"corp.example.co", false, "少一层"},
	}
	for _, tc := range cases {
		if got := rule.match(tc.host); got != tc.want {
			t.Errorf("rule(*.corp.example.com).match(%q) = %v，期望 %v（%s）",
				tc.host, got, tc.want, tc.why)
		}
	}

	exact := guardHostRule{exact: "api.example.com"}
	if !exact.match("api.example.com") {
		t.Error("精确条目必须命中它自己")
	}
	if exact.match("x.api.example.com") {
		t.Error("精确条目不得命中子域")
	}
	// 空规则不得命中任何东西（构造期虽已排除，但匹配是热路径，不能有"空即全通"）。
	if (guardHostRule{}).match("api.example.com") {
		t.Error("空规则不得命中任何主机")
	}
}

// TestURLGuardAllowPrivate allow_private：放内网，但**不放**回环与 metadata。
//
// 这条测试钉住的是本实现与 new-api 的**刻意偏离**（new-api 的 allowPrivateIp
// 把 metadata 一起放了）。偏离的理由写在 URLGuardConfig.AllowPrivate 的注释里，
// 这里用测试让它变成可执行的事实，而不是一句注释。
func TestURLGuardAllowPrivate(t *testing.T) {
	g, err := NewURLGuard(URLGuardConfig{AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	allow := []struct{ url, why string }{
		{"http://10.0.0.5:8000/v1", "VPC 内上游"},
		{"http://172.16.3.4/", "VPC 内上游"},
		{"http://192.168.1.10/", "机房内上游"},
		{"http://100.64.1.1/", "运营商 NAT 段的内网服务"},
		{"http://169.254.1.1/", "链路本地：私有部署里也有这种地址"},
		{"http://[fc00::1]/", "IPv6 ULA 内网"},
		{"http://buildbox:8080/v1", "内网主机名（单标签）—— 只放松 IP 不够用"},
		{"http://nas.local/", "mDNS 内网名"},
		{"http://vault.internal/", "私有 TLD"},
		{"http://192.0.2.10/", "保留段在私有部署里常被当内部地址用"},
	}
	for _, tc := range allow {
		if err := g.ValidateBaseURL(tc.url); err != nil {
			t.Errorf("allow_private 下应放行 %q（%s），实际被拒: %v", tc.url, tc.why, err)
		}
	}

	// 仍必须拒的：回环 / metadata / DNS 别名 / 非 http。
	still := []struct {
		url  string
		want URLGuardReason
		why  string
	}{
		{"http://127.0.0.1:8080/v1", URLGuardReasonLoopback,
			"回环指向网关自己，任何「内网上游」都不该是网关自身"},
		{"http://localhost:7868/", URLGuardReasonLoopback,
			"localhost 同回环"},
		{"http://169.254.169.254/latest/meta-data/", URLGuardReasonMetadata,
			"metadata 不是「内网上游」，是这台机器的云身份 —— 放行它等于放行凭据读取"},
		{"http://100.100.100.200/", URLGuardReasonMetadata,
			"阿里云 metadata 同样单列"},
		{"http://[fd00:ec2::254]/", URLGuardReasonMetadata,
			"AWS IMDS v6 落在 ULA 里，allow_private 会顺带放开 ULA ⇒ 必须单独拦"},
		{"http://127.0.0.1.nip.io/", URLGuardReasonDNSAlias,
			"DNS 别名服务没有任何内网上游用途，唯一作用是藏 IP 绕过检查"},
		{"gopher://10.0.0.5:6379/_INFO", URLGuardReasonScheme,
			"allow_private 不绕过 scheme 检查"},
	}
	for _, tc := range still {
		err := g.ValidateBaseURL(tc.url)
		if err == nil {
			t.Errorf("allow_private 下仍应拒绝 %q（%s）", tc.url, tc.why)
			continue
		}
		var ge *URLGuardError
		if errors.As(err, &ge) && ge.Reason != tc.want {
			t.Errorf("%q 的拒绝依据应是 %s，实际 %s（%s）", tc.url, tc.want, ge.Reason, tc.why)
		}
	}

	// allow_private + 白名单：显式条目仍能放行回环（"我知道我在做什么"通道）。
	g2, _ := NewURLGuard(URLGuardConfig{AllowPrivate: true, AllowHosts: []string{"127.0.0.1"}})
	if err := g2.ValidateBaseURL("http://127.0.0.1:11434/v1"); err != nil {
		t.Errorf("显式白名单应能放行回环: %v", err)
	}
}

// TestURLGuardConfigErrors 白名单条目写错必须报错，而不是静默不生效。
func TestURLGuardConfigErrors(t *testing.T) {
	cases := []struct{ name, entry, why string }{
		{"带端口", "10.0.0.5:8080", "白名单只判主机；带端口的条目会「配了看似生效、实际永不命中」"},
		{"带scheme", "https://api.example.com", "条目是主机而不是 URL"},
		{"裸星号", "*", "等于关掉整个校验 —— 要关请显式设 allow_private 或删掉校验，不要伪装成白名单"},
		{"中间星号", "api.*.example.com", "只支持前缀通配，中途通配会让匹配规则无法预测"},
		{"空后缀", "*.", "通配后缀为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewURLGuard(URLGuardConfig{AllowHosts: []string{tc.entry}}); err == nil {
				t.Fatalf("条目 %q 应被拒（%s）", tc.entry, tc.why)
			}
		})
	}

	// 合法条目不应报错（含空白与空串：配置里留空行是常态）。
	if _, err := NewURLGuard(URLGuardConfig{AllowHosts: []string{"", "  ", "api.example.com"}}); err != nil {
		t.Fatalf("合法配置报错: %v", err)
	}
}

// TestURLGuardResolvedHostIP 拨号层复核（防 DNS 重绑定）。
//
// # 这条测试为什么必须存在
//
// ValidateBaseURL 只看字面量主机。一个攻击者可控的域名可以在「保存渠道」与
// 「真正发请求」之间改变解析结果 —— 只有拨号层的复核能拦住它。
// 这里模拟的场景就是那一步：域名看着合法，解析出来是 127.0.0.1。
func TestURLGuardResolvedHostIP(t *testing.T) {
	rebind := net.ParseIP("127.0.0.1")
	private := net.ParseIP("10.0.0.5")

	t.Run("默认策略挡住重绑定", func(t *testing.T) {
		g := DefaultURLGuard()
		err := g.ValidateResolvedHostIP("api.evil.example", rebind)
		if err == nil {
			t.Fatal("域名解析到 127.0.0.1 必须被拒（DNS 重绑定）")
		}
		var ge *URLGuardError
		if errors.As(err, &ge) && ge.Reason != URLGuardReasonLoopback {
			t.Fatalf("依据应是 loopback，实际 %s", ge.Reason)
		}
	})

	t.Run("域名解析到私网也挡", func(t *testing.T) {
		g := DefaultURLGuard()
		if err := g.ValidateResolvedHostIP("api.evil.example", private); err == nil {
			t.Fatal("域名解析到 10.0.0.5 必须被拒")
		}
	})

	t.Run("白名单按主机名授权到拨号层", func(t *testing.T) {
		g, _ := NewURLGuard(URLGuardConfig{AllowHosts: []string{"*.corp.example.com"}})
		if err := g.ValidateResolvedHostIP("api.corp.example.com", private); err != nil {
			t.Fatalf("被白名单的内网主机名应放行: %v", err)
		}
		// 同一个 IP，但主机名没被授权 ⇒ 拒。证明授权的是「主机」而不是「网段」。
		if err := g.ValidateResolvedHostIP("other.example.com", private); err == nil {
			t.Fatal("未授权主机名解析到私网必须被拒")
		}
	})

	t.Run("allow_private 放内网但挡metadata与回环", func(t *testing.T) {
		g, _ := NewURLGuard(URLGuardConfig{AllowPrivate: true})
		if err := g.ValidateResolvedHostIP("upstream.vpc", private); err != nil {
			t.Fatalf("allow_private 下应放行私网: %v", err)
		}
		if err := g.ValidateResolvedHostIP("upstream.vpc", net.ParseIP("169.254.169.254")); err == nil {
			t.Fatal("allow_private 不应放行 metadata")
		}
		if err := g.ValidateResolvedHostIP("upstream.vpc", rebind); err == nil {
			t.Fatal("allow_private 不应放行回环")
		}
	})

	t.Run("公网IP放行", func(t *testing.T) {
		g := DefaultURLGuard()
		if err := g.ValidateResolvedHostIP("api.openai.com", net.ParseIP("104.18.7.1")); err != nil {
			t.Fatalf("公网地址应放行: %v", err)
		}
	})

	t.Run("非法IP被拒", func(t *testing.T) {
		g := DefaultURLGuard()
		if err := g.ValidateResolvedHostIP("api.example.com", nil); err == nil {
			t.Fatal("nil IP 应被拒（否则调用方传错参数就变成静默放行）")
		}
		if err := g.ValidateResolvedHostIP("api.example.com", net.IP{1, 2, 3}); err == nil {
			t.Fatal("长度非法的 IP 应被拒")
		}
	})

	t.Run("IPv4映射地址在拨号层也还原", func(t *testing.T) {
		g := DefaultURLGuard()
		// net.ParseIP("::ffff:10.0.0.5") 的 16 字节形态：必须被当作 10.0.0.5 判。
		if err := g.ValidateResolvedHostIP("api.example.com", net.ParseIP("::ffff:10.0.0.5")); err == nil {
			t.Fatal("IPv4-mapped 的私网地址必须被拒")
		}
	})
}

// TestURLGuardLooseIPv4 宽松 IPv4 解析：inet_aton 语义。
//
// 单独一条测试，因为这十几行代码是「宁可多判」的取舍（见 parseLooseIPv4 的注释），
// 而它的边界（越界、八进制里的 8、超过四段）不写清楚就一定会被后来者放宽。
func TestURLGuardLooseIPv4(t *testing.T) {
	cases := []struct {
		in   string
		want string // 解析成功时的规范形态；空表示应当解析失败
		why  string
	}{
		{"127.1", "127.0.0.1", "两段：a.b ⇒ a(8).b(24)"},
		{"127.0.1", "127.0.0.1", "三段：a.b.c ⇒ a(8).b(8).c(16)，c=1 落在低 16 位"},
		{"2130706433", "127.0.0.1", "单段当作 32 位数"},
		{"0x7f.0.0.1", "127.0.0.1", "每段允许 0x 前缀"},
		{"0X7F.0.0.1", "127.0.0.1", "0X 大写同样接受"},
		{"0177.0.0.1", "127.0.0.1", "前导 0 = 八进制"},
		{"10.1", "10.0.0.1", "非回环的两段写法：10.1 ⇒ 10.0.0.1"},
		{"10.0.1", "10.0.0.1", "三段写法：256 以内的最后一段落在低 16 位"},
		{"0x0a.1", "10.0.0.1", "十六进制 + 两段混合"},
		{"8.8.8.8", "8.8.8.8", "规范形态照样解析"},
		{"", "", "空串"},
		{"999.1.1.1", "", "段越界：glibc 也会拒（不像某些实现那样截断成 999&0xff）"},
		{"1.2.3.4.5", "", "超过四段"},
		{"08.0.0.1", "", "八进制段里出现 8 —— glibc 的 strtol 同样拒绝"},
		{"+127.0.0.1", "", "不接受符号"},
		{"-1", "", "负数字面量"},
		{"a.b.c.d", "", "非数字段"},
		{"example.com", "", "普通域名不是 IP"},
	}
	for _, tc := range cases {
		addr, ok := parseLooseIPv4(tc.in)
		if tc.want == "" {
			if ok {
				t.Errorf("parseLooseIPv4(%q) 应失败，得到 %s（%s）", tc.in, addr, tc.why)
			}
			continue
		}
		if !ok {
			t.Errorf("parseLooseIPv4(%q) 应成功（%s）", tc.in, tc.why)
			continue
		}
		if addr.String() != tc.want {
			t.Errorf("parseLooseIPv4(%q) = %s，期望 %s（%s）", tc.in, addr, tc.want, tc.why)
		}
	}
}

// TestURLGuardErrorKeepsNoCredentials 错误文案不得带出 userinfo / path / query。
//
// # 为什么这条是硬要求
//
// 这个错误会原样出现在管理台弹窗、API 响应与日志里（它是**入站**错误，
// 不像出站错误那样会被 errmask 再过一遍）。而 base_url 里塞凭据是常见手滑：
//
//	https://user:pass@internal-gw/v1?api_key=sk-xxx   ← 三段敏感信息
//
// 拒绝时只需要说清「哪台主机不行」，多说一句都是白送。
func TestURLGuardErrorKeepsNoCredentials(t *testing.T) {
	g := DefaultURLGuard()
	err := g.ValidateBaseURL("http://user:sup3rsecret@10.0.0.5:8080/tenant/acme/v1?api_key=sk-live-abc123")
	if err == nil {
		t.Fatal("应当被拒")
	}
	msg := err.Error()
	for _, leak := range []string{"sup3rsecret", "user:", "sk-live-abc123", "api_key", "tenant/acme"} {
		if strings.Contains(msg, leak) {
			t.Errorf("错误文案泄漏了 %q: %s", leak, msg)
		}
	}
	// 但主机与端口要保留 —— 否则用户不知道是哪一条地址被拒。
	if !strings.Contains(msg, "10.0.0.5") || !strings.Contains(msg, "8080") {
		t.Errorf("错误文案应保留主机与端口: %s", msg)
	}

	// Input 字段同样不能带 userinfo / query。
	var ge *URLGuardError
	if errors.As(err, &ge) {
		for _, leak := range []string{"sup3rsecret", "api_key", "tenant"} {
			if strings.Contains(ge.Input, leak) {
				t.Errorf("URLGuardError.Input 泄漏了 %q: %q", leak, ge.Input)
			}
		}
	}
}

// TestURLGuardErrorExplainsItself 文案必须「说清拒绝原因 + 怎么放行」。
func TestURLGuardErrorExplainsItself(t *testing.T) {
	g := DefaultURLGuard()

	err := g.ValidateBaseURL("http://10.0.0.5:8000/v1")
	if err == nil {
		t.Fatal("应当被拒")
	}
	msg := err.Error()
	wants := []string{
		"SSRF",        // 说明这是安全策略，不是"地址写错了"
		"10.0.0.5",    // 哪台主机
		"私网地址",        // 哪一类
		"10.0.0.0/8",  // 精确到段：用户能据此判断"我是不是确实在内网"
		"allow_hosts", // 怎么放行
	}
	for _, w := range wants {
		if !strings.Contains(msg, w) {
			t.Errorf("文案缺少 %q: %s", w, msg)
		}
	}

	// metadata 的 hint 必须不同（它不该建议「加白名单」了事）。
	err = g.ValidateBaseURL("http://169.254.169.254/latest/meta-data/")
	if err == nil {
		t.Fatal("应当被拒")
	}
	if !strings.Contains(err.Error(), "元数据") {
		t.Errorf("metadata 拒绝文案应点明「元数据」: %s", err.Error())
	}
}

// TestURLGuardReasonContract 拒绝原因的取值是对外契约，只许追加。
//
// # 为什么钉死字符串
//
// 管理台要按 reason 提示「加白名单」还是「换 scheme」，告警要按 reason 分级
// （metadata / loopback 应当触发安全告警）。改一个取值 = 静默改变这些分支的行为，
// 而不会有任何编译错误。与 errcode.go 的取向一致：取值钉在这里，改动必须显式。
func TestURLGuardReasonContract(t *testing.T) {
	want := map[URLGuardReason]string{
		URLGuardReasonMalformed:    "malformed",
		URLGuardReasonScheme:       "scheme",
		URLGuardReasonLoopback:     "loopback",
		URLGuardReasonPrivate:      "private",
		URLGuardReasonLinkLocal:    "link_local",
		URLGuardReasonULA:          "ula",
		URLGuardReasonMetadata:     "metadata",
		URLGuardReasonReserved:     "reserved",
		URLGuardReasonInternalName: "internal_name",
		URLGuardReasonDNSAlias:     "dns_alias",
	}
	seen := map[string]bool{}
	for r, s := range want {
		if string(r) != s {
			t.Errorf("拒绝原因取值变了：%q，契约里是 %q", r, s)
		}
		if seen[s] {
			t.Errorf("拒绝原因取值重复: %s", s)
		}
		seen[s] = true
		if r.String() == "" || r.String() == s {
			t.Errorf("原因 %s 缺少中文标签（错误文案会退化成英文 snake_case）", s)
		}
	}
}

// TestURLGuardZeroValueIsStrict 零值与 nil 接收者必须是**最严**策略。
//
// # 为什么这条很重要（fail-open 是安全组件最典型的死法）
//
// 「装配层忘了注入配置」在真实项目里一定会发生一次。此时如果零值等价于
// 「不校验」，那么这个功能只有在被正确配置时才生效 —— 而它对配置错误的
// 惩罚恰好是**静默失效**。所以零值的默认行为必须是拒绝内网。
func TestURLGuardZeroValueIsStrict(t *testing.T) {
	var zero URLGuard
	if err := zero.ValidateBaseURL("http://127.0.0.1/"); err == nil {
		t.Error("零值 URLGuard 必须拒绝回环（默认最严）")
	}
	if zero.AllowsPrivate() {
		t.Error("零值不应放开私网")
	}

	// nil 接收者：方法必须安全（不 panic、仍按最严策略判）。
	var nilGuard *URLGuard
	if nilGuard.AllowsPrivate() {
		t.Error("nil 接收者不应放开私网")
	}
	if err := nilGuard.ValidateBaseURL("http://10.0.0.5/"); err == nil {
		t.Error("nil 接收者必须拒绝私网")
	}
	if err := nilGuard.ValidateBaseURL("https://api.openai.com/v1"); err != nil {
		t.Errorf("nil 接收者不应误拒公网: %v", err)
	}
	if err := nilGuard.ValidateResolvedHostIP("h", net.ParseIP("127.0.0.1")); err == nil {
		t.Error("nil 接收者在拨号层也必须拒绝回环")
	}

	// 默认策略入口与零值同义。
	if err := CheckBaseURL("http://192.168.1.1/"); err == nil {
		t.Error("CheckBaseURL 必须按默认（最严）策略判")
	}
	if err := CheckBaseURL("https://api.openai.com/v1"); err != nil {
		t.Errorf("CheckBaseURL 不应误拒公网: %v", err)
	}
}

// TestURLGuardBlocksEveryReservedRange 逐个内置网段取样，确认**每一个**都真的被拦。
//
// # 为什么每个 CIDR 都要各取一次样
//
// 表驱动的主用例只覆盖了"我想到的地址"。而 guardIPv4Rules / guardIPv6Rules
// 是**数据** —— 加一行写错掩码（比如把 172.16.0.0/12 写成 /16）不会被任何
// 编译错误发现，只会让半个网段静默放行。这条测试从数据本身出发取样：
// 漏挡一定会被它抓到，而且报错直接指出是哪一行。
func TestURLGuardBlocksEveryReservedRange(t *testing.T) {
	check := func(label string, addr string) {
		t.Helper()
		g := DefaultURLGuard()
		url := "http://" + addr + "/"
		if strings.Contains(addr, ":") {
			url = "http://[" + addr + "]/"
		}
		if err := g.ValidateBaseURL(url); err == nil {
			t.Errorf("内置网段 %s 的取样地址 %s 未被拦截（掩码或判据写错了）", label, addr)
		}
	}

	for _, r := range guardIPv4Rules {
		// 取网络地址本身作为样本（每个隐私段都包含它）。
		check(r.prefix.String(), r.prefix.Addr().String())
	}
	for _, r := range guardIPv6Rules {
		check(r.prefix.String(), r.prefix.Addr().String())
	}
	// metadata 表也逐个取样（它们不一定落在上面那些网段的取样点上）。
	for addr, label := range guardMetadataIPv4 {
		check(label, addr.String())
	}
	for addr, label := range guardMetadataIPv6 {
		check(label, addr.String())
	}
	// 内嵌 IPv4 的翻译前缀：**每个**前缀都要取一条"内嵌私网"的样本。
	//
	// 只取前缀的网段地址是不够的：64:ff9b::/96 的网络地址内嵌的是 0.0.0.0
	// （单靠前缀匹配就会被拦），而"内嵌 10.0.0.1"才真正依赖递归判定 ——
	// 少了递归，这个地址会被当普通公网 IPv6 放行。
	for _, p := range guardEmbeddedV4Prefixes {
		var raw [16]byte
		copy(raw[:], p.Addr().AsSlice())
		// 把最后 4 字节（内嵌的 IPv4 位置）设成 10.0.0.1。
		raw[12], raw[13], raw[14], raw[15] = 10, 0, 0, 1
		addr := netip.AddrFrom16(raw)
		check(p.String()+" 内嵌 10.0.0.1", addr.String())
	}
	check("NAT64 内嵌 10.0.0.1（显式形态）", "64:ff9b::a00:1")
}
