package account

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newSessionFixture 起一套账号库 + 会话库（同一临时目录），并建一个账号。
func newSessionFixture(t *testing.T) (*Store, *SessionStore, *User) {
	t.Helper()
	dir := t.TempDir()
	accounts, err := New(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("账号库: %v", err)
	}
	sessions, err := NewSessionStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("会话库: %v", err)
	}
	u, err := accounts.Create("tester", "tester-password-001", RoleMember)
	if err != nil {
		t.Fatalf("建号: %v", err)
	}
	return accounts, sessions, u
}

// rewind 白盒把某条会话的过期时间往前挪（模拟"时间过去了"）。
//
// # 为什么用白盒改写而不是注入时钟
//
// 给 Store 加一个 `nowFn` 接缝会让**每个**生产路径都多一层间接调用，
// 只为了测试里几处时间跳变。测试与实现同一个包，直接改私有字段更诚实：
// 改的就是真正参与判定的那个值，不存在"测试里走的是假时钟、
// 生产里走真时钟"这种两边不一致的可能。
//
// ⚠ 只改内存。要测"重启后过期会话不复活"，必须用 rewindPersisted ——
// 重启是从**文件**读的，不落盘的话盘上那条还没过期（第一版测试就栽在这，
// 报"过期会话复活"，其实是测试自己没把改动写下去）。
func rewind(t *testing.T, s *SessionStore, sid string, d time.Duration) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sid]
	if sess == nil {
		t.Fatalf("会话 %s 不存在，无法改时间", sid)
	}
	sess.ExpiresAt -= int64(d / time.Second)
	sess.LastActiveAt -= int64(d / time.Second)
	sess.HardExpiresAt -= int64(d / time.Second)
}

// rewindPersisted 同 rewind，但把改动一起落盘（供"重启"类测试用）。
func rewindPersisted(t *testing.T, s *SessionStore, sid string, d time.Duration) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sid]
	if sess == nil {
		t.Fatalf("会话 %s 不存在，无法改时间", sid)
	}
	sess.ExpiresAt -= int64(d / time.Second)
	sess.LastActiveAt -= int64(d / time.Second)
	sess.HardExpiresAt -= int64(d / time.Second)
	if err := s.saveLocked(); err != nil {
		t.Fatalf("落盘: %v", err)
	}
}

// TestIssueAndLookup 签发 → 校验（含 SID 猜错/空白的反例）。
func TestIssueAndLookup(t *testing.T) {
	accounts, sessions, u := newSessionFixture(t)

	sess, err := sessions.Issue(u, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(sess.SID) != 64 {
		t.Errorf("SID 应当是 32 字节十六进制（64 字符），得到 %d 字符", len(sess.SID))
	}
	if sess.AuthVersion != u.AuthVersion || sess.Version != 1 {
		t.Errorf("版本初值不对: %+v", sess)
	}
	if sess.ExpiresAt <= time.Now().Unix() {
		t.Error("新会话的过期时间应当在未来")
	}
	if sess.HardExpiresAt <= sess.ExpiresAt {
		t.Error("绝对上限应当不早于滑动窗口")
	}

	got, user, err := sessions.Lookup(sess.SID, accounts)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.SID != sess.SID || user.ID != u.ID {
		t.Errorf("Lookup 拿到的不是同一条会话/账号: %+v %+v", got, user)
	}

	for _, bad := range []string{"", "   ", "deadbeef", sess.SID + "x"} {
		if _, _, err := sessions.Lookup(bad, accounts); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("SID %q 应得 ErrSessionNotFound，得到 %v", bad, err)
		}
	}
}

// TestExpiredSessionIsRejectedAndPurged 过期会话被拒、且立刻从库里消失（不复活）。
func TestExpiredSessionIsRejectedAndPurged(t *testing.T) {
	accounts, sessions, u := newSessionFixture(t)
	sess, err := sessions.Issue(u, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	rewind(t, sessions, sess.SID, SessionTTL+time.Minute)

	if _, _, err := sessions.Lookup(sess.SID, accounts); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("过期会话应得 ErrSessionExpired，得到 %v", err)
	}
	if sessions.Count() != 0 {
		t.Errorf("过期会话应被清除，库里仍有 %d 条", sessions.Count())
	}
	// 再查一次仍是"不存在"（不能因为第一次已经删了就变成别的错误）
	if _, _, err := sessions.Lookup(sess.SID, accounts); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("清除后再查应得 ErrSessionNotFound，得到 %v", err)
	}
	// 重新登录仍然可以拿到新会话
	if _, err := sessions.Issue(u, "127.0.0.1", "probe/1.0"); err != nil {
		t.Errorf("过期后应能重新签发，得到 %v", err)
	}
}

// TestSlidingRenewalCappedByHardExpiry 滑动续期生效，但被绝对上限封顶。
func TestSlidingRenewalCappedByHardExpiry(t *testing.T) {
	accounts, sessions, u := newSessionFixture(t)
	sess, err := sessions.Issue(u, "127.0.0.1", "probe/1.0")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// 把窗口挪到只剩 1 分钟（但绝对上限仍在 30 天后）
	sessions.mu.Lock()
	sessions.sessions[sess.SID].ExpiresAt = time.Now().Unix() + 60
	sessions.mu.Unlock()

	renewed, _, err := sessions.Lookup(sess.SID, accounts)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if renewed.ExpiresAt <= time.Now().Unix()+int64(SessionTTL/time.Second)-5 {
		t.Errorf("滑动续期没生效: ExpiresAt=%d", renewed.ExpiresAt)
	}

	// 绝对上限封顶：把上限设到 2 分钟后，续期不得超过它
	sessions.mu.Lock()
	sessions.sessions[sess.SID].HardExpiresAt = time.Now().Unix() + 120
	sessions.sessions[sess.SID].ExpiresAt = time.Now().Unix() + 60
	sessions.mu.Unlock()
	capped, _, err := sessions.Lookup(sess.SID, accounts)
	if err != nil {
		t.Fatalf("Lookup(封顶): %v", err)
	}
	if capped.ExpiresAt > capped.HardExpiresAt {
		t.Errorf("★ 续期越过了绝对上限: ExpiresAt=%d Hard=%d", capped.ExpiresAt, capped.HardExpiresAt)
	}
}

// TestRevokeAndRevokeOthers 登出与"退出其它设备"。
func TestRevokeAndRevokeOthers(t *testing.T) {
	accounts, sessions, u := newSessionFixture(t)
	a, _ := sessions.Issue(u, "127.0.0.1", "probe/1.0")
	b, _ := sessions.Issue(u, "127.0.0.1", "probe/1.0")
	c, _ := sessions.Issue(u, "127.0.0.1", "probe/1.0")

	if n := sessions.RevokeOthers(u.ID, c.SID); n != 2 {
		t.Errorf("应撤销 2 条别的会话，得到 %d", n)
	}
	if _, _, err := sessions.Lookup(c.SID, accounts); err != nil {
		t.Errorf("保留的那条应当仍有效，得到 %v", err)
	}
	for _, sid := range []string{a.SID, b.SID} {
		if _, _, err := sessions.Lookup(sid, accounts); !errors.Is(err, ErrSessionNotFound) {
			t.Errorf("被踢的会话应得 ErrSessionNotFound，得到 %v", err)
		}
	}
	if !sessions.Revoke(c.SID) {
		t.Error("撤销已存在的会话应返回 true")
	}
	if sessions.Revoke(c.SID) {
		t.Error("重复撤销应返回 false（幂等）")
	}
	if sessions.Revoke("") {
		t.Error("撤销空 SID 应返回 false")
	}
}

// TestSetPasswordKeepsCurrentSessionAndKillsOthers ★ 用户报障 ② 的正解。
func TestSetPasswordKeepsCurrentSessionAndKillsOthers(t *testing.T) {
	accounts, sessions, u := newSessionFixture(t)
	mine, _ := sessions.Issue(u, "127.0.0.1", "本机浏览器")
	other, _ := sessions.Issue(u, "192.168.1.9", "别的设备")

	// 改口令：账号 AuthVersion +1
	next, err := accounts.SetPassword(u.ID, "brand-new-password-002")
	if err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	// 未升级的会话立刻失效（包括自己那条）
	if _, _, err := sessions.Lookup(mine.SID, accounts); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("改口令后未升级的会话应失效，得到 %v", err)
	}
	if _, _, err := sessions.Lookup(other.SID, accounts); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("别的设备的会话也应失效，得到 %v", err)
	}

	// ★ 就地升级当前会话 ⇒ 自己不掉线（这就是"轮换/改密后不用重启"）
	if _, err := sessions.AdvanceAuthVersion(mine.SID, next); err != nil {
		t.Fatalf("AdvanceAuthVersion: %v", err)
	}
	got, _, err := sessions.Lookup(mine.SID, accounts)
	if err != nil {
		t.Fatalf("★ 就地升级后自己的会话应当仍有效，得到 %v", err)
	}
	if got.AuthVersion != next || got.Version != 2 {
		t.Errorf("版本号应升到 (%d, 2)，得到 (%d, %d)", next, got.AuthVersion, got.Version)
	}
	// 别的设备仍然失效（改口令就该把别人踢掉）
	if _, _, err := sessions.Lookup(other.SID, accounts); !errors.Is(err, ErrSessionRevoked) {
		t.Errorf("别的设备不该被升级，得到 %v", err)
	}
	// 版本不许回退
	if _, err := sessions.AdvanceAuthVersion(mine.SID, next-1); err == nil {
		t.Error("★ 会话版本回退应当被拒绝")
	}
}

// TestSessionLimits 同时在线数量与签发频率两条上限。
func TestSessionLimits(t *testing.T) {
	accounts, sessions, u := newSessionFixture(t)
	var last *Session
	for i := 0; i < MaxSessionsPerUser+1; i++ {
		sess, err := sessions.Issue(u, "127.0.0.1", "probe/1.0")
		if err != nil {
			t.Fatalf("第 %d 次签发失败: %v", i+1, err)
		}
		last = sess
		time.Sleep(2 * time.Millisecond) // 让 LastActiveAt 拉开，踢"最久没用"的
	}
	if sessions.Count() != MaxSessionsPerUser {
		t.Errorf("同时在线应被限制在 %d 条，得到 %d", MaxSessionsPerUser, sessions.Count())
	}
	if _, _, err := sessions.Lookup(last.SID, accounts); err != nil {
		t.Errorf("最新签发的那条不该被踢，得到 %v", err)
	}

	// 签发频率：继续签，直到超过窗口上限
	issued := MaxSessionsPerUser + 1
	var limitHit error
	for i := 0; i < maxIssuedPerWindow; i++ {
		if _, err := sessions.Issue(u, "127.0.0.1", "probe/1.0"); err != nil {
			limitHit = err
			break
		}
		issued++
	}
	if !errors.Is(limitHit, ErrSessionLimit) {
		t.Errorf("★ 签发频率应当受限（已签发 %d 次），得到 %v", issued, limitHit)
	}
}

// TestDisabledAndDeletedAccountSessions 禁用/删号后旧会话立刻不可用。
func TestDisabledAndDeletedAccountSessions(t *testing.T) {
	accounts, sessions, u := newSessionFixture(t)
	sess, _ := sessions.Issue(u, "127.0.0.1", "probe/1.0")

	if err := accounts.SetEnabled(u.ID, false); err != nil {
		t.Fatalf("禁用: %v", err)
	}
	if _, _, err := sessions.Lookup(sess.SID, accounts); !errors.Is(err, ErrDisabled) {
		t.Errorf("禁用账号的会话应得 ErrDisabled，得到 %v", err)
	}
	if err := accounts.SetEnabled(u.ID, true); err != nil {
		t.Fatalf("启用: %v", err)
	}
	if _, _, err := sessions.Lookup(sess.SID, accounts); err != nil {
		t.Errorf("重新启用后会话应当恢复可用（禁用不等于撤销），得到 %v", err)
	}

	// 删号（白盒：我们没提供 Delete，只有禁用；这里模拟"账号没了"）
	accounts.mu.Lock()
	delete(accounts.users, normalizeUsername(u.Username))
	accounts.mu.Unlock()
	if _, _, err := sessions.Lookup(sess.SID, accounts); !errors.Is(err, ErrSessionRevoked) {
		t.Errorf("账号不存在时应作废会话，得到 %v", err)
	}
	if sessions.Count() != 0 {
		t.Error("账号没了之后那条会话应当已被清掉")
	}
}

// TestSessionsSurviveRestart 会话跨重启保留；加载时顺手丢掉过期的。
func TestSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	accountPath := filepath.Join(dir, "accounts.json")
	sessionPath := filepath.Join(dir, "sessions.json")
	accounts, err := New(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := NewSessionStore(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	u, err := accounts.Create("keeper", "keeper-password-003", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	keep, _ := sessions.Issue(u, "127.0.0.1", "probe/1.0")
	drop, _ := sessions.Issue(u, "127.0.0.1", "probe/1.0")
	rewindPersisted(t, sessions, drop.SID, SessionTTL+time.Hour) // 这条已过期（且已落盘）

	// 重开（模拟重启）
	accounts2, err := New(accountPath)
	if err != nil {
		t.Fatal(err)
	}
	sessions2, err := NewSessionStore(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if sessions2.Count() != 1 {
		t.Errorf("重启后应只剩 1 条有效会话，得到 %d", sessions2.Count())
	}
	if _, _, err := sessions2.Lookup(keep.SID, accounts2); err != nil {
		t.Errorf("重启后保留的会话应当仍可用，得到 %v", err)
	}
	if _, _, err := sessions2.Lookup(drop.SID, accounts2); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("过期会话不该复活，得到 %v", err)
	}
}

// TestSessionViewsAndCleanup 会话列表脱敏 + 清理计数。
func TestSessionViewsAndCleanup(t *testing.T) {
	_, sessions, u := newSessionFixture(t)
	keep, _ := sessions.Issue(u, "10.0.0.1", "browser/1")
	drop, _ := sessions.Issue(u, "10.0.0.2", "browser/2")
	rewind(t, sessions, drop.SID, SessionTTL+time.Hour)

	views := sessions.ListForUser(u.ID, keep.SID)
	if len(views) != 1 {
		t.Fatalf("应当只剩 1 条有效会话，得到 %d", len(views))
	}
	if views[0].SID == keep.SID {
		t.Error("★ 会话列表里不能出现完整 SID（那是登录凭据）")
	}
	if !views[0].Current {
		t.Error("当前会话应当被标记")
	}
	if views[0].IP != "10.0.0.1" {
		t.Errorf("IP 应保留，得到 %q", views[0].IP)
	}
	// ListForUser 已经忽略了过期那条（列表只看有效会话）；
	// Cleanup 的作用是把**内存与文件里**的过期记录真正删掉 ⇒ 应当清掉 1 条。
	if n := sessions.Cleanup(); n != 1 {
		t.Errorf("Cleanup 应清掉 1 条过期会话，得到 %d", n)
	}
	if sessions.Count() != 1 {
		t.Errorf("清理后应剩 1 条，得到 %d", sessions.Count())
	}
	if n := sessions.Cleanup(); n != 0 {
		t.Errorf("再清理应当无事可做，得到 %d", n)
	}
}
