// migrate-sql 是一次性迁移/导出 CLI：把 data/*.json 导进 SQLite，也能反向导回 JSON。
//
// # 为什么需要它（PLAN-sql-unify.md D3 的落地件）
//
// SQL 成为唯一权威之后，data/*.json 降级为「导出/备份格式」。但迁移这一步
// **必须可逆且可核对**，否则「换数据库」在运维眼里就是一次不可回滚的赌博。
// 所以这个工具的三条纪律写死在这里，而不是写在文档里：
//
//	① **只读 JSON**：任何子命令都不写、不删、不改 data/*.json。
//	② **导入幂等**：按主键 upsert，跑两次的结果与跑一次相同。
//	③ **坏行跳过而不是整体失败**：一条坏记录不该让整个账号库迁不过去；
//	   但要逐条报出「哪条、为什么」，否则跳过等于藏起来。
//
// # 为什么用子命令而不是用 flag 选模式
//
// import / verify / export 是三个**不同权限等级**的动作（写库 / 只读核对 /
// 写文件）。用 flag 表达时，「同时给了 --import --export」的行为要靠代码约定；
// 用子命令则天然互斥，且 migrate-sql help 的输出就是一张权限清单。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"workbuddy2api/internal/store"
)

// 文件名常量。这些名字是全仓约定（internal/account、internal/apikey 各自
// 硬编码了同一套），集中在这里只为了让本文件不出现裸字符串。
const (
	fileAccounts  = "accounts.json"
	fileSessions  = "sessions.json"
	fileAPIKeys   = "apikeys.json"
	fileState     = "state.json"
	fileConfig    = "config.json"
	defaultDBName = "gw.db"
)

// 退出码。
//
// ⚠ exitMismatch 单独一个值，是为了让 CI/脚本能区分「跑挂了」与「跑通了但数不对」：
// 后者是数据问题，前者是程序问题，排查方向完全不同。
const (
	exitOK       = 0
	exitFailure  = 1
	exitUsage    = 2
	exitMismatch = 3
)

const usageText = `migrate-sql — data/*.json 与 SQLite 之间的单向迁移与反向导出

用法:
  migrate-sql import [--data DIR] [--db FILE] [--dry-run]
  migrate-sql verify [--data DIR] [--db FILE]
  migrate-sql export [--data DIR] [--db FILE] [--out DIR] [--allow-lossy]

子命令:
  import   把 accounts.json / sessions.json / config.json 导入 SQL；幂等，只读 JSON
  verify   核对「JSON 里认得的行」在 SQL 里都在（不做写操作）
  export   从 SQL 反向导出 JSON；拒绝覆盖 data/ 目录，拒绝无提示的有损导出

退出码: 0 成功 / 1 执行失败 / 2 用法错误 / 3 校验不一致
`

// ── JSON 形态（外部输入 / 输出契约）──────────────────────────────────────
//
// ⚠ 这些结构体**不复用** internal/account 的 User / Session。
//
// 理由不是洁癖：那两个是**运行时**形态，改它们要连带改登录与会话逻辑；
// 本工具是**一次性**的，只关心「盘上那串字节长什么样」。绑在一起会让
// 「给 User 加一个字段」意外改变导出文件的字节（于是第 N 次备份恢复才发现
// diff），也会让本工具跟着运行时结构体一起漂移。
//
// 字段顺序 = 输出 JSON 的键序，**不要重排**：PLAN-sql-unify.md §7 要求
// 「导出与迁移前逐字节等价」，键序是这个要求的一半，另一半是记录顺序
// （见 exportAccounts 的排序注释）。

// accountJSON 对应 data/accounts.json 的一个元素（internal/account.User）。
type accountJSON struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	DisplayName  string `json:"display_name"`
	PasswordHash string `json:"password_hash"`
	Role         int    `json:"role"`
	Enabled      bool   `json:"enabled"`
	AuthVersion  int64  `json:"auth_version"`
	CreatedAt    string `json:"created_at"`
	// omitempty 必须与 internal/account.User 一致：空值不落键。
	// 少了这对 tag，导出会给老记录凭空多出两个空字符串键 ⇒ 字节不等。
	LastLoginAt string `json:"last_login_at,omitempty"`
	LastLoginIP string `json:"last_login_ip,omitempty"`
}

// sessionJSON 对应 data/sessions.json 的一个元素（internal/account.Session）。
//
// ⚠ 这里有一个**契约缺口**，写在这里而不是只写在实现里：
// store.UserSession 只有 9 列（ID/UserID/AuthVersion/CreatedAt/LastActiveAt/
// ExpiresAt/IP/UserAgent/Status），而盘上的会话有 12 个键。差的是
// username、role、version、hard_expires_at。
//
//	username/role   → 导出时从 users 表回填（本仓用户名与角色不可变，见
//	                  internal/account/store.go 的「用户名不支持改名」）
//	version         → 无列可存（会话自身的乐观并发版本）
//	hard_expires_at → 无列可存（会话绝对上限，安全属性）
//
// 后一个决定了 **sessions 的导出不可能逐字节等价**，且丢 hard_expires_at
// 会让恢复出来的会话失去绝对上限（滑动续期可以无限延长）——这是安全退化，
// 不是格式瑕疵。所以 export 默认不写 sessions，要 --allow-lossy 才写。
type sessionJSON struct {
	SID           string `json:"sid"`
	UserID        string `json:"user_id"`
	Username      string `json:"username"`
	Role          int    `json:"role"`
	AuthVersion   int64  `json:"auth_version"`
	Version       int64  `json:"version"`
	CreatedAt     int64  `json:"created_at"`
	LastActiveAt  int64  `json:"last_active_at"`
	ExpiresAt     int64  `json:"expires_at"`
	HardExpiresAt int64  `json:"hard_expires_at"`
	IP            string `json:"ip,omitempty"`
	UA            string `json:"ua,omitempty"`
}

// ── 统计 ────────────────────────────────────────────────────────────────

// importStat 一张表的导入结果。
//
// 「跳过」必须与「失败」分开：跳过是**预期**的（坏行、按决议跳过的文件），
// 失败是**意外**（磁盘满、库被锁）。混在一个数里，人分不清该不该担心。
type importStat struct {
	Table    string
	Imported int
	Skipped  int
	// Reasons 逐条说明跳过原因。只放跳过，不放正常流程。
	Reasons []string
}

// summary 一次 import 的合计。
type summary struct {
	Stats []importStat
}

func (s *summary) total() (imported, skipped int) {
	for _, st := range s.Stats {
		imported += st.Imported
		skipped += st.Skipped
	}
	return imported, skipped
}

// ── 入口 ────────────────────────────────────────────────────────────────

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是**可测的**入口：不碰 os.Args / os.Stdout，全部走参数。
//
// 这样测试能直接断言输出文本与退出码 —— 「打印 导入N/跳过M」是验收口径，
// 必须能被断言，而起子进程解析 stdout 既慢又难看清失败原因。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return exitUsage
	}
	switch args[0] {
	case "import":
		return runImport(args[1:], stdout, stderr)
	case "verify":
		return runVerify(args[1:], stdout, stderr)
	case "export":
		return runExport(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(stderr, "migrate-sql: 未知子命令 %q\n\n%s", args[0], usageText)
		return exitUsage
	}
}

// commonFlags 三个子命令共用的 flag。
type commonFlags struct {
	dataDir string
	dbPath  string
}

func (c *commonFlags) register(fset *flag.FlagSet) {
	fset.StringVar(&c.dataDir, "data", "data", "data 目录（只读，任何子命令都不写它）")
	fset.StringVar(&c.dbPath, "db", "", "SQLite 文件路径；空 = <data>/"+defaultDBName)
}

func (c *commonFlags) resolve() {
	if c.dataDir == "" {
		c.dataDir = "data"
	}
	if c.dbPath == "" {
		c.dbPath = filepath.Join(c.dataDir, defaultDBName)
	}
}

// ── import ──────────────────────────────────────────────────────────────

func runImport(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("import", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var c commonFlags
	c.register(fset)
	dryRun := fset.Bool("dry-run", false, "只解析与校验，不写数据库（先看会导入什么）")
	if err := fset.Parse(args); err != nil {
		return exitUsage
	}
	c.resolve()

	if *dryRun {
		st, err := inspectDataDir(c.dataDir)
		if err != nil {
			fmt.Fprintf(stderr, "migrate-sql import: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "（dry-run）将导入 %s\n", st.describe())
		printStats(stdout, st)
		imported, skipped := st.total()
		fmt.Fprintf(stdout, "导入 %d/跳过 %d\n", imported, skipped)
		return exitOK
	}

	db, err := openStore(c.dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "migrate-sql import: %v\n", err)
		return exitFailure
	}
	defer func() { _ = store.Close() }()

	st, err := importAll(db, c.dataDir, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "migrate-sql import: %v\n", err)
		return exitFailure
	}
	printStats(stdout, st)
	imported, skipped := st.total()
	// ⚠ 这一行是验收口径（任务书：「打印 导入N/跳过M」），改格式前先看任务书。
	fmt.Fprintf(stdout, "导入 %d/跳过 %d\n", imported, skipped)
	return exitOK
}

// importAll 依次导三类数据。顺序**有依赖**：先 users 再 sessions
// （sessions.user_id 指向 users；现在没有外键约束，但先导父表能让
// 「会话指向了一个不存在的账号」这类脏数据在核对时更早暴露）。
func importAll(db *gorm.DB, dataDir string, stderr io.Writer) (*summary, error) {
	s := &summary{Stats: []importStat{}}

	acc, err := importUsers(db, dataDir, stderr)
	if err != nil {
		return nil, err
	}
	s.Stats = append(s.Stats, acc)

	ses, err := importSessions(db, dataDir, stderr)
	if err != nil {
		return nil, err
	}
	s.Stats = append(s.Stats, ses)

	opt := importOptions(dataDir, stderr)
	s.Stats = append(s.Stats, opt)

	// apikeys.json **没有对应表**（PLAN-sql-unify.md D4 的 12 张表里没有它）。
	// 不静默跳过：运维会以为「成功 = 全迁完了」。
	if exists(filepath.Join(dataDir, fileAPIKeys)) {
		s.Stats = append(s.Stats, importStat{
			Table:   "apikeys",
			Reasons: []string{"跳过整个文件: 12 表契约内无 apikeys 表（D4 未列），数据仍以 data/apikeys.json 为准"},
		})
	}
	// state.json 是**明确要求跳过**的（号池高频运行态，Q3 决议不进 SQL）。
	// 仍然打一行，理由同上：让「跳过」是一个可见的决策，而不是遗漏。
	if exists(filepath.Join(dataDir, fileState)) {
		s.Stats = append(s.Stats, importStat{
			Table:   "state",
			Reasons: []string{"跳过整个文件: 号池运行态按 Q3 决议不进 SQL，仍由 data/state.json + Redis 快照承载"},
		})
	}
	return s, nil
}

// importUsers 导入 accounts.json。
func importUsers(db *gorm.DB, dataDir string, stderr io.Writer) (importStat, error) {
	st := importStat{Table: "users"}
	raw, err := readJSONArray(filepath.Join(dataDir, fileAccounts))
	if err != nil {
		return st, fmt.Errorf("读取 %s: %w", fileAccounts, err)
	}
	if raw == nil {
		return st, nil // 文件不存在 = 没东西可导，不是错误
	}

	rows := []store.User{}
	for i, item := range raw {
		var a accountJSON
		if err := json.Unmarshal(item, &a); err != nil {
			st.Skipped++
			st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: JSON 解析失败（%v）", i+1, err))
			continue
		}
		u, reason := accountToUser(a)
		if reason != "" {
			st.Skipped++
			st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: %s", i+1, reason))
			continue
		}
		rows = append(rows, u)
	}

	imported, skipped := upsertUsers(db, rows, &st, stderr)
	st.Imported += imported
	st.Skipped += skipped
	return st, nil
}

// accountToUser 做行级校验 + 类型换算。返回非空 reason 表示这行该跳过。
//
// 判据只有三条，每条都对应一种真实会遇到的坏数据：
//
//	id 为空            → 无法作为主键，upsert 会把它当新行反复插入
//	username 全空      → users.username 有 UNIQUE，空串会让第二条坏行直接撞约束
//	created_at 不可解析 → 排序键坏了，导出顺序就不稳定（「逐字节等价」失守）
func accountToUser(a accountJSON) (store.User, string) {
	if strings.TrimSpace(a.ID) == "" {
		return store.User{}, "id 为空"
	}
	if strings.TrimSpace(a.Username) == "" {
		return store.User{}, "username 为空（UNIQUE 列不接受空值）"
	}
	created, err := parseRFC3339(a.CreatedAt)
	if err != nil {
		return store.User{}, fmt.Sprintf("created_at 无法解析（%q）", a.CreatedAt)
	}
	lastLogin, err := parseRFC3339(a.LastLoginAt)
	if err != nil {
		// 登录时间坏掉不该让账号迁不过去：它是审计信息，不是身份信息。
		// 归零并继续 —— 代价是「这个号上次何时登录」丢了，
		// 远小于「这个号整体丢了」。
		lastLogin = 0
	}
	return store.User{
		ID:           a.ID,
		Username:     a.Username,
		DisplayName:  a.DisplayName,
		PasswordHash: a.PasswordHash,
		Role:         a.Role,
		Enabled:      a.Enabled,
		AuthVersion:  a.AuthVersion,
		CreatedAt:    created,
		LastLoginAt:  lastLogin,
		LastLoginIP:  a.LastLoginIP,
	}, ""
}

// upsertUsers 批量 upsert；失败时**逐条重试**以定位坏行。
//
// # 为什么不是「批量失败就整体报错」
//
// 批量写入里只要有一条撞了 UNIQUE（比如两个不同 ID 同名），整批都会失败。
// 直接报错返回，用户看到的是「第 7 条违反唯一约束」，然后**修不动**：
// 他得自己二分找出是哪条。逐条重试把这一步自动化了 —— 好的进库、坏的进
// Reasons，正好落在「跳过坏行、其余全进」的验收口径上。
func upsertUsers(db *gorm.DB, rows []store.User, st *importStat, stderr io.Writer) (imported, skipped int) {
	if len(rows) == 0 {
		return 0, 0
	}
	err := db.Clauses(clause.OnConflict{UpdateAll: true}).CreateInBatches(rows, 100).Error
	if err == nil {
		return len(rows), 0
	}
	fmt.Fprintf(stderr, "migrate-sql: users 批量写入失败，改为逐条定位: %v\n", err)
	for i := range rows {
		if e := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&rows[i]).Error; e != nil {
			skipped++
			st.Reasons = append(st.Reasons, fmt.Sprintf("users %s: 写入失败（%v）", rows[i].ID, e))
			continue
		}
		imported++
	}
	return imported, skipped
}

// importSessions 导入 sessions.json。
func importSessions(db *gorm.DB, dataDir string, stderr io.Writer) (importStat, error) {
	st := importStat{Table: "user_sessions"}
	raw, err := readJSONArray(filepath.Join(dataDir, fileSessions))
	if err != nil {
		return st, fmt.Errorf("读取 %s: %w", fileSessions, err)
	}
	if raw == nil {
		return st, nil
	}

	rows := []store.UserSession{}
	// 两个字段在契约表里没有列。这里**不**冒充「都存下了」：数一下有多少行
	// 真的带了这些值，写进 Reasons 让运维看见损失面（是事实，不是错误）。
	var withVersion, withHardExpiry int
	for i, item := range raw {
		var s sessionJSON
		if err := json.Unmarshal(item, &s); err != nil {
			st.Skipped++
			st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: JSON 解析失败（%v）", i+1, err))
			continue
		}
		if strings.TrimSpace(s.SID) == "" {
			st.Skipped++
			st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: sid 为空", i+1))
			continue
		}
		if strings.TrimSpace(s.UserID) == "" {
			st.Skipped++
			st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: user_id 为空", i+1))
			continue
		}
		if s.Version != 0 {
			withVersion++
		}
		if s.HardExpiresAt != 0 {
			withHardExpiry++
		}
		rows = append(rows, store.UserSession{
			ID:           s.SID,
			UserID:       s.UserID,
			AuthVersion:  s.AuthVersion,
			CreatedAt:    s.CreatedAt,
			LastActiveAt: s.LastActiveAt,
			ExpiresAt:    s.ExpiresAt,
			IP:           s.IP,
			UserAgent:    s.UA,
			// Status=1（活跃）：盘上老数据没有这个字段 ⇒ 一律视为活跃。
			// 已过期的会话在这里**不**过滤：迁移工具不做业务判断，
			// 过期清理是运行期 account.SessionStore 加载时干的事。
			Status: 1,
		})
	}

	if len(rows) > 0 {
		if err := db.Clauses(clause.OnConflict{UpdateAll: true}).CreateInBatches(rows, 100).Error; err != nil {
			fmt.Fprintf(stderr, "migrate-sql: user_sessions 批量写入失败: %v\n", err)
			// 会话丢失的代价远小于账号丢失（重新登录即可），所以这里不逐条重试，
			// 整体计入跳过并说明原因 —— 但**必须**报出来，不能静默。
			st.Skipped += len(rows)
			st.Reasons = append(st.Reasons, fmt.Sprintf("整批 %d 条写入失败: %v", len(rows), err))
			rows = nil
		}
	}
	st.Imported = len(rows)

	if withVersion > 0 {
		st.Reasons = append(st.Reasons, fmt.Sprintf(
			"%d 条带 version，但 user_sessions 无该列 ⇒ 未迁移（恢复后该值回落 0）", withVersion))
	}
	if withHardExpiry > 0 {
		st.Reasons = append(st.Reasons, fmt.Sprintf(
			"%d 条带 hard_expires_at，但 user_sessions 无该列 ⇒ 未迁移（⚠ 恢复后会话失去绝对上限）", withHardExpiry))
	}
	return st, nil
}

// importOptions 从 config.json 抽可热改键写进 options 表。
//
// 委托给 store.ImportFromConfigJSON（契约里已定）：它保证「首次导入、
// 二次不重复写」。这条幂等规则若在这里再写一遍，两处实现必然漂移。
func importOptions(dataDir string, stderr io.Writer) importStat {
	st := importStat{Table: "options"}
	cfgPath := filepath.Join(dataDir, fileConfig)
	if !exists(cfgPath) {
		return st
	}
	n, err := store.ImportFromConfigJSON(cfgPath)
	if err != nil {
		// options 导入失败不该让整次迁移失败：config.json 仍在盘上，
		// 而账号与会话才是不可再生的数据。所以降级成一条跳过记录。
		fmt.Fprintf(stderr, "migrate-sql: options 导入失败（config.json 仍在，不影响其它表）: %v\n", err)
		st.Skipped++
		st.Reasons = append(st.Reasons, fmt.Sprintf("config.json 导入失败: %v", err))
		return st
	}
	st.Imported = n
	return st
}

// ── verify ──────────────────────────────────────────────────────────────

// verifyRow 一行核对结果。
type verifyRow struct {
	What   string
	InJSON int
	InSQL  int64
	Note   string
}

func (v verifyRow) ok() bool { return int64(v.InJSON) == v.InSQL }

func runVerify(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("verify", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var c commonFlags
	c.register(fset)
	if err := fset.Parse(args); err != nil {
		return exitUsage
	}
	c.resolve()

	if !exists(c.dbPath) {
		fmt.Fprintf(stderr, "migrate-sql verify: 数据库不存在: %s（先跑 import）\n", c.dbPath)
		return exitFailure
	}
	db, err := openStore(c.dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "migrate-sql verify: %v\n", err)
		return exitFailure
	}
	defer func() { _ = store.Close() }()

	rows, err := verifyAll(db, c.dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "migrate-sql verify: %v\n", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "%-14s %8s %8s  %s\n", "表", "JSON", "SQL", "结果")
	bad := 0
	for _, r := range rows {
		verdict := "OK"
		if !r.ok() {
			verdict = "不一致"
			bad++
		}
		if r.Note != "" {
			verdict += "  (" + r.Note + ")"
		}
		fmt.Fprintf(stdout, "%-14s %8d %8d  %s\n", r.What, r.InJSON, r.InSQL, verdict)
	}
	if bad > 0 {
		fmt.Fprintf(stdout, "校验失败: %d 项不一致\n", bad)
		return exitMismatch
	}
	fmt.Fprintln(stdout, "校验通过")
	return exitOK
}

// verifyAll 核对两件事。
//
// # 为什么按 ID 比对，而不是比对总行数
//
// 总行数会被「库里原本就有别的行」污染（运行期又写了新会话）——
// 那时总行数比 JSON 多，看着像「迁移失败」，其实完全正常。
// 按「JSON 里认得的那些 ID 在不在库里」比对，语义唯一：
// **这次迁移是否把它该搬的都搬到了**。
func verifyAll(db *gorm.DB, dataDir string) ([]verifyRow, error) {
	rows := []verifyRow{}

	accRaw, err := readJSONArray(filepath.Join(dataDir, fileAccounts))
	if err != nil {
		return nil, err
	}
	if accRaw != nil {
		accIDs := []string{}
		for _, item := range accRaw {
			var a accountJSON
			if err := json.Unmarshal(item, &a); err != nil {
				continue // 坏行导入时就跳过了，核对时同样不计入分母
			}
			if _, reason := accountToUser(a); reason != "" {
				continue
			}
			accIDs = append(accIDs, a.ID)
		}
		n, err := countRows(db, &store.User{}, "id", accIDs)
		if err != nil {
			return nil, err
		}
		rows = append(rows, verifyRow{What: "users", InJSON: len(accIDs), InSQL: n})
	}

	sesRaw, err := readJSONArray(filepath.Join(dataDir, fileSessions))
	if err != nil {
		return nil, err
	}
	if sesRaw != nil {
		sesIDs := []string{}
		for _, item := range sesRaw {
			var s sessionJSON
			if err := json.Unmarshal(item, &s); err != nil {
				continue
			}
			if strings.TrimSpace(s.SID) == "" || strings.TrimSpace(s.UserID) == "" {
				continue
			}
			sesIDs = append(sesIDs, s.SID)
		}
		n, err := countRows(db, &store.UserSession{}, "id", sesIDs)
		if err != nil {
			return nil, err
		}
		rows = append(rows, verifyRow{
			What:   "user_sessions",
			InJSON: len(sesIDs),
			InSQL:  n,
			Note:   "丢 version/hard_expires_at（无列）",
		})
	}
	return rows, nil
}

// countRows 数「这些 ID 里有多少在库里」。
//
// 分片查询：SQLite 的 SQL 变量上限默认 999，一次塞进上万个 ID 会直接报错。
// 用 500 一片（留余量给将来可能加的其它条件）。
func countRows(db *gorm.DB, model any, col string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	const chunk = 500
	var total int64
	for i := 0; i < len(ids); i += chunk {
		j := i + chunk
		if j > len(ids) {
			j = len(ids)
		}
		var n int64
		if err := db.Model(model).Where(col+" IN ?", ids[i:j]).Count(&n).Error; err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// ── export ──────────────────────────────────────────────────────────────

func runExport(args []string, stdout, stderr io.Writer) int {
	fset := flag.NewFlagSet("export", flag.ContinueOnError)
	fset.SetOutput(stderr)
	var c commonFlags
	c.register(fset)
	outDir := fset.String("out", "", "导出目录；空 = <data>/export（**不写 data 目录本身**）")
	allowLossy := fset.Bool("allow-lossy", false, "允许导出行数/字段有损的文件（sessions）")
	if err := fset.Parse(args); err != nil {
		return exitUsage
	}
	c.resolve()
	if *outDir == "" {
		*outDir = filepath.Join(c.dataDir, "export")
	}

	// 安全闸：绝不让导出覆盖到 data 目录里的正本。
	//
	// # 为什么这是硬闸而不是警告
	//
	// 「导出」的心理模型是只读的（从库里拿出来），但实现是**写文件**。
	// 用户很自然会写 --out data（"导回原来的地方"），而那会把 data/*.json
	// 全部覆盖 —— 本工具的核心承诺「不删不改任何 data/*.json」当场破功，
	// 且破在一个**看起来成功**的操作里。所以直接拒绝，且不设 --force 后门：
	// 真想覆盖请自己 cp 目录，别指望迁移工具替你决定。
	same, err := samePath(*outDir, c.dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "migrate-sql export: %v\n", err)
		return exitFailure
	}
	if same {
		fmt.Fprintf(stderr, "migrate-sql export: 拒绝把导出写到 data 目录本身（会覆盖正本）: %s\n", *outDir)
		return exitUsage
	}

	if !exists(c.dbPath) {
		fmt.Fprintf(stderr, "migrate-sql export: 数据库不存在: %s\n", c.dbPath)
		return exitFailure
	}
	db, err := openStore(c.dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "migrate-sql export: %v\n", err)
		return exitFailure
	}
	defer func() { _ = store.Close() }()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "migrate-sql export: 建目录失败: %v\n", err)
		return exitFailure
	}

	nAcc, err := exportAccounts(db, *outDir)
	if err != nil {
		fmt.Fprintf(stderr, "migrate-sql export: accounts: %v\n", err)
		return exitFailure
	}
	// sessions 的有损是**契约缺口**，不是本工具的疏忽。默认拒绝写：
	// 一个「看起来成功」的有损备份比明确失败危险得多。
	if !*allowLossy {
		fmt.Fprintf(stdout, "导出 accounts=%d sessions=0 → %s\n", nAcc, *outDir)
		fmt.Fprintf(stderr,
			"migrate-sql export: sessions 未导出 —— user_sessions 表缺 version / hard_expires_at 两列，\n"+
				"  导出的会话会失去绝对过期上限（滑动续期可无限延长），属于安全退化。\n"+
				"  确需导出请加 --allow-lossy；根治要在 store.UserSession 补这两列。\n")
		return exitOK
	}
	fmt.Fprintf(stderr, "migrate-sql export: ⚠ --allow-lossy 生效：sessions 导出将丢失 version / hard_expires_at\n")
	nSes, err := exportSessions(db, *outDir)
	if err != nil {
		fmt.Fprintf(stderr, "migrate-sql export: sessions: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "导出 accounts=%d sessions=%d → %s\n", nAcc, nSes, *outDir)
	return exitOK
}

// exportAccounts 导出 users 表。**这是唯一能做到逐字节等价的文件。**
func exportAccounts(db *gorm.DB, outDir string) (int, error) {
	users := []store.User{}
	if err := db.Order("created_at asc").Order("id asc").Find(&users).Error; err != nil {
		return 0, err
	}
	arr := make([]accountJSON, 0, len(users))
	for _, u := range users {
		arr = append(arr, accountJSON{
			ID:           u.ID,
			Username:     u.Username,
			DisplayName:  u.DisplayName,
			PasswordHash: u.PasswordHash,
			Role:         u.Role,
			Enabled:      u.Enabled,
			AuthVersion:  u.AuthVersion,
			CreatedAt:    formatRFC3339(u.CreatedAt),
			LastLoginAt:  formatRFC3339(u.LastLoginAt),
			LastLoginIP:  u.LastLoginIP,
		})
	}
	// 「键序按 CreatedAt 稳定排序」：created_at 是主判据，id 是次判据。
	// 只按 created_at 排，会在同秒记录上依赖 SQLite 的返回顺序（不保证稳定），
	// 于是同一份数据两次导出可能字节不同 —— 那会让「逐字节等价」变成玄学。
	if err := writeJSONArray(filepath.Join(outDir, fileAccounts), arr); err != nil {
		return 0, err
	}
	return len(arr), nil
}

// exportSessions 导出 user_sessions 表（有损，见 sessionJSON 的注释）。
//
// username/role 从 users 表回填：本仓用户名与角色**不可变**（store.go 的
// 「用户名不支持改名」+ 单账号管理员只有 RoleAdmin 一个可写角色），
// 所以「签发时的快照」与「现在的值」必然相同，回填安全。
// 反过来说，将来若支持改名，这里会静默导出错误的历史快照 ——
// 那时必须改成在表里存快照列，而不是继续 join。
func exportSessions(db *gorm.DB, outDir string) (int, error) {
	ses := []store.UserSession{}
	if err := db.Order("created_at asc").Order("id asc").Find(&ses).Error; err != nil {
		return 0, err
	}
	users := []store.User{}
	if err := db.Find(&users).Error; err != nil {
		return 0, err
	}
	byID := make(map[string]store.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}

	arr := make([]sessionJSON, 0, len(ses))
	for _, s := range ses {
		u := byID[s.UserID]
		arr = append(arr, sessionJSON{
			SID:          s.ID,
			UserID:       s.UserID,
			Username:     u.Username,
			Role:         u.Role,
			AuthVersion:  s.AuthVersion,
			CreatedAt:    s.CreatedAt,
			LastActiveAt: s.LastActiveAt,
			ExpiresAt:    s.ExpiresAt,
			IP:           s.IP,
			UA:           s.UserAgent,
			// Version / HardExpiresAt 无列可存 ⇒ 保持零值。
			// 刻意**不猜**（比如用 created_at+30d 反推 hard_expires_at）：
			// 那个常量将来会改，猜出来的值会把「没有上限」伪装成「有上限」。
		})
	}
	if err := writeJSONArray(filepath.Join(outDir, fileSessions), arr); err != nil {
		return 0, err
	}
	return len(arr), nil
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readJSONArray 读一个 JSON 数组文件的**每个元素原始字节**。
//
// 返回 nil, nil 表示文件不存在（缺省即「没东西可导」，不是错误）：
// 迁移工具会在只配了账号库的部署里跑，缺 sessions.json 是常态。
//
// 用 []json.RawMessage 而不是直接解码到结构体切片，是为了让**一条坏记录
// 不拖垮整个文件**：直接 Unmarshal 到 []T 时，任何一条类型不符都会让整个
// 数组解析失败，于是「跳过坏行」无从谈起 —— 而那是验收的第一条。
func readJSONArray(path string) ([]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("不是合法的 JSON 数组: %w", err)
	}
	return arr, nil
}

// writeJSONArray 原子写（临时文件 + rename），格式与 internal/account 的
// saveLocked 逐字一致：json.MarshalIndent(arr, "", "  ")、无末尾换行、0600。
//
// ⚠ 这三处必须同时一致，否则「逐字节等价」差的就是这几块钱：
//   - MarshalIndent 而不是 Encoder（Encoder 会追加 '\n'）
//   - 两个空格缩进（不是 tab）
//   - 0600（导出文件里有 password_hash）
func writeJSONArray(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// parseRFC3339 把盘上的字符串时间转成 unix 秒；空串 → 0（=「没有这个时间」）。
func parseRFC3339(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// formatRFC3339 反向转换。**必须**用 UTC：盘上的账号文件是 UTC（Z 结尾），
// 用本地时区格式化会得到 +08:00 后缀，字节立刻不等。
//
// ⚠ 已知边界：源文件若原本是 +08:00 形式，转成 unix 再转回来会变成 Z ——
// 语义相同、字节不同。accounts.json 目前全是 Z（已核对 364 字节那份），
// 而 apikeys.json 那条 +08:00 不参与迁移（无对应表），所以这里不做保留
// 偏移量的额外处理；真要保留，应当把原串一并存列，而不是在这里猜时区。
func formatRFC3339(n int64) string {
	if n == 0 {
		return ""
	}
	return time.Unix(n, 0).UTC().Format(time.RFC3339)
}

// samePath 判断两个路径是否指向同一目录（防 --out data 覆盖正本）。
func samePath(a, b string) (bool, error) {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	if filepath.Clean(aa) == filepath.Clean(bb) {
		return true, nil
	}
	// 软链 / 大小写 / Junction：拿到真实路径再比一次。
	// 目录可能还不存在（导出会新建它），那就退回 Abs 比较 ——
	// 不存在的路径不可能是正本的别名。
	ra, ea := filepath.EvalSymlinks(aa)
	rb, eb := filepath.EvalSymlinks(bb)
	if ea != nil || eb != nil {
		return false, nil
	}
	return filepath.Clean(ra) == filepath.Clean(rb), nil
}

func openStore(dbPath string) (*gorm.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("建库目录失败: %w", err)
	}
	if err := store.Open(dbPath); err != nil {
		return nil, fmt.Errorf("打开数据库 %s: %w", dbPath, err)
	}
	if err := store.Migrate(); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	if store.DB == nil {
		_ = store.Close()
		return nil, errors.New("store.Open 成功但 store.DB 为 nil（契约被破坏）")
	}
	return store.DB, nil
}

func printStats(w io.Writer, s *summary) {
	for _, st := range s.Stats {
		fmt.Fprintf(w, "[%s] 导入 %d/跳过 %d\n", st.Table, st.Imported, st.Skipped)
		for _, r := range st.Reasons {
			fmt.Fprintf(w, "    - %s\n", r)
		}
	}
}

// inspectDataDir 是 --dry-run 的实现：只解析校验，不碰数据库。
//
// 它的存在理由不是「方便」而是**安全**：迁移前先看一遍「会导入什么、跳过什么」，
// 比跑完再 verify 更早暴露坏数据，而且完全没有副作用。
func inspectDataDir(dataDir string) (*summary, error) {
	s := &summary{Stats: []importStat{}}

	accRaw, err := readJSONArray(filepath.Join(dataDir, fileAccounts))
	if err != nil {
		return nil, err
	}
	if accRaw != nil {
		st := importStat{Table: "users"}
		for i, item := range accRaw {
			var a accountJSON
			if err := json.Unmarshal(item, &a); err != nil {
				st.Skipped++
				st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: JSON 解析失败（%v）", i+1, err))
				continue
			}
			if _, reason := accountToUser(a); reason != "" {
				st.Skipped++
				st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: %s", i+1, reason))
				continue
			}
			st.Imported++
		}
		s.Stats = append(s.Stats, st)
	}

	sesRaw, err := readJSONArray(filepath.Join(dataDir, fileSessions))
	if err != nil {
		return nil, err
	}
	if sesRaw != nil {
		st := importStat{Table: "user_sessions"}
		for i, item := range sesRaw {
			var x sessionJSON
			if err := json.Unmarshal(item, &x); err != nil {
				st.Skipped++
				st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: JSON 解析失败（%v）", i+1, err))
				continue
			}
			if strings.TrimSpace(x.SID) == "" {
				st.Skipped++
				st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: sid 为空", i+1))
				continue
			}
			if strings.TrimSpace(x.UserID) == "" {
				st.Skipped++
				st.Reasons = append(st.Reasons, fmt.Sprintf("第%d条: user_id 为空", i+1))
				continue
			}
			st.Imported++
		}
		s.Stats = append(s.Stats, st)
	}
	return s, nil
}

func (s *summary) describe() string {
	parts := []string{}
	for _, st := range s.Stats {
		parts = append(parts, fmt.Sprintf("%s(%d)", st.Table, st.Imported))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
