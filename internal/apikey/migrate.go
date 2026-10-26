// migrate.go P3.5：把存量 key 的 Models 白名单迁移成**分组**（幂等）。
//
// # 为什么需要迁移（PLAN-groups.md Q3）
//
// P2 之后「这把 key 能用哪些模型」有两个来源：已废弃的 Key.Models 白名单，
// 和将要取代它的 Key.Groups。两者语义完全重复，并存必然不一致 —— 改了一边
// 忘改另一边，而排查时要查两处。
//
// 但**不能直接删白名单**：存量 apikeys.json 里全是 Models、没有 Groups。
// 直接删等于一次升级把所有老 key 的限制静默解开（权限被放大，且用户看不出）。
// 所以走迁移：把每把"只有白名单"的 key 转成"指向一个分组"，
// 然后清空白名单 —— 升级后**能用的模型一模一样**，但配置收敛到一处。
//
// # 为什么分成三步、而不是一步改完
//
// ① 先建分组（可能撞名，要重试）
// ② 再改 key（写回 Groups + 清空 Models + 打标记）—— 一次落盘、原子
// ③ 失败时**不**回滚①：建出来的空分组最多是一份多余的配置，
//
//	而回滚它会让"重跑"变成另一条路径（那条路径没人测过）
//
// 顺序不能反：先改 key 再建分组的话，中间失败会留下"key 指向一个不存在的分组"
// ⇒ 受限且为空 ⇒ 这把 key 一个模型都不能用（P2 的边界）。
package apikey

import (
	"errors"
	"fmt"
	"log"
	"strings"
)

// MigrateResult 一次迁移的结果（供启动日志与界面提示「迁了几把」）。
type MigrateResult struct {
	// KeysMigrated 实际完成迁移的 key 数。
	KeysMigrated int `json:"keys_migrated"`
	// GroupsCreated 新建的分组数（撞名重试的那几次不算）。
	GroupsCreated int `json:"groups_created"`
	// Skipped 跳过的 key 数（已有分组 / 没有白名单 / 已标记迁过）。
	Skipped int `json:"skipped"`
	// Failed 失败的 key 数。**不是**"整段失败"—— 一把失败不该拖住其它几把。
	Failed int `json:"failed"`
	// Details 每个失败 key 的原因（key 名 + 错误），供启动日志直出。
	Details []string `json:"details,omitempty"`
}

// ErrGroupNameExists createGroup 报告"这个名字已被占用"时返回的哨兵。
//
// 为什么用哨兵而不是看错误字符串：撞名重试是本文件唯一的控制流分支，
// 靠字符串匹配会让它的正确性取决于 groupstore 的文案（改一个字就静默失效，
// 变成"整个迁移在第一把重名 key 上失败"）。
var ErrGroupNameExists = errors.New("apikey: 分组名已存在")

// CreateGroupFunc 建一个分组并返回它的 ID。名字已被占用时返回 ErrGroupNameExists。
//
// # 为什么是函数值而不是 *groupstore.Store
//
// 与 SetGroupModels 同一条理由：本包零内部依赖（见 apikey.go 里 ModelAllowed
// 那段注释）。apikey 认识 groupstore 就等于把配置层拖进凭据层的依赖图，
// 而依赖方向一旦长出来就收不回去。
type CreateGroupFunc func(name string, models []string) (id string, err error)

// MigrateModelsToGroups 把存量 key 的 Models 白名单迁移成分组。
//
// 只处理 Groups 为空 && Models 非空 && 未打迁移标记 的 key —— 三个条件都要：
//
//	Groups 非空  → 已经迁过（或用户手工配了分组），再迁会覆盖用户配置
//	Models 为空  → 本来就不限制，没有东西可迁
//	已打标记     → 上次迁移中途失败留下的，跳过等人工看日志
//
// # ⚠ "Models 为空就不迁"是最要紧的一条
//
// 空白名单的语义是**不限制**。若给它建一个空分组再指过去，就变成了
// 「受限且并集为空」⇒ 这把 key 一个模型都不能用（见 P2 的边界）。
// 一次迁移把所有"没想过要限制"的 key 打死，而错误信息只说"模型不允许"。
//
// # 幂等
//
// 连跑两次：第一次迁完 Groups 非空 + Models 空 + 标记已打 ⇒ 第二次全部跳过，
// 分组数不变。这是本文件唯一必须靠测试钉住的承诺（见 migrate_test.go）。
//
// # 撞名
//
// 分组名唯一（P1）。两把同名 key（"app"）迁移时会想要同一个分组名 ——
// 第二个必然撞。这里**加后缀重试**而不是整段失败：迁移是启动时跑的一次性动作，
// 因为"重名"而整段中止意味着后面的 key 全都不迁，而它们本来没有问题。
func MigrateModelsToGroups(s *Store, createGroup CreateGroupFunc) (MigrateResult, error) {
	s.maybeAttachSQL()
	var res MigrateResult
	if s == nil || createGroup == nil {
		return res, nil
	}

	// 先快照出候选，**不在锁内**建分组。
	//
	// createGroup 会去问 groupstore（那边有自己的锁）。持着本包的锁调用它，
	// 两个存储的加锁顺序就成了"谁先谁"的隐式约定 —— 今天不会死锁，
	// 下一个在 groupstore 侧回调 apikey 的人就会制造一个。
	todo := s.migrationCandidates(&res)
	if len(todo) == 0 {
		return res, nil
	}

	for _, c := range todo {
		gid, err := createGroupWithRetry(createGroup, migrationGroupName(c.name, c.id), c.models)
		if err != nil {
			res.Failed++
			res.Details = append(res.Details, fmt.Sprintf("key %s（%s）: %v", c.id, c.name, err))
			log.Printf("apikey: 迁移 key %s 到分组失败: %v", c.id, err)
			continue
		}
		res.GroupsCreated++

		// ② 改 key：三个字段一次落盘（原子）。
		if err := s.applyMigration(c.id, gid); err != nil {
			// ③ 不回滚分组：建出来的空分组最多是"一份多余配置"，
			//    而回滚会让重跑走另一条没人测过的路径。如实记下来。
			res.Failed++
			res.Details = append(res.Details, fmt.Sprintf("key %s（%s）: 分组已建 %s，但写回失败: %v", c.id, c.name, gid, err))
			log.Printf("apikey: 分组 %s 已创建，但写回 key %s 失败: %v", gid, c.id, err)
			continue
		}
		res.KeysMigrated++
	}
	return res, nil
}

// migrationCandidate 一把待迁移的 key（快照，不持有任何内部指针）。
type migrationCandidate struct {
	id     string
	name   string
	models []string
}

// migrationCandidates 快照出待迁移的 key，并把跳过的计入 res。
func (s *Store) migrationCandidates(res *MigrateResult) []migrationCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	var todo []migrationCandidate
	for _, k := range s.keys {
		if k == nil {
			continue
		}
		if k.MigratedToGroups || len(k.Groups) > 0 || len(k.Models) == 0 {
			res.Skipped++
			continue
		}
		todo = append(todo, migrationCandidate{
			id:     k.ID,
			name:   strings.TrimSpace(k.Name),
			models: append([]string(nil), k.Models...),
		})
	}
	return todo
}

// applyMigration 把 key 指向分组、清空白名单、打上迁移标记（**一次**落盘）。
//
// ⚠ 三个字段必须同一次写：分三次 save 的话，中间崩溃会留下
// "Groups 有了但 Models 还在"之类的形态 —— 判定链走分组（白名单不参与），
// 表面上看不出错，但它让"失败的表现"取决于崩溃发生的时刻，而那种 bug
// 只有在别人的机器上才复现。
//
// ⚠ ModelMap **保留**：它做的是"改名"，不是"限定范围"（PLAN Q3 的表格）。
// 清掉它等于把所有配了对外名的 key 的别名一次性作废。
func (s *Store) applyMigration(id, groupID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		// 迁移期间被删掉了：不是错误。分组留着就留着，P5 的引用完整性会收拾。
		return nil
	}
	prevGroups := append([]string(nil), k.Groups...)
	prevModels := append([]string(nil), k.Models...)
	prevFlag := k.MigratedToGroups

	k.Groups = normalizeGroups([]string{groupID})
	k.Models = nil
	k.MigratedToGroups = true
	s.markDirtyLocked(id)
	if err := s.saveLocked(); err != nil {
		// 落盘失败 ⇒ 内存回到迁移前，否则会出现"这个进程里已迁移、重启后又回到
		// 白名单"的幽灵状态（与 CreateOwned 的撤回同一做法）。
		k.Groups = prevGroups
		k.Models = prevModels
		k.MigratedToGroups = prevFlag
		return err
	}
	return nil
}

// migrationGroupName 生成迁移用的分组名。
//
// 用户视角：分组列表里看到「app 的模型」，能直接对上"app 这把 key"。
// key 没有名字时退化成 ID 前 8 位 —— 仍然可辨认（管理界面上 ID 可见），
// 而"迁移自"三个字说明了它的来历，不会让人以为是自己建的。
func migrationGroupName(keyName, keyID string) string {
	if keyName != "" {
		return keyName + " 的模型"
	}
	short := keyID
	if len(short) > 8 {
		short = short[:8]
	}
	return "迁移自 " + short
}

// createGroupWithRetry 撞名时加后缀重试（-2、-3…）。
//
// 上限 100 次：到这个量级已经不是"撞名"了，而是 createGroup 实现有问题
// （比如永远返回 ErrGroupNameExists），继续重试只会把启动拖住。
//
// ⚠ 非撞名错误**立刻**返回，不重试：写盘失败、分组名非法这类问题重试一万次
// 也是同一个结果，而重试会让启动日志里出现 100 条同样的错误，把真正的原因埋掉。
func createGroupWithRetry(createGroup CreateGroupFunc, name string, models []string) (string, error) {
	const maxAttempts = 100
	var lastErr error
	for i := 1; i <= maxAttempts; i++ {
		attempt := name
		if i > 1 {
			attempt = fmt.Sprintf("%s-%d", name, i)
		}
		id, err := createGroup(attempt, models)
		if err == nil {
			if strings.TrimSpace(id) == "" {
				return "", errors.New("createGroup 返回了空的分组 ID")
			}
			return id, nil
		}
		lastErr = err
		if !errors.Is(err, ErrGroupNameExists) {
			return "", err
		}
	}
	return "", fmt.Errorf("分组名 %q 连续撞名 %d 次仍失败: %w", name, maxAttempts, lastErr)
}
