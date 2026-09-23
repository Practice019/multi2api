// callback.go MiMo OAuth 回调的本机接收器（127.0.0.1:18081）。
//
// 与 trae 的 18080 分开固定端口：同机部署两个上游登录不互踩。
// 官方回跳形态：GET /?u=<base64url 密文>（redirect_uri 由我们构造，可控）。
// 收到后异步解密并标记会话就绪，浏览器 302 回平台的 status 页（官方 UX）。
package mimo

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

type callbackServer struct {
	srv  *http.Server
	addr string
}

// ensureCallbackServer 惰性启动回调监听（每个 Provider 一次）。
func (p *Provider) ensureCallbackServer() error {
	p.cbOnce.Do(func() {
		addr := net.JoinHostPort("127.0.0.1", p.callbackPort)
		mux := http.NewServeMux()
		mux.HandleFunc("/", p.handleCallback)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			p.cbErr = fmt.Errorf("mimo: 回调监听 %s 失败（端口被占？可配 mimo.oauth_callback_port）: %w", addr, err)
			log.Printf("%v", p.cbErr)
			return
		}
		p.cbServer = &callbackServer{srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}, addr: addr}
		go func() { _ = p.cbServer.srv.Serve(ln) }()
		log.Printf("mimo: 回调监听已就绪 %s/（OAuth 登录后自动接收 u 密文）", addr)
	})
	return p.cbErr
}

// handleCallback GET /?u=...：捕获 → 交给最新在途会话 → 302 平台 status 页。
func (p *Provider) handleCallback(w http.ResponseWriter, r *http.Request) {
	u := r.URL.Query().Get("u")
	if u == "" {
		// 浏览器直接访问（或探针）：给个静态说明页，不是错误。
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!DOCTYPE html><meta charset="utf-8"><title>MiMo 登录回调</title>` +
			`<body style="font:14px/1.6 system-ui;padding:32px">这里是 MiMo 登录回调端口。` +
			`请通过控制台的「＋ 添加账号」发起登录。</body>`))
		return
	}
	f, _ := p.LoginFlow()
	entry := f.(*loginFlow).newestPending()
	if entry == nil {
		writeCallbackPage(w, "没有进行中的登录会话 —— 回控制台重新点「添加账号」")
		return
	}
	if !f.(*loginFlow).markProcessing(entry) {
		writeCallbackPage(w, "这个登录会话已在处理中，请稍候。")
		return
	}
	go func() {
		if err := f.(*loginFlow).completeWithU(entry.state, u); err != nil {
			log.Printf("mimo: 回调处理失败（会话 %s）: %v", shortUID(entry.state), err)
		}
	}()
	writeCallbackPage(w, "登录成功，可关闭本页回控制台，账号会自动添加完成。")
}

func writeCallbackPage(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="zh-CN"><meta charset="utf-8"><title>MiMo 登录</title>` +
		`<body style="font:14px/1.6 system-ui,sans-serif;padding:32px"><h2>MiMo 登录</h2><p>` + msg +
		`</p></body></html>`))
}
