// login.go 把「登录」适配成 gateway.LoginFlow —— 本包现在有**两条**登录路径。
//
// # 两条路径
//
//	① 本机拾取     读本机 Loomy 客户端已登录的 session（无需任何交互）
//	② 手机号验证码  向讯飞账号网关发短信、验码、换 session（见 smslogin.go）
//
// 优先走 ①：客户端已经登录过就不该再让用户输一次验证码（而且 ② 会**真的发短信**）。
// ① 不可用时（客户端没装 / 没登录 / 网关不在同一台机器）才走 ②。
//
// # 怎么把这个交互塞进 LoginFlow 那个形状里
//
// `LoginFlow` 只有 Start / Poll 两个动作，核心不许认识上游名，所以不能给它加
// "loomy 专用分支"。映射方式是：
//
//	Start()  → 试 ①；成功就**立刻**把凭证准备好并返回 authURL=""（不需要浏览器）
//	           失败就建一个 state，返回该 state 的**表单页地址**当 authURL
//	Poll()   → 凭证就绪就返回；否则 ErrLoginPending（核心回 202）
//	表单页   → 网关自己提供的同源页面，两个 POST 路由：
//	             /admin/loomy/login/sms     发验证码
//	             /admin/loomy/login/verify  验码 → 凭证就绪
//
// 这样**前端的 `addAccount` 一行都不用改**：它本来就支持两种形态 ——
// `auth_url` 非空就渲染「打开授权页面」并轮询，为空就只轮询。
// 打开的那个"授权页面"这次是我们自己的表单页。
package loomy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// loginSessionTTL 一次进行中的登录会话保留多久。
//
// 短信登录是**人来操作**的（收短信、敲 6 位码），所以 TTL 要比"本机拾取"
// 那种瞬时流程宽得多。取 10 分钟：与账号网关自己给验证码的 300 秒有效期
// 同一量级，再留够重发一次的余量。
const loginSessionTTL = 10 * time.Minute

// loginPagePath 表单页路径。state 作为 query 传进去。
const loginPagePath = "/admin/loomy/login"

// loginFlow 实现 gateway.LoginFlow。
type loginFlow struct {
	p *Provider

	mu       sync.Mutex
	sessions map[string]*loginEntry
}

// loginEntry 一次进行中的登录。
//
// 同一份结构同时服务两条路径：`ready` 为真表示凭证已就绪（本机拾取直接成功，
// 或短信验码已完成），`phone`/`msgid` 是短信那条路的中间状态。
type loginEntry struct {
	at    time.Time
	ready bool
	cred  gateway.Credential
	// phone / msgid 短信验证码那条路的中间状态（发过码之后才有）。
	phone string
	msgid string
}

var _ gateway.LoginFlow = (*loginFlow)(nil)

// LoginFlow 返回本上游的登录流程。
//
// ⚠ 必须**缓存**实例（见 codearts 那边的同款注释：每次新建会让 sessions
// map 各是一份空的，于是 start 存下的会话在 poll 里找不到）。这里尤其重要：
// 表单页的两个 POST 与轮询必须用**同一个**实例，否则验码结果永远送不到轮询。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil {
		return nil, false
	}
	p.loginOnce.Do(func() {
		p.loginCached = &loginFlow{p: p, sessions: make(map[string]*loginEntry)}
	})
	return p.loginCached, true
}

// Start 让 *Provider 满足 gateway.LoginFlow。
func (p *Provider) Start() (string, string, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("loomy: 未配置登录流程")
	}
	return f.Start()
}

// Poll 让 *Provider 满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("loomy: 未配置登录流程")
	}
	return f.Poll(state)
}

// Configured 报告这份部署**真的能**添加账号。
//
// 判据是"两条路径里至少有一条通"：
//
//	本机有客户端数据目录  → 本机拾取可用
//	短信客户端已配置      → 手机号验证码可用（默认 key 内嵌，所以恒为真）
//
// 本轮之前这里只判第一条，于是"网关跑在服务器上"时按钮根本不出现；
// 有了短信登录之后**任何部署都能加账号** —— 那正是用户提这条要求的目的。
func (p *Provider) Configured() bool {
	if p == nil {
		return false
	}
	if _, _, exists := p.clientStoreDirWithSource(); exists {
		return true
	}
	return p.smsClient().Configured()
}

// Configured 报告这份部署真的能走登录流程（*loginFlow 视角）。
func (f *loginFlow) Configured() bool {
	return f != nil && f.p != nil && f.p.Configured()
}

// Start 建一次登录：先试本机拾取，不行就给出短信表单页。
//
// 返回 (state, authURL, error)：
//
//	本机拾取成功 → authURL 为空串（表示"不需要打开任何页面"），Poll 立刻有凭证
//	否则         → authURL 是**同源的表单页地址**（核心原样下发给前端，
//	                前端渲染成「打开授权页面」）
func (f *loginFlow) Start() (string, string, error) {
	state, err := newLoginState()
	if err != nil {
		return "", "", fmt.Errorf("生成登录 state 失败: %w", err)
	}
	e := &loginEntry{at: time.Now()}

	// ── 路径 ①：本机客户端已经登录过 → 直接拿到 ──
	//
	// ⚠ login_mode=sms 时**跳过**这一步。理由是一个实测会遇到的形状：
	// 本机客户端登录的是 A，用户想加 B —— 若先试本机拾取，每次都会把 A
	// 加回来，页面永远打不开，"加另一个号"根本做不到。
	if f.p.loginMode != "sms" {
		if a, err := f.p.localAuth(); err != nil {
			// 读本机存储失败**不算致命**：还有短信那条路。只记一行日志。
			log.Printf("loomy: 本机客户端登录态读取失败（继续走短信登录）: %v", err)
		} else if a != nil {
			e.ready = true
			e.cred = credentialOf(a)
		}
	}

	f.mu.Lock()
	f.gcLocked()
	f.sessions[state] = e
	f.mu.Unlock()

	if e.ready {
		return state, "", nil
	}
	if f.p.loginMode == "local" {
		// 明确要求只用本机拾取，而它没成 —— 不悄悄换成短信（那会发一条
		// 用户没预期的短信），而是说清怎么改。
		return "", "", errors.New("本机没有可用的 Loomy 客户端登录态，" +
			"而 loomy.login_mode=local 不允许走手机号验证码。" +
			"请先在客户端登录，或者把 login_mode 改成 auto / sms")
	}
	// 路径 ②：把 state 交给表单页。
	return state, loginPagePath + "?state=" + state, nil
}

// normalizeLoginMode 归一化登录方式；未知取值一律落回 auto。
//
// 落回而不是报错：一个拼错的 mode 不该让整个上游起不来 ——
// auto 是"两条路都留着"的安全默认。
func normalizeLoginMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "sms", "phone":
		return "sms"
	case "local", "client":
		return "local"
	default:
		return "auto"
	}
}

// Poll 取回已经就绪的凭证；没就绪就是 pending（核心回 202，前端继续轮询）。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	f.mu.Lock()
	e, ok := f.sessions[state]
	if !ok {
		f.mu.Unlock()
		return gateway.Credential{}, errors.New("loomy: 登录会话不存在或已结束，请重新发起")
	}
	if !e.ready {
		f.mu.Unlock()
		// 用户还在输手机号/验证码 —— 这正是 pending 的含义。
		return gateway.Credential{}, gateway.ErrLoginPending
	}
	// 一次性：取走即删（否则"添加账号"可以无限重复落盘）。
	delete(f.sessions, state)
	f.mu.Unlock()
	return e.cred, nil
}

// BeginSMS 表单页第 1 步：发验证码，返回账号网关给的 msgid。
func (f *loginFlow) BeginSMS(state, phone string) (string, error) {
	f.mu.Lock()
	e, ok := f.sessions[state]
	f.mu.Unlock()
	if !ok {
		return "", errors.New("登录会话不存在或已结束，请回到控制台重新点「添加账号」")
	}
	if e.ready {
		return "", errors.New("这个会话已经登录完成，请回到控制台刷新")
	}
	if !looksLikePhone(phone) {
		return "", errors.New("请填写 11 位手机号")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgid, err := f.p.smsClient().SendCode(ctx, phone)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	if cur, ok := f.sessions[state]; ok {
		cur.phone, cur.msgid = phone, msgid
	}
	f.mu.Unlock()
	return msgid, nil
}

// FinishSMS 表单页第 2 步：验码换 session，并把凭证置为就绪。
//
// 置为就绪之后，控制台那边的轮询会在 2.5 秒内取到它，核心负责落盘 + 并池。
func (f *loginFlow) FinishSMS(state, phone, code, msgid string) (*Auth, error) {
	f.mu.Lock()
	e, ok := f.sessions[state]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("登录会话不存在或已结束，请回到控制台重新点「添加账号」")
	}
	if e.ready {
		return nil, errors.New("这个会话已经登录完成")
	}
	// 手机号 / msgid 以页面回传为准，但**缺省回落**到 BeginSMS 存下的那份 ——
	// 用户刷新页面之后表单会变空，那时不该要求他重发一次短信。
	if strings.TrimSpace(phone) == "" {
		phone = e.phone
	}
	if strings.TrimSpace(msgid) == "" {
		msgid = e.msgid
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, err := f.p.smsClient().Login(ctx, phone, code, msgid)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if cur, ok := f.sessions[state]; ok {
		cur.ready = true
		cur.cred = credentialOf(a)
		cur.phone, cur.msgid = phone, msgid
	}
	f.mu.Unlock()
	return a, nil
}

// sessionState 给表单页读中间状态（手机号已填过没有）。
func (f *loginFlow) sessionState(state string) (phone string, hasMsgid bool, ready bool, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, found := f.sessions[state]
	if !found {
		return "", false, false, false
	}
	return e.phone, e.msgid != "", e.ready, true
}

// gcLocked 回收超过 TTL 的会话（调用方持锁）。
//
// 只可能清掉"用户点了添加账号但没做完"的条目 —— 做完的会被 Poll 取走并删除。
func (f *loginFlow) gcLocked() {
	for k, e := range f.sessions {
		if time.Since(e.at) > loginSessionTTL {
			delete(f.sessions, k)
		}
	}
}

// localAuth 读本机客户端已登录的 session（路径 ①）。
//
// 目录不存在 → (nil,nil)：**不是错误**，只是这条路走不通。
func (p *Provider) localAuth() (*Auth, error) {
	dir := p.effectiveClientDir()
	if dir == "" {
		return nil, nil
	}
	a, _, err := ReadLocalAuth(dir)
	return a, err
}

// credentialOf 把一份 *Auth 包成核心要的 gateway.Credential。
//
// Secret 必须是**能落盘**的类型（核心的 pollViaFlow 要求它实现
// MarshalAuthFile）—— 本包的 `MarshalAuthFile` 是包级函数，不进方法集，
// 所以外面要包一层 authFile。不包会让核心回 501
// 「该上游的凭证结构尚未接入落盘」，而账号其实已经拿到了。
func credentialOf(a *Auth) gateway.Credential {
	return gateway.Credential{
		Provider: providerID,
		UID:      a.UID,
		Nickname: a.Nickname,
		Secret:   &authFile{a: a},
	}
}

// newLoginState 生成一个不可预测的 state。
func newLoginState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sourceHint 把 ReadLocalAuth 的第三个返回值渲染成一句可读的话。
func sourceHint(source string) string {
	if strings.TrimSpace(source) == "" {
		return "目录里没有可用的 Local Storage 数据文件（.log/.ldb/.sst）。"
	}
	return "已扫描 " + source + "，但里面没有完整的登录态记录。"
}

// looksLikePhone 粗判手机号形态（11 位数字，以 1 开头）。
//
// ⚠ 只在**发短信之前**做一次体检，用来省下一条必然失败的短信；
// 它不校验手机号是否真实存在 —— 那只有账号网关知道。
func looksLikePhone(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 11 || s[0] != '1' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// authFile 让一份 *Auth 满足核心的落盘窄接口（见 credentialOf 的注释）。
type authFile struct {
	a *Auth
}

// MarshalAuthFile 返回 (文件名, 内容) —— 核心的 authFileWriter 契约。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.a == nil {
		return "", nil, errors.New("loomy: 凭证为空")
	}
	raw, err := MarshalAuthFile(f.a)
	if err != nil {
		return "", nil, fmt.Errorf("loomy: 序列化凭证失败: %w", err)
	}
	name := FileName(f.a)
	if name == "" {
		return "", nil, errors.New("loomy: 凭证文件名为空")
	}
	return name, raw, nil
}
