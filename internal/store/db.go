// db.go 数据库地基：打开 / 迁移 / 关闭，以及那个进程级单例句柄。
//
// # 为什么是 pure-Go SQLite（PLAN §2 D1 的铁律）
//
// 本仓的部署优势是「单二进制 + 零依赖」（见 internal/account/store.go 里
// 「为什么用 JSON 文件」那段的论证）。cgo 会拆掉它：
//
//	cgo 版本 → 需要目标平台的 C 工具链与 libc ⇒ 交叉编译退化、镜像要带 gcc
//	pure-Go → GOOS/GOARCH 随便换，CGO_ENABLED=0 也能构建
//
// 所以驱动只许 github.com/glebarez/sqlite（底层 modernc.org/sqlite，纯 Go 实现）。
// **不要**换成 gorm.io/driver/sqlite —— 它底层是 mattn/go-sqlite3，需要 cgo。
// 这条约束有测试守着（db_test.go 的 TestNoCgoDriver）：它扫描 go.mod，
// 因为「某天有人为了一个特性换驱动」是真实发生过的一类改动。
//
// # 为什么把 DB 做成包级变量（而不是注入）
//
// 因为本仓现在的持久化是**包级函数式**的（account.Store、logbuf 都是包级单例）。
// 改成依赖注入要动所有调用点，而这次改造的纪律是「公开 API 签名一个都不动」
// （PLAN §4）。包级变量是同一语义的最小实现。
//
// ⚠ 代价必须写清：它让**测试之间会互相影响**（若两个用例都 Open，
// 后一个会关掉前一个的句柄）。所以约定：测试里各用 t.TempDir() 下的独立文件，
// 且不要对同一个 store.DB 并行跑用例。
package store

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB 进程级数据库句柄。由 Open 设置、Close 置空。
//
// # 为什么不做 nil 检查铺满每个仓储函数
//
// 它不是「可选的」：没有它任何仓储函数都跑不了。而初始化顺序上它必然存在
// 一个 nil 窗口（Open 之前）。把检查放进每个函数 = 一百处重复；不如让
// 使用点直接撞上 nil（GORM 对 nil 接收者会 panic，立刻暴露漏接线）。
//
// 因此本文件的契约只有一句：**Open 成功之前不要用 DB**。
var DB *gorm.DB

// openMu 串行化 Open/Close。
//
// # 为什么必须加锁
//
// Open 会先关掉旧句柄再开新的（见 Open 的幂等性说明）。两个 goroutine 同时
// Open 时，A 可能关掉 B 刚开的句柄 ⇒ 得到一个指向已关闭连接池的 DB，
// 而错误要到第一次查询才出现（sql: database is closed），离现场很远。
var openMu sync.Mutex

// dsnDefaults 是每条 SQLite 连接都必须带的 PRAGMA。三个参数各解决一类具体故障：
//
//	busy_timeout(30000)  写者等锁最多 30s 再报 database is locked。
//	                     SQLite 默认是 **0**（立刻失败）：并发写时表现为随机的
//	                     「database is locked」，日志里看不出规律，极难归因。
//	                     30s 对齐 PLAN 的约定，也够覆盖一次 AutoMigrate。
//	journal_mode(WAL)    读写不互斥：读者不被写者阻塞。默认的 rollback journal
//	                     会让「写日志」（每次请求一行）阻塞「读指标」，
//	                     而那正好是本改造要同时做的两件事。
//	_txlock=immediate    事务一开始就拿写锁，而不是「读到写时才升级」。
//	                     ⚠ 这是本组参数里最容易被低估的一条：deferred 事务在
//	                     「先读后写」时会拿到 SQLITE_BUSY_SNAPSHOT，且**不能靠
//	                     busy_timeout 重试**（重试也不会变好，事务必须重来）。
//	                     本仓的初始化/导入都是「先查再写」，正好落在这个形态上。
var dsnDefaults = []string{
	"busy_timeout(30000)",
	"journal_mode(WAL)",
}

// txLockMode 见 dsnDefaults。单独提出来是因为它是**参数名**而不是 pragma。
const txLockMode = "immediate"

// Open 打开（必要时创建）数据库文件并设置连接池。
//
// # 幂等性：同一路径调两次不报错
//
// 实现方式是「先关旧的再开新的」，而不是「已打开就返回 nil」：
//
//	已打开就返回 nil → 换路径时静默沿用旧库（灾难：以为迁到了新库，实际在写旧的）
//	先关旧的再开     → 行为总是「Open 之后 DB 就是这个路径」，可预期
//
// ⚠ 它假设调用时机是**启动期**。不要在请求路径上调用 Open —— 关闭连接池会让
// 在途查询全部失败。这个假设由「DB 是包级单例」这一事实强化：全仓只有 main
// 与测试会调它。
func Open(path string) error {
	openMu.Lock()
	defer openMu.Unlock()

	// 先关旧句柄（见上方幂等性说明）。关闭失败不阻止重新打开：
	// 旧句柄已经不可用了，坚持报错只会让「进程内重开数据库」这件事永远做不成。
	if DB != nil {
		_ = closeLocked()
	}

	if err := ensureParentDir(path); err != nil {
		return err
	}

	db, err := gorm.Open(sqlite.Open(buildDSN(path)), gormConfig())
	if err != nil {
		return fmt.Errorf("store: 打开数据库 %q 失败: %w", path, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("store: 取连接池失败: %w", err)
	}

	// # 连接数为什么是这个量级，而不是参考项目的 1000
	//
	// SQLite 是**单写**数据库：任一瞬间只有一个写事务。开 1000 个连接不会提高
	// 写吞吐，只会让它们排队等同一把写锁，并各自持有一个栈与页缓存。
	// 8 个连接足够「少量读并发 + 一个写者」的形态（本仓是单用户的控制台网关）。
	//
	// ⚠ 内存库必须压到 1 个连接：每个连接会是**各自独立的空库**
	// （除非显式用 cache=shared），迁移建的表在下一个连接里根本不存在。
	if isMemoryDSN(path) {
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
	} else {
		sqlDB.SetMaxOpenConns(maxOpenConns)
		sqlDB.SetMaxIdleConns(maxIdleConns)
	}
	// 不设 ConnMaxLifetime：SQLite 没有「服务端断连」这回事，连接可永久复用。
	// 设一个短 lifetime 只会让 PRAGMA 在每个新连接上重跑（包括 WAL 模式切换），
	// 平白多出一类「新连接建起来时正好撞上写锁」的失败。

	// Ping 一次：让连接真正建立，从而让 PRAGMA（DSN 参数）在这里就生效并暴露错误。
	// 不 Ping 的话，一个写错的 PRAGMA 会推迟到第一次业务查询才报错。
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("store: 连接数据库 %q 失败: %w", path, err)
	}

	DB = db
	return nil
}

// 连接池上限。见 Open 里的论证。
const (
	maxOpenConns = 8
	maxIdleConns = 4
)

// gormConfig 返回全仓统一的 GORM 配置。
//
// # 为什么 PrepareStmt 是 false（与两个参考项目相反）
//
// 参考项目（new-api / hyxapi）都开 PrepareStmt。本仓刻意关掉，理由是
// 「加载后立即 AutoMigrate」这条路径：
//
//	PrepareStmt 的语句缓存挂在 *gorm.DB 上，按 SQL 文本缓存，**不随表结构失效**。
//	而 AutoMigrate 会 ALTER TABLE（加列/建索引）—— 启动完成后，缓存里那条
//	「INSERT INTO users (… 旧列 …)」就指向了旧列集，报 no such column。
//
// 这个故障有恶劣特征：**只在新版本第一次启动时出现**（老库才需要 ALTER），
// 而测试每次建新库，所以测试永远不会红。
//
// # 为什么 DisableForeignKeyConstraintWhenMigrating
//
// 本包 12 张表**没有声明任何关系字段**（不嵌入关联），而是用逻辑 ID 关联。
// 两个实际原因：
//
//	① SQLite 的外键约束默认**关闭**，需要在每条连接上 PRAGMA foreign_keys=ON
//	   才生效 —— 即「写了约束但不生效」，比不写更危险。
//	② 迁移会变成顺序敏感：建表顺序错了 AutoMigrate 直接失败，
//	   而加表的人不一定知道顺序。
//
// 需要它时再一起加（连同 PRAGMA），不要只加一半。
func gormConfig() *gorm.Config {
	return &gorm.Config{
		// Warn 级别：只记慢查询与错误。本仓的日志是给人看的，
		// Info 会把每条 SQL 都打出来，反而埋掉真正的信号。
		Logger: logger.Default.LogMode(logger.Warn),
		// 见函数注释：开它会与启动期的 AutoMigrate 冲突。
		PrepareStmt: false,
		// 见函数注释：本包不用外键约束。
		DisableForeignKeyConstraintWhenMigrating: true,
	}
}

// Migrate 建表/补列（AutoMigrate 全部 12 张表）。
//
// # 为什么要一个显式函数，而不是在 Open 里自动跑
//
// 「建表」是**会改库**的动作，而 Open 应当只负责连接。分开之后：
//
//	只读诊断工具、导出脚本 → 可以 Open 而不 Migrate（不碰人家的库结构）
//	测试 → 可以 Open 两个库互相对照，只对其中一个 Migrate
//
// 这也是参考项目的形态（InitDB 里显式调 migrateDB）。
//
// # AutoMigrate 的边界（必须知道）
//
// 它只做**加法**：建表、加列、加索引。它**不会**删列、不会改列类型、不会回滚。
// 所以「改字段类型」在本仓需要手写迁移步骤 —— 不要以为改了 struct 就完事
// （GORM 会静默跳过）。
// ⚠ 实现走 migrateAll：DDL 必须**串行**，否则多进程同时首启会撞建表竞态
// （2026-10-26 修，见 migrateAll 的论证）。
func Migrate() error {
	if DB == nil {
		return ErrNotOpen
	}
	if err := migrateAll(); err != nil {
		return fmt.Errorf("store: 建表失败: %w", err)
	}
	return nil
}

// migrateAttempts / migrateRetryBackoff：见 migrateAll 的说明。
const (
	migrateAttempts     = 5
	migrateRetryBackoff = 200 * time.Millisecond
)

// migrateAll 是 AutoMigrate 的**多进程安全**包装。
//
// # 为什么必须包一层事务（2026-10-26 修，实测红：TestInitializeSetupConcurrentProcesses）
//
// AutoMigrate 的每一步都是「先查在不在 → 不在就建」（TOCTOU）。N 个进程同时首启时，
// 两个进程可以都判定「表不存在」，然后其中一个的 CREATE 撞上另一个刚建好的表 ——
// 子进程以 `store: 建表失败` 退出（跨进程测试里表现为退出码 12）。
//
// 这**不是** busy_timeout 能覆盖的故障：建表竞态在重试语义上落在
// SQLITE_BUSY / SNAPSHOT 那一类**不可靠重试**的错误上（与 dsnDefaults 里
// _txlock 那段论证同源）。正确的修法是**让 DDL 本身串行**：
//
//	BEGIN IMMEDIATE（DSN 的 _txlock=immediate）→ 先拿到写锁才继续
//	  ⇒ 同一时刻只有一个进程在迁移，其余进程在 busy_timeout(30s) 内等待
//	  ⇒ 等到的进程进入事务后重新检查，看到的已是完整表结构，不再产生任何 DDL
//
// 这正是 dsnDefaults 里 _txlock=immediate 存在的原始理由（「先查再写」形态），
// 只是迁移路径此前没有真的用上它（AutoMigrate 默认**不**开事务）。
//
// 外层再叠一层有限重试：万一等待超过 busy_timeout，或驱动给出别的瞬时错误，
// 重试 AutoMigrate 是安全且幂等的（它只做加法：建表 / 加列 / 加索引）。
func migrateAll() error {
	var lastErr error
	for attempt := 1; attempt <= migrateAttempts; attempt++ {
		err := DB.Transaction(func(tx *gorm.DB) error {
			return tx.AutoMigrate(allModels()...)
		})
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == migrateAttempts || !isTransientMigrationErr(err) {
			break
		}
		time.Sleep(migrateRetryBackoff * time.Duration(attempt))
	}
	return lastErr
}

// isTransientMigrationErr 判断迁移错误是否属于「等一下再来」那一类。
//
// 只认 SQLite 的锁 / 竞争字样，别的一律不重试 —— 把语法错、磁盘满这类确定性失败
// 也重试 5 次，只会拖长启动时间，并把真正的错误埋进日志。
func isTransientMigrationErr(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"database is locked",
		"database table is locked",
		"sqlite_busy",
		"snapshot",
		"already exists",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Close 关闭连接池并把 DB 置空。可重复调用。
func Close() error {
	openMu.Lock()
	defer openMu.Unlock()
	return closeLocked()
}

// closeLocked 是 Close 的无锁版本（供 Open 在持锁时调用；sync.Mutex 不可重入）。
func closeLocked() error {
	if DB == nil {
		return nil
	}
	sqlDB, err := DB.DB()
	DB = nil
	if err != nil {
		// 取不到底层连接池说明句柄已经坏了，没有可关的东西。
		return nil
	}
	return sqlDB.Close()
}

// ErrNotOpen 在 DB 为 nil 时返回。
//
// # 为什么不是 panic
//
// 这确实是一个编程错误（忘了接线），但本仓的约定是「服务不能因为一条路径
// 就整个崩掉」：admin 页的某个按钮点下去报 500 并说明「数据库未初始化」，
// 比进程退出好得多 —— 后者会让已建立的会话和正在跑的请求一起断掉。
var ErrNotOpen = errors.New("store: 数据库未打开（先调用 store.Open）")

// allModels 是 AutoMigrate 的**全量**输入。
//
// # 为什么这张清单必须与 models.go 一一对应，且有测试守着
//
// 漏一张表不会有任何编译错误、也不会有运行时错误 —— 它只在某个功能第一次
// 用到那张表时报 no such table，而那可能发生在几天后的一次后台任务里。
// 所以 db_test.go 用**反射**枚举 models.go 里的 struct 与本清单比对
// （TestAllModelsAreMigrated）：加了 struct 忘了登记，测试立刻红。
func allModels() []any {
	return []any{
		&Setup{},
		&User{},
		&UserSession{},
		&Option{},
		&Channel{},
		&Ability{},
		&Model{},
		&Vendor{},
		&Log{},
		&Account{},
		&AccountEvent{},
		&AuditLog{},
	}
}

// buildDSN 把路径补全成带 PRAGMA 的 DSN。
//
// # 为什么**只补缺、不覆盖**
//
// 调用方显式给了 _pragma 时必须尊重它（测试要用 journal_mode(DELETE) 验证
// 「参数真的来自调用方」这类断言；运维也可能为网络盘关掉 WAL）。
// 这段逻辑照搬参考项目的 SQLiteDSN —— 它在生产里已经处理过「自定义 DSN 与
// 默认值共存」的边角，没必要重新发明一遍。
func buildDSN(path string) string {
	filename, rawQuery, _ := strings.Cut(path, "?")
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		// 让驱动去报这个错：它给出的信息（哪个参数非法）比我们转述的更准确。
		return path
	}
	for _, def := range dsnDefaults {
		name, _, _ := strings.Cut(def, "(")
		if !hasPragma(query["_pragma"], name) {
			query.Add("_pragma", def)
		}
	}
	if query.Get("_txlock") == "" {
		query.Set("_txlock", txLockMode)
	}
	return filename + "?" + query.Encode()
}

// hasPragma 判断已有参数里是否已设置名为 name 的 pragma。
//
// 同时认 name(...) 与 name=... 两种写法：SQLite 两者都接受，而「只认一种」
// 会在对方用了另一种时重复追加，悄悄覆盖对方的设置。
func hasPragma(pragmas []string, name string) bool {
	for _, p := range pragmas {
		v := strings.ToLower(strings.TrimSpace(p))
		if strings.HasPrefix(v, name+"(") || strings.HasPrefix(v, name+"=") {
			return true
		}
	}
	return false
}

// isMemoryDSN 判断路径是否为内存数据库。
//
// 三种写法都要认（驱动都支持）：
//
//	:memory:
//	file::memory:?cache=shared
//	file:xxx?mode=memory&cache=shared
//
// 认错是灾难性的：把内存库当文件库建目录，会创建出一个名为 :memory: 的目录。
func isMemoryDSN(path string) bool {
	if path == ":memory:" || path == "file::memory:" {
		return true
	}
	if !strings.HasPrefix(path, "file:") {
		return false
	}
	_, rawQuery, _ := strings.Cut(path, "?")
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return false
	}
	if q.Get("mode") == "memory" {
		return true
	}
	// file::memory:?cache=shared 的 path 部分就是 :memory:。
	filename, _, _ := strings.Cut(path, "?")
	return filename == "file::memory:"
}

// ensureParentDir 确保数据库文件的父目录存在。
//
// # 为什么这件事必须由 Open 做
//
// SQLite 不会替你建目录：它报 unable to open database file，而这条错误
// **不包含路径**，现场只能靠猜。首次部署时 data/ 不存在是常态，
// 一个 mkdir 就能消掉这类「部署即失败」。
//
// 内存库没有目录，直接跳过（见 isMemoryDSN：认错会建出名为 :memory: 的目录）。
func ensureParentDir(path string) error {
	if isMemoryDSN(path) {
		return nil
	}
	filename, _, _ := strings.Cut(path, "?")
	if strings.HasPrefix(filename, "file:") {
		filename = strings.TrimPrefix(filename, "file:")
	}
	if filename == "" {
		return nil
	}
	dir := filepath.Dir(filename)
	if dir == "." || dir == "" {
		// 相对当前目录（例如 gw.db），无需建目录。
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: 创建数据库目录 %q 失败: %w", dir, err)
	}
	return nil
}

// Tables 返回本包迁移的**表名**清单（供自检与运维对账）。
//
// # 为什么不直接返回 struct
//
// 调用方是人（启动日志打一行「已建 12 张表」）与测试（断言 12 个名字）。
// 返回名字就够了，也不必把 GORM 类型泄漏给调用方。
func Tables() []string {
	return []string{
		"setups", "users", "user_sessions", "options", "channels", "abilities",
		"models", "vendors", "logs", "accounts", "account_events", "audit_log",
	}
}
