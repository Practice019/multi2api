// 扩展点：上游自报"我怎么生图"。
//
// # 为什么需要它
//
// Loomy（`loomyad.xunfei.cn`）上有两个**只能生图、不能对话**的模型：
//
//	doubao-seedream-5-lite   豆包 Seedream 5 Lite
//	qwen-image-3.0-pro       通义万相
//
// 它们的 `modalities.output` 只有 `image`（见 Loomy 客户端的 opencode.json），
// 走 `/chat/completions` 会得到 404「该模型暂未开放」，但走
// `/images/generations` **完全可用**（实测 HTTP 200 + 2304x1728 PNG）。
//
// 所以"能不能用"取决于**端点**，而 `/v1/chat/completions` 这条唯一出口
// 表达不了"我要生图"。本扩展点就是补上这条出口。
//
// # 职责边界：核心只做转发，不解释生图协议
//
// 与 QuotaExt / DailyCheckinExt 同一条判据：**协议细节是上游的事实**
// （Loomy 用 `{model,prompt,n,size,response_format}`、返回
// `{data:[{url}]}`，别的上游可能完全不同）。
//
// 核心只负责三件事：
//
//  1. 鉴权（复用网关既有的 Bearer）
//  2. 从账号池取一个可用凭证
//  3. 把上游的响应**原样**回给客户端
//
// 它**不解析** prompt、不校验 size、不重排响应字段 —— 那是上游的事。
// 这样加一个新上游不需要动核心。
//
// # 为什么响应原样透传（不转存图片）
//
// Loomy 返回的是腾讯 COS 的**签名直链**（`q-sign-time` 实测有效期约 12 小时）。
// 调用方拿到 URL 会**立即**下载保存（所有生图客户端都是这个行为），
// 12 小时远远够用 —— 这与 Loomy 官方客户端自己的做法一致
// （它也只在"落盘到工作区"时才下载）。
//
// 刻意**不**在网关里转存：那会把"几十 MB 的图片"塞进网关的磁盘与内存，
// 而网关的定位是转发。真要落盘是调用方的事。
package gateway

import "context"

// ImageGenExt 上游自报"我支持生图，请求这样发"。
//
// 可选实现：不实现的上游，`POST /v1/images/generations` 会回 501
// （**明确失败**，不回落默认上游 —— 与其余分派端点同一条约定）。
type ImageGenExt interface {
	// GenerateImage 用某个账号向上游发起一次生图，返回**上游的原始响应体**。
	//
	// 参数：
	//
	//	uid   用哪个账号（凭证由上游自己从 uid 取 —— 核心只给主键）
	//	body  请求体**原文**（未解析的 JSON）。上游可以原样转发，
	//	      也可以按自己的协议改写（例如补 model 的默认值）。
	//
	// 返回 (响应体, HTTP 状态码, error)：
	//
	//	error != nil        传输层失败（网络/凭证取不到）→ 核心回 502
	//	error == nil        上游有应答，状态码原样回给调用方
	//	                    （**包括 4xx/5xx** —— 上游的错误信封对调用方
	//	                     更有诊断价值，不该被网关吞掉换成自己的措辞）
	//
	// ⚠ 实现不得 panic（契约要求，与 Provider 一致）。
	//
	// ⚠ 必须**先取凭证再发请求**，取值失败要返回 error 而不是发一个空凭证
	// 出去 —— 后者会被上游当成"token 缺失"（Loomy 返回的正是
	// `{"code":"100002","desc":"缺少 token"}`，那是误导性的失败）。
	GenerateImage(ctx context.Context, uid string, body []byte) ([]byte, int, error)
}

// ImageModelExt 上游自报"我有哪些**只能生图**的模型"。
//
// # 为什么与 ImageGenExt 分开（两个接口而不是一个）
//
// 因为它们的消费者**不同**：
//
//	ImageGenExt   → `POST /v1/images/generations` 的处理路径（要发请求）
//	ImageModelExt → `GET /v1/models` 的目录构建（只要一个名字列表）
//
// 合在一起会让"只想在模型目录里露个名字"的上游被迫实现发请求的方法。
// 分开后，两个都可以独立实现、独立测试。
//
// # 为什么模型目录需要它
//
// `KnownModels()` 里的 `Unavailable` 语义是"**对话**不可用"，
// 而这两个模型恰恰是"对话不可用但生图可用"。目录若不区分，
// 调用方在 `/v1/models` 里根本看不到它们，也就不会来调生图端点。
type ImageModelExt interface {
	// ImageModels 返回**仅用于生图**的模型 ID（含可用于生图的通用模型）。
	//
	// 这些 ID 会被并进 `/v1/models` 目录，并带上 `image` 能力标记。
	// 返回空切片表示"我没额外可报的"。
	ImageModels() []ImageModel
}

// ImageModel 一个生图模型在目录里的投影。
//
// # 为什么是"投影"而不是完整 ModelInfo
//
// 目录要的只有"ID + 展示名 + 它是生图的"，其余字段
// （上下文窗口 / 输出上限）对生图模型**没有意义** ——
// 生图不消费 token 上下文。硬塞一组 0 会让目录里出现
// "上下文 0" 的可疑条目（本项目已有 `qwen-image-3.0-pro`
// 就是这种形态，见 models.go 的注释）。
type ImageModel struct {
	// ID 上游的模型 ID（逐字，请求时要原样发回去）。
	ID string
	// Name 展示名（可空；空则目录回落成 ID）。
	Name string
}
