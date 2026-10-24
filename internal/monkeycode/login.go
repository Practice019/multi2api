// login.go 页内「添加账号」—— 走**客户端登录流程**（浏览器授权 → 回调）。
//
// # MonkeyCode 的登录是百智云 OAuth
//
// 逆向客户端（monkeycode-desktop.exe）得到的事实：
//
//	GET /api/v1/users/login?redirect=&inviter_id=
//	  → 302 https://baizhi.cloud/oauth/authorize?client_id=monkeycode-ai
//	       &redirect_uri=https://monkeycode-ai.com/api/v1/users/baizhi/callback
//	       &response_type=code&scope=user+phone&state=…
//
// 也就是说 **MonkeyCode 是百智云的一个 OAuth 应用**（client_id=monkeycode-ai）。
// 登录地址栏里看到的是百智云的授权页，而那个页面支持 GitHub / 密码 / 微信
// 等多种登录方式 —— 用户选一种完成授权，百智云回调 MonkeyCode，
// MonkeyCode 种下 `monkeycode_ai_session` cookie。
//
// # 为什么这才是"客户端登录流程"
//
// 上一版实现成"读本机客户端的登录态"，那是错的：本仓的 LoginFlow 语义是
// **打开授权页让用户登录**，而"本机拾取"是 workbuddy 那条 auth_url 为空的分支。
// 混用会让界面显示「登录态就在本机那个桌面客户端里」—— 用户看到那句话
// 会以为网关在偷偷读他的客户端。
//
// 所以这里 authURL 给**真实的授权 URL**，与其余上游同款。
package monkeycode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：让 *Provider 直接满足 gateway.LoginFlow 与 ManualLoginExt。
//
// ⚠ 这些方法**必须挂在 *Provider 上**（不能用访问器返回）：
// 核心用 ExtOf[LoginFlow](p) 做类型断言，访问器的返回值不参与方法集匹配。
// 少了它们，manifest 会下发 login:null —— 界面上没有「添加账号」按钮
// （raccoon 实测踩过这个形态）。
var _ gateway.LoginFlow = (*Provider)(nil)
var _ gateway.ManualLoginExt = (*Provider)(nil)

// loginStateTTL 一次登录会话的有效期。
const loginStateTTL = 15 * time.Minute

// loginSession 一次待完成的登录。
type loginSession struct {
	created time.Time
	region  string
}

// Start 申请一次登录，返回 (state, 授权URL)。
//
// # 三条路，按站点与优先级选
//
// 逆向 `monkeycode-ai.net/login` 的 SPA（`assets/index-*.js`）得出的完整事实：
//
//	国际站(global)  choices.github / choices.google → 都用
//	                GET /api/v1/users/oauth/{platform}/login?redirect_url=…
//	                → {data:{auth_url}} → 跳到 GitHub / Google 授权页
//
//	国内站(cn)      该登录方式暂未启用（实测两者都回这句话）
//	                只有 choices.baizhi → GET /api/v1/users/login → 302 百智云授权页
//	                与 choices.password → POST /api/v1/users/password-login
//
// 所以：国际站优先给 GitHub，国内站给百智云。
//
// # 为什么不再用"打 login 入口取 302"
//
// 那条路只有国内站成立（国际站回的是相对路径且 client_id 为空）。
// 而 `oauth/{platform}/login` 是**显式端点**，回的是 JSON 里的 auth_url ——
// 两站语义一致，且能选平台。
// 编译期断言：本上游需要用户先选站点，所以额外实现 LoginFlowWithOptions。
var _ gateway.LoginFlowWithOptions = (*Provider)(nil)

// 编译期断言：自报登录选项。
var _ gateway.LoginOptionsExt = (*Provider)(nil)

// loginRegionKey 登录选项的键名（"站点"）。
const loginRegionKey = "region"

// LoginOptions 自报登录前要用户选的选项。
//
// # 为什么必须让用户选（用户报的）
//
// 用户原话：「添加账号需要用户选择国内或者国际啊」。
//
// 两站是**完全独立的账号体系**：
//
//	国内站 monkeycode-ai.com   45 个模型   ← 百智云 OAuth
//	国际站 monkeycode-ai.net   11 个模型   ← GitHub / Google OAuth
//
// 且 cookie **互不通用**（拿国际站的 session 打国内站回 401 未授权）。
// 所以必须在开始登录**之前**就知道要去哪个站 —— 那时候用户还没登录，
// 我们无从探测。
func (p *Provider) LoginOptions() []gateway.LoginOption {
	return []gateway.LoginOption{{
		Key:   loginRegionKey,
		Label: "站点",
		Values: []gateway.LoginOptionValue{
			{Value: RegionCN, Label: "国内站（monkeycode-ai.com，45 个模型）"},
			{Value: RegionIntl, Label: "国际站（monkeycode-ai.net，11 个模型，GitHub 登录）"},
		},
		Default: RegionCN,
		Hint:    "两站账号不通用：国内站走百智云授权，国际站走 GitHub/Google。选错登录后账号也读不到额度。",
	}}
}

// Start 无选项入口（向后兼容）。
//
// 一些老调用方（以及契约测试）只认 LoginFlow.Start，所以这里按默认值走。
func (p *Provider) Start() (state, authURL string, err error) {
	return p.StartWith(nil)
}

// StartWith 按用户选的站点启动浏览器登录。
//
// opts 里没有 region（或值不认识）时用国内站 —— 它是主站。
func (p *Provider) StartWith(opts map[string]string) (state, authURL string, err error) {
	region := RegionCN
	if v := strings.TrimSpace(opts[loginRegionKey]); v != "" {
		if v == RegionIntl {
			region = RegionIntl
		}
	}
	id, err := p.StartBrowserLogin(region, 10*time.Minute)
	if err != nil {
		return "", "", err
	}
	// authURL 回空串 —— 前端据此走"轮询等结果"分支（而不是"打开链接"）。
	return id, "", nil
}

// rememberLogin 记下这次登录会话。
func (p *Provider) rememberLogin(state, region string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logins == nil {
		p.logins = map[string]*loginSession{}
	}
	now := time.Now()
	for k, v := range p.logins {
		if now.Sub(v.created) > loginStateTTL {
			delete(p.logins, k)
		}
	}
	p.logins[state] = &loginSession{created: now, region: region}
}

// oauthLoginURL 取某个平台的授权地址（`GET /api/v1/users/oauth/{platform}/login`）。
//
// 回 `{code:0,data:{auth_url:"…"}}`；平台没启用时 code 非 0 且 message 是
// 「该登录方式暂未启用」—— 那种情况按错误返回，让 Start 换下一个。
func (p *Provider) oauthLoginURL(ctx context.Context, region, platform string) (string, error) {
	host := hostCN
	if region == RegionIntl {
		host = hostIntl
	}
	url := host + "/api/v1/users/oauth/" + platform + "/login?redirect_url=/console/tasks"
	code, raw, err := p.client.doJSON(ctx, &Auth{Region: region}, http.MethodGet, url, http.Header{
		"Accept":     []string{"application/json"},
		"User-Agent": []string{defaultUserAgent},
	}, nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("%s/%s 返回 %d", region, platform, code)
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AuthURL string `json:"auth_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	if env.Code != 0 || strings.TrimSpace(env.Data.AuthURL) == "" {
		return "", fmt.Errorf("%s/%s: %s", region, platform, firstNonEmptyStr(env.Message, "没有 auth_url"))
	}
	return env.Data.AuthURL, nil
}

// baizhiAuthorizeURL 取国内站的百智云授权地址。
//
// 打 `GET /api/v1/users/login`，**不跟随重定向**，取 302 的 Location。
func (p *Provider) baizhiAuthorizeURL(ctx context.Context) (string, error) {
	url := hostCN + pathLoginEntry + "?redirect=&inviter_id="
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/html,application/json,*/*")

	base := p.client.httpFor(&Auth{Region: RegionCN})
	client := &http.Client{
		Transport: base.Transport,
		Timeout:   20 * time.Second,
		// ⚠ 不跟随：跟了会被带到授权页 HTML，拿不到 URL 本身。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("国内站 login 入口返回 %d", resp.StatusCode)
	}
	loc := strings.TrimSpace(resp.Header.Get("Location"))
	if loc == "" {
		return "", fmt.Errorf("国内站 302 没有 Location")
	}
	if strings.HasPrefix(loc, "/") {
		loc = hostCN + loc
	}
	return loc, nil
}

// firstNonEmptyStr 取第一个非空字符串。
func firstNonEmptyStr(list ...string) string {
	for _, s := range list {
		if v := strings.TrimSpace(s); v != "" {
			return v
		}
	}
	return ""
}

// Poll 查授权结果。
//
// # 本上游为什么只能"等用户粘回来"
//
// OAuth 的 code 是回调打到 `monkeycode-ai.com/api/v1/users/baizhi/callback`
// 之后由**上游**换 cookie 的，我们拿不到那个 code —— 除非登录页把浏览器
// 导到一个我们能拦的地址。所以真正的收号走 ManualLogin 的粘贴框
// （用户把回调后地址栏里那条 `/?ic=…` 或直接 cookie 粘回来）。
//
// 恒回 ErrLoginPending 是**正确语义**：流程没完成，而且不会自动完成。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	st := p.BrowserLoginStatusOf(state)
	if st == nil {
		return gateway.Credential{}, gateway.ErrLoginPending
	}
	if !st.OK {
		return gateway.Credential{}, gateway.ErrLoginPending
	}
	// 登录完成：从凭证目录把它找出来交给核心。
	list, err := LoadDir(p.authDir)
	if err != nil {
		return gateway.Credential{}, err
	}
	for _, a := range list {
		if a.UserID == st.UserID || a.UID() == st.UserID {
			return gateway.Credential{
				Provider: providerID,
				UID:      a.UID(),
				Nickname: a.DisplayName(),
				Secret:   a,
			}, nil
		}
	}
	return gateway.Credential{}, gateway.ErrLoginPending
}

// Configured 恒 true：登录不需要任何部署侧配置。
func (p *Provider) Configured() bool { return true }

// ManualLogin 粘贴完成端点与指引。
func (p *Provider) ManualLogin() (string, string) {
	return adminManualCompletePath,
		"在打开的授权页里完成登录（GitHub / 密码 / 微信任选）。" +
			"登录成功后浏览器会跳回 MonkeyCode；此时按 F12 → Application → Cookies → " +
			"monkeycode_ai_session，把 Value 粘到下面。" +
			"也可以直接粘贴 cookie 那一整行，或一行一个批量粘贴。"
}

// randomState 生成随机 state（16 字节 hex）。
func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleManualComplete 粘贴完成端点：{state, code} → 校验 → 补签 → 落盘。
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

	region := ""
	if s := strings.TrimSpace(body.State); s != "" {
		p.mu.Lock()
		sess, ok := p.logins[s]
		if ok {
			delete(p.logins, s)
			region = sess.region
		}
		p.mu.Unlock()
	}

	creds, err := p.ImportCredentials(body.Code)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if len(creds) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "没有识别出任何 session"})
		return
	}

	imported := 0
	var notes []string
	for _, c := range creds {
		a, perr := p.preparePasted(c, region)
		if perr != nil {
			notes = append(notes, shortErr(perr))
			continue
		}
		if _, serr := SaveFile(p.authDir, a); serr != nil {
			notes = append(notes, "落盘失败 "+serr.Error())
			continue
		}
		p.mu.Lock()
		if p.creds == nil {
			p.creds = map[string]*Auth{}
		}
		p.creds[a.UID()] = a
		p.mu.Unlock()
		imported++
		log.Printf("monkeycode: 已添加账号 %s（region=%s）", a.DisplayName(), a.RegionOrDefault())
	}

	if imported == 0 {
		msg := "没有成功导入任何账号"
		if len(notes) > 0 {
			msg += "：" + strings.Join(notes, "; ")
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported, "notes": notes})
}

// preparePasted 校验一份刚粘贴的凭证，并在缺 api_key 时自动补签。
func (p *Provider) preparePasted(c gateway.ImportedCredential, regionHint string) (*Auth, error) {
	a, err := parseAuth(c.Raw)
	if err != nil {
		return nil, err
	}
	if a.Region == "" && regionHint != "" {
		a.Region = regionHint
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := p.probeStatus(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("session 校验失败：%w", err)
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
func (p *Provider) probeStatus(ctx context.Context, a *Auth) (*StatusPayload, error) {
	if strings.TrimSpace(a.Region) != "" {
		return p.client.FetchStatus(ctx, a)
	}
	var msgs []string
	for _, region := range []string{RegionCN, RegionIntl} {
		probe := *a
		probe.Region = region
		st, err := p.client.FetchStatus(ctx, &probe)
		if err == nil {
			a.Region = region
			return st, nil
		}
		msgs = append(msgs, region+": "+shortErr(err))
	}
	return nil, fmt.Errorf("国内站与国际站都不认这个 session（%s）", strings.Join(msgs, "; "))
}

// shortErr 把错误压成一句话。
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

// jsonDecode 解请求体（带上限）。
func jsonDecode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}
