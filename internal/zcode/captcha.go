// captcha.go JWT 通道的阿里云无痕验证码求解。
//
// # 这是本上游唯一"无法纯后端完成"的部分
//
// JWT 通道的每个模型请求都要带一个**新生成**的验证码参数
// （`X-Aliyun-Captcha-Verify-Param`），而它的生成必须执行
// AliyunCaptcha.js 里的无痕验证流程：
//
//	POST /api/v1/client/configs  → 拿 {prefix, region, sceneId}（免鉴权公开）
//	页面加载 AliunCaptcha.js     → startTracelessVerification()
//	回调 success(param)          → param 就是要塞进请求头的值
//
// ⚠ **官方仓库搜 captcha / aliyun 零命中** —— 说明这是纯第三方逆向出来的机制，
// 不是官方支持的做法。它的失效风险显著高于本包其它部分，所以：
//
//  1. 求解失败要**如实报错**，绝不静默去掉请求头（那会被上游当风控）
//  2. 参数要**缓存**（参照实现实测有效 45s，宽限 300s）
//  3. 求解器做成接口 —— 换实现不用改调用方
//
// # 为什么不用 go-rod（参照项目的做法）
//
// 参照项目 `zcode-proxy` 引入 go-rod 驱动真实 Chrome，注释说
// "捆绑 Chromium 会被风控识别"。那是**正确的做法**，但它带来：
//
//	go-rod + launcher 依赖树（本仓现在只有 2 个直接依赖）
//	部署机上必须有真 Chrome/Edge
//	无 GUI 环境（服务器/容器）无法用
//
// 本仓的判据是"对齐其它上游的做法"（用户明确要求）：其它上游
// （含 raccoon 最近的浏览器登录）都是**用系统浏览器打开一个本机回环页面 + 收回调**。
// 本文件沿用同一模式，代价是需要一次人工交互，但零新依赖、服务器也能用。
//
// ⚠ 已知限制（诚实标注）：阿里云无痕验证会校验来源域。本方案把页面放在
// `http://127.0.0.1:PORT`，而配置里的 prefix/sceneId 是给 zcode.z.ai 签发的。
// 若上游校验来源，求解会失败并得到明确的 fail/error 回调 —— 这时**唯一的
// 可行路径是改用 API Key 通道**（不需要验证码）。失败信息里会这样说。
package zcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CaptchaSolver 验证码求解器。
//
// 实现者只需回答一个问题："给我一个当前有效的验证码参数。"
// 缓存、并发去重、失败上报都由调用方（cachedSolver）负责。
type CaptchaSolver interface {
	// Solve 求解一次，返回可塞进请求头的 param。
	//
	// region 是当前凭证绑定的区域（"cn" / "sg"），
	// 它必须与签发时一致 —— 所以由调用方传入而不是实现自己猜。
	Solve(ctx context.Context, region string) (string, error)
}

// CaptchaConfig 上游签发的验证码配置（GET /api/v1/client/configs）。
//
// 实测值（2026-10-05 线上）：
//
//	{"enabled":true,"prefix":"no8xfe","region":"cn","sceneId":"11xygtvd","skip_model_request":true}
//
// ⚠ 注意 `skip_model_request: true` —— 字面意思是"模型请求跳过验证码"。
// 官方源码里搜不到这个字段（零命中），所以**不能据此假定 JWT 通道不需要验证码**。
// 本包的做法是：仍然求解并发送；求解不可用时如实报错（不赌那个字段的含义）。
// 若将来实测证明真的不需要，去掉 attachCaptcha 的调用即可。
type CaptchaConfig struct {
	Enabled bool   `json:"enabled"`
	Prefix  string `json:"prefix"`
	Region  string `json:"region"`
	SceneID string `json:"sceneId"`
}

// DefaultCaptchaConfig 实测拷下来的线上配置（取不到实时配置时的兜底）。
//
// 为什么要有兜底：那个端点虽然免鉴权，但它是**另一个**外部依赖 ——
// 它挂掉不该让整个 zcode 上游的 JWT 通道瘫痪。
var DefaultCaptchaConfig = CaptchaConfig{
	Enabled: true,
	Prefix:  "no8xfe",
	Region:  "cn",
	SceneID: "11xygtvd",
}

// captchaTTL 参数的有效期。
//
// 45 秒抄自参照实现（zcode-proxy captcha.go 的注释："参数按出口代理分组缓存 45s"）。
// 太短会导致频繁求解（每次都弹浏览器），太长则会用到已失效的参数
// —— 后者更糟，因为它表现为"偶发的鉴权失败"，极难归因。
const captchaTTL = 45 * time.Second

// captchaGrace 宽限期：TTL 过期后仍可复用旧值的时间窗。
//
// 300 秒抄自参照实现（"过期后 300s 宽限期内返回旧参数并后台刷新"）。
// 为什么需要它：求解要弹浏览器，可能花十几秒。若严格按 TTL 判，
// 用户正要发请求时刚好过期，就得干等一次求解 —— 而旧参数往往还没真失效。
const captchaGrace = 300 * time.Second

// cachedSolver 给任意 CaptchaSolver 加上缓存与并发去重。
type cachedSolver struct {
	inner CaptchaSolver

	mu       sync.Mutex
	param    string
	region   string
	issuedAt time.Time
	inflight chan struct{} // 非 nil = 已有一次求解在跑，其余请求等它
}

// NewCachedSolver 包一层缓存。
func NewCachedSolver(inner CaptchaSolver) CaptchaSolver {
	if inner == nil {
		return nil
	}
	return &cachedSolver{inner: inner}
}

// Solve 取一个参数：命中缓存就直接给，否则求解（并发去重）。
func (s *cachedSolver) Solve(ctx context.Context, region string) (string, error) {
	s.mu.Lock()
	// 区域变了 → 旧参数不可用（区域必须与签发时一致）。
	if s.param != "" && s.region == region {
		age := time.Since(s.issuedAt)
		if age < captchaTTL {
			p := s.param
			s.mu.Unlock()
			return p, nil
		}
		if age < captchaTTL+captchaGrace {
			// 宽限期内：返回旧值，同时后台刷新。
			// 这样调用方**永远不用等**求解，代价是这次可能用到即将失效的值。
			p := s.param
			s.mu.Unlock()
			s.refreshInBackground(region)
			return p, nil
		}
	}
	// 已经有一次求解在跑 → 等它，不重复弹浏览器。
	// 为什么必须去重：并发请求会各弹一次浏览器，用户会看到一堆窗口。
	if s.inflight != nil {
		ch := s.inflight
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return s.Solve(ctx, region)
	}
	ch := make(chan struct{})
	s.inflight = ch
	s.mu.Unlock()

	p, err := s.inner.Solve(ctx, region)

	s.mu.Lock()
	if err == nil && p != "" {
		s.param, s.region, s.issuedAt = p, region, time.Now()
	}
	s.inflight = nil
	s.mu.Unlock()
	close(ch)

	if err != nil {
		return "", err
	}
	if p == "" {
		return "", errors.New("zcode: 验证码求解器返回了空参数")
	}
	return p, nil
}

// refreshInBackground 后台刷新（宽限期内用）。
func (s *cachedSolver) refreshInBackground(region string) {
	s.mu.Lock()
	if s.inflight != nil {
		s.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	s.inflight = ch
	s.mu.Unlock()

	go func() {
		// 后台刷新给一个独立的超时预算：它不占用任何请求的时间。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		p, err := s.inner.Solve(ctx, region)
		s.mu.Lock()
		if err == nil && p != "" {
			s.param, s.region, s.issuedAt = p, region, time.Now()
		}
		s.inflight = nil
		s.mu.Unlock()
		close(ch)
	}()
}

// ---- 本机浏览器求解器（对齐本仓其它上游的做法）----

// BrowserSolver 用**系统浏览器**打开一个本机回环页面求解。
//
// 流程（与 raccoon 的浏览器登录同一模式）：
//
//	起本机回环端口（net.Listen "127.0.0.1:0"）
//	→ 用系统浏览器打开 http://127.0.0.1:PORT/（页面内嵌阿里云 SDK）
//	→ SDK 跑无痕验证，成功时把 param POST 回本机端口
//	→ 我们拿到 param，关闭监听
//
// 为什么不用内嵌浏览器：见文件头的"为什么不用 go-rod"。
type BrowserSolver struct {
	// Open 打开浏览器（注入以便测试；nil = 用 browseropen 的默认行为）。
	Open func(url string) error
	// Timeout 单次求解的总超时（默认 3 分钟 —— 要留出用户手动操作的时间）。
	Timeout time.Duration
	// Config 验证码配置（空 = 用 DefaultCaptchaConfig）。
	Config CaptchaConfig
}

// Solve 起回环页面、等回调、返回 param。
func (s *BrowserSolver) Solve(ctx context.Context, region string) (string, error) {
	cc := s.Config
	if cc.SceneID == "" {
		cc = DefaultCaptchaConfig
	}
	if region != "" {
		cc.Region = region
	}
	if s.Open == nil {
		return "", errors.New("zcode: 未注入浏览器打开函数（BrowserSolver.Open 为空）")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("zcode: 起验证码回环端口失败: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// 一次性结果通道（缓冲 1：回调方不该因为没人收而阻塞）。
	// 一次性 nonce 防止本机其它进程伪造回调（虽然只在回环，但成本极低）。
	nonce := randomUUID()
	type result struct {
		param string
		err   string
	}
	done := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, captchaPage(cc, nonce))
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/param", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		var msg struct {
			Nonce string `json:"nonce"`
			Param string `json:"param"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if msg.Nonce != nonce {
			// 不匹配的 nonce 直接拒，且**不消费**这次求解。
			http.Error(w, "bad nonce", http.StatusBadRequest)
			return
		}
		select {
		case done <- result{param: msg.Param, err: msg.Error}:
		default: // 已经收到过结果，忽略重复回调
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if err := s.Open(pageURL); err != nil {
		return "", fmt.Errorf("zcode: 打开验证码页面失败（%s）: %w；"+
			"若本机无图形环境，请改用 API Key 通道（不需要验证码）", pageURL, err)
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", fmt.Errorf("zcode: 验证码求解超时（%s）—— 未收到回调。"+
			"若上游校验了页面来源域，本机回环页面会被拒；"+
			"此时请改用 API Key 通道（%s%s/chat/completions，不需要验证码）",
			timeout, DefaultOriginZAI, pathPaaS)
	case res := <-done:
		if res.err != "" {
			return "", fmt.Errorf("zcode: 验证码被上游拒绝: %s；"+
				"这通常意味着来源域校验不通过 —— 请改用 API Key 通道", res.err)
		}
		if strings.TrimSpace(res.param) == "" {
			return "", errors.New("zcode: 验证码回调里没有 param")
		}
		return res.param, nil
	}
}

// captchaPage 生成验证码页面（内嵌阿里云 SDK）。
//
// ⚠ 页面结构抄自参照实现（zcode-proxy captcha.go 的 captchaHTML），
// 因为初始化参数的**键名与回调形态是逆向出来的**，自己写必然对不上：
//
//	SceneId / region / prefix 三个值必须原样传（大小写敏感）
//	getInstance 里要调 startTracelessVerification（无痕），失败退回 show（弹窗）
//	success 回调收到的 param 才是要放进请求头的值
//
// 所有插值都过 html.EscapeString / json.Marshal，避免配置里带引号时破坏页面。
func captchaPage(cc CaptchaConfig, nonce string) string {
	js := func(v string) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	// 回调到本机端口；带上 nonce 防止别的进程伪造。
	report := func(kind string) string {
		return `fetch('/param', {method:'POST', headers:{'Content-Type':'application/json'},
  body: JSON.stringify(Object.assign({nonce: ` + js(nonce) + `}, ` + kind + `))})`
	}
	return `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8">
<title>ZCode 验证码</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;max-width:640px;margin:60px auto;padding:0 20px;color:#1a1a1a}
h1{font-size:19px} p{line-height:1.7;color:#444} code{background:#f4f4f5;padding:2px 6px;border-radius:4px;font-size:13px}
#status{margin-top:24px;padding:14px 16px;border-radius:8px;background:#f4f4f5;font-size:14px}
.ok{background:#e7f8ed !important;color:#12683a} .bad{background:#fdecec !important;color:#8a1c1c}
</style></head><body>
<h1>ZCode 验证码求解</h1>
<p>本页面正在向上游申请一次性验证参数。若浏览器弹出滑块，请完成它。</p>
<p>参数拿到后本页会自动关闭，无需手动操作。</p>
<div id="status">正在初始化…</div>
<button id="btn" style="display:none"></button>
<div id="cap"></div>
<script src="https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"></script>
<script>
var statusEl = document.getElementById('status');
function report(obj) { ` + report(`obj`) + `; }
window.initAliyunCaptcha({
  SceneId: ` + js(cc.SceneID) + `, mode: 'popup', region: ` + js(cc.Region) + `, prefix: ` + js(cc.Prefix) + `,
  element: '#cap', button: '#btn', captchaLogoImg: '', showErrorTip: false,
  getInstance: function (inst) {
    var fn = inst.startTracelessVerification || inst.show;
    try { fn.call(inst); }
    catch (e) { statusEl.className = 'bad';
      statusEl.textContent = '启动失败: ' + (e && e.message || e);
      report({error: String(e && e.message || e)}); }
  },
  success: function (param) {
    statusEl.className = 'ok'; statusEl.textContent = '已拿到参数，正在提交…';
    report({param: param});
    setTimeout(function(){ window.close(); }, 600);
  },
  fail: function (m) { statusEl.className = 'bad'; statusEl.textContent = '验证失败: ' + m;
    report({error: String(m)}); },
  onError: function (m) { statusEl.className = 'bad'; statusEl.textContent = '出错: ' + m;
    report({error: String(m)}); }
});
</script></body></html>`
}

// FetchCaptchaConfig 取上游的实时验证码配置。
//
// 端点免鉴权（实测 HTTP 200），所以不需要凭证。
// 取不到时由调用方决定用 DefaultCaptchaConfig 兜底。
func FetchCaptchaConfig(ctx context.Context, hc *http.Client) (CaptchaConfig, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		planOrigin+"/api/v1/client/configs?app_version="+defaultAppVersion, nil)
	if err != nil {
		return CaptchaConfig{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return CaptchaConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return CaptchaConfig{}, fmt.Errorf("zcode: 取验证码配置 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return CaptchaConfig{}, err
	}
	var env struct {
		Data struct {
			Configs struct {
				Captcha CaptchaConfig `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return CaptchaConfig{}, err
	}
	cc := env.Data.Configs.Captcha
	if cc.SceneID == "" {
		return CaptchaConfig{}, errors.New("zcode: 上游配置里没有 captcha.sceneId")
	}
	return cc, nil
}
