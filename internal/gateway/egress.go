// egress.go 「出口」的共享契约：上游**从哪出去**、能不能出去、出不去时怎么读错误。
//
// # 为什么单独立一层（2026-11-01 的实测教训）
//
// 用户看到的历史记录原文：
//
//	签到 002d3463: fail · Get https://monkeycode-ai.net/api/v1/users/wallet/checkin：
//	net/http: TLS handshake timeout
//
// 这条消息**指不到真正的原因**：monkeycode 的国际站按设计必须走代理，而代理挂掉时
// 的表现恰好是「TCP 连得上、TLS 握手没回音」⇒ Go 报 TLS handshake timeout，
// 与「上游自己慢」长得一模一样。用户于是去查账号、查上游公告，而问题在他自己的代理。
//
// monkeycode 那边已经把出口写进错误文案了（internal/monkeycode/netretry.go），
// 但那是**一个上游**的私有实现。这一层做两件事，让「出口」变成全仓通用的事实：
//
//	① EgressExt      上游**自报**它的出口（纯描述、不发网络 ⇒ 可以进 manifest，
//	                 打开账号池就能看见「这个上游的请求从哪出去」）
//	② EgressCheckExt 上游**实测**出口可达性（发一次最小请求，结果带类别 + 人话）
//
// 以及一个共享的错误分类器 ClassifyNetErr —— 它是「可诊断」三个字的落点：
// 把 net.Error 翻成「DNS 解析失败 / 连接被拒 / TLS 握手超时（多为代理没起来）/ 证书」
// 这类**能指导动作**的类别，而不是把 err.Error() 原样抛给用户。
package gateway

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"syscall"
)

// EgressInfo 上游自报的出口描述。
//
// ⚠ 纯数据：**不许**在这里发网络请求 —— 它每次打开账号池都会随 manifest 下发。
type EgressInfo struct {
	// Direct 该上游是否有直连出口（大多数上游是：它们只有一个域名）。
	Direct bool `json:"direct"`
	// Proxies 用到的代理地址（HTTP 代理形态，如 http://127.0.0.1:10808）。
	// 空 = 这个上游不用代理。
	Proxies []string `json:"proxies,omitempty"`
	// Note 一句人话（为什么这样配、哪个域名必须走哪条路）。
	// 没有可说的就不写 —— 界面上不留一行空话。
	Note string `json:"note,omitempty"`
}

// EgressExt 上游自报出口（可选实现）。
//
// 判据同其余扩展点：**类型断言**，不认上游名。没实现 ⇒ manifest 里不带这个字段
// （既有部署的界面逐字节不变）。
type EgressExt interface {
	Egress() EgressInfo
}

// EgressProbe 一次出口实测的结果。
type EgressProbe struct {
	// Name 给人看的名字，如「国际站 · 走代理」。
	Name string `json:"name"`
	// Target 实测的目标（URL 或 host:port），排障时要能抄进 curl。
	Target string `json:"target"`
	// Via 这一条走的是哪条路（「直连」/「代理 http://…」）。
	Via string `json:"via"`
	// OK 出口通不通。
	OK bool `json:"ok"`
	// Kind 失败类别（ClassifyNetErr 的输出；成功时为空）。
	Kind string `json:"kind,omitempty"`
	// Hint 一句可操作的话（成功时为空）。
	Hint string `json:"hint,omitempty"`
	// Detail 原始错误文本（截断），留给排障。
	Detail string `json:"detail,omitempty"`
	// MS 耗时毫秒。
	MS int64 `json:"ms"`
}

// EgressCheckExt 上游能**实测**出口可达性（可选实现）。
//
// 约定（这条约定是整套自检的价值所在）：
//
//	判据是「**能不能拿到 HTTP 响应**」，不是「凭证对不对」。
//	所以用一次**不带凭证**的最小请求：401/403 也算**通**（DNS+TCP+TLS+HTTP 全通了），
//	只有连不上 / 握手失败 / 超时才判不通。
//
// 理由：出口问题与凭证问题必须能分开。把 401 记成「出口不通」会让用户去修代理；
// 反之把超时记成「凭证失效」会让他去重新登录。两类动作完全不同。
type EgressCheckExt interface {
	EgressCheck(ctx context.Context) []EgressProbe
}

// 失败类别（Kind 的取值）。用**字符串**而不是枚举：它要进 JSON、进日志、进界面文案。
const (
	EgressKindDNS     = "dns"     // 域名解析失败
	EgressKindRefused = "refused" // TCP 连接被拒（端口没人听 = 代理没起来）
	EgressKindTLS     = "tls"     // TLS 握手失败/超时（代理挂掉最常见的样子）
	EgressKindCert    = "cert"    // 证书校验失败（中间人/自签）
	EgressKindTimeout = "timeout" // 整体超时
	EgressKindNet     = "net"     // 其它网络错误（兜底，不假装知道原因）
)

// refusedErr 「连接被拒」的完整判据：平台 errno + 文本兜底。
//
// ⚠ 文本那一条**不是**冗余：包装层（url.Error / 上游 SDK）会把 errno 链切断，
// 而「actively refused」这句原话是各平台共有的（Windows connectex、Unix ECONNREFUSED
// 的文案都含 connection refused）。实测先例见 egress_errno_windows.go。
func refusedErr(err error) bool {
	if connRefused(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused")
}

// ClassifyNetErr 把网络错误翻成「类别 + 一句可操作的话」。
//
// # 为什么必须有它
//
// 同一句 TLS handshake timeout 至少有三种成因（代理没起来 / 上游挂了 / 中间设备拦了），
// 而它们要用户做的事完全不同。分类器不是把错误美化，而是把**动作**指出来。
//
// ⚠ 判据顺序有讲究：具体错误类型优先于 net.Error 的超时判定 ——
// 否则 DNSError / OpError 会被 err.Timeout() 先吃掉，全都归成「超时」。
func ClassifyNetErr(err error) (kind string, hint string) {
	if err == nil {
		return "", ""
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return EgressKindDNS, "域名解析不了：查 DNS / 本机 hosts，或这条链路用的出口是否连 DNS 都过不去"
	}
	// ⚠⚠ 这里必须是**值类型**目标（不是 `*T`）：x509 这几个错误的 Error() 挂在值接收者上，
	// 而 Go 的 http/tls 链把它们按**值**包进 url.Error ⇒ 用 `var e *x509.UnknownAuthorityError`
	// 做 errors.As 目标**匹配不到**（可赋值性要求的是指针类型）——
	// TestClassifyNetErr 的「证书不受信」那条用例就是这么抓到这个洞的。
	// 指针形态也一并认（不同版本/不同链路可能给指针）。
	var certErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var certPtr *x509.UnknownAuthorityError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &certInvalid) || errors.As(err, &certPtr) {
		return EgressKindCert, "证书校验失败：中间的证书代理没装可信根，或出口被劫持"
	}
	// 连接被拒：按平台认 errno（Windows 是 WSAECONNREFUSED，见 egress_errno_*.go），
	// 再加一层文本兜底 —— 有些包装会把它变成普通 error，errno 认不到但原话还在。
	if refusedErr(err) {
		return EgressKindRefused, "对方端口没有服务在听：本地代理没起来？（代理挂了最常见的就是这条）"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// ⚠ 这里刻意**不**说「上游慢」：TLS 握手超时在代理场景里更常见。
		return EgressKindTimeout, "超时：先确认这条链路用的代理 / 出口是通的（比「上游慢」更常见）"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "tls") || strings.Contains(msg, "handshake"):
		return EgressKindTLS, "TLS 握手失败：多为代理 / 出口不通（TCP 通了但没回音），先测出口再怀疑上游"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return EgressKindTLS, "连接被对端重置：代理 / 出口中断（换节点，或关掉代理试直连）"
	case strings.Contains(msg, "proxyconnect") || strings.Contains(msg, "proxy"):
		return EgressKindRefused, "代理连接失败：核对代理地址与端口，并确认代理进程在跑"
	}
	return EgressKindNet, "网络错误：看下面的原文，并用「出口自检」逐条测出口"
}
