package codearts

// 续期失败的**窄自愈**测试。
//
// # 现场（这个文件要覆盖的缺陷）
//
// refresh_token 是**一次性消费**的：用一次即作废，服务端返 400 且响应体含
// `"error_code":"STS5.1806"` / `"the refresh token has been used"`。
//
// 而"内存里那份已被消费、磁盘上有一份更新的未用 token"是**已知会发生的状态**
// （Task 007 修的就是产生它的那个 bug）。修复前 `RefreshToken` 只会把错误
// 原样返回 —— 只能靠重启网关恢复。
//
// # 这些用例为什么能证伪
//
// 自愈的触发判据必须**窄**：只有服务端明确说"这个 token 已被用过"才进；
// 别的 400（client_id 错、DPoP 证明无效）不得读盘重试。
// 并且只重试**一次**，不得循环。
// 三条边界各有一个用例，删掉自愈分支时第一个用例必红。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefreshSelfHealsFromDiskWhenMemoryTokenConsumed
// 自愈生效：内存是已消费的 RT-consumed、磁盘是未用过的 RT-disk
// → RefreshToken 成功返回 nil，对象字段更新，磁盘落盘。
func TestRefreshSelfHealsFromDiskWhenMemoryTokenConsumed(t *testing.T) {
	srv, st := newRefreshServer(t)
	dir := t.TempDir()
	a := newTestAuth(t, dir)

	// 让 "RT-consumed" 在服务端变成"已用过"。
	consumedRT := "RT-consumed-" + randHex(4)
	if code, _ := postRefreshForm(t, srv.URL, consumedRT); code != http.StatusOK {
		t.Fatalf("预热消费失败: HTTP %d", code)
	}

	// 磁盘上放一份**更新且未被用过**的 token（模拟"磁盘比内存新"的现场）。
	diskRT := "RT-disk-" + randHex(4)
	a.RefreshToken = diskRT
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	// 内存里退回那份已被消费的（磁盘保持不变）。
	a.RefreshToken = consumedRT

	c := New()
	c.STSBase = srv.URL
	baseReq, baseRej := st.reqCount(), st.rejCount()

	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("内存 token 已消费、磁盘有更新 token 时应自愈成功，实际: %v", err)
	}

	// 断言：确实发生了"一次被拒 + 一次成功重试"。
	if got := st.reqCount() - baseReq; got != 2 {
		t.Errorf("自愈应恰好发 2 次请求（首次 + 重试 1 次），实际 %d", got)
	}
	if got := st.rejCount() - baseRej; got != 1 {
		t.Errorf("应恰好被拒 1 次（内存那份已消费），实际 %d", got)
	}

	// 断言：对象字段被更新。
	if a.AccessKey != "AK-new" || a.SecretKey != "SK-new" || a.SecurityToken != "ST-new" {
		t.Errorf("自愈后凭证字段未更新: ak=%q sk=%q st=%q",
			a.AccessKey, a.SecretKey, a.SecurityToken)
	}
	if a.RefreshToken == consumedRT || a.RefreshToken == diskRT || a.RefreshToken == "" {
		t.Errorf("refresh_token 应已轮换为服务端新发的那份，实际 %q", a.RefreshToken)
	}
	if a.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expiresAt 未更新: %d", a.ExpiresAt)
	}

	// 断言：磁盘已落盘（新凭证 + 新 refresh_token）。
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("自愈后磁盘凭证无法解析: %v", err)
	}
	if back.AccessKey != "AK-new" || back.RefreshToken != a.RefreshToken {
		t.Errorf("磁盘未落盘: ak=%q rt=%q（内存 ak=%q rt=%q）",
			back.AccessKey, back.RefreshToken, a.AccessKey, a.RefreshToken)
	}
}

// TestRefreshDoesNotSelfHealWhenDiskTokenMatchesMemory
// 不误触发：磁盘 token 与内存相同时，仍只发 1 次请求并返回原错误。
func TestRefreshDoesNotSelfHealWhenDiskTokenMatchesMemory(t *testing.T) {
	srv, st := newRefreshServer(t)
	dir := t.TempDir()
	a := newTestAuth(t, dir) // 磁盘与内存都是同一个 RT-initial-xxxx

	// 让这份（内存 == 磁盘）的 token 在服务端变成已用过。
	rt := a.RefreshToken
	if code, _ := postRefreshForm(t, srv.URL, rt); code != http.StatusOK {
		t.Fatalf("预热消费失败: HTTP %d", code)
	}

	c := New()
	c.STSBase = srv.URL
	baseReq := st.reqCount()

	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("磁盘 token 与内存相同，无从自愈，应当返回错误")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("磁盘 token 相同时应当**原样返回原错误**（含 http 400），实际: %v", err)
	}
	if got := st.reqCount() - baseReq; got != 1 {
		t.Errorf("磁盘 token 与内存相同时不得重试：发了 %d 次请求，期望 1 次", got)
	}
	if a.AccessKey != "AK-old" || a.RefreshToken != rt {
		t.Errorf("失败路径不得改动凭证: ak=%q rt=%q", a.AccessKey, a.RefreshToken)
	}
}

// TestRefreshDoesNotSelfHealOnUnrelatedBadRequest
// 判据必须窄：与"token 已被用过"无关的 400 **不得**触发读盘重试。
//
// 这里把假上游造成"第 1 次 400、之后一律 200"：
// 只要实现判据过宽（例如只看 status>=400），就会重试并"成功"，
// 本用例因此变红。
func TestRefreshDoesNotSelfHealOnUnrelatedBadRequest(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error_code":"STS5.1801",` +
				`"error_msg":"invalid client_id or client secret"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credentials":{"access_key_id":"AK-new",` +
			`"secret_access_key":"SK-new","security_token":"ST-new",` +
			`"expiration":"2099-01-01T00:00:00Z"},"refresh_token":"RT-should-not-be-issued"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	a := newTestAuth(t, dir)
	// 磁盘上放一份不同的、未用过的 token —— 判据过宽的话它就会被采用。
	diskRT := "RT-disk-" + randHex(4)
	a.RefreshToken = diskRT
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	memRT := "RT-memory-" + randHex(4)
	a.RefreshToken = memRT

	c := New()
	c.STSBase = srv.URL

	if err := c.RefreshToken(a); err == nil {
		t.Fatal("client_id 类的 400 不得触发自愈（这里自愈了 = 判据过宽）")
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("无关的 400 不得读盘重试：发了 %d 次请求，期望 1 次", got)
	}
	if a.RefreshToken != memRT {
		t.Errorf("无关的 400 不得采用磁盘凭证: rt=%q 期望 %q", a.RefreshToken, memRT)
	}
}

// TestRefreshSelfHealRetriesOnlyOnce
// 自愈只重试一次：内存与磁盘两份 token **都已被消费**时，
// 恰好发 1 + 1 = 2 次请求后返回错误，不得循环。
func TestRefreshSelfHealRetriesOnlyOnce(t *testing.T) {
	srv, st := newRefreshServer(t)
	dir := t.TempDir()
	a := newTestAuth(t, dir)

	memRT := "RT-consumed-mem-" + randHex(4)
	diskRT := "RT-consumed-disk-" + randHex(4)
	for _, rt := range []string{memRT, diskRT} {
		if code, _ := postRefreshForm(t, srv.URL, rt); code != http.StatusOK {
			t.Fatalf("预热 %s 失败: HTTP %d", rt, code)
		}
	}

	a.RefreshToken = diskRT
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	a.RefreshToken = memRT

	c := New()
	c.STSBase = srv.URL
	baseReq := st.reqCount()

	if err := c.RefreshToken(a); err == nil {
		t.Fatal("两份 token 都已被消费，应当报错")
	}
	if got := st.reqCount() - baseReq; got != 2 {
		t.Errorf("自愈只允许重试一次：共发 %d 次请求，期望 2 次（首次 + 重试 1 次）", got)
	}
}
