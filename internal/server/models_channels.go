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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/channelstore"
	"workbuddy2api/internal/gateway"
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
	names := modelvis.PublicModels(abilities, allowed, restricted, reverse)
	// ⚠⚠ 补上"被别的渠道抢走裸名"的那一份 —— 用户实测报障的真实缺陷（见下面的长注释）。
	names = append(names, h.channelShadowedPublicNames(reverse, allowed, restricted)...)
	return names, true
}

// channelShadowedPublicNames 补出"裸名被别的渠道抢走、因而在目录里消失"的渠道模型名。
//
// # 判据（为什么不是"给每条渠道的每个模型都补"）
//
// 只补**确实被遮蔽**的那些，判据三条同时成立：
//
//	① 该渠道没有为这个模型写 ModelMap（写了映射的渠道，它的对外名就是那个别名，
//	   再暴露 <渠道名>/<模型> 等于凭空多出一个用户没打算公开的名字）；
//	② 全局反向表里这个名字的得主**不是**本渠道（先到先得 ⇒ 本渠道输了）；
//	③ 名字本身可用（白名单口径与 modelvis.PublicModels 一致）。
//
// 不这么收窄的后果是"无冲突场景下目录也会变大"——实测会打破既有的目录基数守卫
// （TestStoreModelCatalogSeed 的 chip 基数、TestModelsCatalogViewMarksSyntheticUnselectable
//
//	的合成行归属），那不是本次要修的东西。
//
// # 为什么必须补（用户实测报障）
//
// # 为什么必须补这一份（用户实测报障）
//
// 对外目录的名字来自两条**按裸模型名收敛**的链路：
//
//	channelstore.EnabledModels  —— 按 ab.Model 去重 ⇒ 丢掉"这个模型属于哪条渠道"
//	channelReverseMapping       —— map[裸上游名]公开名；多条渠道声明同名时**先到先得**
//
// 于是两条渠道声明同一个模型名时，只有一条能出现在目录里，另一条的模型**彻底消失**
// （路由侧其实两条都能服务，只是名字没被暴露出来）。
//
// 实测证据（用户报「渠道有这些模型，但分组里找不到」，渠道声明 8 个、目录只见 3 个）：
//
//	deepseek-flash       ← 与 apigoto      重名 ⇒ 缺
//	deepseek-v4-flash    ← 与 token360     重名 ⇒ 缺
//	deepseek-v4.1-flash  ← 与 token360     重名 ⇒ 缺
//	glm-5.2              ← 与 token360     重名 ⇒ 缺
//	kimi-k3              ← 与 token360     重名 ⇒ 缺
//	sensenova-6.8-flash  ← 该渠道独有 ⇒ 在目录里（所以只剩 3 个）
//
// 命名空间形式 `<渠道名>/<上游名>` 本来就是路由侧认得的形状
// （channelroute.upstreamName 走 modelmap.SplitNamespaced），非重名模型一直就是这个形态 ——
// 这里只是把重名的那一半补齐，让"每条渠道的每个模型"都可见、可选。
func (h *Handler) channelShadowedPublicNames(reverse map[string]string, allowed []string, restricted bool) []string {
	s := h.cfg.Channels
	if s == nil {
		return nil
	}
	allow := make(map[string]bool, len(allowed))
	for _, w := range allowed {
		if w != "" {
			allow[w] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, ch := range s.List() {
		if ch == nil || ch.Status != channelstore.StatusEnabled {
			continue
		}
		rev, _ := modelmap.Mapping(ch.ModelMap).Reverse()
		for _, m := range ch.Models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			// ① 有映射的模型不补：它的对外名是用户指定的别名。
			if p, ok := rev[m]; ok && p != "" {
				continue
			}
			want := modelmap.Namespaced(ch.Name, m)
			// ② 没被抢走 ⇒ 目录里已经有这个名字，不必补。
			if got, ok := reverse[m]; ok && got == want {
				continue
			}
			if _, exists := reverse[m]; !exists && want == m {
				// 反向表里根本没有这个名字（例如能力行缺失）：交给既有链路，别在这里造名字。
				continue
			}
			// 白名单按**公开名**挑，与 modelvis.PublicModels 同一口径。
			if restricted && !allow[want] {
				continue
			}
			if seen[want] {
				continue
			}
			seen[want] = true
			out = append(out, want)
		}
	}
	return out
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
	// ② 没写映射的声明模型：公开名 = <渠道名>/<上游名>（共享规则见 modelmap.Namespaced）。
	//
	// 不这么补的话，modelvis 会按"没有反向项就原样输出"把**上游原名**当公开名暴露出去 ——
	// 用户实测到的 `fake-refusal-test`（无前缀）就是这么进到请求日志里的。
	//
	// 显式映射**优先**：上面的循环先跑，这里只在 out 里还没有这个上游名时才补（先到先得，顺序稳定）。
	for _, ch := range s.List() {
		if ch == nil {
			continue
		}
		for _, m := range ch.Models {
			if m == "" {
				continue
			}
			if _, exists := out[m]; exists {
				continue // 已有显式映射（或前面的渠道已认领这个名字）
			}
			out[m] = modelmap.Namespaced(ch.Name, m)
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
			} else {
				// ⚠ 与 channelReverseMapping / channelroute.upstreamName 用**同一份**规则：
				//   没写映射的声明模型，公开名是 <渠道名>/<上游名>（modelmap.Namespaced）。
				//
				// 这里原来就是 `pub := real`（拿裸名当键）—— 它是同一个漏法的第 2、3 个副本：
				// 目录里名字已经带前缀、这份表还按裸名索引 ⇒ 查不到 ⇒ 目录视图里合成行的
				// "供应商"列变成 unknown（用户看到的就是那列空掉）。
				//
				// 三处同规则是硬要求：否则必然分叉成"目录里看得见、来源标不出"。
				pub = modelmap.Namespaced(ch.Name, real)
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
			} else {
				// ⚠ 与 channelReverseMapping / channelroute.upstreamName 用**同一份**规则：
				//   没写映射的声明模型，公开名是 <渠道名>/<上游名>（modelmap.Namespaced）。
				//
				// 这里原来就是 `pub := real`（拿裸名当键）—— 它是同一个漏法的第 2、3 个副本：
				// 目录里名字已经带前缀、这份表还按裸名索引 ⇒ 查不到 ⇒ 目录视图里合成行的
				// "供应商"列变成 unknown（用户看到的就是那列空掉）。
				//
				// 三处同规则是硬要求：否则必然分叉成"目录里看得见、来源标不出"。
				pub = modelmap.Namespaced(ch.Name, real)
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

// ── T14：模型管理页（/ui/models）与它的取数端点（/ui/models/data）───────────
//
// # 为什么取数端点在 /ui/ 下，而不是 /admin/ 下
//
// 页面的身份判据是 h.uiIdentify（会话 cookie / admin token，见 webui.go 的 ui）。
// /admin/* 子树是**另一套**门禁，且它的注册表在 internal/admin（装配层）。
// 把数据挂到 /admin/ 下要么多一套判据，要么让页面去用一把它拿不到的钥匙 ——
// 后者正是 groups_catalog.go 开头记着的那个事故（/v1/models 要求 API key，
// 管理台拿到的是 admin 身份，于是稳定 401）。
//
// 所以页与数据**同一套判据、同一个包**：没有第二种身份，也没有第三个真相源。
//
// # 为什么是两条显式路由，而不是一条按 Accept 分叉
//
// 一条路由两种表现会让 iframe 的 src 与 fetch 的 URL 长得一样：贴日志/看缓存/
// 排查时得先问"你打的是哪一种"。显式两条路由代价只有 handler.go 一行。
//
// # 数据从哪来（两档，按接线状态自动选）
//
//  1. **store（T11 之后）**：models/vendors 两张表是权威目录（T09 seed）。
//     非空就用它 —— 页面显示的就是 SQL 里那一份，与目录 seed 一一对应。
//  2. **memory（当前，T11 之前）**：直接复用 /v1/models 的**同一条管线**
//     （upstreamCatalogEntries + channelSyntheticCatalogEntries，见 T09 段），
//     按需实时算。它与 /v1/models 的可见集合相等，所以页面上不会出现
//     "chips 里有、目录里没有"这类不一致。
//
// 为什么要这个回落而不是"库没接线就报错"：T14 的验收（隔离实例 + 真浏览器）
// 跑在 T11 之前也必须能验到东西；而一个只会报"未接线"的页面在 Wave 1 收口前
// 完全不可测 —— 那不是严谨，是把验证推给下一个任务。

// catalogViewModel 是页面 Model tab 的一行。
//
// 字段名就是**前端契约**（webui-models.html 按它们取值），改这里必须同时改页面。
type catalogViewModel struct {
	// Name 对外可见 id（多上游下是 "provider/model"，与 /v1/models 同口径）。
	Name string `json:"name"`
	// Vendor 一级行（上游 id / 渠道名）。
	Vendor string `json:"vendor"`
	// Endpoints 该模型支持的端点（目前恒为 chat）。
	Endpoints []string `json:"endpoints"`
	// Synthetic 渠道声明在用、但上游目录里没有 ⇒ 由合成行 API 补出来的。
	Synthetic bool `json:"synthetic"`
	// Selectable 能不能被勾选。**恒等于 !Synthetic**，但仍然单独下发：
	// 页面的"不可勾选"必须来自数据而不是前端自己再推一遍 —— 两处推导
	// 迟早分叉（一边改了另一边不改，表现是"看起来能勾、点了没反应"）。
	Selectable bool `json:"selectable"`
}

// catalogViewVendor 是页面 Vendor tab 的一行（由 Model 行聚合而来）。
type catalogViewVendor struct {
	// Name 供应商名（= models 行的 vendor）。
	Name string `json:"name"`
	// Models 它下面的模型数。
	Models int `json:"models"`
	// Synthetic 其中合成行数（渠道补出来的）。单独一列：一个 vendor
	// 全是合成行（渠道，没在上游目录里注册）与全是真行，处置完全不同。
	Synthetic int `json:"synthetic"`
}

// catalogFreshnessView 是 /ui/models/data 里那份「这份目录是什么时候取的」。
//
// # 为什么必须有它（用户报障的直接修法）
//
//	「反代模型目录有 bug，感觉列表目录都不是实时的。
//	  前端读取本地缓存，但是本地缓存需要有办法可以更新。」
//
// 缓存（1h TTL）是对的 —— 它挡的是「每个客户端连一次就打一轮上游」。缺的是**可见性**：
// 在此之前页面上唯一的时刻是 webui-models.html 的「更新于 HH:MM:SS」，那是**这个页面
// 这次请求的时刻**，与目录何时从上游取回无关 ⇒ 一份 59 分钟前的目录看起来和刚取的
// 一模一样，「刷新到底有没有生效」无从判断。所以把上游取数时刻如实下发。
//
// 字段语义（**前端契约**：改这里必须同时改 webui-models.html）：
//
//	fetched_at  RFC3339；空串 = 不知道（冷缓存 / SQL 目录档）
//	age_seconds 距上次取数的秒数；**-1 = 不知道**（为 -1 时不要读 stale）
//	ttl_seconds 正缓存时长（与后端 dynamicModelsTTL 同一个常量，不许前端各写一份）
//	stale       age_seconds >= ttl_seconds（这份目录下次请求会被回源刷新）
type catalogFreshnessView struct {
	FetchedAt  string `json:"fetched_at"`
	AgeSeconds int64  `json:"age_seconds"`
	TTLSeconds int64  `json:"ttl_seconds"`
	Stale      bool   `json:"stale"`
}

// catalogFreshness 汇总「这份目录由哪些快照拼成」，返回其中**最旧**的一份的时刻。
//
// # 为什么取最旧而不是最新
//
// 页面上的目录是多份快照合并的（默认上游的 dynamicModelsCache + 其余各上游的
// providerModelsCache）。只要有一份过了 TTL，「这份目录是不是实时的」答案就是「不是」；
// 取最新的一份会把 90 分钟前的旧目录报成「1 秒前取的」——那比没有这个字段更坏（假新鲜）。
//
// # 为什么不知道时返回 -1 而不是 0
//
// 0 秒前 = 「刚刚取的」，是一个**断言**；不知道时说不知道才是诚实的。
// 页面对 -1 的处置见 webui-models.html 的 catalogUpdatedText。
func catalogFreshness() catalogFreshnessView {
	out := catalogFreshnessView{AgeSeconds: -1, TTLSeconds: int64(dynamicModelsTTL.Seconds())}
	oldest := time.Time{}
	consider := func(ts time.Time) {
		if ts.IsZero() {
			return
		}
		if oldest.IsZero() || ts.Before(oldest) {
			oldest = ts
		}
	}
	consider(dynamicModelsFetchedAt())
	for _, ts := range providerModelsCachedTimes() {
		consider(ts)
	}
	if oldest.IsZero() {
		return out
	}
	out.FetchedAt = oldest.Format(time.RFC3339)
	age := int64(time.Since(oldest).Seconds())
	if age < 0 {
		age = 0 // 时钟回拨：不报负龄（前端会读到「-3 秒前」这种不可能的说法）
	}
	out.AgeSeconds = age
	out.Stale = age >= out.TTLSeconds
	return out
}

// modelsUI 提供模型管理页（与 channelsUI / statusUI 逐字同构）。
func (h *Handler) modelsUI(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.uiIdentify(r); !ok {
		http.Redirect(w, r, "/ui", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-store：页面按身份分叉（未认证会被 302），缓存住会出现「登出后仍能打开」。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(modelsUIPage)
}

// modelsCatalogJSON GET /ui/models/data —— 模型管理页的取数端点。
//
// 契约（webui-models.html 按它渲染；改动必须两边一起改）：
//
//	{ "ready":bool, "source":"store"|"memory",
//	  "models":[catalogViewModel], "vendors":[catalogViewVendor],
//	  "catalog":catalogFreshnessView }
//
// ⚠ catalog 是**必需**字段（不是可选增强）：页面靠它回答"这份目录是不是实时的"。
//
//	页面原来只显示"更新于 <本地时刻>"，那与上游缓存无关（见 catalogFreshnessView）。
//
// 未认证 ⇒ 401 + JSON，**不是** 200 + 空数组：空数组在页面上是"一个模型都
// 没有"，会把"没登录"读成"目录是空的" —— 那是两类完全不同的故障。
func (h *Handler) modelsCatalogJSON(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.uiIdentify(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未认证"})
		return
	}
	models, vendors, source, ready := h.catalogView()
	catalog := catalogFreshness()
	if source == "store" {
		// ⚠ SQL 档的目录行里**没有**取数时刻（models 表只存名字/端点）。
		// 如实报「不知道」，**不许**拿内存里那两份缓存的最新时刻去冒充它 ——
		// 那会把一张"只增不改"的表显示成"刚刚取的"（假新鲜，比不显示更坏）。
		catalog = catalogFreshnessView{AgeSeconds: -1, TTLSeconds: catalog.TTLSeconds}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"ready":   ready,
		"source":  source,
		"models":  models,
		"vendors": vendors,
		"catalog": catalog,
	})
}

// catalogView 组装页面要的两张表（Model / Vendor）。
//
// 它是**纯装配**：不写库、不改状态；数据来源二选一见上面的注释。
// 返回的 vendors 由 models 聚合，两种来源共用同一段聚合 —— 否则两档之间
// 会出现"模型列表变了、供应商计数没变"的错配。
func (h *Handler) catalogView() (models []catalogViewModel, vendors []catalogViewVendor, source string, ready bool) {
	if rows, ok := h.storeCatalogRows(); ok {
		source = "store"
		models = make([]catalogViewModel, 0, len(rows))
		for _, r := range rows {
			models = append(models, catalogViewModel{
				Name:       r.Name,
				Vendor:     r.Vendor,
				Endpoints:  splitCatalogEndpoints(r.EndpointsCSV),
				Synthetic:  r.Synthetic,
				Selectable: !r.Synthetic,
			})
		}
	} else {
		source = "memory"
		base := h.upstreamCatalogEntries()
		entries := append([]store.CatalogEntry(nil), base...)
		entries = append(entries, h.channelSyntheticCatalogEntries(store.CatalogEntryNames(base))...)
		models = make([]catalogViewModel, 0, len(entries))
		for _, e := range entries {
			models = append(models, catalogViewModel{
				Name:       e.Name,
				Vendor:     e.Vendor,
				Endpoints:  splitCatalogEndpoints(strings.Join(e.Endpoints, ",")),
				Synthetic:  e.Synthetic,
				Selectable: !e.Synthetic,
			})
		}
	}
	vendors = aggregateCatalogVendors(models)
	return models, vendors, source, len(models) > 0
}

// storeCatalogRows 读 SQL 目录；未接线 / 未迁移 / 空目录都返回 ok=false。
//
// 为什么把"空目录"也当成没接线：一个建好但还没 seed 的 gw.db 与"完全没有库"
// 在页面上应当表现一样（回落实时聚合），否则 T11 刚接上、T09 seed 还没跑的那
// 一瞬间，页面会突然从"有 700 条"变成"空"—— 而那看起来就是故障。
func (h *Handler) storeCatalogRows() ([]store.CatalogRow, bool) {
	if store.DB == nil {
		return nil, false
	}
	rows, err := store.ListCatalogRows()
	if err != nil || len(rows) == 0 {
		return nil, false
	}
	return rows, true
}

// aggregateCatalogVendors 由模型行聚出供应商行（按名字升序）。
func aggregateCatalogVendors(models []catalogViewModel) []catalogViewVendor {
	type agg struct{ models, synthetic int }
	byName := make(map[string]*agg, 8)
	for _, m := range models {
		name := m.Vendor
		if name == "" {
			name = store.CatalogVendorUnknown
		}
		a := byName[name]
		if a == nil {
			a = &agg{}
			byName[name] = a
		}
		a.models++
		if m.Synthetic {
			a.synthetic++
		}
	}
	out := make([]catalogViewVendor, 0, len(byName))
	for name, a := range byName {
		out = append(out, catalogViewVendor{Name: name, Models: a.models, Synthetic: a.synthetic})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// splitCatalogEndpoints 把 CSV 端点列拆成数组（页面按数组渲染）。
//
// 空 ⇒ 返回**空数组**而不是 ["chat"]：这里只做投影，不该替数据补一个它没有的
// 事实（"chat" 的默认值在 seed 侧，见 store.normalizeCatalogEndpoints）。
func splitCatalogEndpoints(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ── T-02：供应商行的两个**写入面**动作（单供应商重拉 / 单模型探测）──────────
//
// # 为什么必须按「行」拆开（用户报障的直接修法）
//
// 在此之前唯一能做的事是"刷新整包目录"（`/ui/models/data` 那条路会逐个打上游，
// 单上游实测 0.2~23.7 s）。后果有两个，都是可观测性问题：
//
//	① 一次刷新会把 14 份快照的时间戳一起推新 ⇒「到底哪个上游变了/坏了」不可知；
//	② 一个上游失败（403/超时）会被整页的平均表现掩盖 ⇒ 排查只能靠手工脚本。
//
// 所以这里把可观测单元从「整包目录」降到「一行」。两条端点都**只**回源一个上游。
//
// # 与既有触发点的分工（别把两件事合并）
//
//	GET  /ui/models/data               只读取数（含 catalog 新鲜度），**绝不回源**
//	POST /ui/models/provider/refresh   **只**回源一个上游，覆盖它自己那份快照
//	POST /ui/models/provider/probe     发一次最小真实请求，回答"这个模型现在能不能用"
//
// # 两份缓存的事实（写错就会"页面没反应"）
//
// 目录**不是**一份缓存：
//
//	默认上游     → dynamicModelsCache（由 h.cfg.Upstream.FetchModels 填）
//	其余各上游   → providerModelsCache[id]（由 h.cfg.Provider.Models 填）
//
// 只在 providerModelsCache 上"刷新"默认上游，页面不会有任何变化 —— 因为默认上游
// 那份目录根本不读它。refreshProviderCatalog 因此按 id 分派到正确的缓存。

// errUnknownProvider 报告该 id 不是本部署认识的上游（前端传错了）。
var errUnknownProvider = errors.New("phanthywork: 未知的上游 id")

// errUpstreamNoCatalog 报告上游这次给不出目录（账号没登录 / 上游抖动）。
//
// ⚠ 与 errUnknownProvider 分开：前者是前端 bug，后者是上游现状。
// 混成一条会让"上游一下午 Optional 抖动"看起来像"你传错了 provider"。
var errUpstreamNoCatalog = errors.New("上游本次没有给出目录")

// providerActionRequest 两条行内动作的请求体（字段名就是前端契约）。
type providerActionRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// providerRefreshResponse POST /ui/models/provider/refresh 的成功体。
//
// count 是**刷新后**该上游的模型数；added/removed 是相对刷新前那份快照的差集
// （按 id 比较）。三个数一起给，用户才能回答"这次刷新是空转还是真的变了"。
type providerRefreshResponse struct {
	Provider string `json:"provider"`
	Count    int    `json:"count"`
	Added    int    `json:"added"`
	Removed  int    `json:"removed"`
	MS       int64  `json:"ms"`
	Source   string `json:"source"`
}

// decodeProviderAction 解析两条端点的公共前半段（鉴权 + 请求体 + provider 校验）。
//
// 返回 ok=false 表示**已经**写好响应，调用方直接 return。
func (h *Handler) decodeProviderAction(w http.ResponseWriter, r *http.Request, needModel bool) (providerActionRequest, bool) {
	if _, ok := h.uiIdentify(r); !ok {
		// 401 + JSON，不是 200/302：这两条是写入面，空响应会被前端读成"成功了但没变化"。
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未认证"})
		return providerActionRequest{}, false
	}
	var req providerActionRequest
	// 64 KiB 上限：请求体只有两个短字符串，放大到 1 MiB 只是给误用留空间。
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return providerActionRequest{}, false
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Model = strings.TrimSpace(req.Model)
	if req.Provider == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 provider"})
		return providerActionRequest{}, false
	}
	if needModel && req.Model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 model"})
		return providerActionRequest{}, false
	}
	return req, true
}

// modelsProviderRefresh POST /ui/models/provider/refresh —— 只回源一个上游。
//
// 失败时**保留旧快照**并把上游原因原样回给前端（非 200 + error.code）：
// 清空缓存会让页面在下一次取数时变成"这个上游没了"，那是比失败更坏的信息。
func (h *Handler) modelsProviderRefresh(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeProviderAction(w, r, false)
	if !ok {
		return
	}
	t0 := time.Now()
	before := h.providerCatalogIDs(req.Provider)
	after, err := h.refreshProviderCatalog(r.Context(), req.Provider)
	ms := time.Since(t0).Milliseconds()
	if err != nil {
		code := "upstream_error"
		status := http.StatusBadGateway
		if errors.Is(err, errUnknownProvider) {
			code = "unknown_provider"
			status = http.StatusBadRequest
		} else if errors.Is(err, errUpstreamNoCatalog) {
			code = "upstream_no_catalog"
		}
		writeJSON(w, status, map[string]any{
			"provider": req.Provider,
			"ms":       ms,
			"error":    map[string]any{"code": code, "message": err.Error()},
		})
		return
	}
	added, removed := diffCatalogIDs(before, after)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, providerRefreshResponse{
		Provider: req.Provider,
		Count:    len(after),
		Added:    added,
		Removed:  removed,
		MS:       ms,
		Source:   "upstream",
	})
}

// refreshProviderCatalog 强制回源**一个**上游，覆盖它自己那份快照，返回刷新后的 id 列表。
//
// ⚠ 绕过 1h TTL 与 stale-while-revalidate（否则"点了没反应"）：这里**直接**调上游，
// 不走 modelsForProviderCached —— 那条路在缓存未过期时会毫秒返回旧目录。
// 但**复用**它写下缓存的唯一入口（storeProviderModels / dynamicModelsCache），
// 保证下一次 /ui/models/data 读到的是同一份数据。
func (h *Handler) refreshProviderCatalog(ctx context.Context, id string) ([]string, error) {
	if id == "" {
		return nil, errUnknownProvider
	}
	isDefault := id == h.defaultProvider()
	// 单上游部署（Provider 未接线）：唯一的上游就是 cfg.Upstream，任何 id 都落到它身上
	// —— 前端那时只有一行，回 400「未知上游」会让唯一可用的按钮失效。
	known := h.cfg.Provider == nil || h.cfg.Provider.Has(id) || isDefault
	if !known {
		return nil, fmt.Errorf("%w: %s", errUnknownProvider, id)
	}
	if h.cfg.Upstream == nil && h.cfg.Provider == nil {
		return nil, fmt.Errorf("%w: 本部署没有配置任何上游", errUpstreamNoCatalog)
	}
	// 默认上游走**它自己**那份缓存，否则页面不会变（见段首"两份缓存的事实"）。
	if isDefault || h.cfg.Provider == nil {
		return h.refreshDefaultCatalog(ctx)
	}
	// ── 先试「强制回源」能力 ────────────────────────────────────────────────
	//
	// 2026-11-14：loomy 这类上游的目录是「缓存 + 后台刷新」形态 —— 上游按账号
	// 下发不同模型集，正确目录是多账号并集（要打 5 个账号），所以 Models() 只读
	// 缓存。若这里直接调 Models()，缓存没过期时按钮就是**点了没反应**。
	//
	// 未实现 gateway.ModelCatalogRefresher 的上游会返回 ok=false，
	// 继续走下面的既有路径 —— 行为完全不变。
	var infos []gateway.ModelInfo
	if rr, ok := h.cfg.Provider.(interface {
		RefreshModels(ctx context.Context, id string) ([]gateway.ModelInfo, bool)
	}); ok {
		if fresh, ok2 := rr.RefreshModels(ctx, id); ok2 && len(fresh) > 0 {
			infos = fresh
		}
	}
	if len(infos) == 0 {
		var ok bool
		infos, ok = h.cfg.Provider.Models(ctx, id)
		if !ok || len(infos) == 0 {
			return nil, fmt.Errorf("%w: %s", errUpstreamNoCatalog, id)
		}
	}
	storeProviderModels(id, infos)
	ids := make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID != "" {
			ids = append(ids, mi.ID)
		}
	}
	return ids, nil
}

// refreshDefaultCatalog 强制回源默认上游（与 fetchDynamicModels 同一判据、同一份缓存）。
//
// 刻意**复制** fetchDynamicModels 的关键几步而不抽公共函数：那条路径是 /v1/models
// 的主干（负缓存/冷却/NoteError 的语义都被测试锁着），为本次新增的按钮去改它，
// 收益是省 20 行、代价是动主干。代价不对等，所以宁可在这里重复一小段。
func (h *Handler) refreshDefaultCatalog(ctx context.Context) ([]string, error) {
	if h.cfg.Upstream == nil {
		return nil, fmt.Errorf("%w: 本部署没有默认上游客户端", errUpstreamNoCatalog)
	}
	// 与 fetchDynamicModels 同一条：选号限定"池子眼里的默认上游"（传 ""）。
	if h.cfg.Pool == nil {
		return nil, fmt.Errorf("%w: 没有账号池", errUpstreamNoCatalog)
	}
	acct := h.cfg.Pool.PickForExcluding("", nil)
	if acct == nil {
		return nil, fmt.Errorf("%w: 默认上游没有可用账号", errUpstreamNoCatalog)
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 失败要**惩罚账号**（与 fetchDynamicModels 一致），否则下次选号会再抽到它。
		h.cfg.Pool.NoteError(acct.UID)
		msg := "上游没有返回任何模型"
		if err != nil {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%w: %s", errUpstreamNoCatalog, msg)
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功即清空负缓存
	dynamicModelsCache.Unlock()
	ids := make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID != "" {
			ids = append(ids, mi.ID)
		}
	}
	return ids, nil
}

// providerCatalogIDs 读该上游**当前**快照的 id 列表（只读，绝不回源）。
//
// 用它算 added/removed：差集的真源是"刷新前页面看到的那份"，
// 而不是刷新过程中另存的一份中间态。
func (h *Handler) providerCatalogIDs(id string) []string {
	if id == h.defaultProvider() || h.cfg.Provider == nil {
		dynamicModelsCache.RLock()
		defer dynamicModelsCache.RUnlock()
		out := make([]string, 0, len(dynamicModelsCache.ids))
		for _, mi := range dynamicModelsCache.ids {
			if mi.ID != "" {
				out = append(out, mi.ID)
			}
		}
		return out
	}
	infos, ok := peekProviderModelsCached(id)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID != "" {
			out = append(out, mi.ID)
		}
	}
	return out
}

// diffCatalogIDs 返回 (added, removed)：按 id 集合比较，顺序无关。
func diffCatalogIDs(before, after []string) (added, removed int) {
	oldSet := make(map[string]bool, len(before))
	for _, id := range before {
		oldSet[id] = true
	}
	newSet := make(map[string]bool, len(after))
	for _, id := range after {
		newSet[id] = true
	}
	for id := range newSet {
		if !oldSet[id] {
			added++
		}
	}
	for id := range oldSet {
		if !newSet[id] {
			removed++
		}
	}
	return added, removed
}

// ── 单模型探测（POST /ui/models/provider/probe）────────────────────────────
//
// 探测 = 发一次**最小真实请求**（max_tokens:16、单条 "hi"），把三个事实回报给页面：
// 上游原始状态码、上游错误码原文、首包延迟。它回答的问题只有一个：
//
//	「这个模型**现在**能不能用，如果不能用，是谁在拒绝它。」
//
// ⚠ 三条刻意设计（每条都对应一次真实事故）：
//
//	① **永远回 HTTP 200**。探测是测量不是操作：上游 403 是"测到了 403"，
//	   把它变成网关的 502 会让页面显示成"探测失败"，而真正的事实（谁拒绝的）消失。
//	② **错误码原文透传**（error.code）。本次调试中 `request_not_authorized`
//	   被中间层折成"502 空消息"，排查方向因此偏了几小时。这条是硬要求。
//	③ **同目标 5 秒内去重**。上游对密集请求会进入 2–4 分钟的 403 冷却窗（实测），
//	   而页面按钮很容易被连点。去重后连点只打一次上游，第二次回放上一次结果。

const (
	// probeMaxTokens 探测请求的输出上限。16 足够让上游走完调度+首 token 路径，
	// 又不至于真的烧掉一次完整生成。
	probeMaxTokens = 16
	// probeTimeout 单次探测上限。慢上游实测 23.7 s ⇒ 30 s 是"比最慢的还长一点"。
	probeTimeout = 30 * time.Second
	// probeBodyLimit 读上游响应的上限。错误体是几百字节的 JSON，SSE 首包也在几 KiB 内。
	probeBodyLimit = 8 << 10
	// probeMessageLimit 回给页面的上游原文上限（前端 tooltip 不需要更多）。
	probeMessageLimit = 300
	// probeDedupWindow 同目标去重窗口。
	probeDedupWindow = 5 * time.Second
)

// providerProbeResponse POST /ui/models/provider/probe 的响应体（永远 HTTP 200）。
type providerProbeResponse struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	OK       bool   `json:"ok"`
	// Status 上游**原始**状态码（0 = 没走到上游，例如没号/取不到凭证）。
	Status int `json:"status"`
	// Code 上游 error.code 原文，或本层自己的失败码（no_healthy_account / no_credential / transport_error）。
	Code string `json:"code,omitempty"`
	// Message 上游原文（截断到 probeMessageLimit）。
	Message string `json:"message,omitempty"`
	// Text 成功时从 SSE 里抽到的第一段文本（可能为空：思考型模型首帧只有 reasoning）。
	Text string `json:"text,omitempty"`
	// FirstTokenMS 首包字节到达耗时（毫秒）。0 = 没收到任何字节。
	FirstTokenMS int64 `json:"firstTokenMs"`
	// TotalMS 整次探测耗时（毫秒），由 handler 层填。
	TotalMS int64 `json:"totalMs"`
}

// probeCacheEntry 一次探测的结果与它的生成时刻（用于去重窗口内的回放）。
type probeCacheEntry struct {
	at   time.Time
	resp providerProbeResponse
}

var probeCache struct {
	sync.Mutex
	entries map[string]probeCacheEntry
}

// modelsProviderProbe POST /ui/models/provider/probe。
func (h *Handler) modelsProviderProbe(w http.ResponseWriter, r *http.Request) {
	req, ok := h.decodeProviderAction(w, r, true)
	if !ok {
		return
	}
	key := req.Provider + "\x00" + req.Model
	now := time.Now()

	// ① 去重窗口内直接回放（不打上游）。
	probeCache.Lock()
	if e, hit := probeCache.entries[key]; hit && now.Sub(e.at) < probeDedupWindow {
		probeCache.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Probe-Replay", "1")
		writeJSON(w, http.StatusOK, e.resp)
		return
	}
	probeCache.Unlock()

	t0 := now
	resp := h.probeProviderModel(r.Context(), req.Provider, req.Model)
	resp.TotalMS = time.Since(t0).Milliseconds()

	// ② 只缓存**真的打到上游**的结果。
	//
	// 为什么不是无条件缓存：`no_healthy_account` 是"现在没号"，
	// 5 秒内用户可能刚在账号池页补了一个号 —— 回放那条旧失败会让他以为浏览器坏了。
	// 而上游明确回了状态码的（status > 0）是真实测量值，回放它是对的。
	if resp.Status > 0 {
		probeCache.Lock()
		if probeCache.entries == nil {
			probeCache.entries = map[string]probeCacheEntry{}
		}
		// 顺手清掉过期项，避免这个 map 无限增长（没有后台清理器，用写入路径上的机会）。
		for k, e := range probeCache.entries {
			if now.Sub(e.at) >= probeDedupWindow {
				delete(probeCache.entries, k)
			}
		}
		probeCache.entries[key] = probeCacheEntry{at: now, resp: resp}
		probeCache.Unlock()
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

// probeProviderModel 真正发一次最小请求。返回值一定带 Provider/Model，其余按实测填。
func (h *Handler) probeProviderModel(ctx context.Context, providerID, model string) providerProbeResponse {
	resp := providerProbeResponse{Provider: providerID, Model: model}

	// 上游不认网关的路由前缀（与 chatCompletions 出站前剥前缀同一条）。
	bare := model
	if pid, rest, ok := gateway.SplitModel(model); ok && (pid == providerID || providerID == "") {
		bare = rest
	}
	body, err := json.Marshal(map[string]any{
		"model":      bare,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		"max_tokens": probeMaxTokens,
		"stream":     true,
	})
	if err != nil {
		resp.Code, resp.Message = "internal_error", err.Error()
		return resp
	}

	if h.cfg.Pool == nil {
		resp.Code, resp.Message = "no_healthy_account", "本部署没有账号池"
		return resp
	}
	acct := h.cfg.Pool.PickFor(providerID, bare, nil)
	if acct == nil {
		// 渠道型模型（不是账号池里的上游）也会走到这里 —— 明确说清楚是谁，比"失败"有用。
		resp.Code = "no_healthy_account"
		resp.Message = "上游 " + providerID + " 没有可用账号（渠道型模型请到「渠道」页确认 key）"
		return resp
	}
	if !h.cfg.Pool.Acquire(acct.UID) {
		resp.Code, resp.Message = "account_busy", "账号并发已满，请稍后重试"
		return resp
	}
	defer h.cfg.Pool.Release(acct.UID)

	cred, ok := h.credentialFor(providerID, acct.UID)
	if !ok {
		resp.Code, resp.Message = "no_credential", "取不到账号 "+acct.UID+" 的凭证"
		return resp
	}

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cs, err := h.chatProvider(pctx, providerID, cred, body)
	if err != nil {
		resp.Code, resp.Message = "transport_error", truncateRunes(err.Error(), probeMessageLimit)
		return resp
	}
	defer func() { _ = cs.Body.Close() }()

	resp.Status = cs.Status
	firstMS, raw := readProbeUpstream(cs.Body, probeBodyLimit)
	resp.FirstTokenMS = firstMS
	code, msg := upstreamErrorCode(raw)
	resp.Code, resp.Message = code, truncateRunes(msg, probeMessageLimit)
	resp.OK = cs.Status >= 200 && cs.Status < 300 && code == ""
	if resp.OK {
		resp.Text = truncateRunes(extractProbeText(raw), probeMessageLimit)
	}
	return resp
}

// readProbeUpstream 读上游响应的前 limit 字节，同时测量**首字节**到达耗时。
//
// 为什么不用 io.ReadAll：探测关心的是"多久给出第一个字"，
// 全读完会把首包与后续流的时间混成一个数字（生图/长文模型能差一个数量级）。
func readProbeUpstream(r io.Reader, limit int) (firstTokenMS int64, raw []byte) {
	if r == nil {
		return 0, nil
	}
	buf := make([]byte, 512)
	start := time.Now()
	n, err := r.Read(buf)
	if n > 0 {
		firstTokenMS = time.Since(start).Milliseconds()
		raw = append(raw, buf[:n]...)
	}
	for len(raw) < limit && err == nil {
		room := limit - len(raw)
		if room > len(buf) {
			room = len(buf)
		}
		var m int
		m, err = r.Read(buf[:room])
		if m > 0 {
			raw = append(raw, buf[:m]...)
		}
	}
	if firstTokenMS == 0 && len(raw) > 0 {
		firstTokenMS = time.Since(start).Milliseconds()
	}
	return firstTokenMS, raw
}

// upstreamErrorCode 从上游响应原文里抽 (code, message)。
//
// 认三种形状（实测过的都在这儿，不猜第四种）：
//
//	{"error":{"code":"...","message":"..."}}   标准 OpenAI/Anthropic 错误体
//	{"error":"something went wrong"}            只给字符串的老上游
//	data: {"error":{...}}                       错误被塞进 SSE 帧里（少数上游 200 + 错误帧）
func upstreamErrorCode(raw []byte) (code, message string) {
	if len(raw) == 0 {
		return "", ""
	}
	if c, m, ok := parseErrorObject(raw); ok {
		return c, m
	}
	// SSE：逐行找 data: 之后的对象。
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if c, m, ok := parseErrorObject([]byte(payload)); ok {
			return c, m
		}
	}
	return "", ""
}

// parseErrorObject 解一个 {"error":...} 形状的 JSON。
func parseErrorObject(raw []byte) (code, message string, ok bool) {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Error) == 0 {
		return "", "", false
	}
	var obj struct {
		Code    string `json:"code"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(probe.Error, &obj); err == nil {
		code = strings.TrimSpace(obj.Code)
		if code == "" {
			// 有些上游只给 type（Anthropic 的 error.type），它同样是"谁拒绝的"的判据。
			code = strings.TrimSpace(obj.Type)
		}
		if code != "" || obj.Message != "" {
			return code, obj.Message, true
		}
	}
	var text string
	if err := json.Unmarshal(probe.Error, &text); err == nil && text != "" {
		return "", text, true
	}
	return "", "", false
}

// extractProbeText 从成功响应里抽第一段可见文本（SSE 或一次性 JSON 都认）。
//
// 抽不到就返回空串 —— 思考型模型的首帧常常只有 reasoning_content，
// 那时"没有可见文本"是**真实结果**，不该编一个占位串。
func extractProbeText(raw []byte) string {
	tryObject := func(b []byte) string {
		var obj struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(b, &obj); err != nil {
			return ""
		}
		for _, c := range obj.Choices {
			if c.Delta.Content != "" {
				return c.Delta.Content
			}
			if c.Message.Content != "" {
				return c.Message.Content
			}
		}
		for _, c := range obj.Content {
			if c.Text != "" {
				return c.Text
			}
		}
		return ""
	}
	if s := tryObject(raw); s != "" {
		return s
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if s := tryObject([]byte(payload)); s != "" {
			return s
		}
	}
	return ""
}

// truncateRunes 按**字符**截断（不是字节）：上游中文错误文案按字节切会切出半个字。
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

