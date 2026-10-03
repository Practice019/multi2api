// imagegen_ext.go Loomy 实现 gateway.ImageGenExt / ImageModelExt
// —— "我怎么生图 / 我有哪些生图模型"。
//
// # 为什么需要（实测的根因）
//
// 本上游有两个**只能生图、不能对话**的模型：
//
//	doubao-seedream-5-lite   豆包 Seedream 5 Lite
//	qwen-image-3.0-pro       通义万相
//
// 它们走 `/chat/completions` 会得到 HTTP 404「该模型暂未开放」，
// 但走 `/images/generations` **完全可用**。实测（2026-10-03）：
//
//	POST /api/v1/images/generations  {"model":"doubao-seedream-5-lite",…}
//	→ HTTP 200  {"created":…,"data":[{"url":"https://…cos…png?q-signature=…"}],
//	             "points_consumed":110}
//	耗时 23.3 秒，产物 2304x1728 PNG
//
// ⚠ 这条实测**推翻**了 models.go 里把这两个模型标 `Unavailable` 的旧结论 ——
// 那个标记的判据只是"chat 端点 404"，而"不能对话"与"不能用"是两件事。
// 详见 docs/loomy-local-toolchain.md 第四节。
//
// # 双轨鉴权在**这个**端点上与既有两个都不同
//
// client.go 记录的判据是"一个端点认一个头"：
//
//	GET  /models            →  token
//	POST /chat/completions  →  Authorization: Bearer
//
// 生图端点实测**两个都认**，而 Loomy 官方客户端两个都发
// （见其 image-generation-service.js 的 buildAuthHeaders：
// "session 模式同时塞 token + Authorization（兼容 Loomy iModel 两种入参）"）。
//
// 本实现跟随官方客户端的做法**两个都发**：这是唯一有实测背书的形态。
// 只发一个也许能通，但没有任何证据，不值得为省一个头去赌。
package loomy

import (
	"context"
	"fmt"

	"workbuddy2api/internal/gateway"
)

// imageModelIDs 本上游**仅用于生图**的模型（实测清单）。
//
// ⚠ 与 models.go 的 knownModels 是**两个轴**：
//
//	knownModels   "这个模型能不能走 /chat/completions"
//	imageModelIDs "这个模型能不能走 /images/generations"
//
// 两个生图模型在前者是"不可用"、在后者是"可用"。这正是旧结论写错的地方：
// 当时只有一个轴，于是把"chat 不可用"记成了"不可用"。
var imageModelIDs = []gateway.ImageModel{
	{ID: "doubao-seedream-5-lite", Name: "Doubao seedream 5 lite"},
	{ID: "qwen-image-3.0-pro", Name: "Qwen Image 3.0 Pro"},
}

// GenerateImage 用某个账号向上游发起一次生图（gateway.ImageGenExt）。
//
// # 本方法只做两件事
//
//	① 按 uid 取 session（取不到就明确报错，不发空凭证）
//	② 交给 Client 发请求（HTTP 细节全在 client.go）
//
// 刻意**不**解析请求体、不改写 response_format、不下载图片：
// 协议是上游的事实，网关是转发层（见 gateway.ImageGenExt 的文件头注释）。
func (p *Provider) GenerateImage(ctx context.Context, uid string, body []byte) ([]byte, int, error) {
	if p == nil {
		return nil, 0, fmt.Errorf("loomy: provider 未初始化")
	}
	a := p.authByUID(uid)
	if a == nil {
		// ⚠ 明确报错而不是发空凭证：后者会被上游当成"token 缺失"
		//（实测返回 `{"code":"100002","desc":"缺少 token"}`）——
		// 那是一个把人往"鉴权头写错了"方向带的误导性失败。
		return nil, 0, fmt.Errorf("loomy: 找不到账号 %s 的凭证（无法生图）", shortUID(uid))
	}
	return p.client.GenerateImage(ctx, a, body)
}

// ImageModels 报出本上游的生图模型（gateway.ImageModelExt）。
//
// 这些 ID 会被并进 `/v1/models`，调用方据此知道"这个上游能生图、能用哪些模型"。
// 返回副本，避免调用方改到包级切片。
func (p *Provider) ImageModels() []gateway.ImageModel {
	out := make([]gateway.ImageModel, len(imageModelIDs))
	copy(out, imageModelIDs)
	return out
}

// 编译期断言：Provider 实现生图两个扩展点。
//
// 不实现会在这里编译失败，而不是等到运行时"调用生图端点回 501"。
var (
	_ gateway.ImageGenExt   = (*Provider)(nil)
	_ gateway.ImageModelExt = (*Provider)(nil)
)
