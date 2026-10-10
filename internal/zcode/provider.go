// provider.go 把 ZCode（Z.ai / 智谱 GLM Coding Plan）适配为 gateway.Provider。
//
// # 这个上游是什么
//
//	ZCode 是 Z.ai（智谱关联）的 AI coding agent（ADE），官方仓库
//	zai-org/ZCode 是完整开源的 TypeScript monorepo（Apache-2.0，7433★）。
//	本包接的是它背后的**模型 API**：GLM-5.3 / GLM-5.3-Flash。
//
// # 两条通道（这是本包与其它上游最大的不同）
//
//	通道       协议       验证码   协议转换   依赖
//	api-key   OpenAI       不需要   不需要    零
//	jwt       Anthropic    每请求要  需要      验证码求解器
//
// 为什么值得同时支持两条：
//
//	Coding Plan 是**订阅套餐**（有额度上限但已付费），按量付费是 API Key。
//	只支持 API Key 会让"手里有 z.ai 订阅"的人用不上；
//	只支持 JWT 则要求部署方有 Chromium 且能过滑块 —— 那会把大多数人挡在门外。
//
// # 与其它上游的差异（都已实测）
//
//	统一信封    失败是 **HTTP 200 或 4xx + body 里的业务码**，两种协议两种形状：
//	            {"error":{"code":"1001",...}}  OpenAI 形态（code 是**字符串**）
//	            {"error":{"message":...,"type":"1001"}}  Anthropic 形态（code 在 type，**数字**）
//	            只认一种会漏判 —— 见 zcode.go 的 errCode
//	端点易变    官方 official-coding-plan-gateway.ts 揭示 Coding Plan 请求会被
//	            服务端**动态改写**到 /api/v1/ultra[-zai]/... 所以 baseURL 可配置
//	风控敏感    多发一个头就触发 3012 "unusual activity"（见 headers.go 的警告）
//	JWT 不可刷  官方明确"暂未提供 refresh token 交换接口"，过期只能重新登录
package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"workbuddy2api/internal/anthroconv"
	"workbuddy2api/internal/gateway"
)

// providerID 本上游在账号池里的标识（模型前缀 `zcode/`）。
const providerID = "zcode"

// ProviderID 导出给装配层用（账号池按它分组，模型前缀按它标注）。
//
// 为什么其它上游也叫 ProviderID：装配层要 `zcode.ProviderID` 这样的字面量
// 来建投影（见 cmd/server/zcodecreds.go），不能从 Provider 实例上取
// —— 建投影时实例可能还没构造。
const ProviderID = providerID

// displayName 控制台上显示的名字。
const displayName = "ZCode（Z.ai / GLM）"

// 能力位。
//
// 刻意**不声明**的：
//
//	CapCheckin   Z.ai 没有"每日签到"端点（额度由订阅周期决定）
//	CapImport    凭证导入需要它 —— 但这里声明了，见下
//	CapTasks / CapInvite / CapGrowth / CapTravel  都是别家特有的
//
// 声明 CapImport 的理由：本包的 API Key 通道是**管理员粘贴 Key**，
// 那正是 import 的形态（与 cline/api-key 类上游一致）。
const caps = gateway.CapChat | gateway.CapModels | gateway.CapQuotaProbe | gateway.CapImport

// Provider 本上游的 Provider 实现。
type Provider struct {
	client  *Client
	authDir string

	mu      sync.RWMutex
	creds   map[string]*Auth // UID → 本地凭证（测试路径；真实运行时由 credSrc 提供）
	credSrc func(uid string) (gateway.Credential, bool)
	// uidEnumerator 装配层注入的"列出全部 uid"能力（诊断端点用）。
	//
	// 为什么需要它：credSrc 只能**按名查**，枚举不出全部 ——
	// 而诊断端点要列全部账号。见 admin.go 的 knownUIDs。
	uidEnumerator func() []string

	// logins 进行中的登录会话（flow_id → 会话）。
	//
	// 为什么放在 Provider 上而不是全局：登录是**上游专属**的，
	// 两个上游的 flow_id 空间互不相干；放全局还要加前缀防撞。
	logins map[string]*loginEntry

	// originProbe 是否在装配时探测凭证属于哪个平台。
	// 测试里关掉它（避免真发请求）。
	probeOrigin bool
}

// New 构造 Provider。
func New(cfg Config) *Provider {
	p := &Provider{
		client:      NewClient(cfg),
		authDir:     cfg.AuthDir,
		creds:       map[string]*Auth{},
		logins:      map[string]*loginEntry{},
		probeOrigin: true,
	}
	return p
}

// SetCredentialSource 装配活凭证来源（余额/额度端点要用到令牌）。
//
// # 为什么不能用账号池的 AuthByUID
//
// 池子暴露的是**核心投影**（只有 uid / nickname / filePath），
// 而本包的计量端点要 `*zcode.Auth`（含 api_key / jwt）——
// 那个东西在 `pool.SecretOf` 里。这与 raccoon 的 raccoonCredSource 同一模式。
//
// 签名用 gateway.Credential 而不是 *Auth：装配层的访问器是通用的
// （各上游共用同一形状），上游自己从 Secret 里取自己的那个类型。
func (p *Provider) SetCredentialSource(f func(uid string) (gateway.Credential, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.credSrc = f
}

// cred 取该 uid 的活凭证（优先池子，回落本地）。
func (p *Provider) cred(uid string) *Auth {
	p.mu.RLock()
	src := p.credSrc
	local := p.creds[uid]
	p.mu.RUnlock()

	if src != nil {
		if c, ok := src(uid); ok {
			if a, ok := c.Secret.(*Auth); ok && a != nil {
				return a
			}
		}
	}
	return local
}

// ---- gateway.Provider ----

// ID 上游标识。
func (p *Provider) ID() string { return providerID }

// Caps 能力位。
func (p *Provider) Caps() gateway.Capability { return caps }

// Chat 发一次对话。
//
// # 契约（与 gateway.Provider 一致）
//
//	✓ status 非 2xx **也算正常返回**（把上游错误体带回给调用方按业务码判）
//	✓ 只有网络层失败才返回 error
//	✓ 返回的 Body **调用方负责 Close**
//
// # 本上游的特殊之处：业务失败可能是 HTTP 200
//
// 上游把"额度耗尽""风控"这类失败放在 body 的业务码里，
// 有时配 4xx，**有时配 200**。所以调用方不能只看 status ——
// 本包为此实现了 gateway.SoftRateExt（见 extensions.go）来告诉核心
// "这次 200 其实是个限流"。
func (p *Provider) Chat(ctx context.Context, cred gateway.Credential, body []byte) (gateway.ChatStream, error) {
	a, err := p.authOf(cred)
	if err != nil {
		return gateway.ChatStream{}, err
	}
	stream := detectStream(body)
	resp, err := p.client.Chat(ctx, a, body, stream)
	if err != nil {
		return gateway.ChatStream{}, err
	}

	// JWT 通道（Anthropic 协议）：把响应转成 OpenAI 形态再交出去。
	//
	// 为什么转换放这里而不是出口层：出口层假定上游是 OpenAI 协议
	//（见 gateway 的 Chat 注释），转换是**本上游的实现细节** ——
	// 与"各上游的 header 拼法、签名、base URL 差异全部关在实现里"同一原则。
	if a.UsesJWT() {
		return p.translateAnthropic(ctx, resp, stream)
	}
	return gateway.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
}

// Models 返回模型目录。
//
// # ⚠ 无凭证也必须给目录（实测发现的缺口）
//
// 一开始的写法是"先 authOf，失败就返回 error" —— 看起来合理，但它让
// **内置兜底清单成了死代码**：出口层调 Models 时**不带凭证**
// （见 handler.go 的 modelList：`h.cfg.Provider.Models(ctx, id)`，
// 只传 provider id），于是新装的 zcode 上游在 `/v1/models` 里
// **一个模型都不出现**，直到有人加账号。
//
// 而那正是用户第一次配置上游的时刻 —— 他会看到"zcode 没有模型"，
// 以为装坏了。本仓其它上游（见 raccoon 的同款注释"无凭据也要能给出
// 兜底目录"）都是这个行为，所以这里对齐。
//
// 顺序：先用 live 配置（有凭证且取得到时），失败一律回落内置清单。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 拿不到凭证不算错误 —— 见上面的说明。
	if a, err := p.authOf(cred); err == nil && a != nil {
		if list, ferr := p.fetchModels(ctx); ferr == nil && len(list) > 0 {
			return list, nil
		}
	}
	return builtinModels(), nil
}

// builtinModels 离线兜底的模型清单（抄自官方 /api/v1/client/configs 实测值）。
//
// ⚠ 这些数字是**实测拷下来的**，不是估的：
//
//	GLM-5.3        contextWindow=1000000  maxCompletionTokens=128000
//	GLM-5.3-Flash  contextWindow=1000000  maxCompletionTokens=128000  vision=true
//
// 为什么把 contextWindow 填进去而不是留 0：本仓其它上游的做法是
// "上游不提供就留 0"，但这里我们**确实知道**（官方配置里写明），
// 留 0 会让前端的上下文用量条显示不出来。
func builtinModels() []gateway.ModelInfo {
	return []gateway.ModelInfo{
		{ID: "GLM-5.3", ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
		{ID: "GLM-5.3-Flash", ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
	}
}

// fetchModels 从官方实时配置拉模型清单。
//
// ⚠ **它不需要凭证** —— 那个端点是免鉴权公开的（实测 HTTP 200）。
// 参数里刻意不收 *Auth：收了会让人误以为"要先有凭证才能取目录"，
// 而那正是上面 Models 注解里说的那个缺口。
//
// 它也**不是必须成功**的：失败时调用方回落内置清单。所以这里
// 不重试、不缓存 —— 出口层的模型列表不是热路径，而多一层缓存
// 只会多一个"清单不跟新"的排查点。
func (p *Provider) fetchModels(ctx context.Context) ([]gateway.ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		planOrigin+"/api/v1/client/configs?app_version="+defaultAppVersion, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zcode: 取模型清单 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseClientConfigModels(raw)
}

// clientConfigs 官方实时配置的信封。
type clientConfigs struct {
	Data struct {
		BuiltinModels []struct {
			ModelID             string `json:"modelId"`
			Name                string `json:"name"`
			ContextWindow       int    `json:"contextWindow"`
			MaxCompletionTokens int    `json:"maxCompletionTokens"`
		} `json:"builtinModels"`
	} `json:"data"`
}

// parseClientConfigModels 从官方配置里解出模型清单。
func parseClientConfigModels(raw []byte) ([]gateway.ModelInfo, error) {
	var cc clientConfigs
	if err := json.Unmarshal(raw, &cc); err != nil {
		return nil, err
	}
	var out []gateway.ModelInfo
	for _, m := range cc.Data.BuiltinModels {
		id := strings.TrimSpace(m.ModelID)
		if id == "" {
			continue
		}
		out = append(out, gateway.ModelInfo{
			ID:              id,
			ContextWindow:   m.ContextWindow,
			MaxOutputTokens: m.MaxCompletionTokens,
		})
	}
	return out, nil
}

// ---- gateway.CredentialLoader / CredentialSecretLoader ----
//
// # 为什么必须实现这两个（实测发现的第二个必需扩展点）
//
// 登录流程走完后，核心要**重扫该上游的凭证目录**把它并进账号池。
// 那一步的判据也是类型断言：
//
//	都不实现 → 501「凭证已写入 …，但上游 X 没有实现
//	           gateway.CredentialLoader，无法按它自己的格式重扫凭证」
//
// 症状极具迷惑性：**凭证文件正确落盘了**（用户去目录里能看到），
// 但账号池里没有它 —— 用户看到的是"登录成功但账号没出现"。
//
// 这也解释了为什么核心不自己解析：凭证格式是**上游的事实**，
// 核心硬编码它等于打破"加新上游核心零改动"这条判据。

// LoadCredentials 读取 dir 下全部凭证（只投影 uid/nickname）。
func (p *Provider) LoadCredentials(dir string) ([]gateway.Credential, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.Credential, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.Credential{
			Provider: providerID,
			UID:      DisplayUID(a),
			Nickname: DisplayNameOf(a),
			FilePath: a.FilePath,
		})
	}
	return out, nil
}

// LoadCredentialsWithSecrets 与 LoadCredentials 同源，但额外给出 Secret。
//
// ⚠ **必须与 LoadCredentials 读同一份对象**（同一次 LoadDir 的结果），
// 不能各扫一次：各造一份会让"活凭证"与"投影"分叉 ——
// 池子里那份 Secret 与磁盘上那份是两个 Go 对象，
// 续期改了池里那份而投影指向的另一份没变，
// 表现为"续期之后重启又变回旧的"。raccoon 的同款注释记的是另一面代价
// （一次性 refresh_token 被消费两次，池里那份永远停在作废的旧值上）。
func (p *Provider) LoadCredentialsWithSecrets(dir string) ([]gateway.CredentialSecret, error) {
	list, err := LoadDir(p.effectiveDir(dir))
	if err != nil {
		return nil, err
	}
	out := make([]gateway.CredentialSecret, 0, len(list))
	for _, a := range list {
		out = append(out, gateway.CredentialSecret{
			Credential: gateway.Credential{
				Provider: providerID,
				UID:      DisplayUID(a),
				Nickname: DisplayNameOf(a),
				FilePath: a.FilePath,
			},
			Secret: a,
		})
	}
	return out, nil
}

// effectiveDir 解析生效的凭证目录。
//
// 核心传进来的 dir 一般就是 AuthDirExt 报的那个；为空时回落本实例的配置
// （单上游部署的旧形态会传空串）。
func (p *Provider) effectiveDir(dir string) string {
	if s := strings.TrimSpace(dir); s != "" {
		return s
	}
	if p != nil {
		return p.authDir
	}
	return ""
}

// ---- 内部辅助 ----

// authOf 从 gateway.Credential 里取出本包的 Auth。
func (p *Provider) authOf(cred gateway.Credential) (*Auth, error) {
	if a, ok := cred.Secret.(*Auth); ok && a != nil {
		return a, nil
	}
	// 回落：按 UID 查活凭证。为什么需要 —— 有些调用路径
	//（比如出口层的探测）只带 UID 不带 Secret。
	if uid := strings.TrimSpace(cred.UID); uid != "" {
		if a := p.cred(uid); a != nil {
			return a, nil
		}
	}
	return nil, fmt.Errorf("zcode: 凭证缺失或类型不符（期望 *zcode.Auth，实际 %T）", cred.Secret)
}

// detectStream 从请求体里看出是不是流式请求。
//
// 为什么要判：流式与非流式在两条通道下的**转换路径不同**
// （JWT 通道要分别走 SSE 转换与 JSON 转换）。
// 出口层已经在 body 里带了 stream 字段，所以这里读它而不是另传参数。
func detectStream(body []byte) bool {
	var probe struct {
		Stream json.RawMessage `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		// 解不出来时按**非流式**处理：非流式路径对畸形 body 更宽容
		//（上游会直接报 400，而流式路径会先建立 SSE 连接再失败）。
		return false
	}
	s := strings.TrimSpace(string(probe.Stream))
	return s == "true"
}

// translateAnthropic 把 JWT 通道的 Anthropic 响应翻成 OpenAI 形态。
//
// 失败时**如实返回上游原文**而不是编一个成功响应 ——
// 后者会让"转换器有 bug"伪装成"模型答了空话"，那极难查。
func (p *Provider) translateAnthropic(ctx context.Context, resp *http.Response, stream bool) (gateway.ChatStream, error) {
	if resp.StatusCode >= 400 {
		// 错误体原样交出去：出口层要按业务码判（两种协议两种形状）。
		return gateway.ChatStream{Status: resp.StatusCode, Body: resp.Body}, nil
	}
	if stream {
		pr, pw := io.Pipe()
		go func() {
			defer resp.Body.Close()
			err := anthroconv.AnthropicSSEToOpenAI(resp.Body, pw)
			pw.CloseWithError(err)
		}()
		return gateway.ChatStream{Status: resp.StatusCode, Body: pr}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return gateway.ChatStream{}, err
	}
	converted, err := anthroconv.AnthropicJSONToOpenAI(raw)
	if err != nil {
		// 转换失败：把**原文**交出去，并保留 200 ——
		// 出口层会因解不出 OpenAI 结构而报 upstream_parse，
		// 那比我们伪造一个空响应诚实得多。
		return gateway.ChatStream{
			Status: resp.StatusCode,
			Body:   io.NopCloser(strings.NewReader(string(raw))),
		}, nil
	}
	return gateway.ChatStream{
		Status: resp.StatusCode,
		Body:   io.NopCloser(strings.NewReader(string(converted))),
	}, nil
}

// （原 refreshSkew 常量已删除。）
//
// 它原来是"JWT 提前 10 分钟续期"的窗口值。而本上游**没有**（也做不出）
// CredentialRefresher —— JWT 官方无 refresh 接口。所以那个正数窗口是一句
// 兑现不了的承诺：核心据此走进续期分支，发现没有实现，再解释成
// 「不需要刷新」而静默跳过。用户看到的就是「已过期」一直挂着。
//
// 现在 RefreshSkew 对 JWT 明确回 (0, true)（"不需要提前刷"），
// 这个常量随之失去用途 —— 留着它会诱使下一个人把它接回去。
