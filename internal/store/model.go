// model.go models + vendors **两级目录**的 seed 与读取（PLAN §4 T09）。
//
// # 为什么目录要"两级"，而不是一张平表
//
// 目录要回答两个不同粒度的问题，而它们是两个不同的实体：
//
//	一级：「这条模型归哪个上游/渠道」  → vendors 一行
//	二级：「这个上游到底提供哪些模型」→ models 若干行（vendors.id = models.vendor_id）
//
// 一张平表（models 里存一列 vendor 字符串）能回答前者，却回答不了后者：
// "这个上游有哪些模型"会退化成全表扫 + 字符串比较，而且改个上游名要 UPDATE 全表。
// 所以这里按 models.go 已定义的两张表展开 —— **不新增列、不改表结构**
// （models.go 是 6 个并发任务的共享文件，谁都不许动）。
//
// # Name 存的是「对外可见 id」，不是上游裸名（这条是计数判据逼出来的）
//
// 本包的名字唯一索引在 models.name 上。若存上游裸名，同一张模型被两个上游
// 同时提供时（"glm-5.2" 在 workbuddy 与 codearts 都能调）就会**折叠成一行**，
// 于是 models 行数与 /v1/models 的可见条目数对不上。
//
// 而目录必须与对外目录**一对一**：用户在 chips 上看到一条，目录里就得有一行，
// 否则"这个模型归谁"这个问题在目录里无解。所以 Name 用对外的那个 id
// （多上游部署下是 "provider/model"，用户要求收窄后的形态见 handler.go 的
// modelList 注释），vendor 用它的 owned_by。
//
// # 合成行为什么也要落库（而不是只在内存里补）
//
// 「渠道用到但目录没有」的模型是**真实存在的服务能力**：渠道声明了它、
// 请求也确实能走通。只在渲染时临时补一行，则任何 SQL 侧的统计/联查都看不到它
// —— 那会造出第二个真相源。所以合成行同样入库，靠 Description 的标记位
// （见 syntheticRowMarker）区分来源；表结构不动。
package store

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// CatalogVendorUnknown 一级行的兜底名。
//
// 上游条目偶尔没有 owned_by（scrub 之后、或上游没给），而 vendors.name 是
// NOT NULL + 唯一索引：留空会让多条无主行互相冲突。给一个显式的"不知道"，
// 比给空串好 —— 空串在界面上看起来像"没这一列"。
const CatalogVendorUnknown = "unknown"

// syntheticRowMarker 合成行在 description 里的前缀。
//
// 为什么不用新列：models.go 的表结构已冻结（见文件头）。为什么用 description：
// 它本来就是给人看的自由文本，且**不参与索引** —— 加前缀不会改变任何查询计划。
// 代价写清楚：任何按 description 做的展示要容忍这个前缀（见 IsSyntheticCatalogRow）。
const syntheticRowMarker = "synthetic:"

// syntheticRowNote 合成行的完整说明（也说明它为什么值得单独记一笔）。
const syntheticRowNote = syntheticRowMarker + " 渠道声明并在用，但上游目录里没有这个名字（由合成行 API 补齐）"

// catalogEndpointChat seed 时每行的默认端点。
//
// 目录来源是 /v1/models（对话目录），所以默认声明 chat。将来有 embeddings /
// images 目录时可以按来源补，但**不要**在这里猜：猜出来的端点会变成
// "目录说有、请求却 404"。
const catalogEndpointChat = "chat"

// CatalogEntry 一条待 seed 的两级目录项。
type CatalogEntry struct {
	// Vendor 一级行（上游 id 或渠道名）。空 ⇒ CatalogVendorUnknown。
	Vendor string
	// Name 二级行（对外可见 id，见文件头）。
	Name string
	// Endpoints 该模型支持的端点；空 ⇒ ["chat"]。
	Endpoints []string
	// Description 备注。
	Description string
	// Synthetic 标记"渠道用到但目录没有"的合成行。
	Synthetic bool
}

// CatalogSeedStats 一次 seed 的结果（只数**新增**，不含已存在的行）。
//
// 为什么要分别数：幂等性判据要能区分"这次真的建了 N 行"与"这次什么都没做"。
// 只回一个 error 的话，二次启动是否重复 seed 就无法断言（T11 的判据之一）。
type CatalogSeedStats struct {
	// Vendors 新建的一级行数。
	Vendors int
	// Models 新建的二级行数。
	Models int
	// Synthetic 其中合成行的条数。
	Synthetic int
}

// CatalogRow 读回的两级目录平铺行（已 JOIN 出一级名）。
type CatalogRow struct {
	ID           int64
	Name         string
	VendorID     int64
	Vendor       string
	EndpointsCSV string
	Description  string
	// Synthetic 是否由合成行 API 补出来的（见 IsSyntheticCatalogRow）。
	Synthetic bool
}

// IsSyntheticCatalogRow 报告一行是否为合成行。
func IsSyntheticCatalogRow(description string) bool {
	return strings.HasPrefix(strings.TrimSpace(description), syntheticRowMarker)
}

// CatalogEntryNames 抽出条目里的名字（保持顺序、去重）。
//
// 合成行 API 需要「已知目录」这份输入；调用方不该为此再查一次库
// （seed 路径上刚算出来的那份就是权威）。
func CatalogEntryNames(entries []CatalogEntry) []string {
	out := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		n := strings.TrimSpace(e.Name)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// SyntheticCatalogEntries 是「渠道用到但目录没有」的**合成行 API**。
//
// 输入是两份名字：known（目录里已有什么）与 used（渠道声明在用什么），
// 输出是 used 里**不在** known 的那些条目，已打好合成标记。
//
// # 为什么是纯函数、不写库
//
// 它同时被两种调用方需要：
//
//	展示侧：目录视图要把这些行**并进**列表（不落库也行）
//	seed 侧：把它们一起 seed 进 models 表（要落库）
//
// 做成纯函数，两条路共用同一份判据，且测试不需要数据库。
// vendorOf 给出每个名字该挂到哪个一级行（渠道名）；nil / 返回空 ⇒ unknown。
func SyntheticCatalogEntries(known, used []string, vendorOf func(string) string) []CatalogEntry {
	knownSet := make(map[string]bool, len(known))
	for _, k := range known {
		if k = strings.TrimSpace(k); k != "" {
			knownSet[k] = true
		}
	}
	out := make([]CatalogEntry, 0, len(used))
	seen := make(map[string]bool, len(used))
	for _, u := range used {
		name := strings.TrimSpace(u)
		if name == "" || knownSet[name] || seen[name] {
			continue
		}
		seen[name] = true
		vendor := CatalogVendorUnknown
		if vendorOf != nil {
			if v := strings.TrimSpace(vendorOf(name)); v != "" {
				vendor = v
			}
		}
		out = append(out, CatalogEntry{
			Vendor:    vendor,
			Name:      name,
			Endpoints: []string{catalogEndpointChat},
			Synthetic: true,
		})
	}
	return out
}

// SeedModelCatalog 把两级目录写进 models + vendors。
//
// # 幂等（这是它最重要的性质）
//
// 它按名字查、缺了才建，**不覆盖**已存在的行：第二次调用新增数恒为 0。
// 首启 seed 与二次启动因此可以共用一个函数，T11 的"二次启动不重复 seed"
// 不需要另写一条路径。
//
// 为什么"不覆盖"而不是"upsert 更新"：目录的名字来自上游实时接口，
// 而上游会漂移。upsert 会把一次**上游抖动**（返回残缺目录）造成的空字段
// 写进已有行；缺了才建则最坏只是少几行新模型，不会毁掉已有数据。
//
// 整个 seed 在一个事务里：一级行与二级行必须一起出现或一起不出现，
// 否则中途失败会留下"有 vendor 没有 model"的半截目录。
func SeedModelCatalog(entries []CatalogEntry) (CatalogSeedStats, error) {
	var st CatalogSeedStats
	if DB == nil {
		return st, ErrNotOpen
	}
	norm := normalizeCatalogEntries(entries)
	if len(norm) == 0 {
		return st, nil
	}
	err := DB.Transaction(func(tx *gorm.DB) error {
		vendorIDs := make(map[string]int64, 8)
		for _, e := range norm {
			vid, ok := vendorIDs[e.Vendor]
			if !ok {
				id, created, err := ensureCatalogVendor(tx, e.Vendor)
				if err != nil {
					return err
				}
				if created {
					st.Vendors++
				}
				vendorIDs[e.Vendor] = id
				vid = id
			}
			created, err := ensureCatalogModel(tx, e, vid)
			if err != nil {
				return err
			}
			if created {
				st.Models++
				if e.Synthetic {
					st.Synthetic++
				}
			}
		}
		return nil
	})
	if err != nil {
		return CatalogSeedStats{}, err
	}
	return st, nil
}

// ListCatalogRows 读回两级目录（平铺、按名字升序）。
//
// 平铺而不是返回两张表：调用方（目录视图 / 计数断言）要的正是
// 「一行 = 一条可见模型 + 它的来源」，JOIN 放在这里做一次，
// 免得每个调用方各写一遍（写两遍必然不一致）。
func ListCatalogRows() ([]CatalogRow, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var models []Model
	if err := DB.Order("name ASC").Find(&models).Error; err != nil {
		return nil, fmt.Errorf("store: 读 models 失败: %w", err)
	}
	var vendors []Vendor
	if err := DB.Find(&vendors).Error; err != nil {
		return nil, fmt.Errorf("store: 读 vendors 失败: %w", err)
	}
	nameOf := make(map[int64]string, len(vendors))
	for _, v := range vendors {
		nameOf[v.ID] = v.Name
	}
	out := make([]CatalogRow, 0, len(models))
	for _, m := range models {
		out = append(out, CatalogRow{
			ID:           m.ID,
			Name:         m.Name,
			VendorID:     m.VendorID,
			Vendor:       nameOf[m.VendorID],
			EndpointsCSV: m.EndpointsCSV,
			Description:  m.Description,
			Synthetic:    IsSyntheticCatalogRow(m.Description),
		})
	}
	return out, nil
}

// CountCatalogModels 返回 models 行数（T09 的计数判据直接读它）。
func CountCatalogModels() (int64, error) {
	return countTable(&Model{})
}

// CountCatalogVendors 返回 vendors 行数。
func CountCatalogVendors() (int64, error) {
	return countTable(&Vendor{})
}

// countTable 是两张表的公共计数（DB 未打开时返回 ErrNotOpen，而不是 0）。
//
// 为什么不是 0：0 与"库没打开"在调用方眼里会变成同一件事，
// 而两者要的处置完全相反（一个是真没有，一个是接线漏了）。
func countTable(model any) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var n int64
	if err := DB.Model(model).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 计数失败: %w", err)
	}
	return n, nil
}

// normalizeCatalogEntries 归一化 + 按名字去重（先到先得）。
func normalizeCatalogEntries(entries []CatalogEntry) []CatalogEntry {
	out := make([]CatalogEntry, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		vendor := strings.TrimSpace(e.Vendor)
		if vendor == "" {
			vendor = CatalogVendorUnknown
		}
		desc := strings.TrimSpace(e.Description)
		if e.Synthetic {
			desc = syntheticRowNote
		}
		out = append(out, CatalogEntry{
			Vendor:      vendor,
			Name:        name,
			Endpoints:   normalizeCatalogEndpoints(e.Endpoints),
			Description: desc,
			Synthetic:   e.Synthetic,
		})
	}
	return out
}

// normalizeCatalogEndpoints 归一化端点列表（去空、去重、兜底 chat）。
func normalizeCatalogEndpoints(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, e := range in {
		e = strings.TrimSpace(e)
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	if len(out) == 0 {
		out = append(out, catalogEndpointChat)
	}
	return out
}

// ensureCatalogVendor 取或建一级行，返回 (id, 是否新建)。
//
// 冲突兜底：唯一索引（vendors.name）在并发/重入下会拒绝重复插入。
// 那时**重查一次**而不是报错 —— 目标状态（这一行存在）已经达成，
// 把一次无害的竞争报成 seed 失败，只会让启动期多一个随机故障。
func ensureCatalogVendor(tx *gorm.DB, name string) (int64, bool, error) {
	var v Vendor
	// ⚠ 用 Find 而不是 First：First 在空结果时返回 ErrRecordNotFound，
	// 而 GORM 的默认 logger 会把每条都按 error 打出来 —— seed 一个 700 条
	// 的目录就是 700 行"record not found"噪音，真正的故障会被淹掉。
	// Find 用 RowsAffected 表达"没找到"，语义一样且不产生日志。
	res := tx.Where("name = ?", name).Limit(1).Find(&v)
	if res.Error != nil {
		return 0, false, fmt.Errorf("store: 查 vendor %q 失败: %w", name, res.Error)
	}
	if res.RowsAffected > 0 {
		return v.ID, false, nil
	}
	v = Vendor{Name: name, Description: "两级目录的一级行（seed 自对外目录的 owned_by）"}
	if err := tx.Create(&v).Error; err != nil {
		// 唯一索引冲突（并发/重入）⇒ 目标状态已达成，重查一次即可。
		var again Vendor
		if e2 := tx.Where("name = ?", name).Limit(1).Find(&again); e2.Error == nil && e2.RowsAffected > 0 {
			return again.ID, false, nil
		}
		return 0, false, fmt.Errorf("store: 建 vendor %q 失败: %w", name, err)
	}
	return v.ID, true, nil
}

// ensureCatalogModel 取或建二级行，返回 (是否新建)。
func ensureCatalogModel(tx *gorm.DB, e CatalogEntry, vendorID int64) (bool, error) {
	var m Model
	res := tx.Where("name = ?", e.Name).Limit(1).Find(&m) // 见 ensureCatalogVendor：Find 不产生 not-found 日志
	if res.Error != nil {
		return false, fmt.Errorf("store: 查 model %q 失败: %w", e.Name, res.Error)
	}
	if res.RowsAffected > 0 {
		return false, nil
	}
	m = Model{
		Name:         e.Name,
		VendorID:     vendorID,
		EndpointsCSV: strings.Join(e.Endpoints, ","),
		Description:  e.Description,
	}
	if err := tx.Create(&m).Error; err != nil {
		var again Model
		if e2 := tx.Where("name = ?", e.Name).Limit(1).Find(&again); e2.Error == nil && e2.RowsAffected > 0 {
			return false, nil
		}
		return false, fmt.Errorf("store: 建 model %q 失败: %w", e.Name, err)
	}
	return true, nil
}
