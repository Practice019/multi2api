// models.go 12 张表的 GORM 模型定义。
//
// # 为什么把 struct 定义单独一个文件，而不是散在各业务文件里
//
// AutoMigrate 的输入是**这一组 struct 的集合**（见 db.go 的 Tables）。把集合
// 放在同一个文件里，读者能一眼看全"库里到底有哪些表、每张表长什么样" ——
// 这正是 PLAN-sql-unify §2 D4 那张表要回答的问题。
// 反过来，若每张 struct 跟着业务代码走，就没有任何一处能回答这个问题。
//
// # 命名策略：显式 TableName，不靠 GORM 的复数化猜测
//
// GORM 默认把 struct 名转成 snake_case **复数**（User → users）。本仓多数表恰好
// 是复数（users / options / channels / vendors），但依赖"巧合"是最坏的选择：
// 复数化表里有 has→hashes / box→boxes 这类不规则变化，意味着**加一张新表就
// 可能悄悄变名**。而表名是**库契约**（运维要能直接 sqlite3 查、备份脚本要能
// 按名字对账），不该由框架的命名推断决定。
//
// 尤其是 audit_log：GORM 会推成 audit_logs，而契约要求单数。这一条足以证明
// "必须显式"。所以本文件给每张表都写明 TableName。
//
// # 为什么全部时间字段是 int64 Unix 秒、而不是 time.Time
//
// 本仓既有的持久化格式（data/*.json、data/state.json）用的就是 Unix 秒。
// SQL 层沿用同一口径有三个具体好处：
//
//   - 迁移时**不需要**任何时区换算。SQLite 没有原生时间类型，driver 会按字符串
//     存 time.Time；跨时区读回来就偏了 —— 这是最难查的一类 bug（数据"看起来是对的"）。
//   - 聚合查询直接比大小（WHERE created_at >= ?），不用把索引列包进函数。
//   - 与 chatError/日志里的时间戳同源，排查时不用心算两种表示。
//
// # 为什么每张表都有 gorm 标签
//
// AutoMigrate 只认标签，不认注释。类型/索引/唯一约束一旦漏写，入库会**悄悄**
// 建出一张"看起来能用"的表，直到需要索引的那天才发现全表扫描。
// 因此下面每个索引都对应一个**真实查询**，注释里写明了是哪一个。
package store

// Setup 初始化单例表（约定恒为 ID=1）。
//
// 由 setup.go 使用：CheckSetup 找不到行但存在 root 用户时**自愈补行**（PLAN §0.1）。
type Setup struct {
	ID uint `gorm:"primaryKey"`
	// Version 写入时的程序版本（排查"这次初始化是哪个版本做的"）。
	Version string `gorm:"type:varchar(32);not null;default:''"`
	// InitializedAt 刻意不用 GORM 的 autoCreateTime（那是 time.Time，见文件头）。
	InitializedAt int64 `gorm:"not null;default:0"`
}

// TableName 见文件头「命名策略」。
func (Setup) TableName() string { return "setups" }

// User 控制台用户。
//
// ⚠ 这是**控制台登录账号**，不是上游账号池里的号（那是 Account）。
type User struct {
	// ID 字符串主键（不是自增）：data/accounts.json 里已有 ID，迁移要能
	// **逐键保留**（PLAN §7 第 6 条）。换成自增会丢掉这层对应关系，
	// 迁移就变成"只能看总量对不对"。
	ID string `gorm:"type:varchar(64);primaryKey"`
	// Username 唯一。这条约束放在库里而不是代码里，才能挡住并发注册与手工改库。
	Username string `gorm:"type:varchar(64);not null;uniqueIndex"`
	DisplayName string `gorm:"type:varchar(64);not null;default:''"`
	// PasswordHash 只存哈希；本层不解释算法，只承诺"这里不是明文"。
	PasswordHash string `gorm:"type:varchar(255);not null;default:''"`
	// Role 1=member(历史) / 10=admin / 100=root，见 PLAN §0.1。
	Role int `gorm:"not null;default:1"`
	Enabled bool `gorm:"not null;default:true"`
	// AuthVersion 会话失效版本号：改密/登出全设备时自增，会话行记录签发时的
	// 版本，两者不等即视为失效。
	//
	// ⚠ 语义必须与现有 account 包完全一致（PLAN §3 Wave2 的"AuthVersion 语义保持"）。
	// 若把它当成普通计数字段，改密就踢不掉旧会话 —— 那是安全问题，不是显示问题。
	AuthVersion int64 `gorm:"not null;default:0"`
	CreatedAt   int64 `gorm:"not null;default:0"`
	LastLoginAt int64 `gorm:"not null;default:0"`
	LastLoginIP string `gorm:"type:varchar(64);not null;default:''"`
}

func (User) TableName() string { return "users" }

// UserSession 登录会话。
type UserSession struct {
	// ID 会话 ID（随机串）。字符串主键：现有 sessions.json 里就是它。
	ID string `gorm:"type:varchar(64);primaryKey"`
	// UserID 索引：按用户查"我的会话"，以及登出全设备时批量删除。
	UserID string `gorm:"type:varchar(64);not null;index:idx_session_user"`
	// AuthVersion 签发时的用户版本。与 users.auth_version 不等 ⇒ 该会话已失效。
	AuthVersion int64 `gorm:"not null;default:0"`
	CreatedAt   int64 `gorm:"not null;default:0"`
	// LastActiveAt 滑动续期用。
	LastActiveAt int64 `gorm:"not null;default:0"`
	// ExpiresAt 索引：清理过期会话的扫描（WHERE expires_at < now）。
	ExpiresAt int64  `gorm:"not null;default:0;index:idx_session_expires"`
	IP        string `gorm:"type:varchar(64);not null;default:''"`
	UserAgent string `gorm:"type:varchar(512);not null;default:''"`
	// Status 0=有效；其余取值由上层定义，本层不解释。
	Status int `gorm:"not null;default:0"`
}

func (UserSession) TableName() string { return "user_sessions" }

// Option 设置键值（从 config.json 的**可热改**部分首次导入）。
type Option struct {
	// Key 是 SQLite 关键字，必须靠 GORM 的引号转义。
	//
	// ⚠ 不要为了"省掉引号问题"把它改名成 option_key：表结构要对齐参考项目的
	// options(key, value)，改名会让两边无法对账。代价只是"所有 SQL 都走 GORM"。
	Key   string `gorm:"type:varchar(128);primaryKey"`
	Value string `gorm:"type:text;not null;default:''"`
}

func (Option) TableName() string { return "options" }

// Channel 渠道（代码内 provider 的**数据化**形态，PLAN §1 G5）。
type Channel struct {
	ID      int64  `gorm:"primaryKey;autoIncrement"`
	Name    string `gorm:"type:varchar(64);not null;default:''"`
	BaseURL string `gorm:"type:varchar(255);not null;default:''"`
	// KeyMasked **只存掩码**，不存明文 key。
	//
	// # 为什么这条注释值得写
	//
	// 表一旦建立，"顺手把 key 也存进来"几乎没有阻力 —— 而 .db 文件是可以被
	// 复制/备份/贴进工单的，比 config.json 更容易外流。凭证继续留在凭证文件/
	// 环境变量里，库里只放"这是哪把 key"的提示。
	KeyMasked string `gorm:"type:varchar(64);not null;default:''"`
	// ModelsCSV 逗号分隔的模型名（对齐参考项目的 models 列形态）。
	ModelsCSV string `gorm:"type:text;not null;default:''"`
	// Status 1=启用 / 2=手动禁用 / 3=自动禁用（语义对齐参考项目）。
	// 索引：渠道页按状态筛选。
	Status int `gorm:"not null;default:1;index:idx_channel_status"`
	// Priority 越小越优先；Weight 同优先级内加权轮询。
	Priority  int   `gorm:"not null;default:0"`
	Weight    int   `gorm:"not null;default:0"`
	CreatedAt int64 `gorm:"not null;default:0"`
}

func (Channel) TableName() string { return "channels" }

// Ability 模型 ↔ 渠道的绑定（PLAN §1 G7）。
//
// # 为什么是一张独立表，而不是 channels 里的一个 CSV 列
//
// 要回答的两个问题都要求按**模型**查：
//
//	"这个模型由哪些渠道提供" → WHERE model = ?
//	"这个渠道能提供哪些模型" → WHERE channel_id = ?
//
// CSV 列只能回答后者，且要做全表 LIKE 扫描。所以按参考项目的做法展开成行。
type Ability struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// Group 分组（本仓只有一个默认分组，保留该列以对齐参考项目）。
	//
	// ⚠ Group 是 SQLite 关键字，靠 GORM 转义 —— 因此插入必须走 GORM，
	// 不要手写裸 SQL（裸 SQL 里它要么语法错，要么被当成列别名）。
	//
	// 三列组成复合唯一索引：同一个"分组+模型+渠道"只该有一条绑定。
	// 没有它，重复 seed 会让"这个模型由几个渠道提供"多算。
	Group string `gorm:"type:varchar(64);not null;default:'';index:idx_ability_group;uniqueIndex:idx_ability_unique,priority:1"`
	Model string `gorm:"type:varchar(128);not null;default:'';index:idx_ability_model;uniqueIndex:idx_ability_unique,priority:2"`
	// ChannelID 索引：按渠道反查能力（删渠道时要能一次删干净）。
	ChannelID int64 `gorm:"not null;default:0;index:idx_ability_channel;uniqueIndex:idx_ability_unique,priority:3"`
	Enabled   bool  `gorm:"not null;default:true"`
}

func (Ability) TableName() string { return "abilities" }

// Model 模型目录（PLAN §1 G6）。
type Model struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// Name 唯一：模型名是选号与统计的标识，重复会让"这个模型归谁"无法回答。
	Name string `gorm:"type:varchar(128);not null;uniqueIndex"`
	// VendorID 供应商。**刻意不加外键约束**，见 db.go 的
	// DisableForeignKeyConstraintWhenMigrating 注释。
	VendorID int64 `gorm:"not null;default:0;index:idx_model_vendor"`
	// EndpointsCSV 该模型支持的端点（chat / embeddings / images…）。
	EndpointsCSV string `gorm:"type:text;not null;default:''"`
	Description  string `gorm:"type:text;not null;default:''"`
}

func (Model) TableName() string { return "models" }

// Vendor 供应商分组。
type Vendor struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	Name        string `gorm:"type:varchar(64);not null;uniqueIndex"`
	Description string `gorm:"type:text;not null;default:''"`
}

func (Vendor) TableName() string { return "vendors" }

// Log 请求日志 —— **系统状态页指标的唯一来源**（PLAN §2 D5）。
//
// # 为什么只有时间戳必须建索引
//
// 三个聚合查询（今日 / 近 5 分钟 / 任意窗口）**全部**带时间范围条件。
// 没有索引时每条都是全表扫描，而这张表是增长最快的表（每次请求一行），
// 全表扫描会在部署几周后变成页面卡顿。
type Log struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// CreatedAt Unix 秒，索引（见上）。
	CreatedAt int64 `gorm:"not null;default:0;index:idx_log_created"`
	// ModelName / ChannelName 存**名字**而不是 id。
	//
	// # 为什么不做外键连表
	//
	// 日志是历史事实：渠道被删掉之后，那条日志仍然该显示"当时是哪个渠道"。
	// 存 id 会让历史日志在删渠道后显示空白（或者被迫软删除才能保住名字）。
	ModelName   string `gorm:"type:varchar(128);not null;default:''"`
	ChannelName string `gorm:"type:varchar(64);not null;default:''"`
	// AccountUID 上游账号池里的号（本仓独有维度）。
	AccountUID       string `gorm:"type:varchar(64);not null;default:''"`
	PromptTokens     int    `gorm:"not null;default:0"`
	CompletionTokens int    `gorm:"not null;default:0"`
	TotalTokens      int    `gorm:"not null;default:0"`
	// CostQuota 消费额度（本仓无计费，但免费额度"消耗了多少"仍要记账）。
	CostQuota int64 `gorm:"not null;default:0"`
	// LatencyMs 单请求耗时，聚合里的 avg_latency 来自它（PLAN §2 D5：AVG 不是 P95）。
	LatencyMs int64 `gorm:"not null;default:0"`
	Success   bool  `gorm:"not null;default:false"`
	// ErrKind 失败时的错误分类（与 gateway.ErrorKind.String() 对齐，便于与日志正文对照）。
	ErrKind string `gorm:"type:varchar(32);not null;default:''"`
	// Type 索引：今日消费只算 type=consume 的行（PLAN §2 D5）。
	//
	// ⚠ 它与 idx_log_created 是两个独立索引，**不是**复合索引 (type, created_at)：
	// 两个使用场景的最左列不同（"全部类型的今日请求数"用不上以 type 打头的复合索引，
	// "某类型的消费"用不上以 created_at 打头的）。单列索引让优化器自己挑。
	Type string `gorm:"type:varchar(16);not null;default:'';index:idx_log_type"`
}

func (Log) TableName() string { return "logs" }

// Account 上游账号池里的号（本仓独有，PLAN §0.2 保留为第一等公民）。
//
// # 为什么它只存**静态信息**
//
// PLAN §8 的 Q3 已定：高频写的运行态（冷却/熔断/在途数）留在 state.json +
// Redis 快照，不进 SQL。把每秒都在变的状态放进一张要 AutoMigrate、要备份、
// 要联查的表里，代价远大于收益，而且会与 Redis 快照形成两个真相源。
// 本表回答的是"池里有哪些号、归哪个上游、启用没有"。
type Account struct {
	// ID 号的标识。沿用现有 data/state.json 的键，保证迁移可逐键对账。
	ID string `gorm:"type:varchar(64);primaryKey"`
	// Provider 上游实现 id（workbuddy / codearts / …）。
	Provider string `gorm:"type:varchar(32);not null;default:'';index:idx_account_provider"`
	// UID 上游侧的账号 UID。
	//
	// ⚠ 刻意**不**加复合唯一索引 (provider, uid)：实测存在同一份凭证被导入两次的
	// 历史数据，加唯一约束会让 AutoMigrate 阶段直接失败 —— 启动即崩，而且是在
	// 用户升级时才发生。去重是迁移工具要做的校验（可报告、可人工裁决），
	// 不是库约束的职责（只会拒绝启动）。
	UID   string `gorm:"type:varchar(64);not null;default:'';index:idx_account_uid"`
	Label string `gorm:"type:varchar(64);not null;default:''"`
	Enabled    bool  `gorm:"not null;default:true"`
	CreatedAt  int64 `gorm:"not null;default:0"`
	LastUsedAt int64 `gorm:"not null;default:0"`
}

func (Account) TableName() string { return "accounts" }

// AccountEvent 号的冷却/熔断/禁用事件（PLAN §1 G3：现在只有内存 + 日志文本）。
//
// # 为什么值得单独建表
//
// "这个号为什么被禁用"是运维最常问、也最难回答的问题：现状只有一行日志文本，
// 查历史要靠 grep。存成表之后，它就是一条可查询、可排序、可关联的事实。
type AccountEvent struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// At 索引：所有查询都是"最近发生了什么"的时间窗口。
	At int64 `gorm:"not null;default:0;index:idx_acct_event_at"`
	// AccountID 索引：按号查它自己的历史。
	AccountID string `gorm:"type:varchar(64);not null;default:'';index:idx_acct_event_account"`
	// Kind 事件种类（cooldown / disable / breaker / recover …，由 pool 层定义）。
	Kind string `gorm:"type:varchar(32);not null;default:''"`
	// Reason 人类可读的原因（保留原始措辞 —— 这列存在的意义就是给人看）。
	Reason      string `gorm:"type:text;not null;default:''"`
	CooldownSec int    `gorm:"not null;default:0"`
	// Detail 结构化补充（JSON 字符串）。用 text 而不是 json 列类型：
	// SQLite 的 json 只是亲和性提示，text 换驱动/换库时更稳。
	Detail string `gorm:"type:text;not null;default:''"`
}

func (AccountEvent) TableName() string { return "account_events" }

// AuditLog 审计日志（PLAN §3 Wave2 T5）。
//
// ⚠ 表名是**单数** audit_log —— 与 GORM 的默认复数化不同。这正是文件头
// 「命名策略」那条不变量最好的反例：没有显式 TableName 就会建成 audit_logs，
// 而契约、文档、备份脚本说的都是 audit_log。
//
// # 为什么不存请求体 / 响应体
//
// 抄参考项目的判据（new-api model/audit_log.go 的 RecordAuditLog 注释）：
// "raw URLs, query strings, credentials and response/request bodies must never
// enter this table"。审计要能长期保留，而请求体里有用户内容与可能的密钥；
// 一旦进表就等于给自己造了一个"必须同样严格保护"的副本。
type AuditLog struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// EventID 全局唯一：审计写入可能被重试/重放，靠它做幂等（而不是靠条数）。
	EventID string `gorm:"type:varchar(64);not null;uniqueIndex"`
	// At 索引：审计页永远按时间倒序看。
	At int64 `gorm:"not null;default:0;index:idx_audit_at"`
	// Actor 谁做的（用户名或 token 标识，**不是**凭证本身）。
	Actor      string `gorm:"type:varchar(64);not null;default:''"`
	AuthMethod string `gorm:"type:varchar(24);not null;default:''"`
	// Category 索引：审计页按类别筛选（login / security / operation / access_token）。
	Category string `gorm:"type:varchar(24);not null;default:'';index:idx_audit_category"`
	Action   string `gorm:"type:varchar(128);not null;default:''"`
	// Params 动作参数（已脱敏的摘要，不是原始请求体）。
	Params string `gorm:"type:text;not null;default:''"`
	IP     string `gorm:"type:varchar(64);not null;default:''"`
	// UserAgent / Method / Route 记录的是**不含 query** 的路由模板。
	UserAgent string `gorm:"type:varchar(512);not null;default:''"`
	Method    string `gorm:"type:varchar(16);not null;default:''"`
	Route     string `gorm:"type:varchar(255);not null;default:''"`
	Status    int    `gorm:"not null;default:0"`
	Success   bool   `gorm:"not null;default:false"`
	Reason    string `gorm:"type:text;not null;default:''"`
}

func (AuditLog) TableName() string { return "audit_log" }
