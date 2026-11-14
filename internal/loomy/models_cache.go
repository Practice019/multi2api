// models_cache.go 模型目录的**缓存快照**与**并集刷新**。
//
// # 它修的是什么（2026-11-14 用户报障）
//
// 用户看到 loomy 只有 5 个模型（且名字是 `spark-x2.6-low` / `spark-x2.6-4b`），
// 而他另有一个实例显示 10 个。问「不同的电脑不可能检测不一样吧」。
//
// 实测结论（证据在 test 里可复跑）：
//
//	上游账号 A 的 GET /models → 10 个（spark-x2.6 / DeepSeek-V4.1-Flash / …）
//	上游账号 B 的 GET /models → 另一组（spark-x2.6-low / spark-x2.6-4b / …）
//	我们原来的目录            → **只取了池子里抽到的那一个账号**
//
// 也就是：**上游是按账号权限下发不同模型集的**，而 Provider.Models 只收一个
// 凭证 ⇒ 单号视角必然不全，而且"抽到谁就是谁"（每次不一样，看起来像环境差异）。
//
// # 为什么是"缓存 + 定时刷新"，而不是每请求并集
//
// 用户 2026-11-14 定的方案，也正是本仓库既有纪律（见 models_prewarm.go 文件头）：
//
//	请求路径：只读缓存（Catalog()）
//	后台：把缓存灌热（StartCatalogPrewarm）
//
// 每请求并集等于"用 5 次上游调用回答一次目录请求"——把一次白烧变成每次白烧。
//
// # 静态表 knownModels 的新角色
//
// 它**降级为冷启动兜底基线**（models.go 里那张 2026-09-14 的手册快照，名字已经
// 全对不上了）。目录的正源是这份缓存；缓存完全没有时（首次启动且刷新失败）
// 才回落到它 —— 至少比"目录空"强（空目录会让客户端判定模型不存在）。
package loomy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/gateway"
)

const (
	// CatalogCacheTTL 目录快照的保质期。
	//
	// 24h（用户 2026-11-14 定：「定时更新我希望24h就行」）。
	// 取值理由：上游模型集的变动以天为尺度；而每次刷新要打 5 个账号，
	// 频率再高只是多花上游调用，签下的收益几乎为零。
	CatalogCacheTTL = 24 * time.Hour

	// CatalogCacheSampleSize 一次刷新最多抽几个账号做并集。
	//
	// 为什么必须 >1：上游按账号下发不同模型集（实测同一上游两个号给出的
	// 集合只有部分交集），单号视角必然缺项。
	// 为什么是 5 而不是全池 45：成本 = 账号数 × 1 次 /models。
	// 5 个已覆盖绝大多数授权差异；全池在 24h 节奏下也只多花 40 次调用，
	// 但会让一次刷新耗时从 ~1s 涨到 ~10s（串行）—— 收益递减，不做。
	CatalogCacheSampleSize = 5

	// catalogCacheFileName 落盘文件名（放在 authDir 下，与凭证同目录便于一起备份）。
	catalogCacheFileName = "models-cache.json"

	// catalogCacheSchema 落盘格式版本。读到不认识的版本就当没有缓存
	// （宁可回落到静态基线，也不要用一份字段含义已变的旧数据）。
	catalogCacheSchema = 1
)

// CatalogSnapshot 一次刷新得到的目录快照（内存与落盘同一形态）。
type CatalogSnapshot struct {
	Schema int `json:"schema"`
	// FetchedAt 这次刷新完成的时间。
	FetchedAt time.Time `json:"fetched_at"`
	// Models 并集去重后的目录。
	Models []Model `json:"models"`
	// Sources 贡献过至少一个模型的账号 UID（诊断：并集是谁给的）。
	Sources []string `json:"sources,omitempty"`
	// FailedUIDs 本次拉取失败的账号 UID（诊断：这次为什么可能不全）。
	FailedUIDs []string `json:"failed_uids,omitempty"`
}

// Age 距这次刷新的时长。
func (s *CatalogSnapshot) Age(now time.Time) time.Duration {
	if s == nil || s.FetchedAt.IsZero() {
		return -1
	}
	return now.Sub(s.FetchedAt)
}

// Fresh 快照是否还在保质期内。
func (s *CatalogSnapshot) Fresh(now time.Time) bool {
	age := s.Age(now)
	return age >= 0 && age < CatalogCacheTTL
}

// Len 快照里的模型数。
func (s *CatalogSnapshot) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Models)
}

// unionModels 把多个账号的目录并起来，按 ID **大小写不敏感**去重。
//
// # 为什么去重必须忽略大小写
//
// 上游对同一模型的写法不稳（`Hy-Image-3.5-preview` / `hy-image-3.5-preview`
// 都可能出现），而用户在另一个实例上看到的正是**同一个模型列了两次**的行。
// 按字节比较会把它们当成两个模型。
//
// # 合并策略
//
// 同一 ID 保留**首个**出现的条目，但元数据取「首个非零值」：某个号可能只回了
// id（上下文字段缺失），后面的号回了完整字段 —— 那就用完整的那份。
// 再缺的用静态表 knownModels 补（见 toModelInfos 的同款思路）。
func unionModels(sets [][]Model) []Model {
	out := make([]Model, 0, 16)
	idx := make(map[string]int, 16) // lower(ID) → out 下标

	for _, set := range sets {
		for _, m := range set {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue
			}
			key := strings.ToLower(id)
			if at, ok := idx[key]; ok {
				out[at] = mergeModel(out[at], m)
				continue
			}
			idx[key] = len(out)
			out = append(out, mergeModel(Model{ID: id}, m))
		}
	}
	// 元数据兜底：静态表里有的字段补上（只补空，不覆盖上游给的）。
	for i := range out {
		if known, _, ok := ResolveModel(out[i].ID); ok {
			if out[i].ContextWindow <= 0 {
				out[i].ContextWindow = known.ContextWindow
			}
			if out[i].MaxOutputTokens <= 0 {
				out[i].MaxOutputTokens = known.MaxOutputTokens
			}
			if out[i].Name == "" {
				out[i].Name = known.Name
			}
		}
	}
	return out
}

// mergeModel 把 b 的非零字段合并进 a（a 优先）。
func mergeModel(a, b Model) Model {
	if a.ContextWindow <= 0 && b.ContextWindow > 0 {
		a.ContextWindow = b.ContextWindow
	}
	if a.MaxOutputTokens <= 0 && b.MaxOutputTokens > 0 {
		a.MaxOutputTokens = b.MaxOutputTokens
	}
	if a.Name == "" && b.Name != "" {
		a.Name = b.Name
	}
	// Unavailable 取"任一为 true"：只要有一个号说它在 chat 端点 404，就当不可用。
	// （这个词的语义在 models.go 里写得很清楚：只描述**对话**端点。）
	if b.Unavailable {
		a.Unavailable = true
	}
	return a
}

// catalogCachePath 快照落盘路径。未配 authDir 时返回空串（不落盘）。
func (p *Provider) catalogCachePath() string {
	dir := strings.TrimSpace(p.authDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, catalogCacheFileName)
}

// Catalog 返回当前目录快照（**只读，绝不回源**）。
//
// # 查三层
//
//	① 内存快照   —— 稳态下就是这里，一次加锁
//	② 落盘快照   —— 重启后的第一次访问（冷启动不该是空的）
//	③ nil        —— 交给调用方回落静态基线（见 Models）
//
// ⚠ 过期的快照照样返回。TTL 只决定"后台要不要刷新"，不决定"要不要给用户看"：
// 用一份 25 小时前的完整目录，比用一个空目录好得多。
func (p *Provider) Catalog() *CatalogSnapshot {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	if p.catalog != nil {
		return p.catalog
	}
	snap := loadCatalogSnapshotFromDisk(p.catalogCachePath())
	if snap != nil {
		p.catalog = snap
	}
	return p.catalog
}

// loadCatalogSnapshotFromDisk 读落盘快照。任何异常都返回 nil（不 panic、不报错）：
// 缓存读不出来等价于"没有缓存"，是**可接受的降级**，不该打断目录请求。
func loadCatalogSnapshotFromDisk(path string) *CatalogSnapshot {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var snap CatalogSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		log.Printf("loomy: 目录缓存 %s 解析失败（当作没有缓存）: %v", path, err)
		return nil
	}
	if snap.Schema != catalogCacheSchema {
		log.Printf("loomy: 目录缓存 %s 的 schema=%d 不认识（期望 %d），忽略", path, snap.Schema, catalogCacheSchema)
		return nil
	}
	if len(snap.Models) == 0 {
		return nil
	}
	return &snap
}

// RefreshCatalog 抽样并集刷新目录快照，并落盘。
//
// # 语义
//
//   - 抽样最多 CatalogCacheSampleSize 个账号（按 authDir 里的顺序取前 N 个；
//     不做健康探测 —— 健康与否由拉取结果体现，失败就跳过）
//   - 并集去重（unionModels）
//   - **至少一个账号成功**才覆盖快照；全失败返回错误并**保留旧快照**
//     （目录静默变空比不全更糟，见 gateway.verifyModelsNonEmpty 的注释）
//   - 落盘失败只记日志：内存快照已更新，落盘只是"下次重启能省一次刷新"
func (p *Provider) RefreshCatalog(ctx context.Context) (*CatalogSnapshot, error) {
	auths, err := loadSampleAuths(p.authDir, CatalogCacheSampleSize)
	if err != nil {
		return nil, err
	}
	if len(auths) == 0 {
		return nil, errors.New("loomy: 目录刷新失败：账号目录里没有凭证")
	}

	sets := make([][]Model, 0, len(auths))
	sources := make([]string, 0, len(auths))
	failed := make([]string, 0, len(auths))
	var lastErr error

	for _, a := range auths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ms, lerr := p.client.ModelList(ctx, a)
		if lerr != nil {
			failed = append(failed, a.UID)
			lastErr = lerr
			continue
		}
		if len(ms) == 0 {
			failed = append(failed, a.UID)
			continue
		}
		sets = append(sets, ms)
		sources = append(sources, a.UID)
	}

	if len(sets) == 0 {
		// 全失败：保留旧快照，只报错。
		old := p.Catalog()
		if old != nil {
			return old, fmt.Errorf("loomy: 目录刷新全部账号失败（保留 %d 个模型的旧快照）: %w",
				old.Len(), lastErr)
		}
		return nil, fmt.Errorf("loomy: 目录刷新全部账号失败，且没有旧快照: %w", lastErr)
	}

	snap := &CatalogSnapshot{
		Schema:     catalogCacheSchema,
		FetchedAt:  time.Now(),
		Models:     unionModels(sets),
		Sources:    sources,
		FailedUIDs: failed,
	}

	p.catalogMu.Lock()
	p.catalog = snap
	p.catalogMu.Unlock()

	if path := p.catalogCachePath(); path != "" {
		if err := saveCatalogSnapshotToDisk(path, snap); err != nil {
			log.Printf("loomy: 目录缓存落盘失败（内存已更新，不影响本次生效）: %v", err)
		}
	}
	return snap, nil
}

// saveCatalogSnapshotToDisk 原子写（先临时文件再 rename）：避免进程在写一半时
// 被杀，留下一份**解析不了**的缓存 —— 那会让下次启动静默退回静态基线。
func saveCatalogSnapshotToDisk(path string, snap *CatalogSnapshot) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadSampleAuths 读 authDir 里最多 n 份凭证（按文件名排序取前 n，保证确定性）。
//
// 为什么取前 n 而不是随机：同一次部署反复刷新应该给出同一个结果，
// 随机抽样会让"这次多了一个模型"变得无法解释（到底是上游变了还是抽到别的号）。
func loadSampleAuths(dir string, n int) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	all, err := LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("loomy: 读凭证目录失败: %w", err)
	}
	// LoadDir 已按文件名排序（os.ReadDir 的顺序），这里只需截断。
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	return all, nil
}

// RefreshCatalogModels 实现 gateway.ModelCatalogRefresher：强制回源并投影成中立契约。
//
// 这是「重拉」按钮走的那条路 —— 它必须**绕过 TTL**，否则缓存没过期时
// 按钮就是"点了没反应"（Models() 只读缓存）。
func (p *Provider) RefreshCatalogModels(ctx context.Context) ([]gateway.ModelInfo, error) {
	snap, err := p.RefreshCatalog(ctx)
	if err != nil {
		// ⚠ 不返回部分结果：调用方看到 error 会保留旧缓存，这正是我们要的
		//   （旧快照已在 RefreshCatalog 里保住）。
		return nil, err
	}
	if snap == nil || snap.Len() == 0 {
		return nil, errors.New("loomy: 目录刷新后仍为空")
	}
	return toModelInfos(snap.Models), nil
}

// StartCatalogPrewarm 后台定时刷新目录（幂等）。
//
// 语义与 StartPanelWarmup / StartFreePoolSweep 一致（同一套约定）：
//
//	① 不阻塞启动：立刻返回，第一次刷新在 goroutine 里做
//	② 幂等：重复调用不会起第二个循环
//	③ 随进程退出而结束（常驻 goroutine，不额外接 ctx —— 与兄弟方法同形）
//
// 启动时**先刷一次**（不等 24h）：否则首次部署的用户要等一整天才有目录，
// 而这段时间目录是空的（只剩静态基线）。
func (p *Provider) StartCatalogPrewarm() {
	if p == nil {
		return
	}
	p.catalogOnce.Do(func() {
		go func() {
			refresh := func() {
				// 独立超时：刷新要打 5 个账号，不该被任何单次请求的 ctx 掐断。
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				snap, err := p.RefreshCatalog(ctx)
				if err != nil {
					log.Printf("loomy: 目录预热失败（沿用旧快照 %d 个模型）: %v", snap.Len(), err)
					return
				}
				log.Printf("loomy: 目录预热完成 %d 个模型（来源 %d 个账号，失败 %d）",
					snap.Len(), len(snap.Sources), len(snap.FailedUIDs))
			}

			refresh() // 启动即刷
			t := time.NewTicker(CatalogCacheTTL)
			defer t.Stop()
			for range t.C {
				refresh()
			}
		}()
	})
}
