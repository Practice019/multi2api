// startup_addr_test.go 启动横幅里那两个地址函数的守卫。
//
// # 用户的要求（原话）
//
//	「运行之后，命令行里需要打印出网关地址是多少，比如 http://127.0.0.1:7863」
//
// # 最容易写错的一点：通配绑定**不是**可访问地址
//
// `listen = ":7863"` 或 `"0.0.0.0:7863"` 时，直接拼出来的
// `http://0.0.0.0:7863` 在浏览器里**打不开** —— 而它恰恰是最容易被
// 用户随手复制的那一行。所以必须换成 `127.0.0.1`。
//
// 这类"日志给出误导性信息"的后果比不打还差：用户会以为网关没起来，
// 然后去查一个根本不存在的问题。
package main

import (
	"os"
	"strings"
	"testing"
)

func TestUIOrigin(t *testing.T) {
	cases := []struct {
		listen string
		want   string
		why    string
	}{
		{":7863", "http://127.0.0.1:7863", "裸端口 = 绑全部网卡，但要给**可点**的地址"},
		{"0.0.0.0:7863", "http://127.0.0.1:7863", "0.0.0.0 不是可访问地址，必须换掉"},
		{"127.0.0.1:7863", "http://127.0.0.1:7863", "本来就是回环，原样"},
		{"localhost:7863", "http://localhost:7863", "localhost 可点，保留"},
		{"192.168.1.5:7863", "http://192.168.1.5:7863", "显式绑内网 IP，原样"},
		{"[::]:7863", "http://127.0.0.1:7863", "IPv6 通配同样不是可访问地址"},
	}
	for _, c := range cases {
		if got := uiOrigin(c.listen); got != c.want {
			t.Errorf("uiOrigin(%q) = %q，want %q —— %s", c.listen, got, c.want, c.why)
		}
	}
}

// 回环绑定**不给**局域网地址（本机地址已经给了，多给一行是噪音）。
func TestExtraOriginsLoopbackIsEmpty(t *testing.T) {
	for _, l := range []string{"127.0.0.1:7863", "localhost:7863", "[::1]:7863"} {
		if got := extraOrigins(l); len(got) != 0 {
			t.Errorf("extraOrigins(%q) 应当为空（回环绑定没有「别的机器可访问」的地址），实际 %v", l, got)
		}
	}
}

// 显式绑了非回环地址 ⇒ 把它自己作为额外地址给出去（用户可能填了内网 IP）。
func TestExtraOriginsExplicitHost(t *testing.T) {
	got := extraOrigins("192.168.1.5:7863")
	if len(got) != 1 || got[0] != "http://192.168.1.5:7863" {
		t.Errorf("显式绑内网 IP 时应当把它自己作为额外地址，实际 %v", got)
	}
}

// 通配绑定 ⇒ 列出内网 IPv4（"从别的机器怎么打开"是真实问题）。
//
// ⚠ 只断言"拿到的是合法 http 地址且不含 0.0.0.0" ——
// 不断言具体 IP（那随机器变化，写死就是 flaky）。
func TestExtraOriginsWildcardListsPrivateIPv4(t *testing.T) {
	for _, l := range []string{":7863", "0.0.0.0:7863", "[::]:7863"} {
		got := extraOrigins(l)
		for _, o := range got {
			if !strings.HasPrefix(o, "http://") {
				t.Errorf("extraOrigins(%q) 给出了非 http 地址: %q", l, o)
			}
			if strings.Contains(o, "0.0.0.0") || strings.Contains(o, "[::]") {
				t.Errorf("extraOrigins(%q) 给出了不可访问的通配地址: %q —— "+
					"那正是这段代码要避免的东西", l, o)
			}
			if !strings.Contains(o, ":7863") {
				t.Errorf("extraOrigins(%q) 的地址丢了端口: %q", l, o)
			}
		}
		// 不强制要求非空：本机可能没有内网网卡（CI 容器里就是这样）。
		// 那条路径由 localIPv4s 返回 nil 兜住 —— 宁可少给一行，
		// 也不要编一个错的地址出来。
	}
}

// splitHostPort 的边界：不带端口的裸串不该 panic，也不该丢信息。
func TestSplitHostPortEdges(t *testing.T) {
	if h, p := splitHostPort("127.0.0.1:7863"); h != "127.0.0.1" || p != "7863" {
		t.Errorf("正常形态解析错: %q %q", h, p)
	}
	if h, p := splitHostPort(":7863"); h != "" || p != "7863" {
		t.Errorf("裸端口应得空 host: %q %q", h, p)
	}
	if h, p := splitHostPort("nonsense"); h != "nonsense" || p != "" {
		t.Errorf("无端口串应整体当 host（而不是报错/空串）: %q %q", h, p)
	}
}

// ⚠ 启动物里**不得**打印管理钥匙本身。
//
// 用户的原始顾虑就是"命令行会不会打印 Token"，而这把 key 权限最大
// （不限额度、不限模型、还能调 /admin/*）。打进终端 = 写进 scrollback、
// 写进他截的图、写进任何一份 `start.bat > log.txt`。
//
// 所以启动横幅只能**指路**（钥匙在哪个文件、界面在哪看），不能给值。
// 这条用源码级判据守住 —— 运行时测不出来（要抓 stdout）。
func TestStartupBannerDoesNotPrintAPIKey(t *testing.T) {
	raw, err := readMainGo()
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	// 反向：不允许出现 `... token=%s", ..., cfg.APIKey` 这类形状。
	for _, bad := range []string{
		`token=%s", cfg.APIKey`,
		`token=%s", ..., cfg.APIKey`,
		`%s<上面的局域网地址>/ui?token=%s", cfg.APIKey`,
	} {
		if strings.Contains(src, bad) {
			t.Errorf("启动横幅里出现了打印管理钥匙的形状 %q ——\n"+
				"那把 key 会进终端历史/日志/截图。只能指路（在哪个文件、界面上哪里看），不能给值。", bad)
		}
	}

	// 正向：必须有一条"到哪去拿"的提示（用户要求的"提醒他去哪里查看"）。
	if !strings.Contains(src, "顶层") || !strings.Contains(src, "api_key") {
		t.Error("启动横幅应当指路：告诉用户管理钥匙在配置文件顶层 api_key 字段、" +
			"或本机控制台的「API 接入信息」里看 —— 这是用户明确要求的")
	}
}

// readMainGo 读 main.go（接线守卫共用）。
func readMainGo() ([]byte, error) {
	return os.ReadFile("main.go")
}
