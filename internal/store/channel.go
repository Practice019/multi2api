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
// 只回填 ModelsCSV。T11 的"二次启动不重复 seed"因此不需要另写一条路径。
package store

import (
	"errors"
	"fmt"
	"strings"
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
	ModelsCSV string
	Status    int
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
				Source: s.Source, ModelsCSV: ch.ModelsCSV, Status: ch.Status,
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
func ListChannels() ([]Channel, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	var out []Channel
	if err := DB.Order("id ASC").Find(&out).Error; err != nil {
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

// joinSeedModels 把模型名拼成 channels.models_csv（列形态对齐参考项目）。
func joinSeedModels(models []string) string {
	return strings.Join(normalizeSeedModels(models), ",")
}

// ensureChannel 取或建一条渠道行，返回 (渠道, 是否新建)。
//
// 已存在时**只补 ModelsCSV**：
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
		if csv != "" && csv != ch.ModelsCSV {
			if uerr := tx.Model(&Channel{}).Where("id = ?", ch.ID).Update("models_csv", csv).Error; uerr != nil {
				return nil, false, fmt.Errorf("store: 补渠道 %q 的模型声明失败: %w", s.Provider, uerr)
			}
			ch.ModelsCSV = csv
		}
		return &ch, false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("store: 查渠道 name=%q base_url=%q 失败: %w", s.Provider, s.BaseURL, err)
	}
	ch = Channel{
		Name:      s.Provider,
		BaseURL:   s.BaseURL,
		ModelsCSV: joinSeedModels(s.Models),
		Status:    ChannelStatusEnabled,
		Priority:  s.Priority,
		Weight:    s.Weight,
		CreatedAt: time.Now().Unix(),
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
