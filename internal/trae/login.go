// login.go TRAE 的页内登录（gateway.LoginFlow 实现）。
//
// # 流程（与 traework2api 的 login.sh 同一条路，只是搬进页面）
//
//	TRAE 的 OAuth 强制回调 127.0.0.1（本机打不开），所以流程是：
//
//	Start() → 生成 state + machine_id/device_id，返回**表单页地址**当 authURL
//	表单页  → 点「生成登录链接」→ POST /admin/trae/login/url 拿到登录 URL
//	         → 浏览器打开登录 → 复制地址栏回调链接 → 粘贴 → 「完成登录」
//	         → POST /admin/trae/login/finish → ExchangeToken → GetUserInfo → 就绪
//	Poll()  → 就绪就返回凭证（核心落盘 + 并池），否则 ErrLoginPending
//
// 与 loomy 的登录页同一套手法：表单页是网关自己服务的同源页面，
// 前端 `addAccount` 零改动（它本来就支持"authURL = 表单页"这种形态）。
package trae

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// loginPagePath 表单页路径。state 作为 query 传进去。
const loginPagePath = "/admin/trae/login"

// loginURLPath / loginFinishPath 表单页的两条 POST 路由。
const (
	loginURLPath    = "/admin/trae/login/url"
	loginFinishPath = "/admin/trae/login/finish"
)

// loginSessionTTL 一次进行中的登录会话保留多久（等用户走完浏览器流程）。
const loginSessionTTL = 15 * time.Minute

// loginFlow 实现 gateway.LoginFlow。
type loginFlow struct {
	p *Provider

	mu       sync.Mutex
	sessions map[string]*loginEntry
}

// loginEntry 一次进行中的登录。
type loginEntry struct {
	at time.Time

	machineID string
	deviceID  string
	traceID   string

	ready bool
	cred  gateway.Credential
}

var _ gateway.LoginFlow = (*loginFlow)(nil)

// LoginFlow 返回本上游的登录流程（缓存实例 —— 见 loomy 的同款注释）。
func (p *Provider) LoginFlow() (gateway.LoginFlow, bool) {
	if p == nil {
		return nil, false
	}
	p.loginOnce.Do(func() {
		p.loginCached = &loginFlow{p: p, sessions: make(map[string]*loginEntry)}
	})
	return p.loginCached, true
}

// Start 让 *Provider 直接满足 gateway.LoginFlow（ExtOf 是类型断言）。
func (p *Provider) Start() (string, string, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return "", "", errors.New("trae: 登录流程未初始化")
	}
	return f.(*loginFlow).Start()
}

// Poll 让 *Provider 直接满足 gateway.LoginFlow。
func (p *Provider) Poll(state string) (gateway.Credential, error) {
	f, ok := p.LoginFlow()
	if !ok {
		return gateway.Credential{}, errors.New("trae: 登录流程未初始化")
	}
	return f.(*loginFlow).Poll(state)
}

// Configured 报告这份部署**真的能**添加账号。
//
// TRAE 登录只需要构造 URL + 两个 POST 端点，无外部配置依赖 → 恒为 true
// （与 loomy 的短信登录同一判据："任何部署都能加账号"）。
func (p *Provider) Configured() bool { return p != nil }

// Configured 让 *loginFlow 满足 gateway.LoginFlow。
func (f *loginFlow) Configured() bool { return f != nil && f.p != nil }

// Start 建一次登录：生成设备对 + 返回表单页地址。
func (f *loginFlow) Start() (string, string, error) {
	state, err := newLoginState()
	if err != nil {
		return "", "", err
	}
	machineID, err := randomHex32()
	if err != nil {
		return "", "", err
	}
	deviceID, err := randomHex32()
	if err != nil {
		return "", "", err
	}
	traceID, err := randomHex16()
	if err != nil {
		return "", "", err
	}
	f.mu.Lock()
	f.gcLocked()
	f.sessions[state] = &loginEntry{
		at:        time.Now(),
		machineID: machineID,
		deviceID:  deviceID,
		traceID:   traceID,
	}
	f.mu.Unlock()
	return state, loginPagePath + "?state=" + state, nil
}

// Poll 取回已就绪的凭证；没就绪就是 pending。
func (f *loginFlow) Poll(state string) (gateway.Credential, error) {
	f.mu.Lock()
	e, ok := f.sessions[state]
	if !ok {
		f.mu.Unlock()
		return gateway.Credential{}, errors.New("trae: 登录会话不存在或已结束，请重新发起")
	}
	if !e.ready {
		f.mu.Unlock()
		return gateway.Credential{}, gateway.ErrLoginPending
	}
	delete(f.sessions, state)
	f.mu.Unlock()
	return e.cred, nil
}

// loginURL 生成一次登录的 URL（表单页第 1 步）。
func (f *loginFlow) loginURL(state string) (string, error) {
	f.mu.Lock()
	e, ok := f.sessions[state]
	f.mu.Unlock()
	if !ok {
		return "", errors.New("登录会话不存在或已结束，请回到控制台重新点「添加账号」")
	}
	return BuildLoginURL(e.machineID, e.deviceID, e.traceID), nil
}

// finish 用回调链接换 token（表单页第 2 步）。
func (f *loginFlow) finish(state, callback string) (*Auth, error) {
	info, err := ParseCallback(callback)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	e, ok := f.sessions[state]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("登录会话不存在或已结束，请回到控制台重新点「添加账号」")
	}
	if e.ready {
		return nil, errors.New("这个会话已经登录完成")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	host := f.p.client.OAuthHost
	a := &Auth{
		ApiHost:   host,
		Domain:    "trae.cn",
		MachineID: e.machineID,
		DeviceID:  e.deviceID,
	}
	if info.RefreshToken != "" {
		// 主路径：refreshToken → ExchangeToken（换 accessToken + 轮换 refreshToken）。
		a.RefreshToken = info.RefreshToken
		if err := f.p.client.RefreshToken(a); err != nil {
			return nil, fmt.Errorf("trae: ExchangeToken 失败: %w", err)
		}
	} else {
		// 兜底：回调里只有 userJwt.Token（无 refreshToken → 无法自动续期）。
		a.AccessToken = info.Token
		a.ExpiresAt = normalizeExpiresAtMilli(info.TokenExpireAt)
		if a.ExpiresAt <= 0 {
			a.ExpiresAt = time.Now().Add(14 * 24 * time.Hour).Unix()
		}
	}
	// 优先取回调 userInfo 的身份，再让 GetUserInfo 确认/补全（失败不阻塞）。
	a.UID = info.UserID
	a.Nickname = info.ScreenName
	a.EnterpriseID = info.TenantID
	if uid, nick, ent, uerr := f.p.client.GetUserInfo(ctx, a); uerr == nil && uid != "" {
		a.UID = uid
		if nick != "" {
			a.Nickname = nick
		}
		if ent != "" {
			a.EnterpriseID = ent
		}
	}
	if a.UID == "" {
		return nil, errors.New("trae: 未能从回调/GetUserInfo 拿到 uid，请确认回调链接完整")
	}

	f.mu.Lock()
	if cur, ok := f.sessions[state]; ok {
		cur.ready = true
		cur.cred = gateway.Credential{
			Provider: providerID,
			UID:      a.UID,
			Nickname: a.Nickname,
			// Secret 必须能落盘（核心 pollViaFlow 要求 MarshalAuthFile）。
			Secret: &authFile{a: a},
		}
	}
	f.mu.Unlock()
	return a, nil
}

// gcLocked 回收超时会话（调用方持锁）。
func (f *loginFlow) gcLocked() {
	for k, e := range f.sessions {
		if time.Since(e.at) > loginSessionTTL {
			delete(f.sessions, k)
		}
	}
}

// ── 表单页路由 ──────────────────────────────────────────────────────────

// handleLoginPage GET /admin/trae/login —— 登录表单页。
func (p *Provider) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(loginPageHTML))
}

// handleLoginURL POST /admin/trae/login/url —— 生成登录链接。
func (p *Provider) handleLoginURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeTraeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	f, ok := p.LoginFlow()
	if !ok {
		writeTraeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "登录流程未接线"})
		return
	}
	u, err := f.(*loginFlow).loginURL(strings.TrimSpace(req.State))
	if err != nil {
		writeTraeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeTraeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": u})
}

// handleLoginFinish POST /admin/trae/login/finish —— 回调链接换 token。
func (p *Provider) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State    string `json:"state"`
		Callback string `json:"callback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeTraeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "请求体不是合法 JSON"})
		return
	}
	f, ok := p.LoginFlow()
	if !ok {
		writeTraeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "登录流程未接线"})
		return
	}
	a, err := f.(*loginFlow).finish(strings.TrimSpace(req.State), req.Callback)
	if err != nil {
		writeTraeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// ⚠ 回执不回 token，只回身份（与 loomy 同一条：凭证由核心 poll 路径落盘）。
	writeTraeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"uid":      a.UID,
		"nickname": a.Nickname,
	})
}

// ── 登录页 HTML ─────────────────────────────────────────────────────────

const loginPageHTML = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TRAE 登录</title>
<style>
 body{margin:0;padding:32px;font:14px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif;
      background:#0f1115;color:#e6e8ee;display:flex;justify-content:center}
 .card{width:100%;max-width:560px;background:#171a21;border:1px solid #262b36;border-radius:12px;padding:24px}
 h1{margin:0 0 4px;font-size:17px}
 p.sub{margin:0 0 18px;color:#8b93a7;font-size:12px}
 .steps{margin:0 0 16px;padding-left:18px;color:#8b93a7;font-size:12.5px}
 .steps b{color:#e6e8ee}
 button{padding:9px 14px;border-radius:8px;border:1px solid #2c323f;background:#222835;color:#e6e8ee;
        font-size:13px;cursor:pointer}
 button.primary{background:#3b6cf0;border-color:#3b6cf0;color:#fff;font-weight:600;width:100%;margin-top:14px}
 button:disabled{opacity:.55;cursor:default}
 textarea{width:100%;box-sizing:border-box;padding:8px;border-radius:8px;border:1px solid #2c323f;
          background:#0f1115;color:#e6e8ee;font:12px ui-monospace,Consolas,monospace;resize:vertical}
 .urlbox{display:none;margin-top:14px}
 .urlbox .u{word-break:break-all;background:#0f1115;border:1px solid #262b36;border-radius:8px;
            padding:10px;font:12px ui-monospace,Consolas,monospace;color:#7ee2a8;max-height:120px;overflow:auto}
 .msg{margin-top:12px;padding:10px 12px;border-radius:8px;font-size:12px;display:none;white-space:pre-wrap}
 .ok{display:block;background:#12301f;border:1px solid #1e5c37;color:#7ee2a8}
 .bad{display:block;background:#301317;border:1px solid #5c1e26;color:#ff9aa5}
 .hint{margin-top:18px;padding-top:14px;border-top:1px solid #262b36;color:#8b93a7;font-size:11.5px}
</style></head><body>
<div class="card">
  <h1>TRAE SOLO 登录</h1>
  <p class="sub">登录成功后本页可以关闭，控制台会在几秒内自动完成添加。</p>
  <ol class="steps">
    <li>点下方<b>生成登录链接</b>，复制链接</li>
    <li>在浏览器打开链接，用手机号/验证码登录</li>
    <li>登录成功后浏览器会跳到打不开的 <b>127.0.0.1</b> 地址</li>
    <li>复制<b>浏览器地址栏的完整链接</b>，粘贴到下方输入框，点<b>完成登录</b></li>
  </ol>
  <button id="gen">生成登录链接</button>
  <div class="urlbox" id="urlbox">
    <div class="dim" style="font-size:11px;margin-bottom:4px">登录链接（复制到浏览器打开）：</div>
    <div class="u" id="url"></div>
    <label style="display:block;margin-top:14px;color:#8b93a7;font-size:12px">粘贴浏览器地址栏的回调链接：</label>
    <textarea id="cb" rows="2" spellcheck="false" placeholder="http://127.0.0.1:18080/authorize?refreshToken=..."></textarea>
    <button class="primary" id="fin">完成登录</button>
  </div>
  <div class="msg" id="msg"></div>
  <div class="hint">
    回调链接含凭证参数，粘贴框<b>不会回显到任何日志</b>；本页面的「生成登录链接」每次都会换一组新的设备标识。<br>
    如果控制台提示「等待授权…」请保持本页完成后再回去，控制台会自动落盘并添加账号。
  </div>
</div>
<script>
(function(){
  var $ = function(id){ return document.getElementById(id); };
  var state = new URLSearchParams(location.search).get('state') || '';
  function show(text, ok){ var m=$('msg'); m.textContent=text; m.className='msg '+(ok?'ok':'bad'); }
  function post(path, body){
    return fetch(path, { method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify(Object.assign({ state: state }, body)) })
      .then(function(r){ return r.json().then(function(j){
        if (!r.ok || !j.ok) throw new Error((j && j.error) || ('HTTP '+r.status)); return j; }); });
  }
  if (!state) show('缺少 state —— 请回到控制台重新点「添加账号」', false);
  $('gen').addEventListener('click', function(){
    var b=this; b.disabled=true; b.textContent='生成中…';
    post('/admin/trae/login/url', {}).then(function(j){
      $('url').textContent = j.url;
      $('urlbox').style.display = 'block';
      b.style.display = 'none';
      show('已生成登录链接，请复制并打开。', true);
    }).catch(function(e){ b.disabled=false; b.textContent='生成登录链接'; show('生成失败：'+e.message, false); });
  });
  $('fin').addEventListener('click', function(){
    var b=this; var cb=$('cb').value.trim();
    if (!cb) { show('请先粘贴回调链接', false); return; }
    b.disabled=true; b.textContent='登录中…';
    post('/admin/trae/login/finish', { callback: cb }).then(function(j){
      b.textContent='登录成功';
      show('登录成功：'+(j.nickname||j.uid)+'\n可以关闭本页了，控制台会自动完成添加。', true);
    }).catch(function(e){ b.disabled=false; b.textContent='完成登录'; show('登录失败：'+e.message, false); });
  });
})();
</script>
</body></html>`

// ── 凭证落盘包装 ────────────────────────────────────────────────────────

// authFile 让一份 *Auth 满足核心的落盘窄接口（MarshalAuthFile）。
type authFile struct{ a *Auth }

// MarshalAuthFile 返回 (文件名, 内容) —— 核心 pollViaFlow 的 authFileWriter 契约。
func (f *authFile) MarshalAuthFile() (string, []byte, error) {
	if f == nil || f.a == nil {
		return "", nil, errors.New("trae: 凭证为空")
	}
	raw, err := MarshalAuthFile(f.a)
	if err != nil {
		return "", nil, err
	}
	name := FileName(f.a)
	if name == "" {
		return "", nil, errors.New("trae: 凭证文件名为空")
	}
	return name, raw, nil
}

// ── 随机工具 ────────────────────────────────────────────────────────────

func newLoginState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomHex32() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomHex16() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
