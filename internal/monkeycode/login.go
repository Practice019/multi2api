// login.go 页内「添加账号」：粘贴 session 的流程。
//
// # 本上游为什么没有 OAuth
//
// MonkeyCode 的凭证是浏览器里的一条 cookie（`monkeycode_ai_session`）。
// 它**没有** OAuth 授权页 —— 登录本身走 GitHub/微信，而那个流程拿到的
// 是 MonkeyCode 自己的 cookie，不是可换的 code。
//
// 所以「添加账号」的形态只能是**粘贴**：用户从 DevTools 复制 cookie 值，
// 粘进来，我们校验后落盘。
//
// # 怎么把它接进 LoginFlow
//
// LoginFlow 的三件套对"没有授权页"的上游仍然成立，只是语义平移：
//
//	Start     生成一个 state（用于把"这一次粘贴"与后续校验对上）
//	          authURL 返回**空串**（没有可打开的授权页）
//	Poll      恒回 ErrLoginPending —— 本流程不靠轮询完成，靠 Manual 端点
//	Manual    {path, instructions}：前端据此渲染粘贴框
//
// ⚠ authURL 必须回空串而不是随便编一个：前端对空串有专门分支
//（不渲染「打开授权页」），编一个会让用户点开一个 404。
//
// # Configured
//
// 恒 true —— 粘贴不需要任何部署侧配置（不像 OAuth 要 client_id）。
// 这正是 Configured 存在的意义：它回答"这份部署真的能走这个流程吗"。
package monkeycode

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：让 *Provider 直接满足 gateway.LoginFlow。
//
// ⚠ 这三个方法**必须挂在 *Provider 上**（不能用访问器返回）：
// 核心用 ExtOf[LoginFlow](p) 做**类型断言**，访问器的返回值不参与
// 方法集匹配。少了它们，测试可能全绿而 manifest 下发 login:null ——
// 界面上就没有「添加账号」按钮（raccoon 实测踩过这个形态）。
var _ gateway.LoginFlow = (*Provider)(nil)

// 编译期断言：粘贴完成端点。
var _ gateway.ManualLoginExt = (*Provider)(nil)

// loginStateTTL 一次粘贴会话的有效期。
//
// 用户从"点添加账号"到"复制 cookie 粘进来"通常是几十秒，
// 给 10 分钟足够，同时避免 state 表无限增长。
const loginStateTTL = 10 * time.Minute

// loginSession 一次待完成的粘贴会话。
type loginSession struct {
	created time.Time
	region  string
}

// Start 生成一次粘贴会话，返回 (state, authURL)。
//
// authURL 恒空：本上游没有可打开的授权页（见文件头）。
func (p *Provider) Start() (string, string, error) {
	state, err := randomState()
	if err != nil {
		return "", "", err
	}
	p.mu.Lock()
	if p.logins == nil {
		p.logins = map[string]*loginSession{}
	}
	// 顺手清掉过期的，避免表无限增长。
	now := time.Now()
	for k, v := range p.logins {
		if now.Sub(v.created) > loginStateTTL {
			delete(p.logins, k)
		}
	}
	p.logins[state] = &loginSession{created: now}
	p.mu.Unlock()
	return state, "", nil
}

// Poll 恒回 ErrLoginPending —— 本流程不靠轮询完成。
//
// 真正的收号发生在 ManualComplete（粘贴之后）。前端在 authURL 为空时
// 会渲染粘贴框而不是轮询倒计时，所以这里返回 pending 不会被反复调用。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	return gateway.Credential{}, gateway.ErrLoginPending
}

// Configured 恒 true：粘贴流程不需要任何部署侧配置。
func (p *Provider) Configured() bool { return true }

// ManualLogin 告诉前端"粘贴完成打哪个端点、文案怎么说"。
func (p *Provider) ManualLogin() (string, string) {
	return adminManualCompletePath,
		"把浏览器 DevTools 里 monkeycode_ai_session 的值粘贴到这里。" +
			"取法：登录 monkeycode-ai.com → F12 → Application → Cookies → " +
			"monkeycode_ai_session → 复制 Value。" +
			"可以一行一个批量粘贴，也可以直接粘 JSON。"
}

// handleManualComplete 粘贴完成端点。
//
// 请求体：{ state, code }。code 是用户粘贴的 session（或 JSON / 多行）。
func (p *Provider) handleManualComplete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}
	if err := jsonDecode(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有粘贴任何内容"})
		return
	}

	// 校验 state（不强制：有些客户端不带。带了就必须是签发过的）。
	if s := strings.TrimSpace(body.State); s != "" {
		p.mu.Lock()
		sess, ok := p.logins[s]
		if ok {
			delete(p.logins, s)
		}
		p.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "登录会话已过期（重新点「添加账号」）"})
			return
		}
		_ = sess
	}

	// 解析出可落盘的凭证。
	creds, err := p.ImportCredentials(body.Code)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if len(creds) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有识别出任何 session"})
		return
	}

	// 原地校验并补签 key（用户粘贴的通常只有 session）。
	imported := 0
	var notes []string
	for _, c := range creds {
		a, perr := p.preparePasted(c)
		if perr != nil {
			notes = append(notes, c.Nickname+": "+perr.Error())
			continue
		}
		if _, serr := SaveFile(p.authDir, a); serr != nil {
			notes = append(notes, a.DisplayName()+": 落盘失败 "+serr.Error())
			continue
		}
		p.mu.Lock()
		if p.creds == nil {
			p.creds = map[string]*Auth{}
		}
		p.creds[a.UID()] = a
		p.mu.Unlock()
		imported++
	}

	if imported == 0 {
		msg := "没有成功导入任何账号"
		if len(notes) > 0 {
			msg += "：" + strings.Join(notes, "; ")
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
		return
	}
	log.Printf("monkeycode: 页内添加账号成功 %d 个", imported)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported, "notes": notes})
}

// preparePasted 校验一份刚粘贴的凭证，并在缺 api_key 时自动补签。
//
// # 为什么自动补签
//
// 用户从 DevTools 只能复制到 session（api_key/signing_secret 是我们调接口
// 换出来的，浏览器里没有）。若不补签，导入的账号**不能对话** ——
// 用户会看到"添加成功了但一发消息就 403"，而那个 403 的文案是
// invalid ohmyagent request，指不到"还没签 key"这一步。
func (p *Provider) preparePasted(c gateway.ImportedCredential) (*Auth, error) {
	a, err := parseAuth(c.Raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := contextWithTimeout(30 * time.Second)
	defer cancel()

	// 校验 session 真的有效（顺带取到 user.id 与昵称）。
	//
	// ⚠ **区域未指定时必须自动探测**：用户粘贴的是裸 session，
	// 里面没有"我属于哪个站"的信息。默认按国内站打会得到
	// `401 未授权` —— 而那个错看起来像"session 复制错了"，
	// 实际只是站点选错（实测踩到：国际站 session 被判成无效）。
	st, err := p.probeStatus(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("session 校验失败（复制错了吗？）：%w", err)
	}
	if strings.TrimSpace(st.User.ID) != "" {
		a.UserID = st.User.ID
	}
	if strings.TrimSpace(st.User.Name) != "" {
		a.Nickname = st.User.Name
	}
	if a.AddedAt == "" {
		a.AddedAt = time.Now().Format(time.RFC3339)
	}
	// 缺 key 就补签（已有则保留，不重签 —— 重签会产生无用 key 堆积）。
	if strings.TrimSpace(a.APIKey) == "" || strings.TrimSpace(a.SigningSecret) == "" {
		pk, kerr := p.client.CreateAPIKey(ctx, a, "multi2api")
		if kerr != nil {
			return nil, fmt.Errorf("已登录但签发模型 key 失败：%w", kerr)
		}
		a.APIKey = pk.APIKey
		a.SigningSecret = pk.SigningSecret
	}
	return a, nil
}

// probeStatus 校验 session 并按需探测区域。
//
// # 为什么需要它
//
// 用户粘贴的只是 session 值，**不含站点信息**。两站互不认对方的 session：
//
//	国内站 session → intl 站回 401 未授权
//	国际站 session → cn   站回 401 未授权
//
// 而 401 的文案是"未授权"，看起来像"复制错了"，实际只是站点不对。
// 所以这里：显式配了 region 就直接用；没配就**两个站都试一次**，
// 谁通了就记住谁。
func (p *Provider) probeStatus(ctx context.Context, a *Auth) (*StatusPayload, error) {
	// 显式指定了区域：只试那一个（用户明确说了，不要自作聪明）。
	if strings.TrimSpace(a.Region) != "" {
		return p.client.FetchStatus(ctx, a)
	}
	// 未指定：先试国内站（主站），失败再试国际站。
	type attempt struct {
		region string
		st     *StatusPayload
		err    error
	}
	var results []attempt
	for _, region := range []string{RegionCN, RegionIntl} {
		probe := *a
		probe.Region = region
		st, err := p.client.FetchStatus(ctx, &probe)
		results = append(results, attempt{region: region, st: st, err: err})
		if err == nil {
			// 命中：写回区域，后续所有请求都走对的站。
			a.Region = region
			return st, nil
		}
	}
	// 两个站都不通：把**两边的**错误都带上 —— 只说一边会让人以为
	// "换个站就好了"，而实际可能 session 真的错了。
	var msgs []string
	for _, r := range results {
		if r.err != nil {
			msgs = append(msgs, r.region+": "+shortErr(r.err))
		}
	}
	return nil, fmt.Errorf("国内站与国际站都不认这个 session（%s）", strings.Join(msgs, "; "))
}

// shortErr 把错误压成一句话（错误体可能很长）。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 150 {
		s = s[:150] + "…"
	}
	return s
}

// randomState 生成一个随机 state（16 字节 hex）。
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// jsonDecode 解请求体（带上限，避免一个巨大的坏 body 灌进内存）。
func jsonDecode(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

// contextWithTimeout 造一个带超时的 ctx（页面交互路径用）。
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
