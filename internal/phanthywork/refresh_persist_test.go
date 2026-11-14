package phanthywork

// refresh_persist_test.go 续期轮换的**落盘**判据。
//
// # 为什么这个文件必须存在
//
// 本上游的 refresh_token 是**轮换**的（RFC 6749 §6：服务端发了新的，客户端
// **必须**丢弃旧的）。这意味着「落盘」不是可选项，而是续期链条的一环：
//
//	落盘成功 → 下一次拿新的去换，继续活
//	落盘失败 → 下一次拿**已作废的旧 token** 去换 ⇒ 上游回 invalid_grant
//	          ⇒ 账号被判终态「需重新登录」，而故障其实发生在**上一次**续期里
//
// 而 2026-11-14 之前，这个包**没有任何判据检查过磁盘**：
// 四条既有断言（jobs_test.go:175/266/336、provider_test.go:273）都只断言
// **内存里那个 `*Auth`** 被更新了 —— 而它们的夹具（`authWithRemaining` →
// `newTestAuth`）恰好 `FilePath` 为空，于是 `saveAuthFile` 直接
// `return nil` 静默跳过。**判据全绿，磁盘上一片空白。**
//
// 这正是"自动续期看着正常、隔一个令牌寿命就死"最可能的形态，所以必须单独钉住。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestRefreshRotationLandsOnDisk 上游轮换 refresh_token ⇒ **磁盘上必须是新值**。
//
// 这一条与 jest_test 里那几条"内存被更新"的判据是互补的，不是重复：
// 内存更新只保证**本次**能用；磁盘更新才保证**下一次**还能续期。
func TestRefreshRotationLandsOnDisk(t *testing.T) {
	dir := t.TempDir()

	// 造一份**与生产同形**的凭证：FilePath 有值（生产由 LoadDir 设置，
	// 见 credential.go 的 a.FilePath = p），且文件里是**旧** refresh_token。
	a := authWithRemaining("u-disk", "rt-OLD", 800)
	a.FilePath = filepath.Join(dir, FileName(a))
	if err := saveAuthFile(a); err != nil {
		t.Fatalf("前置落盘失败: %v", err)
	}
	before, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("前置读回失败: %v", err)
	}
	if !strings.Contains(string(before), "rt-OLD") {
		t.Fatalf("前置不成立：磁盘上应是旧值 rt-OLD")
	}

	// 假上游的 /oauth/token 回的是 refresh_token="refresh-rotated"（见 upstream_test.go）。
	srv, fake := startFakeUpstream(t)
	p := NewWithClient(NewWithBase(srv.URL), dir, 0)
	wireAccounts(p, a)

	if err := p.RefreshCredential(gateway.Credential{
		Provider: providerID, UID: a.UID(), Secret: a,
	}); err != nil {
		t.Fatalf("续期应成功: %v", err)
	}
	if n := fake.pathCount(pathOAuthToken); n != 1 {
		t.Fatalf("应恰好打 1 次续期，实际 %d", n)
	}

	// ① 内存：本次可用
	if got := a.OAuth.RefreshToken; got != "refresh-rotated" {
		t.Fatalf("内存里的 refreshToken 未更新：%q", got)
	}

	// ② ★ 磁盘：下一次可用 —— 这一条是以前**完全缺失**的那一半
	after, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("读回落盘文件失败: %v", err)
	}
	if !strings.Contains(string(after), "refresh-rotated") {
		t.Fatalf("上游轮换了 refresh_token，但**磁盘上仍是旧值**：\n"+
			"  文件: %s\n"+
			"  内容片段: %s\n"+
			"后果：下一次续期会拿**已作废的旧 token** 去换 ⇒ 上游回 invalid_grant\n"+
			"      ⇒ 账号被判终态「需重新登录」，而真正的故障点是这次落盘没写进去。\n"+
			"      （本上游的 refresh_token 是轮换的，落盘不是可选项）",
			a.FilePath, brief(after))
	}
}

// TestRefreshWithoutFilePathIsLoud 轮换了却**没有落盘路径**时必须报错，不能静默丢。
//
// # 这条判据挡的是什么
//
// `saveAuthFile` 在 `FilePath == ""` 时 `return nil`（"凭据不是文件形态"）。
// 对只需要内存凭证的调用方这是对的；但一旦**上游轮换了 refresh_token**，
// 静默丢弃就等于：本次看起来成功，下一次必然 invalid_grant —— 而且**没有任何线索**。
//
// 数据丢失必须响。所以 applyTokenSet 在"轮换了但落不了盘"时返回错误。
func TestRefreshWithoutFilePathIsLoud(t *testing.T) {
	a := authWithRemaining("u-nofile", "rt-OLD", 800)
	a.FilePath = "" // 明确：没有落盘路径

	srv, _ := startFakeUpstream(t)
	p := NewWithClient(NewWithBase(srv.URL), t.TempDir(), 0)
	wireAccounts(p, a)

	err := p.RefreshCredential(gateway.Credential{
		Provider: providerID, UID: a.UID(), Secret: a,
	})
	if err == nil {
		t.Fatal("上游轮换了 refreshToken 而这份凭证没有落盘路径 —— " +
			"此时必须报错；静默成功会让下一次续期必然 invalid_grant，且无任何线索")
	}
	if !strings.Contains(err.Error(), "落盘") {
		t.Errorf("错误文本应说明是「落盘」问题（便于区分上游故障与本地缺陷）：%v", err)
	}
}
