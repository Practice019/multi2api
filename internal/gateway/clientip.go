// clientip.go 客户端 IP 的提取与跨层传递。
//
// # 为什么放在 gateway
//
// 两件事各自需要它，而它们唯一的共同依赖就是本包：
//
//	core（internal/server）   在入站请求上提取 IP 并放进 ctx
//	上游（internal/upstream） 在出站请求上注入 IP 头
//
// core **不得** import 具体上游（架构判据 3），上游也拿不到 HTTP 请求对象。
// ctx 是 `gateway.Provider.Chat(ctx, cred, body)` 这条固定四参契约里
// 唯一可用的传递通道 —— 加第五个参数会动所有上游，那是判据 1 要避免的。
//
// # 为什么不用 Client 上的字段
//
// 那是一个共享可变态：A 请求写、B 请求读，B 的 IP 会被 A 覆盖。
// 表现是上游风控看到一批来源错乱的请求，且**只在并发下出现**（最难查的一类）。
// 按请求传递是唯一正确的形态。
//
// # 与上游无关
//
// 本文件不含任何上游专有知识：XFF / X-Real-IP 是通用 HTTP 头，
// 提取规则由 RFC 7239 与通行代理约定决定。
package gateway

import (
	"context"
	"net/http"
	"strings"
)

// clientIPKey ctx 键（未导出的空结构体，杜绝与其他包的键碰撞）。
type clientIPKey struct{}

// WithClientIP 把客户端 IP 放进 ctx。
//
// ip 为空时**原样返回 ctx**：不塞一个空值键，
// 让下游用 `ClientIPFrom(ctx) == ""` 就能区分"没传"与"传了空"。
func WithClientIP(ctx context.Context, ip string) context.Context {
	if ctx == nil || ip == "" {
		return ctx
	}
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFrom 取回客户端 IP；没有则返回空串。
//
// nil ctx 安全返回空串：Provider 实现可能收到 nil ctx（测试直接构造调用），
// 一个 panic 会让整条出站路径崩掉。
func ClientIPFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(clientIPKey{}).(string)
	return s
}

// ExtractClientIP 从入站请求提取客户端 IP（X-Forwarded-For 首段，回落 X-Real-IP）。
//
// # 为什么不看 RemoteAddr
//
// 网关通常跑在反向代理之后，RemoteAddr 是**代理**的地址，不是客户端。
//
// # 为什么只取 XFF 首段
//
// XFF 是可被客户端伪造的头部，且形如 `client, proxy1, proxy2`。
// 首段才是最初的客户端；把多段原样透传等于把伪造链一起带到上游，
// 而上游通常只信任首段 —— 发全链反而可能被判为异常。
//
// # ⚠ 本函数只做提取，**不做可信性判断**
//
// 是否信任 XFF 完全取决于部署链路（直连公网时它一文不值）。
// 因此透传是配置项（upstream.passthrough_ip）且**默认关闭**，
// 由部署者判断自己的链路是否可信 —— 本包不替它做这个决定。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}
