// openai_compat.go OpenAI 兼容的「Key 型渠道」上游 Provider。
//
// # 一条实例 = 一条渠道
//
// 本仓的账号型上游（workbuddy / codearts / loomy / trae）都是"一堆号 + 选号池"：
// 账号在 auths/ 里，由 Pool 选号 / 惩罚 / 轮换。Key 型渠道没有"号"——
// 用户手填一个 Base URL 加一把 API Key 就是一条新上游。所以：
//
//	Base URL  → 构造时**烘焙进实例**（OpenAICompat.BaseURL）
//	API Key   → 每次调用从 gateway.Credential 取（装在 Secret 上，见 OpenAICompatSecret）
//	选号      → **不存在**：本文件不 import auths / pool，也不调用任何 Pool 方法
//
// # 为什么不进 Pool（这是设计决定，不是省事）
//
// 进 Pool 意味着"一把 key 被限流"会被 NoteError 记成"这个号坏了"，
// 从而触发换号 / 惩罚 / 冷却 —— 而 Key 型渠道只有一把 key，换无可换：
// 结果是"上游 429 一次，整条渠道被拉黑一段时间"，与事实完全不成比例。
// （对标文档：docs/design-channels.md §9、tasks/plan-channels-mapping.md 风险表那一行。）
//
// # 只做中转，不改协议
//
// Chat 把调用方给的 body **逐字节**送上去，只换 URL 与 Authorization 头。
// 请求体改写（提示词替换 / 强制 stream / 反指纹脱敏）是 workbuddy 那条账号型
// 路径的**私有动作**（见 wire_reexport.go 的说明），不得作用在这里：
// Key 型渠道对接的是任意兼容上游，它们既不认识我们的字段白名单，
// 也不该被我们的指纹规则改写。
//
// # 与 Provider 接口注释的一点偏差（有意，写在这里免得后人以为写错了）
//
// gateway.Provider 的注释说"非 2xx 也返回 ChatStream，只有网络层失败才返回 error"。
// 本类型**不这么做**：Key 型渠道没有"换个号再试"这回事（它不进 Pool），
// 非 2xx 就是一个确定的失败。若把它当普通流返回，调用方很可能把它当成
// "上游的 SSE" 直接透传给客户端 —— 客户端收到一个 HTTP 200 的空流，
// 比我们显式报错更难查。所以这里返回**带状态码与响应体片段**的 error。
// 契约允许这么做：Chat 返回 error 时不再检查响应体是否为空。
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gw "workbuddy2api/internal/gateway"
)

// 两个端点都挂在渠道的 Base URL 之后（OpenAI 兼容线协议）。
//
// ⚠ Base URL 里**已经含** /v1 是常态（用户手填的是 "https://api.x.com/v1"），
// 所以这里只拼 /chat/completions 与 /models，不做任何版本段推断。
const (
	compatChatPath   = "/chat/completions"
	compatModelsPath = "/models"
)

// compatErrSnippet 错误信息里回带的上游响应体长度上限（字节）。
//
// 为什么必须截断：非 2xx 的响应体有时是一整页 HTML —— 实测 workbuddy 上游的
// 401 就是 openresty/APISIX 的错误页（见 health.go 的实测记录），Key 型渠道
// 上游同样可能是网关页。而这条 error 会经调用方、日志、界面三处传递，
// 整页塞进去会把日志淹掉。几百字节足够看清 code / message。
const compatErrSnippet = 300

// compatBodyLimit 读上游响应体的硬上限（1MiB）。与 client.go 里既有的
// `io.LimitReader(resp.Body, 1<<20)` 同口径：上游返回超大页时不能让我们 OOM。
const compatBodyLimit = 1 << 20

// OpenAICompat 一条 OpenAI 兼容的 Key 型渠道。一个实例 = 一条手动新增的渠道。
//
// 并发安全：三个字段都是构造期确定的只读值（HTTP 也是并发安全的 http.Client），
// 所以一个实例可以同时被多个请求使用 —— 与账号型 Provider 一样是长生命周期对象。
type OpenAICompat struct {
	// Name 渠道标识，即 Provider.ID() 的返回值。
	// 必须是 ^[a-z][a-z0-9-]*$（它会作为模型名前缀出现，见 gateway.validProviderID）。
	Name string
	// BaseURL 上游基址（如 "https://api.example.com/v1"）。
	//
	// 为什么在实例上而不是在 Credential 里：一个实例就是一条渠道，
	// 基址是这条渠道的身份，不是"某个账号的凭证"。放 Credential 会让
	// "同一个渠道的凭证"与"渠道本身"混在一起，也没法在构造期就报"没填基址"。
	// 尾斜杠会被吃掉（"…/v1/" 与 "…/v1" 等价），见 compatURL。
	BaseURL string
	// HTTP 出站客户端。nil = 用包级默认 compatHTTPDefault。
	// 测试注入 httptest 的 client 或短超时 client 用得上（与 client.go 的 HTTP 字段同惯例）。
	HTTP *http.Client
}

// NewOpenAICompat 构造一条 Key 型渠道。
//
// name 是渠道标识（ID()），baseURL 是上游基址。两者都只做 trim，
// **不做格式校验**：ID 的合法性由核心契约（RunProviderContract）与注册表判定，
// 基址的合法性由 compatURL 在真正发请求前判定（那时才能给出可读的报错）。
//
// 为什么构造期就把 baseURL 里的尾斜杠吃掉：拼 URL 的地方有两处（chat / models），
// 只在拼的时候处理就有"改一处漏一处"的风险（本仓真的踩过，见 webui 面板的教训）。
// 归一化只在构造与 baseURL() 一处做。
func NewOpenAICompat(name, baseURL string) *OpenAICompat {
	return &OpenAICompat{
		Name:    strings.TrimSpace(name),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
}

// ID 渠道的唯一标识。
//
// nil 接收者也返回空串而不是 panic：契约要求"任何输入都不得 panic"，
// 而一个 nil 的渠道指针恰是最容易被调用方漏判的那种输入。
func (p *OpenAICompat) ID() string { return p.name() }

// Caps 声明能力位。
//
// 两个都真实现，所以两个都声明（契约会验证"声明了就真的能用"：
// 声明 CapModels 就必须返回非空目录，见 contract.go 的 capabilityProbes）。
func (p *OpenAICompat) Caps() gw.Capability { return gw.CapChat | gw.CapModels }

// Chat 转发一次对话（流式）。
//
// body **原样透传**（逐字节）：只换 URL、加 Authorization 与两个协议头。
// 上游 2xx 时把响应体作为 ChatStream.Body 返回（**调用方负责 Close**）；
// 非 2xx / 传输失败 / 凭证缺失一律显式返回 error。
func (p *OpenAICompat) Chat(ctx context.Context, cred gw.Credential, body []byte) (gw.ChatStream, error) {
	const op = "chat"
	key, err := compatKey(cred)
	if err != nil {
		return gw.ChatStream{}, p.wrap(op, err)
	}
	url, err := p.compatURL(compatChatPath)
	if err != nil {
		return gw.ChatStream{}, p.wrap(op, err)
	}

	// NewRequestWithContext 是"取消能被尊重"的唯一来源：调用方断开后，
	// 上游连接由 ctx 直接掐掉，而不是等我们自己发现（契约会验证这一条）。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return gw.ChatStream{}, p.wrap(op, fmt.Errorf("构造请求失败: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	// 只声明"我要流"，不主动加 stream:true：body 里有没有 stream 由调用方决定，
	// 我们改它就不是"原样透传"了（上面包注释里那条硬约束）。
	req.Header.Set("Accept", "text/event-stream")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return gw.ChatStream{}, p.wrap(op, fmt.Errorf("请求上游失败: %w", err))
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, compatBodyLimit))
		_ = resp.Body.Close()
		return gw.ChatStream{}, p.statusErr(op, resp.StatusCode, raw)
	}
	return gw.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
}

// Models 拉取该渠道的模型目录（GET {BaseURL}/models）。
//
// 把上游的 data[].id 投影成 gateway.ModelInfo：ID 必填，其余字段有就填。
// **空目录是显式错误**，不是空切片 —— 见函数末尾的说明。
func (p *OpenAICompat) Models(ctx context.Context, cred gw.Credential) ([]gw.ModelInfo, error) {
	const op = "models"
	key, err := compatKey(cred)
	if err != nil {
		return nil, p.wrap(op, err)
	}
	url, err := p.compatURL(compatModelsPath)
	if err != nil {
		return nil, p.wrap(op, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, p.wrap(op, fmt.Errorf("构造请求失败: %w", err))
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient().Do(req)
	if err != nil {
		return nil, p.wrap(op, fmt.Errorf("请求上游失败: %w", err))
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, compatBodyLimit))
		_ = resp.Body.Close()
		return nil, p.statusErr(op, resp.StatusCode, raw)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, compatBodyLimit))
	_ = resp.Body.Close()
	if rerr != nil {
		return nil, p.wrap(op, fmt.Errorf("读上游响应体失败: %w", rerr))
	}

	var env struct {
		Data []compatModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		// 报错里只带**状态与片段**，不带整个响应体；解码错误本身不带凭证。
		return nil, p.wrap(op, fmt.Errorf("解析模型目录失败: %w（响应体片段：%s）",
			err, truncate(string(raw), compatErrSnippet)))
	}

	out := make([]gw.ModelInfo, 0, len(env.Data))
	for _, m := range env.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue // 没有 id 的条目是无意义的噪音，跳过而不是当成模型
		}
		out = append(out, gw.ModelInfo{
			ID:              id,
			ContextWindow:   firstNonZero(m.ContextWindow, m.ContextLength),
			MaxOutputTokens: firstNonZero(m.MaxOutputTokens, m.MaxTokens),
		})
	}
	// 空目录 ⇒ 显式 error，而不是返回空切片。
	//
	// 理由与契约一致（contract.go 的 verifyModelsNonEmpty）：声明了 CapModels
	// 却返回空列表，等于没实现 —— 它会让 /v1/models 静默变空，而调用方
	// 分不清"这个渠道没有模型"与"我们没实现"。返回 error 至少能让管理层
	// 在"拉到 0 个模型"时给出可读提示（用户手填错基址时最常见就是这个）。
	if len(out) == 0 {
		return nil, p.wrap(op, errors.New("上游模型目录为空（data 里没有任何带 id 的条目）"))
	}
	return out, nil
}

// compatModel 上游 /models 的一个条目。
//
// 只解析"真的会填进 gateway.ModelInfo"的字段。上下文窗口在兼容实现里有两种
// 常见写法（context_window / context_length），两个都读：
// 这一层**没有**统一标准（OpenAI 官方在这个端点上并不返回它们），
// 少读一个就等于把元信息丢了，而多读一个是零成本的。
type compatModel struct {
	ID string `json:"id"`
	// ContextWindow / ContextLength 同一个语义的两种写法，取先出现的非零值。
	ContextWindow int `json:"context_window"`
	ContextLength int `json:"context_length"`
	// MaxOutputTokens / MaxTokens 同上。
	MaxOutputTokens int `json:"max_output_tokens"`
	MaxTokens       int `json:"max_tokens"`
}

// firstNonZero 返回第一个非零值（没有则 0）。用于兼容别名字段。
func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// OpenAICompatSecret Key 型渠道的凭证内容。
//
// # 为什么装在 gateway.Credential.Secret 里（而不是给 Credential 加一个 APIKey 字段）
//
// gateway 的 Credential 注释写得很清楚：Provider / UID / Nickname / ExpiresAt
// 是**跨上游真的有共识**的字段，而各上游完全不同的凭证结构放 Secret，
// 且**核心不得读它**。Key 型渠道的全部凭证就是"一把 key"，
// 所以它落在 Secret 上，形状由本包定义（与 workbuddy / codearts 各自定义
// 自己的凭证结构是同一条规矩）。
type OpenAICompatSecret struct {
	// APIKey 上游的 API Key。空 ⇒ 调用在**打上游之前**失败（见 compatKey）。
	APIKey string `json:"api_key"`
}

// compatKey 从 Credential 里取 API Key。
//
// 接受三种形状（都是同一条 key，只是包装不同，装配层从渠道存储投影过来时
// 不必先转成某一种"官方"结构）：
//
//	Secret = OpenAICompatSecret{APIKey: "sk-..."}     ← 首选
//	Secret = "sk-..."                                 ← 裸字符串
//	Secret = map[string]any{"api_key": "sk-..."}      ← JSON 反序列化后的自然形状
//
// 空 key / 取不到 key **一律显式报错**，而不是"发一个空 Bearer"：
// 空 Bearer 只会换来上游 401，而那个 401 会把"我们自己的凭证没装上"
// 误报成"上游拒绝了这条渠道" —— 排查方向直接跑偏。
//
// ⚠ 任何一条错误信息都**不得**包含 key 明文（要求见包注释与 statusErr）。
// 所以未知类型只报 %T（类型名），不报值。
func compatKey(cred gw.Credential) (string, error) {
	switch s := cred.Secret.(type) {
	case nil:
		return "", errNoCompatKey
	case string:
		return nonEmptyKey(s)
	case []byte:
		return nonEmptyKey(string(s))
	case OpenAICompatSecret:
		return nonEmptyKey(s.APIKey)
	case *OpenAICompatSecret:
		if s == nil {
			return "", errNoCompatKey
		}
		return nonEmptyKey(s.APIKey)
	case map[string]any:
		// 兼容 JSON 反序列化：字段名不定，几个常见写法都认。
		for _, field := range []string{"api_key", "apiKey", "key", "apikey"} {
			if v, ok := s[field].(string); ok {
				if k, err := nonEmptyKey(v); err == nil {
					return k, nil
				}
			}
		}
		return "", errNoCompatKey
	default:
		return "", fmt.Errorf("无法识别的凭证类型 %T（Key 型渠道需要 Secret 里装 api_key）", cred.Secret)
	}
}

// nonEmptyKey 归一化并校验一把 key。
func nonEmptyKey(raw string) (string, error) {
	if k := strings.TrimSpace(raw); k != "" {
		return k, nil
	}
	return "", errNoCompatKey
}

// errNoCompatKey 凭证里没有可用的 key（哨兵，便于调用方判别这种"没接线"的失败）。
var errNoCompatKey = errors.New("凭证里没有 api_key（Key 型渠道的 key 装在 Credential.Secret 上）")

// name nil 安全的渠道名（错误信息与日志用）。
func (p *OpenAICompat) name() string {
	if p == nil {
		return ""
	}
	return p.Name
}

// baseURL 归一化后的基址（构造期已 trim，这里再兜一次：字段是导出的，
// 调用方可以绕过构造函数直接赋值）。
func (p *OpenAICompat) baseURL() string {
	if p == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
}

// compatURL 拼出目标 URL。
//
// 基址为空 / 缺 scheme 都在这里显式报错：否则 http 层会给出
// "unsupported protocol scheme" 或 "/chat/completions: no such host"
// 这类完全看不出根因的报错，而根因其实是"这条渠道没填 Base URL"。
func (p *OpenAICompat) compatURL(path string) (string, error) {
	base := p.baseURL()
	if base == "" {
		return "", errors.New("渠道没有配置 Base URL")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return "", fmt.Errorf("Base URL %q 必须以 http:// 或 https:// 开头", base)
	}
	return base + path, nil
}

// httpClient 返回出站客户端（未注入时回落包级默认）。
func (p *OpenAICompat) httpClient() *http.Client {
	if p != nil && p.HTTP != nil {
		return p.HTTP
	}
	return compatHTTPDefault
}

// wrap 给错误加上"是哪条渠道、哪个动作"的上下文。
//
// 为什么不留白：一个进程里可以有多条 Key 型渠道，报错不带渠道名就等于
// 让人去猜是哪一条填错了。⚠ 只加渠道名（不是 key）。
func (p *OpenAICompat) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("openai 兼容渠道 %q 的 %s: %w", p.name(), op, err)
}

// statusErr 把"上游非 2xx"变成显式可读的错误：带状态码 + 响应体的一小段。
//
// ⚠ 绝不能把请求头或 key 拼进来：这条 error 会被日志与界面消费，
// 而 Code 型渠道的 key 是一把长期凭据（不是可刷新的 token）。
func (p *OpenAICompat) statusErr(op string, status int, raw []byte) error {
	return p.wrap(op, fmt.Errorf("上游返回 HTTP %d（响应体片段：%s）",
		status, truncate(string(raw), compatErrSnippet)))
}

// compatHTTPDefault 默认出站客户端。
//
// Timeout **刻意留 0**：chat 那条是 SSE 长连接，任何总时长一到就会把
// 正常的慢回复砍断（workbuddy 的 New() 对聊天 client 也是这么做的，
// 见 client.go：Timeout 0 + ResponseHeaderTimeout 管首字节）。
// 真正的"挂死"发生在握手/首字节阶段，由 ResponseHeaderTimeout 兜住；
// 整体生命周期由 ctx 管（两个方法都用 NewRequestWithContext）。
var compatHTTPDefault = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	},
}
