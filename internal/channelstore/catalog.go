// catalog.go 统一能力目录 —— 公开名 → 真实模型 的**可校验版本化视图**。
//
// # 这个文件为什么存在（U-04）
//
// 改造前 `/v1/models` 直接读渠道配置的库存，把"渠道声明了什么"当成
// "当前能提供什么"。两者在三种情况下会分叉：
//
//	· 刷新失败 —— 库存还是旧值，用户看到的是过期能力
//	· 映射有歧义 —— 同一个公开名指向两个不同真实模型，合并后**静默**路由到错的
//	· 映射写到不存在的真实模型 —— 运维以为发布成功，实际什么都没发出去
//
// 本文件把目录算成一份**带版本号的快照**（Version = 内容哈希），并**显式报告**
// 拒绝发布的原因。获取与读取的分离在 catalog_snapshot.go。
//
// # 三条判据（对应计划 §5 U-04 的验收）
//
//	判据 1  同名多渠道**显式**聚合：多个渠道把同一公开名映射到同一/多个真实模型，
//	         只要都是显式映射（ModelMap 里写了），就聚合 —— 那是运维的意图表达。
//	判据 2  隐式撞名**拒绝发布**：某个公开名既是 A 渠道的映射目标，又在 B 渠道里
//	         作为**真实模型名**被声明 —— 那两个是不同模型，合并会把请求路由错。
//	         该公开名从 Models 里剔除，原因记进 Conflicts。
//	判据 3  无效映射**记录不静默**：ModelMap 指向未声明的真实模型时，既不发布也不
//	         假装成功 —— 记进 Conflicts，让运维看得见。
//
// 判据 1 与 2 的区别只在一件事：**这个公开名有没有"以自己为真实模型"的供给**。
package channelstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// internalEndpoints 目录计算时遍历的**内部**端点类别。
//
// ⚠ 这三个值只在包内与 `SupportsEndpoint` / 路由共用，**不出现在任何对外响应里**。
// 对外一律走 PublicEndpoint 映射（计划 U-04 验收要求 `image_generation` 保留）。
func internalEndpoints() []string { return []string{"chat", "images"} }

// PublicEndpoint 把内部端点类别映射成**对外名字**。
//
// # 为什么必须分开
//
//	内部（SupportsEndpoint / 路由 / 渠道能力表）  "images"
//	对外（/v1/models 的 capabilities、本目录）    "image_generation"
//
// 现有 `/v1/models` 就是这么发的（见 handler.go 的
// `out[at]["capabilities"] = []string{"image_generation"}`）。
// 统一目录如果照抄内部名发 "images"，同一个网关会对同一个能力给出两个名字 ——
// 客户端按名字匹配能力，两边不一致就会出现"列表里有、调不了"。
//
// ⚠ 改内部名会动路由；改对外只是措辞。所以只映射这一侧。
func PublicEndpoint(kind string) string {
	if kind == "images" {
		return "image_generation"
	}
	return kind
}

// CatalogProvider 一个公开模型背后的一条供给。
type CatalogProvider struct {
	ChannelID  string   `json:"channel_id"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	ProviderID string   `json:"provider_id,omitempty"`
	RealModel  string   `json:"real_model"`
	Endpoints  []string `json:"endpoints"`
	Enabled    bool     `json:"enabled"`
	// Groups 这条渠道所属的分组（排序后）。
	//
	// # 为什么它是"来源映射"缺的那一格
	//
	// U-09 的验收要求「provider / channel / account 来源映射」。前三者里
	// provider 与 channel 已经在了（ProviderID / ChannelID），**group 缺**。
	//
	// 而 group 恰恰是 v2 的**可见性边界**：`ResolvePolicy` 用令牌的分组取渠道候选，
	// `EnabledModels(group)` 按分组过滤能力。所以运维问"这个公开模型谁能用"
	// 时，答案是「ch-a 能服务它，而 ch-a 属于 g-route 与 g-beta」——
	// 没有 Groups 这一格，那句话就说不完整。
	//
	// ⚠ 它**不是**"谁能用这个模型"的结论，只是渠道的归属。真正的可见性
	// 还要看令牌的分组（`ResolvePolicy`）—— 两件事不能混。
	Groups []string `json:"groups,omitempty"`
}

// PublicCatalogModel 一个公开模型及其全部供给。
type PublicCatalogModel struct {
	ID        string            `json:"id"`
	Endpoints []string          `json:"endpoints"`
	Providers []CatalogProvider `json:"providers"`
}

// CatalogConflict 拒绝发布的原因（判据 2 / 判据 3）。
//
// ⚠ 它**不是**警告：出现在这里的公开名一律不进 Models —— 宁可少一个模型，
// 也不把请求路由到一个不确定的真实模型上。
type CatalogConflict struct {
	Public   string   `json:"public"`
	Reason   string   `json:"reason"`
	Reals    []string `json:"reals,omitempty"`
	Channels []string `json:"channels,omitempty"`
}

// CatalogConflict 的 reason 取值。
const (
	// ConflictImplicitCollision 判据 2：公开名既被显式映射，又作为真实模型被声明。
	ConflictImplicitCollision = "implicit_collision"
	// ConflictMappingUndeclared 判据 3：ModelMap 指向未声明的真实模型。
	ConflictMappingUndeclared = "mapping_target_undeclared"
	// ConflictAliasAmbiguous 判据 4（U-09 别名兼容）：同一个名字**一语两解** ——
	// 它既是对外别名（某个 ModelMap 的键），又是另一个对外名背后的真实模型（某个
	// ModelMap 的值）。于是"它到底指谁"有两个答案，而现有解析链静默选了其中一种。
	ConflictAliasAmbiguous = "alias_ambiguous"
)

// UnifiedCatalog 一次目录快照。
type UnifiedCatalog struct {
	Version string               `json:"snapshot_version"`
	ReadAt  string               `json:"read_at"`
	Source  string               `json:"source"`
	Models  []PublicCatalogModel `json:"models"`
	// Conflicts 本轮被拒绝发布的公开名（判据 2 / 3）。
	// 空数组而非 nil —— 前端与判据都要能直接判空而不怕 null。
	Conflicts []CatalogConflict `json:"conflicts"`
}

// publicName 一条渠道内解析出的一项供给。
type publicName struct {
	public   string
	real     string
	explicit bool // true = 来自 ModelMap（运维显式写的）；false = 该模型以自己为公开名
}

// resolvePublicNames 解析**单条渠道**的 公开名 → 真实名。
//
// 返回的 conflicts 只覆盖**渠道内**的无效映射（判据 3）—— 跨渠道撞名（判据 2）
// 必须由 BuildCatalog 在全局视角下判定，单条渠道看不到别的渠道。
func resolvePublicNames(ch *Channel) ([]publicName, []CatalogConflict) {
	if ch == nil {
		return nil, nil
	}
	declared := make(map[string]bool, len(ch.Models))
	for _, model := range ch.Models {
		if model != "" {
			declared[model] = true
		}
	}

	out := make([]publicName, 0, len(ch.Models)+len(ch.ModelMap))
	var conflicts []CatalogConflict
	mapped := make(map[string]bool, len(ch.ModelMap))

	// ① 显式映射优先：ModelMap[公开名] = 真实名
	//
	// ⚠ 必须排序遍历：map 迭代顺序随机，不排序 Version 哈希会随每次调用变化，
	// 于是"内容没变但版本变了"，快照比较立刻失效。
	for _, public := range sortedKeys(ch.ModelMap) {
		real := ch.ModelMap[public]
		switch {
		case public == "" || real == "":
			// 空串是脏数据，没有语义 —— 既不算映射也不必报。
			continue
		case !declared[real]:
			// 判据 3：指向未声明的真实模型。
			conflicts = append(conflicts, CatalogConflict{
				Public:   public,
				Reason:   ConflictMappingUndeclared,
				Reals:    []string{real},
				Channels: []string{ch.ID},
			})
			continue
		}
		out = append(out, publicName{public: public, real: real, explicit: true})
		mapped[real] = true
	}

	// ② 未被映射覆盖的声明模型：公开名 = 真实名（恒等）。
	//
	// 这是"没配映射"的正常形态。它对判据 2 极其关键 ——
	// 有没有这一条，决定了这个公开名是否"以自己为真实模型"被供给。
	for _, model := range ch.Models {
		if model == "" || mapped[model] {
			continue
		}
		out = append(out, publicName{public: model, real: model, explicit: false})
	}
	return out, conflicts
}

// PublicNames 返回**单条渠道**的 公开名 → 真实名。
//
// 无效映射（判据 3）在这里被略过 —— 需要原因的一方用 resolvePublicNames。
// 这个签名被 routing_policy.go 与既有判据使用，保持不变。
func PublicNames(ch *Channel) map[string]string {
	names, _ := resolvePublicNames(ch)
	result := make(map[string]string, len(names))
	for _, n := range names {
		result[n.public] = n.real
	}
	return result
}

// aliasAmbiguities 找出**一语两解**的名字（U-09 别名兼容段）。
//
// # 什么是一语两解
//
//	渠道 A：ModelMap{"pub": "real"}      ⇒ real 是 pub 背后的真实模型
//	渠道 B：ModelMap{"real": "real2"}    ⇒ real 自己又是一个对外别名
//
// 这时客户端请求 `real` 有两个可能的答案：
//
//	① 当别名解 ⇒ real2（现有解析链选这个：名字是键就原样用）
//	② 当真实模型解 ⇒ 渠道 A 的 real
//
// 而 `/v1/models` 会把 `pub` 与 `real` 都列出来，用户复制哪个就照着用哪个 ——
// 于是"列表里看到的名字"与"它实际打到的模型"对不上。
//
// # 为什么必须报出来而不是选一个
//
// 计划 U-09 的验收补充写着「冲突**不得任选**」。选一种解会让另一半配置静默失效，
// 而失效的那一半正是运维以为生效的那一半。
func aliasAmbiguities(channels []*Channel) []CatalogConflict {
	type where struct {
		asAlias  []string // 它作为对外别名的渠道
		asTarget []string // 它作为真实模型（被别名指向）的渠道
	}
	seen := map[string]*where{}
	get := func(name string) *where {
		if seen[name] == nil {
			seen[name] = &where{}
		}
		return seen[name]
	}
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		for public, real := range ch.ModelMap {
			if public == "" || real == "" {
				continue
			}
			// ⚠ 映射到**自己**不算歧义：两种解指向同一个目标。
			// 不带这一条的话，`{"pub":"pub"}` 会被误报成"一语两解" ——
			// 而那是运维显式声明"这个名字就是它自己"的正常写法。
			if public == real {
				continue
			}
			get(public).asAlias = append(get(public).asAlias, ch.ID)
			get(real).asTarget = append(get(real).asTarget, ch.ID)
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)

	out := []CatalogConflict{}
	for _, name := range names {
		w := seen[name]
		if len(w.asAlias) == 0 || len(w.asTarget) == 0 {
			continue // 只有一种解，不是歧义
		}
		channelsInvolved := map[string]bool{}
		for _, id := range w.asAlias {
			channelsInvolved[id] = true
		}
		for _, id := range w.asTarget {
			channelsInvolved[id] = true
		}
		out = append(out, CatalogConflict{
			Public:   name,
			Reason:   ConflictAliasAmbiguous,
			Reals:    []string{name}, // 作为别名它指向别的名字；具体目标已在注释里说明
			Channels: sortedKeys(channelsInvolved),
		})
	}
	return out
}

// BuildCatalog 把渠道配置算成一份带版本号的目录快照。
//
// 纯函数：同样的输入必得同样的 Version；不做 IO；渠道的 Keys **一个字节都不进结果**
// （有判据守着 —— 目录泄漏凭据是这条链路上最严重的错）。
func BuildCatalog(channels []*Channel) UnifiedCatalog {
	result := UnifiedCatalog{
		Models:    []PublicCatalogModel{},
		Conflicts: []CatalogConflict{},
		Source:    "channel_configuration",
		ReadAt:    time.Now().UTC().Format(time.RFC3339Nano),
	}

	byModel := map[string]*PublicCatalogModel{}
	// 判据 2 需要全局视角：分别记录"显式映射到哪些真实模型"与"以自己为真实模型"。
	explicitReals := map[string]map[string]bool{}
	identityReals := map[string]map[string]bool{}
	channelsOf := map[string]map[string]bool{}
	mark := func(m map[string]map[string]bool, public, real string) {
		if m[public] == nil {
			m[public] = map[string]bool{}
		}
		m[public][real] = true
	}

	for _, ch := range channels {
		if ch == nil {
			continue
		}
		names, conflicts := resolvePublicNames(ch)
		result.Conflicts = append(result.Conflicts, conflicts...)
		for _, n := range names {
			if n.explicit {
				mark(explicitReals, n.public, n.real)
			} else {
				mark(identityReals, n.public, n.real)
			}
			if channelsOf[n.public] == nil {
				channelsOf[n.public] = map[string]bool{}
			}
			channelsOf[n.public][ch.ID] = true

			model := byModel[n.public]
			if model == nil {
				model = &PublicCatalogModel{ID: n.public, Endpoints: []string{}, Providers: []CatalogProvider{}}
				byModel[n.public] = model
			}
			endpoints := []string{}
			for _, endpoint := range internalEndpoints() {
				if ch.SupportsEndpoint(n.real, endpoint) {
					// 存内部名用于聚合去重，输出前再映射 —— 见下面的 publicEndpoints。
					endpoints = append(endpoints, endpoint)
				}
			}
			model.Providers = append(model.Providers, CatalogProvider{
				ChannelID:  ch.ID,
				Name:       ch.Name,
				Kind:       ch.EffectiveKind(),
				ProviderID: ch.ProviderID,
				RealModel:  n.real,
				Endpoints:  endpoints,
				Enabled:    ch.Status == StatusEnabled,
				// 分组归属：v2 的可见性边界。排序保证 Version 稳定。
				Groups: sortedCopy(ch.Groups),
			})
			for _, endpoint := range endpoints {
				if !containsString(model.Endpoints, endpoint) {
					model.Endpoints = append(model.Endpoints, endpoint)
				}
			}
		}
	}

	// 判据 2：同一公开名既有"显式映射"又有"恒等声明"，且真实模型不止一个 ⇒ 歧义。
	for _, public := range sortedKeys(byModel) {
		exp := explicitReals[public]
		ident := identityReals[public]
		if len(exp) == 0 || len(ident) == 0 {
			continue // 判据 1：全是显式、或全是恒等 ⇒ 运维意图明确，不算歧义
		}
		reals := map[string]bool{}
		for r := range exp {
			reals[r] = true
		}
		for r := range ident {
			reals[r] = true
		}
		if len(reals) < 2 {
			continue // 显式映射到的正是它自己的名字 ⇒ 指向同一个模型，无害
		}
		result.Conflicts = append(result.Conflicts, CatalogConflict{
			Public:   public,
			Reason:   ConflictImplicitCollision,
			Reals:    sortedKeys(reals),
			Channels: sortedKeys(channelsOf[public]),
		})
		delete(byModel, public) // 拒绝发布
	}

	for _, public := range sortedKeys(byModel) {
		model := byModel[public]
		// 每条供给与整个模型的端点都换成对外名（"images" → "image_generation"）。
		for i := range model.Providers {
			model.Providers[i].Endpoints = publicEndpoints(model.Providers[i].Endpoints)
		}
		model.Endpoints = publicEndpoints(model.Endpoints)
		sort.Strings(model.Endpoints)
		sort.Slice(model.Providers, func(i, j int) bool {
			if model.Providers[i].ChannelID != model.Providers[j].ChannelID {
				return model.Providers[i].ChannelID < model.Providers[j].ChannelID
			}
			return model.Providers[i].RealModel < model.Providers[j].RealModel
		})
		result.Models = append(result.Models, *model)
	}

	// 判据 4：别名歧义。它不影响 Models 的聚合（那些名字仍在），
	// 但必须出现在 Conflicts 里 —— 否则运营看不到"有两个解"这件事。
	result.Conflicts = append(result.Conflicts, aliasAmbiguities(channels)...)

	// 冲突也排序：Version 必须只由内容决定。
	sort.Slice(result.Conflicts, func(i, j int) bool {
		if result.Conflicts[i].Public != result.Conflicts[j].Public {
			return result.Conflicts[i].Public < result.Conflicts[j].Public
		}
		return result.Conflicts[i].Reason < result.Conflicts[j].Reason
	})

	raw, _ := json.Marshal(struct {
		Models    []PublicCatalogModel `json:"models"`
		Conflicts []CatalogConflict    `json:"conflicts"`
	}{result.Models, result.Conflicts})
	digest := sha256.Sum256(raw)
	result.Version = hex.EncodeToString(digest[:])
	return result
}

// publicEndpoints 去重 + 映射成对外名，保持排序。
func publicEndpoints(internal []string) []string {
	seen := make(map[string]bool, len(internal))
	out := make([]string, 0, len(internal))
	for _, kind := range internal {
		name := PublicEndpoint(kind)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sortedCopy 复制并排序（nil 安全，返回空切片而不是 nil —— JSON 里是 [] 不是 null）。
//
// ⚠ 必须排序：map/切片顺序不稳定会让 Version 哈希随每次调用变化，
// 于是"内容没变但版本变了"，快照比较立刻失效（本文件头就有这条纪律）。
func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// sortedKeys 排序后的键。泛型是为了让 map[string]bool / map[string]string /
// map[string][]string 共用一份实现，不必写三个同名函数。
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
