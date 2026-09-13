package admin

// authdir_test.go —— 落盘目录与文件名的守卫。
//
// # 这两个 bug 都是用户实测报出来的
//
//  1. **文件名是空的** → 核心把**目录**当成文件重命名：
//
//     filepath.Join("auths", "") = "auths"    ← 目录本身
//     os.Rename("auths.tmp", "auths")         ← Access is denied
//
//     用户看到的报错：
//     `凭证落盘失败: rename auths.tmp auths: Access is denied.`
//     —— 完全看不出与"文件名"有关。
//
//  2. **落盘用的是默认上游的目录** → codearts 的凭证被写进 workbuddy 的目录。
//
// 两条都不是"接口没实现"，而是**跨层契约里我假设了对方会做某件事，
// 但没对着它的代码核实**。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

// authFileStub 实现 authFileWriter，可控地返回文件名。
type authFileStub struct {
	name string
	raw  []byte
	err  error
}

func (a *authFileStub) MarshalAuthFile() (string, []byte, error) {
	return a.name, a.raw, a.err
}

// jsonBody 造一个 JSON 请求体，避免每处都写 io.NopCloser(strings.NewReader(...))。
func jsonBody(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}

// TestPollViaFlowWritesToProviderDir 落盘目录必须是**上游自报的**。
//
// 实测踩过：核心用的是 `h.cfg.AuthDir`（默认上游 workbuddy 的目录），
// 于是 codearts 授权成功后凭证被写进 workbuddy 的目录。
func TestPollViaFlowWritesToProviderDir(t *testing.T) {
	careartsDir := t.TempDir()
	workbuddyDir := t.TempDir()

	flow := &fakeFlow{
		configured: true,
		authDir:    careartsDir, // ← 上游自报的目录
		pollFn: func(string) (gateway.Credential, error) {
			return gateway.Credential{
				Provider: "codearts",
				UID:      "AK-TEST",
				Secret:   &authFileStub{name: "codearts-AK-TEST.json", raw: []byte(`{"a":1}`)},
			}, nil
		},
	}

	reg := gateway.NewRegistry()
	if err := reg.Register(&flowProvider{
		stubProvider: stubProvider{id: "codearts", caps: gateway.CapChat},
		flow:         flow,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{
		Registry:  reg,
		AuthDir:   workbuddyDir, // ← 默认上游的目录（**不该**被用到）
		AuthsBase: t.TempDir(),
	})

	rec := httptest.NewRecorder()
	req := localReq("POST", "/admin/login/poll")
	req.Body = jsonBody(`{"state":"S","provider":"codearts"}`)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", rec.Code, rec.Body)
	}
	// 凭证必须落在**上游自报的**目录
	got := filepath.Join(careartsDir, "codearts-AK-TEST.json")
	if _, err := os.Stat(got); err != nil {
		t.Errorf("凭证没写到上游自报的目录 %s: %v", careartsDir, err)
	}
	// 而且**不该**落到默认上游的目录
	wrong := filepath.Join(workbuddyDir, "codearts-AK-TEST.json")
	if _, err := os.Stat(wrong); err == nil {
		t.Errorf("凭证被写进了默认上游的目录 %s —— "+
			"codearts 的号会出现在 workbuddy 的目录里", workbuddyDir)
	}
}

// TestPollViaFlowRescansWithUpstreamParser 重扫必须用**上游自己的解析器**。
//
// # 为什么上面那条抓不到这个 bug
//
// `TestPollViaFlowWritesToProviderDir` 只断言**落盘位置**。旧实现（bug）
// 的特征是"落盘对了、重扫错了"：
//
//	auths, lerr := auth.LoadDirCompat(h.cfg.AuthDir, cred.Provider)
//
// `auth.LoadDir` 的 Glob 写死 `workbuddy*.json`，而 fixture 的两个目录里
// 都**没有** `workbuddy*.json` → 返回**空切片、无错** → 断言全绿。
// **测试全绿却抓不到 bug**，正是这条要补的原因。
//
// # 这条怎么把它变成红的
//
// ① 桩自己实现 `gateway.CredentialLoader`，报出**只有它认得**的凭证
//
//	（uid `ca-1`/`ca-2`）。正确实现走桩 → 池子里有这两个号；
//	错误实现走 `auth.LoadDirCompat` → 扫不到任何东西 → 池子里空空，
//	而且 `SyncToDirFor("codearts", [])` 会把预置的旧号**删掉**。
//
// ② 在两个目录里**都**摆上真的 `workbuddy*.json` 老式凭证文件来模拟
//
//	"根目录遗留"（`LoadDirCompat` 会连父目录**一起**扫，这是它的设计）。
//	于是错误实现不但扫到东西，还扫到的是 **workbuddy 的号并灌进 codearts 域**
//	—— 这正是用户实测的那条跨域污染。
//
// ⚠ 断言用的是"池子里出现了**桩报出来的** uid"，而不是"scanned 的个数"：
// 个数在两个实现下可能巧合相等（都可能是 0 或都是 2），
// **uid 的身份**才是不可能巧合的判据 —— 本文件头的教训（"fixture 要能区分
// 两个实现，否则变异蒙对"）在这里的对应物。
func TestPollViaFlowRescansWithUpstreamParser(t *testing.T) {
	base := t.TempDir()
	caDir := filepath.Join(base, "codearts")  // ← 上游自报的目录
	wbDir := filepath.Join(base, "workbuddy") // ← 默认上游的目录（**不该**被扫）

	// ⚠ 模拟"根目录/默认上游目录里留有 workbuddy 旧凭证"。
	// 这些文件**恰好**能被 `auth.LoadDirCompat` 扫到（它 glob `workbuddy*.json`
	// 并且会扫父目录 base）—— 于是错误实现会拿到"wb-stale-1"这个号。
	// 正确实现（走桩的 CredentialLoader）**永远看不到它们**。
	//
	// 两个位置都放：base 根（`LoadDirCompat` 的兼容读）与 wbDir
	//（`h.cfg.AuthDir` 指向的地方）—— 旧代码传的正是 `h.cfg.AuthDir` 作为 base，
	// `LoadDirCompat(wbDir, "codearts")` 会扫 `wbDir/codearts` + `wbDir`。
	seedStaleWorkbuddyCred(t, base, "wb-stale-root")
	if err := os.MkdirAll(wbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedStaleWorkbuddyCred(t, wbDir, "wb-stale-default")

	// 桩报出的凭证：只有它自己的解析器认得（uid 集合与 workbuddy 的**不相交**）。
	caCreds := []gateway.Credential{{UID: "ca-1", Nickname: "CA One"}, {UID: "ca-2", Nickname: "CA Two"}}

	flow := &fakeFlow{
		configured: true,
		authDir:    caDir,
		pollFn: func(string) (gateway.Credential, error) {
			return gateway.Credential{
				Provider: "codearts",
				UID:      "AK-TEST",
				Secret:   &authFileStub{name: "codearts-AK-TEST.json", raw: []byte(`{"a":1}`)},
			}, nil
		},
	}

	// ⚠ 桩必须**同时**满足 LoginFlow（fakeFlow 提供）与 CredentialLoader
	//（credFlowProvider 提供）—— 两者住在同一个 Provider 上的不同面。
	prov := &credFlowProvider{
		flowProvider: flowProvider{
			stubProvider: stubProvider{id: "codearts", caps: gateway.CapChat},
			flow:         flow,
		},
		creds: caCreds,
	}

	reg := gateway.NewRegistry()
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}

	// 前提自检：桩必须真的实现了 CredentialLoader —— 否则会走 501 分支，
	// 用例会红得莫名其妙（实测踩过这种"断言失配静默退化"）。
	if _, ok := gateway.ExtOf[gateway.CredentialLoader](prov); !ok {
		t.Fatal("前提不满足：桩没实现 gateway.CredentialLoader")
	}
	if _, ok := gateway.ExtOf[gateway.AuthDirExt](prov); !ok {
		t.Fatal("前提不满足：桩没实现 gateway.AuthDirExt（authDir 转发链断了）")
	}

	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("workbuddy")
	// 预置 codearts 域里的旧号 —— 错误实现扫到空切片后
	// `SyncToDirFor` 会把它**删掉**（这也是可观测的差异）。
	p.SyncToDirFor("codearts", []*auth.Auth{{UID: "ca-old"}})

	h := New(Config{
		Pool:            p,
		Registry:        reg,
		DefaultProvider: "codearts",
		// ⚠ 这两行是 bug 的现场：`AuthDir` 是默认上游的目录，
		// 旧代码把它连同 workbuddy 的解析器一起用去扫 codearts。
		AuthDir:   wbDir,
		AuthsBase: base,
	})

	rec := httptest.NewRecorder()
	req := localReq("POST", "/admin/login/poll")
	req.Body = jsonBody(`{"state":"S","provider":"codearts"}`)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", rec.Code, rec.Body)
	}

	// ---- ① 池子的 codearts 域里必须有**桩报出来的**那些号 ----
	got := map[string]bool{}
	for _, st := range p.ListFor("codearts") {
		got[st.UID] = true
	}
	for _, want := range []string{"ca-1", "ca-2"} {
		if !got[want] {
			t.Errorf("池子的 codearts 域里没有 %q（实际有 %v）—— "+
				"说明重扫**没用上游自己的解析器**（gateway.CredentialLoader），"+
				"而是回落成了写死的 `auth.LoadDirCompat`（只 glob workbuddy*.json）",
				want, keysOf(got))
		}
	}

	// ---- ② workbuddy 的号**一个都不能**被灌进 codearts 域 ----
	//
	// 这正是用户实测的跨域污染：旧实现扫到根目录遗留的 workbuddy 旧凭证，
	// 紧接着 `SyncToDirFor(cred.Provider /* =codearts */, auths)` 把它们
	// 灌进了 codearts 域，而回执仍然 `status:"ok"`。
	for _, bad := range []string{"wb-stale-root", "wb-stale-default"} {
		if got[bad] {
			t.Errorf("workbuddy 的账号 %q 被灌进了 **codearts** 域（实际 %v）—— "+
				"重扫用错了目录/解析器，扫到了默认上游的旧凭证", bad, keysOf(got))
		}
	}

	// ---- ③ 池子的 workbuddy 域不该被这次 codearts 轮询改动 ----
	for _, st := range p.ListFor("workbuddy") {
		if st.UID == "wb-stale-root" || st.UID == "wb-stale-default" {
			t.Errorf("codearts 的轮询动了 workbuddy 域（%q）—— 域串了", st.UID)
		}
	}

	// ---- ④ 旧号被正确对齐掉（codearts 域 = 恰好桩报出来的那两个）----
	if len(got) != 2 {
		t.Errorf("codearts 域应有恰好 2 个号（ca-1/ca-2），实际 %d 个：%v —— "+
			"多出来的说明扫到了别的上游的凭证，少了的说明没扫到自己的",
			len(got), keysOf(got))
	}
	if got["ca-old"] {
		t.Errorf("预置的旧号 ca-old 应被对齐掉（SyncToDirFor 的语义），实际仍在池中")
	}
}

// keysOf 把集合拍成有序切片，仅供报错信息可读。
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// seedStaleWorkbuddyCred 在 dir 下写一个 `workbuddy*.json` 凭证文件。
//
// 文件名前缀**必须**是 `workbuddy` —— 它模拟的是"迁移期遗留在旧位置的
// workbuddy 凭证"，只有 `auth.LoadDir`（glob `workbuddy*.json`）认得出。
// 这正是错误实现会误读到的那个东西。
func seedStaleWorkbuddyCred(t *testing.T, dir, uid string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{
		UID:          uid,
		AccessToken:  "at-" + uid,
		RefreshToken: "rt-" + uid,
		FilePath:     filepath.Join(dir, "workbuddy-"+uid+".json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("落 fixture 凭证 %s 失败: %v", uid, err)
	}
}

// credFlowProvider 同时实现 LoginFlow 与 CredentialLoader 的桩。
//
// 两个面住在同一个 Provider 上，正是真实上游（workbuddy / codearts）的形态 ——
// 而这个 bug 的本质就是"两个面用了不一致的判据"：`AuthDirExt` 面问对了目录，
// `CredentialLoader` 面（当时**不存在**，核心写死 workbuddy 的解析器）却错了。
type credFlowProvider struct {
	flowProvider
	creds []gateway.Credential
}

// LoadCredentials 报出本桩自己的凭证（**不看 dir**）。
//
// ⚠ 刻意**不**去磁盘读：这样"核心到底问没问上游"这件事在测试里是可判定的
// —— 若核心问上游，池子里一定有这些 uid；若核心自己写死解析器，一定没有。
// 凭证格式是上游的事实，桩就是它的权威。
func (p *credFlowProvider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	out := make([]gateway.Credential, 0, len(p.creds))
	for _, c := range p.creds {
		c.Provider = p.ID()
		out = append(out, c)
	}
	return out, nil
}

// 编译期断言：漏了任何一个都会让本用例静默走错分支。
var (
	_ gateway.CredentialLoader = (*credFlowProvider)(nil)
	_ gateway.AuthDirExt       = (*credFlowProvider)(nil)
	_ gateway.LoginFlow        = (*credFlowProvider)(nil)
)

// TestPollViaFlowRejectsNilPool 池子为 nil 时不能 panic。
//
// 上面的用例都靠 Pool 非 nil 才走到最后一步；这条补上"没池子"的退化路径 ——
// 它是启动期缺配置的常见形态。
func TestPollViaFlowRejectsNilPool(t *testing.T) {
	dir := t.TempDir()
	flow := &fakeFlow{
		configured: true,
		authDir:    dir,
		pollFn: func(string) (gateway.Credential, error) {
			return gateway.Credential{
				Provider: "codearts", UID: "AK2",
				Secret: &authFileStub{name: "codearts-AK2.json", raw: []byte(`{}`)},
			}, nil
		},
	}
	reg := gateway.NewRegistry()
	if err := reg.Register(&flowProvider{
		stubProvider: stubProvider{id: "codearts", caps: gateway.CapChat},
		flow:         flow,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg, AuthDir: dir, AuthsBase: t.TempDir()}) // Pool = nil

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Pool 为 nil 时 panic 了：%v", r)
		}
	}()
	rec := httptest.NewRecorder()
	req := localReq("POST", "/admin/login/poll")
	req.Body = jsonBody(`{"state":"S","provider":"codearts"}`)
	h.ServeHTTP(rec, req)

	// 凭证仍应落盘（落盘不依赖池子），只是不会进池
	if _, err := os.Stat(filepath.Join(dir, "codearts-AK2.json")); err != nil {
		t.Errorf("凭证应已落盘（落盘与池子无关）: %v", err)
	}
}
