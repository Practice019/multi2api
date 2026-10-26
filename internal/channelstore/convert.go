// convert.go —— SQL（internal/store 的渠道仓储）与 channelstore 自有类型之间的**边界翻译层**（B1）。
//
// # 这一层存在的理由
//
// B1 要把 channelstore 的存储后端从 JSON 换成 SQL，但**导出方法签名一个都不许动**
// （admin / server / channelroute 零改动）。两边的类型不是同一个：
//
//	channelstore.Channel.ID  —— string（admin 造的 ch-xxxxxxxx，外部可寻址 id）
//	store.Channel.ID         —— int64（SQL 自增主键，abilities.channel_id 指向它）
//
// 主脑裁决（方案 A）：A1 在冻结契约上加了 ExternalID 列 + GetChannelByExternalID。
// 于是"字符串 id ⇄ 内部主键"的翻译**只能发生在这一层**，业务代码看不到 int64。
//
// # 三个必须记住的映射细节
//
//  1. **多 Key 状态**：真值永远在 store 的 Key 列（换行分隔）+ ChannelInfo 的那三张
//     map（下标 → 状态/原因/时刻）。本文件负责把 []ChannelKey ⇄ 这两处**一起**读写 ——
//     只改一处就会得到"库里 key 有 3 把、状态表还是 2 把"的鬼状态。
//  2. **选择模式的字面量不同**：channelstore 用 "round_robin"，store 用 store.MultiKeyModePolling
//     （"polling"，hyxapi 口径）。这一层翻译，**不要**把它漏到两边任何一侧去比较。
//  3. **channelstore 有两个字段在冻结契约里没有对应列**（ChannelInfo.ContextLength /
//     MaxOutputTokens）。它们落在 OtherSettings（settings 列，"不需要检索的补充设置"）
//     的**命名空间键**下：存量 JSON 里它们有真实值，丢掉会让 /v1/models 的 ctx 字段
//     静默消失（那正是当初加这两个字段要修的用户报障）。
//
// # 不变量清单（改这一层前先读；每条都有 convert_test.go 的用例钉着）
//
//	I1  Key 列与三张下标表**同生同死**：applyKeysToRow 必须先 SetKeys（它按新长度重算
//	    MultiKeySize 并截断越界下标）再覆盖 MultiKeyStatusList/DisabledReason/DisabledTime。
//	    顺序颠倒 ⇒ 删掉一把 key 之后仍留着指向旧下标的状态（幽灵状态）。
//	I2  没被记录的下标 = **启用**（keysFromRow 的缺省是 StatusEnabled）：反过来会让
//	    新加的 key 一进来就是坏的。
//	I3  选择模式是**两种字面量**：channelstore "round_robin" ⇄ store "polling"
//	    （store.MultiKeyModePolling），只在本层翻译；两侧任何地方都不许直接比较。
//	I4  OtherSettings 是**合并写**：只增删自己的命名空间键（channelExtrasKey），
//	    别人的键必须逐字保留；解析失败时**不覆盖**（不替别的写入者清洗数据）。
//	I5  maskedKeys 只改副本：掩码绝不写回库里（写回去就再也拿不回明文了）。
//	I6  空 ExternalID 不当外部可寻址（rowByExternalID 直接短路）；清空必须走
//	    store.SetChannelExternalID(id, "")，因为 UpdateChannel 的空串语义是"不传就不改"。
package channelstore

import (
	"encoding/json"
	"fmt"
	"strings"

	"workbuddy2api/internal/store"
)

// ── 外部 id（ch-xxxxxxxx）⇄ SQL 主键 ────────────────────────────────────────

// rowByExternalID 按外部可寻址 id 取一行。
//
// 空串直接当"不存在"：store 那边的 GetChannelByExternalID("") 也这么判，
// 但这里再挡一次，免得将来有人改成"按空串匹配"时静默命中所有非外部行。
func rowByExternalID(id string) (*store.Channel, bool, error) {
	s := strings.TrimSpace(id)
	if s == "" {
		return nil, false, nil
	}
	return store.GetChannelByExternalID(s)
}

// mustRowByExternalID 取行；不存在时给一条**与旧实现文案一致**的错误。
//
// 旧实现（JSON 版）在各处写的是 "渠道 %s 不存在"（Get 用 (nil,false)，不改动的那几处
// 直接返回 error）。这里保留同一句文案：调用方与测试都在匹配它。
func mustRowByExternalID(id string) (*store.Channel, error) {
	row, ok, err := rowByExternalID(id)
	if err != nil {
		return nil, err
	}
	if !ok || row == nil {
		return nil, fmt.Errorf("渠道 %s 不存在", id)
	}
	return row, nil
}

// ── 多 Key 状态 ⇄ store 的 Key 列 + ChannelInfo ─────────────────────────────

// keysFromRow 把一行还原成 []ChannelKey（**明文**）。
//
// Status 缺省是 StatusEnabled：store 的 MultiKeyStatusList 只记录"被改过的"下标，
// 没记录的必须是启用 —— 反过来（缺省 disabled）会让新加的 key 一进来就是坏的。
func keysFromRow(row *store.Channel) []ChannelKey {
	if row == nil {
		return nil
	}
	return keysFromRowWith(row, row.GetKeys())
}

// keysFromRowWith 同上，但明文由调用方显式给。
//
// # 为什么必须分成两个函数（B1 的明文边界）
//
// store 的读路径（GetChannel / ListChannels / GetChannelByExternalID）**一律 Omit("key")**
// —— 那是有意的（hyxapi 对等 + 本仓 D1 口径）。所以"行"里通常**没有明文**：
// 状态/原因/时刻从行里读，key 的字面量只能从显式入口 store.GetChannelKey(s) 取来传进去。
//
// ⚠ 传进来的明文**只许留在本进程**：List() 拿它算完掩码就丢（见 List 的注释）。
func keysFromRowWith(row *store.Channel, raw []string) []ChannelKey {
	if row == nil {
		return nil
	}
	out := make([]ChannelKey, 0, len(raw))
	for i, k := range raw {
		ck := ChannelKey{Key: k, Status: StatusEnabled}
		if st, ok := row.ChannelInfo.MultiKeyStatusList[i]; ok && st != 0 {
			ck.Status = st
		}
		ck.Reason = row.ChannelInfo.MultiKeyDisabledReason[i]
		ck.DisabledAt = row.ChannelInfo.MultiKeyDisabledTime[i]
		out = append(out, ck)
	}
	return out
}

// maskedKeys 把 key 换成掩码（列表/详情/KeysView 这三条**对外**路径用）。
//
// ⚠ 不要把掩码写回库里：掩码是展示形态，写回去就再也拿不回明文了。
func maskedKeys(keys []ChannelKey) []ChannelKey {
	out := make([]ChannelKey, 0, len(keys))
	for _, k := range keys {
		cp := k
		cp.Key = MaskKey(k.Key)
		out = append(out, cp)
	}
	return out
}

// applyKeysToRow 把 []ChannelKey 写进一行（Key 列 + 三张状态表**一起**写）。
//
// 顺序不能换：先 SetKeys（它按**新**长度重算 MultiKeySize 并截断旧状态表的越界下标），
// 再整体覆盖三张表。反过来会让"删掉一把 key"之后还留着指向旧下标的状态。
func applyKeysToRow(row *store.Channel, keys []ChannelKey) {
	if row == nil {
		return
	}
	raw := make([]string, 0, len(keys))
	statuses := make(map[int]int, len(keys))
	reasons := make(map[int]string, len(keys))
	times := make(map[int]int64, len(keys))
	for i, k := range keys {
		raw = append(raw, k.Key)
		if k.Status != 0 {
			statuses[i] = k.Status
		}
		if k.Reason != "" {
			reasons[i] = k.Reason
		}
		if k.DisabledAt != 0 {
			times[i] = k.DisabledAt
		}
	}
	row.SetKeys(raw, len(raw) > 1)
	row.ChannelInfo.MultiKeyStatusList = statuses
	row.ChannelInfo.MultiKeyDisabledReason = reasons
	row.ChannelInfo.MultiKeyDisabledTime = times
}

// ── 选择模式的字面量翻译（两边不同名，见文件头第 2 条）──────────────────────

func modeFromStore(m string) string {
	switch m {
	case store.MultiKeyModePolling:
		return "round_robin"
	case store.MultiKeyModeRandom:
		return "random"
	default:
		return ""
	}
}

func modeToStore(m string) string {
	switch m {
	case "round_robin":
		return store.MultiKeyModePolling
	case "random":
		return store.MultiKeyModeRandom
	default:
		return ""
	}
}

// ── ContextLength / MaxOutputTokens 的落点（见文件头第 3 条）────────────────

// channelExtrasKey 是 OtherSettings(settings 列) 里的命名空间键。
//
// 为什么用 OtherSettings 而不是 Setting：后者注释写明是"代理、Vertex key 类型"这类
// **上游语义**的设置，会被 hyxapi 对等代码读写；OtherSettings 的定位正是"不需要检索的
// 补充设置"，与本层用途一致，且我们不按它做任何查询。
const channelExtrasKey = "wb2api_channelstore"

type channelExtras struct {
	ContextLength   int64 `json:"context_length,omitempty"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}

func extrasFromRow(row *store.Channel) channelExtras {
	var ex channelExtras
	if row == nil || strings.TrimSpace(row.OtherSettings) == "" {
		return ex
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row.OtherSettings), &all); err != nil {
		return ex
	}
	raw, ok := all[channelExtrasKey]
	if !ok {
		return ex
	}
	_ = json.Unmarshal(raw, &ex)
	return ex
}

func extrasToRow(row *store.Channel, ex channelExtras) {
	if row == nil {
		return
	}
	all := map[string]json.RawMessage{}
	if strings.TrimSpace(row.OtherSettings) != "" {
		// 解析失败时**不覆盖**：库里那段可能是别的写入者放的脏数据，
		// 我们只负责自己的命名空间键，不替别人做数据清洗。
		if err := json.Unmarshal([]byte(row.OtherSettings), &all); err != nil {
			return
		}
	}
	if ex.ContextLength == 0 && ex.MaxOutputTokens == 0 {
		delete(all, channelExtrasKey)
	} else {
		b, err := json.Marshal(ex)
		if err != nil {
			return
		}
		all[channelExtrasKey] = b
	}
	if len(all) == 0 {
		row.OtherSettings = ""
		return
	}
	b, err := json.Marshal(all)
	if err != nil {
		return
	}
	row.OtherSettings = string(b)
}

// ── 行 → channelstore.Channel ──────────────────────────────────────────────

// toChannelView 把一行翻译成 channelstore.Channel。
//
// keys 由调用方给（明文 / 掩码两条路，见 keysFromRow 与 maskedKeys）——
// 翻译层不自己决定"要不要带明文"，那是**路径**的属性，不是类型的属性。
func toChannelView(row *store.Channel, keys []ChannelKey) *Channel {
	if row == nil {
		return nil
	}
	ex := extrasFromRow(row)
	ch := &Channel{
		ID:       row.ExternalID,
		Name:     row.Name,
		Type:     row.Type,
		BaseURL:  derefString(row.BaseURL),
		Keys:     keys,
		Models:   splitCSV(row.Models),
		ModelMap: strMapFromJSON(row.ModelMapping),
		Group:    row.Group,
		Groups:   row.GroupsOf(),
		Priority: int(derefInt64(row.Priority)),
		Weight:   int(derefUint(row.Weight)),
		Status:   row.Status,
		Overrides: Overrides{
			Param:  anyMapFromJSON(row.ParamOverride),
			Header: strMapFromJSON(row.HeaderOverride),
		},
		ChannelInfo: ChannelInfo{
			MultiKey:         row.ChannelInfo.IsMultiKey || len(keys) > 1,
			KeySelectionMode: modeFromStore(row.ChannelInfo.MultiKeyMode),
			ContextLength:    ex.ContextLength,
			MaxOutputTokens:  ex.MaxOutputTokens,
		},
	}
	// ⚠ 这里**不能**调 normalizeChannel：那个函数会把 ch.Group 当成"一个分组"并进
	// Groups，而 SQL 里 group 列是**逗号分隔的 CSV**（A1 的冻结契约）⇒ 会多出一个
	// "a,b" 形状的假分组（实测：Groups 读回来变成 [vip svip "vip,svip"]）。
	// 读路径只信 row.GroupsOf()（它就是按 CSV 拆的），并保证至少一个分组。
	ch.Groups = effectiveGroupsOf(row.GroupsOf(), "")
	ch.Group = ch.Groups[0]
	if ch.ModelMap == nil {
		ch.ModelMap = map[string]string{}
	}
	return ch
}

// ── channelstore.Channel → 行（写路径）─────────────────────────────────────

// applyChannelToRow 把 channelstore 的**自有字段**写进一行，其余列原样保留。
//
// # 为什么是"改一行"而不是"造一行"
//
// 冻结契约里的 Channel 比 channelstore.Channel 宽得多（hyxapi 的 30 多个字段：
// Other/Setting/Remark/AutoBan/StatusCodeMapping…）。若从 channelstore.Channel
// 造一整行，那些字段会被**空值覆盖**——而它们由别的路径维护，表现是"改一次渠道，
// 上游配置静默丢一半"。所以写路径一律先取原行、只覆盖本层拥有的字段。
func applyChannelToRow(row *store.Channel, ch *Channel) {
	if row == nil || ch == nil {
		return
	}
	row.ExternalID = ch.ID
	row.Name = strings.TrimSpace(ch.Name)
	row.Type = ch.Type
	row.BaseURL = stringPtr(strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/"))
	row.Models = strings.Join(normalizeModels(ch.Models), ",")
	row.ModelMapping = jsonPtr(ch.ModelMap)
	// ⚠ Group（列，CSV）与 Groups（gorm:"-"，内存态）**必须一起写**。
	// store.UpdateChannel 重建能力行时调 ch.GroupsOf()，而它优先返回内存态 Groups
	// （len(Groups) > 0 即用）⇒ 只改列不改内存态时，能力行会按**旧分组**重建：
	// 实测形态是"把渠道移出 vip 之后，vip 组里还留着它的模型"（幽灵能力，选路仍会选中它）。
	groups := ch.EffectiveGroups()
	row.Group = strings.Join(groups, ",")
	row.Groups = groups
	row.Priority = int64Ptr(int64(ch.Priority))
	row.Weight = uintPtr(uint(ch.Weight))
	row.Status = ch.Status
	row.ParamOverride = jsonPtrAny(ch.Overrides.Param)
	row.HeaderOverride = jsonPtrStr(ch.Overrides.Header)
	applyKeysToRow(row, ch.Keys)
	row.ChannelInfo.MultiKeyMode = modeToStore(ch.ChannelInfo.KeySelectionMode)
	extrasToRow(row, channelExtras{
		ContextLength:   ch.ChannelInfo.ContextLength,
		MaxOutputTokens: ch.ChannelInfo.MaxOutputTokens,
	})
}

// ── 小工具 ────────────────────────────────────────────────────────────────

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefUint(p *uint) uint {
	if p == nil {
		return 0
	}
	return *p
}

func stringPtr(s string) *string { return &s }

func int64Ptr(v int64) *int64 { return &v }

func uintPtr(v uint) *uint { return &v }

// splitCSV 逗号分隔 → 非空去重保序切片（与 store 的 ability CSV 同一口径）。
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, raw := range parts {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func normalizeModels(models []string) []string { return splitCSV(strings.Join(models, ",")) }

// strMapFromJSON 解析 JSON 对象为 map[string]string；坏值/空值一律回空 map（不报错）。
//
// 为什么不报错：这些列是**历史脏值风险最高**的地方（手改库、旧版本写入），
// 为一段坏 JSON 让整条渠道读不出来，表现是"渠道从列表里消失了"——比显示空映射糟得多。
func strMapFromJSON(p *string) map[string]string {
	out := map[string]string{}
	if p == nil || strings.TrimSpace(*p) == "" {
		return out
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(*p), &m); err != nil {
		return out
	}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func anyMapFromJSON(p *string) map[string]any {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*p), &m); err != nil {
		return nil
	}
	return m
}

func jsonPtr(m map[string]string) *string {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return stringPtr(string(b))
}

func jsonPtrAny(m map[string]any) *string {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return stringPtr(string(b))
}

func jsonPtrStr(m map[string]string) *string { return jsonPtr(m) }
