// listens_loopback_test.go 「本机免令牌」前提的守卫。
//
// # 为什么这个小小的解析函数值得单独一组测试
//
// 它是管理台门禁里**唯一**一处"从配置字面值推断安全属性"的地方：
// 它回答"这个部署会不会对外敞开"。判错一个值，后果不是"功能不对"，
// 而是**管理台对全网敞开**（用户报障的那件事）。
//
// 最容易错的一条：`:7863` 与 `0.0.0.0:7863` 都是"监听全部网卡"，
// 但 `net.SplitHostPort(":7863")` 给出**空串** host。若把空串当"本机"，
// 一个对外暴露的部署就被判成只本机可达 —— 于是"本机免令牌"对所有人生效。
package main

import "testing"

func TestListensLoopback(t *testing.T) {
	cases := []struct {
		listen string
		want   bool
		why    string
	}{
		// ---- 只本机可达 ----
		{"127.0.0.1:7863", true, "显式回环，最典型的本机部署"},
		{"localhost:7863", true, "localhost 解析到回环"},
		{"[::1]:7863", true, "IPv6 回环（**必须**带方括号，net.Listen 的合法写法）"},

		// ---- 对外可达（全部必须是 false）----
		{":7863", false, "⚠ 空 host = 全部网卡。最容易误判成'本机'的一条"},
		{"0.0.0.0:7863", false, "显式全部网卡"},
		{"[::]:7863", false, "IPv6 全部网卡"},
		{"192.168.0.5:7863", false, "显式内网地址（局域网可达）"},
		{"203.0.113.9:7863", false, "显式公网地址"},

		// ---- 畸形/边界：保守判 false（不放宽）----
		{"", false, "空配置：不放宽（畸形会在 ListenAndServe 时报错）"},
		{"garbage", false, "解析不出 host：不放宽"},
		{"7863", false, "只有端口没冒号：SplitHostPort 失败 ⇒ 整体当 host ⇒ 非回环"},
		// ⚠ 无方括号的 IPv6 是**畸形配置**（net.Listen 的合法写法是 `[::1]:7863`）。
		// 这里刻意要求 false：解析不出一个**可信**的回环结论时就不放宽 ——
		// 保守的代价只是"本机要填一次令牌"，而放宽的代价是管理台敞开。
		// （我第一版把这条写成 true，测试立刻红了 —— 期望本身是错的。）
		{"::1:7863", false, "无方括号 IPv6：畸形配置，不给予豁免"},
	}
	for _, c := range cases {
		if got := listensLoopback(c.listen); got != c.want {
			t.Errorf("listensLoopback(%q) = %v，期望 %v —— %s", c.listen, got, c.want, c.why)
		}
	}
}

// ⚠ 反向断言：`listensLoopback` 与 `isLoopbackHost` 的判据**不同**，不能互相替代。
//
// 空 host 在 isLoopbackHost 里返回 false（解析不出 IP），在这里**也必须** false。
// 但两者回答的是不同问题：
//
//	isLoopbackHost   "这个 host 是不是回环地址"
//	listensLoopback  "这个 listen 配置会不会对外敞开"
//
// 这条测试存在的意义是防止有人日后"顺手合并"这两个函数 —— 合并不了会
// 丢掉"空 host 是全部网卡"这条知识，而那正是本次安全修复的关键。
func TestListensLoopbackDiffersFromIsLoopbackHost(t *testing.T) {
	// 空 host：isLoopbackHost 说"不是回环"，listensLoopback 必须也说"不安全"。
	if isLoopbackHost("") {
		t.Error("isLoopbackHost(\"\") 应当 false（解析不出 IP）")
	}
	if listensLoopback(":7863") {
		t.Error("listensLoopback(\":7863\") 必须 false —— 空 host 是全部网卡，" +
			"判成回环会让一个对外暴露的部署获得「本机免令牌」")
	}
}
