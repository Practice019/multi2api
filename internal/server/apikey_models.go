// apikey_models.go API key 的**模型白名单**准入判断。
//
// # 用户的需求
//
//	「API管理里面，我们设置那个API，我觉得需要设置这个API可以用哪些模型？」
//
// 也就是：一把 key 只允许调用指定的模型，别的模型回 403。
//
// # 为什么判断放在出口层，而不是鉴权中间件
//
// `withAuth` 跑在**请求体被解析之前** —— 它拿不到 `model` 字段。
// 而"哪把 key 在调用"是鉴权阶段的事实（已放进 ctx，见 apikeyCtxKey），
// "要调哪个模型"是解析之后的事实。所以：身份在 withAuth 落定，
// 判断在各自 handler 里、拿到 model 之后立刻做。
//
// # 三个入口都要判，而且判的位置都在"拿到模型名之后、动手之前"
//
//	/v1/chat/completions   解析出 peek.Model 之后
//	/v1/images/generations 同上（生图模型与对话模型是两笔账）
//	/v1/models             **反向**：把列表裁成白名单里的那些
//
// 第三条不是可选的：一个被限制的 key 若在 /v1/models 里看见全部模型，
// 调一个就 403 一次 —— 用户会以为是网关坏了，而不是"这把 key 没这个权限"。
// 列表如实反映权限，权限才可发现。
package server

import (
	"net/http"
	"strconv"
	"strings"

	"workbuddy2api/internal/gateway"
)

// modelAllowed 报告 model 是否被这份白名单放行（**唯一**一份实现）。
//
//	空白名单（nil / 空）→ 恒定 true（不限制）
//	非空               → 按**裸模型名**比（剥掉 "provider/" 前缀）
//
// # 为什么按裸名比，而不是逐字比
//
// 客户端发来的名字可能是 `glm-5.2`（裸名 → 走默认上游），也可能是
// `workbuddy/glm-5.2`（显式带上游前缀）—— 两者是**同一个模型**。
// 用户在白名单里填哪一种都是他的本意；逐字比会让另一种写法被拒，
// 而他完全看不出为什么（他填的名字明明对）。
//
// # 为什么这份实现只在 server 里有一份
//
// 剥前缀用的是 `gateway.SplitModel` —— 与**选路**同一个函数，所以"白名单
// 认的模型"与"请求会路由到的模型"不可能分叉。
//
// ⚠ 它**不能**放进 internal/apikey：import 了 gateway 的包会被
// discoverUpstreams 当成上游实现，于是核心依赖 apikey 就成了"核心依赖上游"
// （架构守卫实测抓到过）。而"在 apikey 里再写一份近似的剥前缀"更糟 ——
// 同一个判据两份实现，在"模型名本身含 '/'"时必然分叉。
// 结论：**存储留在 apikey（零依赖），解释留在 server（认识 gateway）**。
func modelAllowed(models []string, model string) bool {
	if len(models) == 0 {
		return true // 不限制
	}
	_, want, _ := gateway.SplitModel(strings.TrimSpace(model))
	if want == "" {
		// 空模型名：白名单生效时不放行，让 403 发生在这里。
		// 放上去的话上游回 400，而 400 更容易被读成"网关坏了"。
		return false
	}
	for _, m := range models {
		if _, got, _ := gateway.SplitModel(strings.TrimSpace(m)); got == want {
			return true
		}
	}
	return false
}

// allowedModelsForRequest 取本次请求命中的 API key 的模型白名单。
//
// 返回 (models, restricted)：
//
//	restricted=false  不限制 —— 没有普通 key 命中（走管理钥匙），
//	                  或该 key 的白名单为空（见 apikey.Key.Models 的三态）
//	restricted=true   只允许 models 里的模型
//
// ⚠ 管理钥匙（config.api_key）**永远不受限**：它是运维/调试用的那把，
// 被一个白名单挡住会让"排障时连模型列表都拿不到"，而它本来就没有对应的
// Key 记录可挂白名单。
func (h *Handler) allowedModelsForRequest(r *http.Request) ([]string, bool) {
	if h.cfg.APIKeys == nil {
		return nil, false
	}
	kid, _ := r.Context().Value(apikeyCtxKey).(string)
	if kid == "" {
		// 没命中普通 key ⇒ 是管理钥匙（或未启用鉴权）⇒ 不限制。
		return nil, false
	}
	models, ok := h.cfg.APIKeys.ModelsOf(kid)
	if !ok || len(models) == 0 {
		return nil, false
	}
	return models, true
}

// modelAllowedForRequest 判断本次请求是否被允许调用 model。
//
// 不允许时**自己写好 403** 并返回 false —— 调用方只需 `if !ok { return }`，
// 免得三处入口各写一遍错误体（写三遍必然写得不一样）。
//
// 错误体带上**允许的模型清单**（截断到前若干个）：调用方拿到 403 时最需要
// 知道的就是"那我到底能用什么"，否则他只能去问发 key 的人。
// 这不是信息泄漏 —— 这是他自己那把 key 的权限。
func (h *Handler) modelAllowedForRequest(w http.ResponseWriter, r *http.Request, model string) bool {
	allowed, restricted := h.allowedModelsForRequest(r)
	if !restricted {
		return true
	}
	if modelAllowed(allowed, model) {
		return true
	}
	writeOpenAIError(w, http.StatusForbidden, "model_not_allowed",
		"这把 API key 不允许调用模型 "+strings.TrimSpace(model)+"；允许的模型："+joinLimited(allowed, 12))
	return false
}

// filterModelsForRequest 把模型列表裁成该 key 允许的那些。
//
// 不限制时**原样返回**（同一个切片），不复制 —— 这条路径每个客户端都会走，
// 不必要地复制一份上百条的列表是白花钱。
//
// ⚠ 判据用的是与准入**同一个** apikey.ModelAllowed：两处若各写一套，
// 会出现"列表里看得见、调了却 403"这种最难解释的形态。
func (h *Handler) filterModelsForRequest(r *http.Request, list []map[string]any) []map[string]any {
	allowed, restricted := h.allowedModelsForRequest(r)
	if !restricted {
		return list
	}
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		id, _ := m["id"].(string)
		if modelAllowed(allowed, id) {
			out = append(out, m)
		}
	}
	return out
}

// joinLimited 用中文顿号连接，超过 n 个就加"…（共 N 个）"。
//
// 为什么限个数：白名单可能有几十项，整串塞进错误体后前端弹窗会变成一堵墙，
// 反而看不到最关键的那句"不许调用"。
func joinLimited(list []string, n int) string {
	if len(list) <= n {
		return strings.Join(list, "、")
	}
	return strings.Join(list[:n], "、") + "…（共 " + strconv.Itoa(len(list)) + " 个）"
}
