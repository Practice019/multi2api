// egress.go monkeycode 的**出口自检**：逐条实测「哪条路通、哪条不通、不通是哪一类」。
//
// # 为什么这个上游最先需要它
//
// monkeycode 是本仓唯一**两站网络前提相反**的上游（见 client.go 的 Client 注释）：
//
//	国内站 (monkeycode-ai.com)  必须直连（走海外代理既慢又触发风控）
//	国际站 (monkeycode-ai.net)  必须走代理（否则连不上）
//
// 所以它有三种「出口姿势」，而任何一种配错，用户看到的都是同一句话：
//
//	签到 xxx: fail · Get …: net/http: TLS handshake timeout
//
// 这条消息既可能是代理没起来、也可能是上游挂了、也可能是把国内站也塞进了代理。
// netretry.go 已经让**错误文案**带上出口；本文件让用户能**主动测一次**，
// 并且把结果分成「通 / 不通 + 类别 + 该做什么」。
package monkeycode

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：本上游自报出口、并能实测（判据是类型断言，前端/核心都不认上游名）。
var (
	_ gateway.EgressExt      = (*Provider)(nil)
	_ gateway.EgressCheckExt = (*Provider)(nil)
)

// egressProbeTimeout 单条出口探测的超时。
//
// # 为什么是 8s，而不是沿用客户端的 20s（TLSHandshakeTimeout）
//
// 本函数是**诊断**：用户点完按钮在等结果。死代理下每条都要等满 20s，
// 三条就是 60s —— 诊断比故障本身还慢，用户会以为界面卡死。
// 8s 足够把「通」与「不通」分开，而「不通」本来就只需要一个类别。
const egressProbeTimeout = 8 * time.Second

// egressProbePath 探测用的端点。
//
// ⚠ 刻意选一个**需要登录**的端点，并且**不带**任何凭证：
// 401/403 也算「出口通」（DNS + TCP + TLS + HTTP 全通了），
// 只有连不上 / 握手失败 / 超时才判不通 —— 判据见 gateway.EgressCheckExt 的约定。
// 这样「出口问题」与「凭证问题」在结果里能被分开：前者让用户去修代理，
// 后者让他去重新登录，两类动作完全不同。
const egressProbePath = "/api/v1/users/wallet"

// Egress 自报出口（**纯描述、不发网络** —— 它随 manifest 每次打开都下发）。
func (p *Provider) Egress() gateway.EgressInfo {
	info := gateway.EgressInfo{
		Direct: true,
		Note:   "国内站必须直连；国际站必须走代理（两站的网络前提相反）",
	}
	if p != nil && p.client != nil {
		if v := strings.TrimSpace(p.client.Proxy); v != "" {
			info.Proxies = append(info.Proxies, v)
		}
		if v := strings.TrimSpace(p.client.DirectProxy); v != "" {
			info.Proxies = append(info.Proxies, v)
			info.Note = "国内站被强制走代理（DirectProxy 只在「本机无法直连国内站」时才该设）；国际站走代理"
		}
	}
	return info
}

// EgressCheck 逐条实测出口，返回每一条的结果（含失败类别与人话）。
//
// 三条的顺序刻意是「设计上必须通的 → 设计上通常不通的 → 真正在用的那条」：
// 用户读完前两条就明白「两站前提相反」这件事，第三条才是他这次要修的。
func (p *Provider) EgressCheck(ctx context.Context) []gateway.EgressProbe {
	if p == nil || p.client == nil {
		return nil
	}
	out := make([]gateway.EgressProbe, 0, 3)
	// ① 国内站 · 直连（按设计必须通）
	out = append(out, probeEgress(ctx, "国内站 · 直连", hostCN, p.client.direct, "直连"))
	// ② 国际站 · 直连（按设计通常不通 —— 如实报，别替用户藏起来）
	out = append(out, probeEgress(ctx, "国际站 · 直连", hostIntl, p.client.direct, "直连"))
	// ③ 国际站 · 走代理（这才是国际站真正在用的那条路）
	if proxy := strings.TrimSpace(p.client.Proxy); proxy != "" {
		out = append(out, probeEgress(ctx, "国际站 · 走代理", hostIntl, p.client.proxied, "代理 "+proxy))
	} else {
		// 没配代理是**静态可知**的结论，不必发网络：直接给出那一条失败 + 该做什么。
		out = append(out, proxylessIntlProbe())
	}
	return out
}

// proxylessIntlProbe 没配代理时的「国际站 · 走代理」条目。
//
// # 为什么单独抽出来
//
// 它是一条**纯判断**（没有代理 ⇒ 国际站一定连不上），抽出来才能被测试钉住，
// 而不必为了测它去发真网络请求（那会让单测变成联网测试）。
func proxylessIntlProbe() gateway.EgressProbe {
	return gateway.EgressProbe{
		Name:   "国际站 · 走代理",
		Target: hostIntl + egressProbePath,
		Via:    "代理（未配置）",
		Kind:   gateway.EgressKindRefused,
		Hint:   "国际站按设计必须走代理，但没配代理地址 ⇒ 它一定连不上。在「设置」里配 monkeycode 的代理，或改用国内站账号",
	}
}

// probeEgress 对一条出口发一次最小 GET 并分类结果。
//
// ⚠ 用**调用方给的 http.Client**（而不是临时造一个）：客户端里带着真实的
// 代理配置与 TLS 设置 —— 自己造一个 transport 测出来的是「另一条路」，
// 那正是这种自检最容易骗人的地方。
func probeEgress(ctx context.Context, name, host string, c *http.Client, via string) gateway.EgressProbe {
	probe := gateway.EgressProbe{Name: name, Target: host + egressProbePath, Via: via}
	if c == nil {
		probe.Kind, probe.Hint = gateway.EgressKindNet, "这条出口没有配好客户端（内部接线问题）"
		return probe
	}
	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, egressProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, probe.Target, nil)
	if err != nil {
		probe.MS = time.Since(start).Milliseconds()
		probe.Kind, probe.Hint = gateway.EgressKindNet, "构造请求失败（内部接线问题）"
		probe.Detail = truncateErrText(err.Error())
		return probe
	}
	req.Header.Set("Accept", "application/json")
	if p := c; p != nil {
		req.Header.Set("User-Agent", defaultUserAgent)
	}
	resp, err := c.Do(req)
	probe.MS = time.Since(start).Milliseconds()
	if err != nil {
		probe.Kind, probe.Hint = gateway.ClassifyNetErr(err)
		probe.Detail = truncateErrText(err.Error())
		return probe
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	// 拿到任何 HTTP 响应 = 出口通（含 401/403/404）。
	probe.OK = true
	return probe
}

// truncateErrText 截断错误原文（界面与日志都不该被一坨堆栈撑爆）。
func truncateErrText(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
