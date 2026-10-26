// Package apikey 提供 API Key 管理（对标 new-api 的令牌管理，适配单机规模）。
//
// # 为什么需要它（用户对标 new-api 提的需求）
//
// 之前鉴权只有一个 config 里的 api_key（全部请求共用一把，没法给不同调用方
// 分开发放、限额、看各自用量）。new-api 的令牌体系提供：多把 key、每把可
// 启用/禁用、额度配额、速率限制、用量统计。本包实现它的单机简化版：
//
//	持久化：**SQL（tokens 表，见 sql.go）**。Wave B2 起库是真源：首次接管时把
//	         data/apikeys.json 一次性导入（幂等），此后 JSON **只读不写**；
//	         库不可用（store.DB == nil，如单测）时仍退回原来的 JSON 原子写。
//	额度：  每把 key 一个 token 上限（0 = 不限），请求成功按实际 token 扣减
//	限速：  每把 key 每分钟请求上限（0 = 不限），进程内滑动窗口
//	统计：  请求数 / 消耗 token / 失败数
//
// # 兼容性
//
// config.api_key 仍是「管理钥匙」（不限额度、不限速，与旧行为一致）；
// 本包的 key 面向普通调用方。未创建任何 key 时鉴权行为与改造前完全相同。
package apikey

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/store"
)

// 错误哨兵：Validate 的返回，供 HTTP 层映射状态码。
var (
	// ErrUnknown key 不存在或已被删除。
	ErrUnknown = errors.New("apikey: 未知的 API key")
	// ErrDisabled key 已被禁用。
	ErrDisabled = errors.New("apikey: API key 已禁用")
	// ErrQuota key 额度已用完。
	ErrQuota = errors.New("apikey: 额度已用完")
	// ErrRateLimit key 触发每分钟请求上限。
	ErrRateLimit = errors.New("apikey: 请求过于频繁（触发每分钟上限）")
	// ErrExpired key 已过期（SQL 后端的 expired_time 已过，或库里 status=3）。
	//
	// 与 ErrDisabled 分开是有用的：前端/运维看到"过期"会去续期，看到"禁用"会去查谁关的。
	ErrExpired = errors.New("apikey: API key 已过期")
)

// Key 一把 API key 的完整状态（持久化字段）。
type Key struct {
	ID        string `json:"id"`         // sk- 开头的随机标识，也是 Bearer 值
	Name      string `json:"name"`       // 显示名（调用方名字/用途）
	Enabled   bool   `json:"enabled"`    // 是否可用
	Limit     int64  `json:"limit"`      // 额度上限（token 数），0 = 不限
	Used      int64  `json:"used"`       // 已消耗 token
	Requests  int64  `json:"requests"`   // 总请求数
	Fails     int64  `json:"fails"`      // 失败请求数
	RPM       int    `json:"rpm"`        // 每分钟请求上限，0 = 不限
	CreatedAt string `json:"created_at"` // RFC3339

	// ExpiredTime 到期时刻（Unix 秒）；0 与 -1 都表示"永不"。
	//
	// 这是 SQL 化后新增的字段：apikey 原来没有过期概念，而 tokens 表有 expired_time
	// （hyxapi 对等），B2 的验收要求"过期"能作为一种拒绝理由。omitempty 让老 JSON
	// 文件写回时不多出这个字段。
	ExpiredTime int64 `json:"expired_time,omitempty"`

	// sqlStatus 是 SQL 后端的**原始 status**（1启用/2禁用/3过期/4耗尽）；0 = 无
	// SQL 状态（JSON 模式）⇒ 由 Enabled 推导。不导出：它只服务于本包内 Validate
	// 的精确报错，不该出现在任何对外形态里。
	sqlStatus int

	// OwnerID 归属账号 ID（`account.User.ID`，形如 u-xxxx）。**空 = 管理员所有**。
	//
	// # 为什么空值判成"管理员所有"，而不是"无主"
	//
	// 本仓既有的每一把 key 都是管理台建的（那时还没有登录体系，能建 key 的
	// 只有 loopback 上的管理员），落盘的 apikeys.json 里根本没有这个字段。
	// 若把空值当成"谁都不属于"，升级后这些 key 会从**每个人的**列表里消失 ——
	// 用户看到的是"我的 key 全没了"，而数据其实一直在盘上。空 = 管理员所有，
	// 与既成事实正好一致：管理员本来就该看得见它们。
	//
	// ⚠ 由此推出一条**不能反过来用**的判据：非管理员不能靠"我的归属也是空"
	// 去匹配这些 key —— 那是把管理员的 key 交给成员，见 ListFor / CanManage。
	//
	// omitempty 让老 key 写回后**不多出**这个字段（"只是重启了一次"不该产生
	// 整文件 diff），也让新文件里管理员建的 key 与老数据形态一致。
	OwnerID string `json:"owner_id,omitempty"`

	// Models 这把 key **允许调用的模型白名单**（空 = 不限制，全部可用）。
	//
	// # 语义（三态，与其它"空值"判据分开）
	//
	//	nil / 空切片 → 不限制（既有 key 与新建时不填，都是这个状态）
	//	非空         → 只允许列表里的模型；其余一律 403
	//
	// ⚠ 「空 = 不限制」而不是「空 = 一个都不许」是刻意的：那把**所有既有的
	// key** 以及"没想过要限制"的新 key 都变成不可用 —— 一次升级把所有调用方
	// 打死。要"一个都不许"的正确表达是禁用这把 key（Enabled=false）。
	//
	// # 匹配口径：按**裸模型名**比
	//
	// 客户端发来的名字可能是 `glm-5.2`（裸名，走默认上游）或
	// `workbuddy/glm-5.2`（带上游前缀）。两者是**同一个模型**，
	// 所以比对时都剥掉上游前缀再比（见 ModelAllowed）。
	// 于是用户填 `glm-5.2` 就同时放行了两种写法，不需要他填两遍。
	//
	// # ⚠ 已废弃（PLAN-groups.md Q3）：改由 Key.Groups 表达
	//
	// 白名单与分组问的是**同一个问题** ——「这把 key 能用哪些模型」。两套配置并存
	// 必然不一致（改了分组忘改白名单、或反过来），而排查时要查两处。所以收敛到分组：
	//
	//	· 读：仍然照旧生效（存量 key 只有 Models、没有 Groups，升级后行为必须不变）
	//	· 写：P3.5 的迁移会把存量 Models 转成一个分组并清空本字段；此后新写入不再产生
	//	· 优先级：Groups 非空时本字段**不参与判定**（见 server.allowedModelsForRequest）
	//
	// 在迁移完成前**不要**删这个字段：存量 apikeys.json 里它还在，删了等于
	// 一次升级把所有老 key 的限制静默解开。
	Models []string `json:"models,omitempty"`

	// Groups 这把 key 可用的**分组 ID 列表**（空 = 不限制，与 Models 的三态同构）。
	//
	// # 为什么存 ID 而不是分组名
	//
	// 名字是给人看的，ID 是给引用用的。存名字的话，改一次分组名就会让所有
	// 引用它的 key **静默断链**（key 还能用，但分组没了 → 变成"不限制"，
	// 于是权限被放大）。ID 在创建后不变（见 groupstore.Group.ID）。
	//
	// # 空值语义：空 = 不限制（**不是**一个都不许）
	//
	// 与 Models 同一条理由：存量 key 全都没有这个字段，把空读成"一个都不许"
	// 会一次升级把所有调用方打死。要"一个都不许"的正确表达是
	// 「指向一个模型清单为空的分组」或直接禁用这把 key。
	//
	// # 与 Models 的关系：Groups 非空时以 Groups 为准
	//
	// Models 已废弃（见上），两者并存只发生在 P3.5 迁移之前。
	// 判定入口只有一处：server.allowedModelsForRequest。
	Groups []string `json:"groups,omitempty"`

	// MigratedToGroups 这把 key 的 Models 白名单是否已经迁移到分组（P3.5，幂等标记）。
	//
	// # 为什么"Groups 非空就不迁"这个天然条件还不够
	//
	// 迁移完成后 Groups 非空、Models 为空，两个条件都拦得住重复跑。但有一次
	// **只做了一半**的窗口：分组建好了、写回 key 时落盘失败 —— 那时 Groups
	// 还是空的、Models 还在，下次启动就会再建一个同名分组（还会撞名）。
	// 这个标记让"已经动过这把 key"这件事本身可查，不必再从字段形状去猜。
	//
	// # 为什么**不**放进 View
	//
	// 它是迁移状态、不是管理界面要看的东西。View 是"给人看的形态"，
	// 往里塞内部记账字段会让前端多一个永远不该渲染的列。
	MigratedToGroups bool `json:"migrated_to_groups,omitempty"`

	// ModelMap 这把 key 的**模型对外名映射**：`对外名 → 内部模型名`（2026-10-21 加）。
	//
	// # 用户的需求（原话）
	//
	//	「这些模型，开放的模型，对于这个 API key 开放的模型，我觉得需要有一个
	//	 模型映射，就是对外的模型应该叫什么」
	//
	// 背景：`Models` 白名单里填的是**内部**模型名（如 `csg/deepseek-v4.1-flash-1017`），
	// 而那是我们的内部命名 —— 直接暴露给调用方既难记、也把渠道结构漏了出去。
	// 有了这张表，可以对外只讲 `deepseek-v4-pro`，请求进来再换回内部名。
	//
	// # 方向：对外名 → 内部名（与渠道层的 Channel.ModelMap 同构）
	//
	// 请求路径每一跳都要查它，方向必须是"进来的名字 → 内部的名字"。
	// 反过来存（内部名 → 对外名）就得在每次请求里遍历全表找 value，
	// 而这张表在 /v1/models 和每次对话请求上都会用到。
	//
	// # 与 Models 白名单的关系：先映射、再判白名单
	//
	//   · 客户端发 `deepseek-v4-pro` → 映射成 `csg/deepseek-...` → 判白名单 ✓
	//   · 若先判白名单，对外名不在 `Models` 里，会被 403 —— 而它明明是发布的那个名字。
	//
	// # 空值语义
	//
	//	nil / 空表 → 不做映射（老 key 与"不需要别名"的 key 都是这个状态）
	//	非空       → 表里有的名字按表换，没有的**原样保留**
	//
	// ⚠ 「没有的原样保留」是刻意的：映射是**增量**能力，不是白名单。
	//   若把未命中的名字判成非法，那加一张映射表就等于把所有老调用方式打死。
	ModelMap map[string]string `json:"model_map,omitempty"`
}

// View 列表/管理端点的对外形态。
// ID 是**完整 key**：管理操作（toggle/update/delete/reset）回传用，仅
// loopback admin 可见（与账号昵称同级敏感度）。Masked 是展示用掩码。
type View struct {
	ID        string `json:"id"`     // 完整 key（操作端点回传）
	Masked    string `json:"masked"` // 掩码展示
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Limit     int64  `json:"limit"`
	Used      int64  `json:"used"`
	Remain    int64  `json:"remain"` // -1 = 不限
	Requests  int64  `json:"requests"`
	Fails     int64  `json:"fails"`
	RPM       int    `json:"rpm"`
	CreatedAt string `json:"created_at"`
	// OwnerID 归属账号 ID；空 = 管理员所有（语义与 Key.OwnerID 同一处定义）。
	// 前端据此显示"这把 key 是谁建的"，也是 ListFor 的过滤判据。
	OwnerID string `json:"owner_id,omitempty"`
	// Models 模型白名单（空 = 不限制）。管理界面据此展示"这把 key 能用什么"。
	Models []string `json:"models,omitempty"`
	// Groups 这把 key 可用的分组 ID 列表（空 = 不限制）。管理界面据此回填分组多选器。
	//
	// ⚠ 与 Models 一样：**List 里漏掉这一行，前端就永远回填不出来**。
	//   本文件已经踩过一次（ModelMap 曾漏在这里，用户报"设置了但编辑时是空的"）。
	//   新加字段时必须同时改 Key / View / List / saveLocked 四处。
	Groups []string `json:"groups,omitempty"`
	// ModelMap 模型对外名映射（对外名 → 内部名，空 = 不做映射）。
	// 管理界面据此展示"这个内部模型对外叫什么"。
	ModelMap map[string]string `json:"model_map,omitempty"`
}

// Store 是 API key 的进程内权威 + JSON 持久化。
//
// 并发：一把全局锁（管理端点与请求路径低频冲突，锁粒度无所谓）。
type Store struct {
	path string

	mu   sync.Mutex
	keys map[string]*Key // key(id) → 状态

	// ↓ SQL 模式（Wave B2）新增，详见 sql.go 文件头
	byBearer   map[string]string   // Bearer 候选 → key ID（Validate 热路径 O(1) 查表）
	rowIDs     map[string]int      // key ID → tokens.id（计量走单条原子 UPDATE 要用）
	dirty      map[string]struct{} // 待写回库的 key
	pendingDel []string            // 待软删的 key
	sqlMode    atomic.Bool         // 已接管为 SQL 模式（热路径一次原子读）
	attachMu   sync.Mutex          // 接管互斥：懒接管与显式装配共用

	// rateHits 限速滑动窗口：id → 最近 60s 的请求时刻（进程内，不持久化）。
	rateHits map[string][]time.Time

	// groupModels 按分组 ID 取模型清单（由装配层注入，见 groups_resolve.go）。
	//
	// # 为什么是函数值而不是 *groupstore.Store
	//
	// 本包要守住"零内部依赖"（见下面 ModelAllowed 那段注释）：认识 groupstore
	// 就等于把它拖进核心的依赖图，而依赖方向一旦长出来就再也收不回去。
	// 一个函数值把"分组里有哪些模型"这件事变成**外部事实**，本包只管拿它与
	// key.Groups 求并集。
	//
	// nil = 未接线（测试路径 / 分组功能未启用）⇒ 见 GroupModelsOf 的降级行为。
	groupModels GroupModelsFunc
}

// New 加载（或初始化）路径上的存储。目录不存在时自动创建。
// 加载失败返回错误（数据文件损坏应显式暴露，不静默重建丢数据）。
func New(path string) (*Store, error) {
	s := &Store{
		path:     path,
		keys:     make(map[string]*Key),
		rateHits: make(map[string][]time.Time),
		byBearer: make(map[string]string),
		rowIDs:   make(map[string]int),
		dirty:    make(map[string]struct{}),
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行：JSON 模式下落盘一个空文件以便文件位置可发现。
			//
			// ⚠ SQL 可用时不落这个文件：真源已经是 tokens 表，还去 data/ 下造一个
			// 永远不会更新的 apikeys.json，只会让人以为"数据还在文件里"。
			if store.DB == nil {
				if err := s.saveLocked(); err != nil {
					return nil, err
				}
			}
			s.maybeAttachSQL()
			return s, nil
		}
		return nil, fmt.Errorf("读取 apikeys: %w", err)
	}
	var arr []*Key
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("解析 apikeys: %w", err)
	}
	for _, k := range arr {
		if k != nil && k.ID != "" {
			s.keys[k.ID] = k
			s.indexBearerLocked(k)
		}
	}
	s.maybeAttachSQL()
	return s, nil
}

// saveLocked 原子写盘（先写临时文件再 rename）。调用方必须持锁。
func (s *Store) saveLocked() error {
	// SQL 模式：真源在库，只把脏行写回去（JSON 那份**只读不写**）。
	if s.sqlMode.Load() {
		return s.flushSQLLocked()
	}
	arr := make([]*Key, 0, len(s.keys))
	for _, k := range s.keys {
		arr = append(arr, k)
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].CreatedAt < arr[j].CreatedAt })
	raw, err := json.MarshalIndent(arr, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Create 新建一把 key（**归管理员所有**）。limit=0 不限额度；rpm=0 不限速；
// models 为空则不限制模型。
//
// 签名与行为保持不变（既有调用方 internal/admin/apikeys.go 与既有测试都据此
// 断言），实现委托给 CreateOwned("", ...) —— owner 为空 = 管理员所有，
// 与老数据同一语义。新增归属能力靠新增方法，不改这个签名。
func (s *Store) Create(name string, limit int64, rpm int, models []string, modelMap ...map[string]string) (*Key, error) {
	return s.CreateOwned("", name, limit, rpm, models, modelMap...)
}

// normalizeModels 规整白名单：去空白、丢空项、去重（保序）。
//
// 返回 nil（而不是空切片）表示"不限制"。这一步是**必要的**而不是洁癖：
// 用户在输入框里多打一个回车、或从邮件里复制带空格的模型名，都会产出
// `["glm-5.2", ""]` —— 若原样存下，`len(models) > 0` 会让这把 key
// 变成"受限制"，而白名单里那个空串永远匹配不上任何模型。
// 后果是"我明明留空了却全都不许用"，极难往"多打了一个回车"上想。
func normalizeModels(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// firstModelMap 从变参里取第一份映射表并规整（nil → nil = 不映射）。
//
// 变参只是为了让老调用点（Create / CreateOwned 的既有用法）不必改动，
// 语义上它最多一个元素。
func firstModelMap(m ...map[string]string) map[string]string {
	if len(m) == 0 || m[0] == nil {
		return nil
	}
	return normalizeModelMap(m[0])
}

// cloneStringMap 复制一份 map（nil → nil）。
//
// ⚠ View 是**对外形态**，必须与 store 内部数据彻底隔离 —— 调用方拿到
// 内部 map 后随手改一个键，就污染了权威数据，而它下一次 saveLocked 就落盘了。
// Models 那里用 `append([]string(nil), ...)`，这里用同样的语义。
func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// normalizeModelMap 规整映射表：去空白、丢掉"键或值为空"的项、键值相同则省略。
//
// ⚠ 键值相同要**删掉**而不是保留：`{"deepseek-v4-pro": "deepseek-v4-pro"}`
// 与"不映射"完全等价，留着只会让 /v1/models 的输出去重逻辑多绕一圈，
// 而且用户在界面上看到自己填了个毫无作用的别名，会以为没生效。
//
// 返回 nil（而不是空 map）表示"不映射"，与 Models 的 nil 语义保持一致：
// 两者都为 nil 时 JSON 里根本不出现这个字段，老文件写回不产生 diff。
func normalizeModelMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for alias, target := range in {
		alias = strings.TrimSpace(alias)
		target = strings.TrimSpace(target)
		if alias == "" || target == "" || alias == target {
			continue
		}
		out[alias] = target
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ModelsOf 返回一把 key 的模型白名单，以及这把 key 是否存在。
//
// 空切片 = 不限制。调用方（出口层）用它做准入判断 —— 见 ModelAllowed。
func (s *Store) ModelsOf(id string) ([]string, bool) {
	s.maybeAttachSQL()
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, false
	}
	// 返回副本：调用方不该拿到内部切片（它能 append 到我们的底层数组上）。
	if len(k.Models) == 0 {
		return nil, true
	}
	out := make([]string, len(k.Models))
	copy(out, k.Models)
	return out, true
}

// GroupsOf 返回一把 key 的分组 ID 列表，以及这把 key 是否存在。
//
// 空切片 = 不限制（与 ModelsOf 的三态完全一致）。
//
// ⚠ 第二个返回值不能省：调用方必须能区分「这把 key 没有分组限制」与
// 「根本没有这把 key」。只看切片长度会把两者混成一个 —— 而前者应当放行、
// 后者应当 401，混起来的后果取决于谁先判断，属于"偶尔放行一把已删除的 key"
// 这种最难复现的故障。
//
// ⚠ 返回副本：调用方 append 一下就写到我们的底层数组上了。
func (s *Store) GroupsOf(id string) ([]string, bool) {
	s.maybeAttachSQL()
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, false
	}
	if len(k.Groups) == 0 {
		return nil, true
	}
	out := make([]string, len(k.Groups))
	copy(out, k.Groups)
	return out, true
}

// normalizeGroups 规整分组 ID 列表：去空白、丢空项、去重（**保序**）。
//
// 与 normalizeModels 的唯一区别是**不排序**：分组的顺序对用户有意义
// （他在界面上按自己的顺序勾的），而且这个顺序将来可能用来表达「优先用哪个
// 分组」。排序会把这个信息抹掉，却没有任何收益 —— 判定用的是并集。
//
// 丢掉空串是必要的：`["g-1", ""]` 若原样存下，len>0 会让这把 key 进入
// 「受限」分支，而那个空串取不到任何分组 ⇒ 并集为空 ⇒ 一个模型都不许用，
// 用户却以为自己配了 g-1。
//
// 返回 nil 表示"不限制"（与 normalizeModels 同一约定：nil 而非空切片，
// 让 omitempty 在落盘时不产生这个字段，老文件写回不产生 diff）。
func normalizeGroups(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	return out
}

// ModelMapOf 返回一把 key 的模型对外名映射（对外名 → 内部名），以及 key 是否存在。
//
// 空表 = 不做映射。调用方（出口层）用它在**判白名单之前**把对外名换成内部名 ——
// 见 server.resolveModelAlias。
//
// ⚠ 返回副本。调用方拿到内部 map 后可能会改它（映射换算是就地替换 model 变量，
// 很容易顺手写成 `for k, v := range` 之外还 `delete(m, k)`），那会污染 store 里的
// 权威数据 —— 而这份数据下一次保存就被写进盘了。
func (s *Store) ModelMapOf(id string) (map[string]string, bool) {
	s.maybeAttachSQL()
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, false
	}
	if len(k.ModelMap) == 0 {
		return nil, true
	}
	out := make(map[string]string, len(k.ModelMap))
	for alias, target := range k.ModelMap {
		out[alias] = target
	}
	return out, true
}

// ⚠ 本包**没有** ModelAllowed —— 那是出口层的事，理由见下。
//
// # 为什么"模型名怎么比"不属于本包（架构守卫逼出来的结论）
//
// 我第一版把 ModelAllowed 写在这里，内部调 `gateway.SplitModel` 复用选路的
// 边界。结果**架构守卫当场抓住**：
//
//	import 了 gateway 的包会被 discoverUpstreams 当成**上游实现**，
//	于是核心（admin / server）依赖本包 = 「核心依赖上游」= 判据 1 违规
//
// 我第二版想退一步：在本包写一份"最保守的近似"（按最后一个 '/' 切）。
// 那更糟 —— 它会在"模型名本身含 '/'"时与选路**分叉**，而分叉的表现是
// "白名单认的模型"与"请求会路由到的模型"不是同一个东西，
// 于是偶尔 403 一个看起来完全合法的请求。同一个判据两份实现，必然出事。
//
// 所以：**本包只负责存**（Models/ModelsOf/normalize），
// **怎么比**只有一份，在 internal/server/apikey_models.go（那一层认识 gateway）。

// List 返回全部 key 的掩码视图（按创建时间排序）。
func (s *Store) List() []View {
	s.maybeAttachSQL()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]View, 0, len(s.keys))
	for _, k := range s.keys {
		remain := int64(-1)
		if k.Limit > 0 {
			remain = k.Limit - k.Used
			if remain < 0 {
				remain = 0
			}
		}
		out = append(out, View{
			ID:        k.ID,
			Masked:    MaskID(k.ID),
			Name:      k.Name,
			Enabled:   k.Enabled,
			Limit:     k.Limit,
			Used:      k.Used,
			Remain:    remain,
			Requests:  k.Requests,
			Fails:     k.Fails,
			RPM:       k.RPM,
			CreatedAt: k.CreatedAt,
			// OwnerID 必须带出来：ListFor 的成员过滤就是按它比的，漏掉这一行
			// 会让每个成员都看到空表（"我的 key 不见了"），而接口仍回 200。
			OwnerID: k.OwnerID,
			Models:  append([]string(nil), k.Models...),
			Groups:  append([]string(nil), k.Groups...),
			// ⚠ 2026-10-21 修：原来这里**没有** ModelMap —— List 构造 View 时漏了它，
			//   View.ModelMap 恒为 nil，于是前端拿到 `k.model_map === undefined`，
			//   编辑弹窗回填成 0 行。
			//
			//   用户报的现象正是：「设置了之后在编辑打开没有了 不会持久化？？」
			//   —— 数据其实好好地存在盘上（apikeys.json 里 model_map 就在），
			//   只是 List 这一个字段忘了带出来。这是"存了但读不出来"，
			//   最容易被误判成"没保存"。
			//
			//   同一个坑本文件已经踩过一次：OwnerID 那里就写着
			//   「漏掉这一行会让每个成员都看到空表」。字段多了就会漏 ——
			//   所以新加字段时**必须同时改 Key、View、List、saveLocked 四处**。
			ModelMap: cloneStringMap(k.ModelMap),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// Toggle 启用/禁用一把 key。
func (s *Store) Toggle(id string, enabled bool) (bool, error) {
	s.maybeAttachSQL()
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return false, ErrUnknown
	}
	if k.Enabled == enabled {
		return k.Enabled, nil
	}
	k.Enabled = enabled
	s.markDirtyLocked(id)
	if err := s.saveLocked(); err != nil {
		return k.Enabled, err
	}
	return k.Enabled, nil
}

// Delete 删除一把 key。成功返回 true；不存在返回 ErrUnknown。
func (s *Store) Delete(id string) error {
	s.maybeAttachSQL()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[id]; !ok {
		return ErrUnknown
	}
	delete(s.keys, id)
	delete(s.rateHits, id)
	s.unindexBearerLocked(id)
	// SQL 模式：记下待删（soft delete —— select 行还在，只是所有读路径看不见）。
	s.pendingDel = append(s.pendingDel, id)
	return s.saveLocked()
}

// Update 修改一把 key 的额度上限、限速与模型白名单。不存在返回 ErrUnknown。
//
// # 每个字段都是三态（`nil` = **不改**）
//
//	limit  == nil       → 不改额度；指向 0 → 不限；指向 N → 上限 N
//	rpm    == nil       → 不改限速；指向 0 → 不限；指向 N → N 次/分
//	models == nil       → **不改**白名单（字段缺失 / 显式 null）
//	models == []string{}→ **清空**白名单 = 恢复不限制
//	models 非空         → 设为这份白名单
//
// ⚠ 为什么数字也要指针：前端有**两类提交** ——
//
//	「改配额」只带 limit/rpm     「改模型」只带 models
//
// 若把"缺失"当成"设为 0"，第一次改模型就会把额度上限**清零**；
// 反过来若把 models 的缺失当成"清空"，改一次配额就把模型限制**静默解开**。
// 两条都是"用户没动过的字段被悄悄改掉"，而界面上看不出任何异常。
// 与 settings 里"漏带字段 = 重置"同型（本仓在 disabledModels 上踩过）。
func (s *Store) Update(id string, limit *int64, rpm *int, models []string, modelMap ...map[string]string) error {
	s.maybeAttachSQL()
	return s.UpdateWithGroups(id, limit, rpm, models, nil, modelMap...)
}

// UpdateWithGroups 与 Update 完全相同，**多一个** groups（P2，分组机制）。
//
// # 为什么是另一个方法，而不是给 Update 再加一个参数
//
// Go 只允许一个变参且必须在最后，而 modelMap 已经占着那个位置。要把 groups
// 塞进去只有两条路，都更差：
//
//	① 把 modelMap 改成 ...any —— 调用点写的还是 map，但类型安全性没了，
//	   而且下一个可选字段又要往 any 里塞（很快就会出现 any 里到底是什么）
//	② 改签名要求调用方传 groups —— 那会动 internal/admin/apikeys.go 与
//	   三个既有测试文件，而它们不在本次改动的范围内（接线由主脑做）
//
// 所以：**老方法原样保留、语义不变**，新方法一次把全部字段写完（单次落盘，
// 不会出现"额度改了、分组没改"的半成功状态）。
//
// # groups 的三态（与 models 同一个约定）
//
//	nil      → 不改（字段缺失 / 显式 null）
//	[]string{} → **清空** = 恢复"不限制"
//	非空     → 设为这些分组 ID
//
// ⚠ 清空 groups 等于**放大权限**（从"只这几个分组"变成"不限制"），
// 与"清空白名单"同一个风险等级。前端必须让它是显式动作，不能因为
// 表单没带这个字段就落到"清空"—— 那正是用 nil 表达"不改"的原因。
func (s *Store) UpdateWithGroups(id string, limit *int64, rpm *int, models, groups []string, modelMap ...map[string]string) error {
	if limit != nil && *limit < 0 {
		return errors.New("额度不能为负数")
	}
	if rpm != nil && *rpm < 0 {
		return errors.New("限速不能为负数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrUnknown
	}
	if limit != nil {
		k.Limit = *limit
	}
	if rpm != nil {
		k.RPM = *rpm
	}
	if models != nil {
		k.Models = normalizeModels(models)
	}
	if groups != nil {
		k.Groups = normalizeGroups(groups)
	}
	// modelMap 用变参表达三态：
	//   不传 / 传 nil  → 不改（老调用点）
	//   传空 map       → 清空映射
	//   传非空 map     → 整体替换
	if len(modelMap) > 0 && modelMap[0] != nil {
		k.ModelMap = normalizeModelMap(modelMap[0])
	}
	s.markDirtyLocked(id)
	return s.saveLocked()
}

// Reset 重置已用额度与失败数（请求数保留）。
func (s *Store) Reset(id string) error {
	s.maybeAttachSQL()
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrUnknown
	}
	k.Used = 0
	k.Fails = 0
	s.markDirtyLocked(id)
	return s.saveLocked()
}

// Validate 校验 Bearer 值对应的 key 当前是否可用。
//
// 返回该 key 的 id（记用量用）与限速判定。错误映射：
//
//	ErrUnknown / ErrDisabled → 401
//	ErrQuota                → 402
//	ErrRateLimit            → 429
func (s *Store) Validate(bearer string) (string, error) {
	s.maybeAttachSQL()
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bearer 先归一化成候选（原样 / 剥 sk- / 丢第一个 - 之后的部分），
	// 于是 sk- 形态与 48 位裸串两种写法都能命中同一把 key（见 sql.go 文件头）。
	k, id := s.lookupBearerLocked(bearer)
	if k == nil {
		return "", ErrUnknown
	}
	// 判定顺序：库侧原始状态 → 手动禁用 → 过期 → 额度 → 限速。
	//
	// ① 库侧原始状态（SQL 模式才有；JSON 模式恒为 0）：status=3/4 是库侧
	//    "已被判过期/耗尽"的落库结果，比按字段重算更权威。
	switch k.sqlStatus {
	case store.TokenStatusExpired:
		return "", ErrExpired
	case store.TokenStatusExhausted:
		return "", ErrQuota
	}
	// ② 手动禁用：Enabled=false 只表示"被关掉了"，与过期/耗尽区分开报错。
	if !k.Enabled {
		return "", ErrDisabled
	}
	// ③ 过期：0 与 -1 都表示"永不"（老 JSON 文件里没有这个字段 ⇒ 0）。
	if k.ExpiredTime > 0 && k.ExpiredTime < time.Now().Unix() {
		return "", ErrExpired
	}
	// ④ 额度：Limit>0 才判（0 = 不限）。
	if k.Limit > 0 && k.Used >= k.Limit {
		return "", ErrQuota
	}
	// ⑤ 限速：滑动窗口（rateHits 仍是进程内状态，不进库 —— 重启即清空是限速该有的行为）。
	if k.RPM > 0 && !s.rateOKLocked(id, k.RPM) {
		return "", ErrRateLimit
	}
	return id, nil
}

// rateOKLocked 记录一次命中并判断是否超速（滑动窗口 60s）。调用方必须持锁。
func (s *Store) rateOKLocked(id string, rpm int) bool {
	now := time.Now()
	hits := s.rateHits[id]
	cutoff := now.Add(-time.Minute)
	kept := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rpm {
		s.rateHits[id] = kept
		return false
	}
	s.rateHits[id] = append(kept, now)
	return true
}

// BumpUsage 请求结束后记录用量：成功加 token/requests，失败加 fails/requests。
// 幂等由调用方保证（一次请求只调一次）。持久化失败只记日志不返回错误——
// 用量统计是尽力而为，不能因为写盘失败让请求失败。
func (s *Store) BumpUsage(id string, tokens int64, ok bool) {
	s.maybeAttachSQL()
	s.mu.Lock()
	k, found := s.keys[id]
	if !found {
		s.mu.Unlock()
		return
	}
	k.Requests++
	if ok {
		if tokens > 0 {
			k.Used += tokens
		}
	} else {
		k.Fails++
	}
	if s.sqlMode.Load() {
		// SQL 模式：热路径走单条原子 UPDATE（requests+1 / used+tokens / 非无限额度扣 remain），
		// 不是"只加内存" —— 重启后从库里读回来仍在（B2 验收④）。
		s.persistUsageLocked(id, tokens, ok)
		s.mu.Unlock()
		return
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		log.Printf("apikey: 用量落盘失败: %v", err)
	}
}

// MaskID 掩码：sk-a1b2c3d4…e5f6（保留首 8 与尾 4）。
func MaskID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:8] + "…" + id[len(id)-4:]
}

// newID 生成 sk-<16 hex>（crypto/rand）。
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// 极不可能；退回时间戳（仍唯一性足够）
		b = []byte(fmt.Sprintf("%016x", time.Now().UnixNano()))
	}
	return "sk-" + hex.EncodeToString(b)
}

// NewAdminKey 生成新的管理钥匙（mk- 前缀 + 32 hex，crypto/rand）。
// 供「轮换管理密钥」端点使用：写回 config.api_key 并立即生效。
func NewAdminKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		b = []byte(fmt.Sprintf("%016x%016x", time.Now().UnixNano(), time.Now().UnixNano()))
	}
	return "mk-" + hex.EncodeToString(b)
}
