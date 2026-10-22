// cdp.go 极简 CDP 客户端 —— 只做「开浏览器 → 等登录 → 读 cookie」。
//
// # 为什么手写 WebSocket 而不引第三方库
//
// 本仓依赖铁律（AGENTS.md §9）：`go.mod` 直接依赖只有 `go-redis`
// （`wazero` 是 qoder WASM 的唯一破例）。引一个 WebSocket 库只为取几条 cookie，
// 与「Release 下载即用」的目标冲突。
//
// 而 CDP 需要的东西极少：
//
//	① HTTP  GET /json/version   —— 探活
//	② HTTP  GET /json/list      —— 找页面 target
//	③ WS    Storage.getCookies  —— 读 cookie
//
// ① ② 用 net/http。③ 需要 WebSocket，而 RFC6455 的握手与帧格式足够简单，
// 手写 ~120 行即可，且**只用到文本帧**（CDP 的 JSON 都是文本）。
//
// # 为什么不用 --headless
//
// 登录必须让用户看得见并操作（输账号密码、过 2FA、点授权）。
// 唯一例外是「本机已登录」的快捷路径 —— 那条也照样要有头，因为
// 我们要读的是**真实用户 profile** 的 cookie。
package monkeycode

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cdpCookie 一条 cookie（只取我们关心的字段）。
type cdpCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
	Secure bool   `json:"secure"`
}

// cdpVersion GET /json/version 的返回。
type cdpVersion struct {
	Browser string `json:"Browser"`
}

// cdpTarget GET /json/list 的一项。
type cdpTarget struct {
	Type             string `json:"type"`
	URL              string `json:"url"`
	WebSocketDebugger string `json:"webSocketDebuggerUrl"`
}

// cdpBase CDP 的 HTTP 基址。
func cdpBase(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// cdpWaitReady 等浏览器把 CDP 端口开起来。
//
// ⚠ 刚 spawn 的浏览器要 1~3 秒才监听端口，直接请求会 ECONNREFUSED。
// 轮询到超时而不是固定 sleep：机器快时立刻返回，慢时也不会失败。
func cdpWaitReady(ctx context.Context, port int, timeout time.Duration) (*cdpVersion, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cdpBase(port)+"/json/version", nil)
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var v cdpVersion
				if json.Unmarshal(body, &v) == nil {
					return &v, nil
				}
			}
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("超时")
	}
	return nil, fmt.Errorf("CDP 端口 %d 未就绪：%w", port, lastErr)
}

// cdpListTargets 列出所有 target。
func cdpListTargets(ctx context.Context, port int) ([]cdpTarget, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdpBase(port)+"/json/list", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var list []cdpTarget
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// cdpPickTarget 挑一个能执行 Storage.getCookies 的页面 target。
//
// 优先挑 URL 命中 host 的那个（用户正在登录的页），否则挑任意 page。
func cdpPickTarget(list []cdpTarget, wantHost string) *cdpTarget {
	var fallback *cdpTarget
	for i := range list {
		t := &list[i]
		if t.Type != "page" || t.WebSocketDebugger == "" {
			continue
		}
		if wantHost != "" && strings.Contains(t.URL, wantHost) {
			return t
		}
		if fallback == nil {
			fallback = t
		}
	}
	return fallback
}

// cdpReadCookies 通过 WebSocket 调 Storage.getCookies。
//
// # WebSocket 手写要点（RFC6455）
//
//	握手   GET <path> + Upgrade/Connection/Sec-WebSocket-Key(16 字节 base64)/Version:13
//	      服务端回 101 + Sec-WebSocket-Accept（本实现**不校验**它，
//	      因为连的是 127.0.0.1 上自己 spawn 的进程，不是不受信的对端）
//	帧     客户端发的**必须 mask**（RFC 强制），服务端发的**必须不 mask**
//	长度    <126 直接；126 → 后 2 字节大端；127 → 后 8 字节大端
//
// 只处理文本帧（opcode 0x1）与 close/ping。CDP 的响应全是文本 JSON。
func cdpReadCookies(ctx context.Context, wsURL string) ([]cdpCookie, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
	}

	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("连 CDP 失败：%w", err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	}

	// ---- 握手 ----
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	reqLine := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, reqLine); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.Contains(statusLine, "101") {
		return nil, fmt.Errorf("CDP 握手失败：%s", strings.TrimSpace(statusLine))
	}
	// 吃掉剩余头部。
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// ---- 发请求帧 ----
	payload, err := json.Marshal(map[string]any{
		"id":     1,
		"method": "Storage.getCookies",
		"params": map[string]any{},
	})
	if err != nil {
		return nil, err
	}
	if err := wsWriteText(conn, payload); err != nil {
		return nil, err
	}

	// ---- 读响应帧 ----
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opcode, data, err := wsReadFrame(br)
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x1: // 文本
			var resp struct {
				ID     int    `json:"id"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
				Result struct {
					Cookies []cdpCookie `json:"cookies"`
				} `json:"result"`
			}
			if err := json.Unmarshal(data, &resp); err != nil {
				continue // 事件消息，跳过
			}
			if resp.ID != 1 {
				continue
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("Storage.getCookies 失败：%s", resp.Error.Message)
			}
			return resp.Result.Cookies, nil
		case 0x8: // close
			return nil, fmt.Errorf("CDP 提前关闭连接")
		case 0x9: // ping → 回 pong
			_ = wsWriteFrame(conn, 0xA, data)
		}
	}
	return nil, fmt.Errorf("读 cookie 超时")
}

// wsWriteText 发一个文本帧（客户端必须 mask）。
func wsWriteText(conn net.Conn, payload []byte) error {
	return wsWriteFrame(conn, 0x1, payload)
}

// wsWriteFrame 发一个帧（opcode 0x1 文本 / 0xA pong 等）。
func wsWriteFrame(conn net.Conn, opcode byte, payload []byte) error {
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	var hdr []byte
	n := len(payload)
	switch {
	case n < 126:
		hdr = []byte{0x80 | opcode, 0x80 | byte(n)}
	case n < 65536:
		hdr = make([]byte, 4)
		hdr[0] = 0x80 | opcode
		hdr[1] = 0x80 | 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(n))
	default:
		hdr = make([]byte, 10)
		hdr[0] = 0x80 | opcode
		hdr[1] = 0x80 | 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := conn.Write(append(append(hdr, mask...), masked...))
	return err
}

// wsReadFrame 读一个帧（服务端的帧**不带 mask**）。
func wsReadFrame(br *bufio.Reader) (opcode byte, payload []byte, err error) {
	h := make([]byte, 2)
	if _, err = io.ReadFull(br, h); err != nil {
		return 0, nil, err
	}
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	length := int(h[1] & 0x7f)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(br, ext); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(br, ext); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext))
	}
	var maskKey []byte
	if masked {
		maskKey = make([]byte, 4)
		if _, err = io.ReadFull(br, maskKey); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload, nil
}

// wsAcceptFor 算 Sec-WebSocket-Accept（本实现不校验，留作自检与测试用）。
func wsAcceptFor(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}
