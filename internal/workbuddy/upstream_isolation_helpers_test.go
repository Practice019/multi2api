// upstream_isolation_helpers_test.go 上游边界测试的装置与纯函数。
//
// 与 upstream_isolation_test.go 分开放的理由：那边是**用例**（读的人关心
// 断言什么），这边是**装置**（读的人关心怎么造的）。混在一起时，
// 装置代码会把用例意图淹掉。
package workbuddy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/upstream"
)

// newIsolationUpstream 起一个对所有上游端点都返回"成功"的桩服务器。
//
// 与 checkin_test.go 的 fakeUpstream 不同：那个还会**计数**校验调用次数；
// 本文件关心的是"作用在哪个账号上"，而 uid 不出现在请求里
// （upstream.Client 从 auth 里取，桩服务器看不到），
// 所以这里改用"结果写进了谁的历史/快照"来观察（见 recordedUIDs）。
func newIsolationUpstream(t *testing.T) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":50,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			// 其余端点（猫档案/旅行/成长）一律 404：
			// 本文件的用例不依赖它们的成功，只依赖"请求打向了谁"。
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(srv.Close)
	return &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
}

// timeNowForTest 包一层 time.Now，让用例读起来不用 import time。
func timeNowForTest() time.Time { return time.Now() }

// newIsolationLog 造一份落在临时目录的历史日志。
func newIsolationLog(t *testing.T) *checkinlog.Log {
	t.Helper()
	return checkinlog.New(filepath.Join(t.TempDir(), "isolation-log.json"), 30)
}

// readPackageSources 读本包目录下所有**非测试** .go 文件的内容。
//
// 只读非测试文件：守卫的对象是产品代码；测试里为了制造脏样本
// 会刻意写 `cfg.Pool.List()`，把它算进来会让守卫永远红。
func readPackageSources(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读包目录失败: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		out[name] = string(raw)
	}
	return out
}

// stripGoComments 剥掉 Go 源码里的行注释与块注释。
//
// # 为什么必须剥（本项目的血泪教训）
//
// 判据若直接匹配原始文本，注释里的说明文字会**自命中** ——
// 我在修这个 bug 的注释里逐字写了 `cfg.Pool.List()` 来解释原本的写法，
// 不剥注释的话守卫会因为文档而红。更糟的是反过来：
// 有人可以**靠删注释**让真缺陷不被发现（注释在文本里，缺陷也在文本里）。
//
// # 为什么不用正则替换注释
//
// 项目已有教训：`/\/\/.*$/` 这类正则遇到 CRLF 会**静默失效**
// （`$` 匹配不上 `\r`）。所以这里用逐字符状态机，对行尾是什么完全免疫。
//
// 简化之处（对本用途足够）：不处理字符串字面量里的 `//`。
// 本包没有把 "cfg.Pool.List()" 写进字符串的地方，且守卫的判据是
// "出现即违规"，误报比漏报安全。
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))

	const (
		stCode = iota
		stLineComment
		stBlockComment
		stString
		stRawString
		stRune
	)
	state := stCode

	runes := []rune(src)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		switch state {
		case stCode:
			switch {
			case c == '/' && next == '/':
				state = stLineComment
				i++
			case c == '/' && next == '*':
				state = stBlockComment
				i++
			case c == '"':
				state = stString
				b.WriteRune(c)
			case c == '`':
				state = stRawString
				b.WriteRune(c)
			case c == '\'':
				state = stRune
				b.WriteRune(c)
			default:
				b.WriteRune(c)
			}
		case stLineComment:
			if c == '\n' {
				state = stCode
				b.WriteRune(c) // 保留换行，避免把两行粘成一行
			}
		case stBlockComment:
			if c == '*' && next == '/' {
				state = stCode
				i++
			} else if c == '\n' {
				b.WriteRune(c)
			}
		case stString:
			b.WriteRune(c)
			if c == '\\' && i+1 < len(runes) {
				i++
				b.WriteRune(runes[i])
			} else if c == '"' {
				state = stCode
			}
		case stRawString:
			b.WriteRune(c)
			if c == '`' {
				state = stCode
			}
		case stRune:
			b.WriteRune(c)
			if c == '\\' && i+1 < len(runes) {
				i++
				b.WriteRune(runes[i])
			} else if c == '\'' {
				state = stCode
			}
		}
	}
	return b.String()
}

// indexOf 已由 admin_routes_test.go 提供（同一测试包内共用），
// 这里不再重复声明 —— 否则会编译冲突。

// ---------------------------------------------------------------------------
// 按凭证区分"请求打给了谁"
// ---------------------------------------------------------------------------

// trackingUpstream 记录每个凭证（按 AccessToken 区分）发起了多少次请求。
//
// # 为什么不能用账号 uid 计数
//
// uid 不出现在 HTTP 请求里（upstream.Client 从 *auth.Auth 取 header），
// 桩服务器看不到它。但**凭证**会出现在 Authorization header 里，
// 所以用 AccessToken 反查是可靠的：每个账号在测试里给不同的 token。
type trackingUpstream struct {
	mu     sync.Mutex
	byTag  map[string]int
	server *httptest.Server
}

// newTrackingUpstream 起一个按 token 计数的桩服务器，并返回 tag→token 的对应关系。
//
// tag 由调用方约定（测试里用账号 uid），token 由 newTrackingAuth 生成。
func newTrackingUpstream(t *testing.T) *trackingUpstream {
	t.Helper()
	tu := &trackingUpstream{byTag: map[string]int{}}
	tu.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tu.record(r.Header.Get("Authorization"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":50,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	t.Cleanup(tu.server.Close)
	return tu
}

// record 按 Authorization header 记一次请求。
func (tu *trackingUpstream) record(authz string) {
	tu.mu.Lock()
	defer tu.mu.Unlock()
	// header 形如 "Bearer <token>"；取 token 部分作为身份。
	token := strings.TrimPrefix(authz, "Bearer ")
	tu.byTag["token:"+token]++
}

// client 造一个指向本桩服务器的 upstream.Client。
func (tu *trackingUpstream) client() *upstream.Client {
	return &upstream.Client{HTTP: tu.server.Client(), ChatBaseCN: tu.server.URL, BillingBaseCN: tu.server.URL}
}

// countForToken 查某个 token 被请求了多少次。
func (tu *trackingUpstream) countForToken(token string) int {
	tu.mu.Lock()
	defer tu.mu.Unlock()
	return tu.byTag["token:"+token]
}

// trackingToken 返回账号对应的 token（约定：token = "at-" + uid）。
func trackingToken(uid string) string { return "at-" + uid }
