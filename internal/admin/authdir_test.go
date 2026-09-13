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
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
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
