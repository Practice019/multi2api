// keyops.go 多 key 的运维操作：挑、启停、增删、掩码视图。
//
// # 对标 new-api
//
// new-api 把多 key 渠道当一等公民：channel_info 记 multi_key / key 数量 / 每 key 状态 /
// key 选择模式（random / round_robin），某个 key 坏了只动那一个 key，不停整条渠道。
// 这里就是那套语义的落点 —— 渠道结构（Keys[] + ChannelInfo.KeySelectionMode）早就按它建好了，
// 缺的是"怎么挑、怎么禁、怎么看"这几个动作。
//
// # 一个刻意的设计：挑 key 必须回下标
//
// 转发失败后要"把这个 key 自动禁用"。只知道 key 的**字符串**再去反查下标有两处坑：
// 用户可能填了两个完全相同的 key（反查到第一个 ⇒ 禁错条目），以及反查需要再拿一次锁
// （两次读之间渠道可能被改）。所以挑的时候直接回下标，这是唯一可定位的坐标。
package channelstore

import (
	"fmt"
	"strings"
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
func (s *Store) PickKeyIdx(ch *Channel) (int, string) {
	if ch == nil || len(ch.Keys) == 0 {
		return -1, ""
	}
	usable := make([]int, 0, len(ch.Keys))
	for i, k := range ch.Keys {
		if k.Status == StatusEnabled && k.Key != "" {
			usable = append(usable, i)
		}
	}
	if len(usable) == 0 {
		return -1, ""
	}
	if len(usable) == 1 {
		return usable[0], ch.Keys[usable[0]].Key
	}
	// 游标与随机源都要在锁里动：它们都是 Store 的共享状态。
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
	return pick, ch.Keys[pick].Key
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
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.channels[id]
	if !ok {
		return fmt.Errorf("渠道 %s 不存在", id)
	}
	if idx < 0 || idx >= len(ch.Keys) {
		return fmt.Errorf("key 下标 %d 越界（该渠道共有 %d 个 key）", idx, len(ch.Keys))
	}
	ch.Keys[idx].Status = status
	if status == StatusEnabled {
		ch.Keys[idx].Reason = ""
		ch.Keys[idx].DisabledAt = 0
	} else {
		ch.Keys[idx].Reason = reason
		ch.Keys[idx].DisabledAt = s.now().Unix()
	}
	return s.saveLocked()
}

// AddKeys 追加 key（一行一个，跳过空行与已存在的完全相同 key）。
//
// ⚠ 为什么要去重：重复 key 在轮询里会让同一把 key 被连续选中两次，
// 看起来像"轮询没生效"，实际是数据里有两条一样的记录。
func (s *Store) AddKeys(id string, keys []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.channels[id]
	if !ok {
		return 0, fmt.Errorf("渠道 %s 不存在", id)
	}
	seen := make(map[string]bool, len(ch.Keys))
	for _, k := range ch.Keys {
		seen[k.Key] = true
	}
	added := 0
	for _, raw := range keys {
		k := strings.TrimSpace(raw)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		ch.Keys = append(ch.Keys, ChannelKey{Key: k, Status: StatusEnabled})
		added++
	}
	if added == 0 {
		return 0, nil
	}
	ch.ChannelInfo.MultiKey = len(ch.Keys) > 1
	return added, s.saveLocked()
}

// RemoveKey 删掉下标 idx 的 key。
func (s *Store) RemoveKey(id string, idx int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.channels[id]
	if !ok {
		return fmt.Errorf("渠道 %s 不存在", id)
	}
	if idx < 0 || idx >= len(ch.Keys) {
		return fmt.Errorf("key 下标 %d 越界（该渠道共有 %d 个 key）", idx, len(ch.Keys))
	}
	ch.Keys = append(ch.Keys[:idx], ch.Keys[idx+1:]...)
	ch.ChannelInfo.MultiKey = len(ch.Keys) > 1
	return s.saveLocked()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.channels[id]
	if !ok {
		return fmt.Errorf("渠道 %s 不存在", id)
	}
	ch.ChannelInfo.KeySelectionMode = mode
	return s.saveLocked()
}

// KeysView 返回某渠道的 key 视图（用于「Key 管理」面板）。
func (s *Store) KeysView(id string) ([]KeyView, error) {
	ch, ok := s.Get(id)
	if !ok {
		return nil, fmt.Errorf("渠道 %s 不存在", id)
	}
	out := make([]KeyView, 0, len(ch.Keys))
	for i, k := range ch.Keys {
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
