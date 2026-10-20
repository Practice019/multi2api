// Package apikey 提供 API Key 管理（对标 new-api 的令牌管理，适配单机规模）。
//
// # 为什么需要它（用户对标 new-api 提的需求）
//
// 之前鉴权只有一个 config 里的 api_key（全部请求共用一把，没法给不同调用方
// 分开发放、限额、看各自用量）。new-api 的令牌体系提供：多把 key、每把可
// 启用/禁用、额度配额、速率限制、用量统计。本包实现它的单机简化版：
//
//	持久化：data/apikeys.json（JSON，原子写，与 config.json 同级安全考虑）
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
	"time"
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
	Models []string `json:"models,omitempty"`

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

	// rateHits 限速滑动窗口：id → 最近 60s 的请求时刻（进程内，不持久化）。
	rateHits map[string][]time.Time
}

// New 加载（或初始化）路径上的存储。目录不存在时自动创建。
// 加载失败返回错误（数据文件损坏应显式暴露，不静默重建丢数据）。
func New(path string) (*Store, error) {
	s := &Store{
		path:     path,
		keys:     make(map[string]*Key),
		rateHits: make(map[string][]time.Time),
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行：空表，落盘一个空文件以便文件位置可发现。
			if err := s.saveLocked(); err != nil {
				return nil, err
			}
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
		}
	}
	return s, nil
}

// saveLocked 原子写盘（先写临时文件再 rename）。调用方必须持锁。
func (s *Store) saveLocked() error {
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

// ModelMapOf 返回一把 key 的模型对外名映射（对外名 → 内部名），以及 key 是否存在。
//
// 空表 = 不做映射。调用方（出口层）用它在**判白名单之前**把对外名换成内部名 ——
// 见 server.resolveModelAlias。
//
// ⚠ 返回副本。调用方拿到内部 map 后可能会改它（映射换算是就地替换 model 变量，
// 很容易顺手写成 `for k, v := range` 之外还 `delete(m, k)`），那会污染 store 里的
// 权威数据 —— 而这份数据下一次保存就被写进盘了。
func (s *Store) ModelMapOf(id string) (map[string]string, bool) {
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
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// Toggle 启用/禁用一把 key。
func (s *Store) Toggle(id string, enabled bool) (bool, error) {
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
	if err := s.saveLocked(); err != nil {
		return k.Enabled, err
	}
	return k.Enabled, nil
}

// Delete 删除一把 key。成功返回 true；不存在返回 ErrUnknown。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[id]; !ok {
		return ErrUnknown
	}
	delete(s.keys, id)
	delete(s.rateHits, id)
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
	// modelMap 用变参表达三态：
	//   不传 / 传 nil  → 不改（老调用点）
	//   传空 map       → 清空映射
	//   传非空 map     → 整体替换
	if len(modelMap) > 0 && modelMap[0] != nil {
		k.ModelMap = normalizeModelMap(modelMap[0])
	}
	return s.saveLocked()
}

// Reset 重置已用额度与失败数（请求数保留）。
func (s *Store) Reset(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrUnknown
	}
	k.Used = 0
	k.Fails = 0
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
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[bearer]
	if !ok {
		return "", ErrUnknown
	}
	if !k.Enabled {
		return "", ErrDisabled
	}
	if k.Limit > 0 && k.Used >= k.Limit {
		return "", ErrQuota
	}
	if k.RPM > 0 && !s.rateOKLocked(k.ID, k.RPM) {
		return "", ErrRateLimit
	}
	return k.ID, nil
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
