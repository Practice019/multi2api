// callback.go TRAE OAuth 回调的**本机自动接收器**。
//
// # 为什么需要它（用户要求：登录对齐其它上游，无手动复制粘贴）
//
// TRAE 的 OAuth 回调地址固定在 `http://127.0.0.1:<port>/authorize`
// （上游登录页写死的 auth_callback_url）。控制台与浏览器同机时，
// 登录完成后浏览器跳回 127.0.0.1 —— 这个跳转**就是发给网关自己的**：
// 网关在 18080（可配 trae.oauth_callback_port）起一个监听，收到回调
// 立即解析 → ExchangeToken → GetUserInfo → 标记会话就绪。用户全程
// 只做一件事：点开登录链接、登录、回到控制台（自动完成添加）。
//
// 与 trae-api-proxy 的做法一致：回调服务器在网关进程内，浏览器必须
// 与控制台同机（127.0.0.1）。这是 TRAE 协议的回调形态决定的。
//
// # 多会话同时进行时怎么对应（启发式）
//
// 回调 query 里没有我们的 state，只有凭证参数；一次登录从 Start 到
// 回调通常只有一次在途。这里取「最新未就绪」的会话（15 分钟内）——
// 两个登录同时进行时，后完成者覆盖前一个，可接受（注释见文件头）。
package trae

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// callbackPort 默认回调端口（与 traework2api login.sh / trae-api-proxy 一致）。
const callbackPort = "18080"

// ensureCallbackServer 惰性启动本机回调监听器（每个 Provider 一次）。
//
// 监听失败（端口被占）时记下错误并返回 —— Start() 会把它报给前端，
// 而不是让"点了添加账号却静默收不到回调"。
func (p *Provider) ensureCallbackServer() error {
	p.cbOnce.Do(func() {
		port := p.callbackPort
		if port == "" {
			port = callbackPort
		}
		addr := net.JoinHostPort("127.0.0.1", port)
		mux := http.NewServeMux()
		mux.HandleFunc("/authorize", p.handleAuthorize)
		p.cbServer = &http.Server{Addr: addr, Handler: mux}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			p.cbErr = fmt.Errorf("trae: 回调监听 %s 失败（端口被占？）: %w", addr, err)
			log.Printf("%v", p.cbErr)
			return
		}
		log.Printf("trae: 回调监听已就绪 %s/authorize（OAuth 登录后自动接收，无需手动粘贴）", addr)
		go func() { _ = p.cbServer.Serve(ln) }()
	})
	return p.cbErr
}

// handleAuthorize 处理 OAuth 回调：捕获完整 URL → 交给最新在途会话。
func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	callback := "http://" + r.Host + r.RequestURI
	f, ok := p.LoginFlow()
	if !ok {
		writeCallbackHTML(w, "登录流程未初始化，请回到控制台重新点「添加账号」")
		return
	}
	entry := f.(*loginFlow).newestPending()
	if entry == nil {
		writeCallbackHTML(w, "没有进行中的登录会话 —— 请回到控制台重新点「添加账号」")
		return
	}
	// 标记处理中（防同一次回调被重复消费），然后异步换 token。
	// 响应先回"登录成功"页 —— ExchangeToken 可能要一两秒，没必要让浏览器等。
	if !f.(*loginFlow).markProcessing(entry) {
		writeCallbackHTML(w, "这个登录会话已在处理中，请稍候回到控制台查看。")
		return
	}
	go func() {
		if _, err := f.(*loginFlow).buildAuth(entry, callback); err != nil {
			log.Printf("trae: 回调处理失败（会话 %s）: %v", shortUID(entry.machineID), err)
		}
	}()
	writeCallbackHTML(w, "登录成功，可以关闭本页并回到控制台，账号会自动添加完成。")
}

// writeCallbackHTML 回一页简单的成功/失败提示。
func writeCallbackHTML(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("<!DOCTYPE html><html lang=\"zh-CN\"><meta charset=\"utf-8\">" +
		"<title>Trae 登录</title><body style=\"font:14px/1.6 system-ui,sans-serif;padding:32px\">" +
		"<h2>TRAE 登录</h2><p>" + msg + "</p></body></html>"))
}

// newestPending 找最新未就绪的会话（回调对应启发式，见文件头）。
func (f *loginFlow) newestPending() *loginEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gcLocked()
	var (
		target *loginEntry
		newest time.Time
	)
	for _, e := range f.sessions {
		if e == nil || e.ready {
			continue
		}
		if e.at.After(newest) {
			target = e
			newest = e.at
		}
	}
	return target
}

// markProcessing 把会话标记为处理中；已在处理/已就绪返回 false。
func (f *loginFlow) markProcessing(e *loginEntry) bool {
	if e == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.ready || e.processing {
		return false
	}
	e.processing = true
	return true
}
