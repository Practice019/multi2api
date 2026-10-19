package authlimit

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errBodyView 解析 413 响应体用。
type errBodyView struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestBodyLimitPassesSmallAndKeepsBody 小 body 放行，且**body 还能被后面的
// handler 读到**（读一遍就把 body 吃掉的话，handler 会看到"用户名口令都是空的"，
// 表现成莫名其妙的 400）。
func TestBodyLimitPassesSmallAndKeepsBody(t *testing.T) {
	const payload = `{"username":"alice","password":"correct-horse-battery"}`

	r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(payload))
	w := httptest.NewRecorder()

	if !EnforceBodyLimit(w, r, MaxAnonymousBody) {
		t.Fatalf("小 body 应当放行，却写回了 %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("放行时不该写任何响应，却写了 %q", w.Body.String())
	}
	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("重读 body: %v", err)
	}
	if string(got) != payload {
		t.Errorf("body 应原样保留，得到 %q", got)
	}
	if r.ContentLength != int64(len(payload)) {
		t.Errorf("ContentLength 应改成真实长度 %d，得到 %d", len(payload), r.ContentLength)
	}
}

// TestBodyLimitBoundary 正好 max 字节放行；max+1 拒绝。
func TestBodyLimitBoundary(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want bool
	}{
		{MaxAnonymousBody, true},
		{MaxAnonymousBody + 1, false},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(strings.Repeat("x", tc.n)))
		w := httptest.NewRecorder()
		if got := EnforceBodyLimit(w, r, MaxAnonymousBody); got != tc.want {
			t.Errorf("%d 字节应当 %v，得到 %v（HTTP %d）", tc.n, tc.want, got, w.Code)
		}
	}
}

// TestBodyLimitZeroMeansDefault max <= 0 退回 MaxAnonymousBody（0 是"忘了传"，
// 不是"一个字节都不许传"）。
func TestBodyLimitZeroMeansDefault(t *testing.T) {
	small := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader("{}"))
	if !EnforceBodyLimit(httptest.NewRecorder(), small, 0) {
		t.Error("max=0 时应退回默认上限，小 body 应当放行")
	}

	big := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(strings.Repeat("x", MaxAnonymousBody+1)))
	if EnforceBodyLimit(httptest.NewRecorder(), big, 0) {
		t.Error("max=0 退回默认上限后，超限仍应拒绝")
	}
}

// TestBodyLimitIgnoresForgedContentLength ★ 只看 Content-Length 是错的：
// 那是客户端自己写的头。这里把 Content-Length 伪造成 10（真实 16 KiB），
// 判据必须是"真读一遍"，而不是"看看头"。
func TestBodyLimitIgnoresForgedContentLength(t *testing.T) {
	big := strings.Repeat("a", 16<<10)
	r := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(big))
	r.ContentLength = 10
	r.Header.Set("Content-Length", "10")

	w := httptest.NewRecorder()
	if EnforceBodyLimit(w, r, MaxAnonymousBody) {
		t.Fatal("伪造 Content-Length 的 16 KiB 请求体必须被拒")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("应当回 413，得到 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("413 的响应体应当是 JSON，Content-Type 得到 %q", ct)
	}
	var got errBodyView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v（%q）", err, w.Body.String())
	}
	if got.Error.Code != bodyTooLargeCode {
		t.Errorf("错误码应为 %q，得到 %q", bodyTooLargeCode, got.Error.Code)
	}
	if !strings.Contains(got.Error.Message, "请求体过大") {
		t.Errorf("错误文案应说清是请求体过大，得到 %q", got.Error.Message)
	}
}

// TestBodyLimitRealServer 走真实 HTTP（Content-Length 由 transport 按真实长度
// 计算），确认"放行后 body 可用"与"超限回 413+JSON"两条在线上路径同样成立。
func TestBodyLimitRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !EnforceBodyLimit(w, r, MaxAnonymousBody) {
			return // 413 已经写好了
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(raw) // 原样回显：证明 body 没被吃掉
	}))
	defer srv.Close()

	small := []byte(`{"username":"bob","password":"secret-1234"}`)
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(small))
	if err != nil {
		t.Fatalf("小 body POST: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("小 body 应当 200，得到 %d", resp.StatusCode)
	}
	if string(raw) != string(small) {
		t.Errorf("body 应原样回显，得到 %q", raw)
	}

	resp2, err := http.Post(srv.URL, "application/json", bytes.NewReader(bytes.Repeat([]byte("a"), 16<<10)))
	if err != nil {
		t.Fatalf("大 body POST: %v", err)
	}
	raw2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("大 body 应当 413，得到 %d", resp2.StatusCode)
	}
	var got errBodyView
	if err := json.Unmarshal(raw2, &got); err != nil {
		t.Fatalf("413 响应体不是合法 JSON: %v（%q）", err, raw2)
	}
	if got.Error.Code != bodyTooLargeCode {
		t.Errorf("错误码应为 %q，得到 %q", bodyTooLargeCode, got.Error.Code)
	}
}

// TestBodyLimitNilBody 没有 body 的请求（GET/HEAD 之类）直接放行。
func TestBodyLimitNilBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/login", nil)
	r.Body = nil
	if !EnforceBodyLimit(httptest.NewRecorder(), r, MaxAnonymousBody) {
		t.Error("没有 body 的请求不该被卡")
	}
	if !EnforceBodyLimit(httptest.NewRecorder(), nil, MaxAnonymousBody) {
		t.Error("r 为 nil 时不该 panic，也不该写响应")
	}
}
