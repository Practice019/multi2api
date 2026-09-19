package clientlogin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试全部用 t.TempDir()：切换会真的改写磁盘，绝不能碰真实客户端目录。

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// clientDoc 生成一份形态与真实客户端一致的凭据 JSON。
func clientDoc(uid, nick, token string, expiresAtMS int64, extraTop map[string]string, extraAcct map[string]string) string {
	auth := map[string]any{
		"accessToken":      token,
		"expiresIn":        5184000,
		"refreshExpiresIn": 7776000,
		"refreshToken":     "refresh-" + uid,
		"tokenType":        "Bearer",
		"notBeforePolicy":  1724292326,
		"sessionState":     "sess-" + uid,
		"scope":            "profile offline_access email",
		"domain":           "copilot.tencent.com",
		"lastRefreshTime":  expiresAtMS - 5184000000,
		"expiresAt":        expiresAtMS,
		"refreshExpiresAt": expiresAtMS + 2592000000,
	}
	acct := map[string]any{
		"uid": uid, "nickname": nick, "uin": "330000000000", "type": "personal",
		"lastLogin": true, "pluginEnabled": true,
		"deployStatus": map[string]any{"statusCode": 0, "statusMsg": "", "detailMsg": ""},
		"sso":          map[string]any{"domain": "", "domainModifiedTimes": 0},
		"phoneNumber":  nick,
	}
	for k, v := range extraAcct {
		acct[k] = v
	}
	doc := map[string]any{"account": acct, "auth": auth, "accounts": []any{acct}, "allAccounts": []any{acct}}
	for k, v := range extraTop {
		doc[k] = v
	}
	raw, _ := json.MarshalIndent(doc, "", "  ")
	return string(raw)
}

func gatewayDoc(uid, nick, token string, expiresAtSec int64) string {
	raw, _ := json.MarshalIndent(map[string]any{
		"auth": map[string]any{
			"accessToken": token, "refreshToken": "gwrefresh-" + uid,
			"expiresAt": expiresAtSec, "domain": "copilot.tencent.com",
		},
		"account": map[string]any{"uid": uid, "nickname": nick, "enterpriseId": ""},
	}, "", "  ")
	return string(raw)
}

// newTestManager 搭好三个目录：客户端目录、账号池目录、存档目录 + 一个假的用户主目录。
func newTestManager(t *testing.T) (*Manager, string, string, string) {
	t.Helper()
	root := t.TempDir()
	clientDir := filepath.Join(root, "auth")
	gwDir := filepath.Join(root, "gw")
	archiveDir := filepath.Join(root, "data", "client-login")
	home := filepath.Join(root, "home")
	for _, d := range []string{clientDir, gwDir, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	m := New(clientDir, gwDir, archiveDir)
	m.SetHome(home)
	// 注入「客户端未运行」的探测器。
	// 不能依赖平台默认实现：开发机上客户端往往真的开着，
	// 那样这些测试会因为环境而红，掩盖真正的问题（这正是最初的失败原因）。
	m.SetProcessProbe(func() bool { return false })
	return m, clientDir, gwDir, home
}

// TestSwitchRefusedWhileClientRunning 客户端在跑时必须拒绝切换。
//
// 这是实测教训固化成测试：登录态活在客户端内存里，我们换掉磁盘文件后
// 它下一次刷 token 会把旧会话写回去 —— 现场表现是「切了一会儿又自己变回去」。
// 所以宁可直接拒绝，也不要让用户以为切成功了。
func TestSwitchRefusedWhileClientRunning(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	before := clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil)
	writeFile(t, filepath.Join(clientDir, ClientFileName), before)
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))

	m.SetProcessProbe(func() bool { return true })

	if _, err := m.Switch("uid-b"); !errors.Is(err, ErrClientRunning) {
		t.Fatalf("期望 ErrClientRunning，得到 %v", err)
	}
	// 被拒的操作不该留下任何痕迹：凭据没改、也没产生备份。
	if got, _ := os.ReadFile(filepath.Join(clientDir, ClientFileName)); string(got) != before {
		t.Error("被拒绝的切换改动了客户端凭据")
	}
	if _, err := os.Stat(m.lastBackupPath()); err == nil {
		t.Error("被拒绝的切换不应产生备份文件")
	}
}

// TestRestoreRefusedWhileClientRunning 回滚同样要拦（同一个覆盖风险）。
func TestRestoreRefusedWhileClientRunning(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))

	// 先在「未运行」状态下做一次切换，制造出可回滚的备份。
	if _, err := m.Switch("uid-b"); err != nil {
		t.Fatalf("准备切换失败: %v", err)
	}
	afterSwitch, _ := os.ReadFile(filepath.Join(clientDir, ClientFileName))

	// 再让客户端「跑起来」，回滚必须被拒且不改文件。
	m.SetProcessProbe(func() bool { return true })
	if _, err := m.Restore(); !errors.Is(err, ErrClientRunning) {
		t.Fatalf("期望 ErrClientRunning，得到 %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(clientDir, ClientFileName)); string(got) != string(afterSwitch) {
		t.Error("被拒绝的回滚改动了客户端凭据")
	}
}

// TestStatusReportsClientRunning 界面需要知道客户端在跑，才能把按钮置灰并说明原因。
func TestStatusReportsClientRunning(t *testing.T) {
	m, clientDir, _, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))

	st, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.ClientRunning {
		t.Error("注入未运行时 ClientRunning 应为 false")
	}

	m.SetProcessProbe(func() bool { return true })
	st2, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st2.ClientRunning {
		t.Error("注入运行中时 ClientRunning 应为 true")
	}
}

// TestParseKeepsUnknownKeys 是保真的核心：我们只认识 account/auth，
// 但客户端将来加的键不能在写回时被我们抹掉。
func TestParseKeepsUnknownKeys(t *testing.T) {
	m, clientDir, _, _ := newTestManager(t)
	raw := clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, map[string]string{"futureFlag": "keep-me"}, map[string]string{"newField": "also-keep"})
	writeFile(t, filepath.Join(clientDir, ClientFileName), raw)

	st, err := m.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Current == nil || st.Current.UID != "uid-a" {
		t.Fatalf("当前账号解析错误: %+v", st.Current)
	}
	cur, err := m.loadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cur.marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["futureFlag"]; !ok {
		t.Error("顶层未知键 futureFlag 被丢弃")
	}
	var acct map[string]json.RawMessage
	if err := json.Unmarshal(got["account"], &acct); err != nil {
		t.Fatal(err)
	}
	if _, ok := acct["newField"]; !ok {
		t.Error("account 块里的未知键 newField 被丢弃")
	}
	// accounts / allAccounts 必须仍然存在且是数组，否则客户端读不到账号列表。
	for _, k := range []string{"accounts", "allAccounts"} {
		var arr []json.RawMessage
		if err := json.Unmarshal(got[k], &arr); err != nil {
			t.Errorf("%s 不是数组: %v", k, err)
		} else if len(arr) != 1 {
			t.Errorf("%s 期望 1 条，得到 %d", k, len(arr))
		}
	}
}

// TestSyncArchiveKeepsBothAccounts 验证轮转备份被按 uid 归档：
// 这正是「客户端自己切过账号，我们就有那个账号的原生凭据」的依据。
func TestSyncArchiveKeepsBothAccounts(t *testing.T) {
	m, clientDir, _, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-b", "乙", "tok-b", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(clientDir, "workbuddy-desktop.2026-01-01T00-00-00-000Z.111.guid-a.info"),
		clientDoc("uid-a", "甲", "tok-a", 3_000_000_000_000, nil, nil))

	if err := m.syncArchive(); err != nil {
		t.Fatalf("syncArchive: %v", err)
	}
	for _, uid := range []string{"uid-a", "uid-b"} {
		if _, err := os.Stat(m.archivePath(uid)); err != nil {
			t.Errorf("%s 未被归档: %v", uid, err)
		}
	}
	// 再放一份 expiresAt 更小的同 uid 凭据，不应覆盖新的。
	writeFile(t, filepath.Join(clientDir, "workbuddy-desktop.2026-02-01T00-00-00-000Z.222.guid-a.info"),
		clientDoc("uid-a", "甲", "tok-a-old", 1_000_000_000_000, nil, nil))
	if err := m.syncArchive(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(m.archivePath("uid-a"))
	c, err := parseCredential(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.AccessToken != "tok-a" {
		t.Errorf("旧凭据覆盖了新的: 得到 %s", c.Auth.AccessToken)
	}
}

// TestSwitchPrefersClientArchive 验证切换优先用客户端原生凭据（字段完整）。
func TestSwitchPrefersClientArchive(t *testing.T) {
	m, clientDir, gwDir, home := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))
	// uid-b 的原生凭据先落进存档（模拟客户端曾经登录过 uid-b）。
	writeFile(t, m.archivePath("uid-b"), clientDoc("uid-b", "乙", "native-tok-b", 4_100_000_000_000, nil, map[string]string{"mpOpenId": "openid-b"}))

	res, err := m.Switch("uid-b")
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if res.Source != SourceClient {
		t.Errorf("应优先使用客户端原生凭据，得到 source=%s", res.Source)
	}
	cur, err := m.loadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if cur.Auth.AccessToken != "native-tok-b" {
		t.Errorf("token 应来自原生存档，得到 %s", cur.Auth.AccessToken)
	}
	if cur.Account.MpOpenID != "openid-b" {
		t.Error("原生凭据的账号专属字段 mpOpenId 未保留")
	}
	if !cur.Account.LastLogin {
		t.Error("切换后 lastLogin 应为 true")
	}
	// 账号指针必须跟着走。
	assertSnapshot(t, home, "uid-b", "乙")
}

// TestSwitchFallsBackToGateway 覆盖「账号池有、客户端从没登录过」这条路径，
// 也是本功能能对任意账号一键切换的关键。
func TestSwitchFallsBackToGateway(t *testing.T) {
	m, clientDir, gwDir, home := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, map[string]string{"stale": "x"}))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙同学", "gw-tok-b", 4_000_000_000))

	res, err := m.Switch("uid-b")
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if res.Source != SourceGateway {
		t.Errorf("应回退到账号池，得到 source=%s", res.Source)
	}
	cur, err := m.loadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if cur.Auth.AccessToken != "gw-tok-b" {
		t.Errorf("token 错误: %s", cur.Auth.AccessToken)
	}
	// 客户端专有字段必须补齐，否则客户端解析会缺字段。
	if cur.Auth.TokenType != "Bearer" || cur.Auth.Scope == "" || cur.Auth.NotBeforePolicy == 0 || cur.Auth.SessionState == "" {
		t.Errorf("客户端专有字段未补齐: %+v", cur.Auth)
	}
	if cur.Auth.ExpiresAt != 4_000_000_000_000 {
		t.Errorf("expiresAt 应换算成毫秒，得到 %d", cur.Auth.ExpiresAt)
	}
	if cur.Auth.RefreshExpiresAt <= cur.Auth.ExpiresAt {
		t.Error("refreshExpiresAt 应晚于 expiresAt")
	}
	if cur.Account.UID != "uid-b" || cur.Account.Nickname != "乙同学" {
		t.Errorf("账号信息错误: %+v", cur.Account)
	}
	if cur.Account.MpOpenID != "" {
		t.Error("不能把上一个账号的微信绑定继承过来")
	}
	if cur.Account.PhoneNumber != "乙同学" {
		t.Errorf("phoneNumber 兜底失败: %s", cur.Account.PhoneNumber)
	}
	assertSnapshot(t, home, "uid-b", "乙同学")
}

// TestSwitchBacksUpAndRestores 验证破坏性操作永远可回滚。
func TestSwitchBacksUpAndRestores(t *testing.T) {
	m, clientDir, gwDir, home := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))

	res, err := m.Switch("uid-b")
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupPath == "" {
		t.Error("切换应留下带时间戳的备份路径")
	}
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Errorf("备份文件不存在: %v", err)
	}
	st, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasBackup || st.BackupUID != "uid-a" || st.BackupNick != "甲" {
		t.Errorf("备份信息不完整: %+v", st)
	}

	back, err := m.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if back.UID != "uid-a" {
		t.Errorf("回滚到了错误的账号: %s", back.UID)
	}
	cur, err := m.loadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if cur.Auth.AccessToken != "tok-a" {
		t.Errorf("回滚后 token 错误: %s", cur.Auth.AccessToken)
	}
	assertSnapshot(t, home, "uid-a", "甲")
}

// TestSwitchRejectsSameAccount 保证重复点击不会做无谓写入。
func TestSwitchRejectsSameAccount(t *testing.T) {
	m, clientDir, _, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	if _, err := m.Switch("uid-a"); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("期望 ErrSameAccount，得到 %v", err)
	}
}

// TestSwitchUnknownAccount 目标在两边都找不到时必须明确报错，而不是写坏文件。
func TestSwitchUnknownAccount(t *testing.T) {
	m, clientDir, _, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	before, _ := os.ReadFile(filepath.Join(clientDir, ClientFileName))

	if _, err := m.Switch("uid-nope"); err == nil {
		t.Fatal("期望报错")
	}
	after, _ := os.ReadFile(filepath.Join(clientDir, ClientFileName))
	if string(before) != string(after) {
		t.Error("失败的切换不应改动客户端凭据")
	}
}

// TestDisabledWithoutArchiveDir 没有存档目录就没有回滚能力，必须拒绝切换。
func TestDisabledWithoutArchiveDir(t *testing.T) {
	root := t.TempDir()
	m := New(filepath.Join(root, "auth"), filepath.Join(root, "gw"), "")
	if m.Enabled() {
		t.Fatal("archiveDir 为空时应为未启用")
	}
	if _, err := m.Switch("uid-a"); err == nil {
		t.Fatal("未启用时切换必须失败")
	}
	st, err := m.Status()
	if err != nil {
		t.Fatalf("Status 不应报错: %v", err)
	}
	if st.Enabled || st.Error == "" {
		t.Errorf("应给出未启用的说明: %+v", st)
	}
}

// TestStatusOrdersCurrentFirst 让 UI 少一层判断：当前账号永远排第一。
func TestStatusOrdersCurrentFirst(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-m", "中间", "tok-m", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-a.json"), gatewayDoc("uid-a", "A", "gw-a", 4_000_000_000))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-z.json"), gatewayDoc("uid-z", "Z", "gw-z", 4_000_000_000))

	st, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Candidates) != 3 {
		t.Fatalf("期望 3 个候选，得到 %d: %+v", len(st.Candidates), st.Candidates)
	}
	if !st.Candidates[0].Current || st.Candidates[0].UID != "uid-m" {
		t.Errorf("当前账号应排第一: %+v", st.Candidates[0])
	}
	for _, c := range st.Candidates {
		if c.Current != (c.UID == "uid-m") {
			t.Errorf("Current 标记错误: %+v", c)
		}
	}
}

// TestSanitizeUIDBlocksTraversal 守住「uid 可能来自网络输入」这条线。
func TestSanitizeUIDBlocksTraversal(t *testing.T) {
	for _, in := range []string{"../../etc/passwd", `..\..\win.ini`, "a/b", ""} {
		got := sanitizeUID(in)
		if strings.ContainsAny(got, `/\`) || strings.Contains(got, "..") {
			t.Errorf("sanitizeUID(%q) = %q 仍含路径片段", in, got)
		}
	}
	if sanitizeUID("") != "unknown" {
		t.Errorf("空 uid 应回退为 unknown，得到 %q", sanitizeUID(""))
	}
}

// TestGatewayFallbackDoesNotLeakIdentity 守住一个真实修过的 bug：
// 早期实现整体继承当前 account 块，导致切过去的新账号带着旧账号的
// uin / phoneNumber / mpOpenId。这比不切换更糟 —— 用户会看到「谁都不是」的账号。
func TestGatewayFallbackDoesNotLeakIdentity(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName),
		clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil,
			map[string]string{"mpOpenId": "openid-of-a", "uin": "999999999999", "someClientOnlyField": "keep"}))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))

	if _, err := m.Switch("uid-b"); err != nil {
		t.Fatal(err)
	}
	cur, err := m.loadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if cur.Account.UID != "uid-b" {
		t.Errorf("uid 泄漏: %s", cur.Account.UID)
	}
	if cur.Account.MpOpenID != "" {
		t.Errorf("mpOpenId 泄漏了上一个账号的值: %s", cur.Account.MpOpenID)
	}
	if cur.Account.Uin != "" {
		t.Errorf("uin 泄漏了上一个账号的值: %s", cur.Account.Uin)
	}
	if cur.Account.PhoneNumber != "乙" {
		t.Errorf("phoneNumber 应为新账号昵称，得到 %q", cur.Account.PhoneNumber)
	}
	// 反方向：客户端自己写的、我们不认识的字段必须原样保留，
	// 否则一次切换就会抹掉客户端的外观/实验状态。
	var raw map[string]json.RawMessage
	out, err := cur.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	var acct map[string]json.RawMessage
	if err := json.Unmarshal(raw["account"], &acct); err != nil {
		t.Fatal(err)
	}
	if string(acct["someClientOnlyField"]) != `"keep"` {
		t.Errorf("客户端专有字段未保留: %s", acct["someClientOnlyField"])
	}
}

// TestFailedSwitchKeepsPreviousBackup 守住一个真实修过的顺序 bug：
// 早期实现先写备份再解析目标，于是一次失败的切换会覆盖掉上一次的有效备份 ——
// 用户本可回滚到 A，失败一次后只能回滚到 B，回滚目标被静默换掉。
// 还顺带产生「凭空出现的备份」（has_backup 由 false 变 true）。
func TestFailedSwitchKeepsPreviousBackup(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))

	// 先做一次成功的 A→B，此时备份应为 A。
	if _, err := m.Switch("uid-b"); err != nil {
		t.Fatalf("第一次切换失败: %v", err)
	}
	bkPath := m.lastBackupPath()
	bkBefore, err := os.ReadFile(bkPath)
	if err != nil {
		t.Fatalf("第一次切换后应有备份: %v", err)
	}

	// 再做一次注定失败的切换（目标不存在）。
	if _, err := m.Switch("uid-does-not-exist"); err == nil {
		t.Fatal("切到不存在的账号应失败")
	}

	bkAfter, err := os.ReadFile(bkPath)
	if err != nil {
		t.Fatalf("失败的切换不应删掉原备份: %v", err)
	}
	if string(bkBefore) != string(bkAfter) {
		t.Error("失败的切换覆盖了上一次的有效备份 —— 回滚目标被静默换掉")
	}
	// 当前登录态也不该被失败的操作改动。
	cur, _ := m.loadCurrent()
	if cur.Account.UID != "uid-b" {
		t.Errorf("失败的切换改动了当前登录态: %s", cur.Account.UID)
	}
	// 回滚必须仍然回到 A。
	res, err := m.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.UID != "uid-a" {
		t.Errorf("回滚目标应为 uid-a，得到 %s", res.UID)
	}
}

// TestFailedSwitchLeavesNoBackup 无备份时失败一次，不应凭空造出备份。
func TestFailedSwitchLeavesNoBackup(t *testing.T) {
	m, clientDir, _, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))

	if _, err := m.Switch("uid-nope"); err == nil {
		t.Fatal("应失败")
	}
	if _, err := os.Stat(m.lastBackupPath()); err == nil {
		t.Error("失败的切换不应产生备份文件")
	}
	st, err := m.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st.HasBackup {
		t.Error("失败的切换后 has_backup 不应为 true")
	}
	if _, err := m.Restore(); err == nil {
		t.Error("没有备份时 Restore 必须失败")
	}
}

// TestRestoreTwiceIsRejected 回滚是幂等的：第二次点回滚应明确报错而不是默默重写。
func TestRestoreTwiceIsRejected(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))

	if _, err := m.Switch("uid-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Restore(); err != nil {
		t.Fatalf("首次回滚应成功: %v", err)
	}
	if _, err := m.Restore(); !errors.Is(err, ErrAlreadyBackedUp) {
		t.Fatalf("第二次回滚应返回 ErrAlreadyBackedUp，得到 %v", err)
	}
}

// TestStatusHidesUselessBackup 备份与当前账号相同时，界面不应提供「回滚」——
// 那个操作只会返回 409。Status 与 Restore 的判定必须一致。
func TestStatusHidesUselessBackup(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))

	if _, err := m.Switch("uid-b"); err != nil {
		t.Fatal(err)
	}
	// 切到 B 后备份是 A，与当前 B 不同 => 可回滚。
	st, _ := m.Status()
	if !st.HasBackup || st.BackupUID != "uid-a" {
		t.Fatalf("切到 B 后应可回滚到 A: %+v", st)
	}
	// 回滚到 A 后，备份仍是 A，与当前相同 => 不可回滚。
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	st2, _ := m.Status()
	if st2.HasBackup {
		t.Errorf("备份与当前账号相同时 HasBackup 应为 false，得到 %+v", st2)
	}
	if st2.BackupUID != "" {
		t.Errorf("此时不应展示回滚目标，得到 %q", st2.BackupUID)
	}
}

// TestSwitchIgnoresCorruptBackup 让坏掉的轮转备份不至于卡死整个面板。
func TestSwitchIgnoresCorruptBackup(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName), clientDoc("uid-a", "甲", "tok-a", 4_000_000_000_000, nil, nil))
	writeFile(t, filepath.Join(clientDir, "workbuddy-desktop.broken.info"), "{ 这不是 JSON")
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-b.json"), gatewayDoc("uid-b", "乙", "gw-tok-b", 4_000_000_000))

	res, err := m.Switch("uid-b")
	if err != nil {
		t.Fatalf("坏备份不应阻断切换: %v", err)
	}
	if res.UID != "uid-b" {
		t.Errorf("切换目标错误: %+v", res)
	}
	cur, _ := m.loadCurrent()
	if cur.Account.UID != "uid-b" {
		t.Error("切换未生效")
	}
}

// assertSnapshot 断言客户端「当前账号」指针已同步。
func assertSnapshot(t *testing.T, home, uid, nick string) {
	t.Helper()
	p := filepath.Join(home, ".workbuddy", "storage", "skeleton", snapshotName)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 account-snapshot.json: %v", err)
	}
	var doc snapshotDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析 account-snapshot.json: %v", err)
	}
	if doc.Primary.UID != uid {
		t.Errorf("snapshot uid = %s，期望 %s", doc.Primary.UID, uid)
	}
	if doc.Primary.Nickname != nick {
		t.Errorf("snapshot nickname = %s，期望 %s", doc.Primary.Nickname, nick)
	}
	if doc.Primary.SavedAt == 0 {
		t.Error("snapshot savedAt 应被更新")
	}
}

// ---------------------------------------------------------------------------
// 海外版（WorkBuddy AI）渠道：文件 / 账号指针 / 切换 / 回滚
// ---------------------------------------------------------------------------

// intlGatewayDoc 生成海外版账号池凭证（domain + channel 均为 intl）。
func intlGatewayDoc(uid, nick, token string, expiresAtSec int64) string {
	raw, _ := json.MarshalIndent(map[string]any{
		"auth": map[string]any{
			"accessToken": token, "refreshToken": "gwrefresh-" + uid,
			"expiresAt": expiresAtSec, "domain": "www.workbuddy.ai", "channel": "intl",
		},
		"account": map[string]any{"uid": uid, "nickname": nick, "enterpriseId": ""},
	}, "", "  ")
	return string(raw)
}

// intlClientDoc 生成形态与海外版客户端一致的凭据（domain=www.workbuddy.ai）。
func intlClientDoc(uid, nick, token string, expiresAtMS int64) string {
	doc := clientDoc(uid, nick, token, expiresAtMS, nil, nil)
	// 覆盖 domain 为海外版（auth 块在 doc 里是字符串化的 map，这里直接替换文本最稳妥）。
	raw := strings.Replace(doc, `"domain": "copilot.tencent.com"`, `"domain": "www.workbuddy.ai"`, 1)
	return raw
}

// TestSwitchCNToIntlIntlFileAbsent（P0-1 回归）：只装 CN 客户端（intl 文件不存在）、
// 账号池有海外版账号时，切到 intl 必须成功——备份目标文件 ENOENT 不能中止切换。
func TestSwitchCNToIntlIntlFileAbsent(t *testing.T) {
	m, clientDir, gwDir, home := newTestManager(t)
	beforeCN := clientDoc("uid-cn", "国内", "tok-cn", 4_000_000_000_000, nil, nil)
	writeFile(t, filepath.Join(clientDir, ClientFileName), beforeCN)
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-intl.json"),
		intlGatewayDoc("uid-intl", "海外", "gw-tok-intl", 4_000_000_000))

	if _, err := m.Switch("uid-intl"); err != nil {
		t.Fatalf("CN 客户端切 intl 账号失败: %v", err)
	}
	// intl 文件被创建、CN 文件保持原样。
	intlRaw, err := os.ReadFile(filepath.Join(clientDir, ClientFileNameAI))
	if err != nil {
		t.Fatalf("intl 凭证文件未创建: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(clientDir, ClientFileName)); string(got) != beforeCN {
		t.Error("切换 intl 不应改动 CN 客户端文件")
	}
	cur, err := parseCredential(intlRaw, ClientFileNameAI)
	if err != nil || cur.Account.UID != "uid-intl" {
		t.Fatalf("intl 文件内容错误: uid=%v err=%v", cur.Account.UID, err)
	}
	// 账号指针写到 ~/.workbuddy-ai。
	aiSnap := filepath.Join(home, ".workbuddy-ai", "storage", "skeleton", snapshotName)
	if raw, err := os.ReadFile(aiSnap); err != nil || !strings.Contains(string(raw), "uid-intl") {
		t.Errorf("intl 账号指针未写入: err=%v raw=%s", err, raw)
	}
	// 首次切换该渠道没有旧登录态：不应产生备份，也不该报错。
	if _, err := os.Stat(m.lastBackupPath()); err == nil {
		t.Error("首次切换 intl 渠道不应产生 last.json 备份")
	}
}

// TestSwitchCNToIntlIntlFileExists（P0-1 正常路径）：intl 文件已存在时，
// 切换必须把旧 intl 登录态备份进 last.json（含渠道元数据）。
func TestSwitchCNToIntlIntlFileExists(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileName),
		clientDoc("uid-cn", "国内", "tok-cn", 4_000_000_000_000, nil, nil))
	oldIntl := intlClientDoc("uid-intl-old", "海外旧", "tok-intl-old", 3_000_000_000_000)
	writeFile(t, filepath.Join(clientDir, ClientFileNameAI), oldIntl)
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-intl.json"),
		intlGatewayDoc("uid-intl", "海外新", "gw-tok-intl", 4_000_000_000))

	if _, err := m.Switch("uid-intl"); err != nil {
		t.Fatalf("切换失败: %v", err)
	}
	// last.json 备份的是旧 intl 登录态。
	bk, err := os.ReadFile(m.lastBackupPath())
	if err != nil {
		t.Fatalf("last.json 未产生: %v", err)
	}
	oldCur, err := parseCredential(bk, "last.json")
	if err != nil || oldCur.Account.UID != "uid-intl-old" {
		t.Errorf("备份内容错误: uid=%v err=%v", oldCur.Account.UID, err)
	}
	// 渠道元数据落盘，供 Restore 按 intl 渠道回写。
	metaRaw, err := os.ReadFile(m.lastBackupMetaPath())
	if err != nil {
		t.Fatalf("last.meta.json 未产生: %v", err)
	}
	var meta struct {
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil || meta.Channel != "intl" {
		t.Errorf("渠道元数据错误: %s (err=%v)", metaRaw, err)
	}
}

// TestSwitchIntlSameAccount（P1-2 回归）：当前登录态是 intl 账号 X 时再切 X，
// 必须判为同账号（ErrSameAccount），不能重写文件谎报切换。
func TestSwitchIntlSameAccount(t *testing.T) {
	m, clientDir, gwDir, _ := newTestManager(t)
	writeFile(t, filepath.Join(clientDir, ClientFileNameAI),
		intlClientDoc("uid-intl", "海外", "tok-intl", 4_000_000_000_000))
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-intl.json"),
		intlGatewayDoc("uid-intl", "海外", "gw-tok-intl", 4_000_000_000))

	if _, err := m.Switch("uid-intl"); !errors.Is(err, ErrSameAccount) {
		t.Fatalf("期望 ErrSameAccount，得到 %v", err)
	}
}

// TestRestoreIntlChannel（P1-3）：切到 intl 后回滚，必须把备份写回 intl 文件，
// CN 客户端文件不受影响。
func TestRestoreIntlChannel(t *testing.T) {
	m, clientDir, gwDir, home := newTestManager(t)
	beforeCN := clientDoc("uid-cn", "国内", "tok-cn", 4_000_000_000_000, nil, nil)
	oldIntl := intlClientDoc("uid-intl-old", "海外旧", "tok-intl-old", 3_000_000_000_000)
	writeFile(t, filepath.Join(clientDir, ClientFileName), beforeCN)
	writeFile(t, filepath.Join(clientDir, ClientFileNameAI), oldIntl)
	writeFile(t, filepath.Join(gwDir, "workbuddy-uid-intl.json"),
		intlGatewayDoc("uid-intl", "海外新", "gw-tok-intl", 4_000_000_000))

	if _, err := m.Switch("uid-intl"); err != nil {
		t.Fatalf("切换失败: %v", err)
	}
	res, err := m.Restore()
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if res.UID != "uid-intl-old" {
		t.Errorf("回滚目标 uid=%s，期望 uid-intl-old", res.UID)
	}
	// intl 文件恢复为旧登录态。
	aiRaw, err := os.ReadFile(filepath.Join(clientDir, ClientFileNameAI))
	if err != nil {
		t.Fatalf("intl 文件丢失: %v", err)
	}
	cur, err := parseCredential(aiRaw, ClientFileNameAI)
	if err != nil || cur.Account.UID != "uid-intl-old" {
		t.Errorf("intl 文件未恢复: uid=%v err=%v", cur.Account.UID, err)
	}
	// CN 文件与 CN 账号指针不受影响（CN snapshot 从未创建过，回滚也不该创建）。
	if got, _ := os.ReadFile(filepath.Join(clientDir, ClientFileName)); string(got) != beforeCN {
		t.Error("回滚不应改动 CN 客户端文件")
	}
	if _, err := os.Stat(filepath.Join(home, ".workbuddy", "storage", "skeleton", snapshotName)); err == nil {
		t.Error("回滚不应创建 CN 账号指针（本次从未操作 CN 渠道）")
	}
}
