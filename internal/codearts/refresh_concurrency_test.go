package codearts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 续期的**并发安全**测试。
//
// # 为什么这个文件存在（从被删的 scheduler_test.go 里救回来的两条）
//
// 阶段 1 评审指出 `backend.go` + `scheduler.go` 已无引用。我用
// "删掉后 go build + go test 仍通过"证明它们是死代码并删除。
//
// 但评审的清单里没区分**测试覆盖什么**：被删的 `scheduler_test.go` 里
// 有两条测试**与那个调度器无关**，它们验证的是 `Client.RefreshToken` 的
// 并发安全 —— 而那是**活的生产代码**（请求路径的惰性续期就在用它）。
//
// 删掉它们会让一条重要不变量失去覆盖：**refresh_token 是消费型的，
// 并发续期必须被串行化**。所以这两条被救回来单独成文，
// 名字沿用原样（便于对照历史）。
//
// 教训：判"死代码"不能只看"删了还编译得过" —— 还要看**删掉后哪些不变量
// 失去了覆盖**。前者是机械判据，后者需要人判断。

// refreshStub 一个假的 STS 续期端点。
//
// 行为要点：**每次成功续期都换发一个新的 refresh_token**，
// 并且**拒绝已经被用过的那个**（模拟上游实测的 STS5.1806
// "the refresh token has been used"）。有了这两条，
// 并发双消费才会真的暴露出来 —— 否则测试会假绿。
type refreshStub struct {
	mu       sync.Mutex
	used     map[string]bool // 已被消费的 refresh_token
	requests int             // 到达续期端点的请求数
	issued   int             // 成功续期次数
	rejected int             // 因 token 已被用过而拒绝的次数
}

// reqCount / rejCount 供测试在锁内读数（避免直接读字段触发 -race）。
func (st *refreshStub) reqCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.requests
}

func (st *refreshStub) rejCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.rejected
}

func newRefreshServer(t *testing.T) (*httptest.Server, *refreshStub) {
	t.Helper()
	st := &refreshStub{used: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ⚠ 请求体是 application/x-www-form-urlencoded（生产代码用
		// url.Values.Encode()），**不是 JSON**。
		//
		// 这里原先写的是 json.Unmarshal —— 这正是本测试假绿的根因：
		// 表单体解出来 refresh_token 恒为空，于是下面那条
		// "拒绝已用过的 token" 的分支永远不成立，rejected 永远是 0。
		raw, _ := readAllLimited(r)
		form, _ := url.ParseQuery(string(raw))
		rt := form.Get("refresh_token")

		st.mu.Lock()
		st.requests++
		if rt != "" && st.used[rt] {
			st.rejected++
			st.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			// 与上游实测报文一致（客户端按这两个标记判"已消费"）。
			_, _ = w.Write([]byte(`{"error_code":"STS5.1806",` +
				`"error_msg":"invalid refresh token: 'the refresh token has been used'"}`))
			return
		}
		if rt != "" {
			st.used[rt] = true
		}
		st.issued++
		st.mu.Unlock()

		// ⚠ 响应体必须是**真实的 STS 形态**：credentials 嵌套 + refresh_token。
		// 原先这里是磁盘凭证的扁平形态（accessKeyId/expiresAt），
		// 客户端解不出 credentials → 续期永远"成功不了"，
		// 内存里的 refresh_token 也就永远不会前进（第二层假绿）。
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"credentials":{"access_key_id":"AK-new",`+
			`"secret_access_key":"SK-new","security_token":"ST-new",`+
			`"expiration":"2099-01-01T00:00:00Z"},"refresh_token":"RT-%s"}`, randHex(8))
	}))
	t.Cleanup(srv.Close)
	return srv, st
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func readAllLimited(r *http.Request) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
		if len(buf) > 1<<20 {
			return buf, nil
		}
	}
}

func newTestAuth(t *testing.T, dir string) *Auth {
	t.Helper()
	// DPoP 私钥必须真的有 —— 否则 RefreshToken 在发请求**之前**就返回
	// "缺 DPoP 私钥"，测试根本到不了 HTTP 层（这是本测试此前假绿的第一层原因）。
	kp, err := NewDPoPKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := json.Marshal(kp.PrivateJWK())
	if err != nil {
		t.Fatal(err)
	}
	a := &Auth{
		AccessKey:         "AK-old",
		SecretKey:         "SK-old",
		SecurityToken:     "ST-old",
		RefreshToken:      "RT-initial-" + randHex(4),
		ExpiresAt:         time.Now().Add(-time.Hour).Unix(),
		FilePath:          filepath.Join(dir, "codearts-test.json"),
		DPoPPrivateKeyJWK: jwk,
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestConcurrentRefreshConsumesTokenOnce 8 个 goroutine 同时续期，
// 不得发生"同一个 refresh_token 被消费两次"。
//
// 这是**活的生产不变量**：refresh_token 一次性，
// 而请求路径（ChatStream 惰性续期）与后台任务（jobs.go）会同时触发续期。
func TestConcurrentRefreshConsumesTokenOnce(t *testing.T) {
	srv, st := newRefreshServer(t)
	dir := t.TempDir()
	a := newTestAuth(t, dir)

	c := New()
	c.STSBase = srv.URL

	const N = 8
	var wg sync.WaitGroup
	errs := make([]error, N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同时起跑，最大化竞争
			errs[i] = c.RefreshToken(a)
		}(i)
	}
	close(start)
	wg.Wait()

	// 防假绿第一道：请求必须真的到达了续期端点。
	// 修复前 RefreshToken 因缺 DPoP 私钥在发请求前就返回，
	// 这个计数会是 0 —— 测试却在"通过"。
	if got := st.reqCount(); got != N {
		t.Errorf("到达续期端点的请求数 = %d，期望 %d（请求没发出去 = 假绿）", got, N)
	}
	// 防假绿第二道：stub 必须真的有能力拒绝（见 TestRefreshStubRejectsUsedToken），
	// 因此这里 rejected==0 才是"没有双消费"的证据而不是"stub 不会拒绝"。
	if got := st.rejCount(); got != 0 {
		t.Errorf("续期已被 refreshMu 串行化，不应出现 token 被重复消费，实际 rejected=%d", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d 续期失败: %v", i, err)
		}
	}
	if a.AccessKey == "" || a.SecretKey == "" {
		t.Fatalf("并发续期后凭证被写坏: ak=%q sk=%q", a.AccessKey, a.SecretKey)
	}
	t.Logf("并发 %d 次续期后 AK=%s（无双消费，rejected=%d）", N, a.AccessKey, st.rejCount())
}

// TestRefreshSerializedWritesConsistentFile 并发续期后磁盘文件必须仍然合法。
//
// 覆盖的是"写盘未串行化会把文件写坏/留 .tmp"这一类故障。
func TestRefreshSerializedWritesConsistentFile(t *testing.T) {
	srv, _ := newRefreshServer(t)
	dir := t.TempDir()
	a := newTestAuth(t, dir)

	c := New()
	c.STSBase = srv.URL

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = c.RefreshToken(a)
		}()
	}
	close(start)
	wg.Wait()

	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("并发续期后磁盘文件损坏: %v", err)
	}
	if back.AccessKey == "" || back.SecurityToken == "" {
		t.Errorf("磁盘凭证不完整: %+v", back)
	}
	if _, err := os.Stat(a.FilePath + ".tmp"); err == nil {
		t.Error("残留 .tmp 文件，说明写盘未收尾")
	}
}

// TestRefreshStubRejectsUsedToken 直接证明 refreshStub 的
// "拒绝已用过的 refresh_token" 分支是**活的**。
//
// 为什么必须有这条：修复前 stub 用 `json.Unmarshal` 解析请求体，
// 而 `Client.RefreshToken` 发的是 `application/x-www-form-urlencoded`
// （`url.Values.Encode()`）→ `in.RefreshToken` **恒为空** →
// 那条 `if in.RefreshToken != "" && st.used[...]` 永远不成立 →
// `rejected` 永远是 0 → `TestConcurrentRefreshConsumesTokenOnce`
// 无论有没有双消费都会绿（假绿）。
//
// 这条断言把"stub 真的会拒绝"变成可证伪的：
// 把 `url.ParseQuery` 改回 `json.Unmarshal`，它立刻变红。
func TestRefreshStubRejectsUsedToken(t *testing.T) {
	srv, st := newRefreshServer(t)

	rt := "RT-single-use-" + randHex(4)
	if code, _ := postRefreshForm(t, srv.URL, rt); code != http.StatusOK {
		t.Fatalf("首次续期应成功，实际 HTTP %d", code)
	}

	// 同一个 token 再用一次 —— 上游必须拒绝（STS5.1806）。
	code, body := postRefreshForm(t, srv.URL, rt)
	if code != http.StatusBadRequest {
		t.Fatalf("已消费的 token 应被拒绝 (400)，实际 HTTP %d", code)
	}
	if !strings.Contains(body, "has been used") {
		t.Errorf("拒绝响应体不含 'has been used': %s", body)
	}
	if got := st.reqCount(); got != 2 {
		t.Fatalf("stub 收到 %d 次请求，期望 2 次", got)
	}
	if got := st.rejCount(); got == 0 {
		t.Fatal("refreshStub.rejected == 0 —— stub 的拒绝分支从未被执行，" +
			"说明请求体没有被解析出 refresh_token（假绿根因）")
	}
}

// postRefreshForm 用与生产代码**完全相同**的形态发一次续期请求：
// application/x-www-form-urlencoded + url.Values.Encode()。
func postRefreshForm(t *testing.T, base, rt string) (int, string) {
	t.Helper()
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", "vscode-codebot")
	form.Set("refresh_token", rt)
	resp, err := http.Post(base+TokenPath, "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
