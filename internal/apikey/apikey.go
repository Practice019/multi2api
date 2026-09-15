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

// Create 新建一把 key。limit=0 不限额度；rpm=0 不限速。
func (s *Store) Create(name string, limit int64, rpm int) (*Key, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("名称不能为空")
	}
	if limit < 0 || rpm < 0 {
		return nil, errors.New("额度/限速不能为负数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := &Key{
		ID:        newID(),
		Name:      name,
		Enabled:   true,
		Limit:     limit,
		RPM:       rpm,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	s.keys[k.ID] = k
	if err := s.saveLocked(); err != nil {
		delete(s.keys, k.ID)
		return nil, err
	}
	return k, nil
}

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

// Update 修改一把 key 的额度上限与限速（0 = 不限）。不存在返回 ErrUnknown。
func (s *Store) Update(id string, limit int64, rpm int) error {
	if limit < 0 || rpm < 0 {
		return errors.New("额度/限速不能为负数")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return ErrUnknown
	}
	k.Limit = limit
	k.RPM = rpm
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
