// models.go Cline 的模型目录：合并两个远端来源 + 内嵌兜底表。
//
// # 为什么必须打**两个**端点（不是冗余）
//
//	GET /api/v1/ai/cline/recommended-models → {recommended[], free[], clinePass[]}
//	                                          唯一权威的**免费集合**，不需要认证
//	GET /api/v1/models                       → 460 个裸 {id,...}，需要认证
//	                                          **其中 cline-free/* 零命中**
//
// 实测（参照 AGENTS.md:1704-1705）：460 个 id 里 `cline-free/` 零命中。
// 也就是说**只调 /models 会看不到任何免费模型** —— 这正是必须两个都打的理由，
// 而不是"多打一个更保险"。
//
// 反向也成立：内嵌兜底表缺 `cline-free/gemini-3.8-flash`（远端 free 有），
// 而 /models 有大量兜底表没有的付费模型。所以正确做法是**合并**，
// 而不是"远端失败就用兜底"的简单回退。
//
// # 两个端点独立容错
//
// 任一端点挂掉只记 warning，由合并逻辑用兜底表补齐 ——
// "目录服务抖动"不该让用户的模型列表整个消失。
package cline

import (
	"context"
	"log"
	"sort"
	"strings"
)

// Model 目录里的一个模型。
type Model struct {
	// ID 上游认的模型名（如 `cline-free/deepseek-v4.1-flash`）。
	ID string
	// Name 展示名。免费模型会被加上 ` · 免费` 后缀（见 displayName）。
	Name string
	// Description 描述。⚠ 只用于 /model 弹窗，**不要**把免费标记放这里。
	Description string
	// ContextWindow 上下文窗口（token）。0 = 未知。
	ContextWindow int
	// MaxTokens 单次输出上限（token）。0 = 未知。
	MaxTokens int
	// SupportsImage 是否接受图片输入。
	SupportsImage bool
	// IsFree 免费模型（服务端随时可撤销的营销状态）。
	IsFree bool
}

// 免费判定常量（参照 cline-models.ts:75-77）。
const (
	freeIDSuffix = ":free"
	freeIDPrefix = "cline-free/"
)

// fallbackModels 内嵌兜底表（参照 cline-product.ts:134-192，逐条照抄）。
//
// ⚠ 本表只是兜底：真实免费集合由远端 `free` 数组下发，且**免费资格是服务端
// 随时可撤销的营销状态**，故不在代码里硬编码"哪些名字免费"的模糊匹配。
// 本表的作用是"离线/远端失败时目录不至于是空的"。
//
// ⚠ 第 4 条 `cline-free/gemini-3.8-flash` 的 MaxTokens **必须是 65536**，
// 不要照抄其它免费模型的 131072 —— 那是真实缺陷（用户报障 2026-09-25）：
// 给该模型发 max_tokens=131072 会被上游 vertex provider 以 400 拒绝
// （"supported range is from 1 (inclusive) to 65537 (exclusive)"，上限即 65536）。
var fallbackModels = []Model{
	{
		ID: "stealth/space-bunny-alpha", Name: "Space Bunny Alpha",
		ContextWindow: 1_000_000, MaxTokens: 524_288, SupportsImage: true, IsFree: true,
		Description: "Blazing-fast inference with 1M context",
	},
	{
		ID: "cline-free/mimo-v2.6-flash", Name: "MiMo-V2.6-Flash",
		ContextWindow: 1_048_576, MaxTokens: 131_072, SupportsImage: true, IsFree: true,
		Description: "Mixture-of-Experts architecture with 309B total parameters",
	},
	{
		ID: "cline-free/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash",
		ContextWindow: 1_048_576, MaxTokens: 131_072, SupportsImage: true, IsFree: true,
		Description: "Fast and efficient with 1M context window",
	},
	{
		// ⚠ maxTokens=65536（不是 131072）—— 见上方注释的真实缺陷
		ID: "cline-free/gemini-3.8-flash", Name: "Gemini 3.8 Flash",
		ContextWindow: 1_048_576, MaxTokens: 65_536, SupportsImage: true, IsFree: true,
		Description: "Google's most intelligent Flash model",
	},
	{
		ID: "cline-free/muse-spark-1.3-contributor", Name: "Muse Spark 1.3 Contributor",
		ContextWindow: 1_048_576, MaxTokens: 943_718, SupportsImage: true, IsFree: true,
		Description: "Meta's multimodal reasoning model for experimentation, learning, " +
			"and early-stage agentic, multi-agent, and coding workflows.",
	},
}

// fallbackByID 兜底表的 id 索引。
var fallbackByID = func() map[string]Model {
	m := make(map[string]Model, len(fallbackModels))
	for _, f := range fallbackModels {
		m[f.ID] = f
	}
	return m
}()

// isFreeModel 免费判定（参照 cline-models.ts:85-94）。
//
//	isFree = 远端 free 集合命中
//	      || id 以 :free 结尾
//	      || id 以 cline-free/ 开头
//	      || 兜底表标记 IsFree
//
// ⚠ 用**后缀**而非 `strings.Contains(id, ":free")`：像 `foo:freebar` 这样的 id
// 含 ":free" 子串却并不免费，后缀判定不会误伤。
//
// ⚠ 免费模型是**独立 id**：`cline-free/deepseek-v4.1-flash`（免费）与
// `deepseek/deepseek-v4.1-flash`（按量计费）是两个不同条目。
// 绝不可用"名字包含 deepseek"之类的模糊匹配 —— 那会把付费条目误标为免费，
// 用户按免费预期使用却被计费。
func isFreeModel(id string, remoteFree map[string]bool) bool {
	if remoteFree[id] {
		return true
	}
	if strings.HasSuffix(id, freeIDSuffix) || strings.HasPrefix(id, freeIDPrefix) {
		return true
	}
	if f, ok := fallbackByID[id]; ok && f.IsFree {
		return true
	}
	return false
}

// nameFromID 由 id 派生兜底展示名（参照 cline-models.ts:112-118）。
//
//	deepseek/deepseek-v4.1-flash → "Deepseek V4.1 Flash"
func nameFromID(id string) string {
	tail := id
	if i := strings.Index(id, "/"); i >= 0 {
		tail = id[i+1:]
	}
	tail = strings.ReplaceAll(tail, freeIDSuffix, "")
	tail = strings.ReplaceAll(tail, "-", " ")
	// 每个单词首字母大写
	var b strings.Builder
	upNext := true
	for _, r := range tail {
		if r == ' ' {
			b.WriteRune(r)
			upNext = true
			continue
		}
		if upNext && r >= 'a' && r <= 'z' {
			b.WriteRune(r - 32)
		} else {
			b.WriteRune(r)
		}
		upNext = false
	}
	return b.String()
}

// displayName 返回列表里显示的模型名。
//
// ⚠ 免费标记**必须写进 Name**，不是 Description：
// 模型切换菜单只渲染 name，完全不读 description。
// 这是被用户报障纠正过的结论（「消耗倍率没有显示在切换模型列表的后面」）。
func displayName(m Model) string {
	if m.Name == "" {
		m.Name = nameFromID(m.ID)
	}
	if m.IsFree {
		return m.Name + " · 免费"
	}
	return m.Name
}

// mergeModels 合并远端与本地的模型来源（参照 cline-models.ts:192-235）。
//
// # 顺序即列表展示顺序
//
//  1. 远端 free 数组（最权威的免费集合，且远端顺序有意义）
//  2. 兜底表（离线时也可见）—— 保持表内顺序
//  3. recommended / clinePass 里出现但上面没覆盖的（保持可发现性）
//  4. 远端 /models 的其余 id（**放最后**：460 个裸 id 放前面会把免费模型
//     挤到看不见）
//
// 按 id 去重（首见优先）。
func mergeModels(rec RecommendedModels, remoteIDs []string) []Model {
	remoteFree := map[string]bool{}
	for _, m := range rec.Free {
		remoteFree[m.ID] = true
	}
	// recommended/clinePass 的元数据（name/description）用于补全
	meta := map[string]RemoteModelRef{}
	for _, group := range [][]RemoteModelRef{rec.Recommended, rec.Free, rec.ClinePass} {
		for _, m := range group {
			if _, ok := meta[m.ID]; !ok {
				meta[m.ID] = m
			}
		}
	}

	out := make([]Model, 0, len(fallbackModels)+len(remoteIDs))
	seen := map[string]bool{}

	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true

		fb, hasFB := fallbackByID[id]
		var m Model
		if hasFB {
			m = fb // 拿到 contextWindow / maxTokens / supportsImage 等元数据
		}
		m.ID = id
		// name 优先级：兜底表名 > 远端 name > 由 id 派生
		if !hasFB || strings.TrimSpace(fb.Name) == "" {
			if mm, ok := meta[id]; ok && strings.TrimSpace(mm.Name) != "" {
				m.Name = strings.TrimSpace(mm.Name)
			} else {
				m.Name = nameFromID(id)
			}
		}
		// description 优先级：远端 > 兜底
		if mm, ok := meta[id]; ok && strings.TrimSpace(mm.Description) != "" {
			m.Description = strings.TrimSpace(mm.Description)
		}
		m.IsFree = isFreeModel(id, remoteFree)
		out = append(out, m)
	}

	// 1. 远端 free（顺序有意义）
	for _, m := range rec.Free {
		add(m.ID)
	}
	// 2. 兜底表
	for _, m := range fallbackModels {
		add(m.ID)
	}
	// 3. recommended / clinePass
	for _, m := range rec.Recommended {
		add(m.ID)
	}
	for _, m := range rec.ClinePass {
		add(m.ID)
	}
	// 4. 远端 /models 其余 id
	for _, id := range remoteIDs {
		add(id)
	}
	return out
}

// loadModels 取两个远端来源并合并（参照 cline-models.ts:247-297）。
//
// 两个来源**独立容错**：任一端点失败只记 warning，由合并逻辑用兜底表补齐。
//
// ⚠ 无凭据时**不发** /models：它需要认证，匿名调用必然 401，
// 只会白白产生一条 warning。
func (c *Client) loadModels(ctx context.Context, a *Auth) ([]Model, []string) {
	var warnings []string

	rec, err := c.FetchRecommendedModels(ctx)
	if err != nil {
		warnings = append(warnings, "recommended-models "+err.Error())
	}

	var ids []string
	if a != nil && a.AccessToken != "" {
		ids, err = c.FetchModelIDs(ctx, a)
		if err != nil {
			warnings = append(warnings, "models "+err.Error())
		}
	}

	models := mergeModels(rec, ids)
	return models, warnings
}

// sortModelsStable 让同一份目录在多次调用间顺序稳定。
//
// 远端顺序是有意义的（免费在前），但 /models 的 460 个 id 顺序不应影响
// 展示 —— 兜底表已经按表内顺序排好了，这里只对"全部来自远端"的部分排序。
func sortModelsStable(models []Model, fallbackOrder map[string]int) {
	sort.SliceStable(models, func(i, j int) bool {
		oi, iok := fallbackOrder[models[i].ID]
		oj, jok := fallbackOrder[models[j].ID]
		if iok != jok {
			return iok
		}
		if iok && jok {
			return oi < oj
		}
		return false
	})
}

// logWarnings 打印目录加载的降级信息。
//
// ⚠ 必须打印：静默降级会让用户看到"少了模型"却无从排查。
func logWarnings(warnings []string) {
	for _, w := range warnings {
		log.Printf("cline: 模型目录降级 —— %s", w)
	}
}
