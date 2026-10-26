// models.go 本包的 GORM 表模型定义（当前 12 张在这个文件里；第 13 张 tokens 在
// token.go，A2 落地。表清单的唯一事实来源是 db.go 的 allModels()/Tables()）。
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

// 本文件是"本文件的 12 张表 + 列的 JSON 值类型"的定义处（第 13 张 tokens 在
// token.go）。导入只为 ChannelInfo 的
// driver.Valuer / sql.Scanner 实现服务（其余 struct 不需要任何导入）。
import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

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
	Username    string `gorm:"type:varchar(64);not null;uniqueIndex"`
	DisplayName string `gorm:"type:varchar(64);not null;default:''"`
	// PasswordHash 只存哈希；本层不解释算法，只承诺"这里不是明文"。
	PasswordHash string `gorm:"type:varchar(255);not null;default:''"`
	// Role 1=member(历史) / 10=admin / 100=root，见 PLAN §0.1。
	Role    int  `gorm:"not null;default:1"`
	Enabled bool `gorm:"not null;default:true"`
	// AuthVersion 会话失效版本号：改密/登出全设备时自增，会话行记录签发时的
	// 版本，两者不等即视为失效。
	//
	// ⚠ 语义必须与现有 account 包完全一致（PLAN §3 Wave2 的"AuthVersion 语义保持"）。
	// 若把它当成普通计数字段，改密就踢不掉旧会话 —— 那是安全问题，不是显示问题。
	AuthVersion int64  `gorm:"not null;default:0"`
	CreatedAt   int64  `gorm:"not null;default:0"`
	LastLoginAt int64  `gorm:"not null;default:0"`
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
//
// # 字段面：对齐参考项目 new-api 的 model.Channel（hyxapi model/channel.go:22-58）
//
// Wave A1（2026-10-26）把本表从"最小可用"扩到**字段对等**：需求是"完全实现 hyxapi
// 的渠道管理 + API 创建全套逻辑"，而 hyxapi 的渠道行有三十来个字段，其中多 Key 状态、
// 若干覆盖项（param/header/model_mapping/status_code_mapping）都是**行内**事实。
// 字段对等是后续把渠道管理 handler 逐个照搬过来的前提 —— 否则每个 handler 都要
// 临时加列，而加列是**只加不删**的（见下）。
//
// 命名策略：Go 字段名照参考项目（Id→ID 是本仓风格），可空语义用指针表达。
// **列名优先复用本仓既有列**，这样 AutoMigrate 在既有库上只做"加列/加索引"，
// 不会触发 SQLite 的"改列 = 重建表"：
//
//	参考项目字段        本仓列名        为什么复用
//	base_url           base_url      既有列（Go 类型 string→*string，列不动）
//	models             models        **新列**（旧列 models_csv 由一次性回填搬过来；
//	                                 不搬的表现是声明静默丢失，不是报错）
//	created_time       created_at    既有列，同语义（Unix 秒）
//	priority / weight  priority / weight  既有列（Go 类型换 *int64 / *uint）
//	name / status / id name / status / id 既有列
//	其余新增字段        同名 snake_case  新列（只加不删）
//
// # D1：channels 表**存明文 key**（有意推翻本仓原设计）
//
// 原来这一列叫 key_masked，注释写着"只存掩码，不存明文"（理由：.db 会被复制/
// 备份/贴进工单，比 config.json 更容易外流）。Wave A1 **推翻了那条设计**：
//
//	· 推翻的理由：需求是"完全实现 hyxapi 的逻辑"，而 hyxapi 把 key 明文存进
//	  channels.key —— 渠道的增删改查、多 Key 状态、key 轮换、余额查询全都建立在
//	  "能读回明文"之上。只存掩码会让每个 handler 都得去另一个存储取 key，逻辑
//	  无法照搬（这正是本仓 channelstore 的 JSON 实现与 SQL 实现长期双轨的根因）。
//	· 代价（必须记住，不能只写"已改"）：
//	  ① data/gw.db 从此含明文凭据 ⇒ 文件权限必须 0600（部署侧检查），且 .db
//	     不能再随手贴进工单/截图/公开位置；
//	  ② cmd/migrate-sql export **不得导出 channels.key**（那个导出物是给人看的
//	     JSON）。今天它只导 users / user_sessions，所以这条是"以后不要加"的
//	     护栏，不是现状修复 —— 加渠道导出的人必须回来看这段。
//	· 保留的约束（照 hyxapi）：列表与详情查询**一律 Omit("key")**，明文只经
//	  GetChannelKey(id) / GetChannelKeys(id) 两个显式方法取（hyxapi 的
//	  GET /api/channel 同样 Omit("key")，明文只走 POST /:id/key）。
//	· 旧列 key_masked 不删（AutoMigrate 只加不删）：它成为**孤儿列**，本包不再
//	  读写；历史掩码值留在库里，没有任何读取路径。
//
// # Keys：计算字段，永不落库
//
// 与 hyxapi 一致：`Keys []string gorm:"-"` 只是**内存态**（多 Key 的解析结果，
// 由缓存层填）。真值永远在 Key 列 —— 普通渠道换行分隔、Vertex 是 JSON 数组（D2）。
type Channel struct {
	ID int64 `gorm:"primaryKey;autoIncrement"`
	// Type 渠道类型（参考项目的 constant.ChannelType* 数值）。
	Type int `gorm:"not null;default:0"`
	// ExternalID 本仓 **admin 层的外部可寻址 id**（形如 ch-9f3a1b2c），不是 hyxapi
	// 的对等字段（hyxapi 用数字 id 寻址）。
	//
	// # 为什么需要它（B1 的硬前置）
	//
	// internal/admin/channels.go 自己生成 ch-+8 位 hex 的字符串 id 并回给前端，
	// 前端之后编辑/停用/删除/管 Key 都用它回传。SQL 成为权威之后，主键是数字
	// 自增 id —— 没有这一列，那些回传的 ch-xxxx 必然 404。
	//
	// # 空串语义与唯一性（**这里有个 SQLite 的坑**）
	//
	// 空串 = 非外部寻址（seed 出来的渠道、内部创建的渠道都没有外部 id）。
	// ⚠ SQLite 的 UNIQUE 索引把多个空串视为**重复**（'' = '' 冲突），所以
	// **不能**在 tag 上写 uniqueIndex：那样 seed 的第二行就会违反唯一约束
	// （13 个 seed 渠道全是空串）。
	//
	// 取舍（任务书给了两条路，这里选第二条）：**非唯一列 + 手动建的部分唯一索引**
	//	CREATE UNIQUE INDEX ... ON channels(external_id) WHERE external_id <> ''
	// 理由：① 唯一性仍然由**库**保证（并发下没有应用层判重的竞态）；② 空串可以有
	// 任意多行（seed/内部渠道）；③ 字段类型保持 string —— B1 的调用点是
	// `ch.ExternalID = "ch-xxxx"`，用 *string 会逼它到处取地址。
	// 索引由 db.go 的 Migrate 每次启动幂等创建（GORM 的 tag 表达不了部分索引）。
	//
	// ⚠ 上面这段取舍**有可执行证据**，不是口头结论：`TestChannelExternalIDEmptySemantics`
	// 建一张带**普通** UNIQUE 的 scratch 表，断言「第一行空串插入成功、第二行空串报
	// UNIQUE 失败」，同时断言本表的多条空串 external_id 共存。改这一列前先读那条测试。
	ExternalID string `gorm:"type:varchar(64);not null;default:''"`
	// Key **明文**（D1）。多 Key 渠道存多把：换行分隔，或 Vertex 的 JSON 数组。
	// ⚠ 列表/详情一律 Omit("key")，只有 GetChannelKey/GetChannelKeys 会读它。
	Key                string  `gorm:"type:text;not null;default:''"`
	OpenAIOrganization *string `gorm:"type:varchar(255)"`
	TestModel          *string `gorm:"type:varchar(128)"`
	// Status 1=启用 / 2=手动禁用 / 3=自动禁用（语义对齐参考项目）。
	// 索引：渠道页按状态筛选。
	Status int `gorm:"not null;default:1;index:idx_channel_status"`
	// Name 渠道名；索引：身份键 (name, base_url) 的查询与渠道页按名搜索。
	Name string `gorm:"type:varchar(64);not null;default:'';index:idx_channel_name"`
	// Weight 同优先级内加权随机；0 = 未指定（可空语义，照参考项目用 *uint）。
	Weight *uint `gorm:"not null;default:0"`
	// CreatedTime 建行时间（Unix 秒）。列名仍复用既有的 created_at（见上表）。
	CreatedTime int64 `gorm:"column:created_at;not null;default:0"`
	// TestTime 最近一次测试时间（Unix 秒）；ResponseTime 该次测试的耗时（毫秒）。
	TestTime     int64 `gorm:"not null;default:0"`
	ResponseTime int   `gorm:"not null;default:0"`
	// BaseURL 上游端点。身份键的另一半是 Name（见 channel.go 文件头）。
	BaseURL *string `gorm:"column:base_url;type:varchar(255);not null;default:''"`
	// Other 渠道类型专属信息（Vertex 的部署地区 JSON 等）。
	Other string `gorm:"type:text;not null;default:''"`
	// Balance 上游余额（USD）；BalanceUpdatedTime 余额时间（Unix 秒）。
	Balance            float64 `gorm:"not null;default:0"`
	BalanceUpdatedTime int64   `gorm:"not null;default:0"`
	// Models 该渠道声明的模型，**逗号分隔**（本仓冻结契约；取代旧的 ModelsCSV）。
	//
	// ⚠ 迁移口径（只加不删，见 D5）：新列 models 与旧列 models_csv **并存**。
	// 首次启动时由 backfillLegacyChannelColumns 把 models_csv 的值搬进 models
	// （只在 models 为空时搬，幂等）—— 不搬的话老部署的模型声明会静默丢失，
	// 而"丢失"的表现是能力行展开不出来（渠道存在但选不到），不是报错。
	Models string `gorm:"type:text;not null;default:''"`
	// Group 该渠道服务的分组，逗号分隔。默认 default。
	// ⚠ group 是 SQLite 关键字，靠 GORM 转义 —— 不要手写裸 SQL（同 Ability.Group）。
	Group string `gorm:"type:varchar(64);not null;default:'default'"`
	// Groups 分组列表的**内存态**解析结果（由 Group 逗号分隔拆出，永不落库）。
	// 与 Keys 同一模式：真值永远是那一列 CSV，本字段只省下每个调用方自己拆一遍。
	Groups []string `gorm:"-"`
	// UsedQuota 累计消耗额度。
	UsedQuota int64 `gorm:"not null;default:0"`
	// ModelMapping 对外模型名 → 上游模型名的映射（JSON）。
	ModelMapping *string `gorm:"type:text"`
	// StatusCodeMapping 上游状态码 → 本地语义的映射（JSON）。
	StatusCodeMapping *string `gorm:"type:varchar(1024);not null;default:''"`
	// Priority 越大越先用（参考项目口径）；可空语义用 *int64。
	Priority *int64 `gorm:"not null;default:0"`
	// AutoBan 1 = 测试失败自动禁用。
	AutoBan *int `gorm:"not null;default:1"`
	// OtherInfo 本层不解释的附加信息（status_reason / status_time 等，JSON）。
	OtherInfo string `gorm:"type:text;not null;default:''"`
	// Tag 渠道标签；索引：按标签批量启停/编辑（hyxapi 的 tag/* 接口）。
	Tag *string `gorm:"type:varchar(64);index:idx_channel_tag"`
	// Setting 渠道额外设置（代理、Vertex key 类型等，JSON）。
	Setting *string `gorm:"type:text"`
	// ParamOverride / HeaderOverride 请求参数与请求头覆盖（JSON）。
	ParamOverride  *string `gorm:"type:text"`
	HeaderOverride *string `gorm:"type:text"`
	// Remark 备注，最长 255（参考项目带 validate:max=255）。
	Remark *string `gorm:"type:varchar(255)"`
	// ChannelInfo 多 Key 状态机的 JSON 列（类型定义在本文件下方）。
	//
	// ⚠ 它**不是表**：不登记进 allModels()、不建表。db_test.go 的门禁因此按
	// "有 TableName() 才算表" 判定（见那里的注释）。
	ChannelInfo ChannelInfo `gorm:"type:json"`
	// OtherSettings 不需要检索的补充设置（Azure 版本等），列名照参考项目 settings。
	OtherSettings string `gorm:"column:settings;type:text;not null;default:''"`
	// Keys 多 Key 的**内存态**解析结果（永不落库）。
	Keys []string `gorm:"-"`
}

func (Channel) TableName() string { return "channels" }

// ChannelInfo 多 Key 状态机（channels.channel_info JSON 列）。
//
// # 为什么它在 models.go 而不在 channel.go
//
// 主脑冻结的 Channel 结构契约要求类型名 `ChannelInfo` 定义在本文件（A1/A3 共用）。
// 但它**不是一张表**，所以**不登记进 allModels()**；db_test.go 的门禁按
// 「有 TableName() 才算表」判定，正好把它与表（本文件 12 张 + token.go 的 tokens）分开。
//
// 字段与 hyxapi model.ChannelInfo 一一对应。
type ChannelInfo struct {
	// IsMultiKey true = 这条渠道的 Key 列里是多把 key。
	IsMultiKey bool `json:"is_multi_key"`
	// MultiKeySize Key 列里 key 的**把数**（解析后）。改 key 时必须重算。
	MultiKeySize int `json:"multi_key_size"`
	// MultiKeyStatusList key 下标 → 状态（1 启用 / 2 手动禁用 / 3 自动禁用）。
	MultiKeyStatusList map[int]int `json:"multi_key_status_list"`
	// MultiKeyDisabledReason key 下标 → 禁用原因（人看的，不参与判定）。
	MultiKeyDisabledReason map[int]string `json:"multi_key_disabled_reason,omitempty"`
	// MultiKeyDisabledTime key 下标 → 禁用时刻（Unix 秒）。
	MultiKeyDisabledTime map[int]int64 `json:"multi_key_disabled_time,omitempty"`
	// MultiKeyPollingIndex 轮询模式下的游标（MultiKeyMode == polling 时有意义）。
	MultiKeyPollingIndex int `json:"multi_key_polling_index"`
	// MultiKeyMode random / polling（见 channel.go 的常量）。空串按 random 处理。
	MultiKeyMode string `json:"multi_key_mode"`
}

// Value 实现 driver.Valuer：写库时序列化成 JSON 文本。
//
// ⚠ 返回 string 而不是 []byte：SQLite 的 text 列收字符串最省事，也避免某些驱动
// 把 []byte 当 blob 处理（那会让列里出现 BLOB 值，之后用 text 函数查它静默不匹配）。
func (c ChannelInfo) Value() (driver.Value, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化 channel_info 失败: %w", err)
	}
	return string(b), nil
}

// Scan 实现 sql.Scanner：读库时反序列化 JSON。
//
// 三种输入都收：NULL、[]byte、string —— 不同驱动/不同列类型给出的东西不同，
// 只认一种就会在某个驱动版本上升级即崩。
func (c *ChannelInfo) Scan(value any) error {
	if c == nil {
		return errors.New("store: ChannelInfo.Scan 收到 nil 接收者")
	}
	var raw []byte
	switch v := value.(type) {
	case nil:
		*c = ChannelInfo{}
		return nil
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("store: channel_info 的库内值类型不支持: %T", value)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		*c = ChannelInfo{}
		return nil
	}
	var info ChannelInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("store: 解析 channel_info 失败: %w", err)
	}
	*c = info
	return nil
}

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
	CreatedAt int64 `gorm:"not null;default:0;index:idx_logs_created_at"`
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
	Type string `gorm:"type:varchar(16);not null;default:'';index:idx_logs_type"`
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
	UID        string `gorm:"type:varchar(64);not null;default:'';index:idx_account_uid"`
	Label      string `gorm:"type:varchar(64);not null;default:''"`
	Enabled    bool   `gorm:"not null;default:true"`
	CreatedAt  int64  `gorm:"not null;default:0"`
	LastUsedAt int64  `gorm:"not null;default:0"`
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
