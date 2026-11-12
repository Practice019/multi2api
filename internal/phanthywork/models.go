// models.go PhanthyWork 的模型目录：远端优先、**永不返回空**。
//
// # 为什么必须有一个内嵌兜底表（这不是"多一层保险"）
//
// 两条硬约束同时成立：
//
//	① gateway 的契约测试**用伪造凭证跑**：Models() 在"凭证无效"时若
//	   `return nil, err`，会被判成「声明了 CapModels 却没实现」
//	   （见 internal/gateway/contract.go 的 verifyModelsNonEmpty）
//	② 报告 §5 的目录降级规则：整个接口失败 → 退回静态表 / 上次快照，
//	   **不返回空列表**
//
// 所以"无凭证 → 返回兜底表"既是契约要求，也是上游自己的降级规则。
//
// # ⚠ 模型 id **不带 phanthy/ 前缀**（坑③）
//
// 这是本报告里最容易踩的一处：
//
//	桌面端本地网关 127.0.0.1:14665/api/models   给的是 phanthy/Phanthy-fast
//	云端目录 code.phanthy.com/api/phanthy_code/models  给的是 Phanthy-fast
//
// 实测对照：`phanthy/Phanthy-fast` → **403 model_not_allowed**；
// `Phanthy-fast` → **200**。打云端就用云端那份的 id。
//
// 本包**只**产出不带前缀的 id —— "phanthywork/" 这个前缀由核心按
// gateway.SplitModel 拼装（"phanthywork/Phanthy-fast" 拆成 providerID 与裸 id）。
package phanthywork

import (
	"context"
	"log"
	"sort"
	"strings"

	"workbuddy2api/internal/gateway"
)

// modelsSchemaVersion 云端目录的 schema_version（不是 1 就当失败，**不许猜字段**）。
const modelsSchemaVersion = 1

// remoteModel 云端模型目录里本包读的一个条目。
//
// 只声明用得上的字段：报告 §5 诚实说明过"逐条 context_window / pricing
// 没有落盘成文件"，所以**不编造**没采集到的数字。
type remoteModel struct {
	ID string `json:"id"`
	// Alias / Label 服务端给的展示名（缺省时用 id）。
	Alias string `json:"alias"`
	Label string `json:"label"`
	// ContextWindowTokens 上下文窗口（服务端字段名）。
	ContextWindowTokens int `json:"context_window_tokens"`
	// Recommended 是否推荐（只用于排序，不影响能力）。
	Recommended bool `json:"recommended"`
}

// catalogEntry 兜底表的一条。
type catalogEntry struct {
	// ID 上游认的模型名（**不带前缀**）。
	ID string
	// Name 展示名。
	Name string
	// ContextWindow 上下文窗口（token）。0 = 未采集。
	ContextWindow int
	// MaxTokens 单次输出上限（token）。0 = 未采集。
	MaxTokens int
	// Free 是否无条件免费。
	Free bool
	// Vendor 厂商（报告 §5 的分档列，仅用于展示与排查）。
	Vendor string
}

// fallbackCatalog 内嵌目录：报告 §5 的 14 条全表（id 逐字照抄）。
//
// ⚠ 关于两张"缺格子"的诚实说明（照抄报告 §5 的原话口径）：
//
//	报告里 context_window / pricing **没有逐条落盘**，"❓未采集"= 没保存输出，
//	不是"服务端没这个字段"。所以本表**只填实测过的那一条**（Phanthy-fast
//	的 262,144，来自 /api/models/current 实测），其余留 0（= 未知）。
//	编一个数字会让"未知"变成"看起来确定但错的" —— 那比 0 更糟。
//
// 唯一无条件免费的是 Phanthy-fast（📖源码 README + ✅实测 cost_points: 0）。
var fallbackCatalog = []catalogEntry{
	{ID: "deepseek-v4.1-flash-1017", Name: "DeepSeek V4.1 Flash", Vendor: "DeepSeek"},
	{ID: "glm-5.2", Name: "GLM 5.2", Vendor: "智谱"},
	{ID: "glm-5.3", Name: "GLM 5.3", Vendor: "智谱"},
	{ID: "glm-5.3-flash", Name: "GLM 5.3 Flash", Vendor: "智谱"},
	{ID: "kimi-k2.7-code", Name: "Kimi K2.7 Code", Vendor: "月之暗面"},
	{ID: "kimi-k3", Name: "Kimi K3", Vendor: "月之暗面"},
	{ID: "Phana-5.2f", Name: "Phana 5.2f", Vendor: "Phana"},
	{ID: "Phana-5.5o", Name: "Phana 5.5o", Vendor: "Phana"},
	{ID: "Phana-5.5s", Name: "Phana 5.5s", Vendor: "Phana"},
	{ID: "Phano-6.1s", Name: "Phano 6.1s", Vendor: "Phano"},
	{ID: "Phano-6a", Name: "Phano 6a", Vendor: "Phano"},
	{
		// ★ 唯一无条件免费 + 唯一有实测上下文窗口的模型。
		ID: "Phanthy-fast", Name: "Phanthy Fast", Vendor: "Phanthy",
		ContextWindow: 262_144, Free: true,
	},
	{ID: "Phanthy-pro", Name: "Phanthy Pro", Vendor: "Phanthy"},
	{ID: "Phanthy-ultra", Name: "Phanthy Ultra", Vendor: "Phanthy"},
}

// DefaultModelID 默认模型（云端目录的 default_model，实测 `Phanthy-fast`）。
//
// 它同时是"兜底表里唯一确定免费"的那个，所以当默认值是合理的。
const DefaultModelID = "Phanthy-fast"

// fallbackByID 兜底表的 id 索引。
var fallbackByID = func() map[string]catalogEntry {
	m := make(map[string]catalogEntry, len(fallbackCatalog))
	for _, e := range fallbackCatalog {
		m[e.ID] = e
	}
	return m
}()

// Catalog 返回本包的静态目录快照（导出给装配层/测试遍历用）。
func Catalog() []catalogEntry {
	out := make([]catalogEntry, len(fallbackCatalog))
	copy(out, fallbackCatalog)
	return out
}

// Models 返回模型目录：远端优先，失败回兜底，**任何情况下都不为空**。
//
// 无凭证时**不打远端**（那必然 401，只会白产生一条 warning）。
func (p *Provider) Models(ctx context.Context, cred gateway.Credential) ([]gateway.ModelInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// ⚠ 用 `_` 吞掉 Secret 错误：无有效凭证也必须能返回目录
	//（契约测试默认拿伪造凭证跑，一上来 return nil, err 会被判
	// 「声明了 CapModels 却没实现」）。
	a, _ := authOf(cred)

	var remote []remoteModel
	if a != nil && a.OAuth.AccessToken != "" {
		ms, err := p.client.FetchModels(ctx, a)
		if err != nil {
			// 远端失败**不是**本方法的失败：退回兜底表。
			// 但必须让它可见（否则"目录服务一直挂着"没人知道）。
			logRemoteModelsFailed(err)
		} else {
			remote = ms
		}
	}
	out := mergeCatalog(remote)
	if len(out) == 0 {
		// 理论上不可达（兜底表非空），留一道防线：空列表会被契约判失败。
		return []gateway.ModelInfo{{ID: DefaultModelID}}, nil
	}
	return out, nil
}

// mergeCatalog 合并远端目录与兜底表（**顺序即列表展示顺序**）。
//
// 顺序：
//
//	1 兜底表的顺序（保证离线与线上一致的可预期顺序，且免费模型不会沉底）
//	2 远端有、兜底表没有的（保持可发现性），按 id 排序保证稳定
//
// 去重按裸 id（**首见优先**）。元数据（context window）取"两边都有的那份
// 里最有信息的那个"：远端给了就用远端（服务端是权威），否则用兜底表。
func mergeCatalog(remote []remoteModel) []gateway.ModelInfo {
	remoteByID := make(map[string]remoteModel, len(remote))
	for _, m := range remote {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue // 报告 §5 的降级规则：单条缺 id → 丢掉这条
		}
		if _, dup := remoteByID[id]; !dup {
			remoteByID[id] = m
		}
	}

	out := make([]gateway.ModelInfo, 0, len(fallbackCatalog)+len(remoteByID))
	seen := map[string]bool{}
	// ① 兜底表顺序
	for _, e := range fallbackCatalog {
		seen[e.ID] = true
		info := gateway.ModelInfo{ID: e.ID, ContextWindow: e.ContextWindow, MaxOutputTokens: e.MaxTokens}
		if rm, ok := remoteByID[e.ID]; ok && rm.ContextWindowTokens > 0 {
			info.ContextWindow = rm.ContextWindowTokens
		}
		out = append(out, info)
	}
	// ② 远端新增的（按 id 排序，保证稳定）
	var extra []string
	for id := range remoteByID {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		rm := remoteByID[id]
		out = append(out, gateway.ModelInfo{
			ID:            id,
			ContextWindow: rm.ContextWindowTokens,
		})
	}
	return out
}

// stripProviderPrefix 把 `phanthywork/<id>` 拆成裸 id（坑③ 的落地）。
//
// 判据：用 gateway.SplitModel 拆**第一个** '/'（模型名自身可含 '/'），
// 只在自己前缀命中时剥离；不带前缀的原样返回。
//
// ⚠ 这一条是本地最贵的一处坑：带前缀发出去，服务端回
// **403 model_not_allowed**，而那句文案看起来像"套餐不含这个模型"。
func stripProviderPrefix(model string) string {
	id := strings.TrimSpace(model)
	pid, rest, has := gateway.SplitModel(id)
	if has && pid == providerID {
		return rest
	}
	return id
}

// catalogHasModel 报告裸 id 是否在已知目录里（含远端新增）。
func catalogHasModel(id string) bool {
	_, ok := fallbackByID[id]
	return ok
}

// logRemoteModelsFailed 记录一次远端目录失败。
//
// 用 log 而不是静默：报告 §5 的降级规则允许失败，但"一直在降级"
// 必须能被看见（否则目录服务挂一个月都没人知道）。
func logRemoteModelsFailed(err error) {
	if err == nil {
		return
	}
	log.Printf("phanthywork: 远端模型目录不可用，已回落到内嵌兜底表: %v", err)
}
