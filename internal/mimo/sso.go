// sso.go — SSO 链全自动换 serviceToken（passToken → serviceLogin → /api/sts）。
//
// 依据：用户 2026-09-24 探测报告 §3.3（实测全链）与 §5.1 脚本，关键复刻点：
//
//  1. 起点 GET {routeBase}/api/user/xiaomi/me（**不带** cookie）→ 302 Location=serviceLogin
//  2. serviceLogin（account.xiaomi.com）必须带**全套** account cookie
//     （passToken/cUserId/userId/pass_ua/deviceId）+ Sec-Fetch 浏览器头，
//     缺一项就 302 到 /fe/service/login（SPA 登录页）—— 纯脚本死路（§7 坑）。
//  3. serviceLogin 302 → /api/sts?sign=…（签名参数由服务端在 Location 里给全，
//     我们只跟链，不构造 —— 这也是"能不能全自动"的关键：auth/_ssign 不需要客户端密钥）。
//  4. /api/sts 307 + Set-Cookie：serviceToken/userId/mimopc_slh/mimopc_ph。
//  5. 每一跳收集 Set-Cookie：**passToken 会被 serviceLogin 续签（30 天滚动）**，
//     拿到新值就更新回凭证 —— 链条只要活着跑，账号会话就一直自我续命。
//
// 域名分发纪律（复刻 requests cookiejar 的域匹配行为，防把账号 cookie 泄给
// 服务域）：*.xiaomi.com / account.xiaomi.com ← account jar；*.xiaomimimo.com ←
// service jar（sts 用 query 签名认账号，不吃账号 cookie）。
package mimo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SSO 链的常量（用户脚本 §5.1 逐字复刻 —— 这些指纹组合是实测通过项）。
const (
	ssoUA          = "miNative PC/Normal Windows_NT/10.0.22631 SDKV/1.0.0 DEVT/PC DEVS/Windows APP/miaccount_desktop APPV/0.1.0"
	ssoAccept      = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
	ssoMaxHops     = 8
	ssoServiceName = "servicetoken" // collect 以小写存键
)

// ServiceTokenMaxAge 主动预换年龄：serviceToken 是会话级、TTL 未公开；
// 后台任务对超过该年龄的票直接重换（宁多换一次，不在对话里吃 401）。
const ServiceTokenMaxAge = 6 * time.Hour

// ServiceTokenAged 报告 route 的 serviceToken 是否已"老了"（含从未换过）。
func (a *Auth) ServiceTokenAged(now time.Time) bool {
	if a == nil {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ServiceToken == "" {
		return true
	}
	if a.ServiceAt <= 0 {
		return true
	}
	return now.Unix()-a.ServiceAt > int64(ServiceTokenMaxAge/time.Second)
}

func jarCookieHeader(jar map[string]string) string {
	if len(jar) == 0 {
		return ""
	}
	parts := make([]string, 0, len(jar))
	for k, v := range jar {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

// isAccountHost 小米账号域（.xiaomi.com 后缀，但 **排除** .xiaomimimo.com ——
// 两域只共享后缀词，不共享 cookie 域，别把 passToken 错发给服务网关）。
func isAccountHost(h string) bool {
	h = strings.ToLower(h)
	if strings.HasSuffix(h, ".xiaomimimo.com") || h == "xiaomimimo.com" {
		return false
	}
	return strings.HasSuffix(h, ".xiaomi.com") || h == "xiaomi.com" || strings.HasSuffix(h, ".xiaomi.net")
}

func isMimoServiceHost(h string) bool {
	h = strings.ToLower(h)
	return strings.HasSuffix(h, ".xiaomimimo.com") || h == "xiaomimimo.com"
}

// isSSOLoginPath SSO 登录端点的路径特征（真实链 = account.xiaomi.com/pass/serviceLogin）。
// host 规则之外再认路径，是为了同 host 假上游也能按真实语义分桶（测试闭环）。
func isSSOLoginPath(pth string) bool { return strings.HasPrefix(pth, "/pass/") }

// SSOFresh 跑完整 SSO 链换新鲜 serviceToken，**原地更新活凭证**（落盘由调用方
// SaveAtomic）。passToken 被续签时同步更新新值（30 天滚动，§3.3 实测点 2）。
func (c *Client) SSOFresh(ctx context.Context, a *Auth) error {
	if a == nil {
		return fmt.Errorf("mimo: 空凭证")
	}
	a.mu.RLock()
	pass, cuid, uid, dev := a.PassToken, a.CUserID, a.UID, a.DeviceD
	a.mu.RUnlock()
	if pass == "" || cuid == "" {
		return fmt.Errorf("mimo: SSO 链需要 passToken 与 cUserId（只贴 serviceToken 的旧式凭证不可续）")
	}

	acct := map[string]string{
		"passToken": pass,
		"pass_ua":   "pc",
		"cUserId":   cuid,
		"uLocale":   "zh_CN",
	}
	if uid != "" {
		acct["userId"] = uid
	}
	if dev != "" {
		acct["deviceId"] = dev
	}
	svc := map[string]string{}

	client := &http.Client{
		Timeout:       40 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}

	base := strings.TrimRight(c.routeBase(), "/")
	routeHost := ""
	if bu, err := url.Parse(base); err == nil {
		routeHost = strings.ToLower(bu.Host)
	}

	collect := func(resp *http.Response, host, path string) {
		for _, ck := range resp.Cookies() {
			v := strings.TrimSpace(ck.Value)
			if v == "" {
				continue
			}
			switch strings.ToLower(ck.Name) {
			case "servicetoken", "mimopc_slh", "mimopc_ph", "userid":
				// 服务 cookie 只从网关 host 收（防被中间域投毒）。
				lh := strings.ToLower(host)
				if isMimoServiceHost(lh) || (routeHost != "" && lh == routeHost) {
					svc[strings.ToLower(ck.Name)] = v
				}
			case "passtoken", "cuserid":
				// 账号域 / SSO 登录端点才会续签 passToken（§3.3：serviceLogin 自动续 30 天）。
				if isAccountHost(host) || isSSOLoginPath(path) {
					if strings.EqualFold(ck.Name, "passToken") {
						acct["passToken"] = v
					} else {
						acct["cUserId"] = v
					}
				}
			case "deviceid":
				if isAccountHost(host) || isSSOLoginPath(path) {
					acct["deviceId"] = v
				}
			}
		}
	}

	next := base + EpRouteMe
	for hop := 0; hop < ssoMaxHops; hop++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		u, err := url.Parse(next)
		if err != nil {
			return fmt.Errorf("mimo: SSO 链 URL 不合法 %q: %w", truncate(next, 80), err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", ssoUA)
		req.Header.Set("Accept", ssoAccept)
		req.Header.Set("Accept-Language", "zh-CN")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-User", "?1")
		req.Header.Set("Sec-Fetch-Dest", "document")
		// 分桶发送（宁窄勿宽）：账号 cookie 只给 *.xiaomi.com 或 SSO 登录端点
		// (/pass/)；服务 cookie 只给网关 host。两个规则同时命中时合并发送。
		h := strings.ToLower(u.Host)
		var hdrs []string
		if isAccountHost(h) || isSSOLoginPath(u.Path) {
			if hh := jarCookieHeader(acct); hh != "" {
				hdrs = append(hdrs, hh)
			}
		}
		if isMimoServiceHost(h) || (routeHost != "" && h == routeHost) {
			if hh := jarCookieHeader(svc); hh != "" {
				hdrs = append(hdrs, hh)
			}
		}
		if len(hdrs) > 0 {
			req.Header.Set("Cookie", strings.Join(hdrs, "; "))
		}

		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		loc := resp.Header.Get("Location")
		collect(resp, u.Host, u.Path)
		bodySniff := ""
		if resp.StatusCode < 300 && !strings.Contains(loc, "/api/sts") {
			// 200 但没换到票：多半是 SPA 登录页（cookie 不齐/风控），必须死心。
			b := make([]byte, 512)
			n, _ := resp.Body.Read(b)
			bodySniff = string(b[:n])
		}
		_ = resp.Body.Close()

		if svc[ssoServiceName] != "" {
			break // 票已到手，链走完了
		}
		if loc == "" {
			if resp.StatusCode == http.StatusOK && bodySniff != "" {
				return fmt.Errorf("mimo: SSO 链止步于 200 页面（疑似 SPA 登录页/风控，检查 passToken 是否过期）: %s", truncate(bodySniff, 80))
			}
			return fmt.Errorf("mimo: SSO 链中断于 %d 且无 Location（hop %d）", resp.StatusCode, hop+1)
		}
		nu, err := u.Parse(loc)
		if err != nil {
			return fmt.Errorf("mimo: SSO 链 Location 不可解析: %w", err)
		}
		next = nu.String()
	}

	st := svc[ssoServiceName]
	if st == "" {
		return fmt.Errorf("mimo: SSO 链走完没换到 serviceToken（passToken 多半已过期，去桌面端重新登录一次再导）")
	}

	a.mu.Lock()
	a.ServiceToken = st
	if v := svc["mimopc_slh"]; v != "" {
		a.Slh = v
	}
	if v := svc["mimopc_ph"]; v != "" {
		a.Ph = v
	}
	if a.UID == "" {
		if v := svc["userid"]; v != "" {
			a.UID = v
		}
	} else if v := svc["userid"]; v != "" && v != a.UID {
		return fmt.Errorf("mimo: SSO 链返回的 userId(%s) 与凭证(%s)不一致，拒绝更新（防串号）", v, a.UID)
	}
	// passToken 被 serviceLogin 续签 → 滚动保存新值（30 天续命）。
	if v := acct["passToken"]; v != "" && v != a.PassToken {
		a.PassToken = v
	}
	if v := acct["cUserId"]; v != "" {
		a.CUserID = v
	}
	if v := acct["deviceId"]; v != "" {
		a.DeviceD = v
	}
	a.ServiceAt = time.Now().Unix()
	a.mu.Unlock()
	return nil
}
