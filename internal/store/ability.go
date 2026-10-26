// ability.go abilities 表：**维护**（增 / 改 / 删 / 全量重建）与**选路查询**。
//
// # abilities 是什么
//
// 它是「模型 ↔ 渠道」的绑定索引，只回答两个问题：
//
//	这个模型由哪些渠道提供 → WHERE group = ? AND model = ? AND enabled = 1
//	这个渠道能提供哪些模型 → WHERE channel_id = ?
//
// # 三条关键不变量（都有测试守着）
//
//  1. **一行一条绑定**：一条渠道产生 len(models) × len(groups) 行
//     （空白项与重复项先去掉，见 abilityRowsOf）。
//  2. **派生但权威**：abilities 是路由索引 —— 上层选渠道**只查它**，
//     不再回表扫 channels。
//  3. **写 channels 表不会自动产生能力行**：必须显式调 AddAbilities / UpdateAbilities。
//     反例守卫见 ability_test.go 的 TestAbilityRoutingIgnoresChannelsWithoutAbilityRows；
//     它手工插一行 channels 之后断言选路查询**返回空** —— 这条不变量一旦被「顺手补上」，
//     那条测试会红。
//
// # 与本仓既有 channel.go 的分工（为什么另起一个文件）
//
// channel.go 的 ExpandAbilities / ensureAbility 走的是**seed 语义**：缺了才建、不删。
// 本文件是 hyxapi model/ability.go 的对等物：**先删后插的整渠道重建** + 分批
// OnConflict DoNothing。两者写同一张表、同一套 (group, model, channel_id) 唯一键，
// 不会互相打架；要重建时用本文件，要补行时用 channel.go 那套。
//
// # 两处 schema 事实（照着写，别猜）
//
//  1. **模型与分组都在渠道行上**：`ch.Models`（列 models_csv，逗号分隔）与
//     `ch.Group`（逗号分隔，默认 default）。AddAbilities 读的就是它们；
//     额外的 groups... 入参是**覆盖**（例如按某个请求分组单独重建索引），不传就用 ch.Group。
//
//  2. **Ability 表没有 priority / weight / tag 三列**（models.go 的 Ability 只有
//     group / model / channel_id / enabled）。所以 hyxapi 那句「Priority/Weight/Tag 从
//     channel 继承」在本仓不落列：**不冗余存**，选路查询时从 channels 行 JOIN 现读
//     （channels.priority / channels.weight）。好处是改优先级不会与 abilities 漂移；
//     tag 在 abilities 这一层没有意义（它只影响"谁能服务"，不影响"发到哪"）。
//     若将来要 denormalize 进 abilities，那是 models.go 的改表，不在这里偷偷加列。
//
//  3. Enabled = (channels.status == ChannelStatusEnabled)（channel.go 的 1）。
package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// abilityInsertBatch 一批插入多少行。
//
// 50 与 hyxapi 的 lo.Chunk(abilities, 50) 对齐：SQLite 的变量上限（默认 999）会随
// 列数摊薄，一条 INSERT 塞几百行会在某个列数上直接报 too many SQL variables。
// 分批 + OnConflict DoNothing 让重入变成无害操作。
const abilityInsertBatch = 50

// ErrBadAbility 能力查询/维护的参数不合法（空 group、channel_id 为 0 之类）。
//
// 单独一个 error 是为了把「参数不对」与「库没开」（ErrNotOpen）、「SQL 失败」分开 ——
// 调用方对三者的处置不同：前者是 bug、中者是启动顺序、后者要看日志。
var ErrBadAbility = errors.New("store: 非法的能力行操作")

// AbilityCandidate 一条能服务 (group, model) 的候选渠道。
//
// Priority / Weight **来自 channels 行**（JOIN 现读），不是 abilities 自己的列 ——
// 见文件头 schema 事实第 2 条。上层拿到之后做「按 priority 分桶 + 桶内按 weight
// 加权随机」；**本层不做随机挑选**（规划层保持纯，随机只留在一个地方，
// 与 internal/channelroute 的分工一致）。
type AbilityCandidate struct {
	ChannelID int64 `gorm:"column:channel_id"`
	Priority  int   `gorm:"column:priority"`
	Weight    int   `gorm:"column:weight"`
}

// abilityRowsOf 展开一条渠道的 (models × groups) 笛卡尔积。
//
// 去重键是 (group, model) —— 与 abilities 的复合唯一索引 idx_ability_unique 同口径；
// 重复项在这一步就去掉，不要让库去撞唯一键（撞了虽然被 DoNothing 吞掉，但会白写一批）。
func abilityRowsOf(ch *Channel, override []string) []Ability {
	models := splitAbilityCSV(ch.Models)
	gs := abilityGroupsOf(ch, override)
	out := make([]Ability, 0, len(models)*len(gs))
	seen := make(map[string]struct{}, len(models)*len(gs))
	enabled := ch.Status == ChannelStatusEnabled
	for _, m := range models {
		for _, g := range gs {
			k := g + "|" + m
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, Ability{Group: g, Model: m, ChannelID: ch.ID, Enabled: enabled})
		}
	}
	return out
}

// splitAbilityCSV 把逗号分隔的列拆成非空、去重、保序的切片。
func splitAbilityCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, raw := range parts {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// abilityGroupsOf 解析这次要写哪些分组：覆盖入参 > ch.Group > 唯一默认分组。
//
// 三级回落的理由：调用方显式给了分组就听它的；否则听渠道行（ch.Group 默认 default）；
// 渠道行也是空（老数据）时落到 AbilityDefaultGroup —— 绝不返回空切片，
// 否则"这条渠道没有分组"会静默变成"一行能力都不建"。
func abilityGroupsOf(ch *Channel, override []string) []string {
	if len(override) > 0 {
		return normalizeAbilityGroups(override)
	}
	if gs := splitAbilityCSV(ch.Group); len(gs) > 0 {
		return gs
	}
	return []string{AbilityDefaultGroup}
}

// normalizeAbilityGroups 清洗分组入参：全空 ⇒ 默认分组。
func normalizeAbilityGroups(groups []string) []string {
	out := make([]string, 0, len(groups))
	seen := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		v := strings.TrimSpace(g)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return []string{AbilityDefaultGroup}
	}
	return out
}

// AddAbilities 把渠道的 (models × groups) 写进 abilities（**缺了才建**）。
//
// tx 可以为 nil（用包级 DB）。分批 50 条 + OnConflict DoNothing ⇒ 可重复调用：
// 第二次什么也不加，行数不变（幂等）。
//
// 注意：本函数**不删**已有行。要「整渠道重建」用 UpdateAbilities。
func AddAbilities(tx *gorm.DB, ch *Channel, groups ...string) error {
	if DB == nil {
		return ErrNotOpen
	}
	if ch == nil {
		return fmt.Errorf("%w：渠道为 nil", ErrBadAbility)
	}
	if ch.ID == 0 {
		return fmt.Errorf("%w：channel_id 为 0（未落库的渠道不能展开能力行）", ErrBadAbility)
	}
	rows := abilityRowsOf(ch, groups)
	if len(rows) == 0 {
		return nil
	}
	use := DB
	if tx != nil {
		use = tx
	}
	for start := 0; start < len(rows); start += abilityInsertBatch {
		end := start + abilityInsertBatch
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		if err := use.Clauses(clause.OnConflict{DoNothing: true}).Create(&chunk).Error; err != nil {
			return fmt.Errorf("store: 写能力行失败（channel=%d，第 %d 批 %d 行）: %w",
				ch.ID, start/abilityInsertBatch+1, len(chunk), err)
		}
	}
	return nil
}

// DeleteAbilities 删掉一条渠道的全部能力行。
//
// ⚠ channel_id 为 0 时**报错而不是放行**：结构体条件会跳过零值字段，
// 条件一旦为空，这条 DELETE 就会变成「删全表」。静默清空路由索引是最坏的一种失败，
// 所以这里显式挡住。
func DeleteAbilities(tx *gorm.DB, channelID int64) error {
	if DB == nil {
		return ErrNotOpen
	}
	if channelID == 0 {
		return fmt.Errorf("%w：channel_id 为 0（拒绝无条件删除）", ErrBadAbility)
	}
	use := DB
	if tx != nil {
		use = tx
	}
	if err := use.Where(&Ability{ChannelID: channelID}).Delete(&Ability{}).Error; err != nil {
		return fmt.Errorf("store: 删能力行（channel=%d）失败: %w", channelID, err)
	}
	return nil
}

// DeleteAllAbilities 清空 abilities 表（FixAllAbilities 的第一步；测试也用它造场景）。
func DeleteAllAbilities() error {
	if DB == nil {
		return ErrNotOpen
	}
	// 用裸 DELETE 而不是 GORM 的 Delete：后者对无条件删除要 AllowGlobalUpdate，
	// 而这里「清空」就是本意（调用方是 FixAllAbilities 或测试）。
	if err := DB.Exec("DELETE FROM abilities").Error; err != nil {
		return fmt.Errorf("store: 清空 abilities 失败: %w", err)
	}
	return nil
}

// UpdateAbilityStatus 只改 enabled（不动行集合）。
//
// 渠道被停用/恢复时调它：**保留** (group, model) 绑定，只翻转可用性 ——
// 这样恢复渠道不需要重建索引，历史绑定也不会丢。
func UpdateAbilityStatus(channelID int64, enabled bool) error {
	if DB == nil {
		return ErrNotOpen
	}
	if channelID == 0 {
		return fmt.Errorf("%w：channel_id 为 0（拒绝无条件下更新）", ErrBadAbility)
	}
	err := DB.Model(&Ability{}).
		Where(&Ability{ChannelID: channelID}).
		Select("enabled").
		Update("enabled", enabled).Error
	if err != nil {
		return fmt.Errorf("store: 同步能力行状态（channel=%d）失败: %w", channelID, err)
	}
	return nil
}

// UpdateAbilities **先删后插**（同一事务）：渠道的模型/分组变了用这一条。
//
// 为什么必须同事务：删完还没插就被别的读看到，会有一段「这条渠道什么模型都不服务」的窗口，
// 而路由查询正是按 abilities 找人的 —— 那个窗口里的请求会得到 no_channel_for_model。
// tx 为 nil 时本函数自建事务（hyxapi 同形，含 panic 回滚）。
func UpdateAbilities(tx *gorm.DB, ch *Channel, groups ...string) error {
	if DB == nil {
		return ErrNotOpen
	}
	if ch == nil || ch.ID == 0 {
		return fmt.Errorf("%w：更新能力行需要已落库的渠道（channel_id 为 0）", ErrBadAbility)
	}
	if tx != nil {
		if err := DeleteAbilities(tx, ch.ID); err != nil {
			return err
		}
		return AddAbilities(tx, ch, groups...)
	}
	return DB.Transaction(func(t *gorm.DB) error {
		if err := DeleteAbilities(t, ch.ID); err != nil {
			return err
		}
		return AddAbilities(t, ch, groups...)
	})
}

// fixAbilityLock 让全表重建串行化：两个并发的 FixAllAbilities 会互相看到对方
// 「清空之后、重建之中」的半成品，结果是行数随机。TryLock 失败即返回错误而不是排队 ——
// 排队只会让第二个调用者在一分钟后拿到同样的结果，不如让它立刻知道有人在跑。
var fixAbilityLock sync.Mutex

// FixAllAbilities 全表重建：清空 abilities，再按 channels 逐条重建。
//
// 返回 (成功条数, 失败条数, error)：单条渠道失败**不**中断整体（继续修别的），
// 失败数交给调用方决定是打日志还是报警。
//
// 幂等：跑第二次得到同样的行集合。
func FixAllAbilities() (fixed, failed int, err error) {
	if DB == nil {
		return 0, 0, ErrNotOpen
	}
	if !fixAbilityLock.TryLock() {
		return 0, 0, errors.New("store: 已有一个能力行重建在运行（FixAllAbilities 是串行的）")
	}
	defer fixAbilityLock.Unlock()

	if err := DeleteAllAbilities(); err != nil {
		return 0, 0, err
	}
	channels, err := ListChannels()
	if err != nil {
		return 0, 0, err
	}
	for i := range channels {
		ch := channels[i]
		if aerr := AddAbilities(nil, &ch); aerr != nil {
			failed++
			continue
		}
		fixed++
	}
	return fixed, failed, nil
}

// AbilityCandidates 选路查询：谁能在 group 里服务 model。
//
// 排序 = channels.priority DESC, channels.weight DESC, abilities.channel_id ASC。
// 第三条是**稳定化**用的：同优先级同权重时不能让两次查询给出不同顺序，
// 否则加权随机的输入本身就在抖（那会让「同一个请求两次走不同渠道」变成不可复现的 bug）。
//
// 只查 enabled=true 的行 —— 停用渠道的能力行仍留在表里（便于恢复），靠这一条过滤。
//
// ⚠ 条件用 clause.Eq + clause.Column 而不是裸 SQL：abilities 的第一列是 SQLite 关键字
// group，裸写要么语法错、要么被当成列别名（models.go 的 Ability.Group 注释 +
// channel.go 的 ensureAbility 注释把这条钉死了）。clause.Column 会按方言加引号。
//
// COALESCE 是防 NULL 的：priority/weight 是可空语义（*int64 / *uint），
// 扫描到裸 int 上遇到 NULL 会直接报错，而「没填权重」是合法状态。
func AbilityCandidates(group, model string) ([]AbilityCandidate, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	g := strings.TrimSpace(group)
	if g == "" {
		g = AbilityDefaultGroup
	}
	m := strings.TrimSpace(model)
	if m == "" {
		return nil, fmt.Errorf("%w：模型名为空", ErrBadAbility)
	}
	var out []AbilityCandidate
	err := DB.Table("abilities").
		Select("abilities.channel_id AS channel_id, " +
			"COALESCE(channels.priority, 0) AS priority, " +
			"COALESCE(channels.weight, 0) AS weight").
		Joins("JOIN channels ON channels.id = abilities.channel_id").
		Where(clause.Eq{Column: clause.Column{Table: "abilities", Name: "group"}, Value: g}).
		Where(clause.Eq{Column: clause.Column{Table: "abilities", Name: "model"}, Value: m}).
		Where(clause.Eq{Column: clause.Column{Table: "abilities", Name: "enabled"}, Value: true}).
		Order("channels.priority DESC, channels.weight DESC, abilities.channel_id ASC").
		Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("store: 选路查询 (group=%s, model=%s) 失败: %w", g, m, err)
	}
	return out, nil
}

// AbilityCountByChannel 每条渠道各有多少能力行（守卫与对账用）。
//
// 返回的 map 只含**有行的**渠道 —— 一条渠道一行都没有时它不在 map 里（读作 0）。
func AbilityCountByChannel() (map[int64]int, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	type row struct {
		ChannelID int64 `gorm:"column:channel_id"`
		N         int   `gorm:"column:n"`
	}
	var rows []row
	err := DB.Model(&Ability{}).
		Select("channel_id, COUNT(*) AS n").
		Group("channel_id").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("store: 统计每渠道能力行失败: %w", err)
	}
	out := make(map[int64]int, len(rows))
	for _, r := range rows {
		out[r.ChannelID] = r.N
	}
	return out, nil
}

// AbilityRowsFor 读回一条渠道的全部能力行（测试、守卫、排障用）。
//
// 顺序 = id 升序 = 插入顺序 = (model × group) 的枚举顺序。
// 不按 group 排序：group 是关键字，ORDER BY 里裸写它同样有歧义风险。
func AbilityRowsFor(channelID int64) ([]Ability, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	if channelID == 0 {
		return nil, fmt.Errorf("%w：channel_id 为 0（拒绝无条件查询）", ErrBadAbility)
	}
	var out []Ability
	err := DB.Where(&Ability{ChannelID: channelID}).Order("id ASC").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("store: 读能力行（channel=%d）失败: %w", channelID, err)
	}
	return out, nil
}

// AbilityRouteIndex 报告「这条渠道有没有路由索引」（供上层判断要不要补建）。
//
// 存在的理由：写 channels 而不建 abilities 是本仓最容易犯的错（见文件头不变量 3），
// 把它做成一个可查询的事实，守卫与界面都能直接问。
func AbilityRouteIndex(channelID int64) (bool, error) {
	rows, err := AbilityRowsFor(channelID)
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}
