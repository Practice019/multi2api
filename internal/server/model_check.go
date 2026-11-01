// model_check.go 模型名的**前置校验**：在任何路由/试号之前把非法名字挡掉。
//
// # 为什么要有这一层（用户报的 bug，实测复现）
//
// 请求一个不存在的模型名时，网关会把它当成"默认上游的模型"拿去**逐个试账号**：
//
//	POST /v1/chat/completions  model=fake-refusal-test      → HTTP 503
//	POST /v1/chat/completions  model=definitely-not-a-model → HTTP 503
//	{"error":{"code":"invalid_request",
//	  "message":"all accounts unavailable (cooling/disabled) —— 6 个账号：0 禁用、0 冷却、
//	              1 个在途未结束: upstream fatal_client (http 400): "}}
//
// 两个后果都不是小事：
//
//	① 白烧上游配额：6 个账号各挨一次 400（用户看到的是"账号坏了"，实际是名字写错）；
//	② 请求日志的模型列留下**没有上游前缀**的名字 —— 用户的目录口径是
//	   「只应有带上游前缀的模型」，他看到 `fake-refusal-test` 才会问"怎么会有这种东西"。
//
// # 规则（与既有命名空间口径一致）
//
//	① 渠道对外名（含 internal/modelmap 的 <渠道名>/<上游名>）⇒ 交给渠道，放行；
//	② 带前缀 p/m ⇒ p 必须是已注册上游，否则 400 unknown_provider（沿用既有 code）；
//	   单上游部署（未注入路由）时前缀只是记号 ⇒ 与 providerFor 的老行为一致，放行；
//	③ 裸名 ⇒ **只有确实在默认上游目录里**的才放行（legacy 客户端路径，例如 `auto`）；
//	   其余 400 unknown_model，并给出用法与目录入口。
//
// # 两条刻意的取舍
//
//	· 目录缓存**冷**时裸名放行（fail-open）：这一层守的是"别把错名字当模型去试号"，
//	  而不是"目录没热好就拒绝所有人"。宁可少拦一次，不可把合法请求误杀成 400。
//	· 只读**缓存**、绝不在请求路径上拉上游（fetchDynamicModels 会真发请求，
//	  在每请求校验里用它等于用一次上游调用来判断该不该调上游）。
package server

import (
	"fmt"
	"net/http"
	"strings"

	"workbuddy2api/internal/gateway"
)

// modelNameUsable 判模型名能不能用。返回 false 表示**已经写好 400**，调用方直接 return。
func (h *Handler) modelNameUsable(w http.ResponseWriter, r *http.Request, model string) bool {
	m := strings.TrimSpace(model)
	if m == "" {
		writeOpenAIError(w, http.StatusBadRequest, "unknown_model",
			"model 不能为空。用法：<上游前缀>/<模型名>，例如 workbuddy/auto；目录见 GET /v1/models")
		return false
	}
	// ① 渠道对外名优先认领 —— 与 channelServes 同一份判据，避免两套口径。
	//    （渠道公开名是另一个命名空间，不能拿 legacy 的前缀解析去判它。）
	if h.channelServes(m) {
		return true
	}
	id, bare, hasPrefix := gateway.SplitModel(m)
	if hasPrefix {
		if h.cfg.Provider == nil {
			// 单上游模式：前缀只是记号，剥掉即可（与 providerFor 的既有行为一致）。
			return true
		}
		if h.cfg.Provider.Has(id) {
			return true
		}
		writeOpenAIError(w, http.StatusBadRequest, "unknown_provider",
			fmt.Sprintf("未知的上游前缀 %q。用法：<上游前缀>/<模型名>，例如 %s/auto；目录见 GET /v1/models",
				id, h.prefixHint()))
		return false
	}
	// ⚠ 别名/映射不用在这里单独放行：调用方（handler.go 的 chatCompletions）
	//   在进本函数**之前**已经跑过别名解析（`h.aliasCandidates` + pickAliasTarget），
	//   所以用户配过的对外名到这里已经是**带前缀的内部名**了（例如 `006/x` → `workbuddy/x`）——
	//   严格模式天然放行它。
	//
	//   我第一版在这里又调了一次 resolveModelAlias 想"放行别名"，复核调用点后发现是死代码：
	//   解析早就发生过了，第二次解析拿到的还是同一个值。留着它只会让人以为
	//   "别名是在这里被保护的"，而真正保护它的是上游那一步。
	// ② 严格前缀模式：裸名一律 400（用户要求「调用的模型必须有上游的前缀」）。
	//
	// ⚠ 放在"目录放行"**之前**：不然目录里的 legacy 裸名（`auto`）会先被放行，
	//   开关就等于没用 —— 而那正是这个开关存在的唯一理由。
	if h.cfg.ModelPrefix != nil && h.cfg.ModelPrefix.Strict() {
		writeOpenAIError(w, http.StatusBadRequest, "unknown_model", fmt.Sprintf(
			"严格前缀模式已开启：模型名必须带上游前缀（收到裸名 %q）。"+
				"用法：<上游前缀>/<模型名>，例如 %s/auto；要放行裸名请关闭 strict_model_prefix",
			m, h.prefixHint()))
		return false
	}
	// ④ 裸名：只有确实在默认上游目录里的才放行（legacy 客户端路径）。
	if h.bareNameInDefaultCatalog(bare) {
		return true
	}
	writeOpenAIError(w, http.StatusBadRequest, "unknown_model", fmt.Sprintf(
		"模型名 %q 不在可用目录里。用法：<上游前缀>/<模型名>，例如 %s/auto；目录见 GET /v1/models",
		m, h.prefixHint()))
	return false
}

// bareNameInDefaultCatalog 裸名是否出现在**默认上游**的模型目录里。
//
// 这就是裸名与"随便一个错名字"的分界线：`auto` 在目录里 ⇒ 放行；
// `definitely-not-a-model-xyz` 不在 ⇒ 400，不再拿它去逐个试账号。
//
// ⚠ 只读缓存（dynamicModelsCache），**不**调用 fetchDynamicModels ——
// 后者会真发一次上游请求，等于用一次上游调用来判断该不该调上游。
// 缓存为空（冷启动 / 拉取失败负缓存期）⇒ 放行：这一层不承担"目录没热好就拒绝全站"的后果。
func (h *Handler) bareNameInDefaultCatalog(bare string) bool {
	if bare == "" {
		return false
	}
	dynamicModelsCache.RLock()
	// 缓存元素是 DefaultModel（带 ContextWindow 等元信息），这里只要名字。
	ids := make([]string, 0, len(dynamicModelsCache.ids))
	for _, m := range dynamicModelsCache.ids {
		ids = append(ids, m.ID)
	}
	dynamicModelsCache.RUnlock()
	if len(ids) == 0 {
		return true // fail-open：见上面的取舍
	}
	for _, id := range ids {
		if id == bare {
			return true
		}
	}
	return false
}

// prefixHint 给错误信息一个可照抄的例子（默认上游前缀）。
//
// 不列全部前缀：ProviderRouter 接口没有"列 ID"的方法，而为了拼一句提示去翻目录
// 会在错误路径上引入目录计算（那本身就是这次要避免的事）。
func (h *Handler) prefixHint() string {
	if d := strings.TrimSpace(h.defaultProvider()); d != "" {
		return d
	}
	return "workbuddy"
}
