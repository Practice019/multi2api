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
// Verified 的判定用 `go run ./cmd/probe-models` 实测得出，
// 会随**账号侧开通情况**变化，不能一次写死就长期相信。
var knownModels = []ModelSpec{
	{
		ID: "GLM-5.2", Name: "GLM-5.2",
		ContextWindow: 196608, MaxTokens: 131072, Reasoning: true,
		Multiplier: 0.7, MultiplierKnown: true,
		Channel: ChannelDefault, Verified: true,
	},
	{
		ID: "glm-5.2-sft-harmony", Name: "glm-5.2-sft-harmony",
		ContextWindow: 196608, MaxTokens: 131072, Reasoning: true,
		Multiplier: 0.7, MultiplierKnown: true,
		Channel: ChannelDefault, Verified: true,
	},
	{
		ID: "openpangu-2.0-pro", Name: "openpangu-2.0-pro",
		ContextWindow: 512000, MaxTokens: 131072, Reasoning: true,
		Multiplier: 0.7, MultiplierKnown: true,
		Channel: ChannelDefault, Verified: true,
	},
	{
		ID: "openpangu-2.0-flash", Name: "openpangu-2.0-flash",
		ContextWindow: 512000, MaxTokens: 131072, Reasoning: true,
		Multiplier: 0.32, MultiplierKnown: true,
		Channel: ChannelDefault, Verified: true,
	},
	{
		ID: "deepseek-v4-flash-0731", Name: "deepseek-v4-flash-0731",
		ContextWindow: 1048576, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0, MultiplierKnown: false,
		Channel: ChannelBenefit, Verified: true,
	},
	{
		ID: "deepseek-v4-pro-0813", Name: "deepseek-v4-pro-0813",
		ContextWindow: 1048576, MaxTokens: 65536, Reasoning: true,
		Multiplier: 0, MultiplierKnown: false,
		Channel: ChannelBenefit, Verified: true,
	},
	{
		ID: "glm-5.3-flash", Name: "glm-5.3-flash",
		ContextWindow: 202752, MaxTokens: 131072, Reasoning: true,
		Multiplier: 0, MultiplierKnown: false,
		Channel: ChannelBenefit, Verified: true,
	},
}

// ChannelFor 按模型 ID 返回它所属的通道。
//
// 未知模型返回 ChannelDefault —— 这是最安全的默认：
// 默认通道不需要额外请求头；而对未知模型来说，
// 多加一个可能不被接受的头（benefit）比不加更容易触发
// "unsupported model" 这类更难诊断的错误。
func ChannelFor(model string) Channel {
	for _, m := range knownModels {
		if m.ID == model {
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
