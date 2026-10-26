// setup.go 初始化单例、启动自愈、跨进程互斥。
//
// # 这个文件负责的三件事（PLAN-sql-unify.md 的 T2）
//
//	IsSetup         只读问一句"装完了吗"
//	CheckSetup      启动时调用；顺带**自愈**（hyxapi model/main.go:57-82 同构）
//	InitializeSetup 真正建管理员 + 写完成标记；**同一事务**，且跨进程只成功一次
//
// # 为什么完成标记是一张表而不是一个字段（抄 hyxapi model/setup.go:20-21）
//
// setup 行一身二任，缺一不可：
//
//	① 完成标记   有行 = 已初始化。放配置文件里做不到"多进程同时首启"的正确性。
//	② 跨进程互斥 主键写死 1 ⇒ 第二个写入者撞主键。
//
// ②是本文件最容易被写错的地方，单独解释（见 InitializeSetup 里的两处判据）。
//
// # 为什么 store 不 import internal/account
//
// 反向：Wave 2（T4）要把 account.Store 换成 SQL 后端，那时 account 会 import
// store。若 store 现在 import account 取它现成的 HashPassword / newID /
// validateUsername，就形成一个**包循环** —— 这个循环要到 T4 那天才爆出来，
// 而那时改动面已经很大了。
//
// 所以这里把四个小工具（用户名/口令校验、bcrypt、ID 生成）**照原样复制一份**，
// 每个都标了出处。代价是两处常量要一起改 —— 没有测试能钉住这种跨包漂移，
// 所以这些注释就是约束本身：改之前先去看另一边。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrSetupAlreadyDone 系统已经初始化过。
//
// 它不是"故障"而是"状态"：初始化只允许成功一次（与 account.ErrBootstrapDone
// 同一性质的哨兵，只是那一套管的是 JSON 后端）。调用方据此返回 409 而不是 500。
var ErrSetupAlreadyDone = errors.New("store: 已初始化")

// setupID setups 表那一行的主键。
//
// ⚠ 必须是**常量 1**，不能自增：自增的话两个进程各拿到 1 和 2，"单例"就破了，
// 而互斥也随之失效（第二行不会撞任何键，两个进程都以为自己成功了）。
// 这是 hyxapi 用 Setup{ID: 1} 的同一个理由。
const setupID uint = 1

// IsSetup 只读地问"系统初始化了吗"。
//
// # 为什么错误一律折成 false
//
// 它被调用在**最容易出事的时机**：页面渲染、健康检查、启动日志。这些地方拿到
// 一个 bool 就够了，而且"读不出来"必须往**安全的方向**倒 —— 读失败的库若被当成
// "已初始化"，下一步就是放行登录/关闭注册入口；反过来只会多显示一次"未初始化"。
//
// 需要区分"确实没初始化"与"库坏了"的调用方请直接用 CheckSetup。
func IsSetup() bool {
	ok, err := CheckSetup()
	return err == nil && ok
}

// CheckSetup 三态判据 + 启动自愈。
//
//	有 setups 行                          ⇒ true
//	无行，但 users 里已有 RoleRoot 用户    ⇒ **补落 setups 行**，返回 true（自愈）
//	两者都没有                            ⇒ false
//
// # 为什么需要自愈（这不是"多此一举的兜底"）
//
// 会出现"有管理员但没有完成标记"的真实路径至少有两条：
//
//	· 旧版本/手工迁移把 users 导进来了，但 setups 表是这一版才有的；
//	· 初始化事务被外部 kill（断电、容器 OOM、用户 Ctrl-C）在"建完用户、
//	  还没写标记"之间 —— 因为标记是刻意写在**最后**的一步（见 InitializeSetup）。
//
// 没有自愈的话，这两种库会永远停在"有管理员却不能登录"的状态：注册入口说
// "管理员已存在"，setup 页说"还没初始化"，两边都走不通，只能手工改库。
//
// # 为什么补行用 DoNothing 而不是直接 Create
//
// 自愈可能被**多个进程同时**触发（多副本部署的启动风暴）。直接 Create 的第二个
// 进程会拿到主键冲突错误，于是它把"已经好了"误报成启动失败。DoNothing 让这件事
// 收敛成一句"我尽力了，结果是有行的"。
func CheckSetup() (bool, error) {
	if DB == nil {
		return false, ErrNotOpen
	}
	var setups int64
	if err := DB.Model(&Setup{}).Count(&setups).Error; err != nil {
		return false, fmt.Errorf("store: 读 setups 表: %w", err)
	}
	if setups > 0 {
		return true, nil
	}

	var roots int64
	if err := DB.Model(&User{}).Where("role = ?", RoleRoot).Count(&roots).Error; err != nil {
		return false, fmt.Errorf("store: 数 root 用户: %w", err)
	}
	if roots == 0 {
		return false, nil
	}

	heal := Setup{ID: setupID, Version: Version, InitializedAt: time.Now().Unix()}
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&heal).Error; err != nil {
		// 补行失败**不**把结论翻回 false：管理员确实存在，系统事实上已经在用。
		// 但错误必须往上带 —— 否则"每次启动都补一次、每次都失败"会变成一个
		// 只在日志里一闪而过的静默循环。
		return true, fmt.Errorf("store: 自愈补写 setups 行: %w", err)
	}
	return true, nil
}

// InitializeSetup 建 root 管理员并写下完成标记，全过程**一个事务**。
//
// 四步（顺序有讲究，不能换）：
//
//	① 查 setups 幂等   已有行 ⇒ ErrSetupAlreadyDone，什么都不改
//	② 查 root 用户数    已有 root ⇒ 跳过建号（自愈路径：只补标记）
//	③ 建 root（Role=RoleRoot）+ upsert 初始化选项
//	④ 写 setups 行      ← **最后一步**
//
// ④必须是最后一步：标记一旦落下就代表"初始化完成"，而"完成"的前提是前面的
// 用户与配置都已经在库里。只要还在同一事务里，中途失败标记也会一起回滚；
// 但如果有人把标记挪到事务**外**去写（很自然的"优化"），就会出现
// "标记说完成、用户不存在"的死局：setup 页被关掉，而没人能登录。
//
// # 参数校验为什么提前到事务之外
//
// bcrypt（cost 12）要几十到几百毫秒。放进事务里意味着这段时间**一直握着写锁**
// （DSN 里的 _txlock=immediate 是"BEGIN 就抢写锁"），并发初始化的另一个进程
// 只能干等 busy_timeout。校验 + 哈希放外面，事务里只剩几条快 SQL。
//
// # 为什么先做一次"是否已初始化"的预检
//
// 纯粹为了错误口径与代价：已初始化的库上再调一次，应当立刻回 ErrSetupAlreadyDone，
// 而不是先花 200ms 哈希口令、再抱怨"用户名太短"。真正的**互斥判据**仍然是
// 事务里那两处（见 ③ 与 ④ 的注释），预检不承担任何正确性责任。
func InitializeSetup(username, password string) (*User, error) {
	if DB == nil {
		return nil, ErrNotOpen
	}
	if IsSetup() {
		return nil, ErrSetupAlreadyDone
	}

	username = strings.TrimSpace(username)
	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}
	hash, err := hashAccountPassword(password)
	if err != nil {
		return nil, err
	}

	var created *User
	err = DB.Transaction(func(tx *gorm.DB) error {
		// ① 幂等：同一进程内的第二次调用、或另一个刚提交的进程，都在这里被挡住。
		var completed int64
		if err := tx.Model(&Setup{}).Count(&completed).Error; err != nil {
			return fmt.Errorf("store: 读 setups 表: %w", err)
		}
		if completed > 0 {
			return ErrSetupAlreadyDone
		}

		// ② 已有 root（自愈路径）就不再建号 —— 否则第二次初始化会在
		//    users.username 的唯一索引上撞车，把"已初始化"误报成"用户名被占用"。
		var roots int64
		if err := tx.Model(&User{}).Where("role = ?", RoleRoot).Count(&roots).Error; err != nil {
			return fmt.Errorf("store: 数 root 用户: %w", err)
		}
		if roots == 0 {
			u := &User{
				ID:           newUserID(),
				Username:     username,
				DisplayName:  username,
				PasswordHash: hash,
				Role:         RoleRoot,
				Enabled:      true,
				// 1 而不是 0：AuthVersion=0 会让"校验签发时的版本"这条判据在
				// 老数据上退化成"任何 0 都匹配"，与 account 的初始化口径保持一致。
				AuthVersion: 1,
				CreatedAt:   time.Now().Unix(),
			}
			if err := tx.Create(u).Error; err != nil {
				return fmt.Errorf("store: 建 root 用户: %w", err)
			}
			created = u
		} else {
			// 只补标记时也要把"初始化结果"给出去：调用方拿到的应该是那个
			// **已经存在**的 root，而不是 nil（nil,nil 会被读成"没建号也没错"，
			// 上层于是无法给出"你已经是管理员了"这类提示）。
			var existing User
			if err := tx.Where("role = ?", RoleRoot).Order("created_at").First(&existing).Error; err != nil {
				return fmt.Errorf("store: 读 root 用户: %w", err)
			}
			created = &existing
		}

		// ③ 初始化选项：见 setupOptionDefaults 的注释（本仓初始化没有模式参数）。
		for key, value := range setupOptionDefaults {
			if err := upsertOptionTx(tx, key, value); err != nil {
				return err
			}
		}

		// ④ 完成标记。DoNothing + RowsAffected 是**互斥的真正判据**：
		//
		//    另一个进程在本事务 BEGIN 之后、Count 之前提交了 setups 行
		//    ⇒ 我们这里插入被吞掉 ⇒ RowsAffected==0 ⇒ 回滚并报"已初始化"。
		//
		// ⚠ 只靠 ① 的 Count 是不够的：它依赖"读的时候没人能写"。本仓的 DSN 用
		// _txlock=immediate 保证了这一点（BEGIN 即独占写锁），但那是**外部配置**，
		// 一旦有人把 DSN 简化掉（比如为了绕开某个 busy 错误），Count 就成了 TOCTOU。
		// 把判据落在主键冲突上，才不依赖 DSN 的某个 pragma。
		marker := Setup{ID: setupID, Version: Version, InitializedAt: time.Now().Unix()}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&marker)
		if res.Error != nil {
			return fmt.Errorf("store: 写 setups 行: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrSetupAlreadyDone
		}
		return nil
	})
	if err != nil {
		// 回滚时 created 已被赋值也不能返回：那个用户**不在库里**。
		return nil, err
	}
	return created, nil
}

// setupOptionDefaults 初始化时要落进 options 的键值。
//
// # 为什么这里是空的（而且刻意写出来，不是忘了）
//
// hyxapi 的 InitializeSetup 会写 SelfUseModeEnabled / DemoSiteEnabled /
// ServerAddress —— 它的 setup 表单里有这些字段。本仓的契约签名只有
// (username, password)，没有模式与站点地址可写。
//
// 而 config.json 的可热改键由 ImportFromConfigJSON 负责（见 option.go），
// 那是**另一条路径**，不该在初始化事务里重复一遍：两处都写同一个键时，
// "谁覆盖谁"取决于调用顺序，而调用顺序是装配层的细节，运维看不出来。
//
// 于是这里保留步骤、集合为空：以后若真出现"只有 setup 才知道的键"（比如
// 首次生成的实例 ID），加到这个 map 里即可 —— 顺序、事务性、幂等都已就位。
var setupOptionDefaults = map[string]string{}

// upsertOptionTx 在给定事务里 upsert 一条 option。
//
// 与 option.go 的 PutOption 共用同一段 clause，但**必须**收 tx 参数：事务内
// 直接调 PutOption 会走包级 DB（另一个连接），于是它写的东西不在本事务里 ——
// 回滚时不会撤销，造成"初始化失败但 options 已经变了"。
func upsertOptionTx(tx *gorm.DB, key, value string) error {
	opt := Option{Key: key, Value: value}
	err := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&opt).Error
	if err != nil {
		return fmt.Errorf("store: 写 option %q: %w", key, err)
	}
	return nil
}

// ── 从 internal/account 复制的四个小工具（理由见文件头） ──────────────
//
// 每个函数都注明了出处。**改一边必须看另一边**。

// 用户名/口令的长度口径，照抄 internal/account/account.go 的
// usernameMin/usernameMax/passwordMin/passwordMax（那边抄的是 new-api
// model/user.go 的 min=8,max=128，用户名另给了自己的界）。
const (
	setupUsernameMin = 3
	setupUsernameMax = 32
	setupPasswordMin = 8
	setupPasswordMax = 128
)

// 口令/用户名非法。文案与 account 的 ErrBad* 同形，便于上层统一映射成 400 ——
// 但**不是**同一个变量：两个包各自持有一份，谁都不该替对方决定口径。
var (
	errBadUsername = errors.New("store: 用户名不合法")
	errBadPassword = errors.New("store: 口令不合法")
)

// validateUsername 出处：internal/account/account.go 的 validateUsername。
//
// 字符集是**白名单**（字母数字与 _ - . @）：用户名会进日志与界面，放任 Unicode
// 会让"看起来一样"的名字变成一类看不见的冒充手段。
func validateUsername(name string) error {
	if len(name) < setupUsernameMin || len(name) > setupUsernameMax {
		return fmt.Errorf("%w：长度需在 %d–%d 之间", errBadUsername, setupUsernameMin, setupUsernameMax)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '@':
		default:
			return fmt.Errorf("%w：只允许字母数字与 _ - . @", errBadUsername)
		}
	}
	return nil
}

// validatePassword 出处：internal/account/account.go 的 validatePassword。
func validatePassword(password string) error {
	if len(password) < setupPasswordMin || len(password) > setupPasswordMax {
		return fmt.Errorf("%w：长度需在 %d–%d 之间", errBadPassword, setupPasswordMin, setupPasswordMax)
	}
	return nil
}

// setupBcryptCost 与 internal/account/password.go 的 bcryptCost 相同（12）。
//
// 12 而不是 Go 默认 10：登录是低频路径，多几十毫秒换暴力破解成本 ×4。
//
// ⚠ bcrypt 把 cost 编码在哈希串里，两边**不需要**一致也能互相校验（各自按
// 记录里的 cost 走）。这条注释存在是因为"两处常量不一致"看起来像 bug，而它
// 其实只影响新写出的哈希，不影响老数据。
const setupBcryptCost = 12

// hashAccountPassword 出处：internal/account/password.go 的 HashPassword。
//
// 超过 72 字节必须显式拒绝：bcrypt 会**静默截断**，而我们的上限是 128，
// 正好踩在这条性质上 —— 让"第 73 个字符不参与校验"成为没人知道的秘密是最糟的。
func hashAccountPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	if len(password) > 72 {
		return "", errors.New("store: 口令过长（bcrypt 只取前 72 字节，超出部分会被静默忽略）")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), setupBcryptCost)
	if err != nil {
		return "", fmt.Errorf("store: 生成口令哈希: %w", err)
	}
	return string(h), nil
}

// newUserID 出处：internal/account/account.go 的 newID（u- + 16 位十六进制）。
//
// 用 crypto/rand 而不是自增：ID 会出现在 URL 与日志里，自增等于广播"这台机器
// 上有几个账号、谁先建"。rand 失败时 panic 是**刻意**的 —— 退化成时间戳会让
// ID 可预测，而账号系统在无法生成不可预测随机数的环境里继续跑本身就是错的。
func newUserID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("store: 无法生成随机用户 ID: " + err.Error())
	}
	return "u-" + hex.EncodeToString(buf[:])
}
