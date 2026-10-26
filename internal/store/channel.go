// channel.go channels + abilities 两表的反向 seed 与「模型↔渠道」展开（PLAN §4 T08）。
//
// # 这一层做什么、不做什么
//
// 本文件只认 ChannelSeed 这个**中立结构**与两张表的读写：
//
//	channels  —— 渠道行（代码内 provider 的数据化形态，见 models.go 的 Channel 注释）
//	abilities —— (group, model, channel_id) 的绑定行（"哪个模型能被哪条渠道服务"）
//
// 「到底有哪些注册点」是**装配层的事实**（cmd/server/main.go 的 registry.Register
// 调用点），所以那份 13 条清单放在 internal/admin/channels.go 的
// RegistryChannelSeeds —— 那里才有 Config.Registry 与上游清单。
// 于是本包不 import 任何上游包、也不写死任何上游名：加一个新上游时本文件零改动。
//
// # 为什么"反向"(从注册点回填数据)而不是"正向"(先建渠道再让上游来认领)
//
// 本仓的运行时真相一直是代码 + config.json：13 处 registry.Register 决定了
// "这台机器有哪些渠道"。渠道表若只能由人在界面上手工建，首启部署就会出现
// 「上游在跑、channels 表空」的分裂。反向 seed 让表从**已经成立的事实**长出来，
// 首启即可对账（13 个注册点 ⇒ 13 行）。
//
// # 身份键为什么必须包含 BaseURL（这是本任务最容易踩的那个坑）
//
// main.go:275 的 wb 与 :347 的 wbIntl 是**同一个上游（WorkBuddy）的两个渠道实例**：
// 前者 ID=workbuddy、授权站点 copilot.tencent.com；后者 ID=workbuddy-intl、
// 授权站点 www.workbuddy.ai（:347 注释原文「海外版 WorkBuddy AI（workbuddy-intl
// 渠道实例）」）。任何"按 provider 去重"的写法 —— 无论是按上游实现
// （*workbuddy.Provider）、按上游品牌、还是按上游包里的 ProviderID 常量
// （两个实例都来自 internal/workbuddy）—— 都会把两行并成一行。
//
// 所以本文件的身份键是 (Name, BaseURL)，而 Name 就是注册点上的 Provider.ID()：
//
//	身份 = 一个注册点（provider 实例 + 它连的那个端点）
//	不是 = 一个上游（上游可以有多个实例）
//
// 后一条由 TestChannelSeed 的两个反例钉住：① 目录必须 13 条而不是 upstream 包数；
// ② 同名 provider、不同 BaseURL 的两条 seed 必须落成两行。
//
// # 幂等（首启 seed 与二次启动共用一条路径）
//
// 按身份键查、缺了才建，**不覆盖** Status（用户手工停用过的渠道不该被 seed 复活）、
// 只回填 Models。T11 的"二次启动不重复 seed"因此不需要另写一条路径。
//
// # Wave A1（2026-10-26）：本文件从"seed"扩成**渠道仓储**
//
// 上半部分（SeedChannels … ensureAbility）是 T08 的反向 seed，原样保留；
// 下半部分（ChannelInfo / 多 Key / CRUD / 分页搜索 / 状态 / 能力重建）是
// hyxapi 渠道管理后端的对等面：
//
//	写路径  —— 新增/更新/批量删除/删除已禁用/复制/修复，**渠道行与能力行同事务**（D3）
//	读路径  —— 列表与详情**一律 Omit("key")**，明文只走 GetChannelKey/GetChannelKeys（D1）
//	多 Key —— channel_info JSON 列 + per-channel 锁 + 下标截断（D2）
//
// # D3 的不变量（照它写 handler 时最容易漏的一条）
//
// **只写 channels 不写 abilities 的表现是「渠道存在但永远选不到」，而且没有任何
// 编译/启动错误。** 所以本文件的每个写路径都在同一事务里重建该渠道的能力行，
// 并由 TestChannelWritePathsKeepAbilities 逐条钉住（含"故意只写 channels"的反例）。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// AbilityDefaultGroup 本仓唯一的分组名。
//
// 与 channelstore.DefaultGroup 同字面量（那边是 JSON 实现，这边是 SQL 实现，
// 两边必须表达同一个分组）。**不用常量引用**：store 是数据层，不该依赖
// channelstore —— 一个字符串字面量换来零耦合，值得。
const AbilityDefaultGroup = "default"

// 渠道状态。数值与 new-api / channelstore 一致（models.go 的 Channel.Status 注释）。
const (
	// ChannelStatusEnabled 启用。
	ChannelStatusEnabled = 1
	// ChannelStatusManuallyDisabled 手工停用。
	ChannelStatusManuallyDisabled = 2
	// ChannelStatusAutoDisabled 自动停用（上游判为不可用）。
	ChannelStatusAutoDisabled = 3
)

// ChannelSeed 一条渠道的 seed 定义。**一个条目 = main.go 里的一处 registry.Register**。
type ChannelSeed struct {
	// Provider 该注册点注册的 Provider.ID()。同一份 seed 列表里允许重复
	// （同一上游的多个实例），判身份的是 (Provider, BaseURL) —— 见文件头。
	Provider string
	// BaseURL 该实例连的端点。两个同上游实例靠它区分（wb / wbIntl）。
	BaseURL string
	// Source 反向来源，形如 "cmd/server/main.go:347"。只用于日志与对照，
	// **不进库**（channels 表结构已冻结，见 models.go 文件头）。
	Source string
	// Models 该渠道声明的模型（上游原名）。空 = 目录是运行时事实
	// （账号型上游的模型目录要活凭证才能取），此时不建能力行。
	Models []string
	// Priority 越大越先用；0 = 未指定。
	Priority int
	// Weight 同档内加权随机；0 = 未指定。
	Weight int
}

// ChannelSeedRow 一行 seed 结果（渠道 → 库内 id 的对照）。
//
// 为什么要把 Source 一起带回来：验收判据要的是「逐行 provider/id 对照」，
// 而 provider 与 source 在库里都不存在（表结构冻结），只有 seed 列表知道。
// 让调用方事后去 zip 两份列表，迟早有人 zip 错顺序 —— 这里一次给出。
type ChannelSeedRow struct {
	ChannelID int64
	Provider  string
	BaseURL   string
	Source    string
	// Models 库里 models 列的**原样字符串**（逗号分隔）——不是 []string。
	//
	// ⚠ 旧字段名 ModelsCSV 已随冻结契约为 Models（列名就叫 models）；
	// 这里刻意保留"字符串"语义的注释，避免与 ChannelSeed.Models []string 看混。
	Models string
	Status int
}

// ChannelSeedStats 一次 seed 的结果。
//
// 分列 Created / Matched 而不是只回总数：幂等性判据要能区分
// 「这次真的建了 13 行」与「这次一行都没建（全都已存在）」。
// 只回一个 error 的话，二次启动是否重复 seed 就无法断言。
type ChannelSeedStats struct {
	// Created 本次新建的渠道行数。
	Created int
	// Matched 本次命中已有渠道的行数。
	Matched int
	// Abilities 本次新建的能力行数（重复展开恒为 0）。
	Abilities int
	// Rows 逐行对照（顺序 = seed 列表顺序）。
	Rows []ChannelSeedRow
}

// SeedChannels 按 seeds 建/补渠道行，并展开每条渠道声明的模型为能力行。
//
// 整批在一个事务里：渠道行与它的能力行必须一起出现或一起不出现，
// 否则中途失败会留下"渠道在、能力不在"的半截状态 —— 而能力索引正是选路读的那张表。
func SeedChannels(seeds []ChannelSeed) (ChannelSeedStats, error) {
	var st ChannelSeedStats
	if DB == nil {
		return st, ErrNotOpen
	}
	norm, err := normalizeChannelSeeds(seeds)
	if err != nil {
		return st, err
	}
	if len(norm) == 0 {
		return st, nil
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		for _, s := range norm {
			ch, created, err := ensureChannel(tx, s)
			if err != nil {
				return err
			}
			if created {
				st.Created++
			} else {
				st.Matched++
			}
			n, err := expandAbilities(tx, ch.ID, AbilityDefaultGroup, s.Models)
			if err != nil {
				return err
			}
			st.Abilities += n
			st.Rows = append(st.Rows, ChannelSeedRow{
				ChannelID: ch.ID, Provider: s.Provider, BaseURL: s.BaseURL,
				Source: s.Source, Models: ch.Models, Status: ch.Status,
			})
		}
		return nil
	})
	if err != nil {
		return ChannelSeedStats{}, err
	}
	return st, nil
}

// ExpandAbilities 把 models 展开成 (group, model, channelID) 能力行（缺了才建）。
//
// 公开它是因为能力行有两个来源：
//
//	seed 期   —— 渠道声明的模型（本文件调用）
//	目录就绪后 —— 上游目录里的模型（T09 的 models 表，见 ExpandAbilitiesFromCatalog）
//
// 两处必须共用这一份实现，否则"哪些模型能被这条渠道服务"会出现两个判据。
func ExpandAbilities(channelID int64, group string, models []string) (int, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if channelID == 0 {
		return 0, fmt.Errorf("store: 展开能力行缺 channel_id")
	}
	if strings.TrimSpace(group) == "" {
		group = AbilityDefaultGroup
	}
	var created int
	err := DB.Transaction(func(tx *gorm.DB) error {
		n, err := expandAbilities(tx, channelID, group, models)
		created = n
		return err
	})
	if err != nil {
		return 0, err
	}
	return created, nil
}

// ExpandAbilitiesFromCatalog 按「目录里的一级名（vendor）= 渠道名（provider id）」
// 把 models × channels 展开成能力行。
//
// # 为什么这条 JOIN 是"模型↔渠道"那半张图
//
// T09 的一级行（vendors.name）存的是上游 id（对外目录的 owned_by），而本文件的
// 渠道行 Name 就是注册点上的 Provider.ID() —— 两者是同一个命名空间的同一个键。
// 于是"这个模型归哪个上游"与"这条渠道是哪个上游"在这里合流成一条绑定。
//
// 目录为空时它什么都不做（返回 0）：账号型上游的目录要活凭证才能取，
// seed 期（启动时、还没有请求打过上游）恒为空是**正常状态**，不是错误。
// 这也是它不在 SeedChannels 里被隐式调用的原因 —— 由装配层在目录 seed 之后显式调。
func ExpandAbilitiesFromCatalog(group string) (int, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if strings.TrimSpace(group) == "" {
		group = AbilityDefaultGroup
	}
	chans, err := ListChannels()
	if err != nil {
		return 0, err
	}
	catalog, err := ListCatalogRows()
	if err != nil {
		return 0, err
	}
	byVendor := make(map[string][]string, len(catalog))
	for _, row := range catalog {
		if row.Vendor == "" || row.Name == "" {
			continue
		}
		byVendor[row.Vendor] = append(byVendor[row.Vendor], row.Name)
	}
	var total int
	for _, ch := range chans {
		models := byVendor[ch.Name]
		if len(models) == 0 {
			continue
		}
		n, err := ExpandAbilities(ch.ID, group, models)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// ListChannels 读回全部渠道行，**按 id 升序**（= seed 列表顺序 = main.go 注册顺序）。
//
// 顺序稳定是判据的一部分：验收要"逐行 provider/id 对照"，插入顺序就是注册顺序，
// 调用方因此不必再猜排序。
//
// ⚠ **不带明文 key**（Omit("key")，D1）：列表是给界面/对账用的，凭据不在这条路上。
// 需要明文只有两个显式入口：GetChannelKey / GetChannelKeys。
func ListChannels() ([]Channel, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var out []Channel
	if err := DB.Omit("key").Order("id ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("store: 读 channels 失败: %w", err)
	}
	return out, nil
}

// ListAbilities 读回全部能力行（按 channel_id、model 升序，顺序稳定）。
func ListAbilities() ([]Ability, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var out []Ability
	if err := DB.Order("channel_id ASC, model ASC").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("store: 读 abilities 失败: %w", err)
	}
	return out, nil
}

// CountChannels 返回 channels 行数（验收判据读它；DB 未打开时返回 ErrNotOpen 而不是 0）。
func CountChannels() (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var n int64
	if err := DB.Model(&Channel{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 数 channels 失败: %w", err)
	}
	return n, nil
}

// CountAbilities 返回 abilities 行数。
func CountAbilities() (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var n int64
	if err := DB.Model(&Ability{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("store: 数 abilities 失败: %w", err)
	}
	return n, nil
}

// normalizeChannelSeeds 归一化输入并做**同一批内的身份冲突检查**。
//
// 冲突要报错而不是静默合并：两条 seed 若身份相同，落库后会只有一行，
// "13 个注册点 ⇒ 13 行"的判据就静默失真了 —— 那种情况下要的是
// 一个显式的装配错误，而不是一行看起来正常的日志。
func normalizeChannelSeeds(seeds []ChannelSeed) ([]ChannelSeed, error) {
	out := make([]ChannelSeed, 0, len(seeds))
	seen := make(map[string]string, len(seeds))
	for _, s := range seeds {
		provider := strings.TrimSpace(s.Provider)
		if provider == "" {
			return nil, fmt.Errorf("store: 渠道 seed 缺 provider（渠道名是身份键的一半）")
		}
		base := strings.TrimSpace(s.BaseURL)
		key := channelSeedKey(provider, base)
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("store: 渠道 seed 身份重复 provider=%q base_url=%q（前一处 %s）", provider, base, prev)
		}
		seen[key] = strings.TrimSpace(s.Source)
		out = append(out, ChannelSeed{
			Provider: provider,
			BaseURL:  base,
			Source:   strings.TrimSpace(s.Source),
			Models:   normalizeSeedModels(s.Models),
			Priority: s.Priority,
			Weight:   s.Weight,
		})
	}
	return out, nil
}

// channelSeedKey 是身份键的可比较形态。
//
// ⚠ 刻意**不是** provider 一个字段：见文件头"身份键为什么必须包含 BaseURL"。
// 用 \x00 连接避免 (ab, c) 与 (a, bc) 撞成同一个键。
func channelSeedKey(provider, baseURL string) string {
	return provider + "\x00" + baseURL
}

// normalizeSeedModels 归一化模型名（去空、去重、保序）。
func normalizeSeedModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// joinSeedModels 把模型名拼成 channels.models（逗号分隔，列形态对齐参考项目）。
func joinSeedModels(models []string) string {
	return strings.Join(normalizeSeedModels(models), ",")
}

// ensureChannel 取或建一条渠道行，返回 (渠道, 是否新建)。
//
// 已存在时**只补 Models**：
//   - Status 不动 —— 用户手工停用过的渠道不该被下一次 seed 复活；
//   - Priority/Weight 不动 —— 同上，那是运维在界面上调过的值。
//
// 冲突兜底：身份不唯一索引（表结构冻结，没有这个索引），并发/重入下可能
// 两个事务同时判定"不存在"。那时 Create 不会报错（没有约束拦它），
// 所以这里不做 T09 那种"重查一次"的兜底 —— 启动期 seed 是单线程的，
// 真出现并发应当由调用方串行化（T11 的启动路径本来就是串行的）。
func ensureChannel(tx *gorm.DB, s ChannelSeed) (*Channel, bool, error) {
	var ch Channel
	err := tx.Where("name = ? AND base_url = ?", s.Provider, s.BaseURL).First(&ch).Error
	if err == nil {
		csv := joinSeedModels(s.Models)
		if csv != "" && csv != ch.Models {
			if uerr := tx.Model(&Channel{}).Where("id = ?", ch.ID).Update("models", csv).Error; uerr != nil {
				return nil, false, fmt.Errorf("store: 补渠道 %q 的模型声明失败: %w", s.Provider, uerr)
			}
			ch.Models = csv
		}
		return &ch, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("store: 查渠道 name=%q base_url=%q 失败: %w", s.Provider, s.BaseURL, err)
	}
	// ⚠ 取地址要落在**局部变量**上：s 是 range 变量，直接 &s.Priority 在
	// Go 1.22 语义下虽然每轮独立，但语义依赖版本 —— 局部变量不依赖任何版本细节。
	base, prio, weight := s.BaseURL, int64(s.Priority), uint(s.Weight)
	ch = Channel{
		Name:        s.Provider,
		BaseURL:     &base,
		Models:      joinSeedModels(s.Models),
		Status:      ChannelStatusEnabled,
		Priority:    &prio,
		Weight:      &weight,
		CreatedTime: time.Now().Unix(),
	}
	if cerr := tx.Create(&ch).Error; cerr != nil {
		return nil, false, fmt.Errorf("store: 建渠道 %q 失败: %w", s.Provider, cerr)
	}
	return &ch, true, nil
}

// expandAbilities 是 ExpandAbilities 的**无锁**版本（供已在事务里的调用方使用）。
func expandAbilities(tx *gorm.DB, channelID int64, group string, models []string) (int, error) {
	var created int
	for _, m := range normalizeSeedModels(models) {
		ok, err := ensureAbility(tx, group, m, channelID)
		if err != nil {
			return created, err
		}
		if ok {
			created++
		}
	}
	return created, nil
}

// ensureAbility 取或建一条能力行，返回 (是否新建)。
//
// 查重**必须走结构体条件**（tx.Where(&Ability{…})）而不是裸 SQL：
// abilities 的第一列是 SQLite 关键字 group，裸写要么语法错、要么被当成列别名
// （models.go 的 Ability.Group 注释把这条钉死了）。GORM 的结构体条件会
// 按方言给标识符加引号 —— 手写 SQL 换不来这个保证。
func ensureAbility(tx *gorm.DB, group, model string, channelID int64) (bool, error) {
	key := Ability{Group: group, Model: model, ChannelID: channelID}
	var ab Ability
	err := tx.Where(&key).First(&ab).Error
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, fmt.Errorf("store: 查能力行 (%s,%s,%d) 失败: %w", group, model, channelID, err)
	}
	ab = key
	ab.Enabled = true
	if cerr := tx.Create(&ab).Error; cerr != nil {
		// 复合唯一索引 idx_ability_unique 会拦下并发/重入造成的重复插入。
		// 目标状态（这一行存在）已经达成，重查一次即可 —— 把无害竞争报成
		// seed 失败，只会给启动期加一个随机故障。
		var again Ability
		if qerr := tx.Where(&key).First(&again).Error; qerr == nil {
			return false, nil
		}
		return false, fmt.Errorf("store: 建能力行 (%s,%s,%d) 失败: %w", group, model, channelID, cerr)
	}
	return true, nil
}

// ══════════════════════════════════════════════════════════════════════════════
// Wave A1：渠道仓储（hyxapi 渠道管理后端的对等面）
//
// 上半部分是 T08 的反向 seed（缺了才建、不删）；这里是**管理路径**：
// 增 / 改 / 删 / 批量删 / 删已禁用 / 复制 / 状态 / 分页搜索 / 多 Key / 明文 key 单取。
//
// # 三条纪律（每条都有测试钉）
//
//  1. **写路径同事务维护能力行**（D3）：渠道行与它的能力行必须一起出现或一起消失。
//     本文件不自己实现展开，而是复用 ability.go 的 UpdateAbilities / DeleteAbilities ——
//     那份实现已经是 hyxapi model/ability.go 的对等物（先删后插 + 分批 OnConflict）。
//     「只写 channels 不写 abilities ⇒ 渠道存在但永远选不到」由
//     TestChannelWritePathsKeepAbilities 的正例与反例一起钉住。
//  2. **读路径一律 Omit("key")**（D1）：列表/详情/分页都不带明文；
//     需要明文只有 GetChannelKey / GetChannelKeys 两个显式入口。
//  3. **多 Key 的读-改-写要拿 per-channel 锁**（D2）：channel_info 是一整块 JSON 快照，
//     两个写者各自"读出来改一个字段再整体写回"会互相覆盖（后写的带走前写的改动）。
//     hyxapi 用 GetChannelPollingLock 解决，本仓同名同义：GetChannelLock。
//
// # 与 hyxapi 的**刻意差异**（写 handler 的人必须知道）
//
//	· 搜索不匹配 key 列：hyxapi 的 where 里有 `key = ?`，本仓刻意去掉 ——
//	  列表已经是 Omit("key") 的口径，让搜索能命中 key 等于开了一条侧信道
//	  （拿搜索框当 key 的存在性探针）。
//	· ChannelInfo.MultiKeyMode 是 string（hyxapi 是 constant.MultiKeyMode 类型别名）。
//	  本仓不引 relay 层的常量包：JSON 列里就是字符串，校验放在 handler 层。
//	· abilities 行不含 priority/weight/tag 列（D4 冻结 Ability 结构）：
//	  priority/weight 在选路时 JOIN channels 现读（ability.go 文件头第 2 条），tag 两边都没有。
// ══════════════════════════════════════════════════════════════════════════════

// ErrChannelNotFound 目标渠道不存在（删除/更新/取 key 时统一报这个）。
var ErrChannelNotFound = errors.New("store: 渠道不存在")

// ErrExternalIDTaken 外部可寻址 id 撞车（唯一约束）。
//
// 与 gorm.ErrDuplicatedKey **一起**包在返回值里（errors.Join）：调用方既能按本包
// 语义判（errors.Is(err, ErrExternalIDTaken)），也能按 GORM 惯例判
// （errors.Is(err, gorm.ErrDuplicatedKey)）—— B1 要据此把外部 id 撞车回成 409。
var ErrExternalIDTaken = errors.New("store: external_id 已被占用")

// ── 多 Key 的存储形态（D2）──────────────────────────────────────────────────
//
//  truth 只有**一列**：channels.key。形态两种，靠**首字符**区分（照 hyxapi）：
//
//	普通渠道  —— 换行分隔："sk-a\nsk-b\nsk-c"
//	Vertex    —— JSON 数组：[{"key":...}, {"key":...}] 或 ["a","b"]
//
//  其余多 Key 事实（哪个下标被禁、为什么、轮询游标）全在 channel_info JSON 列里，
//  **以 key 下标为键**。下标是数组位置 —— 所以每次改 key 列表都必须重算 size 并
//  把越界的下标删掉，否则状态会挂在一个不存在的 key 上（hyxapi 的 Update() 同样做）。

// MultiKeyMode 轮询模式下使用的常量。
//
// ⚠ 刻意只用**字符串字面量**：本仓还没有 relay 层的 MultiKeyMode 类型，
// 而 JSON 列里存的就是字符串。加类型别名要等 handler 层的校验一起做（A2）。
const (
	MultiKeyModeRandom  = "random"
	MultiKeyModePolling = "polling"
)

// ChannelInfo 的定义在 models.go（主脑冻结的 Channel 结构契约要求类型随结构体走）。
//
// 判据用**首字符 `[`**（照 hyxapi）：两种形态互斥，不需要额外标志位。
// 空串返回 nil（不是 [""]）—— "没有 key" 与 "有一把空 key" 必须能区分。
func splitChannelKeys(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, item := range arr {
				// Vertex 的每项可能是对象（{"key":"..."}）也可能是裸字符串。
				// 两种都归一成"这一项的字面 JSON"，与 hyxapi 的取法一致
				// （hyxapi 直接用 json.RawMessage 的字面量当 key）。
				v := strings.TrimSpace(string(item))
				if v == "" || v == "null" {
					continue
				}
				if strings.HasPrefix(v, "\"") {
					var s string
					if err := json.Unmarshal(item, &s); err == nil {
						v = s
					}
				}
				out = append(out, v)
			}
			if len(out) > 0 {
				return out
			}
		}
		// 不是合法 JSON 数组 ⇒ 当普通换行分隔处理（不报错：库里可能有历史脏值，
		// 报错会让整条渠道读不出来，而读不出来的表现是"渠道消失"）。
	}
	parts := strings.Split(trimmed, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// joinChannelKeys 把 key 列表写回 Key 列的**普通形态**（换行分隔）。
//
// 为什么不用 JSON：只有 Vertex 需要 JSON 数组，而那是**类型特有**的形态，
// 由调用方（Vertex 的分支）自己拼好再交给本函数/直接写 Key。
func joinChannelKeys(keys []string) string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := strings.TrimSpace(k); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, "\n")
}

// GetKeys 解析这条渠道的 key 列表（内存态；不落库）。
func (c *Channel) GetKeys() []string {
	if c == nil {
		return nil
	}
	if len(c.Keys) > 0 {
		return c.Keys
	}
	return splitChannelKeys(c.Key)
}

// SetKeys 把 key 列表写进 Key 列，并同步 ChannelInfo 的多 Key 事实。
//
// 同步做三件事（顺序不能换）：① 写 Key；② 重算 MultiKeySize；③ 截断越界下标。
func (c *Channel) SetKeys(keys []string, multiKey bool) {
	if c == nil {
		return
	}
	c.Key = joinChannelKeys(keys)
	c.Keys = splitChannelKeys(c.Key)
	c.ChannelInfo.IsMultiKey = multiKey && len(c.Keys) > 1
	c.ChannelInfo.MultiKeySize = len(c.Keys)
	c.truncateMultiKeyState()
}

// truncateMultiKeyState 把 ChannelInfo 里越界的 key 下标删掉（D2 ①）。
//
// 为什么必须有：MultiKeyStatusList 的键是**数组下标**。改一次 key 列表（少了几把）
// 之后，旧下标就指向一个不存在的 key —— 后续按它去索引 key 列表会越界，
// 而越界在 Go 里是 panic、在接口层表现为 500（hyxapi 的 Update() 因此也做这件事）。
func (c *Channel) truncateMultiKeyState() {
	if c == nil {
		return
	}
	size := c.ChannelInfo.MultiKeySize
	for idx := range c.ChannelInfo.MultiKeyStatusList {
		if idx < 0 || idx >= size {
			delete(c.ChannelInfo.MultiKeyStatusList, idx)
		}
	}
	for idx := range c.ChannelInfo.MultiKeyDisabledReason {
		if idx < 0 || idx >= size {
			delete(c.ChannelInfo.MultiKeyDisabledReason, idx)
		}
	}
	for idx := range c.ChannelInfo.MultiKeyDisabledTime {
		if idx < 0 || idx >= size {
			delete(c.ChannelInfo.MultiKeyDisabledTime, idx)
		}
	}
	if c.ChannelInfo.MultiKeyPollingIndex >= size {
		c.ChannelInfo.MultiKeyPollingIndex = 0
	}
}

// channelLocks 每条渠道一把锁（**读-改-写 channel_info 快照**用，D2 ②）。
//
// 与 hyxapi 的 channelPollingLocks 同义同名（那边叫 GetChannelPollingLock）。
// ⚠ 只保护"进程内的读-改-写"；跨进程要靠 SQL 层的即时事务（_txlock=immediate）。
var channelLocks sync.Map

// GetChannelLock 取（或建）一条渠道的进程内锁。
func GetChannelLock(channelID int64) *sync.Mutex {
	if lock, ok := channelLocks.Load(channelID); ok {
		return lock.(*sync.Mutex)
	}
	created := &sync.Mutex{}
	actual, _ := channelLocks.LoadOrStore(channelID, created)
	return actual.(*sync.Mutex)
}

// SetKeyStatus 在**已持有 GetChannelLock** 的前提下改一把 key 的状态（内存态）。
//
// 调用方拿锁 → GetChannel（带 key）→ SetKeyStatus → UpdateChannel，四步之间
// 不能被另一个写者插进来，否则后写的快照会吃掉前一个的改动。
func (c *Channel) SetKeyStatus(index, status int, reason string, now int64) error {
	if c == nil {
		return fmt.Errorf("%w：渠道为 nil", ErrBadAbility)
	}
	keys := c.GetKeys()
	if index < 0 || index >= len(keys) {
		return fmt.Errorf("store: key 下标越界: %d（共 %d 把）", index, len(keys))
	}
	if c.ChannelInfo.MultiKeyStatusList == nil {
		c.ChannelInfo.MultiKeyStatusList = map[int]int{}
	}
	c.ChannelInfo.MultiKeyStatusList[index] = status
	if status == ChannelStatusEnabled {
		delete(c.ChannelInfo.MultiKeyDisabledReason, index)
		delete(c.ChannelInfo.MultiKeyDisabledTime, index)
		return nil
	}
	if c.ChannelInfo.MultiKeyDisabledReason == nil {
		c.ChannelInfo.MultiKeyDisabledReason = map[int]string{}
	}
	if c.ChannelInfo.MultiKeyDisabledTime == nil {
		c.ChannelInfo.MultiKeyDisabledTime = map[int]int64{}
	}
	c.ChannelInfo.MultiKeyDisabledReason[index] = reason
	c.ChannelInfo.MultiKeyDisabledTime[index] = now
	return nil
}

// ── 写路径：渠道行与能力行**同事务**（D3）──────────────────────────────────

// ErrBadChannel 渠道写路径的参数不合法（缺 name / 非法 status / 缺 id）。
var ErrBadChannel = errors.New("store: 非法的渠道写操作")

// IsValidChannelStatus 渠道状态是否合法（1 启用 / 2 手动禁用 / 3 自动禁用）。
func IsValidChannelStatus(status int) bool {
	return status == ChannelStatusEnabled ||
		status == ChannelStatusManuallyDisabled ||
		status == ChannelStatusAutoDisabled
}

// GroupsOf 取这条渠道声明的分组，**以 Group 列为唯一事实来源**。
//
// # ⚠ 为什么不能让内存态 Groups 优先（wave1-a 在 B1 实测到的"幽灵能力"）
//
// Channel.Groups 是 gorm:"-" 的计算字段，由读路径（GetChannel /
// GetChannelByExternalID / fillComputed）在**读取那一刻**按当时的 Group 列填。
// 若这里写成"len(c.Groups) > 0 就先用它"，下面这条链会**静默**出错：
//
//	读出来（Groups=[default vip]）→ 调用方只改 Group 列（改成 default）
//	→ UpdateChannel → GroupsOf() 取到**旧的内存态** → 能力行按旧分组重建
//	⇒ 渠道已被移出 vip，vip 组却仍能选到它的模型：选路照旧命中，且不报任何错。
//
// 实测来源：wave1-a（B1）在 internal/channelstore 把渠道从 vip 移出后，vip 组
// 仍能选到它的模型；它在 applyChannelToRow 里同步写 Groups 规避，但**任何直接
// 调用 UpdateChannel 的调用方都会踩**。所以护栏必须在本层。
//
// 判据：**列优先**；只有列为空时才回落内存态（兼容"调用方只构造了 Groups、
// 没写 Group 列"的调用点）。UpdateChannel 入口还会再做一次同样的归一（双保险）。
func (c *Channel) GroupsOf() []string {
	if c == nil {
		return nil
	}
	if g := splitAbilityCSV(c.Group); len(g) > 0 {
		return g
	}
	if len(c.Groups) > 0 {
		return c.Groups
	}
	return nil
}

// OtherInfoMap 解析 OtherInfo（JSON 文本）。
//
// 坏值返回空 map 而不是报错：它只装 status_reason/status_time 这类人看的补充信息，
// 解析失败不该让渠道读不出来（读不出来的表现是"渠道消失"）。
func (c *Channel) OtherInfoMap() map[string]any {
	out := map[string]any{}
	if c == nil || strings.TrimSpace(c.OtherInfo) == "" {
		return out
	}
	if err := json.Unmarshal([]byte(c.OtherInfo), &out); err != nil {
		return map[string]any{}
	}
	return out
}

// SetOtherInfoMap 把 map 写回 OtherInfo（JSON 文本）。
func (c *Channel) SetOtherInfoMap(info map[string]any) {
	if c == nil {
		return
	}
	b, err := json.Marshal(info)
	if err != nil {
		return
	}
	c.OtherInfo = string(b)
}

// CreateChannel 新增一条渠道，返回自增 ID。
//
// **事务边界 = 渠道行 + 它的能力行。** 这不是风格问题：ability.go 的文件头把
// 「写 channels 不会自动产生能力行」列为不变量 3，而能力行正是选路**唯一**读的表。
// 只在事务里写一半（渠道行）的表现是「渠道存在但永远选不到」，
// 而且没有任何编译或启动错误 —— 这正是本任务要钉住的那条静默失败。
func CreateChannel(ch *Channel) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if ch == nil {
		return 0, fmt.Errorf("%w：渠道为 nil", ErrBadChannel)
	}
	if strings.TrimSpace(ch.Name) == "" {
		return 0, fmt.Errorf("%w：缺 name（name 是身份键的一半，见文件头）", ErrBadChannel)
	}
	if ch.Status == 0 {
		// 0 不是合法状态，而列默认值是 1（启用）。照列默认值走，不发明第三种语义。
		ch.Status = ChannelStatusEnabled
	}
	if !IsValidChannelStatus(ch.Status) {
		return 0, fmt.Errorf("%w：status=%d 非法（1 启用 / 2 手动禁用 / 3 自动禁用）", ErrBadChannel, ch.Status)
	}
	if ch.CreatedTime == 0 {
		ch.CreatedTime = time.Now().Unix()
	}
	// Key 列与 channel_info 的多 Key 事实必须自洽（D2）：size 永远等于 key 把数。
	ch.ChannelInfo.MultiKeySize = len(splitChannelKeys(ch.Key))
	ch.ChannelInfo.IsMultiKey = ch.ChannelInfo.IsMultiKey && ch.ChannelInfo.MultiKeySize > 0
	ch.truncateMultiKeyState()

	err := DB.Transaction(func(tx *gorm.DB) error {
		if cerr := tx.Create(ch).Error; cerr != nil {
			if isUniqueViolation(cerr) && ch.ExternalID != "" {
				return fmt.Errorf("store: 建渠道 %q 失败：external_id=%q 已被占用: %w",
					ch.Name, ch.ExternalID, errors.Join(ErrExternalIDTaken, gorm.ErrDuplicatedKey))
			}
			return fmt.Errorf("store: 建渠道 %q 失败: %w", ch.Name, cerr)
		}
		return AddAbilities(tx, ch, ch.GroupsOf()...)
	})
	if err != nil {
		return 0, err
	}
	return ch.ID, nil
}

// UpdateChannel 更新一条渠道（整行保存 + 重建能力行，同一事务）。
//
// # 三处「不传就不改」的保护（都是踩过或几乎会踩的坑）
//
//	· Key 为空 ⇒ 保留原 key。列表/详情不回显明文，所以「改个名字」的请求里
//	  必然没有 key；不保护就会把渠道的凭据清空。
//	· CreatedTime 为 0 ⇒ 保留原值（否则每次编辑都把建行时间改成今天）。
//	· Status 为 0 ⇒ 保留原状态（状态有专门的 SetChannelStatus 路径；
//	  0 不是合法状态，把它当「停用」会把一次改名变成一次停机）。
func UpdateChannel(ch *Channel) error {
	if DB == nil {
		return ErrNotOpen
	}
	if ch == nil || ch.ID == 0 {
		return fmt.Errorf("%w：更新渠道需要 id", ErrBadChannel)
	}
	if ch.Status != 0 && !IsValidChannelStatus(ch.Status) {
		return fmt.Errorf("%w：status=%d 非法", ErrBadChannel, ch.Status)
	}
	// ⚠ 以列为准（幽灵能力的第二道保险，见 GroupsOf 的注释）：内存态 Groups 可能
	// 是"读取那一刻"的旧值，调用方改了 Group 列却忘了刷新它。这里先归一，
	// 保证下面重建能力行用的是**列**里的分组。
	ch.Groups = splitAbilityCSV(ch.Group)
	// per-channel 锁：channel_info 是整块 JSON 快照，读-改-写要串行（D2 ②）。
	lock := GetChannelLock(ch.ID)
	lock.Lock()
	defer lock.Unlock()

	return DB.Transaction(func(tx *gorm.DB) error {
		var origin Channel
		if err := tx.First(&origin, "id = ?", ch.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: id=%d", ErrChannelNotFound, ch.ID)
			}
			return fmt.Errorf("store: 查渠道 id=%d 失败: %w", ch.ID, err)
		}
		// ChannelInfo 是**整块 JSON 快照**，而"从列表视图构造的整行更新"里它是零值。
		// 不保护就会被写成 {} ⇒ 多 Key 状态、禁用原因、轮询游标全丢（hyxapi 的
		// UpdateChannel 同样"Always copy the original ChannelInfo"）。
		if ch.ChannelInfo.IsMultiKey == false && ch.ChannelInfo.MultiKeySize == 0 &&
			len(ch.ChannelInfo.MultiKeyStatusList) == 0 &&
			len(ch.ChannelInfo.MultiKeyDisabledReason) == 0 &&
			len(ch.ChannelInfo.MultiKeyDisabledTime) == 0 &&
			ch.ChannelInfo.MultiKeyMode == "" {
			ch.ChannelInfo = origin.ChannelInfo
		}
		if ch.Key == "" {
			ch.Key = origin.Key
		}
		// ExternalID 同 Key/CreatedTime 一样"不传就不改"：从列表视图构造的整行更新
		// 里它是空串，若不保护就会把外部可寻址 id 抹掉 —— 而前端正是拿 ch-xxxx
		// 来回传的，抹掉等于把一个渠道从界面上"变没"（B1 的寻址立刻 404）。
		// 要**显式清空**请走 SetChannelExternalID(id, "")。
		if ch.ExternalID == "" {
			ch.ExternalID = origin.ExternalID
		}
		if ch.CreatedTime == 0 {
			ch.CreatedTime = origin.CreatedTime
		}
		if ch.Status == 0 {
			ch.Status = origin.Status
		}
		// D2 ①：改完 key 必须重算 size 并截断越界下标，否则状态挂在不存在的 key 上。
		keys := splitChannelKeys(ch.Key)
		ch.ChannelInfo.MultiKeySize = len(keys)
		if len(keys) > 1 {
			ch.ChannelInfo.IsMultiKey = true
		} else {
			// key 列没变、或真的删到剩一把：都保留「这是多 Key 渠道」的标记 ——
			// MultiKeyMode 与状态表是渠道级设置，清掉标记会把用户配的模式一起丢掉。
			ch.ChannelInfo.IsMultiKey = origin.ChannelInfo.IsMultiKey
		}
		ch.truncateMultiKeyState()
		if serr := tx.Save(ch).Error; serr != nil {
			return fmt.Errorf("store: 更新渠道 id=%d 失败: %w", ch.ID, serr)
		}
		// 先删后插（同事务）：模型/分组变了，能力行必须跟着变。
		return UpdateAbilities(tx, ch, ch.GroupsOf()...)
	})
}

// DeleteChannel 删除一条渠道（能力行与渠道行同事务）。
//
// 顺序是**先能力后渠道**：反过来的话，中途失败会留下「渠道没了但能力行还在」，
// 选路会选中一个已不存在的渠道 id。
func DeleteChannel(id int64) error {
	if DB == nil {
		return ErrNotOpen
	}
	if id == 0 {
		return fmt.Errorf("%w：删除渠道需要 id", ErrBadChannel)
	}
	lock := GetChannelLock(id)
	lock.Lock()
	defer lock.Unlock()
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := DeleteAbilities(tx, id); err != nil {
			return err
		}
		res := tx.Where("id = ?", id).Delete(&Channel{})
		if res.Error != nil {
			return fmt.Errorf("store: 删渠道 id=%d 失败: %w", id, res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("%w: id=%d", ErrChannelNotFound, id)
		}
		return nil
	})
}

// channelDeleteBatch 批量删除的分批大小（照 hyxapi BatchDeleteChannels 的 200）。
const channelDeleteBatch = 200

// DeleteChannels 批量删除（一个事务，分批 200）。返回真正删掉的渠道行数。
//
// 输入先去重：重复 id 会让"删了几条"的统计虚高，而那个数字要进审计。
func DeleteChannels(ids []int64) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	uniq := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return 0, nil
	}
	var removed int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(uniq); start += channelDeleteBatch {
			end := start + channelDeleteBatch
			if end > len(uniq) {
				end = len(uniq)
			}
			chunk := uniq[start:end]
			if err := tx.Where("channel_id IN ?", chunk).Delete(&Ability{}).Error; err != nil {
				return fmt.Errorf("store: 批量删能力行失败: %w", err)
			}
			res := tx.Where("id IN ?", chunk).Delete(&Channel{})
			if res.Error != nil {
				return fmt.Errorf("store: 批量删渠道失败: %w", res.Error)
			}
			removed += res.RowsAffected
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// DeleteDisabledChannels 清空已禁用渠道（status = 2 或 3）。返回删除行数。
//
// ⚠ 与 hyxapi 的 model.DeleteDisabledChannel 有一处**刻意的差异**：hyxapi 只删
// channels、把 abilities 留给后面的 FixAbility；本仓顺手在同一事务里也删能力行 ——
// 因为漏删的表现是"路由选中一个已不存在的渠道"，而那个故障不会自愈。
func DeleteDisabledChannels() (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	var ids []int64
	if err := DB.Model(&Channel{}).
		Where("status IN ?", []int{ChannelStatusManuallyDisabled, ChannelStatusAutoDisabled}).
		Pluck("id", &ids).Error; err != nil {
		return 0, fmt.Errorf("store: 查已禁用渠道失败: %w", err)
	}
	return DeleteChannels(ids)
}

// CopyChannel 复制一条渠道（照 hyxapi CopyChannel 的语义）。
//
//	suffix       新渠道名后缀（调用方通常给 "_复制"）
//	resetBalance true 时把 balance / used_quota / balance_updated_time 归零：
//	             复制是"再开一条"，继承旧渠道的用量会让统计与额度判断都失真。
//
// 复制会**连同明文 key 一起复制**（多 Key 整列复制）—— 这是 hyxapi 的行为，
// 也是运维要的（复制一条来改 base_url/模型，不必重贴凭据）。
func CopyChannel(id int64, suffix string, resetBalance bool) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	origin, err := getChannelWithKey(id)
	if err != nil {
		return 0, err
	}
	clone := *origin
	clone.ID = 0
	// ⚠ 外部 id **不继承**：它是唯一索引（非空值唯一），复制出来的行沿用旧值会
	// 直接撞唯一约束；而且"同一个外部 id 指向两条渠道"在 B1 的翻译层里是歧义。
	// 新行要么由调用方随后 SetChannelExternalID 分配，要么保持空串（非外部寻址）。
	clone.ExternalID = ""
	clone.Name = origin.Name + suffix
	clone.CreatedTime = time.Now().Unix()
	clone.TestTime = 0
	clone.ResponseTime = 0
	if resetBalance {
		clone.Balance = 0
		clone.UsedQuota = 0
		clone.BalanceUpdatedTime = 0
	}
	return CreateChannel(&clone)
}

// ── 状态与运维字段（同样要同步能力行）───────────────────────────────────────

// updateAbilityStatusTx 事务内的能力行启停。
//
// ability.go 的 UpdateAbilityStatus 是**非事务**便捷版；这里需要与渠道状态写在
// 同一个事务里，否则会留下「渠道已停用但能力行仍 enabled」的窗口 ——
// 那个窗口里的请求会被路由到这条渠道。
func updateAbilityStatusTx(tx *gorm.DB, channelID int64, enabled bool) error {
	if channelID == 0 {
		return fmt.Errorf("%w：channel_id 为 0（拒绝无条件下更新）", ErrBadAbility)
	}
	err := tx.Model(&Ability{}).
		Where(&Ability{ChannelID: channelID}).
		Select("enabled").
		Update("enabled", enabled).Error
	if err != nil {
		return fmt.Errorf("store: 同步能力行状态（channel=%d）失败: %w", channelID, err)
	}
	return nil
}

// SetChannelStatus 改一条渠道的状态（1 启用 / 2 手动禁用 / 3 自动禁用）。
//
// # 为什么 3（自动禁用）也允许**人工**置入

// 本函数原先只收 1/2，理由是"3 由测试/自动禁用路径写"。那条限制站不住：
//
//	· admin 的 status 端点与 channelstore 的**冻结签名**允许 3 —— 存储层单独拒绝
//	  会把"上层合法、下层报错"变成一个只在运行时才暴露的分裂；
//	· 人工把渠道标记成"自动停用"是合法运维动作（例如上游被安全团队临时封禁，
//	  先按自动停用的姿态停掉，等测试路径自己去恢复），hyxapi 的口径同样允许。
//
// ⚠ 语义完全沿用既有状态机，不新造路径：status != 1 视为"不可用"
// （abilities.enabled=false，停用保留绑定便于恢复），== 1 才置 true。
//
// 返回 changed：false = 本来就是该状态（调用方据此决定要不要审计/推送）。
// 副作用（同一事务）：① channels.status 与 other_info 的 status_reason/status_time；
// ② abilities.enabled 跟着翻转（同一事务的 updateAbilityStatusTx，与 ability.go 的
//
//	UpdateAbilityStatus 同语义 —— 只是必须和目标状态写在同一个事务里）。
func SetChannelStatus(id int64, status int, reason string) (bool, error) {
	if DB == nil {
		return false, ErrNotOpen
	}
	if !IsValidChannelStatus(status) {
		return false, fmt.Errorf("%w：非法状态 %d（1 启用 / 2 手动禁用 / 3 自动禁用；3 也允许人工置入）", ErrBadChannel, status)
	}
	lock := GetChannelLock(id)
	lock.Lock()
	defer lock.Unlock()
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var ch Channel
		if err := tx.First(&ch, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: id=%d", ErrChannelNotFound, id)
			}
			return fmt.Errorf("store: 查渠道 id=%d 失败: %w", id, err)
		}
		if ch.Status == status {
			return nil
		}
		info := ch.OtherInfoMap()
		info["status_reason"] = reason
		info["status_time"] = time.Now().Unix()
		ch.SetOtherInfoMap(info)
		ch.Status = status
		if err := tx.Save(&ch).Error; err != nil {
			return fmt.Errorf("store: 写渠道状态 id=%d 失败: %w", id, err)
		}
		if err := updateAbilityStatusTx(tx, id, status == ChannelStatusEnabled); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// SetChannelsStatus 批量改状态，返回真正发生变化的条数（照 hyxapi 的 changedCount）。
//
// 逐条走 SetChannelStatus（各自的锁与事务）：某一条失败时，已经改好的**不回滚** ——
// 与 hyxapi 的逐条循环同语义（批量停用的语义是"尽量停"，不是"全停或全不停"）。
func SetChannelsStatus(ids []int64, status int, reason string) (int, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	changed := 0
	for _, id := range ids {
		ok, err := SetChannelStatus(id, status, reason)
		if err != nil {
			return changed, err
		}
		if ok {
			changed++
		}
	}
	return changed, nil
}

// SetChannelsTag 批量打标签（tag = nil 表示清空标签）。返回影响行数。
//
// 用 map 而不是结构体更新：结构体更新会跳过零值，而 **nil 正是"清空标签"** ——
// 用结构体写会变成"什么也没做"（这类静默无操作最难查）。
func SetChannelsTag(ids []int64, tag *string) (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res := DB.Model(&Channel{}).Where("id IN ?", ids).Updates(map[string]any{"tag": tag})
	if res.Error != nil {
		return 0, fmt.Errorf("store: 批量改标签失败: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// UpdateChannelBalance 写回余额与余额时间（余额查询路径用）。
func UpdateChannelBalance(id int64, balance float64) error {
	if DB == nil {
		return ErrNotOpen
	}
	if id == 0 {
		return fmt.Errorf("%w：更新余额需要 id", ErrBadChannel)
	}
	res := DB.Model(&Channel{}).Where("id = ?", id).Updates(map[string]any{
		"balance":              balance,
		"balance_updated_time": time.Now().Unix(),
	})
	if res.Error != nil {
		return fmt.Errorf("store: 写余额 id=%d 失败: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: id=%d", ErrChannelNotFound, id)
	}
	return nil
}

// ── 读路径：一律 Omit("key")（D1）──────────────────────────────────────────
//
// 明文只有两个显式入口：GetChannelKey（原始列）/ GetChannelKeys（拆成列表）。
// 列表、分页、详情、搜索都不带它 —— 这是本仓从"只存掩码"改成"存明文"之后
// **唯一**挡住"顺手把凭据发出去"的那道闸。

// fillComputed 回填计算字段（Keys / Groups，永不落库）。
//
// key 被 Omit 时 Keys 自然是 nil —— 那是对的：没读过的凭据不该在内存里留一份。
func (c *Channel) fillComputed() {
	if c == nil {
		return
	}
	c.Groups = splitAbilityCSV(c.Group)
	c.Keys = splitChannelKeys(c.Key)
}

// getChannel 是读路径的唯一实现（withKey 决定要不要那一列）。
func getChannel(id int64, withKey bool) (*Channel, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	if id == 0 {
		return nil, fmt.Errorf("%w：取渠道需要 id", ErrBadChannel)
	}
	q := DB
	if !withKey {
		q = q.Omit("key")
	}
	var ch Channel
	if err := q.First(&ch, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: id=%d", ErrChannelNotFound, id)
		}
		return nil, fmt.Errorf("store: 读渠道 id=%d 失败: %w", id, err)
	}
	ch.fillComputed()
	return &ch, nil
}

// GetChannel 取一条渠道，**不含明文 key**。
func GetChannel(id int64) (*Channel, error) { return getChannel(id, false) }

// getChannelWithKey 内部用：带明文 key 的取法。
//
// 谁需要它：复制渠道（要复制凭据）、状态与多 Key 的读-改-写（整块 JSON 要写回，
// 而 Save 会把 key 列一起写 —— 不先读出来就会被写成空）。
func getChannelWithKey(id int64) (*Channel, error) { return getChannel(id, true) }

// GetChannelKey 取一条渠道的**明文** Key（多 Key 时是原始列：换行分隔或 JSON 数组）。
//
// ⚠ 这是"我知道我在做什么"的入口：只在真正要用凭据时调（测试渠道、余额查询、轮换）。
// 对应 hyxapi 的 POST /api/channel/:id/key（那条路由额外挂了 RootAuth + 限流 +
// 安全验证）—— 本层的等价物是调用方必须显式调用它。
func GetChannelKey(id int64) (string, error) {
	ch, err := getChannel(id, true)
	if err != nil {
		return "", err
	}
	return ch.Key, nil
}

// GetChannelKeys 取一条渠道的 key 列表（普通渠道 = 一把；多 Key = 多把）。
func GetChannelKeys(id int64) ([]string, error) {
	ch, err := getChannel(id, true)
	if err != nil {
		return nil, err
	}
	return splitChannelKeys(ch.Key), nil
}

// ── 列表 / 分页 / 搜索（照 hyxapi 的列表口径）───────────────────────────────

const (
	// ChannelPageDefaultSize 分页默认条数（照 common.ItemsPerPage 的 10）。
	ChannelPageDefaultSize = 10
	// ChannelPageMaxSize 单页上限（照 hyxapi 的 100，防"page_size=100000"打库）。
	ChannelPageMaxSize = 100
	// ChannelTypeAny / ChannelStatusAny 过滤器"不过滤"的哨兵值。
	//
	// ⚠ 为什么必须是负数而不是 0：Status 的 0 在 hyxapi 口径里是**已禁用**
	// （status != 1），Type 的 0 是一个真实类型。拿 0 当"不过滤"会把
	// "查已禁用"变成"查全部"。
	ChannelTypeAny   = -1
	ChannelStatusAny = -1
)

// ChannelQuery 渠道列表/搜索的过滤条件（零值不是"全部"，见上面哨兵值的说明）。
type ChannelQuery struct {
	// Keyword 关键词：匹配 id（整数时）/ name / base_url。**刻意不匹配 key**。
	Keyword string
	// Group 分组精确匹配（channels.group 列）。
	Group string
	// Model 模型名（channels.models 列 LIKE 匹配，逗号分隔里的子串）。
	Model string
	// Status >0 该状态；==0 已禁用（status != 1）；<0 不过滤（ChannelStatusAny）。
	Status int
	// Type >=0 该类型；<0 不过滤（ChannelTypeAny）。
	Type int
	// Page 从 1 开始；PageSize 默认 10、上限 100。
	Page     int
	PageSize int
}

// ChannelPage 一页渠道 + 总数 + 各类型计数（照 hyxapi 的 items/total/type_counts）。
type ChannelPage struct {
	Items    []Channel
	Total    int64
	Page     int
	PageSize int
	// TypeCounts 按 type 分组的计数。**不受 Type 过滤影响**（照 hyxapi：
	// 类型计数的用途正是"让用户看到还有哪些类型可选"，把它按当前类型筛掉就没意义了）。
	TypeCounts map[int]int64
}

// normalized 归一化分页参数（默认 10 / 上限 100 / 页码从 1 起）。
func (q ChannelQuery) normalized() ChannelQuery {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize <= 0 {
		q.PageSize = ChannelPageDefaultSize
	}
	if q.PageSize > ChannelPageMaxSize {
		q.PageSize = ChannelPageMaxSize
	}
	return q
}

// applyChannelQuery 把过滤条件下推到 query。
//
// includeType=false 用于 type_counts：那一项的用途是"按当前其它条件看类型分布"，
// 所以**不带** type 过滤（照 hyxapi 的 countQuery 传 type=-1）。
func applyChannelQuery(db *gorm.DB, q ChannelQuery, includeType bool) *gorm.DB {
	if g := strings.TrimSpace(q.Group); g != "" {
		// 结构体条件让 GORM 给关键字 group 加引号（裸 SQL 里它会歧义）。
		db = db.Where(&Channel{Group: g})
	}
	switch {
	case q.Status > 0:
		db = db.Where("status = ?", q.Status)
	case q.Status == 0:
		// 照 hyxapi applyChannelStatusFilter：0 = 已禁用（status != 1）。
		db = db.Where("status <> ?", ChannelStatusEnabled)
	}
	if includeType && q.Type >= 0 {
		db = db.Where("type = ?", q.Type)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		// ⚠ 刻意**不**匹配 key 列（hyxapi 的 where 里有 `key = ?`）：
		// 列表已是 Omit("key") 的口径，让搜索能命中 key 等于开了一条侧信道
		// （拿搜索框当 key 的存在性探针）。
		if id, err := strconv.ParseInt(kw, 10, 64); err == nil {
			db = db.Where("(id = ? OR name LIKE ? OR base_url LIKE ?)", id, like, like)
		} else {
			db = db.Where("(name LIKE ? OR base_url LIKE ?)", like, like)
		}
	}
	if m := strings.TrimSpace(q.Model); m != "" {
		db = db.Where("models LIKE ?", "%"+m+"%")
	}
	return db
}

// ListChannelPage 分页读渠道（**不含明文 key**）+ 总数 + 各类型计数。
//
// 顺序 = priority DESC, id DESC（与 hyxapi 的列表口径一致：优先级高的先看，
// 同级按新→旧）。
func ListChannelPage(q ChannelQuery) (ChannelPage, error) {
	if DB == nil {
		return ChannelPage{}, ErrNotOpen
	}
	q = q.normalized()
	out := ChannelPage{Page: q.Page, PageSize: q.PageSize, TypeCounts: map[int]int64{}}
	if err := applyChannelQuery(DB.Model(&Channel{}), q, true).Count(&out.Total).Error; err != nil {
		return ChannelPage{}, fmt.Errorf("store: 数渠道失败: %w", err)
	}
	if err := applyChannelQuery(DB.Model(&Channel{}), q, true).
		Order("COALESCE(priority, 0) DESC, id DESC").
		Limit(q.PageSize).
		Offset((q.Page - 1) * q.PageSize).
		Omit("key").
		Find(&out.Items).Error; err != nil {
		return ChannelPage{}, fmt.Errorf("store: 分页读渠道失败: %w", err)
	}
	for i := range out.Items {
		out.Items[i].fillComputed()
	}
	type typeCountRow struct {
		Type  int64 `gorm:"column:type"`
		Count int64 `gorm:"column:count"`
	}
	var rows []typeCountRow
	if err := applyChannelQuery(DB.Model(&Channel{}), q, false).
		Select("type, count(*) as count").
		Group("type").
		Scan(&rows).Error; err != nil {
		return ChannelPage{}, fmt.Errorf("store: 统计渠道类型失败: %w", err)
	}
	for _, r := range rows {
		out.TypeCounts[int(r.Type)] = r.Count
	}
	return out, nil
}

// SearchChannelsByKeyword 不分页地按关键词/模型搜索（照 hyxapi 的 /search）。
//
// **不含明文 key**；顺序与分页列表一致。
func SearchChannelsByKeyword(q ChannelQuery) ([]Channel, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var out []Channel
	if err := applyChannelQuery(DB.Model(&Channel{}), q, true).
		Order("COALESCE(priority, 0) DESC, id DESC").
		Omit("key").
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("store: 搜索渠道失败: %w", err)
	}
	for i := range out {
		out[i].fillComputed()
	}
	return out, nil
}

// ListChannelTags 读回全部非空标签（去重、字典序）—— 供标签批量操作的下拉。
func ListChannelTags() ([]string, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var tags []string
	if err := DB.Model(&Channel{}).
		Where("tag IS NOT NULL AND tag <> ''").
		Distinct().
		Order("tag ASC").
		Pluck("tag", &tags).Error; err != nil {
		return nil, fmt.Errorf("store: 读渠道标签失败: %w", err)
	}
	return tags, nil
}

// ── 旧列一次性回填（D5：只加不删 ≠ 可以丢数据）─────────────────────────────

// backfillLegacyChannelColumns 把旧列 channels.models_csv 的值搬进新列 channels.models。
//
// # 为什么必须有这一步
//
// Wave A1 把模型声明从 models_csv 换到 models（本仓冻结契约），而 AutoMigrate
// **只加不删** —— 老库升级后两列并存，新列全是空串。少了这一步的表现是：
// 老部署的模型声明**静默**丢失 ⇒ 能力行展开不出来 ⇒「渠道存在但永远选不到」，
// 而且没有任何报错（这正是本任务要钉住的那类失败）。
//
// # 幂等与边界
//
//	· 只在 models 为空时搬（WHERE models IS NULL OR models = ''）⇒ 跑第二次 0 行；
//	· 旧库没有 models_csv 列（全新库）时直接跳过（HasColumn 判定）；
//	· 只搬列，不删旧列 —— 运维要能对照，回滚也不需要重新迁。
func backfillLegacyChannelColumns() (int64, error) {
	if DB == nil {
		return 0, ErrNotOpen
	}
	if !DB.Migrator().HasColumn(&Channel{}, "models_csv") {
		return 0, nil
	}
	res := DB.Exec("UPDATE channels SET models = models_csv " +
		"WHERE (models IS NULL OR models = '') AND models_csv IS NOT NULL AND models_csv <> ''")
	if res.Error != nil {
		return 0, fmt.Errorf("store: 回填 channels.models 失败: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// BackfillLegacyChannelColumns 是上面那个函数的**显式入口**（运维/排障用）。
//
// Migrate 已经会自动调一次；这个导出只为"迁移后想单独再跑一遍看影响行数"的场景。
func BackfillLegacyChannelColumns() (int64, error) { return backfillLegacyChannelColumns() }

// ── 外部可寻址 id（task-60 / B1 前置）──────────────────────────────────────

// ensureChannelExternalIDIndex 建 channels.external_id 的**部分唯一索引**。
//
// # 为什么不能用 gorm 的 uniqueIndex tag（这是本列最容易做错的一步）

// SQLite 的**普通** UNIQUE 索引把多个空串视为重复（” = ”）：`not null;default:”`
// + uniqueIndex 的组合下，**第二行** external_id 为空的渠道就会 `UNIQUE constraint failed`。
// 而本仓有 13 条 seed 渠道、以及所有内部创建的渠道，external_id 全是空串 ——
// 那意味着 SeedChannels 从第二行起直接失败。
//
// ⚠ 这条结论**有可执行证据**：`TestChannelExternalIDEmptySemantics` 建一张带**普通**
// UNIQUE 的 scratch 表，断言「第一行空串成功、第二行空串报 UNIQUE 失败」，并断言本表
// 的多条空串 external_id 共存。要动这一列/这条索引，先读那条测试。
//
// 所以这里手动建一条**只对非空值**生效的唯一索引：
//
//	CREATE UNIQUE INDEX idx_channel_external_id ON channels(external_id) WHERE external_id <> ''
//
// 效果：① 空串可以有任意多行（非外部寻址）；② 非空值由**库**保证唯一（并发安全，
// 不靠应用层判重）；③ 字段类型保持 string —— B1 的调用点是 ch.ExternalID = "ch-x"，
// 用 *string 会逼它到处取地址。
//
// 幂等：IF NOT EXISTS，每次 Migrate 都执行；AutoMigrate 不认这条索引也不会删它。
func ensureChannelExternalIDIndex() error {
	if DB == nil {
		return ErrNotOpen
	}
	if !DB.Migrator().HasColumn(&Channel{}, "external_id") {
		// 表还没建（AutoMigrate 之前调用）时不该在这里建索引：让 Migrate 的顺序决定。
		return nil
	}
	if err := DB.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_external_id " +
		"ON channels(external_id) WHERE external_id <> ''").Error; err != nil {
		return fmt.Errorf("store: 建 channels.external_id 部分唯一索引失败: %w", err)
	}
	return nil
}

// 复用 user.go 的 isUniqueViolation（它按错误文本判，理由与 db.go 的 gormConfig 未开
// TranslateError 有关 —— 见那里的注释）。渠道写路径把它翻成两个都能判的哨兵：
// ErrExternalIDTaken（本包语义）与 gorm.ErrDuplicatedKey（GORM 惯例）。

// GetChannelByExternalID 按外部可寻址 id（形如 ch-9f3a1b2c）取一条渠道。
//
// 三态返回值是**刻意**的（B1 要用它区分"不存在"与"库坏了"）：
//
//	找到     → (*Channel, true,  nil)
//	没找到   → (nil,      false, nil)
//	库/参数错 → (nil,      false, error)
//
// ⚠ s == "" 时直接 (nil,false,nil)：空串在库里表示"非外部寻址"，拿它去查要么
// 命中一大堆（相等比较）要么全不中，两种都不是调用方想要的语义。
// ⚠ 与其它读路径同口径：**Omit("key")**，这一路也不带出明文凭据。
func GetChannelByExternalID(s string) (*Channel, bool, error) {
	if DB == nil {
		return nil, false, ErrNotOpen
	}
	if strings.TrimSpace(s) == "" {
		return nil, false, nil
	}
	var ch Channel
	if err := DB.Omit("key").Where("external_id = ?", s).First(&ch).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("store: 按 external_id=%q 读渠道失败: %w", s, err)
	}
	ch.fillComputed()
	return &ch, true, nil
}

// SetChannelExternalID 显式写/清空外部可寻址 id（空串 = 清空 ⇒ 非外部寻址）。
//
// # 为什么除了 UpdateChannel 还要有它
//
// UpdateChannel 的保护规则是"空串 = 不改"（防列表视图的整行更新把外部 id 抹掉），
// 于是**清空**这件事就没有入口了。把"清空"做成一个显式动作，两种意图就都能表达：
//
//	ch.ExternalID = "ch-x"; UpdateChannel(ch)   ⇒ 补写（回填场景）
//	SetChannelExternalID(id, "")                ⇒ 显式清空
//
// 非空值的唯一性由 idx_channel_external_id 保证；撞了就返回 gorm.ErrDuplicatedKey
// （SQLite 的 SQLITE_CONSTRAINT_UNIQUE 由驱动翻译），调用方据此回 409 之类。
func SetChannelExternalID(id int64, externalID string) (bool, error) {
	if DB == nil {
		return false, ErrNotOpen
	}
	if id == 0 {
		return false, fmt.Errorf("%w：设置 external_id 需要 id", ErrBadChannel)
	}
	lock := GetChannelLock(id)
	lock.Lock()
	defer lock.Unlock()
	var ch Channel
	if err := DB.First(&ch, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, fmt.Errorf("%w: id=%d", ErrChannelNotFound, id)
		}
		return false, fmt.Errorf("store: 读渠道 id=%d 失败: %w", id, err)
	}
	if ch.ExternalID == externalID {
		return false, nil
	}
	res := DB.Model(&Channel{}).Where("id = ?", id).Update("external_id", externalID)
	if res.Error != nil {
		if isUniqueViolation(res.Error) && externalID != "" {
			return false, fmt.Errorf("store: external_id=%q 已被别的渠道占用: %w",
				externalID, errors.Join(ErrExternalIDTaken, gorm.ErrDuplicatedKey))
		}
		return false, fmt.Errorf("store: 写 external_id（id=%d）失败: %w", id, res.Error)
	}
	return res.RowsAffected > 0, nil
}
