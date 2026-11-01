// netretry.go 传输层：出口描述、瞬时错误判定、以及**只对幂等请求**的一次重试。
//
// # 它解决的问题（2026-11-01 用户实测）
//
// 用户看到的历史记录原文：
//
//	签到 002d3463: fail · Get "https://monkeycode-ai.net/api/v1/users/wallet/checkin":
//	net/http: TLS handshake timeout
//
// 这条消息里**没有任何线索指向真正的原因**：国际站配了代理时（见 client.go 的 Client 注释，
// 国际站默认直连、只有本机直连不通才配代理），而代理挂掉时的表现恰好是
// 「TCP 连得上、TLS 握手没回音」 ⇒ Go 报 TLS handshake timeout —— 与「上游自己慢」长得一模一样。
//
// client.go 里那段注释早就写过同一件事：
//
//	「全局配了一个 proxy，国内站请求直接 EOF，表现为『账号不健康』——
//	  错误信息完全指不到代理上。」
//
// 所以这里补两件事：
//
//	① 错误里带上出口（直连 / 代理 <地址>）+ 一句可操作的话；
//	② 对**幂等**请求（GET）做一次退避重试 —— 代理抖动是常态，而签到/额度
//	   这类每日动作撞上一次抖动就被记成 fail，用户看到的是"今天失败"，
//	   下一次扫描要等一小时。
//
// # 为什么不重试 POST
//
// 领取类请求（签到 POST）重复提交在上游是错误码，而且会污染历史
// （checkin.go:54 那段「已签必须早退」就是为这个）。GET 重复提交没有副作用，
// 所以重试范围严格限定在 GET —— 宁可让一次 POST 失败可见，也不要假装成功。
package monkeycode

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"syscall"
	"time"
)

const (
	// getAttempts GET 的总尝试次数（1 次 + 1 次重试）。
	//
	// 为什么是 2 而不是 3：单次 TLS 握手超时就有 20s（见 newTransport），
	// 而「全部签到」是**串行**的。3 次尝试意味着一个死代理下每个账号要等 60s，
	// 26 个账号要等半小时 —— 把"失败"变成"卡住"，比失败更糟。
	getAttempts = 2

	// getRetryBaseMS 第一次重试的退避基数（含抖动，见 retryDelay）。
	getRetryBaseMS = 500

	// retryCostCeiling 只重试**便宜**的失败。
	//
	// # 为什么需要这条（实测 2026-11-01）
	//
	// 代理不通时每一次尝试都要等满 TLS 握手超时（20s，见 newTransport）。
	// 重试一次 ⇒ 每个账号 40s，而「全部签到」是串行的：26 个账号 ≈ 18 分钟，
	// 已经超过跨上游全量签到的 15 分钟上限 —— 用户看到的是"任务被掐断"，
	// 比"签到失败"更糟。
	//
	// 而重试真正能救的是**快失败**（连接被重置 / RST / 瞬间 EOF）：那种情况
	// 第二次往往就成了。所以判据是"已经等了多久"，不是"错在哪一类"。
	retryCostCeiling = 5 * time.Second
)

// transientNetErr 判断传输层错误是否**值得再试一次**。
//
// 判据用"错在哪一层"而不是错误串匹配优先：net.Error.Timeout() 覆盖 TLS 握手
// 超时 / 拨号超时 / 读超时（Go 对它们的分类就在这里），比字符串稳。
func transientNetErr(err error) bool {
	if err == nil {
		return false
	}
	// 调用方主动取消 / 上下文到点：重试毫无意义（只会白等一次退避）。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	// 结构化判据优先于文案：连接被拒 / 被重置 / 管道断开都是可重试的，
	// 而它们的**文案随平台变**（Windows 是 "actively refused"，Linux 是
	// "connection refused"）—— 靠字符串匹配会在另一个平台上静默失效。
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	// 兜底：被包在 url.Error 里的具体串（EOF / HTTP2 这些没有哨兵可判）。
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"tls handshake timeout", "i/o timeout", "connection reset",
		"broken pipe", "unexpected eof", " eof",
		"connection refused", "actively refused", "no connection could be made",
		"forcibly closed", "no such host", "http2: ",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// retryDelay 第 n 次重试（n 从 1 起）的退避：500ms 起、带抖动。
//
// 抖动是必要的：批量签到是串行循环，同一秒里几十个账号同时重试会把出口
// 打出一次尖峰 —— 而出口刚从抖动里恢复，尖峰会让它再抖一次。
func retryDelay(attempt int) time.Duration {
	base := time.Duration(getRetryBaseMS*(1<<uint(attempt-1))) * time.Millisecond
	jitter := time.Duration(rand.Int63n(int64(base/2 + 1)))
	return base + jitter
}

// egress 描述这条凭证的出口（写进错误，让"指不到代理上"不再发生）。
func (c *Client) egress(a *Auth) string {
	if a != nil && a.RegionOrDefault() == RegionIntl {
		if p := strings.TrimSpace(c.Proxy); p != "" {
			return "国际站出口（代理 " + p + "）"
		}
		return "国际站出口（直连）"
	}
	if p := strings.TrimSpace(c.DirectProxy); p != "" {
		return "国内站出口（代理 " + p + "）"
	}
	return "国内站出口（直连）"
}

// wrapNetErr 给传输层错误补上出口信息与一句可操作的话。
//
// ⚠ 必须保留 %w：调用方（dailycheckin 的 driver）要用 errors.Is 区分
// 取消 / 超时 / 其它；换成 %v 会让那层判断静默失效。
func (c *Client) wrapNetErr(a *Auth, method, rawURL string, err error) error {
	if err == nil {
		return nil
	}
	hint := ""
	if transientNetErr(err) {
		hint = "（瞬时网络故障，已重试仍失败）"
		if a != nil && a.RegionOrDefault() == RegionIntl && strings.TrimSpace(c.Proxy) != "" {
			// 这是最常见的一种：代理进程在、CONNECT 也通，但出不了国。
			hint = "（代理节点不可用？先在「设置」里确认代理端口，或换一个出口；国际站多数环境**可直连**，把代理留空也是一条可试的路）"
		}
	}
	host := rawURL
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return fmt.Errorf("monkeycode: %s %s 失败 —— %s%s：%w", method, host, c.egress(a), hint, err)
}
