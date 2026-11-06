package codearts

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── T-232：对话期间的 busy 续期（心跳）＋ 首次 busy 失败要注销 ─────────────
//
// 上游的并发闸只认 `chat-session/heartbeat` 的 busy/idle（官方 session.rs:5-8）：
//   ① 续期失败**不能**释放槽位；② 首次 busy 失败也要注销刚造的 id；
//   ③ 只注销自己造的 id；④ idle 恰好一次、且在最后一次 busy 之后。
//
// 本文件测 ①②④（③由"只有一个 id 变量"结构保证：全函数只用 sessionID 那一个值）。

// hbRecorder 记录假上游收到的每一次心跳与 chat，并可注入失败。
type hbRecorder struct {
	mu    sync.Mutex
	order []string // "busy" / "idle" / "chat"，按到达顺序
	ids   map[string]int
	// busyFailFrom：从第 N 次 busy 起返回 500（0 或 1 = 不失败；2 = 第 2 次起失败）
	busyFailFrom int
	busySeen     int
	holdBody     time.Duration // chat 响应体保持多久才结束（制造"长对话"）
}

func (r *hbRecorder) record(kind, id string) {
	r.mu.Lock()
	r.order = append(r.order, kind)
	if id != "" {
		if r.ids == nil {
			r.ids = map[string]int{}
		}
		r.ids[id]++
	}
	r.mu.Unlock()
}

func (r *hbRecorder) snapshot() ([]string, map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := append([]string(nil), r.order...)
	ids := map[string]int{}
	for k, v := range r.ids {
		ids[k] = v
	}
	return cp, ids
}

func (r *hbRecorder) count(kind string) int {
	o, _ := r.snapshot()
	n := 0
	for _, x := range o {
		if x == kind {
			n++
		}
	}
	return n
}

// newHeartbeatServer 起一个假上游：心跳按其 status 记录并可按需失败；chat 先回头、
// 再慢慢吐一个 SSE 帧（hold 之后才结束）—— 这样"读流期间"足够长，能触发续期。
func newHeartbeatServer(t *testing.T, r *hbRecorder) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == HeartbeatPath {
			status := req.URL.Query().Get("status")
			id := req.Header.Get("user-session-id")
			r.mu.Lock()
			if status == "busy" {
				r.busySeen++
				if r.busyFailFrom > 0 && r.busySeen >= r.busyFailFrom {
					r.mu.Unlock()
					r.record("busy", id) // 失败的也算"到达过"，便于断言次数
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			r.mu.Unlock()
			r.record(status, id)
			w.WriteHeader(http.StatusOK)
			return
		}
		// chat：正常 SSE
		r.record("chat", req.Header.Get("user-session-id"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data:{"id":"t","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(r.holdBody) // ★ 关键：这段时间里续期循环应该跑起来
		_, _ = io.WriteString(w, `data:[DONE]`+"\n\n")
	}))
}

func chatOnce(t *testing.T, c *Client) io.ReadCloser {
	t.Helper()
	rc, status, _, err := c.ChatStreamWith(
		&Auth{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"},
		[]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		nil)
	if err != nil {
		t.Fatalf("ChatStreamWith: %v", err)
	}
	if status != http.StatusOK || rc == nil {
		t.Fatalf("ChatStreamWith status=%d rc=%v", status, rc)
	}
	return rc
}

// TestChatRenewsBusyHeartbeat ★ 铁律①：长对话期间必须**反复**续 busy，且 idle 在最后。
func TestChatRenewsBusyHeartbeat(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	defer func() { heartbeatInterval = old }()

	r := &hbRecorder{holdBody: 120 * time.Millisecond}
	srv := newHeartbeatServer(t, r)
	defer srv.Close()

	c := New()
	c.EngineBase = srv.URL
	c.HTTP = srv.Client()
	c.ChatHTTP = srv.Client()

	rc := chatOnce(t, c)
	_, _ = io.Copy(io.Discard, rc) // 读流期间 ≈ holdBody
	_ = rc.Close()

	order, ids := r.snapshot()
	busy, idle := r.count("busy"), r.count("idle")
	t.Logf("顺序=%v 次数=%v busy=%d idle=%d", order, ids, busy, idle)

	if busy < 2 {
		t.Errorf("busy 心跳 = %d，期望 ≥2（占槽 1 次 + 续期至少 1 次）—— 续期循环没跑起来", busy)
	}
	if idle != 1 {
		t.Errorf("idle = %d，期望恰好 1 次（铁律④）", idle)
	}
	if len(order) == 0 || order[len(order)-1] != "idle" {
		t.Errorf("最后一次心跳不是 idle（顺序=%v）—— idle 必须发生在最后一次 busy 之后", order)
	}
	if len(ids) != 1 {
		t.Errorf("出现了多个会话 id（铁律③：只该操作自己造的那一个）：%v", ids)
	}
	// 同一个 id 既用于心跳也用于 chat
	for id, n := range ids {
		if n < 3 {
			t.Errorf("会话 id %s 只出现 %d 次（应含 busy×N + chat + idle）", id, n)
		}
	}
}

// TestFirstBusyFailureDeregisters ★ 铁律②：首次 busy 失败 ⇒ 必须尽力注销该 id。
func TestFirstBusyFailureDeregisters(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = time.Hour // 本用例不需要续期，避免干扰
	defer func() { heartbeatInterval = old }()

	r := &hbRecorder{busyFailFrom: 1, holdBody: 0} // 第 1 次 busy 就 500
	srv := newHeartbeatServer(t, r)
	defer srv.Close()

	c := New()
	c.EngineBase = srv.URL
	c.HTTP = srv.Client()
	c.ChatHTTP = srv.Client()

	rc := chatOnce(t, c) // 失败**不阻断**：仍然要能拿到流
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	order, ids := r.snapshot()
	busy, idle := r.count("busy"), r.count("idle")
	t.Logf("顺序=%v ids=%v busy=%d idle=%d", order, ids, busy, idle)

	if busy < 1 {
		t.Fatal("假上游没收到 busy 心跳，用例前提不成立")
	}
	// 首次 busy 失败(1) + 注销(1) + 流结束时的 idle(1) = 至少 2 次 idle
	if idle < 2 {
		t.Errorf("idle = %d，期望 ≥2（首次 busy 失败后要**额外**注销一次，铁律②）；顺序=%v", idle, order)
	}
	if len(ids) != 1 {
		t.Errorf("注销用的 id 必须与占槽同一个（铁律③）：%v", ids)
	}
	// 失败之后依然把槽还回去了：最后一个是 idle
	if order[len(order)-1] != "idle" {
		t.Errorf("最后一次不是 idle（顺序=%v）", order)
	}
}

// TestBusyRenewalFailureKeepsSlot ★ 铁律①：续期失败**不能**释放槽位（不能提前发 idle）。
func TestBusyRenewalFailureKeepsSlot(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	defer func() { heartbeatInterval = old }()

	r := &hbRecorder{busyFailFrom: 2, holdBody: 90 * time.Millisecond} // 占槽成功，续期全失败
	srv := newHeartbeatServer(t, r)
	defer srv.Close()

	c := New()
	c.EngineBase = srv.URL
	c.HTTP = srv.Client()
	c.ChatHTTP = srv.Client()

	rc := chatOnce(t, c)

	// 读流中途检查：续期开始失败之后，**不应该**出现 idle（槽位还被我们占着）
	time.Sleep(60 * time.Millisecond)
	if got := r.count("idle"); got != 0 {
		t.Errorf("续期失败期间出现了 %d 次 idle —— 铁律①要求失败不释放槽位", got)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	order, _ := r.snapshot()
	idle := r.count("idle")
	t.Logf("顺序=%v busy=%d idle=%d", order, r.count("busy"), idle)
	if r.count("busy") < 2 {
		t.Errorf("busy = %d，期望 ≥2（失败后仍要继续重试，而不是退出循环）", r.count("busy"))
	}
	if idle != 1 {
		t.Errorf("idle = %d，期望恰好 1 次（只在流结束时）", idle)
	}
	if !strings.HasSuffix(strings.Join(order, ","), "idle") {
		t.Errorf("最后一个是 %s，期望 idle", order[len(order)-1])
	}
}
