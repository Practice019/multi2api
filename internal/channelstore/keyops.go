// keyops.go 多 key 的运维操作：挑、启停、增删、掩码视图。
//
// # 对标 new-api
//
// new-api 把多 key 渠道当一等公民：channel_info 记 multi_key / key 数量 / 每 key 状态 /
// key 选择模式（random / round_robin），某个 key 坏了只动那一个 key，不停整条渠道。
//
// # 一个刻意的设计：挑 key 必须回下标
//
// 转发失败后要"把这个 key 自动禁用"。只知道 key 的**字符串**再去反查下标有两处坑：
// 用户可能填了两个完全相同的 key（反查到第一个 ⇒ 禁错条目），以及反查需要再拿一次锁
// （两次读之间渠道可能被改）。所以挑的时候直接回下标，这是唯一可定位的坐标。
//
// # B1：读-改-写全部落在 A1 的 per-channel 锁里
//
// 三个写方法（SetKeyStatus / AddKeys / RemoveKey）都是"取行 → 改 key 列表与三张下标表
// → 写回"。这种 RMW 必须加锁，否则两个并发请求会互相覆盖（丢状态）。
// 锁**复用** store.GetChannelLock(int64)（A1 已有，per-channel），不要另造一把 ——
// 两把锁保护同一份数据等于没锁。
package channelstore

import (
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/store"
)

// key 被停用的原因（只用于展示与日志，不参与选路 —— 选路只看 Status）。
const (
	// ReasonUpstreamAuth 上游 401/403：key 无效、过期、没权限。
	// 这是**唯一**会自动禁用 key 的情形：它是确定性的（同一把 key 再打一万次还是 401）。
	ReasonUpstreamAuth = "upstream_auth"
	// ReasonManual 人为停用。
	ReasonManual = "manual"
)

// KeyView 给前端看的单个 key 视图。**不含明文** —— 要明文走 /admin/channels/reveal。
type KeyView struct {
	Index       int    `json:"index"`
	Mask        string `json:"mask"`
	Status      int    `json:"status"`
	Reason      string `json:"reason,omitempty"`
	DisabledAt  int64  `json:"disabled_at,omitempty"`
	Unavailable bool   `json:"unavailable"`
}

// MaskKey 打码：只留首 4 与末 4。短 key 全打码（露一半等于露全部）。
func MaskKey(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + "…" + k[len(k)-4:]
}

// PickKeyIdx 挑一个可用 key，返回**它在 ch.Keys 里的下标**与该 key。
//
// 语义（与 new-api 一致）：
//   - 只挑 Status == StatusEnabled 且非空的 key —— 手动停用与自动停用的都不参与；
//   - KeySelectionMode == "round_robin" ⇒ 按游标轮转（游标按**可用 key 的序号**推进，
//     不是绝对下标：中间有 key 被禁用时不会跳错）；
//   - 其它（""/"random"）⇒ 等概率随机。
//
// 全坏了返回 (-1, "")，调用方据此把渠道判为不可用（而不是拿空串当 key 去发请求）。
//
// ⚠ B1：本函数**不读库**，它只看传进来的 ch.Keys。调用方（Select/Get）拿到的渠道已经带
// 明文；从 List() 拿到的渠道只有掩码 —— 那种渠道不能用来转发（调用方务必用 Select/Get）。
func (s *Store) PickKeyIdx(ch *Channel) (int, string) {
	if ch == nil || len(ch.Keys) == 0 {
		return -1, ""
	}
	keys := ch.Keys
	// ⚠ 明文边界的一个必须的补丁：List() 给的是**掩码**（主脑裁定），而转发路径的候选
	// 正是从 channelroute.Plan ← List() 来的 ⇒ 直接拿 ch.Keys 去发请求会把掩码当 key 发出去
	// （实测形态：上游收到 "Bearer sk-e…-key"）。所以**仅当传进来的 key 是掩码时**
	// 回 SQL 取真 key。
	//
	// ⚠ 判据必须是"看起来是掩码"而不是"无条件回库"：本函数的历史语义是
	// **以传进来的 ch 为准**（自动禁用路径会先改内存态再落库），无条件回库会把调用方
	// 刚设的状态丢掉（单测 TestPickKeyHonoursStatus 正是这么钉的）。
	// MaskKey 的省略号 U+2026 是掩码的指纹 —— 真 key 是 ASCII，不会含它。
	if hasMaskedKeys(ch) {
		if plain, err := s.plaintextKeysOf(ch.ID); err == nil && len(plain) == len(keys) {
			keys = plain
		}
	}
	usable := make([]int, 0, len(keys))
	for i, k := range keys {
		if k.Status == StatusEnabled && k.Key != "" {
			usable = append(usable, i)
		}
	}
	if len(usable) == 0 {
		return -1, ""
	}
	if len(usable) == 1 {
		return usable[0], keys[usable[0]].Key
	}
	// 游标与随机源都要在锁里动：它们都是 Store 的共享状态（s.mu 只保护这两样）。
	s.mu.Lock()
	var pick int
	if ch.ChannelInfo.KeySelectionMode == "round_robin" {
		cursor := s.rr[ch.ID] % len(usable)
		s.rr[ch.ID] = (cursor + 1) % len(usable)
		pick = usable[cursor]
	} else {
		pick = usable[s.rand.Intn(len(usable))]
	}
	s.mu.Unlock()
	return pick, keys[pick].Key
}

// hasMaskedKeys 报告这条渠道的 key 是不是**掩码形态**（List() 的产物）。
//
// 判据是 MaskKey 的省略号 U+2026：真 key 是 ASCII，"…" 只可能来自掩码。
func hasMaskedKeys(ch *Channel) bool {
	if ch == nil {
		return false
	}
	for _, k := range ch.Keys {
		if strings.Contains(k.Key, "…") {
			return true
		}
	}
	return false
}

// plaintextKeysOf 取一条渠道的**明文** key 列表（按外部 id）。
// 渠道不在库里 / 没有外部 id ⇒ 回 error，调用方沿用自己手上的 keys。
func (s *Store) plaintextKeysOf(id string) ([]ChannelKey, error) {
	row, ok, err := rowByExternalID(id)
	if err != nil {
		return nil, err
	}
	if !ok || row == nil {
		return nil, fmt.Errorf("渠道 %s 不在库里", id)
	}
	plain, err := store.GetChannelKeys(row.ID)
	if err != nil {
		return nil, err
	}
	return keysFromRowWith(row, plain), nil
}

// keyRow 取一行 + 它的明文 key 列表（RMW 的公共前置）。
//
// 顺序有讲究：先解析外部 id 拿 int64 主键，再用**锁**保护后续的读-改-写。
// 明文只在进程内，绝不回写到返回值以外的地方。
func keyRow(id string) (*store.Channel, []string, error) {
	row, err := mustRowByExternalID(id)
	if err != nil {
		return nil, nil, err
	}
	keys, kerr := store.GetChannelKeys(row.ID)
	if kerr != nil {
		return nil, nil, kerr
	}
	return row, keys, nil
}

// persistKeys 把改好的 key 状态写回 SQL：**只写我们拥有的两列**（key + channel_info）。
//
// # 为什么不能用 store.UpdateChannel（整行覆盖）——实测踩到过
//
// 两个原因，任一都足以否掉整行覆盖：
//
//  1. **死锁**：UpdateChannel 内部自己会 `GetChannelLock(id).Lock()`。
//     而本文件的三个写方法必须**先持同一把锁**才能做"读-改-写"（不加锁就是丢更新，
//     见下面并发用例）。Go 的 sync.Mutex **不可重入** ⇒ 持锁后再调 UpdateChannel
//     就是自己等自己。第一版这么写，整个包测试直接挂住（不是失败，是卡死）。
//  2. **空串语义**：UpdateChannel 把 `Key == ""` 当"不传就不改"（channel.go:835），
//     于是"删到最后一把 key"这个**合法目标状态**永远写不进去。
//
// 所以 key 相关的写一律走这条单语句 Updates：key 与 channel_info 在**同一条 SQL** 里
// 一起写（本身即原子，不需要第二个事务），且不触碰行上的其它 30 多个字段 ——
// 那些字段归别的路径维护，整行覆盖才是真正的风险。
func persistKeys(row *store.Channel, keys []ChannelKey) error {
	applyKeysToRow(row, keys)
	info, err := json.Marshal(row.ChannelInfo)
	if err != nil {
		return fmt.Errorf("序列化 channel_info 失败: %w", err)
	}
	return store.DB.Model(&store.Channel{}).Where("id = ?", row.ID).
		Updates(map[string]any{"key": row.Key, "channel_info": string(info)}).Error
}

// persistChannelInfoOnly 只写 channel_info（选法切换用：渠道没变，只是模式变了）。
//
// ⚠ 绝不能顺手把 key 也写进去：这里的 row 来自 store.GetChannel（**Omit key**），
// 它的 Key 是空串 —— 一起写就等于把该渠道的 key 全抹掉。
func persistChannelInfoOnly(row *store.Channel) error {
	info, err := json.Marshal(row.ChannelInfo)
	if err != nil {
		return fmt.Errorf("序列化 channel_info 失败: %w", err)
	}
	return store.DB.Model(&store.Channel{}).Where("id = ?", row.ID).
		Updates(map[string]any{"channel_info": string(info)}).Error
}

// SetKeyStatus 改某个 key 的状态（手动启停与自动禁用都走这里）。
//
// status == Enabled 时清空 reason/disabled_at；否则记下原因与时间。
// ⚠ 不重建能力表：能力只看**渠道**状态与 models，单个 key 的死活不改变"这条渠道能服务哪些模型"。
func (s *Store) SetKeyStatus(id string, idx, status int, reason string) error {
	switch status {
	case StatusEnabled, StatusManuallyDisabled, StatusAutoDisabled:
	default:
		return fmt.Errorf("非法的 key 状态 %d", status)
	}
	row, _, err := keyRow(id)
	if err != nil {
		return err
	}
	lock := store.GetChannelLock(row.ID)
	lock.Lock()
	defer lock.Unlock()
	// 锁内**重新读**：锁外那次读只用来解析主键，不能拿它的快照去改（否则丢并发写）。
	fresh, plain, err := keyRow(id)
	if err != nil {
		return err
	}
	keys := keysFromRowWith(fresh, plain)
	if idx < 0 || idx >= len(keys) {
		return fmt.Errorf("key 下标 %d 越界（该渠道共有 %d 个 key）", idx, len(keys))
	}
	keys[idx].Status = status
	if status == StatusEnabled {
		keys[idx].Reason = ""
		keys[idx].DisabledAt = 0
	} else {
		keys[idx].Reason = reason
		keys[idx].DisabledAt = s.now().Unix()
	}
	return persistKeys(fresh, keys)
}

// AddKeys 追加 key（一行一个，跳过空行与已存在的完全相同 key）。
//
// ⚠ 为什么要去重：重复 key 在轮询里会让同一把 key 被连续选中两次，
// 看起来像"轮询没生效"，实际是数据里有两条一样的记录。
func (s *Store) AddKeys(id string, keys []string) (int, error) {
	row, _, err := keyRow(id)
	if err != nil {
		return 0, err
	}
	lock := store.GetChannelLock(row.ID)
	lock.Lock()
	defer lock.Unlock()
	fresh, plain, err := keyRow(id)
	if err != nil {
		return 0, err
	}
	cur := keysFromRowWith(fresh, plain)
	seen := make(map[string]bool, len(cur))
	for _, k := range cur {
		seen[k.Key] = true
	}
	added := 0
	for _, raw := range keys {
		k := strings.TrimSpace(raw)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		cur = append(cur, ChannelKey{Key: k, Status: StatusEnabled})
		added++
	}
	if added == 0 {
		return 0, nil
	}
	// MultiKey/IsMultiKey 由 persistKeys → applyKeysToRow → row.SetKeys 按新长度维护，
	// 这里不要再手写一份（两处各写一次必然会漂移）。
	if err := persistKeys(fresh, cur); err != nil {
		return added, err
	}
	return added, nil
}

// RemoveKey 删掉下标 idx 的 key。
//
// ⚠ 删完要把三张下标表**整体重写**（applyKeysToRow 里的顺序约束）：否则原下标 idx+1
// 的状态会留在原位，指向一把已经不存在的 key。
func (s *Store) RemoveKey(id string, idx int) error {
	row, _, err := keyRow(id)
	if err != nil {
		return err
	}
	lock := store.GetChannelLock(row.ID)
	lock.Lock()
	defer lock.Unlock()
	fresh, plain, err := keyRow(id)
	if err != nil {
		return err
	}
	cur := keysFromRowWith(fresh, plain)
	if idx < 0 || idx >= len(cur) {
		return fmt.Errorf("key 下标 %d 越界（该渠道共有 %d 个 key）", idx, len(cur))
	}
	cur = append(cur[:idx], cur[idx+1:]...)
	return persistKeys(fresh, cur)
}

// SetKeySelectionMode 设多 key 的选法（""/"random"/"round_robin"）。
//
// 非法值直接拒：PickKeyIdx 对不认识的值会**静默落回随机**，那会让用户以为设了轮询其实没设。
func (s *Store) SetKeySelectionMode(id, mode string) error {
	switch mode {
	case "", "random", "round_robin":
	default:
		return fmt.Errorf("key_selection_mode 只能是 random / round_robin（空 = 随机）")
	}
	row, err := mustRowByExternalID(id)
	if err != nil {
		return err
	}
	lock := store.GetChannelLock(row.ID)
	lock.Lock()
	defer lock.Unlock()
	fresh, err := store.GetChannel(row.ID)
	if err != nil || fresh == nil {
		return fmt.Errorf("渠道 %s 不存在", id)
	}
	// 只动 mode：只写 channel_info 一列（见 persistChannelInfoOnly 的警告）。
	fresh.ChannelInfo.MultiKeyMode = modeToStore(mode)
	return persistChannelInfoOnly(fresh)
}

// KeysView 返回某渠道的 key 视图（用于「Key 管理」面板）。
//
// **只回掩码**：明文走 /admin/channels/reveal 与 keys?reveal=1 两个显式入口。
func (s *Store) KeysView(id string) ([]KeyView, error) {
	row, plain, err := keyRow(id)
	if err != nil {
		return nil, err
	}
	keys := keysFromRowWith(row, plain)
	out := make([]KeyView, 0, len(keys))
	for i, k := range keys {
		out = append(out, KeyView{
			Index:      i,
			Mask:       MaskKey(k.Key),
			Status:     k.Status,
			Reason:     k.Reason,
			DisabledAt: k.DisabledAt,
			// unavailable 只标"被系统自动停掉"的：手动停用是用户自己的决定，
			// 前端该安静地显示灰，不该报警红让用户以为出故障了。
			Unavailable: k.Status == StatusAutoDisabled,
		})
	}
	return out, nil
}
