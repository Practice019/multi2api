// login_test.go —— 「页内添加账号」（本机拾取）的守卫。
//
// # 这里守的三条契约
//
//  1. **authURL 为空 = 不需要浏览器**。前端按它分叉（空则不渲染授权链接、
//     文案改成"正在从本机读取登录态"）。若某天有人"顺手"填一个看起来
//     合理的 URL，界面会多出一个点了没用的「打开授权页面」按钮 ——
//     而测试必须在那里变红。
//  2. **Poll 一次性**。同一个 state 不能被取两次，否则"添加账号"可以
//     无限重复落盘。
//  3. **落盘接口靠包装而非直接暴露 \*Auth**。包级函数不进方法集，
//     直接把 `*Auth` 交出去会让核心回 501「凭证结构尚未接入落盘」——
//     而账号其实已经读到了（一个"看起来像没接线"的失败）。
package loomy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/gateway"
)

// authStoreDir 造一个"客户端已登录"的假数据目录。
func authStoreDir(t *testing.T, uid, session, nickname, loggedInAt string) string {
	t.Helper()
	return writeStore(t,
		storeRecord(keyAuthSession,
			storeSessionJSON(session, uid, nickname, loggedInAt)))
}

// TestLoginStartReturnsEmptyAuthURL Start 必须报"不需要浏览器"。
//
// authURL 为空串是本实现与 LoginFlow 形状之间的**唯一**表达方式
// （没有别的字段能说"这次不用打开页面"）。所以它是协议的一部分，
// 不是"暂时没填"。
func TestLoginStartReturnsEmptyAuthURL(t *testing.T) {
	dir := authStoreDir(t, "260911173523492332", fixtureSession, "150****3411",
		"2026-09-11T09:35:22.593Z")
	p := NewWithConfig(Config{ClientDataDir: dir})

	state, authURL, err := p.Start()
	if err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if state == "" {
		t.Fatal("state 不能为空（Poll 靠它取回凭证）")
	}
	if authURL != "" {
		t.Errorf("★ authURL = %q，want 空串 —— "+
			"非空会让前端渲染出一个点了没用的「打开授权页面」按钮"+
			"（本机拾取没有授权页）", authURL)
	}
}

// TestLoginPollReturnsCredentialOnce Poll 立刻给凭证，且只给一次。
func TestLoginPollReturnsCredentialOnce(t *testing.T) {
	dir := authStoreDir(t, "260911173523492332", fixtureSession, "150****3411",
		"2026-09-11T09:35:22.593Z")
	p := NewWithConfig(Config{ClientDataDir: dir})

	state, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	cred, err := p.Poll(state)
	if err != nil {
		t.Fatalf("Poll 失败: %v（凭证在 Start 里就已经就绪，不该 pending）", err)
	}
	if cred.Provider != providerID {
		t.Errorf("Provider = %q，want %q（落盘目录按它选，错了会写进别的上游目录）",
			cred.Provider, providerID)
	}
	if cred.UID != "260911173523492332" {
		t.Errorf("UID = %q", cred.UID)
	}
	if cred.Nickname != "150****3411" {
		t.Errorf("Nickname = %q", cred.Nickname)
	}

	if _, err := p.Poll(state); err == nil {
		t.Error("同一个 state 被取了两次 —— 「添加账号」可以无限重复落盘")
	}
}

// TestLoginPollUnknownState Poll 一个不存在的 state 必须明确失败。
func TestLoginPollUnknownState(t *testing.T) {
	p := NewWithConfig(Config{ClientDataDir: t.TempDir()})
	if _, err := p.Poll("从未发起过的 state"); err == nil {
		t.Error("未知 state 应当返回错误（核心据此回 502，而不是 pending 让界面空等）")
	}
}

// TestLoginStartFallsBackToSMS 目录在、但本机没登录过 → 走手机号验证码那条路。
//
// # 本轮行为变更（这是**订正**，不是放宽断言）
//
// 上一轮这里断言"读不到登录态就必须报错"——那时本机拾取是**唯一**一条路，
// 报错是唯一诚实的回答。本轮加了短信登录，于是正确的回答变了：
//
//	本机拾取不成立 → 给出**表单页地址**（authURL 非空），而不是报错
//
// 判据仍然是"不能返回一个必然空等的 state"：返回表单页意味着**用户有下一步可做**；
// 而返回空 authURL + 永远 pending 才是"静默卡住"。
func TestLoginStartFallsBackToSMS(t *testing.T) {
	dir := t.TempDir() // 目录在，但没有任何数据文件
	p := NewWithConfig(Config{ClientDataDir: dir})

	if !p.Configured() {
		t.Fatal("前置条件：目录存在时 Configured 应当为 true（按钮要出现）")
	}
	state, authURL, err := p.Start()
	if err != nil {
		t.Fatalf("本机读不到登录态时应当回落到短信登录，而不是报错: %v", err)
	}
	if state == "" {
		t.Fatal("state 不能为空")
	}
	if authURL == "" {
		t.Fatal("★ 回落路径必须给出表单页地址（authURL 非空）—— " +
			"空 authURL 意味着前端只会轮询一个永远不会就绪的 state，那是静默卡住")
	}
	if !strings.Contains(authURL, state) {
		t.Errorf("表单页地址必须带上 state（否则页面无法把结果送回轮询）: %q", authURL)
	}
	// 此时轮询必须回 pending（而不是错误）—— 用户正在页面上操作。
	_, perr := p.Poll(state)
	if perr != gateway.ErrLoginPending {
		t.Errorf("等待用户输验证码期间 Poll 应当回 ErrLoginPending，得到 %v", perr)
	}
}

// TestLoginLocalModeRefusesSMS 显式 login_mode=local 时**不许**悄悄改走短信。
//
// 那会发一条用户没预期的短信（而且真的扣费/有风控），所以必须明确报错，
// 并在文案里说清怎么改。
func TestLoginLocalModeRefusesSMS(t *testing.T) {
	p := NewWithConfig(Config{ClientDataDir: t.TempDir(), LoginMode: "local"})
	if _, _, err := p.Start(); err == nil {
		t.Error("login_mode=local 且本机没有登录态时必须报错")
	} else if !strings.Contains(err.Error(), "login_mode") {
		t.Errorf("错误文案应当指出是 login_mode 的限制，实际: %v", err)
	}
}

// TestLoginConfiguredTrueViaSMS 本机没有客户端目录 → 仍然能加账号（走短信）。
//
// # 本轮行为变更
//
// 上一轮这里断言 Configured()==false（那时只有本机拾取一条路）。
// 现在短信登录的默认 key 是**内嵌**的，所以任何部署都能加账号 ——
// 按钮必须出现，否则用户根本没有入口去输手机号。
func TestLoginConfiguredTrueViaSMS(t *testing.T) {
	p := NewWithConfig(Config{
		ClientDataDir: filepath.Join(t.TempDir(), "并不存在的目录"),
	})
	if p.Configured() {
		return // 有客户端目录时也是 true，两种都合法
	}
	t.Error("本机没有客户端目录时，只要有短信登录就必须 Configured=true（否则用户没有入口）")
}

// TestLoginConfiguredFalseWhenNoClient 本机没有客户端 → 不给按钮。
//
// 这条走的是**自动探测**那条路（配置为空）。为了不依赖运行测试的机器上
// 有没有装 Loomy，这里用一个"探测必然失败"的方式：把配置指到空串
// 且断言在**探测结果为空**时 Configured 为 false。
// 若这台机器上真的装了 Loomy，这条断言会变成"探测到了" ——
// 那也是一种正确行为，所以用条件断言而不是硬断言。
func TestLoginConfiguredFalseWhenNoClient(t *testing.T) {
	p := NewWithConfig(Config{})
	dir, source, exists := p.clientStoreDirWithSource()
	if source == "missing" {
		if exists || p.Configured() {
			t.Error("自动探测不到时不该 Configured")
		}
		return
	}
	if !exists || !p.Configured() {
		t.Errorf("探测到了 %s（source=%s）却报告 Configured=false", dir, source)
	}
}

// TestLoginFlowInstanceIsCached LoginFlow() 必须缓存实例。
//
// ⚠ 这是 codearts 那边端到端实测抓到的 bug：每次新建会让 sessions map
// 各是一份空的，于是 start 存下的会话在 poll 里找不到。
// 本实现没有那个具体窗口（Poll 紧跟 Start），但同一形状不值得重踩。
func TestLoginFlowInstanceIsCached(t *testing.T) {
	p := NewWithConfig(Config{})
	f1, ok1 := p.LoginFlow()
	f2, ok2 := p.LoginFlow()
	if !ok1 || !ok2 {
		t.Fatal("LoginFlow() 应当总是返回 (flow,true)")
	}
	if f1 != f2 {
		t.Error("LoginFlow() 每次返回新实例 —— start 与 poll 会各拿到一份空 sessions")
	}
	// 核心走的是类型断言这条路，必须通。
	if _, ok := gateway.ExtOf[gateway.LoginFlow](p); !ok {
		t.Error("ExtOf[LoginFlow] 认不出 loomy —— manifest 里不会有 login 字段，" +
			"「添加账号」按钮永远不出现")
	}
}

// TestLoginFlowStartRespectsStoreUpdates Start 读的是**当时**的登录态。
//
// 客户端换了账号（同一 uid 的新记录）之后，下一次 Start 必须读到新的。
func TestLoginFlowStartRespectsStoreUpdates(t *testing.T) {
	dir := t.TempDir()
	// 先写一个旧账号
	writeFileIn(t, dir, "000003.log", storeRecord(keyAuthSession,
		storeSessionJSON("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "OLD", "100****0001",
			"2026-09-11T09:35:22.593Z")))
	p := NewWithConfig(Config{ClientDataDir: dir})

	state1, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	cred1, err := p.Poll(state1)
	if err != nil {
		t.Fatal(err)
	}
	if cred1.UID != "OLD" {
		t.Fatalf("第一次读到 %q，want OLD", cred1.UID)
	}

	// 客户端追加写入新账号（同一键的历史）
	appendFile(t, dir, "000003.log", storeRecord(keyAuthSession,
		storeSessionJSON("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "NEW", "100****0002",
			"2026-09-14T08:15:28.627Z")))

	state2, _, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	cred2, err := p.Poll(state2)
	if err != nil {
		t.Fatal(err)
	}
	if cred2.UID != "NEW" {
		t.Errorf("第二次读到 %q，want NEW —— 读的是启动时的快照而不是当时的存储", cred2.UID)
	}
}

// ---------------------------------------------------------------------------
// 落盘包装
// ---------------------------------------------------------------------------

// authFileWriterShape 与 core 的 pollViaFlow 要求的**同名同签名**窄接口。
//
// 这里重声明一份是刻意的：本包不许依赖 internal/admin（那正是
// "加新上游核心零改动"的反面），所以形状只能在这里复制一遍。
// 它与 core 那份的一致性由**端到端**保证（真点一次「添加账号」）。
type authFileWriterShape interface {
	MarshalAuthFile() (name string, raw []byte, err error)
}

// TestAuthFileMarshalName 落盘文件名与内容。
func TestAuthFileMarshalName(t *testing.T) {
	a := &Auth{Session: fixtureSession, UID: "260911173523492332"}
	name, raw, err := (&authFile{a: a}).MarshalAuthFile()
	if err != nil {
		t.Fatal(err)
	}
	if name != "loomy-260911173523492332.json" {
		t.Errorf("文件名 = %q，want loomy-<uid>.json", name)
	}
	back, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("落盘内容读不回来: %v", err)
	}
	if back.Session != fixtureSession || back.UID != a.UID {
		t.Errorf("落盘内容读回来不一致：%+v", back)
	}
}

// TestAuthFileWrapperIsRequired 钉住"必须包一层"这条事实。
//
// # 这是反向验证
//
// 如果 `*Auth` 自己就有 `MarshalAuthFile() (string,[]byte,error)` 方法，
// 那么 authFile 这层包装就是多余的 —— 有人会顺手删掉它。
// 断言它**没有**这个方法，就说明了包装存在的原因：
// 本包的 `MarshalAuthFile(a *Auth) ([]byte, error)` 是**包级函数**，
// 签名也不同，不进方法集。
func TestAuthFileWrapperIsRequired(t *testing.T) {
	if _, ok := any((*Auth)(nil)).(authFileWriterShape); ok {
		t.Error("*Auth 竟然实现了落盘接口 —— 那 authFile 包装层就是多余的，" +
			"可以删掉（但它现在不是多余的：包级函数不进方法集）")
	}
	var w authFileWriterShape = &authFile{a: &Auth{Session: fixtureSession, UID: "u"}}
	if w == nil {
		t.Fatal("authFile 必须实现落盘接口")
	}
}

// TestAuthFileMarshalRejectsNil 空凭证必须明确报错，而不是写出一个空文件。
func TestAuthFileMarshalRejectsNil(t *testing.T) {
	if _, _, err := (&authFile{}).MarshalAuthFile(); err == nil {
		t.Error("空凭证应当报错")
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func writeFileIn(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, dir, name, content string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}
