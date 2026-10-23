// Package channelstore 存"渠道"与"能力索引"，形状对标 new-api。
//
// # 为什么是两张表（而不是一张渠道表带一个 models 数组）
//
// 蓝本：知识库 `viking://resources/wiki/new-api/concept/channel-ability-routing.md` ——
//
//	"An ability is the join record that makes a channel usable for a model inside a
//	 group. Its fields are Group, Model, ChannelId, Enabled, Priority, Weight, and an
//	 optional Tag, with a composite primary key of (group, model, channel_id)."
//	"...routing ... is produced from an *ability* index"，列表侧同样从它算
//	（GetGroupEnabledModels / GetEnabledModels）。
//
// ⇒ 选路与"对外模型列表"都**只读能力索引**，不遍历渠道数组。所以我们物化同一张表：
// 渠道表回答"这个渠道怎么连"（base_url/key/映射/覆盖），能力表回答
// "哪个组里的哪个模型，能被哪个渠道服务、优先级/权重多少"。
//
// ⚠ 本轮 group 固定单组 `default`（字段与主键都留着，逻辑不做）—— 这样将来要支持
// new-api 的 group / auto group 时，数据结构不需要重新设计（只补选路逻辑）。
//
// ⚠ 与账号型上游的关系：本包**不碰** auths/ 与 Pool（账号的选号/惩罚/轮换在那边）。
// Key 型渠道没有"号"可换，进 Pool 会把"一个 key 被限流"误判成"账号坏了"。
package channelstore

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 渠道状态。数值与 new-api 一致（Enable / ManuallyDisabled=2 / AutoDisabled=3）：
// 保留数值是为了将来与它的日志/前端语义对得上，别改成 0/1 两档。
const (
	StatusEnabled          = 1
	StatusManuallyDisabled = 2
	StatusAutoDisabled     = 3
)

// DefaultGroup 是本轮唯一的分组。new-api 里 group 是路由的第一维（默认 'default'）。
const DefaultGroup = "default"

// Channel 是一条渠道记录。字段命名与 new-api 的 model.Channel 一一对应，便于对照：
//
//	new-api                          channestore
//	Id / Name / Type                 ID / Name / Type
//	Key（多 key 时用别的字段）        Keys[]（multi-key 一等公民）
//	BaseURL / Models                 BaseURL / Models
//	ModelMapping                     ModelMapping
//	Group / Priority / Weight        Group / Priority / Weight
//	Status                           Status
//	（overrides）                     Overrides
//	（channel info）                  ChannelInfo
type Channel struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        int               `json:"type"`          // 渠道类型（OpenAI 兼容 = TypeOpenAICompat）
	BaseURL     string            `json:"base_url"`      // R3：手填
	Keys        []ChannelKey      `json:"keys"`          // R3：手填（支持多 key）
	Models      []string          `json:"models"`        // 该渠道**声明**能服务的模型（上游原名）
	ModelMap    map[string]string `json:"model_mapping"` // R2：公开名 → 上游名
	Group       string            `json:"group"`
	Priority    int               `json:"priority"` // 越大越先用；重试第 n 次走第 n 档
	Weight      int               `json:"weight"`   // 同一档内加权随机的权重
	Status      int               `json:"status"`
	Overrides   Overrides         `json:"overrides"`
	ChannelInfo ChannelInfo       `json:"channel_info"`
}

// ChannelKey 一个 key 及其状态（multi-key 渠道：某个 key 坏了不该停掉整条渠道）。
type ChannelKey struct {
	Key    string `json:"key"`
	Status int    `json:"status"`
	// Reason / DisabledAt 记"为什么、什么时候"被停用（对标 new-api 的 multi_key_info）。
	//
	// ⚠ 自动禁用必须留下原因：否则用户看到某个 key 自己变灰，只会以为程序坏了，
	// 而不是"这把 key 上游说无效了"。理由取值见 keyops.go 的 Reason* 常量。
	Reason     string `json:"reason,omitempty"`
	DisabledAt int64  `json:"disabled_at,omitempty"`
}

// Overrides 渠道级覆盖（对标 new-api 的参数覆盖 / 头部覆盖）。
type Overrides struct {
	Param  map[string]any    `json:"param,omitempty"`
	Header map[string]string `json:"header,omitempty"`
}

// ChannelInfo 渠道的元信息（对标 new-api 的 channel info）。
type ChannelInfo struct {
	MultiKey         bool   `json:"multi_key"`
	KeySelectionMode string `json:"key_selection_mode"` // "" / "random" / "round_robin"

	// ContextLength 本渠道模型的上下文窗口（0 = 未知）。
	//
	// # 为什么需要（用户报的第 1 条）
	//
	// 实测 /v1/models 里渠道模型（001/deepseek-v4.1-flash-1017）的
	// context_length 与 max_output_tokens **两个字段都没有**，
	// 而同表里其它五个模型都有值。
	//
	// 根因：渠道的模型是"公开名 → 上游名"的映射，本身不带任何元数据；
	// appendChannelModels 只写了 {id, object}。于是同一个列表里
	// 有的条目有 ctx、有的是空的 —— 客户端做上下文预算时无法处理。
	//
	// 0 = 未知 ⇒ 输出时**整个字段不写**（不是写 0）。
	ContextLength int64 `json:"context_length,omitempty"`

	// MaxOutputTokens 本渠道模型的单次最大输出（0 = 未知）。
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}

// Ability 是"某个组里的某个模型，可以被某个渠道服务"这条 join 记录。
// 主键 (Group, Model, ChannelID) —— 与 new-api 的复合主键一致。
type Ability struct {
	Group     string `json:"group"`
	Model     string `json:"model"`
	ChannelID string `json:"channel_id"`
	Enabled   bool   `json:"enabled"`
	Priority  int    `json:"priority"`
	Weight    int    `json:"weight"`
	Tag       string `json:"tag,omitempty"`
}

// abilityKey 能力索引的键。(group, model, channel_id) 三元组，用 \x00 连接避免歧义。
func abilityKey(group, model, channelID string) string {
	return group + "\x00" + model + "\x00" + channelID
}

// snapshot 是落盘形状（一个文件两段：channels + abilities，便于人工查看与备份）。
type snapshot struct {
	Version   int               `json:"version"`
	SavedAt   time.Time         `json:"saved_at"`
	Channels  []*Channel        `json:"channels"`
	Abilities []*Ability        `json:"abilities"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// Store 是渠道与能力索引的存储。并发安全；写盘走"临时文件 + rename"原子替换。
type Store struct {
	mu        sync.RWMutex
	path      string
	channels  map[string]*Channel
	abilities map[string]*Ability
	now       func() time.Time // 便于测试注入
	// rand 供选路（档内加权随机 / 多 key 随机挑选）使用；rr 是 round_robin 的游标。
	// 测试在同包内直接改 s.rand = newRand(1) 拿确定性分布，不额外开 API。
	rand *rand.Rand
	rr   map[string]int
}

// New 从 path 载入；文件不存在视为空库（不报错 —— 首次使用就是这个状态）。
func New(path string) (*Store, error) {
	s := &Store{
		path:      path,
		channels:  map[string]*Channel{},
		abilities: map[string]*Ability{},
		now:       time.Now,
		rand:      newRand(time.Now().UnixNano()),
		rr:        map[string]int{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("读取渠道存储 %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("解析渠道存储 %s: %w", path, err)
	}
	for _, ch := range snap.Channels {
		if ch == nil || ch.ID == "" {
			continue
		}
		normalizeChannel(ch)
		s.channels[ch.ID] = ch
	}
	// 能力表：以文件里记的为准；但**渠道侧变化优先** —— 落盘的能力若有孤儿（渠道没了），丢弃。
	for _, ab := range snap.Abilities {
		if ab == nil || ab.ChannelID == "" || ab.Model == "" {
			continue
		}
		if _, ok := s.channels[ab.ChannelID]; !ok {
			continue
		}
		s.abilities[abilityKey(ab.Group, ab.Model, ab.ChannelID)] = ab
	}
	// 自愈：把每个渠道的 models 重新展开一遍（对标 new-api 的"truncates, rebuilds, and re-initializes"）。
	for _, ch := range s.channels {
		s.rebuildAbilitiesLocked(ch)
	}
	return s, nil
}

// normalizeChannel 补齐缺省值（Group 空 ⇒ default；Status 0 ⇒ Enabled；Weight 0 保留 0 语义）。
func normalizeChannel(ch *Channel) {
	if ch.Group == "" {
		ch.Group = DefaultGroup
	}
	if ch.Status == 0 {
		ch.Status = StatusEnabled
	}
	if ch.ModelMap == nil {
		ch.ModelMap = map[string]string{}
	}
	ch.ChannelInfo.MultiKey = len(ch.Keys) > 1
}

// rebuildAbilitiesLocked 把一条渠道的 models 展开成能力记录（调用方持写锁）。
//
// 对标 new-api："writes expand a channel's model list across groups in chunks and ignore
// conflicts" —— 这里单组实现：先删该渠道的旧能力，再按当前 models 重建；
// Enabled 由渠道状态决定（只有 Enabled 的渠道才对外可用）。
func (s *Store) rebuildAbilitiesLocked(ch *Channel) {
	for k, ab := range s.abilities {
		if ab.ChannelID == ch.ID {
			delete(s.abilities, k)
		}
	}
	if ch.Status != StatusEnabled {
		return // 停用的渠道不产生能力（new-api：状态必须是 enabled 才可用）
	}
	for _, m := range ch.Models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		ab := &Ability{
			Group:     ch.Group,
			Model:     m,
			ChannelID: ch.ID,
			Enabled:   true,
			Priority:  ch.Priority,
			Weight:    ch.Weight,
			Tag:       ch.Name,
		}
		s.abilities[abilityKey(ab.Group, ab.Model, ab.ChannelID)] = ab
	}
}

// List 返回全部渠道（按 ID 排序，稳定输出便于前端与测试）。返回的是**深拷贝**，调用方随便改。
func (s *Store) List() []*Channel {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Channel, 0, len(s.channels))
	for _, ch := range s.channels {
		out = append(out, cloneChannel(ch))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get 取一条渠道的深拷贝；不存在返回 (nil,false)。
func (s *Store) Get(id string) (*Channel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ch, ok := s.channels[id]
	if !ok {
		return nil, false
	}
	return cloneChannel(ch), true
}

// Upsert 新增或覆盖一条渠道，并同步能力索引。ID 为空则报错（ID 由调用方/管理端生成）。
func (s *Store) Upsert(ch *Channel) error {
	if ch == nil || strings.TrimSpace(ch.ID) == "" {
		return fmt.Errorf("渠道 ID 不能为空")
	}
	cp := cloneChannel(ch)
	normalizeChannel(cp)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[cp.ID] = cp
	s.rebuildAbilitiesLocked(cp)
	return s.saveLocked()
}

// SetStatus 只改状态（对标 new-api 的"status can be updated in bulk by tag"的单条版本），
// 并同步能力索引（停用 ⇒ 该渠道的能力消失）。
func (s *Store) SetStatus(id string, status int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.channels[id]
	if !ok {
		return fmt.Errorf("渠道不存在: %s", id)
	}
	ch.Status = status
	s.rebuildAbilitiesLocked(ch)
	return s.saveLocked()
}

// Delete 删渠道并连带删掉它的能力记录。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.channels[id]; !ok {
		return fmt.Errorf("渠道不存在: %s", id)
	}
	delete(s.channels, id)
	for k, ab := range s.abilities {
		if ab.ChannelID == id {
			delete(s.abilities, k)
		}
	}
	return s.saveLocked()
}

// EnabledModels 返回某组里"至少有一个启用渠道能服务"的模型集合（升序）。
// 这是对外模型目录的数据源（对标 new-api 的 GetGroupEnabledModels）——
// **它不带 token 维度的裁剪**，那层在调用方（我们已有 apikey_models.go 的三态语义）。
func (s *Store) EnabledModels(group string) []string {
	if group == "" {
		group = DefaultGroup
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]bool{}
	for _, ab := range s.abilities {
		if ab.Group == group && ab.Enabled {
			seen[ab.Model] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Abilities 返回当前能力索引的快照（测试与调试用；只读）。
func (s *Store) Abilities() []*Ability {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Ability, 0, len(s.abilities))
	for _, ab := range s.abilities {
		cp := *ab
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].ChannelID < out[j].ChannelID
	})
	return out
}

// saveLocked 原子落盘（调用方持写锁）。权限 0600：里面有渠道的 API Key。
func (s *Store) saveLocked() error {
	snap := snapshot{Version: 1, SavedAt: s.now()}
	for _, ch := range s.channels {
		snap.Channels = append(snap.Channels, cloneChannel(ch))
	}
	sort.Slice(snap.Channels, func(i, j int) bool { return snap.Channels[i].ID < snap.Channels[j].ID })
	for _, ab := range s.abilities {
		cp := *ab
		snap.Abilities = append(snap.Abilities, &cp)
	}
	sort.Slice(snap.Abilities, func(i, j int) bool {
		if snap.Abilities[i].Model != snap.Abilities[j].Model {
			return snap.Abilities[i].Model < snap.Abilities[j].Model
		}
		return snap.Abilities[i].ChannelID < snap.Abilities[j].ChannelID
	})
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, out)
}

// writeFileAtomic 临时文件 + rename，避免写到一半被中断留下半个文件；权限 0600。
// （与 internal/admin 的同名实现同款；那边是包内私有，所以这里各留一份。）
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".channels-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这次删除是 no-op
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// cloneChannel 深拷贝一条渠道（map/slice 都要复制，否则调用方改到缓存）。
func cloneChannel(ch *Channel) *Channel {
	if ch == nil {
		return nil
	}
	cp := *ch
	cp.Keys = append([]ChannelKey(nil), ch.Keys...)
	cp.Models = append([]string(nil), ch.Models...)
	if ch.ModelMap != nil {
		cp.ModelMap = make(map[string]string, len(ch.ModelMap))
		for k, v := range ch.ModelMap {
			cp.ModelMap[k] = v
		}
	}
	if ch.Overrides.Param != nil {
		cp.Overrides.Param = make(map[string]any, len(ch.Overrides.Param))
		for k, v := range ch.Overrides.Param {
			cp.Overrides.Param[k] = v
		}
	}
	if ch.Overrides.Header != nil {
		cp.Overrides.Header = make(map[string]string, len(ch.Overrides.Header))
		for k, v := range ch.Overrides.Header {
			cp.Overrides.Header[k] = v
		}
	}
	return &cp
}
