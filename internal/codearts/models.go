// models.go CodeArts 的模型目录。
//
// 为什么是静态目录：CodeArts 没有公开的模型列表端点。
// 实测以下路径全部 404：
//
//	/api/v2/models  /v1/models  /api/v2/chat/models
//	/PromptCenterService/v1/default/models  /v1/llm/vendors
//
// 客户端展示的清单来自本地缓存（state.vscdb 的 hc-task-info-key），
// 而**界面显示的 ≠ 服务端已注册的** —— 实测 7 个候选里只有 GLM-5.2 真正可用
// （其余返回 InferHub.002002009 "The model is not registered"）。
//
// 因此这里只列**实测确认可用**的模型，其余以 Disabled 形式保留备查。
// 数据来源：AgentKernel 日志的 provider models 体 + 实际调用验证。
package codearts

import (
	"fmt"
	"strings"
	"sync"

	"workbuddy2api/internal/upstream"
)

// ModelSpec 是 CodeArts 侧的模型描述（含实测可用性）。
type ModelSpec struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
	Reasoning     bool
	// Channel 是该模型所属的服务通道。
	//
	// 实测发现：7 个模型分成**两个互斥的通道**，走错通道会报
	// "The model is not registered"（看起来像账号未开通，实际是通道不对）。
	//
	//	默认通道（不带 maas_type 头）：GLM-5.2 / glm-5.2-sft-harmony
	//	                              openpangu-2.0-pro / openpangu-2.0-flash
	//	benefit 通道（maas_type: benefit）：deepseek-v4-flash-0731
	//	                                   deepseek-v4-pro-0813 / glm-5.3-flash
	//
	// 反向也成立：给默认通道的模型加 maas_type=benefit 会报
	// InferHub.4005.200 "unsupported model"。
	Channel Channel
	// Multiplier 是上游下发的**固定成本倍率**（`ratio_display`，如 0.7 表示 0.7x）。
	//
	// 数据源：IDE 的模型配置块，每个模型带
	//
	//	"credit":[{"ratio":"0.028","input_price":"3.2","output_price":"14.5",
	//	           "ratio_display":"0.7x"}]
	//
	// 用 `ratio_display` 的数值部分（0.7x → 0.7）。
	//
	// **这是上游固定值，不是本地估算** —— 它由服务端随模型配置下发，
	// 同一模型对所有账号一致。因此不需要"经验倍率"那套统计，
	// 也不该随时间漂移。
	//
	// 0 表示**上游未下发**（不是"免费"）。区别很重要：
	// 显示成 0x 会被误读为免费，而实际只是我们不知道。用 MultiplierKnown 区分。
	Multiplier float64
	// MultiplierKnown 表示 Multiplier 是上游真实下发的值。
	//
	// 为什么不能只看 Multiplier != 0：真实存在合法的 0 倍率（免费模型），
	// 而"未下发"与"免费"在展示上必须区分开。
	MultiplierKnown bool
	// Verified 表示该模型已实测返回 200。
	Verified bool
}

// Channel 是模型所属的服务通道。
type Channel int

const (
	// ChannelDefault 不需要额外请求头。
	ChannelDefault Channel = iota
	// ChannelBenefit 需要 `maas_type: benefit` 头（免费额度通道）。
	ChannelBenefit
)

func (c Channel) String() string {
	if c == ChannelBenefit {
		return "benefit"
	}
	return "default"
}

// Header 返回该通道需要附加的请求头（默认通道返回 nil）。
func (c Channel) Header() map[string]string {
	if c == ChannelBenefit {
		return map[string]string{"maas_type": "benefit"}
	}
	return nil
}

// knownModels 是 CodeArts 侧的模型表。
//
// **ID 必须与服务端注册名逐字符一致（大小写敏感）**。
// 早期版本写成 `OpenPangu-2.0-Pro`，服务端注册的是 `openpangu-2.0-pro`，
// 于是恒返回 `InferHub.002002009 The model is not registered` ——
// 看起来像"账号没开通"，实际是名字打错了。
//
// 清单来源：AgentKernel 日志里 cag-global-server 下发的 provider 列表
// （用 `go run ./cmd/model-list` 可重新导出）。共 7 个。
//
// 上下文/输出上限取自同一处 provider models 体：
//
//	GLM-5.2               input 196608 / output 131072 / context 202752
//	openpangu-2.0-pro     input 512000 / output 131072 / context 524288
//	deepseek-v4-*                     output 393216 / context 1048576
//
// Verified 的判定是**实测**得出的，会随账号侧开通情况变化，
// 不能一次写死就长期相信。重新判定的方法：
//
//	go test -tags probe ./internal/codearts/ -run TestProbeRealUpstreamErrorShape -v
//
// （见同目录 probe_real_test.go，用真实凭证打一次上游并打印**原始帧**。）
//
// ⚠ 判定**只能看内容，不能看状态码**：被拒绝的模型不会返回 4xx，
// 而是 200 + 流内错误信封，所以"200"在这里什么都不代表。
//
// ⚠ 早先这里的注释引用 `go run ./cmd/probe-models`，但该命令在本仓库
// 并不存在（cmd/ 下只有 credit / login / server / signin）。按不存在
// 的命令去复验等于没有复验手段，故改为上面这条可直接执行的命令。
var knownModels = []ModelSpec{
	{
		// ⚠ MaxTokens 是**输出**上限（出站裁剪用），实测为 65536。
		//
		// 早先这里写 131072 —— 那是 IDE 配置里的 **input/context** 数字
		// （GLM-5.2 input 196608），被当成了输出上限。后果是 clampMaxTokens
		// 把超限值"裁剪"到 131072，而 131072 本身就非法
		// （InferHub.001001005.400 The request param is invalid）；
		// 且 131072 <= 131072 让"是否超限"的判据认为无需裁剪 ——
		// 表现与完全不裁剪一模一样，裁剪日志一条都不打。
		// 实测（go test -tags probe）：65536 接受，70000 起被拒。
		ID: "GLM-5.2", Name: "GLM-5.2",
		ContextWindow: 196608, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0.7, MultiplierKnown: true,
		Channel: ChannelDefault, Verified: true,
	},
	{
		// ⚠ 同理，输出上限实测 65536（同上注释）。
		ID: "glm-5.2-sft-harmony", Name: "glm-5.2-sft-harmony",
		ContextWindow: 196608, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0.7, MultiplierKnown: true,
		Channel: ChannelDefault, Verified: true,
	},
	{
		// ⚠ 同理，输出上限实测 65536（同上注释）。
		ID: "openpangu-2.0-pro", Name: "openpangu-2.0-pro",
		ContextWindow: 512000, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0.7, MultiplierKnown: true,
		Channel: ChannelDefault, Verified: true,
	},
	{
		// ⚠ 同理，输出上限实测 65536（同上注释）。
		ID: "openpangu-2.0-flash", Name: "openpangu-2.0-flash",
		ContextWindow: 512000, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0.32, MultiplierKnown: true,
		Channel: ChannelDefault, Verified: true,
	},
	{
		ID: "deepseek-v4-flash-0731", Name: "deepseek-v4-flash-0731",
		ContextWindow: 1048576, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0, MultiplierKnown: false,
		// ⚠ Verified=true：这个模型**存在且可用**，只是 benefit 通道的
		// **每日免费额度**可能已用完（实测报 InferHub.4004.200 benefit not found
		// 或 InferHub.4291.200 insufficient quota）。
		//
		// 这**不是**"模型不可用"，绝不能据此设成 Verified=false：
		// benefit 额度次日重置，而 Verified 是静态事实，摘掉就等于永久隐藏，
		// 且 ProbeAllQuota 也再不会探测它 —— 永远发现不了它已恢复。
		//
		// 临时耗尽由 quotaCache 表达（见 quota.go 的 QuotaState.ExhaustedUntil
		// 与 Provider.Models 的过滤），那是**可逆**的。
		Channel: ChannelBenefit, Verified: true,
	},
	{
		ID: "deepseek-v4-pro-0813", Name: "deepseek-v4-pro-0813",
		ContextWindow: 1048576, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0, MultiplierKnown: false,
		// ⚠ Verified=true，原因同上（每日额度，不是不可用）。
		Channel: ChannelBenefit, Verified: true,
	},
	{
		ID: "glm-5.3-flash", Name: "glm-5.3-flash",
		// ⚠ MaxTokens 取 65536 是**保守**值，不是实测值。
		//
		// 原写 131072，与 GLM-5.2 当初同一个来源（IDE 配置里的 input/context
		// 数字被当成输出上限）—— 而 GLM-5.2 的 131072 已被实测证伪。
		// 本模型属于 benefit 通道，探测时额度已耗尽，**无法实测**。
		//
		// 不确定性下必须取小：表值偏高 → clampMaxTokens 裁到一个仍非法的值
		// → 上游直接 400（本类 bug 的形态，硬失败）；表值偏低 → 只是输出被
		// 限制得比模型允许的短一点（软限制，客户端极少请求 >64k 输出）。
		// 两边的代价不对称，所以宁可取小。
		//
		// 额度恢复后应跑 probe 复测并改回实测值。
		ContextWindow: 202752, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0, MultiplierKnown: false,
		// ⚠ Verified=true，原因同上（每日额度，不是不可用）。
		Channel: ChannelBenefit, Verified: true,
	},
}

// CanonicalModel 把一个（可能大小写不规范的）模型名映射到表中注册的规范 ID。
//
// # 为什么需要它
//
// 本文件的模型 ID 必须与服务端注册名**逐字符一致**（见 knownModels 的注释：
// 早期写成 `OpenPangu-2.0-Pro` 而服务端注册的是 `openpangu-2.0-pro`，
// 于是恒返回 `InferHub.002002009 The model is not registered`，
// 看起来像"账号没开通"）。
//
// 而客户端**不保证**遵守大小写：`/v1/models` 的下游消费方、OpenAI 兼容
// 工具链、手输模型名的用户都可能发来 `codearts/glm-5.2`。
//
// 实测这种请求不会得到任何 4xx —— 上游用 **200 + 流内错误信封**拒绝它，
// 客户端只看到一个空回复（见 upstream.InBandError 的注释）。
// 所以这里做一次大小写不敏感的兜底映射：唯一命中表中某个 ID 就采用它。
//
// # 歧义与未知模型
//
// 精确匹配优先于折叠匹配。若折叠匹配命中**多个** ID（表内不该出现这种
// 碰撞，见 models_test.go 的守卫）→ 返回 ok=false，宁可让上游报错也不猜。
//
// 返回 ok=false 表示"这不是本上游的模型"：调用方必须**原样转发**，
// 让上游给出它自己的错误，而不是悄悄替换成某个默认模型。
func CanonicalModel(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	// 精确匹配优先：绝大多数请求走这条，且不受折叠歧义影响。
	for _, m := range knownModels {
		if m.ID == id {
			return m.ID, true
		}
	}
	found := ""
	hits := 0
	for _, m := range knownModels {
		if strings.EqualFold(m.ID, id) {
			found = m.ID
			hits++
		}
	}
	if hits == 1 {
		return found, true
	}
	return "", false
}

// ChannelFor 按模型 ID 返回它所属的通道。
//
// 未知模型返回 ChannelDefault —— 这是最安全的默认：
// 默认通道不需要额外请求头；而对未知模型来说，
// 多加一个可能不被接受的头（benefit）比不加更容易触发
// "unsupported model" 这类更难诊断的错误。
//
// ⚠ 判定前先过 CanonicalModel：通道是按**规范 ID**查表的。
// 不做这一步的话，`glm-5.2` 这类大小写不符的名字会落到默认通道，
// 而服务端同样不认这个名字 —— "名字不对"与"通道不对"同时发生，
// 排查时只看到下游的"模型未注册"，极易误判成账号没开通。
func ChannelFor(model string) Channel {
	id, ok := CanonicalModel(model)
	if !ok {
		return ChannelDefault
	}
	for _, m := range knownModels {
		if m.ID == id {
			return m.Channel
		}
	}
	return ChannelDefault
}

// MaxTokensTable 返回每模型的 max_tokens 上限，供出站请求裁剪用。
//
// 为什么需要：客户端（尤其 DSH）会带远超上限的 max_tokens
// （实测 DSH 发 max_completion_tokens: 384000），而上游对超限**直接 400**
// （InferHub.001001005 The request param is invalid）。
// 网关若原样透传，该错误还会被误判成账号故障 —— 一个客户端的过大参数
// 就能把整个账号池拖垮。
//
// 裁剪而非报错：客户端要的是"尽量长的输出"，给它上限即可；
// 因为"要多了"让整个请求失败没有道理。
func MaxTokensTable() map[string]int {
	out := make(map[string]int, len(knownModels))
	for _, m := range knownModels {
		if m.MaxTokens > 0 {
			out[m.ID] = int(m.MaxTokens)
		}
	}
	return out
}

// KnownModels 返回可对外暴露的模型（仅 Verified 的）。
func KnownModels() []upstream.ModelInfo {
	out := make([]upstream.ModelInfo, 0, len(knownModels))
	for _, m := range knownModels {
		if !m.Verified {
			continue
		}
		out = append(out, upstream.ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
			MaxTokens:     m.MaxTokens,
		})
	}
	return out
}

// AllModels 返回全部模型（含未验证），供管理台展示与排查。
func AllModels() []ModelSpec {
	out := make([]ModelSpec, len(knownModels))
	copy(out, knownModels)
	return out
}

// SetModelVerified 在运行时把某个模型标记为已验证（探测成功后调用）。
//
// 用途：账号侧开通了新模型时，不必改代码即可让它出现在 /v1/models。
func SetModelVerified(id string) {
	modelMu.Lock()
	defer modelMu.Unlock()
	for i := range knownModels {
		if knownModels[i].ID == id {
			knownModels[i].Verified = true
			return
		}
	}
}

var modelMu sync.Mutex

// BuildModelCatalog 把模型表映射成 upstream.ModelCatalog。
//
// 倍率来自 knownModels 的 Multiplier（**上游下发的固定值**），
// 不再是此前硬编码的 1.0 —— 那个占位值让管理台的倍率显示形同虚设。
//
// 上游未下发倍率的模型（MultiplierKnown == false）**不写入目录**：
// ModelCatalog.MultiplierTable() 只收 Multiplier > 0 的条目，
// 写 0 等于告诉调用方"这个模型免费"，而我们实际只是不知道。
// 宁可不显示，也不要给一个会被读错的数字。
func BuildModelCatalog() *upstream.ModelCatalog {
	cat := &upstream.ModelCatalog{}
	for _, m := range knownModels {
		if !m.Verified || !m.MultiplierKnown {
			continue
		}
		cat.Models = append(cat.Models, upstream.ModelCatalogEntry{
			ID:                m.ID,
			Name:              m.Name,
			Vendor:            "codearts",
			Tags:              []string{"codearts"},
			CreditsRaw:        fmt.Sprintf("x%.2f credits", m.Multiplier),
			Multiplier:        m.Multiplier,
			MaxInputTokens:    m.ContextWindow,
			SupportsToolCall:  true,
			SupportsImages:    false,
			SupportsReasoning: m.Reasoning,
		})
	}
	return cat
}

// MultiplierFor 返回某模型的**上游固定倍率**，以及该值是否已知。
//
// 第二个返回值区分"免费（0）"与"上游未下发"：
// 调用方应据此决定显示数字还是显示"未知"。
func MultiplierFor(model string) (float64, bool) {
	for _, m := range knownModels {
		if m.ID == model {
			return m.Multiplier, m.MultiplierKnown
		}
	}
	return 0, false
}
