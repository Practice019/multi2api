// Package channelstore 存"渠道"与"能力索引"，形状对标 new-api。
//
// # B1：存储后端已从 JSON 文件换成 SQL（internal/store 的渠道仓储）
//
// 对外**方法签名一个都没变**（admin / server / channelroute 零改动）—— 变的是里面：
//
//	旧：一个 JSON 文件两段（channels + abilities）全读进内存，改完整体重写文件
//	新：一律走 store.ChannelXxx / store.AbilityXxx，SQL 是唯一权威
//
// ⚠ 外部可寻址 id（ch-xxxxxxxx）与 SQL 自增主键（int64）**不是同一个东西**：
// 前者由 admin 生成并出现在前端，后者是行主键、也是 abilities.channel_id 指向的东西。
// 两者的翻译只发生在 convert.go（主脑裁决的方案 A：A1 加了 ExternalID 列）。
//
// ⚠ 明文 key 的边界（主脑裁定）：List() 只回**掩码**；Get() / Select() 回**明文**
// （转发要真 key、reveal 要能显示、keys?reveal=1 要能管理）。明文只经
// store.GetChannelKeys 这一个显式入口取，且绝不写进 List 的返回值。
//
// ⚠ 与账号型上游的关系：本包**不碰** auths/ 与 Pool（账号的选号/惩罚/轮换在那边）。
// Key 型渠道没有"号"可换，进 Pool 会把"一个 key 被限流"误判成"账号坏了"。
package channelstore

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/store"
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
//	new-api                          channelstore
//	Id / Name / Type                 ID / Name / Type
//	Key（多 key 时用别的字段）        Keys[]（multi-key 一等公民）
//	BaseURL / Models                 BaseURL / Models
//	ModelMapping                     ModelMapping
//	Group / Priority / Weight        Group / Priority / Weight
//	Status                           Status
//	（overrides）                     Overrides
//	（channel info）                  ChannelInfo
type Channel struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     int               `json:"type"`          // 渠道类型（OpenAI 兼容 = TypeOpenAICompat）
	BaseURL  string            `json:"base_url"`      // R3：手填
	Keys     []ChannelKey      `json:"keys"`          // R3：手填（支持多 key）
	Models   []string          `json:"models"`        // 该渠道**声明**能服务的模型（上游原名）
	ModelMap map[string]string `json:"model_mapping"` // R2：公开名 → 上游名
	// Group 主分组（**兼容字段**，多分组下等于 Groups[0]）。
	//
	// 存量 channels.json 里只有这个字段，而且旧版 exe 回退时要能读懂新文件形态。
	// 读入时若 Groups 为空，就用 [Group] 或 [DefaultGroup] 补齐（见 normalizeChannel）；
	// 写出时把它同步成 Groups[0]。
	//
	// ⚠ 新代码**不要**再直接读它做判定 —— 那是"只看第一个分组"的老语义。
	// 表示"这条渠道属于哪些组"的唯一入口是 EffectiveGroups()。
	Group string `json:"group"`
	// Groups 这条渠道所属的分组（多值，对齐 new-api 的逗号分隔 Group）。
	//
	// 一个上游模型常常同时属于"默认"和"VIP"两档：不是复制两条渠道（那要维护两份
	// base_url/key，改一个漏一个），而是同一条渠道进两个组。选路时按组过滤，
	// 能力索引对 (models × groups) 做笛卡尔积 —— 与 new-api 的 AddAbilities 同一做法。
	//
	// 空 = 用 Group / DefaultGroup 补齐（存量文件就是这个形态）。
	Groups      []string    `json:"groups,omitempty"`
	Priority    int         `json:"priority"` // 越大越先用；重试第 n 次走第 n 档
	Weight      int         `json:"weight"`   // 同一档内加权随机的权重
	Status      int         `json:"status"`
	Overrides   Overrides   `json:"overrides"`
	ChannelInfo ChannelInfo `json:"channel_info"`
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
	// 实测 /v1/models 里渠道模型曾**两个字段都没有**（同表其它模型有值），客户端做
	// 上下文预算时无法处理。0 = 未知 ⇒ 输出时整个字段不写（不是写 0）。
	//
	// ⚠ B1：冻结的 Channel 契约里**没有**对应列，它落在 OtherSettings 的命名空间键下
	//（见 convert.go 的文件头第 3 条与不变量 I4）。
	ContextLength int64 `json:"context_length,omitempty"`

	// MaxOutputTokens 本渠道模型的单次最大输出（0 = 未知）。同上落在 OtherSettings。
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}

// Ability 是"某个组里的某个模型，可以被某个渠道服务"这条 join 记录。
// 主键 (Group, Model, ChannelID) —— 与 new-api 的复合主键一致。
//
// ⚠ B1 起它是**投影**：真值在 SQL 的 abilities 表 + channels 行的 priority/weight。
type Ability struct {
	Group     string `json:"group"`
	Model     string `json:"model"`
	ChannelID string `json:"channel_id"`
	Enabled   bool   `json:"enabled"`
	Priority  int    `json:"priority"`
	Weight    int    `json:"weight"`
	Tag       string `json:"tag,omitempty"`
}

// snapshot 是**历史**落盘形状（B1 之前是权威，现在只用于一次性回填）。
type snapshot struct {
	Version   int               `json:"version"`
	SavedAt   time.Time         `json:"saved_at"`
	Channels  []*Channel        `json:"channels"`
	Abilities []*Ability        `json:"abilities"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// Store 是渠道与能力索引的存储。B1 起**所有读写都走 SQL**（store 包）。
//
// 剩下的进程内状态只有三样，都不进库：
//
//	legacyPath —— 一次性回填的真源（auths/channels.json），回填后只用于日志
//	rand / rr  —— 选路的随机源与 round_robin 游标（选路是每次调用的决策，不是事实）
//
// ⚠ 没有内存缓存：SQL 是唯一权威。加缓存等于制造第二真相源，
// 而"改了渠道但选路还是旧的"是这类缓存最典型的故障（且极难归因）。
type Store struct {
	legacyPath string

	now func() time.Time // 便于测试注入

	// mu 只保护 rand / rr（进程内共享的可变状态）。
	// 渠道行的并发读改写由 store.GetChannelLock(int64) 负责 —— **复用 A1 那把**，不要另造。
	mu   sync.Mutex
	rand *rand.Rand
	rr   map[string]int
}

// New 打开渠道存储。
//
// # 参数 path 的含义变了（但签名没变）
//
// 它不再是"数据库文件"，而是**历史真源**：SQL 的 channels 表为空、且该文件存在时，
// 一次性把里面的渠道导进 SQL（随后 SQL 就是唯一权威）。
//
// 三条行为约定（主脑裁定）：
//
//	store.DB == nil      → 返回 error（装配错误启动即见，不要静默起一个空库）
//	表非空               → 跳过回填（幂等：跑一百次也只导一次）
//	表空且文件不存在     → 允许起空库，但**打一行日志**（排查"渠道没了"时它是第一条线索）
func New(path string) (*Store, error) {
	if store.DB == nil {
		return nil, fmt.Errorf("channelstore: 数据库未打开（需要先 store.Open + store.Migrate）")
	}
	s := &Store{
		legacyPath: path,
		now:        time.Now,
		rand:       newRand(time.Now().UnixNano()),
		rr:         map[string]int{},
	}
	if err := s.backfillLegacyOnce(); err != nil {
		return nil, err
	}
	return s, nil
}

// backfillLegacyOnce 把历史 channels.json 一次性导进 SQL（幂等）。
//
// ⚠ 幂等判据是"channels 表**非空**即跳过"，不是"看过某个标记文件"：
// 标记文件会与"用户手工删库"打架（库空了、标记还在 ⇒ 再也回填不进来），
// 而"表非空"在任何时候都表达同一个事实。
func (s *Store) backfillLegacyOnce() error {
	n, err := store.CountChannels()
	if err != nil {
		return fmt.Errorf("channelstore: 读 channels 行数失败: %w", err)
	}
	if n > 0 {
		return nil
	}
	b, err := os.ReadFile(s.legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("channelstore: SQL 里没有渠道，且历史文件 %s 不存在 —— 按空库启动", s.legacyPath)
			return nil
		}
		return fmt.Errorf("channelstore: 读历史文件 %s 失败: %w", s.legacyPath, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		log.Printf("channelstore: 历史文件 %s 是空的 —— 按空库启动", s.legacyPath)
		return nil
	}
	var snap snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return fmt.Errorf("channelstore: 解析历史文件 %s 失败: %w", s.legacyPath, err)
	}
	imported := 0
	for _, ch := range snap.Channels {
		if ch == nil || strings.TrimSpace(ch.ID) == "" {
			continue
		}
		if err := s.Upsert(ch); err != nil {
			return fmt.Errorf("channelstore: 回填渠道 %s 失败: %w", ch.ID, err)
		}
		imported++
	}
	log.Printf("channelstore: 已从 %s 一次性回填 %d 条渠道到 SQL（幂等：channels 表非空即跳过）",
		s.legacyPath, imported)
	return nil
}

// normalizeChannel 补齐缺省值（Group 空 ⇒ default；Status 0 ⇒ Enabled）。
func normalizeChannel(ch *Channel) {
	// Groups 与 Group 双向补齐（见 Channel.Groups 的注释）：
	//
	//	Groups 非空 ⇒ 以它为准，Group 同步成 Groups[0]（旧版 exe 读得懂）
	//	Groups 为空 ⇒ 用 [Group] 或 [DefaultGroup] 补上
	ch.Groups = effectiveGroupsOf(ch.Groups, ch.Group)
	ch.Group = ch.Groups[0]
	if ch.Status == 0 {
		ch.Status = StatusEnabled
	}
	if ch.ModelMap == nil {
		ch.ModelMap = map[string]string{}
	}
	ch.ChannelInfo.MultiKey = len(ch.Keys) > 1
}

// EffectiveGroups 这条渠道实际所属的分组（已归一化，**至少一个**）。
//
// 这是"这条渠道属于哪些组"的唯一入口。任何按组过滤的地方（选路、目录）都必须调它，
// 不要直接读 ch.Group —— 那是"只看第一个分组"的老语义，多分组渠道会被静默漏掉。
func (ch *Channel) EffectiveGroups() []string {
	if ch == nil {
		return []string{DefaultGroup}
	}
	return effectiveGroupsOf(ch.Groups, ch.Group)
}

// effectiveGroupsOf 合并 Groups 与 Group 得到一个确定的分组清单（**至少一个**）。
//
// 返回的切片总是新的：调用方可能把它 append 到别处，共用一个底层数组会让
// "改返回值"静默污染渠道对象（本包在 cloneChannel 上踩过同一类问题）。
func effectiveGroupsOf(groups []string, group string) []string {
	out := make([]string, 0, len(groups)+1)
	seen := map[string]bool{}
	add := func(g string) {
		g = strings.TrimSpace(g)
		if g == "" || seen[g] {
			return
		}
		seen[g] = true
		out = append(out, g)
	}
	for _, g := range groups {
		add(g)
	}
	add(group)
	if len(out) == 0 {
		out = append(out, DefaultGroup)
	}
	return out
}

// List 返回全部渠道（按 ID 排序，稳定输出便于前端与测试）。
//
// # keys 为什么是**掩码**（主脑裁定的明文边界）
//
// 列表是每 30 秒轮询的接口，明文塞进来等于每次轮询都把凭据发一遍。
// 但掩码必须由明文算出来，而 store 的列表读路径 Omit("key") ⇒ 这里对**每一行**
// 调 store.GetChannelKeys 取明文、立刻 MaskKey、只把掩码放进返回值。
//
// 取舍与规模前提（主脑要求写明）：一次 List = N+1 次查询。渠道数在 13~25 量级
// （手工维护）时完全可接受；**将来渠道上百条时要改成批量取 key**（一次 IN 查询），
// 否则列表会退化成几百次串行查询。
//
// 签名没有 error（冻结），DB 故障时返回 nil 并打日志：调用方会显示"加载失败"，
// 不会静默显示成"没有渠道"——错误处理的落点在 HTTP 层。
func (s *Store) List() []*Channel {
	rows, err := store.ListChannels()
	if err != nil {
		log.Printf("channelstore: 读渠道列表失败: %v", err)
		return nil
	}
	out := make([]*Channel, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		plain, kerr := store.GetChannelKeys(row.ID)
		if kerr != nil {
			log.Printf("channelstore: 读渠道 %s 的 key 失败（只影响列表的掩码列）: %v", row.ExternalID, kerr)
			plain = nil
		}
		out = append(out, toChannelView(row, maskedKeys(keysFromRowWith(row, plain))))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get 取一条渠道。**带明文 key** —— 它是 reveal / keys?reveal=1 / 自动发现三条
// 显式路径唯一依赖的明文入口（冻结签名里没有第二个）。
//
// ⚠ 因此调用方有责任不把它直接序列化出去：admin 的 viewOf 只取掩码（MaskID），
// 明文只走 /admin/channels/reveal 与 keys?reveal=1 两个显式端点。
func (s *Store) Get(id string) (*Channel, bool) {
	row, ok, err := rowByExternalID(id)
	if err != nil {
		log.Printf("channelstore: 按 id=%s 取渠道失败: %v", id, err)
		return nil, false
	}
	if !ok || row == nil {
		return nil, false
	}
	plain, kerr := store.GetChannelKeys(row.ID)
	if kerr != nil {
		log.Printf("channelstore: 读渠道 %s 的 key 失败: %v", id, kerr)
		plain = nil
	}
	return toChannelView(row, keysFromRowWith(row, plain)), true
}

// Upsert 新增或覆盖一条渠道（ID 由调用方/管理端生成，空则报错）。
//
// 能力行由 store 的写路径在**同一事务**里重建（CreateChannel/UpdateChannel
// 内部调 ability.go 的 AddAbilities/UpdateAbilities）——所以这里不要自己再建一遍，
// 否则会出现"渠道写成功、能力行写两次"的半截状态。
func (s *Store) Upsert(ch *Channel) error {
	if ch == nil || strings.TrimSpace(ch.ID) == "" {
		return fmt.Errorf("渠道 ID 不能为空")
	}
	cp := cloneChannel(ch)
	normalizeChannel(cp)
	row, exists, err := rowByExternalID(cp.ID)
	if err != nil {
		return err
	}
	if exists && row != nil {
		applyChannelToRow(row, cp)
		return store.UpdateChannel(row)
	}
	fresh := &store.Channel{}
	applyChannelToRow(fresh, cp)
	fresh.CreatedTime = s.now().Unix()
	id, err := store.CreateChannel(fresh)
	if err != nil {
		return err
	}
	// 保险：CreateChannel 应已持久化 ExternalID（A1 已确认）。万一某个版本没带上，
	// 这里补一次 —— 补不上就意味着"刚建的渠道立刻查不到"，那是最难查的一类故障。
	if cur, ok, gerr := store.GetChannelByExternalID(cp.ID); gerr == nil && ok && cur != nil && cur.ID == id {
		return nil
	}
	if _, serr := store.SetChannelExternalID(id, cp.ID); serr != nil {
		return fmt.Errorf("channelstore: 渠道 %s 已建（id=%d）但外部 id 没写上: %w", cp.ID, id, serr)
	}
	return nil
}

// SetStatus 只改状态（对标 new-api 的"status can be updated in bulk by tag"的单条版本）。
//
// 副作用（store 侧同一事务）：abilities.enabled 跟着翻转（停用 ⇒ 该渠道的能力不再被
// 选到，但绑定**保留**，便于恢复）。1/2/3 全部委托 store.SetChannelStatus（3 已放开）。
func (s *Store) SetStatus(id string, status int) error {
	row, err := mustRowByExternalID(id)
	if err != nil {
		return err
	}
	if _, err := store.SetChannelStatus(row.ID, status, ""); err != nil {
		return err
	}
	return nil
}

// Delete 删渠道并连带删掉它的能力行（store 侧同一事务）。
func (s *Store) Delete(id string) error {
	row, err := mustRowByExternalID(id)
	if err != nil {
		return err
	}
	return store.DeleteChannel(row.ID)
}

// EnabledModels 返回某组里"至少有一个启用渠道能服务"的模型集合（升序）。
// 这是对外模型目录的数据源（对标 new-api 的 GetGroupEnabledModels）。
//
// ⚠ 读的是**能力表**而不是"渠道声明的 models"：两者在正常情况下一致，但能力表才是
// 选路真正读的那张。按渠道现算会掩盖"能力行缺失"，而那种状态下请求根本选不到渠道。
func (s *Store) EnabledModels(group string) []string {
	if group == "" {
		group = DefaultGroup
	}
	abs, err := store.ListAbilities()
	if err != nil {
		log.Printf("channelstore: 读能力表失败: %v", err)
		return nil
	}
	seen := map[string]bool{}
	for _, ab := range abs {
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
//
// ChannelID 是**外部可寻址 id**（ch-xxxxxxxx），与 Ability 类型的既有契约一致；
// 没有对应渠道（或渠道没有外部 id）的能力行跳过 —— 那种行选路也不会用。
func (s *Store) Abilities() []*Ability {
	rows, err := store.ListChannels()
	if err != nil {
		log.Printf("channelstore: 读渠道表失败: %v", err)
		return nil
	}
	byID := make(map[int64]*store.Channel, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	abs, err := store.ListAbilities()
	if err != nil {
		log.Printf("channelstore: 读能力表失败: %v", err)
		return nil
	}
	out := make([]*Ability, 0, len(abs))
	for _, ab := range abs {
		// ⚠ 只回**启用**的能力行：SQL 侧"停用渠道"是保留绑定 + enabled=false（便于恢复），
		// 而本包的公开语义（老 JSON 实现 + 管理台的 ability_count 判据）是
		// "停用的渠道不产生能力"。要恢复时把渠道启用即可，绑定一直都在。
		if !ab.Enabled {
			continue
		}
		row, ok := byID[ab.ChannelID]
		if !ok || row == nil || strings.TrimSpace(row.ExternalID) == "" {
			continue
		}
		out = append(out, &Ability{
			Group:     ab.Group,
			Model:     ab.Model,
			ChannelID: row.ExternalID,
			Enabled:   ab.Enabled,
			Priority:  int(derefInt64(row.Priority)),
			Weight:    int(derefUint(row.Weight)),
			Tag:       row.Name,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].ChannelID < out[j].ChannelID
	})
	return out
}

// cloneChannel 深拷贝一条渠道（map/slice 都要复制，否则调用方改到缓存）。
func cloneChannel(ch *Channel) *Channel {
	if ch == nil {
		return nil
	}
	cp := *ch
	cp.Keys = append([]ChannelKey(nil), ch.Keys...)
	cp.Models = append([]string(nil), ch.Models...)
	// Groups 是切片：不复制的话调用方改一下返回值就改到了缓存。
	cp.Groups = append([]string(nil), ch.Groups...)
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
