// egress.go monkeycode 的**出口自检**：逐条实测「哪条路通、哪条不通、不通是哪一类」。
//
// # 为什么这个上游最先需要它
//
// monkeycode 是本仓唯一**两站网络前提不同**的上游（见 client.go 的 Client 注释）：
//
//	国内站 (monkeycode-ai.com)  必须直连（走海外代理既慢又触发风控）
//	国际站 (monkeycode-ai.net)  **直连或走代理均可** —— 代理只在本机直连不通时才需要
//
// ⚠ 2026-11-01 更正：「国际站必须走代理（否则连不上）」是**错误前提**。
// 用户实测（零代理直连）三发全通：GET /users/status → 401（业务响应，说明连得上）、
// POST /public/captcha/challenge → 201、GET / → 200。
// 多数环境直连即可；把代理写成"设计上必须"，会让没有代理的用户以为功能不可用。
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
		Note:   "国内站必须直连；国际站**直连或走代理均可**（代理只在本机直连不通时才需要）",
	}
	if p != nil && p.client != nil {
		if v := strings.TrimSpace(p.client.Proxy); v != "" {
			info.Proxies = append(info.Proxies, v)
		}
		if v := strings.TrimSpace(p.client.DirectProxy); v != "" {
			info.Proxies = append(info.Proxies, v)
			info.Note = "国内站被强制走代理（DirectProxy 只在「本机无法直连国内站」时才该设）；国际站直连或走代理均可"
		}
	}
	return info
}

// EgressCheck 逐条实测出口，返回每一条的结果（含失败类别与人话）。
//
// 三条的顺序刻意是「设计上必须通的 → 国际站的常态路径（直连）→ 配了代理才有的那条路」：
// 用户读完前两条就明白「国内站直连、国际站直连也行」，第三条是「本机直连不通时的备选」。
func (p *Provider) EgressCheck(ctx context.Context) []gateway.EgressProbe {
	if p == nil || p.client == nil {
		return nil
	}
	out := make([]gateway.EgressProbe, 0, 3)
	// ① 国内站 · 直连（按设计必须通）
	out = append(out, probeEgress(ctx, "国内站 · 直连", hostCN, p.client.direct, "直连"))
	// ② 国际站 · 直连（**这是常态路径**，实测多数环境直连即可；不通时才需要配代理）
	out = append(out, probeEgress(ctx, "国际站 · 直连", hostIntl, p.client.direct, "直连"))
	// ③ 国际站 · 走代理（只在"本机直连不通"时的备选）
	if proxy := strings.TrimSpace(p.client.Proxy); proxy != "" {
		out = append(out, probeEgress(ctx, "国际站 · 走代理", hostIntl, p.client.proxied, "代理 "+proxy))
	} else {
		// 没配代理**不等于连不上** —— 这一步不发网络、也不断言失败：
		// 真正的判据是上面第②条（国际站 · 直连）的实测结果。
		out = append(out, proxylessIntlProbe())
	}
	return out
}

// proxylessIntlProbe 没配代理时的「国际站 · 走代理」条目。
//
// # 为什么单独抽出来
//
// 它是一条**说明性**条目（未配置代理 ⇒ 这一条无从实测），抽出来才能被测试钉住，
// 而不必为了测它去发真网络请求（那会让单测变成联网测试）。
//
// ⚠ 2026-11-01 语义更正：这里以前是**静态判定**「没代理 ⇒ 国际站一定连不上」
// （直接给 Kind=Refused）。那个前提是错的 —— 国际站直连多数环境可用，
// 实测：零代理下 GET /users/status 401、POST /public/captcha/challenge 201、GET / 200。
// 现在只报「未配置代理（直连）」，把"到底通不通"交给上一条「国际站 · 直连」的实测结果。
//
// Kind 用字面量 "skipped" 而**不**在 gateway 里新增常量：这是本上游对"未实测"的
// 局部表达，不该为它扩共享契约（gateway.EgressKind* 那组是**失败类别**，而这条不是失败）。
//
// ⚠ OK 必须为 true：渲染层（internal/admin/egress_check.go）只按 pr.OK 决定
// "ok / fail"，`OK=false` 会在这条上打出一个**假的失败**（正是本次要消掉的误导）。
// 这条不是"通"，而是"没测" —— 真正的可达性判据是上一条「国际站 · 直连」的实测结果。
func proxylessIntlProbe() gateway.EgressProbe {
	return gateway.EgressProbe{
		Name:   "国际站 · 走代理",
		Target: hostIntl + egressProbePath,
		// Via 会被原样显示在 detail 里，所以把"怎么回事"直接写进它（Hint 在 OK=true 时不被渲染）
		Via:  "代理（未配置）· 国际站多数环境可直连（未实测；看上一条「国际站 · 直连」）",
		Kind: "skipped",
		OK:   true,
		Hint: "未配置代理 —— 国际站多数环境**可直连**（请看上一条「国际站 · 直连」的实测结果）。若直连不通，再到「设置」里配 monkeycode 的代理",
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
