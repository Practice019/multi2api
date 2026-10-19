package admin

// account_delete_test.go —— 「删除账号」必须**真的删掉凭证文件**，且删不掉要说出来。
//
// # 为什么必须有这几条（用户实测报的缺陷，2026-10-19）
//
// 用户原话：「导入之后删除不掉」，并附了截图（账号池里两行 loomy 账号）。
// 探针（tests/frontend/verify_delete_flow_e2e.js，纯合成数据）实测拿到：
//
//	DELETE /admin/accounts/{uid}?purge_file=1
//	→ 200 {"file":"","purged_file":"","removed":true}   日志 deleted=""
//	→ 文件还在盘上 → 重载/重启又装回池子
//
// 两条独立的失效：
//
//	① 池条目上的 FilePath 为空（上游投影漏传）⇒ purge 静默空转
//	② 回执**不说**"文件没删" ⇒ 前端照旧 toast「已彻底删除」
//
// 所以下面分别钉住：修①的兜底（按上游扫描找回路径）、修②的如实回执，
// 以及"不许删凭证树之外的文件"这条安全边界。
//
// # 用的是真文件、真池子、真路由
//
// 只有"上游"是桩（`authDirProvider`，见 reload_by_provider_test.go）——
// 凭证格式由上游自报，桩直接给出 `gateway.Credential`。
// 文件在 t.TempDir() 里，**不碰**仓库的 auths/ 与任何真实账号。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/gateway"
	"workbuddy2api/internal/pool"
)

const delTestUID = "ca-1"

// deleteFixture 一个"一个上游、一个账号、一个真凭证文件"的最小场景。
type deleteFixture struct {
	h    *Handler
	p    *pool.Pool
	base string
	dir  string
	file string // 真文件的全路径
}

// newDeleteFixture 搭场景。
//
//	poolFilePath    池条目上那份 FilePath（空 = 复现用户报的"投影漏传"）
//	loaderKnowsPath 上游**扫描器**是否报得出路径（false = 连兜底都找不回来，
//	                用于验"找不回时必须如实回执"）
func newDeleteFixture(t *testing.T, poolFilePath string, loaderKnowsPath bool) *deleteFixture {
	t.Helper()

	base := t.TempDir()
	dir := filepath.Join(base, "codearts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, delTestUID+".json")
	if err := os.WriteFile(file, []byte(`{"session":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaderFilePath := ""
	if loaderKnowsPath {
		loaderFilePath = file
	}

	reg := gateway.NewRegistry()
	prov := &authDirProvider{
		stubProvider: stubProvider{id: "codearts", caps: gateway.CapChat},
		dir:          dir,
		creds:        []gateway.Credential{{UID: delTestUID, FilePath: loaderFilePath}},
	}
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}

	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.SetDefaultProvider("codearts")
	p.SyncToDirFor("codearts", []*auth.Auth{{UID: delTestUID, FilePath: poolFilePath}})

	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true,
		Pool:            p,
		Registry:        reg,
		AuthDir:         dir,
		AuthsBase:       base,
		DefaultProvider: "codearts",
	})
	return &deleteFixture{h: h, p: p, base: base, dir: dir, file: file}
}

// del 发一次删除请求，返回 (状态码, 回执字段)。
func (f *deleteFixture) del(t *testing.T, uid string, purge bool) (int, map[string]any) {
	t.Helper()
	target := "/admin/accounts/" + uid
	if purge {
		target += "?purge_file=1"
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, localReq("DELETE", target))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// fileExists 文件还在不在（这才是用户真正的判据：不在 = 真的删掉了）。
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestDeletePurgesCredentialFile 正常路径：池里有 FilePath ⇒ 文件被删。
func TestDeletePurgesCredentialFile(t *testing.T) {
	f := newDeleteFixture(t, "", true)
	// 让池条目带上真实路径（= 上游投影正确时的形态）
	f.p.SyncToDirFor("codearts", []*auth.Auth{{UID: delTestUID, FilePath: f.file}})

	code, resp := f.del(t, delTestUID, true)
	if code != http.StatusOK {
		t.Fatalf("status=%d resp=%v", code, resp)
	}
	if fileExists(f.file) {
		t.Error("★ 凭证文件还在 —— purge_file=1 必须连文件一起删，" +
			"否则重载 auths / 重启会把账号装回池子（用户实测的「删除不掉」）")
	}
	if got, _ := resp["purged_file"].(string); got == "" {
		t.Errorf("purged_file=%q 应当是被删掉的文件路径", got)
	}
	if _, bad := resp["purge_error"]; bad {
		t.Errorf("正常路径不该有 purge_error：%v", resp)
	}
	if _, ok := f.p.Status(delTestUID); ok {
		t.Error("账号应当已从池子里移除")
	}
}

// TestDeleteRecoversFilePathByScanningUpstream 池条目没带 FilePath 时，
// 用**上游自己的扫描器**找回路径再删 —— 这正是用户那条缺陷的兜底。
//
// 判据必须是"文件真的没了"，而不是"回执里有个字段"：
// 缺陷的形态恰恰是"回执说成功、文件还在"。
func TestDeleteRecoversFilePathByScanningUpstream(t *testing.T) {
	// 池条目 FilePath 为空（上游投影漏传的那个形态），但上游扫描器报得出来。
	f := newDeleteFixture(t, "", true)
	f.p.SyncToDirFor("codearts", []*auth.Auth{{UID: delTestUID}}) // 重新对齐成"不带路径的那份"

	if a := f.p.AuthByUID(delTestUID); a == nil || a.FilePath != "" {
		t.Fatalf("前提不满足：池条目上的 FilePath 应为空，实际 %+v", a)
	}

	code, resp := f.del(t, delTestUID, true)
	if code != http.StatusOK {
		t.Fatalf("status=%d resp=%v", code, resp)
	}
	if fileExists(f.file) {
		t.Errorf("★ 文件没被删掉 —— 兜底没生效。池条目 FilePath 为空时，"+
			"必须按上游扫描器（gateway.CredentialLoader）找回路径再删。resp=%v", resp)
	}
	if got, _ := resp["file"].(string); got != f.file {
		t.Errorf("file=%q want %q（回执里应给出找回后的路径，便于排查）", got, f.file)
	}
	if _, bad := resp["purge_error"]; bad {
		t.Errorf("兜底成功后不该有 purge_error：%v", resp)
	}
}

// TestDeleteReportsPurgeFailureWhenNoPathFound 连扫描都找不到路径时，
// **必须如实回执**，不能报成"已彻底删除"。
//
// 这条守的是"界面在说谎"：旧代码只写日志、回执里什么都没有，
// 前端于是 toast「已彻底删除」——而文件还在。
func TestDeleteReportsPurgeFailureWhenNoPathFound(t *testing.T) {
	// 池里没有路径，上游扫描器也不报路径（例如凭证不是文件形态）
	f := newDeleteFixture(t, "", false)
	f.p.SyncToDirFor("codearts", []*auth.Auth{{UID: delTestUID}})

	code, resp := f.del(t, delTestUID, true)
	if code != http.StatusOK {
		t.Fatalf("账号确实出池了 ⇒ 仍是 200，status=%d resp=%v", code, resp)
	}
	if _, ok := resp["purge_error"].(string); !ok {
		t.Errorf("★ 回执里必须有 purge_error（前端据此把「已彻底删除」换成如实提示），实际 %v", resp)
	}
	if fileExists(f.file) {
		// 这是**预期**的：找不到路径就不删。但界面必须说出来（上面那条断言）。
		t.Log("文件确实还在（路径未知 ⇒ 不删），关键是回执如实报告了")
	}
	if _, ok := f.p.Status(delTestUID); ok {
		t.Error("账号应当已从池子里移除")
	}
}

// TestDeleteRefusesFileOutsideCredentialTree 不许删凭证树之外的文件。
//
// 安全边界：FilePath 若被构造成任意路径（上游的 bug / 手工改过的 state.json），
// 这里必须拒绝，并把拒绝原因回给前端（而不是静默）。
func TestDeleteRefusesFileOutsideCredentialTree(t *testing.T) {
	f := newDeleteFixture(t, "", true)

	// 凭证树之外的"文件"（存在，但不该被删）
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.p.SyncToDirFor("codearts", []*auth.Auth{{UID: delTestUID, FilePath: outside}})

	code, resp := f.del(t, delTestUID, true)
	if code != http.StatusOK {
		t.Fatalf("status=%d resp=%v", code, resp)
	}
	if !fileExists(outside) {
		t.Error("★ 凭证树之外的文件被删了 —— 这是越界删除，必须拒绝")
	}
	if _, ok := resp["purge_error"].(string); !ok {
		t.Errorf("拒绝删除必须有 purge_error 说明，实际 %v", resp)
	}
}

// TestDeleteWithoutPurgeKeepsFile 不带 purge_file 时保持老语义：只出池、留文件。
//
// 老客户端（以及缓存里的旧页面）可能只发不带参数的 DELETE；
// 把它们变成"连文件一起删"是数据损失，不能顺手做。
func TestDeleteWithoutPurgeKeepsFile(t *testing.T) {
	f := newDeleteFixture(t, "", true)
	f.p.SyncToDirFor("codearts", []*auth.Auth{{UID: delTestUID, FilePath: f.file}})

	code, resp := f.del(t, delTestUID, false)
	if code != http.StatusOK {
		t.Fatalf("status=%d resp=%v", code, resp)
	}
	if !fileExists(f.file) {
		t.Error("不带 purge_file 时不该删文件（老语义）")
	}
	if _, bad := resp["purge_error"]; bad {
		t.Errorf("没要求 purge 就不该报 purge 失败：%v", resp)
	}
}

// TestDeleteUnknownUIDNotFound 不存在的 uid → 404（前端会显示成失败）。
func TestDeleteUnknownUIDNotFound(t *testing.T) {
	f := newDeleteFixture(t, "", true)
	code, resp := f.del(t, "no-such-uid", true)
	if code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 resp=%v", code, resp)
	}
}
