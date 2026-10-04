// imagedisplay_rewrite.go 把生图响应里的**签名 URL 换成可预览的裸 URL**。
//
// # 为什么需要它（用户报障：「图片无法预览 / 拿不到文件实体」）
//
// 上游返回的是腾讯 COS **签名** URL。实测（10/10 复现）：
//
//	带签名 URL  GET → 200 ✓   HEAD → **403 ✗**
//	裸 URL      GET → 200 ✓   HEAD → 200 ✓
//
// **HEAD 必然失败**：COS 把 HTTP method 也算进签名，而 URL 是按 GET 签的。
// 而预览器/取图器/文件发送队列**通常先发 HEAD** 探测类型与大小 ——
// 一探测就 403，于是显示"无法预览"、"file unavailable"。
//
// ⚠ 关键在于：**对象实体一直都在**（GET 200、大小正确）。所以
// "上传到对象存储的管道断了"是一个会把人带偏的结论 ——
// 真因只是 method 不匹配，而现象却是"取不到文件"。
//
// # 为什么改在 GenerateImage 里（而不是各调用方各自处理）
//
// `GenerateImage` 是**两条生图路径的共同入口**：
//
//	对话工具路径  execGenerateImage → p.GenerateImage
//	生图端点路径  /v1/images/generations → ImageGen → p.GenerateImage
//
// 我第一版只在对话工具路径做了裸 URL 转换，结果**端点路径漏了** ——
// 实测 `/v1/images/generations` 返回的仍是签名 URL（HEAD 403）。
// 一处漏改、两条路径行为不一致，是"同一件事有两个地方做"的典型代价。
//
// 现在改在共同入口：两条路径自动一致，且核心（internal/server）
// **不需要认识 COS**（架构约束：核心不得依赖任何具体上游）。
package loomy

import (
	"context"
	"encoding/json"
)

// rewriteImageURLs 把响应里每个 data[].url 换成可预览的形态。
//
// 返回改写后的响应体。**任何一步失败都返回原样** ——
// 改写是"让预览器能工作"的增强，绝不是"不改就出错"的必需品。
// 拿不到更好的形态时保留原签名 URL（它 GET 仍可用）。
//
// # ⚠ 必须改「原地」而不是「重新构造」（我第一版在这里丢了字段）
//
// 我第一版定义了一个只含 `Data []map[string]any` 的结构体，
// 反序列化 → 改 url → 重新 Marshal。结果是**顶层其它字段全丢**：
//
//	上游原始:  {"created":…, "data":[…], "points_consumed":110}
//	我的输出:  {"data":[…]}                    ← created 与 points_consumed 没了
//
// 而 `points_consumed` 是**计费依据** —— 丢掉它不是格式问题，是
// 让调用方再也算不出这次花了多少积分。
//
// 这是"局部反序列化再整体序列化"的经典数据损失：结构体只声明了
// 你想动的那些字段，Marshal 时就只剩它们。所以这里改成对**原始 map**
// 原地改（只碰 data[].url），其余字段原样带出去。
func (p *Provider) rewriteImageURLs(ctx context.Context, raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	// 用 map 而不是结构体：目的是"只改一个字段、其余原样保留"，
	// 结构体做不到（它会替我们决定留哪些字段）。
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		// 不是我们认识的形状（错误信封、上游改格式…）→ 原样返回。
		// 不报错：本函数在响应路径上，不该把一个能用的响应变成失败。
		return raw
	}
	items, ok := top["data"].([]any)
	if !ok || len(items) == 0 {
		return raw
	}

	hc := p.imageProbeClient()
	changed := false
	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		u, _ := item["url"].(string)
		if u == "" {
			continue
		}
		display, usedBare := displayImageURL(ctx, hc, u)
		if !usedBare || display == u {
			continue
		}
		// ⚠ 保留原签名 URL 到 `url_signed`：它是**兜底**。
		// 万一 bucket 将来改成私有读（裸 URL 会 403），调用方还有一条路。
		item["url"] = display
		item["url_signed"] = u
		item["url_is_bare"] = true
		changed = true
	}
	if !changed {
		// 探测失败（bucket 私有 / 网络抖动）：原样返回。
		// 此时调用方拿到签名 URL —— GET 可用，只是 HEAD 会 403。
		return raw
	}

	out, err := json.Marshal(top)
	if err != nil {
		return raw
	}
	return out
}
