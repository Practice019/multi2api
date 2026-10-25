// provider.go Verdent 作为反代项目的一个上游。
//
// # 这个上游的特殊之处：我们没有它的登录协议
//
// 其余账号型上游（workbuddy / codearts / monkeycode…）都能"网关内点一下加账号"
// —— 它们有可自动化的登录面（OAuth / 扫码 / 浏览器驱动）。
// Verdent **没有**：它的 access token 只在桌面端主进程内存里，不落盘、
// 也没有可脚本化的登录端点（见 auth.go 的说明）。
//
// 所以本上游的账号来源是**手工投放**：把人从别处拿到（DevTools / 抓包 /
// register-machine 导出）的 token 写成 auths/verdent/verdent-<uid>.json，
// 然后点「重载 auths」。这是**刻意不实现 LoginFlow**，不是漏了。
//
// # 实现了哪些扩展点（每一个都有具体用途，没有一个是"顺手加的"）
//
//	CredentialLoader / CredentialSecretLoader  重载 auths 能扫到本上游（否则静默无效）
//	AuthDirExt                                  凭证目录 = auths/verdent（读与写问同一个）
//	DisplayNameExt                              面板上显示「Verdent」而不是裸 id
//	HealthProbeExt                              被冷却/禁用后能被探活恢复（额度端点最便宜）
package verdent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/gateway"
)

// 编译期断言：本上游实现了哪些契约与扩展点。
//
// ⚠ 这些断言不是装饰：gateway.ExtOf[T] 是**方法集精确匹配**的类型断言，
// 少一个方法（哪怕只是名字不同）都会**静默**断言失败 ——
// 表现是"重载 auths 扫不到这个上游"或"面板显示裸 id"，都不报错。
// （写这份代码时真的踩了：AuthDirExt 要的是 AuthDir()，我因为字段占了名字
//
//	写成了 AuthDirPath()，而 +build 是通过的 —— 是下面这行断言把它挡住的。）
var (
	_ gateway.Provider               = (*Provider)(nil)
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)
	_ gateway.AuthDirExt             = (*Provider)(nil)
	_ gateway.DisplayNameExt         = (*Provider)(nil)
	_ gateway.HealthProbeExt         = (*Provider)(nil)
)

// Provider Verdent 上游。
//
// 字段在构造期确定，之后只读 —— 与其余账号型上游一样是长生命周期对象，
// 可被多个请求并发使用（HTTP 客户端本身并发安全）。
type Provider struct {
	// Client 出站客户端。nil = 用包级默认。
	Client *Client
	// Dir 本上游的凭证目录（默认 ./auths/verdent）。
	//
	// ⚠ 字段名是 Dir 而不是 AuthDir —— AuthDirExt 要求一个**同名方法**，
	// 而 Go 里字段与方法不能重名。叫 AuthDir 会编译不过（这次是断言先发现的）。
	Dir string

	// mu 保护下面两个注入的访问器（登录/额度探测会在运行时读它们）。
	mu sync.RWMutex
	// credSrc 活的凭证来源（由装配层注入）。
	//
	// # 为什么需要它（而不是自己 LoadDir）
	//
	// 账号池**保管**着活凭证（SyncToDirWithSecrets 装进去的那个 *Auth）。
	// 刷新额度、页内添加账号这类动作必须拿**池里那一份** ——
	// 自己 LoadDir 会造出第二个 *Auth 实例，两边一旦分叉
	//（磁盘被改、池里还是旧的），就会出现"界面上额度是新的、对话用的还是旧 token"
	// 这种无法解释的现象。上游不得 import pool，所以由装配层适配进来。
	credSrc func(uid string) (gateway.Credential, bool)
	// uidEnumerator 列出全部 uid（批量额度刷新用）。
	uidEnumerator func() []string
	// logins 未完成的登录会话（state → 创建时刻）。
	//
	// 本上游的"登录"就是一次粘贴，state 的作用只是"这次粘贴属于哪次点击添加账号" ——
	// 它不参与校验（凭证能否用由上游回答，不由 state 回答）。
	logins map[string]loginSession
}

// SetCredentialSource 装配活凭证来源（由 cmd/server 注入）。
func (p *Provider) SetCredentialSource(fn func(uid string) (gateway.Credential, bool)) {
	p.mu.Lock()
	p.credSrc = fn
	p.mu.Unlock()
}

// SetUIDEnumerator 装配 uid 枚举来源（由 cmd/server 注入）。
func (p *Provider) SetUIDEnumerator(fn func() []string) {
	p.mu.Lock()
	p.uidEnumerator = fn
	p.mu.Unlock()
}

// authOfUID 取某 uid 的活凭证。
//
// 顺序：先问注入的凭证源（生产路径），再回落 LoadDir（测试 / 未注入时）。
// 反过来会让生产永远读到磁盘副本（见 credSrc 的注释）。
func (p *Provider) authOfUID(uid string) *Auth {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return nil
	}
	p.mu.RLock()
	src := p.credSrc
	p.mu.RUnlock()
	if src != nil {
		if c, ok := src(uid); ok {
			if a, err := authOf(c); err == nil {
				return a
			}
		}
	}
	// 回落：扫目录（未注入凭证源时的测试路径）。
	list, err := LoadDir(p.AuthDir())
	if err != nil {
		return nil
	}
	for _, a := range list {
		if a.UID() == uid {
			return a
		}
	}
	return nil
}

// UIDs 列出本上游全部账号 uid（批量刷新额度用）。
func (p *Provider) UIDs() []string {
	p.mu.RLock()
	en := p.uidEnumerator
	p.mu.RUnlock()
	if en != nil {
		return en()
	}
	list, err := LoadDir(p.AuthDir())
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.UID())
	}
	return out
}

// New 构造一个上游实例。
func New(authDir string) *Provider {
	return &Provider{Client: NewClient(), Dir: strings.TrimSpace(authDir)}
}

// NewProvider 无依赖构造（契约测试与 allUpstreams 用）。
//
// 与 New 的区别：不绑定任何目录 —— 契约测试不该依赖磁盘状态。
func NewProvider() *Provider { return New("") }

// ID 上游标识。
func (p *Provider) ID() string { return ProviderID }

// Caps 声明能力位。
//
// 只声明 Chat 与 Models：
//   - 没有 Checkin —— Verdent 没有签到动作可做
//   - 没有 CredentialRefresher —— token 无法续期（见文件头），续期能力不该虚报。
//     ⚠ 虚报的后果不是"多一个按钮"，而是 pool 的「能续期 ⇒ 必须有后台续期任务」
//     守卫会被触发，要求我们注册一个根本没法实现的续期任务。所以如实不声明。
func (p *Provider) Caps() gateway.Capability {
	return gateway.CapChat | gateway.CapModels | gateway.CapQuotaProbe | gateway.CapImport
}

// AuthDir 凭证目录（AuthDirExt）。
func (p *Provider) AuthDir() string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Dir)
}

// DisplayName 面板上的中文名（DisplayNameExt）。
func (p *Provider) DisplayName() string { return "Verdent" }

// authOf 从凭证里取出 *Auth。
//
// 核心**不解释** Secret（它是 any），断言回本包的类型是这里的职责。
// 失败时返回可读错误 —— 最常见的成因是"池里的号没有 secret"（装配层没接
// LoadCredentialsWithSecrets，见 extension.go 的 CredentialSecretLoader）。
func authOf(cred gateway.Credential) (*Auth, error) {
	switch v := cred.Secret.(type) {
	case *Auth:
		if v == nil || !v.Usable() {
			return nil, fmt.Errorf("verdent: 凭证 %s 里没有可用的 token", cred.UID)
		}
		return v, nil
	case Auth:
		a := v
		if !a.Usable() {
			return nil, fmt.Errorf("verdent: 凭证 %s 里没有可用的 token", cred.UID)
		}
		return &a, nil
	case nil:
		return nil, fmt.Errorf("verdent: 凭证 %s 没有 secret（装配层是否接了 LoadCredentialsWithSecrets？）", cred.UID)
	default:
		return nil, fmt.Errorf("verdent: 凭证 %s 的 secret 类型是 %T，不是 *Auth", cred.UID, cred.Secret)
	}
}

// buildPayload 拼上游要的请求体。
//
// # 为什么抽成独立函数（而不是写在 Chat 里）
//
// 它必须**逐字段**等于参考客户端（register-machine 的 verdent-client.mjs）——
// 而我第一版就漏了 temperature 与 env.today_date 两处。
// 抽出来之后能对着"字段清单"写断言（见 payload_test.go），
// 否则下次改这里没人发现又少了一个。
//
// 它不发请求、不碰凭证 —— 纯函数，所以可以直接测。
func buildPayload(model, encSystem, encMessages string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"channel":    "deck",
		"model":      model,
		"session_id": "session_" + randUUID(),
		"conv_id":    "conv_" + randUUID(),
		"react_id":   "model_agent_" + randUUID(),
		"react_type": reactType, // ⚠ "Main Agent"，带空格
		"stream":     true,
		"max_tokens": 64000,
		// ⚠ temperature 与 env.today_date 是**补齐**的（2026-10-26，对照真实抓包）：
		//   参考客户端两个都带，我第一版漏了。漏了不影响能不能跑通
		//   （02:09 那次没带也成功生成了内容），但既然来源就是那个客户端，
		//   就没有理由偏离 —— 少一个字段就多一个"为什么我和它不一样"的变量。
		//
		// ⚠ 参考客户端**也没有** tools / tool_choice（那是桌面端 agent 才需要的
		//   94KB 加密工具定义），所以这里同样不发 —— 不做无意义的模仿。
		"temperature": 1,
		"system":      encSystem,
		"messages":    encMessages,
		"thinking":    map[string]any{"type": "enabled", "budget_tokens": 20000},
		"agent_name":  agentName,
		"env": map[string]any{
			"platform":   "win32",
			"os_version": "Windows_NT 10.0.22631",
			"shell":      "cmd",
			"today_date": time.Now().Format("2006-01-02"),
		},
		"encrypt":                   true,
		"custom_trace_tags_tmp":     []any{},
		"custom_trace_metadata_tmp": map[string]any{"selected_model_id": model, "effective_model_id": model},
		"model_catalog_version":     modelCatalogVersion,
		"effort":                    "high",
		"context_window_tokens":     1000000,
		"is_eco":                    false,
		"is_auto":                   false,
		"is_free":                   false,
		"is_limit_free":             false,
		"native_api":                false,
	})

}

// Chat 发一次对话（流式）。
//
// 流程：OpenAI 请求体 → 上游形状（system/messages 加密）→ POST → SSE 转回 OpenAI SSE。
//
// 非 2xx 时**照契约**把状态与响应体带回去（不返回 error）——
// 出口层要按状态码决定是不是换号重试，网络层失败才返回 error。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	system, messages, model, err := parseSystemAndMessages(body)
	if err != nil {
		return gateway.ChatStream{}, fmt.Errorf("verdent: %w", err)
	}
	if len(messages) == 0 {
		return gateway.ChatStream{}, fmt.Errorf("verdent: 请求体里没有可发送的消息")
	}

	// ★ system 必须含 "Verdent"，否则上游回一个伪装成 429 的校验失败。
	system = ensureVerdentSystem(system)
	encSystem, err := ProxyEncode([]any{map[string]any{"type": "text", "text": system}})
	if err != nil {
		return gateway.ChatStream{}, fmt.Errorf("verdent: 加密 system 失败: %w", err)
	}
	// ★ messages 加密的是**整个数组**（一个密文），不是逐条。
	encMessages, err := ProxyEncode(messages)
	if err != nil {
		return gateway.ChatStream{}, fmt.Errorf("verdent: 加密 messages 失败: %w", err)
	}

	payload, err := buildPayload(model, encSystem, encMessages)
	if err != nil {
		return gateway.ChatStream{}, fmt.Errorf("verdent: 构造请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, streamPath, bytes.NewReader(payload))
	if err != nil {
		return gateway.ChatStream{}, fmt.Errorf("verdent: 构造请求失败: %w", err)
	}
	// ★ proxyBetaHeader 缺了报 10004（实测）。
	req.Header = headersOf(a.Token, map[string]string{
		proxyBetaHeader: proxyBetaValue,
		"agent_type":    "ts_agent",
		"x-device-id":   defaultDeviceID,
		"x-team-id":     DefaultTeamID,
	})

	resp, err := p.client().httpClient().Do(req)
	if err != nil {
		return gateway.ChatStream{}, fmt.Errorf("verdent: 请求上游失败: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
		_ = resp.Body.Close()
		// 把上游的错误体原样交回（出口层按状态码/业务码判），只记一行日志。
		log.Printf("verdent: 上游 HTTP %d（uid=%s）：%s", resp.StatusCode, cred.UID, truncate(string(raw), errSnippet))
		return gateway.ChatStream{Status: resp.StatusCode, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	}
	return gateway.ChatStream{
		Status: resp.StatusCode,
		Body:   ConvertStreamToOpenAI(resp.Body, model),
	}, nil
}

// Models 模型目录。
//
// 实时拉取优先；拉不到（网络/凭证被拒）时**回落静态兜底** —— 不返回空目录。
//
// # 为什么不把失败直接抛出去
//
// 这个方法被 /v1/models 合并与面板展示调用，两者都只想要"有个目录能显示"。
// 抛错会让整个上游在界面上变成"没有模型"，而它其实只是**这一次**请求失败。
// 契约只要求"声明 CapModels 就必须返回非空目录"，兜底正好满足。
//
// ⚠ 但**日志必须记**：静默回落会让"上游其实已经不认这个 token 了"这件事
// 永远不可见 —— 那正是需要被发现的。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	a, err := authOf(cred)
	if err != nil {
		return KnownModels(), nil // 没有可用凭证：给兜底，不报错（理由见上）
	}
	list, err := p.client().FetchModels(ctx, a.Token)
	if err != nil {
		log.Printf("verdent: 拉模型目录失败，回落静态兜底（uid=%s）: %v", cred.UID, err)
		return KnownModels(), nil
	}
	return list, nil
}

// ProbeHealth 主动探活（HealthProbeExt）：用最便宜的额度端点。
//
// ⚠ 一定不能用 /llm/stream 探活：那会真的消耗一次对话额度，
// 而这个端点会被 10 分钟级的健康检查反复调用。
func (p *Provider) ProbeHealth(ctx context.Context, cred gateway.Credential) error {
	a, err := authOf(cred)
	if err != nil {
		return err
	}
	status, raw, err := p.client().doJSON(ctx, a.Token, usageURL, http.MethodGet, nil)
	if err != nil {
		return fmt.Errorf("verdent: 探活请求失败: %w", err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("verdent: token 被拒（HTTP %d）", status)
	}
	if status/100 != 2 {
		return fmt.Errorf("verdent: 探活 HTTP %d：%s", status, truncate(string(raw), errSnippet))
	}
	return nil
}

// client 取出站客户端（nil 安全）。
func (p *Provider) client() *Client {
	if p == nil || p.Client == nil {
		return defaultClient
	}
	return p.Client
}

// defaultClient 包级默认客户端（nil 接收者时用）。
var defaultClient = NewClient()

// LoadDir 读本上游凭证目录（供装配层与测试调用）。
func (p *Provider) LoadDir() ([]*Auth, error) { return LoadDir(p.AuthDir()) }

// SaveAuth 把一份凭证写成**规范形态**落盘（导入用）。
//
// 目录不存在时创建（auths/verdent/ 在首次导入前通常不存在）。
func (p *Provider) SaveAuth(a *Auth, remark string) (string, error) {
	if a == nil || !a.Usable() {
		return "", fmt.Errorf("verdent: 凭证不可用（token 为空或不是合法 JWT）")
	}
	dir := p.AuthDir()
	if dir == "" {
		return "", fmt.Errorf("verdent: 未配置凭证目录")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("verdent: 创建凭证目录 %s 失败: %w", dir, err)
	}
	raw, err := MarshalAuthFile(a, remark)
	if err != nil {
		return "", err
	}
	fp := filepath.Join(dir, a.FileName())
	// 先写临时文件再改名：直接覆盖时若中途失败，会留下一个半截的凭证文件 ——
	// 而 LoadDir 会**跳过**它（解析失败），表现是"账号莫名少了一个"。
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", fmt.Errorf("verdent: 写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, fp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("verdent: 改名到 %s 失败: %w", fp, err)
	}
	return fp, nil
}
