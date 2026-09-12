package codearts

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	issued   int             // 成功续期次数
	rejected int             // 因 token 已被用过而拒绝的次数
}

func newRefreshServer(t *testing.T) (*httptest.Server, *refreshStub) {
	t.Helper()
	st := &refreshStub{used: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RefreshToken string `json:"refresh_token"`
		}
		raw, _ := readAllLimited(r)
		_ = json.Unmarshal(raw, &in)

		st.mu.Lock()
		if in.RefreshToken != "" && st.used[in.RefreshToken] {
			st.rejected++
			st.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"the refresh token has been used"}`))
			return
		}
		if in.RefreshToken != "" {
			st.used[in.RefreshToken] = true
		}
		st.issued++
		st.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessKeyId":"AK-new","secretAccessKey":"SK-new",
			"securityToken":"ST-new","refreshToken":"RT-` + randHex(8) + `",
			"expiresAt":"2099-01-01T00:00:00Z"}`))
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
	a := &Auth{
		AccessKey:     "AK-old",
		SecretKey:     "SK-old",
		SecurityToken: "ST-old",
		RefreshToken:  "RT-initial-" + randHex(4),
		ExpiresAt:     time.Now().Add(-time.Hour).Unix(),
		FilePath:      filepath.Join(dir, "codearts-test.json"),
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
	srv, _ := newRefreshServer(t)
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

	for i, err := range errs {
		if err != nil && strings.Contains(err.Error(), "has been used") {
			t.Errorf("goroutine %d 发生双消费: %v", i, err)
		}
	}
	if a.AccessKey == "" || a.SecretKey == "" {
		t.Fatalf("并发续期后凭证被写坏: ak=%q sk=%q", a.AccessKey, a.SecretKey)
	}
	t.Logf("并发 %d 次续期后 AK=%s（无双消费）", N, a.AccessKey)
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
