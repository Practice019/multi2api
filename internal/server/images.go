// images.go OpenAI 兼容的生图端点：`POST /v1/images/generations`。
//
// # 为什么加这条出口
//
// 网关此前只有 `/v1/chat/completions` 一条出口，而 Loomy 上游上有两个
// **只能生图、不能对话**的模型（doubao-seedream-5-lite / qwen-image-3.0-pro）：
// 它们走 chat 端点会 404，走 `/images/generations` 完全可用（实测 200 +
// 2304x1728 PNG）。所以"能不能用"取决于端点，单靠 chat 出口表达不了生图。
//
// # 形状：OpenAI Images API
//
// 请求体按 OpenAI 的形状透传（`{model,prompt,n,size,response_format,…}`），
// 响应也**原样**回给客户端：
//
//	{"created":…,"data":[{"url":"https://…"}],"points_consumed":110}
//
// 这样选是因为上游（Loomy）**本身就是 OpenAI 兼容**的 —— 转发即兼容，
// 各种现成的 OpenAI 生图客户端（含 Loomy 自己）不用改一行就能用。
// 网关刻意**不**重排字段、不改名、不补默认值：任何一处自作主张都会让
// "上游加了新字段"变成"要改网关"（与 chat 出口同一条判据）。
//
// # 与 chat 出口共享的部分
//
//	鉴权       相同的 Bearer（含 apikey 额度/限速）
//	选号       PickFor(provider, model, tried) —— 按上游与模型过滤
//	错误信封   writeOpenAIError（OpenAI 形状）
//
// 复用而不是另起一套：出口层只有"往里转发"这一件事，
// 两套鉴权或两套信封会让同一个网关在两个端点上表现不一致。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// imageGenTimeout 生图的上游超时。
//
// 实测一次 2304x1728 耗时 **23.3 秒**（Loomy 官方客户端给 5 分钟）。
// 这里取 5 分钟与之对齐：生图是重任务，按普通 API 的 30 秒会大面积超时，
// 而超时对调用方表现为"网关坏了"—— 一次白跑的失败。
const imageGenTimeout = 5 * time.Minute

// imagesGenerations POST /v1/images/generations。
//
// # 与 chatCompletions 的差别（都是生图固有的事实）
//
//   - **不重试换号**。一次生图要 20+ 秒且**上游已计费**（实测扣 110 分/张）。
//     失败就如实报错，绝不悄悄换号重试 —— 那会让用户被扣两次分
//     却只拿到一张图（或什么都没有）。
//   - **不解析响应**。重排字段就会丢掉上游新加的字段（points_consumed
//     就是这种：它不是 OpenAI 标准字段，但调用方很需要）。
func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	if !h.requireReady(w, "生图") {
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(h.cfg.MaxBodyMB)<<20))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("请求体超过上限 %d MiB（可在 config.json 的 server.max_body_mb 调整）", h.cfg.MaxBodyMB))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}

	var peek struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(peek.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 prompt")
		return
	}

	// 模型前缀决定上游（"loomy/doubao-seedream-5-lite" → loomy）。
	// 与 chat 出口同一套解析：不带前缀时按默认上游。
	reqProvider, reqModel, perr := h.providerFor(peek.Model)
	if perr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "unknown_provider", perr.Error())
		return
	}

	// 出站请求体写回**剥掉前缀**的模型名：前缀是网关的路由记号，
	// 上游不认识它（与 chatCompletions 同一条，见那里的注释）。
	outBody := body
	if hasPrefixIn(peek.Model) && reqModel != peek.Model {
		if rewritten, ok := rewriteModel(body, reqModel); ok {
			outBody = rewritten
		}
	}
	// 没有前缀时，reqModel 就是 peek.Model；但若模型名为空也该早报错。
	if strings.TrimSpace(reqModel) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 model")
		return
	}

	// 选号：限定在 reqProvider 的账号里，且按**模型**取额度
	//（生图模型与对话模型的可用额度是两笔账）。
	acct := h.cfg.Pool.PickFor(reqProvider, reqModel, nil)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			h.poolUnavailableHint(reqProvider))
		return
	}
	if !h.cfg.Pool.Acquire(acct.UID) {
		// 唯一可用账号的并发名额被抢走 → 503 而不是重试：
		// 生图很慢，让调用方稍后重试比在这里死等更好。
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"账号并发已满，请稍后重试")
		return
	}
	defer h.cfg.Pool.Release(acct.UID)

	// 找该上游的生图实现。**没实现就明确 501**，不回落别的上游 ——
	// 回落会让"生图请求打到了不支持的上游"变成一个静默的错图
	//（与 /admin/accounts/import 等分派端点同一条约定）。
	//
	// ⚠ 用接口断言发现可选能力，与 providerIDs() / ownedBy() 同一范式：
	// 出口层不认识任何具体上游，只问装配层"你能不能帮我生图"。
	gen, ok := h.cfg.Provider.(interface {
		ImageGen(ctx context.Context, id, uid string, body []byte) ([]byte, int, bool, error)
	})
	if !ok {
		writeOpenAIError(w, http.StatusNotImplemented, "image_not_supported",
			"本部署未接线生图（出口层没有可用的 ImageGen 适配器）")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), imageGenTimeout)
	defer cancel()

	// ⚠ 传 uid（不是完整凭证）：凭证目录只有上游自己知道，它按 uid 取 session。
	raw, status, supported, err := gen.ImageGen(ctx, reqProvider, acct.UID, outBody)
	if err != nil {
		// 传输层失败（网络/凭证取不到）→ 502。**不换号重试**（见函数头注释）。
		h.cfg.Pool.NoteError(acct.UID)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error",
			"生图请求失败: "+err.Error())
		return
	}
	if !supported {
		// 上游存在但没实现生图（或该 ID 未注册）。
		//
		// ⚠ 这里**不**记账号失败：这不是这个号的错，是路由/能力问题。
		// 记了会把一个健康账号冷却掉（"生图不支持"变成一个看似随机的账号故障）。
		writeOpenAIError(w, http.StatusNotImplemented, "image_not_supported",
			"上游 "+reqProvider+" 不支持生图")
		return
	}

	// 上游有应答：**状态码与响应体原样回**。
	//
	// 包括 4xx/5xx —— 上游的错误信封带 type/code/metadata，比网关
	// 重写一句话更有诊断价值（实测 404 时给出
	// `{"message":"该模型暂未开放","type":"not_found_error"}`）。
	//
	// ⚠ 也不在这里改账号状态：上游的业务错误（模型不对、提示词被拒）
	// 不是"这个号坏了"。只有 chat 出口那套 applyErrorPolicy 才有
	// 熔断语义，而它需要 providerID+状态分类，这里不做 ——
	// 少做一件没有证据支撑的事，比照抄一套不适用的策略好。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// requireReady 检查处理器已接线（池/注册表就绪）。
//
// 单独一个方法而不是在每个出口里重复判空：本包有测试用
// `Config{}` 构造空 handler，"未接线要说清楚"必须一致（否则
// 表现为 nil panic，而不是一条可读的错误）。
func (h *Handler) requireReady(w http.ResponseWriter, what string) bool {
	if h.cfg.Pool == nil || h.cfg.Provider == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "not_ready",
			what+"未接线（账号池或上游注册表为空）")
		return false
	}
	return true
}
