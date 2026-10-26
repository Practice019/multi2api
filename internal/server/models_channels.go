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
	"strings"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/modelmap"
	"workbuddy2api/internal/modelvis"
	"workbuddy2api/internal/store"
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
func (h *Handler) appendChannelModels(r *http.Request, data []map[string]any, scrub bool) []map[string]any {
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
	meta := h.channelModelMeta()
	for _, n := range extra {
		// scrub=true（普通 API Key 配了别名）时不标来源渠道：
		// 渠道名（csg / 七牛云）同样是「这个模型从哪来」的信息，
		// 与 owned_by 泄露上游是同一类问题。用户口径是「设置了别名就按别名处理」。
		ownedBy := owner[n]
		if scrub {
			ownedBy = ""
		}
		item := map[string]any{
			"id":     n,
			"object": "model",
		}
		if ownedBy != "" {
			item["owned_by"] = ownedBy
		}
		// ctx / max_output 与上游目录**同形**：有值才写。
		//
		// 没有这一段时，同一个 /v1/models 里渠道条目缺这两个键、
		// 上游条目有 —— 客户端按统一 schema 解析会得到 0 或 undefined。
		if mi, ok := meta[n]; ok {
			if mi.ContextLength > 0 {
				item["context_length"] = mi.ContextLength
			}
			if mi.MaxOutputTokens > 0 {
				item["max_output_tokens"] = mi.MaxOutputTokens
			}
		}
		out = append(out, item)
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

// channelModelMeta 公开名 → 渠道声明的模型元数据（ctx / max_output）。
//
// 与 channelOwnerByModel 同一手法：一个公开名可能被多条渠道提供，
// 取**优先级最高**的那条（与选路顺序一致 —— 目录里显示的元数据应当是
// 请求实际会走的那条，否则用户按它算预算会算错）。
//
// 元数据在渠道上（ChannelInfo），不在 map 上：同一个公开名换成别的上游
// 名字时窗口大小不变，它描述的是「这条渠道背后那个模型」的能力。
func (h *Handler) channelModelMeta() map[string]channelModelMetaInfo {
	s := h.cfg.Channels
	if s == nil {
		return nil
	}
	out := map[string]channelModelMetaInfo{}
	best := map[string]int{}
	for _, ch := range s.List() {
		if ch.Status != channelstore.StatusEnabled {
			continue
		}
		if ch.ChannelInfo.ContextLength <= 0 && ch.ChannelInfo.MaxOutputTokens <= 0 {
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
				out[pub] = channelModelMetaInfo{
					ContextLength:   ch.ChannelInfo.ContextLength,
					MaxOutputTokens: ch.ChannelInfo.MaxOutputTokens,
				}
			}
		}
	}
	return out
}

// channelModelMetaInfo 一个渠道模型的元数据（0 = 未知）。
type channelModelMetaInfo struct {
	ContextLength   int64
	MaxOutputTokens int64
}

// ── T09：models + vendors 两级目录（seed + 合成行 API）──────────────────────
//
// # 为什么 seed 的口径必须与 /v1/models 是**同一条管线**
//
// /v1/models 的可见集合是**加**出来的（上游目录 ∪ 渠道公开名，见文件头），
// 而 SQL 层的 models/vendors 要能回答"这个 chips 归谁"。两者唯一安全的关系
// 是**相等**：用户在 chips 上看到一条，目录里就得有一行。若 seed 另写一份聚合
// （哪怕只差一个 map 遍历顺序），立刻出现两套目录 —— 而"chips 里有、目录里
// 查不到"是最难查的一类不一致（两边都不报错，只有人对不上账时才发现）。
//
// 所以这里不重新发明聚合，直接调 models() 用的那三步
// （modelListWithScrub → appendChannelModels → applyPublicNames），
// 只把结果投影成 store.CatalogEntry。
//
// ⚠ 与 /v1/models 有两处**故意不同**，各自有理由：
//
//  1. **不叠 filterModelsForRequest**：那层是按 API Key 的白名单/分组过滤，
//     是权限视图不是目录。seed 进库的必须是全局目录 —— 否则先来的那把钥匙
//     会把自己的白名单永久写进 models 表，后加入的模型对所有人都不见了。
//  2. **传 scrub=false**：目录要记上游真名（owned_by）作为一级行。scrub=true
//     是"对普通 key 隐藏来源"；把它写进库等于把某个 key 的隐私视图变成全局事实。

// catalogEndpointChat seed 时每行声明的端点。
//
// 来源是 /v1/models（对话目录），所以是 chat。将来有 embeddings/images 目录
// 时按来源补，**不要**在这里猜：猜出来的端点会变成"目录说有、请求却 404"。
const catalogEndpointChat = "chat"

// upstreamCatalogEntries 取**各上游目录**并投影成两级目录项。
//
// 它是 /v1/models 管线的前两步（modelListWithScrub → applyPublicNames），
// **不含** appendChannelModels 那一步 —— 渠道那半由合成行 API 补，
// 且只有分开算，"渠道用到但目录没有"才有确定的含义：
// 缺的是相对于**上游目录**，不是相对于"已经并过渠道的目录"（那恒为空集，
// 合成行 API 会退化成一个永远返回空的函数 —— 这正是第一版的错）。
func (h *Handler) upstreamCatalogEntries() []store.CatalogEntry {
	data := h.modelListWithScrub(false)
	data = h.applyPublicNames(nil, data)
	out := make([]store.CatalogEntry, 0, len(data))
	for _, m := range data {
		name, _ := m["id"].(string)
		if name == "" {
			continue
		}
		owner, _ := m["owned_by"].(string)
		if owner == "" {
			owner = catalogVendorOfID(name)
		}
		out = append(out, store.CatalogEntry{
			Vendor:    owner,
			Name:      name,
			Endpoints: []string{catalogEndpointChat},
		})
	}
	return out
}

// catalogSeedEntries 是入库的全量目录：上游目录 ∪ 渠道合成行。
//
// 它与 /v1/models 的可见集合**相等**（appendChannelModels 做的就是"上游 ∪ 渠道"
// 的去重并集；这里只是把渠道那半换成带合成标记的版本），这正是 T09 计数判据
// 「models 行数 = 可见 chips 基数」的成立依据。
func (h *Handler) catalogSeedEntries() []store.CatalogEntry {
	base := h.upstreamCatalogEntries()
	// ⚠ 显式复制再拼：append(base, ...) 会写进 base 的底层数组（若还有余量）。
	// 这条坑在 appendChannelModels 的注释里已经踩过一次，不再踩第二次。
	entries := append([]store.CatalogEntry(nil), base...)
	return append(entries, h.channelSyntheticCatalogEntries(store.CatalogEntryNames(base))...)
}

// catalogVendorOfID 从 "provider/model" 里切出上游 id，作为没有 owned_by 时的
// 一级行兜底。
//
// 为什么切前缀而不是一律 unknown：多上游目录里的 id 自带上游（收窄之后恒定如此），
// 丢掉这层信息会让一级行退化成一条"unknown 装了所有东西"—— 目录白建。
// 单上游（没有 "/"）时同样兜不住，才回落到 unknown（那是真的无从得知）。
func catalogVendorOfID(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[:i]
	}
	return store.CatalogVendorUnknown
}

// channelSyntheticCatalogEntries 是服务器侧的合成行来源：渠道声明在用、
// 但**上游目录里没有**的公开名。
//
// known 由调用方传入（seed 路径上就是刚算出来的上游目录），这样同一份判据
// 不会因为"算两遍"而出现两次快照不一致。
func (h *Handler) channelSyntheticCatalogEntries(known []string) []store.CatalogEntry {
	names, ok := h.channelPublicNames(nil)
	if !ok || len(names) == 0 {
		return nil
	}
	owner := h.channelOwnerByModel()
	return store.SyntheticCatalogEntries(known, names, func(name string) string {
		return owner[name]
	})
}

// ChannelSyntheticCatalogEntries 暴露"渠道用到但目录没有"的合成行。
//
// 它是给**展示侧**用的（目录视图要把这些行并进列表，不一定要落库）；
// seed 路径见 SeedModelCatalog。两者共用 store.SyntheticCatalogEntries
// 这一份判据 —— 展示与入库的口径必须一致，否则"页面上有、库里没有"。
func (h *Handler) ChannelSyntheticCatalogEntries() []store.CatalogEntry {
	return h.channelSyntheticCatalogEntries(store.CatalogEntryNames(h.upstreamCatalogEntries()))
}

// SeedModelCatalog 把「上游目录 + 渠道合成行」seed 进 models/vendors（幂等）。
//
// # 为什么合成行也一起 seed（而不是只 seed 上游）
//
// 计数判据是「models 行数 = /v1/models 可见 chips 基数」。渠道声明的名字**确实
// 出现在** /v1/models 里（appendChannelModels 并进来的），若不入库，
// 等式立刻不成立 —— 而且用户会在 chips 上看到一个目录里不存在的模型。
//
// 入口是 *Handler 而不是包级函数：上游目录只有出口层拿得到（它同时知道
// "有哪些上游"与"每个上游给什么"），装配层只把 ProviderRouter 注入进来。
func (h *Handler) SeedModelCatalog() (store.CatalogSeedStats, error) {
	return store.SeedModelCatalog(h.catalogSeedEntries())
}

// CatalogRows 读回两级目录（平铺，含合成行标记）。
//
// 为什么是 Handler 方法而不是让调用方直接 import store：目录的行数/来源
// 将来可能还要叠渠道元数据（ctx / max_output，见 channelModelMeta），
// 那需要访问 Handler。现在先做一层薄封装，调用点不必再改。
func (h *Handler) CatalogRows() ([]store.CatalogRow, error) {
	return store.ListCatalogRows()
}
