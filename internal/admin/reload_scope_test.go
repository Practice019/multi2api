package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// 评审 F3 的回归测试：reload 必须**显式**指定域，不能靠"谁是默认上游"。
//
// # 缺陷原貌
//
// `accountsReload` 调裸 `SyncToDir(auths)`，它归一成"**当前默认上游**"。
// 那在本部署里碰巧正确 —— `AuthDir` 只装 workbuddy 凭证，
// 而 workbuddy 恰好是第一个注册的（于是也是默认）。
//
// 评审证明：默认上游一旦不是 workbuddy，同一个 reload 会
// **扫描 workbuddy 文件却按别的域剔除** → 把那个上游的账号全删掉并落盘。
//
// 这是"靠巧合成立"的典型：改一下注册顺序、或将来有第三个上游先注册，
// 就会复现生产事故。

// seedWorkbuddyFile 在目录里落一个能被 `auth.LoadDir` 扫到的 workbuddy 凭证文件。
//
// `auth.LoadDir` 只 glob `workbuddy*.json`，文件名前缀不能乱改。
func seedWorkbuddyFile(t *testing.T, dir, uid string) {
	t.Helper()
	a := &auth.Auth{
		UID:          uid,
		AccessToken:  "at-" + uid,
		RefreshToken: "rt-" + uid,
		FilePath:     filepath.Join(dir, "workbuddy-"+uid+".json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
}

// TestReloadScopesToConfiguredProvider 默认上游是 codearts 时，
// reload 仍必须只动 workbuddy 的账号。
//
// 这正是评审证明的绕过场景：若 reload 按 DefaultProvider 归一，
// codearts 的账号会被全部删除并落盘。
func TestReloadScopesToConfiguredProvider(t *testing.T) {
	dir := t.TempDir()
	seedWorkbuddyFile(t, dir, "wb1")
	seedWorkbuddyFile(t, dir, "wb2")

	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	// 关键：默认上游**不是** workbuddy
	p.SetDefaultProvider("codearts")
	p.SyncToDirFor("workbuddy", []*auth.Auth{{UID: "wb1"}})
	p.SyncToDirFor("codearts", []*auth.Auth{{UID: "ca1"}})
	p.SyncToDirFor("other", []*auth.Auth{{UID: "zz1"}})

	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 {
		t.Fatalf("前置：应扫到 2 个 workbuddy 文件，得到 %d", len(auths))
	}

	h := New(Config{
		Pool:            p,
		AuthDir:         dir,
		DefaultProvider: "codearts",  // 默认不是 workbuddy
		ReloadProvider:  "workbuddy", // 本目录属于 workbuddy
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localPost("/admin/accounts/reload"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}

	// 核心断言：别的上游的账号必须存活
	if got := len(p.ListFor("codearts")); got != 1 {
		t.Errorf("codearts 账号被误删了（剩 %d）—— reload 按错误的域剔除了它（评审 F3 复发）", got)
	}
	if got := len(p.ListFor("other")); got != 1 {
		t.Errorf("其它上游的账号被误删了（剩 %d）", got)
	}
	// workbuddy 被对齐到扫描结果（wb1、wb2 都在文件里）
	if got := len(p.ListFor("workbuddy")); got != 2 {
		t.Errorf("workbuddy 账号数=%d want 2（扫描结果里有 2 个）", got)
	}

	// 响应里应回显用了哪个域（便于排障）
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["provider"] != "workbuddy" {
		t.Errorf("响应 provider=%v want workbuddy（应回显实际使用的域）", resp["provider"])
	}
}

// TestReloadFallsBackButWorks 未配置 ReloadProvider 时回落默认上游
// —— 兼容旧装配（会记日志，静默回落正是 bug 藏身之处）。
func TestReloadFallsBackButWorks(t *testing.T) {
	dir := t.TempDir()
	seedWorkbuddyFile(t, dir, "u1")

	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("workbuddy")
	p.SyncToDirFor("workbuddy", []*auth.Auth{{UID: "u1"}})

	h := New(Config{Pool: p, AuthDir: dir, DefaultProvider: "workbuddy"}) // 无 ReloadProvider
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localPost("/admin/accounts/reload"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["provider"] != "workbuddy" {
		t.Errorf("回落后的 provider=%v want workbuddy", resp["provider"])
	}
	if got := len(p.ListFor("workbuddy")); got != 1 {
		t.Errorf("workbuddy 账号数=%d want 1", got)
	}
}

// localPost 造一个本机 POST 请求。
func localPost(target string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	return req
}
