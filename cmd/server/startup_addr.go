// startup_addr.go 启动横幅要用的两个纯函数：地址归一化与"额外可访问地址"。
//
// # 为什么单独抽成纯函数
//
// `listen` 有三种形态（`:7863` / `0.0.0.0:7863` / `127.0.0.1:7863`），
// 而**只有中间那种是两个地址**（本机 + 局域网）。这类分支写在 main() 里
// 就没法测了 —— 而它恰好是最容易写错、且错了只有用户能发现的那种：
//
//	打错的结果是命令行给出一个**点不开的地址**（http://0.0.0.0:7863），
//	用户会以为是网关没起来。本仓在别处踩过"日志给出误导性信息"的形态。
package main

import (
	"net"
	"strings"
)

// uiOrigin 把配置里的 listen 归一化成**一定可点**的 http 源。
//
//	bindHost = ""（`:7863`）或 `0.0.0.0`（`0.0.0.0:7863`）→ `http://127.0.0.1:7863`
//	`127.0.0.1:7863`                                → `http://127.0.0.1:7863`
//	`192.168.1.5:7863`                             → `http://192.168.1.5:7863`
//
// ⚠ 通配绑定必须换成 `127.0.0.1`：`http://0.0.0.0:7863` 在浏览器里**打不开**，
// 而它恰恰是最容易随手复制的那一行。
//
// 协议一律 http：本程序**不透传 TLS**（要 HTTPS 是反代的事，见 README）。
// 所以这里不会凭 listen 猜出 https 来 —— 猜错的后果是让用户对着一个
// 不存在的 https 地址排查半天。
func uiOrigin(listen string) string {
	host, port := splitHostPort(listen)
	// 通配绑定（空 / 0.0.0.0 / ::）换成 127.0.0.1：`http://0.0.0.0:7863`
	// 在浏览器里打不开，而它恰恰是最容易被随手复制的那一行。
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), port)
}

// extraOrigins 返回**额外**可访问的地址（不含上面那个本机地址）。
//
// 只在绑定了非回环地址时返回局域网地址 —— 那时"从别的机器怎么打开"
// 是个真实问题，而用户手上不一定想得起来自己这台的内网 IP。
//
// 探测不到内网 IP 时返回空（**不是**编一个出来）：宁可少给一行，
// 也不要给一个错的地址。
func extraOrigins(listen string) []string {
	host, port := splitHostPort(listen)
	// 通配绑定时：把内网 IP 列出来（"从别的机器怎么打开"是真实问题）。
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::":
		// 继续往下，列内网 IP。
	default:
		// 显式绑了某个地址：回环就不给额外地址（本机地址已经给了），
		// 非回环就把它自己作为额外地址给出去（用户可能填了内网 IP）。
		if isLoopbackHost(host) {
			return nil
		}
		return []string{"http://" + net.JoinHostPort(strings.Trim(host, "[]"), port)}
	}
	out := make([]string, 0, 4)
	for _, ip := range localIPv4s() {
		out = append(out, "http://"+net.JoinHostPort(ip, port))
	}
	return out
}

// splitHostPort 拆 host:port，缺 host 时返回空 host。
//
// `net.SplitHostPort` 对 `:7863` 返回 ("", "7863") ✓，
// 但对不带端口的裸串会报错 —— 那种形态回落到"整体当 host、端口空"，
// 让调用方拼出一个没有端口的地址（而不是 panic 或空串）。
func splitHostPort(listen string) (host, port string) {
	if h, p, err := net.SplitHostPort(listen); err == nil {
		return h, p
	}
	return listen, ""
}

// isLoopbackHost 判断是否回环地址（含 `localhost`）。
func isLoopbackHost(host string) bool {
	h := strings.Trim(strings.TrimSpace(host), "[]")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// listensLoopback 报告 listen 是否**只**绑定回环地址。
//
// # 它是「本机免令牌」的前提之一（安全相关，不是工具函数）
//
// 管理台门禁对"本机直连"免令牌（用户要求：本机不该手填）。但这个放宽
// 只有在**进程根本没对外监听**时才是安全的 —— 否则任何人都能连上那个
// 端口，而请求的来源地址并不保证是回环。
//
//	127.0.0.1:7863 / localhost:7863 / [::1]:7863  → true（只本机可达）
//	:7863 / 0.0.0.0:7863 / 192.168.0.5:7863       → false（对外可达）
//
// ⚠ 空 host（`:7863`）与 `0.0.0.0` 都是"监听全部网卡"，**不是**回环。
// 这是最容易写错的一条：`net.SplitHostPort(":7863")` 给出空串 host，
// 而空串在 net.Listen 里等于所有网卡 —— 若按"空 = 本机"处理，
// 就会把一个对外暴露的部署当成只本机可达。
//
// ⚠ 与 isLoopbackHost 的区别：那个回答"这个 host 是不是回环地址"，
// 这个回答"这个 listen 配置会不会对外敞开"。空 host 在前者返回 false
// （解析不出 IP），在这里也必须返回 false —— 两者判据不同，不能互相替代。
func listensLoopback(listen string) bool {
	host, _ := splitHostPort(strings.TrimSpace(listen))
	// 通配（空 / 0.0.0.0 / ::）一律不算回环。
	if h := strings.Trim(strings.TrimSpace(host), "[]"); h == "" || h == "0.0.0.0" || h == "::" {
		return false
	}
	return isLoopbackHost(host)
}

// localIPv4s 列出本机的私有 IPv4（局域网地址），按稳定性排序。
//
// 只取私有网段（RFC1918）与链路本地之外的回环排除 —— 公网 IPv4 与
// 点对点地址对"从另一台机器打开控制台"没有意义，列出来只会干扰。
func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	return out
}
