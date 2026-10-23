// errmask_test.go —— 错误脱敏的表驱动测试。
//
// # 为什么这张表里「不改」的用例比「改」的还多
//
// 脱敏是**全局**作用在所有错误文本上的，误伤的代价比漏打更高：
// 漏打只影响一次可能被探测的信息泄漏，误伤会让一整类排障线索永久消失，
// 而且没人会想到去查脱敏 —— 看到 handler.go 变 ***.go 只会以为日志被截断了。
// 所以表里同时钉住两个方向：
//
//	改：host / path / query 值 / userinfo / IPv4 / IPv6 / 裸域名 / api_key
//	不改：行号、时间戳、文件名、HTTP 语义词、带点号的普通标识符
//
// 每个「不改」的用例都对应实现在某处刻意的克制（TLD 白名单、net.ParseIP 复核），
// 谁把这些克制删掉，表里就会有对应用例变红。
package gateway

import (
	"strings"
	"testing"
)

// TestMaskErrorText 主表：逐条钉住输入与期望输出。
func TestMaskErrorText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		why  string
	}{
		// ── 带 scheme 的 URL ────────────────────────────────────────────
		{
			name: "https_主机与路径",
			in:   `Post "https://api.opencsg.com/v1/chat/completions": dial tcp: connection refused`,
			want: `Post "https://***.com/***/***/***": dial tcp: connection refused`,
			why:  "引号是定界符，必须原样留下（下游会把它拼进 JSON 或日志）",
		},
		{
			name: "https_保留端口",
			in:   `GET https://api.example.com:8443/v1/models: 502`,
			want: `GET https://***.com:8443/***/***: 502`,
			why:  "端口是第一排障信息：8443 说明没走标准 TLS 端口，是配置问题而非网络问题",
		},
		{
			name: "query_保名打值",
			in:   `GET https://api.example.com/v1/models?key=secret&alt=sse`,
			want: `GET https://***.com/***/***?key=***&alt=***`,
			why:  "alt=sse 这个名字解释了「为什么是流式请求失败」，值才是秘密",
		},
		{
			name: "query_无值参数保留",
			in:   `GET https://api.example.com/health?verbose`,
			want: `GET https://***.com/***?verbose`,
			why:  "无值参数不携带内容，改名是自找麻烦",
		},
		{
			name: "query_顺序不重排",
			in:   `https://api.example.com/x?z=1&a=2`,
			want: `https://***.com/***?z=***&a=***`,
			why:  "url.ParseQuery 会按字母排序；对比日志时会让人以为参数集合变了",
		},
		{
			name: "userinfo_抹掉凭据",
			in:   `https://user:pass@api.example.com/v1/token`,
			want: `https://***@***.com/***/***`,
			why:  "userinfo 是 basic auth 凭据，与 host 是否是上游无关",
		},
		{
			name: "userinfo_带端口",
			in:   `https://admin:s3cr3t@gw.example.io:7868/admin/login`,
			want: `https://***@***.io:7868/***/***`,
			why:  "userinfo + 端口 + 路径同时出现时三者各自按规则处理，不要互相吞",
		},
		{
			name: "fragment_是凭据不是路径",
			in:   `https://auth.example.com/token#access_token=abc123def`,
			want: `https://***.com/***#***`,
			why:  "OAuth 隐式流把 token 放在 # 后面，整体抹掉最省事也最安全",
		},
		{
			name: "二级后缀整体保留",
			in:   `https://api.vendor.co.uk:443/v1/x`,
			want: `https://***.co.uk:443/***/***`,
			why:  "***.uk 会把 co 误当成子域，等于制造假信息",
		},
		{
			name: "ws_也打码",
			in:   `wss://stream.example.com/v1/realtime failed`,
			want: `wss://***.com/***/*** failed`,
			why:  "流式重连错误里出现 wss 是常态",
		},
		{
			name: "括号包裹的URL",
			in:   `(see https://api.example.com:8080/v1/errors)`,
			want: `(see https://***.com:8080/***/***)`,
			why:  "括号是标点，不该被吃进路径；但路径里的合法括号要保留（见 trimURLSuffix 的配对判断）",
		},
		{
			name: "句尾句号",
			in:   `connect to https://api.example.com/v1/x.`,
			want: `connect to https://***.com/***/***.`,
			why:  "句号是句子的，不是 URL 的",
		},
		{
			name: "URL里的IPv6主机",
			in:   `https://[2001:db8::1]:8443/v1/x`,
			want: `https://[***]:8443/***/***`,
			why:  "IPv6 必须用方括号形态，url.Port() 才解析得出来",
		},

		// ── IP ──────────────────────────────────────────────────────────
		{
			name: "IPv4_带端口",
			in:   `dial tcp 192.168.1.10:8443: connect: connection refused`,
			want: `dial tcp ***.***.***.***:8443: connect: connection refused`,
			why:  "端口保留，地址打码 —— 内网拓扑不对外",
		},
		{
			name: "IPv6_方括号",
			in:   `dial tcp [2001:db8::1]:443: connect: no route to host`,
			want: `dial tcp [***]:443: connect: no route to host`,
			why:  "Go 网络错误对 IPv6 统一用方括号",
		},
		{
			name: "IPv6_带zone",
			in:   `dial tcp [fe80::1%eth0]:80: connect: invalid argument`,
			want: `dial tcp [***]:80: connect: invalid argument`,
			why:  "网卡名（eth0）也是内网拓扑，跟着一起抹",
		},
		{
			name: "IPv6_裸形态",
			in:   `lookup 2001:db8::1: no such host`,
			want: `lookup [***]: no such host`,
			why:  "DNS 错误里 IPv6 常不带方括号；句尾冒号要留在外面",
		},

		// ── 裸域名 ──────────────────────────────────────────────────────
		{
			name: "裸域名_DNS错误",
			in:   `dial tcp: lookup api.opencsg.com on 10.0.0.1:53: no such host`,
			want: `dial tcp: lookup ***.com on ***.***.***.***:53: no such host`,
			why:  "DNS 解析失败时地址以裸域名出现，没有 scheme 可依赖",
		},
		{
			name: "裸域名_二级后缀",
			in:   `dial tcp: lookup api.vendor.co.uk: no such host`,
			want: `dial tcp: lookup ***.co.uk: no such host`,
			why:  "co.uk 整体保留，见 maskDomainTail",
		},
		{
			name: "裸域名_子域层级不保留",
			in:   `dial tcp: lookup internal-auth.corp.example.com: no such host`,
			want: `dial tcp: lookup ***.com: no such host`,
			why:  "internal-auth 这类子域比域名本身更能定位内网结构，不能留",
		},
		{
			name: "localhost",
			in:   `dial tcp localhost:7868: connect: connection refused`,
			want: `dial tcp ***:7868: connect: connection refused`,
			why:  "localhost 没有点，裸域名规则碰不到；但它暴露「错误出在网关自身」",
		},

		// ── 凭据形态 ────────────────────────────────────────────────────
		{
			name: "api_key_冒号形态",
			in:   `api_key:AIzaSyAAAaUooTUni8AdaOkSRMda30n_Q4vrV70`,
			want: `api_key:***`,
			why:  "与 new-api 的 mask.go 输出逐字一致，便于两边日志对照",
		},
		{
			name: "api_key_JSON形态",
			in:   `{"error":{"message":"invalid key","api_key":"sk-live-abcdefghijklmnop123456"}}`,
			want: `{"error":{"message":"invalid key","api_key":"***"}}`,
			why:  "上游错误体常把出站凭据回显一遍；引号与冒号必须留着，否则体就不合法了",
		},
		{
			name: "bearer头",
			in:   `Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc.def rejected`,
			want: `Authorization: Bearer *** rejected`,
			why:  "只抹 token，保留 Authorization 与 Bearer 两个词 —— 它们说明「凭证被拒」",
		},
		{
			name: "sk形态",
			in:   `invalid key sk-proj-abcdefghijklmnopqrstuvwx`,
			want: `invalid key ***`,
			why:  "没有键名的裸 key 靠前缀 + 长度识别；门槛 16 位是为了不碰 sk-learn",
		},

		// ── 不该动的 ────────────────────────────────────────────────────
		{
			name: "普通文本_行号",
			in:   `error at line 3: unexpected EOF`,
			want: `error at line 3: unexpected EOF`,
			why:  "验收用例本身：普通文本一个字都不许动",
		},
		{
			name: "普通文本_超时文案",
			in:   `context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
			want: `context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
			why:  "Client.Timeout 形如域名，但 TLD 不在白名单 —— 这正是白名单存在的理由",
		},
		{
			name: "普通文本_时间戳",
			in:   `timeout at 12:30:45.123 elapsed`,
			want: `timeout at 12:30:45.123 elapsed`,
			why:  "冒号串像 IPv6，net.ParseIP 复核后放行",
		},
		{
			name: "普通文本_文件名与行号",
			in:   `open internal/gateway/handler.go:42: no such file or directory`,
			want: `open internal/gateway/handler.go:42: no such file or directory`,
			why:  ".go 刻意不在 TLD 白名单里：错误文本里它几乎总是文件名",
		},
		{
			name: "普通文本_配置文件名",
			in:   `parse config.yaml: unexpected end of JSON input`,
			want: `parse config.yaml: unexpected end of JSON input`,
			why:  "同 .go：.yaml/.mod/.sh 这类后缀一律不收",
		},
		{
			name: "普通文本_OpenSSL错误串",
			in:   `upstream connect error: TLS error: 268435703:SSL routines:OPENSSL_internal:WRONG_VERSION_NUMBER`,
			want: `upstream connect error: TLS error: 268435703:SSL routines:OPENSSL_internal:WRONG_VERSION_NUMBER`,
			why:  "OpenSSL 的错误串里全是冒号；裸 IPv6 规则不复核就会把它打成 [***]",
		},
		{
			name: "普通文本_中文",
			in:   `上游返回 HTTP 429（响应体片段：model capacity exceeded: tpm_exceeded）`,
			want: `上游返回 HTTP 429（响应体片段：model capacity exceeded: tpm_exceeded）`,
			why:  "我们的错误文案是中文，脱敏必须对中文无损",
		},
		{
			name: "普通文本_短值不当凭据",
			in:   `api_key: not set`,
			want: `api_key: not set`,
			why:  "值只有 3 个字符时不是凭据；打成 *** 会让人以为这里本来有个 key",
		},

		// ── 混合 ────────────────────────────────────────────────────────
		{
			name: "混合_多目标一次打完",
			in:   `Post "https://api.a.example.com:8443/v1/chat?key=k1": proxyconnect tcp: dial tcp 10.1.2.3:3128: connect: connection refused; retry via https://user:pw@b.example.io/x`,
			want: `Post "https://***.com:8443/***/***?key=***": proxyconnect tcp: dial tcp ***.***.***.***:3128: connect: connection refused; retry via https://***@***.io/***`,
			why:  "真实错误链里 URL、代理地址、userinfo 会同时出现；一轮要全部处理掉",
		},
		{
			name: "空串",
			in:   "",
			want: "",
			why:  "边界：出口层可能拿着空错误文本调用",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MaskErrorText(tc.in)
			if got != tc.want {
				t.Fatalf("MaskErrorText(%q)\n  got:  %q\n  want: %q\n  理由: %s", tc.in, got, tc.want, tc.why)
			}
		})
	}
}

// TestMaskErrorTextIdempotent 脱敏必须收敛。
//
// # 为什么这条是硬要求而不是锦上添花
//
// 错误会被多层包装、也可能被多个出口各脱敏一次。若函数不幂等，
// **脱敏发生了几次**就会改变对客户端可见的文案：
//
//	一次：***.co.uk    两次：***.***.co.uk
//
// 这类 bug 极难从现象上认出来（文案看着"也很像打码了"），只能靠测试钉住。
func TestMaskErrorTextIdempotent(t *testing.T) {
	inputs := []string{
		`dial tcp: lookup api.vendor.co.uk: no such host`,
		`https://api.example.com:8443/v1/x?key=1`,
		`https://user:pass@api.example.com/x`,
		`https://api.a.example.co.uk/x`,
		`dial tcp [2001:db8::1]:443: refused`,
		`dial tcp 192.168.1.10:443: refused`,
		`api_key:AIzaSyAAAaUooTUni8AdaOkSRMda30n_Q4vrV70`,
		`Authorization: Bearer abcdefghijkl`,
		`token sk-proj-abcdefghijklmnopqrstuvwx`,
		`dial tcp localhost:7868: refused`,
		`https://[2001:db8::1]:8443/v1/x`,
	}
	for _, in := range inputs {
		once := MaskErrorText(in)
		twice := MaskErrorText(once)
		if once != twice {
			t.Errorf("不幂等：%q\n  一次: %q\n  两次: %q", in, once, twice)
		}
	}
}

// TestMaskErrorTextLeavesNoIdentity 反向断言：打码后不能残留可识别的上游身份。
//
// # 为什么要有这条（表驱动正向用例挡不住的漏）
//
// 正向用例只验证「我想到的形态处理对了」。这一条换一个提法：
// 把种子（host 片段 / IP / key）列出来，要求它们在输出里**一个都不出现**。
// 以后有人新增打码规则、或把顺序调换（比如让域名规则先于 URL 规则跑），
// 只要产生漏打，这里就会红 —— 而且报错直接指出漏了哪一段。
func TestMaskErrorTextLeavesNoIdentity(t *testing.T) {
	seeds := []string{
		"opencsg", "openai", "example", "corp", "vendor", "internal-auth",
		"192.168", "10.0.0.5", "10.1.2.3", "2001:db8", "fe80::",
		"AIzaSy", "sk-live", "sk-proj", "eyJhbGciOiJIUzI1NiJ9", "s3cr3t", "hunter2",
		"localhost",
	}
	inputs := []string{
		`Post "https://api.opencsg.com/v1/chat/completions?key=sk-live-abc123456789": dial tcp 10.0.0.5:443: connect: connection refused`,
		`Post "https://api.openai.com/v1/x": proxyconnect tcp: dial tcp 10.1.2.3:3128: connect: connection refused`,
		`dial tcp: lookup internal-auth.corp.example.com: no such host`,
		`dial tcp [2001:db8::1]:443: no route to host`,
		`dial tcp [fe80::1%eth0]:80: invalid argument`,
		`api_key:AIzaSyAAAaUooTUni8AdaOkSRMda30n_Q4vrV70 rejected`,
		`Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc.def rejected`,
		`password:hunter2 rejected`,
		`https://admin:s3cr3t@gw.vendor.co.uk:7868/admin/login failed`,
		`dial tcp localhost:7868: connect: connection refused`,
		`bad key sk-proj-abcdefghijklmnopqrstuvwx`,
	}
	for _, in := range inputs {
		out := MaskErrorText(in)
		for _, seed := range seeds {
			if strings.Contains(strings.ToLower(out), strings.ToLower(seed)) {
				t.Errorf("脱敏后仍含种子 %q\n  in:  %s\n  out: %s", seed, in, out)
			}
		}
	}
}

// TestMaskErrorTextKeepsPorts 端口必须保留 —— 这是与 new-api 对齐时最容易丢的一点。
//
// 单独立一条（不混在正向表里），因为「把 ***.com:8443 打成 ***.com」这种退化
// 在正向表里只表现为一个字符串不相等，看不出它违反的是哪条设计意图。
func TestMaskErrorTextKeepsPorts(t *testing.T) {
	cases := []struct{ in, port string }{
		{`https://api.example.com:8443/v1/x`, ":8443"},
		{`dial tcp 192.168.1.10:8443: refused`, ":8443"},
		{`dial tcp [2001:db8::1]:443: refused`, ":443"},
		{`dial tcp localhost:7868: refused`, ":7868"},
	}
	for _, tc := range cases {
		out := MaskErrorText(tc.in)
		if !strings.Contains(out, tc.port) {
			t.Errorf("端口 %q 丢了\n  in:  %s\n  out: %s", tc.port, tc.in, out)
		}
	}
}

// TestMaskError 覆盖 error 版本的两个边界：nil 与包装后的错误。
func TestMaskError(t *testing.T) {
	if got := MaskError(nil); got != "" {
		t.Errorf("nil 错误应返回空串，实际 %q —— 返回 \"<nil>\" 会把实现细节漏进响应体", got)
	}
	err := &UpstreamHTTPError{Status: 502, Body: `boom at https://api.opencsg.com/v1/x`, Op: "chat"}
	got := MaskError(err)
	if strings.Contains(got, "opencsg") {
		t.Errorf("MaskError 漏打上游 host: %s", got)
	}
	if !strings.Contains(got, "HTTP 502") {
		t.Errorf("MaskError 不该动状态码文案: %s", got)
	}
}
