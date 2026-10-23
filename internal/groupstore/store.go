// Package groupstore 存「分组」—— 一组内部模型名的**具名集合**，独立于渠道与 API key。
//
// # 为什么分组要单独存一张表（而不是像 new-api 那样从 ability 派生）
//
// PLAN-groups.md Q1 已定：分组是**独立实体**。new-api 里分组只是 ability 上的
// 一个字符串值，存在性靠「哪些渠道声明过它」反推 —— 那条路有两个死结：
//
//	· 「先建一个空分组、稍后再往里放模型」做不到（没有 ability 就没有分组）
//	· 改名要遍历所有渠道逐条改，漏一条就出现「两个名字指同一批模型」
//
// 所以这里走 G2（实体）。落盘形状与 channelstore 同款：一个 snapshot 一个文件。
//
// # 边界：本包只回答一件事
//
//	「这个分组里有哪些模型名」。
//
// 「哪条渠道能服务某个模型」在 channelstore 的能力索引里；「哪把 key 能用哪些
// 分组」在 apikey 里。⇒ 所以 Group 里**不出现**任何渠道引用（PLAN 3.1.3）：
// 分组管模型集合、选路管渠道，两层职责糊在一起后，同一个模型想换渠道还得改分组。
//
// # 与 channelstore 对齐的三条实现约定
//
//  1. 并发：一把 RWMutex 护住整张表。分组是低频配置（管理台点一下才写一次），
//     拆细粒度锁只会增加出错面而换不来任何东西。
//  2. 落盘：临时文件 + rename 原子替换，0600。半截文件比没有文件更难排查 ——
//     它会让下次启动报一个「解析失败」，却没人知道是哪次写坏掉的。
//  3. 读盘失败 / JSON 损坏**显式报错**，绝不静默重建：静默重建等于把用户手工配的
//     分组全部清空，用户看到的是「分组莫名其妙变空了」，且无从恢复。
package groupstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 错误哨兵：让 HTTP 层能把「重名」映射成 409、「不存在」映射成 404，
// 而不是把所有失败都回 400 让调用方猜（errors.Is 判定，包装后仍然成立）。
var (
	// ErrNotFound 分组不存在。
	ErrNotFound = errors.New("groupstore: 分组不存在")
	// ErrNameExists 分组名已被别的分组占用（分组名唯一）。
	ErrNameExists = errors.New("groupstore: 分组名已存在")
	// ErrNameEmpty 分组名为空或只有空白。
	ErrNameEmpty = errors.New("groupstore: 分组名不能为空")
)

// Group 一个分组。字段与 PLAN-groups.md 3.2 一致。
//
// Models 存的是**内部模型名**（如 csg/deepseek-v4.1-flash-1017），不是渠道 ID、
// 也不是对外别名 —— 分组回答的是「允许哪些模型」，改名是 key 侧 ModelMap 的事。
type Group struct {
	ID          string   `json:"id"`          // g-xxxx（随机生成，创建后不变）
	Name        string   `json:"name"`        // 展示名，全局唯一
	Description string   `json:"description"` // 备注
	Models      []string `json:"models"`      // 该分组包含的内部模型名
	CreatedAt   string   `json:"created_at"`  // RFC3339
}

// snapshot 是落盘形状（一个文件一段，便于人工查看、备份与 diff）。
type snapshot struct {
	Version int       `json:"version"`
	SavedAt time.Time `json:"saved_at"`
	Groups  []*Group  `json:"groups"`
}

// Store 分组存储。并发安全；写盘走「临时文件 + rename」原子替换。
type Store struct {
	mu     sync.RWMutex
	path   string
	groups map[string]*Group // id → 分组
	now    func() time.Time  // 便于测试注入（CreatedAt 与 SavedAt 都走它）
}

// New 从 path 载入；文件不存在视为空库（不报错 —— 首次使用就是这个状态）。
//
// 文件存在但内容坏了 ⇒ 返回错误，**不**新建空库。这条是刻意的：静默重建会
// 把用户配好的分组全部抹掉，而错误信息至少能让人知道「去修 groups.json」。
func New(path string) (*Store, error) {
	s := &Store{
		path:   path,
		groups: map[string]*Group{},
		now:    time.Now,
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("读取分组存储 %s: %w", path, err)
	}
	// 空文件（0 字节或被编辑器清空）按空库处理：它不是「损坏」，而是「还没写过」。
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("解析分组存储 %s（文件已损坏，未做任何重建）: %w", path, err)
	}
	names := make(map[string]string, len(snap.Groups))
	for i, g := range snap.Groups {
		// 手改文件时最容易漏的就是 id / name。这里**报错**而不是跳过：
		// 跳过等于把那条分组静默丢掉，而用户以为它还在。
		if g == nil || strings.TrimSpace(g.ID) == "" {
			return nil, fmt.Errorf("解析分组存储 %s: 第 %d 条分组缺少 id", path, i+1)
		}
		g.Name = strings.TrimSpace(g.Name)
		if g.Name == "" {
			return nil, fmt.Errorf("解析分组存储 %s: 分组 %s 缺少 name", path, g.ID)
		}
		if other, dup := names[g.Name]; dup {
			return nil, fmt.Errorf("解析分组存储 %s: 分组名重复 %q（%s 与 %s）", path, g.Name, other, g.ID)
		}
		names[g.Name] = g.ID
		if _, dup := s.groups[g.ID]; dup {
			return nil, fmt.Errorf("解析分组存储 %s: 分组 id 重复 %q", path, g.ID)
		}
		// 载入时也归一化：老文件（或手改文件）里的模型名可能有空格、重复、乱序，
		// 不归一化的话「这个分组有几个模型」在内存与落盘之间会不一致。
		g.Models = normalizeModels(g.Models)
		s.groups[g.ID] = g
	}
	return s, nil
}

// List 返回全部分组（按创建时间升序；同一秒内按名字定序，保证输出稳定）。
// 返回的是**深拷贝**，调用方随便改。
func (s *Store) List() []*Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Group, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, cloneGroup(g))
	}
	sortGroups(out)
	return out
}

// Get 取一个分组的深拷贝；不存在返回 (nil,false)。
func (s *Store) Get(id string) (*Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok {
		return nil, false
	}
	return cloneGroup(g), true
}

// GetByName 按名字取（名字唯一，所以最多一条）。
//
// 为什么需要它：key 里存的是分组 ID，但**人**手里的配置、迁移脚本与日志
// 用的都是名字；没有按名字查就只能 List 出来自己扫一遍。
func (s *Store) GetByName(name string) (*Group, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, g := range s.groups {
		if g.Name == name {
			return cloneGroup(g), true
		}
	}
	return nil, false
}

// Create 新建分组。名字必填且唯一；模型名会被归一化（去空白/去空/去重/排序）。
//
// 失败时**不留半改状态**：落盘失败会把刚放进内存的那条撤掉，否则内存里
// 有一个「盘上没有」的分组，重启后凭空消失。
func (s *Store) Create(name, desc string, models []string) (*Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrNameEmpty
	}
	g := &Group{
		Name:        name,
		Description: strings.TrimSpace(desc),
		Models:      normalizeModels(models),
		CreatedAt:   s.now().UTC().Format(time.RFC3339),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nameTakenLocked(g.Name, "") {
		return nil, fmt.Errorf("%w: %q", ErrNameExists, g.Name)
	}
	// ID 在锁内才生成：4 字节随机（1/2^32）虽罕见，但撞了就会**静默覆盖**一个已有
	// 分组 —— 而那个 ID 可能已经被某把 key 引用。重试比"报告一个百万分之一的事故"便宜。
	g.ID = s.newIDLocked()
	s.groups[g.ID] = g
	if err := s.saveLocked(); err != nil {
		delete(s.groups, g.ID)
		return nil, err
	}
	return cloneGroup(g), nil
}

// Update 改一个分组。三个可选参数的空值语义**不一样**，这是刻意的：
//
//	name == nil          → 不改名字；指向空串 ⇒ ErrNameEmpty（分组总得有个名字）
//	desc == nil          → 不改描述
//	models == nil        → 不改模型清单
//	models == 空切片      → **清空**模型清单（JSON 里显式写 "models": [] 就是这个）
//
// 「nil = 不改、空切片 = 清空」是 Go/JSON 里唯一能同时表达这两件事的组合。
// 若把空切片也当成「不改」，用户就永远没法把分组清空；反过来把 nil 当清空，
// 一次只想改名字的表单提交就会顺手抹掉模型。
func (s *Store) Update(id string, name, desc *string, models []string) (*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.groups[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	// 先在副本上算完再替换：这样任何一步校验失败都还没碰到 map。
	next := cloneGroup(old)
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" {
			return nil, ErrNameEmpty
		}
		if n != old.Name && s.nameTakenLocked(n, id) {
			return nil, fmt.Errorf("%w: %q", ErrNameExists, n)
		}
		next.Name = n
	}
	if desc != nil {
		next.Description = strings.TrimSpace(*desc)
	}
	if models != nil {
		next.Models = normalizeModels(models)
	}
	s.groups[id] = next
	if err := s.saveLocked(); err != nil {
		s.groups[id] = old
		return nil, err
	}
	return cloneGroup(next), nil
}

// Delete 删分组。ID 不存在时返回 ErrNotFound 且**什么都不动**（不写盘）。
//
// ⚠ 本包不做级联：key 里对该分组的引用由 P5 的引用完整性负责。
// 在这里顺手改 apikey 会让两个存储互相依赖，且「删分组」与「清 key」必须能
// 分别重试（一个失败不该让另一个看起来也失败）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.groups[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(s.groups, id)
	if err := s.saveLocked(); err != nil {
		s.groups[id] = old
		return err
	}
	return nil
}

// Upsert 写入一个分组（按 ID 覆盖）。ID 为空时生成一个，CreatedAt 为空时补当前时间。
//
// 用途是「从别处搬过来的整条记录」（导入 / P3.5 的存量迁移）：Create 不接 ID，
// 而迁移需要能指定 ID（幂等：重复跑不会建出第二个分组）。
//
// 名字仍要唯一：拿 A 的 ID 去写 B 的名字 ⇒ ErrNameExists，不会出现两个同名分组。
func (s *Store) Upsert(g *Group) error {
	if g == nil {
		return errors.New("groupstore: 分组不能为 nil")
	}
	cp := cloneGroup(g)
	cp.Name = strings.TrimSpace(cp.Name)
	if cp.Name == "" {
		return ErrNameEmpty
	}
	cp.Description = strings.TrimSpace(cp.Description)
	cp.Models = normalizeModels(cp.Models)
	if strings.TrimSpace(cp.CreatedAt) == "" {
		cp.CreatedAt = s.now().UTC().Format(time.RFC3339)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(cp.ID) == "" {
		cp.ID = s.newIDLocked() // 同 Create：ID 必须在锁内生成，且不能撞已有的
	}
	if s.nameTakenLocked(cp.Name, cp.ID) {
		return fmt.Errorf("%w: %q", ErrNameExists, cp.Name)
	}
	old, existed := s.groups[cp.ID]
	s.groups[cp.ID] = cp
	if err := s.saveLocked(); err != nil {
		if existed {
			s.groups[cp.ID] = old
		} else {
			delete(s.groups, cp.ID)
		}
		return err
	}
	return nil
}

// newIDLocked 生成一个当前未被占用的 ID（调用方持锁）。
//
// 为什么不能直接复用 newID() 的返回值：随机 4 字节有碰撞概率，而这里赋值
// 是 map 写入 —— 撞上就会把旧分组**原地替换**掉，连报错都没有。
func (s *Store) newIDLocked() string {
	for i := 0; i < 8; i++ {
		id := newID()
		if id == "g-fallback" {
			// crypto/rand 失败：退化成"g-fallback-N" 仍要保证唯一，
			// 否则每次建分组都写同一个 ID，只剩最后一条。
			for n := 1; ; n++ {
				cand := id + "-" + strconv.Itoa(n)
				if _, used := s.groups[cand]; !used {
					return cand
				}
			}
		}
		if _, used := s.groups[id]; !used {
			return id
		}
	}
	// 连续 8 次都撞（实际上不可能）：宁可返回一个带后缀的确定唯一值，
	// 也不要让"建分组"失败 —— 调用方没有任何手段影响这件事。
	for n := 0; ; n++ {
		cand := "g-" + strconv.Itoa(n)
		if _, used := s.groups[cand]; !used {
			return cand
		}
	}
}

// nameTakenLocked 名字是否已被「别的分组」占用（调用方持锁）。excludeID 用于改名时排除自己。
//
// 名字比较**大小写敏感**：真正的引用是 ID，名字只用于展示与人工查；
// 强行折叠大小写会让 OpenAI 与 openai 无法共存，而用户很可能就是想区分它们。
func (s *Store) nameTakenLocked(name, excludeID string) bool {
	for id, g := range s.groups {
		if id == excludeID {
			continue
		}
		if g.Name == name {
			return true
		}
	}
	return false
}

// saveLocked 原子落盘（调用方持写锁）。排序输出：同一份配置反复保存不产生 diff 噪音。
func (s *Store) saveLocked() error {
	snap := snapshot{Version: 1, SavedAt: s.now()}
	for _, g := range s.groups {
		snap.Groups = append(snap.Groups, cloneGroup(g))
	}
	sortGroups(snap.Groups)
	out, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, out)
}

// writeFileAtomic 临时文件 + rename，避免写到一半被中断留下半个文件。
//
// 权限 0600 与 channelstore 一致：分组里虽然没有密钥，但同目录下其它文件都是
// 0600，这里放宽只会让「这个目录里哪些文件敏感」变得需要逐个判断。
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".groups-*.tmp")
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
	// Sync 之后再 rename：否则断电可能留下「文件名对、内容空」的文件 ——
	// 那正是「分组莫名变空」最常见的一种成因。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// normalizeModels 归一化模型清单：去首尾空白、丢空串、去重、排序。
//
// 为什么要做：同一批模型从表单（一行一个，随手带空格）、JSON 数组、以及
// 迁移脚本三个来源进来，形状必然不同。不归一化的话「分组里到底有几个模型」
// 会随输入写法变化，而前端的计数、diff 与去重判断全都建立在这个数字上。
//
// 返回值**总是非 nil**（空输入得到空切片）：这样 JSON 里写出来是 []，
// 而不是 null —— 前端不必为 null 单独写一条分支。
func normalizeModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// sortGroups 稳定排序：先按创建时间（用户看到的是「我建它们的顺序」），
// 同一秒内按名字（RFC3339 只到秒，并发建的分组会撞同一秒）。
func sortGroups(list []*Group) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].CreatedAt != list[j].CreatedAt {
			return list[i].CreatedAt < list[j].CreatedAt
		}
		return list[i].Name < list[j].Name
	})
}

// newID 造分组 id。用 crypto/rand 而不是自增序号：序号在「删一条再建一条」时会重号，
// 而 ID 会被 api key 引用 —— 重号会让某把 key 突然指向另一个分组。
func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在正常系统上不会失败；真失败了宁可给一个仍能工作的兜底值，
		// 也不要让「建分组」这个动作整个不可用（调用方还会做重名校验兜底）。
		return "g-fallback"
	}
	return "g-" + hex.EncodeToString(b[:])
}

// cloneGroup 深拷贝一个分组（Models 必须复制，否则调用方改到缓存里）。
func cloneGroup(g *Group) *Group {
	if g == nil {
		return nil
	}
	cp := *g
	cp.Models = append([]string(nil), g.Models...)
	if cp.Models == nil {
		cp.Models = []string{}
	}
	return &cp
}
