// models_channels.go 对外模型目录的**渠道口径**（T5 接线）。
//
// # 什么时候走这条口径（关键：不改变既有部署的行为）
//
// 配了渠道存储（cfg.Channels != nil）⇒ 对外目录**由渠道能力索引决定**；
// 没配 ⇒ 一个字节都不变，仍旧走上游目录（h.modelList）。
// 这样"中转站模式"是**显式开启**的，既有部署与既有测试不受影响。
//
// # 口径（与 new-api 一致，默认收紧）
//
//	对外可见 = 已启用渠道声明并有能力的模型
//	           ∩ 令牌白名单（非空的 key 才受限，见 allowedModelsForRequest 的三态）
//	           → 再按每渠道的 model_mapping **反向**换成公开名
//
// 计算本身在 internal/modelvis（纯函数、8 条测试）；这里只负责把三份数据取出来喂给它。
//
// ⚠ 这里**不再**叠加 filterModelsForRequest：modelvis 已经把白名单算进去了，
// 再叠一次会出现两套名字口径（那层按 "provider/model" 比对，而对外名字是公开名）——
// 那正是"列表里看得见、发请求却 403"的来源。
package server

import (
	"net/http"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/modelmap"
	"workbuddy2api/internal/modelvis"
)

// channelPublicNames 取"对外可见的模型名"，返回 (名字, 是否走渠道口径)。
func (h *Handler) channelPublicNames(r *http.Request) ([]string, bool) {
	s := h.cfg.Channels
	if s == nil {
		return nil, false
	}
	abilities := s.EnabledModels(channelstore.DefaultGroup)
	reverse := h.channelReverseMapping()
	allowed, restricted := h.allowedModelsForRequest(r)
	return modelvis.PublicModels(abilities, allowed, restricted, reverse), true
}

// channelReverseMapping 汇总所有渠道的映射，得到"上游真实名 → 公开名"。
//
// 多条渠道声明同一上游名时**先到先得**（List 的顺序稳定 ⇒ 结果确定）。
// 冲突不在这里报错：单个渠道内部的环已由 modelmap.Validate 在保存时拦下，
// 而跨渠道的同名映射只是"两条渠道都对外叫这个名字"，对外目录会把它们折叠成一条。
func (h *Handler) channelReverseMapping() map[string]string {
	s := h.cfg.Channels
	if s == nil {
		return nil
	}
	out := map[string]string{}
	for _, ch := range s.List() {
		if len(ch.ModelMap) == 0 {
			continue
		}
		rev, _ := modelmap.Mapping(ch.ModelMap).Reverse()
		for real, pub := range rev {
			if _, exists := out[real]; !exists {
				out[real] = pub
			}
		}
	}
	return out
}

// appendChannelModels 把渠道声明的公开名**并进**上游目录（只增不减、按 id 去重）。
//
// # 它为什么存在（两次反馈的差别）
//
// 第一次用户看到的是「除新建渠道外其余上游全是 0 个」⇒ 那是收紧（目录只由渠道决定）；
// 第二次用户看到的是「可用模型 674 个里没有我那条渠道」⇒ 那是收紧撤过头（目录只看上游）。
// 两次都不是"要不要渠道"，而是**并集**：上游照旧全量，渠道声明的也进来。
//
// 名字排序：已在上游目录里的同名**不重复添加**（上游那份为准，它的 owned_by 更准确）。
//
// ⚠ 为什么要先复制再追加：data 可能来自 h.modelList() 的缓存切片（共享底层数组）。
// 就地 append 会写进缓存底层数组 —— 后续请求会看到"幽灵模型"，并发下更是数据竞争。
// 这个坑在单请求测试里完全看不出来，所以我在这里显式复制。
func (h *Handler) appendChannelModels(r *http.Request, data []map[string]any) []map[string]any {
	names, ok := h.channelPublicNames(r)
	if !ok || len(names) == 0 {
		return data
	}
	seen := make(map[string]bool, len(data)+len(names))
	for _, m := range data {
		if id, _ := m["id"].(string); id != "" {
			seen[id] = true
		}
	}
	extra := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" && !seen[n] {
			seen[n] = true
			extra = append(extra, n)
		}
	}
	if len(extra) == 0 {
		return data
	}
	out := make([]map[string]any, len(data), len(data)+len(extra))
	copy(out, data)
	owner := h.channelOwnerByModel()
	for _, n := range extra {
		out = append(out, map[string]any{
			"id":       n,
			"object":   "model",
			"owned_by": owner[n],
		})
	}
	return out
}

// channelModelsForRequest 返回对外目录（OpenAI 形状的 data 数组）。
// 没有渠道存储时返回 nil ⇒ 调用方回落既有路径。
//
// ⚠ 现在**不再**被 /v1/models 调用（撤收紧后用 appendChannelModels 做并集）；
// 保留它是给"只要渠道视角"的调用方（以及它的测试）用的，语义没变。
func (h *Handler) channelModelsForRequest(r *http.Request) []map[string]any {
	names, ok := h.channelPublicNames(r)
	if !ok {
		return nil
	}
	owner := h.channelOwnerByModel()
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		item := map[string]any{
			"id":       name,
			"object":   "model",
			"owned_by": owner[name],
		}
		data = append(data, item)
	}
	return data
}

// channelOwnerByModel 给公开名标一个来源渠道名（前端展示"这个模型是谁提供的"）。
//
// 一个公开名可能被多条渠道提供 ⇒ 取**优先级最高**的那条（与选路顺序一致：
// 目录里显示的来源，应当是请求实际会走的那条，否则用户看到的来源是假的）。
func (h *Handler) channelOwnerByModel() map[string]string {
	s := h.cfg.Channels
	if s == nil {
		return nil
	}
	out := map[string]string{}
	best := map[string]int{}
	for _, ch := range s.List() {
		if ch.Status != channelstore.StatusEnabled {
			continue
		}
		rev, _ := modelmap.Mapping(ch.ModelMap).Reverse()
		for _, real := range ch.Models {
			pub := real
			if p, ok := rev[real]; ok && p != "" {
				pub = p
			}
			if old, seen := best[pub]; !seen || ch.Priority > old {
				best[pub] = ch.Priority
				out[pub] = ch.Name
			}
		}
	}
	return out
}
