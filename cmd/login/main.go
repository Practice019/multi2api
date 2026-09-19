// login.go — WorkBuddy CN / 海外版（WorkBuddy AI）OAuth 登录（设备授权流程）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login url   → POST /v2/plugin/auth/state?platform=workbuddy-ai&version=5.5.2（海外版）
//	              / ?platform=CLI（国内版）拿 state+authUrl，
//	              state 落 /tmp/wb2api-login-state.json，stdout 打印授权 URL
//	login poll  → 读 state，GET /v2/plugin/auth/token?state= 一次，
//	              成功再 GET /v2/plugin/login/account?state= 拿 uid/nickname，
//	              stdout 打印完整 token+account JSON
//
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
//
// 渠道：默认 CN（copilot.tencent.com）；`-intl` flag 或环境变量 WB2A_LOGIN_INTL=1
// 切换到海外版（www.workbuddy.ai），state 文件与 Origin 头随之切换。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"
)

// 上游常量。CN 为默认；intl 由 -intl 切换。
const (
	upstreamBaseCN    = "https://copilot.tencent.com"
	upstreamBaseIntl  = "https://www.workbuddy.ai"
	clientUA          = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN   = "https://www.codebuddy.cn"
	originRefererIntl = "https://www.workbuddy.ai"
	stateFileCN       = "/tmp/wb2api-login-state.json"
	stateFileIntl     = "/tmp/wb2api-login-state-intl.json"
)

// loginEnv 一次登录流程的站点常量（CN / intl 二选一）。
type loginEnv struct {
	origin    string
	stateFile string
	channel   string // cn / intl
	authState string
	loginAcct string
	authToken string
}

func newLoginEnv(intl bool) loginEnv {
	if intl {
		return loginEnv{
			origin:    originRefererIntl,
			stateFile: stateFileIntl,
			channel:   "intl",
			// ★ platform=workbuddy-ai（客户端流程，含「设置地区」），不用 CLI：
			//   CLI 版登录页没有地区那一步 → 账号不完整开通 → 对话 14017 / 计费 500。
			//   参照 register-machine 已跑通的注册流程。见 internal/oauth 常量区注释。
			authState: upstreamBaseIntl + "/v2/plugin/auth/state?platform=workbuddy-ai&version=5.5.2",
			loginAcct: upstreamBaseIntl + "/v2/plugin/login/account?state=",
			authToken: upstreamBaseIntl + "/v2/plugin/auth/token?state=",
		}
	}
	return loginEnv{
		origin:    originRefererCN,
		stateFile: stateFileCN,
		channel:   "cn",
		authState: upstreamBaseCN + "/v2/plugin/auth/state?platform=CLI",
		loginAcct: upstreamBaseCN + "/v2/plugin/login/account?state=",
		authToken: upstreamBaseCN + "/v2/plugin/auth/token?state=",
	}
}

// commonHeaders 通用请求头（origin 随渠道）。
func commonHeaders(req *http.Request, env loginEnv) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", env.origin)
	req.Header.Set("Referer", env.origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// apiEnvelope 与 main.go:429-433 一致
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 与 oauth.go:33-66 一致：{code,msg,data} 信封，code!=0 → error
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

type loginState struct {
	State string `json:"state"`
}

func main() {
	intl := flag.Bool("intl", false, "登录海外版 WorkBuddy AI (www.workbuddy.ai)")
	flag.Parse()
	if v := strings.TrimSpace(os.Getenv("WB2A_LOGIN_INTL")); v != "" && (v == "1" || strings.EqualFold(v, "true")) {
		*intl = true
	}
	if flag.NArg() < 1 {
		fatal("usage: login [-intl] <url|poll>")
	}
	env := newLoginEnv(*intl)
	// 每个流程独立 cookie jar（oauth.go:22-29：多账号登录互不串会话）
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	common := func(r *http.Request) { commonHeaders(r, env) }

	switch flag.Arg(0) {
	case "url":
		// handleStartLogin (oauth.go:68-87)
		data, _, err := doJSON(client, http.MethodPost, env.authState, common, bytes.NewReader([]byte("{}")))
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		var st struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		}
		if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
			fatal("auth state: missing state or authUrl")
		}
		raw, _ := json.Marshal(loginState{State: st.State})
		if err := os.WriteFile(env.stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(st.AuthURL)

	case "poll":
		raw, err := os.ReadFile(env.stateFile)
		if err != nil {
			fatal("read state: %v (先跑 login url)", err)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("parse state: %v", err)
		}
		// handlePollLogin (oauth.go:108-162)：auth/token 是权威登录状态端点，
		// pending 时业务 code 非 0（"login ing"），完成时 code=0 + token bundle
		tokRaw, status, errTok := doJSON(client, http.MethodGet, env.authToken+ls.State, common, nil)
		if errTok != nil {
			if status == 0 || status >= 500 {
				fatal("token endpoint error: %v", errTok)
			}
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		var tok struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
		}
		if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		// login/account 拿 uid/nickname（带 Bearer）
		var acct struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		acctHeaders := func(r *http.Request) {
			commonHeaders(r, env)
			r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		}
		if acctRaw, _, errAcct := doJSON(client, http.MethodGet, env.loginAcct+ls.State, acctHeaders, nil); errAcct == nil {
			_ = json.Unmarshal(acctRaw, &acct)
		}
		out := map[string]any{
			"access_token":  tok.AccessToken,
			"refresh_token": tok.RefreshToken,
			"expires_in":    tok.ExpiresIn,
			"domain":        tok.Domain,
			"channel":       env.channel,
			"uid":           acct.UID,
			"enterprise_id": acct.EnterpriseID,
			"nickname":      acct.Nickname,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(env.stateFile)

	default:
		fatal("unknown subcommand %q (want url|poll)", flag.Arg(0))
	}
}
