// channel_test.go —— A1（task-48）渠道仓储的门禁。
//
// # 它守的是哪几类**静默失败**
//
//  1. **只写 channels 不写 abilities** ⇒ 渠道存在但永远选不到（没有编译/启动错误）。
//     正例（CreateChannel 之后能力行数 = models × groups）与反例（绕开写路径直插
//     channels 行 ⇒ 选路查询必须返回空）一起钉。
//  2. **多 Key 状态挂在越界下标上** ⇒ 后续按它索引 key 列表会 panic/500。
//  3. **列表/详情带出明文 key** ⇒ 凭据泄露，而且常是"顺手"造成的。
//  4. **分页过滤口径错**（把 Status 的 0 当"不过滤"）⇒ 查"已禁用"变成查全部。
//  5. **旧列 models_csv 不回填** ⇒ 老部署的模型声明静默丢失（同 1 的表现）。
package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func strPtr(s string) *string { return &s }
func i64Ptr(v int64) *int64   { return &v }
func uPtr(v uint) *uint       { return &v }

// newTestChannel 造一条未落库的渠道（指针字段都填好，避免 nil 解引用）。
func newTestChannel(name, models, group string) *Channel {
	return &Channel{
		Name:     name,
		BaseURL:  strPtr("https://" + name + ".example/v1"),
		Key:      "sk-" + name,
		Models:   models,
		Group:    group,
		Status:   ChannelStatusEnabled,
		Priority: i64Ptr(0),
		Weight:   uPtr(0),
	}
}

// TestChannelCreateRebuildsAbilities 正例：能力行数 = models（去重去空）× groups。
func TestChannelCreateRebuildsAbilities(t *testing.T) {
	openTemp(t)
	// 模型列刻意混入重复与空白项：展开必须先去重去空，否则同 (group,model) 撞唯一键。
	ch := newTestChannel("cart", "m1, m2, m1, ,", "default, vip")
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if id == 0 {
		t.Fatal("CreateChannel 没有返回自增 ID")
	}
	rows, err := AbilityRowsFor(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("能力行数 = %d，期望 4（2 个模型 × 2 个分组）", len(rows))
	}
	for _, r := range rows {
		if !r.Enabled {
			t.Fatalf("启用渠道的能力行必须 enabled=true: %+v", r)
		}
		if r.ChannelID != id {
			t.Fatalf("能力行 channel_id = %d，期望 %d", r.ChannelID, id)
		}
	}
	got, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 2 {
		t.Fatalf("Groups 计算字段 = %v，期望 2 个", got.Groups)
	}
}

// TestChannelWithoutAbilityRowsIsUnroutable 反例：**只写 channels ⇒ 选不到**。
//
// 这是任务书点名的失败形态：渠道行看起来完全正常，但路由永远选不到它，
// 而且没有任何编译/启动/运行错误。
func TestChannelWithoutAbilityRowsIsUnroutable(t *testing.T) {
	openTemp(t)
	ch := newTestChannel("lonely", "m1", "default")
	// 故意绕开 CreateChannel：只插渠道行（模拟"忘了维护能力行"的实现）。
	if err := DB.Create(ch).Error; err != nil {
		t.Fatalf("直插 channels 失败: %v", err)
	}
	idx, err := AbilityRouteIndex(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if idx {
		t.Fatal("只插了 channels 行却已有路由索引 —— 反例前提不成立")
	}
	cands, err := AbilityCandidates(AbilityDefaultGroup, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("没有能力行却选到了渠道: %+v", cands)
	}
	// 补建之后必须立刻选得到 —— 证明"选不到"的原因就是缺能力行，而不是别的。
	if err := AddAbilities(nil, ch, ch.GroupsOf()...); err != nil {
		t.Fatalf("补建能力行失败: %v", err)
	}
	cands, err = AbilityCandidates(AbilityDefaultGroup, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ChannelID != ch.ID {
		t.Fatalf("补建后应选到 channel %d，得到 %+v", ch.ID, cands)
	}
}

// TestChannelUpdateReplacesAbilities 更新：模型变少时旧能力行必须消失（先删后插）。
func TestChannelUpdateReplacesAbilities(t *testing.T) {
	openTemp(t)
	ch := newTestChannel("upd", "m1, m2", "default")
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := AbilityRowsFor(id); len(rows) != 2 {
		t.Fatalf("建后应 2 行，得到 %d", len(rows))
	}
	got, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	got.Models = "m3"
	if err := UpdateChannel(got); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	rows, err := AbilityRowsFor(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Model != "m3" {
		t.Fatalf("更新后应只剩 m3 一行，得到 %+v", rows)
	}
	cands, err := AbilityCandidates(AbilityDefaultGroup, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("旧模型 m1 仍能被选到: %+v", cands)
	}
}

// TestChannelDeleteRemovesAbilities 删除：渠道行与能力行一起消失。
func TestChannelDeleteRemovesAbilities(t *testing.T) {
	openTemp(t)
	ch := newTestChannel("del", "m1, m2", "default")
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteChannel(id); err != nil {
		t.Fatalf("DeleteChannel: %v", err)
	}
	if rows, _ := AbilityRowsFor(id); len(rows) != 0 {
		t.Fatalf("删渠道后能力行应为 0，得到 %d", len(rows))
	}
	if _, err := GetChannel(id); err == nil {
		t.Fatal("删掉的渠道还能读回来")
	}
	if err := DeleteChannel(id); err == nil {
		t.Fatal("重复删除应当报 ErrChannelNotFound")
	}
}

// TestUpdateChannelTruncatesMultiKeyState D2 ①：改 key 列表必须截断越界下标。
func TestUpdateChannelTruncatesMultiKeyState(t *testing.T) {
	openTemp(t)
	ch := newTestChannel("multi", "m1", "default")
	ch.ChannelInfo.IsMultiKey = true
	ch.ChannelInfo.MultiKeyMode = MultiKeyModePolling
	ch.SetKeys([]string{"sk-1", "sk-2", "sk-3"}, true)
	ch.ChannelInfo.MultiKeyStatusList = map[int]int{
		0: ChannelStatusEnabled, 1: ChannelStatusManuallyDisabled, 2: ChannelStatusEnabled,
	}
	ch.ChannelInfo.MultiKeyDisabledReason = map[int]string{1: "手动停用", 2: "上游 401"}
	ch.ChannelInfo.MultiKeyDisabledTime = map[int]int64{1: 111, 2: 222}
	ch.ChannelInfo.MultiKeyPollingIndex = 2
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChannelInfo.MultiKeySize != 3 {
		t.Fatalf("MultiKeySize = %d，期望 3", got.ChannelInfo.MultiKeySize)
	}
	// ⚠ 读 key 必须走显式入口：GetChannel 是 Omit("key") 的（D1），Key 列读回来是空的。
	keys3, err := GetChannelKeys(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys3) != 3 {
		t.Fatalf("Key 列应解析出 3 把，得到 %v", keys3)
	}
	// 删到剩 2 把：下标 2 全部越界，必须被清掉（否则后续索引会 panic）。
	got.SetKeys([]string{"sk-1", "sk-2"}, true)
	if err := UpdateChannel(got); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	after, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.ChannelInfo.MultiKeySize != 2 {
		t.Fatalf("截断后 MultiKeySize = %d，期望 2", after.ChannelInfo.MultiKeySize)
	}
	if _, exists := after.ChannelInfo.MultiKeyStatusList[2]; exists {
		t.Fatal("越界的 MultiKeyStatusList[2] 没被截断")
	}
	if _, exists := after.ChannelInfo.MultiKeyDisabledReason[2]; exists {
		t.Fatal("越界的 MultiKeyDisabledReason[2] 没被截断")
	}
	if _, exists := after.ChannelInfo.MultiKeyDisabledTime[2]; exists {
		t.Fatal("越界的 MultiKeyDisabledTime[2] 没被截断")
	}
	if after.ChannelInfo.MultiKeyPollingIndex != 0 {
		t.Fatalf("越界的轮询游标应归零，得到 %d", after.ChannelInfo.MultiKeyPollingIndex)
	}
	// 没带 ChannelInfo 的整行更新（列表视图构造的）不能把多 Key 状态清空。
	partial, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	partial.ChannelInfo = ChannelInfo{}
	partial.Name = "multi-renamed"
	if err := UpdateChannel(partial); err != nil {
		t.Fatalf("UpdateChannel(partial): %v", err)
	}
	keep, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if !keep.ChannelInfo.IsMultiKey || keep.ChannelInfo.MultiKeySize != 2 {
		t.Fatalf("零值 ChannelInfo 的更新把多 Key 状态清空了: %+v", keep.ChannelInfo)
	}
}

// TestChannelReadPathsOmitPlaintextKey D1：列表/详情/分页/搜索都不许带明文 key。
func TestChannelReadPathsOmitPlaintextKey(t *testing.T) {
	openTemp(t)
	ch := newTestChannel("secretive", "m1", "default")
	ch.Key = "sk-plaintext-do-not-leak"
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatal(err)
	}
	one, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if one.Key != "" {
		t.Fatalf("GetChannel 带出了明文 key: %q", one.Key)
	}
	list, err := ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Key != "" {
			t.Fatalf("ListChannels 带出了明文 key（id=%d）", c.ID)
		}
	}
	page, err := ListChannelPage(ChannelQuery{Status: ChannelStatusAny, Type: ChannelTypeAny})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Items {
		if c.Key != "" {
			t.Fatalf("ListChannelPage 带出了明文 key（id=%d）", c.ID)
		}
	}
	found, err := SearchChannelsByKeyword(ChannelQuery{Keyword: "secretive", Status: ChannelStatusAny, Type: ChannelTypeAny})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range found {
		if c.Key != "" {
			t.Fatalf("SearchChannelsByKeyword 带出了明文 key（id=%d）", c.ID)
		}
	}
	// 明文只能从这两个显式入口拿。
	key, err := GetChannelKey(id)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-plaintext-do-not-leak" {
		t.Fatalf("GetChannelKey = %q，期望明文原值", key)
	}
	keys, err := GetChannelKeys(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "sk-plaintext-do-not-leak" {
		t.Fatalf("GetChannelKeys = %v", keys)
	}
}

// TestListChannelPageFiltersAndTypeCounts 分页/状态口径/type_counts（照 hyxapi）。
func TestListChannelPageFiltersAndTypeCounts(t *testing.T) {
	openTemp(t)
	makeCh := func(name string, typ, status int) *Channel {
		c := newTestChannel(name, "m1", "default")
		c.Type = typ
		c.Status = status
		return c
	}
	for _, c := range []*Channel{
		makeCh("a", 1, ChannelStatusEnabled),
		makeCh("b", 1, ChannelStatusManuallyDisabled),
		makeCh("c", 2, ChannelStatusEnabled),
	} {
		if _, err := CreateChannel(c); err != nil {
			t.Fatal(err)
		}
	}
	all, err := ListChannelPage(ChannelQuery{Status: ChannelStatusAny, Type: ChannelTypeAny, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 3 {
		t.Fatalf("total = %d，期望 3", all.Total)
	}
	if len(all.Items) != 2 || all.PageSize != 2 {
		t.Fatalf("分页没生效: items=%d pageSize=%d", len(all.Items), all.PageSize)
	}
	if all.TypeCounts[1] != 2 || all.TypeCounts[2] != 1 {
		t.Fatalf("type_counts = %v，期望 {1:2, 2:1}", all.TypeCounts)
	}
	// page_size 必须被夹到上限，防"page_size=100000"打库。
	huge, err := ListChannelPage(ChannelQuery{Status: ChannelStatusAny, Type: ChannelTypeAny, PageSize: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if huge.PageSize != ChannelPageMaxSize {
		t.Fatalf("page_size 上限 = %d，期望 %d", huge.PageSize, ChannelPageMaxSize)
	}
	// Status = 0 是"已禁用"（status != 1），不是"不过滤"。
	disabled, err := ListChannelPage(ChannelQuery{Status: 0, Type: ChannelTypeAny})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Total != 1 {
		t.Fatalf("已禁用 total = %d，期望 1（0 必须当已禁用而不是不过滤）", disabled.Total)
	}
	// Type 过滤；type_counts 不受 type 过滤影响。
	byType, err := ListChannelPage(ChannelQuery{Status: ChannelStatusAny, Type: 2})
	if err != nil {
		t.Fatal(err)
	}
	if byType.Total != 1 {
		t.Fatalf("type=2 的 total = %d，期望 1", byType.Total)
	}
	if byType.TypeCounts[1] != 2 {
		t.Fatalf("type_counts 不该被 type 过滤掉: %v", byType.TypeCounts)
	}
}

// TestSetChannelStatusFlipsAbilityEnabled 状态翻转必须同步 abilities.enabled。
func TestSetChannelStatusFlipsAbilityEnabled(t *testing.T) {
	openTemp(t)
	ch := newTestChannel("toggle", "m1", "default")
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := SetChannelStatus(id, ChannelStatusManuallyDisabled, "测试停用")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("首次停用应当 changed=true")
	}
	rows, err := AbilityRowsFor(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("停用不该删能力行（要留着便于恢复），得到 %d 行", len(rows))
	}
	if rows[0].Enabled {
		t.Fatal("渠道停用后能力行仍 enabled=true —— 路由会把请求发给停用的渠道")
	}
	if cands, _ := AbilityCandidates(AbilityDefaultGroup, "m1"); len(cands) != 0 {
		t.Fatalf("停用渠道不该被选到: %+v", cands)
	}
	if changed, err = SetChannelStatus(id, ChannelStatusEnabled, "恢复"); err != nil || !changed {
		t.Fatalf("恢复失败: changed=%v err=%v", changed, err)
	}
	if rows, _ := AbilityRowsFor(id); len(rows) != 1 || !rows[0].Enabled {
		t.Fatalf("恢复后能力行应 enabled=true: %+v", rows)
	}
	if changed, err = SetChannelStatus(id, ChannelStatusEnabled, "重复恢复"); err != nil || changed {
		t.Fatalf("同状态重复设置应当 changed=false，得到 %v/%v", changed, err)
	}
}

// TestCreateChannelRejectsBadInput 参数校验（缺 name / 非法 status）。
func TestCreateChannelRejectsBadInput(t *testing.T) {
	openTemp(t)
	bad := newTestChannel("", "m1", "default")
	if _, err := CreateChannel(bad); err == nil {
		t.Fatal("缺 name 应当被拒（name 是身份键的一半）")
	}
	badStatus := newTestChannel("bad", "m1", "default")
	badStatus.Status = 99
	if _, err := CreateChannel(badStatus); err == nil {
		t.Fatal("非法 status 应当被拒")
	}
}

// TestCopyChannelClonesKeyAndAbilities 复制：能力行跟着建、明文 key 一起复制。
func TestCopyChannelClonesKeyAndAbilities(t *testing.T) {
	openTemp(t)
	ch := newTestChannel("origin", "m1, m2", "default")
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateChannelBalance(id, 12.5); err != nil {
		t.Fatalf("UpdateChannelBalance: %v", err)
	}
	newID, err := CopyChannel(id, "_复制", true)
	if err != nil {
		t.Fatalf("CopyChannel: %v", err)
	}
	if newID == id || newID == 0 {
		t.Fatalf("复制应产生新 id，得到 %d（原名 %d）", newID, id)
	}
	clone, err := GetChannel(newID)
	if err != nil {
		t.Fatal(err)
	}
	if clone.Name != "origin_复制" {
		t.Fatalf("复制后的名字 = %q", clone.Name)
	}
	if clone.Balance != 0 || clone.UsedQuota != 0 {
		t.Fatalf("reset_balance=true 时余额/用量应归零: %v/%v", clone.Balance, clone.UsedQuota)
	}
	key, err := GetChannelKey(newID)
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-origin" {
		t.Fatalf("复制应带上明文 key，得到 %q", key)
	}
	if rows, _ := AbilityRowsFor(newID); len(rows) != 2 {
		t.Fatalf("复制出来的渠道能力行应 2 行，得到 %d", len(rows))
	}
}

// TestBackfillLegacyChannelModels 旧列回填：models_csv 有值而 models 为空时必须搬。
func TestBackfillLegacyChannelModels(t *testing.T) {
	openTemp(t)
	// 造一个"老库形态"的行：手工加旧列并写值，新列留空。
	if err := DB.Exec("ALTER TABLE channels ADD COLUMN models_csv text NOT NULL DEFAULT ''").Error; err != nil {
		t.Fatalf("加旧列失败（迁移测试自身前提）: %v", err)
	}
	ch := newTestChannel("legacy", "", "default")
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := DB.Exec("UPDATE channels SET models_csv = ? WHERE id = ?", "m-legacy-1,m-legacy-2", id).Error; err != nil {
		t.Fatal(err)
	}
	n, err := BackfillLegacyChannelColumns()
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("回填影响行数 = %d，期望 1", n)
	}
	got, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Models != "m-legacy-1,m-legacy-2" {
		t.Fatalf("回填后的 models = %q", got.Models)
	}
	// 幂等：再跑一次影响 0 行。
	again, err := BackfillLegacyChannelColumns()
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("回填不幂等：第二次影响 %d 行", again)
	}
}

// TestChannelExternalIDRoundTrip 外部 id 往返：存进去 ⇒ 按它查得到，且三处列表都带着它。
func TestChannelExternalIDRoundTrip(t *testing.T) {
	openTemp(t)
	ch := newTestChannel("ext", "m1", "default")
	ch.ExternalID = "ch-9f3a1b2c"
	id, err := CreateChannel(ch)
	if err != nil {
		t.Fatal(err)
	}
	found, ok, err := GetChannelByExternalID("ch-9f3a1b2c")
	if err != nil {
		t.Fatalf("GetChannelByExternalID: %v", err)
	}
	if !ok || found == nil {
		t.Fatal("按外部 id 没找到（ok=false）")
	}
	if found.ID != id {
		t.Fatalf("外部 id 命中 id=%d，期望 %d", found.ID, id)
	}
	// 这一路也不许带出明文 key（与其它读路径同口径）。
	if found.Key != "" {
		t.Fatalf("GetChannelByExternalID 带出了明文 key: %q", found.Key)
	}
	one, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if one.ExternalID != "ch-9f3a1b2c" {
		t.Fatalf("GetChannel 的 ExternalID = %q", one.ExternalID)
	}
	list, err := ListChannels()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ExternalID != "ch-9f3a1b2c" {
		t.Fatalf("ListChannels 没带上 ExternalID: %+v", list)
	}
	page, err := ListChannelPage(ChannelQuery{Status: ChannelStatusAny, Type: ChannelTypeAny})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ExternalID != "ch-9f3a1b2c" {
		t.Fatalf("ListChannelPage 没带上 ExternalID: %+v", page.Items)
	}
	searched, err := SearchChannelsByKeyword(ChannelQuery{Keyword: "ext", Status: ChannelStatusAny, Type: ChannelTypeAny})
	if err != nil {
		t.Fatal(err)
	}
	if len(searched) != 1 || searched[0].ExternalID != "ch-9f3a1b2c" {
		t.Fatalf("SearchChannelsByKeyword 没带上 ExternalID: %+v", searched)
	}
}

// TestChannelExternalIDUnique 非空外部 id 由**库**保证唯一（部分唯一索引）。
func TestChannelExternalIDUnique(t *testing.T) {
	openTemp(t)
	first := newTestChannel("uniq-a", "m1", "default")
	first.ExternalID = "ch-dup"
	if _, err := CreateChannel(first); err != nil {
		t.Fatalf("第一条应当成功: %v", err)
	}
	second := newTestChannel("uniq-b", "m1", "default")
	second.ExternalID = "ch-dup"
	if _, err := CreateChannel(second); err == nil {
		t.Fatal("同一个非空 external_id 插第二行应当被库拒绝")
	} else if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("期望 gorm.ErrDuplicatedKey，得到: %v", err)
	}
}

// TestChannelExternalIDEmptySemantics 空串语义（本列最容易做错的地方）。
//
// 钉两件事：① 多条空串**必须**共存（seed 13 行全是空串）；② 因此 gorm tag
// **不能**写 uniqueIndex —— SQLite 的普通 UNIQUE 把空串当重复，第二行就炸。
func TestChannelExternalIDEmptySemantics(t *testing.T) {
	openTemp(t)
	for _, name := range []string{"empty-a", "empty-b", "empty-c"} {
		if _, err := CreateChannel(newTestChannel(name, "m1", "default")); err != nil {
			t.Fatalf("多条空 external_id 必须共存，%s 插入失败: %v", name, err)
		}
	}
	var n int64
	if err := DB.Model(&Channel{}).Where("external_id = ?", "").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("空 external_id 行数 = %d，期望 3", n)
	}
	ch, ok, err := GetChannelByExternalID("")
	if err != nil || ok || ch != nil {
		t.Fatalf("空串应当 (nil,false,nil)，得到 (%v,%v,%v)", ch, ok, err)
	}
	// 反证：普通 UNIQUE 索引（= 直接把 uniqueIndex 写进 tag 的等价物）会拒绝第二行空串。
	if err := DB.Exec("CREATE TABLE extid_trap(id INTEGER PRIMARY KEY, ext TEXT NOT NULL DEFAULT '' UNIQUE)").Error; err != nil {
		t.Fatal(err)
	}
	if err := DB.Exec("INSERT INTO extid_trap(ext) VALUES('')").Error; err != nil {
		t.Fatalf("trap 表第一行空串应当成功: %v", err)
	}
	if err := DB.Exec("INSERT INTO extid_trap(ext) VALUES('')").Error; err == nil {
		t.Fatal("普通 UNIQUE 索引居然允许第二行空串 —— 那本列的取舍就不再需要部分索引了，请重新评估")
	}
}

// TestChannelExternalIDBackfillAndClear 回填与清空两条路径都要能表达。
func TestChannelExternalIDBackfillAndClear(t *testing.T) {
	openTemp(t)
	id, err := CreateChannel(newTestChannel("backfill", "m1", "default"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	got.ExternalID = "ch-backfilled"
	if err := UpdateChannel(got); err != nil {
		t.Fatalf("UpdateChannel 回填 external_id: %v", err)
	}
	if _, ok, err := GetChannelByExternalID("ch-backfilled"); err != nil || !ok {
		t.Fatalf("回填后按外部 id 找不到: ok=%v err=%v", ok, err)
	}
	// 保护：整行更新里 ExternalID 为空 ⇒ 保留原值（否则前端寻址会把渠道"变没"）。
	partial, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	partial.ExternalID = ""
	partial.Name = "backfill-renamed"
	if err := UpdateChannel(partial); err != nil {
		t.Fatal(err)
	}
	keep, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if keep.ExternalID != "ch-backfilled" {
		t.Fatalf("空 ExternalID 的整行更新把外部 id 抹掉了: %q", keep.ExternalID)
	}
	// 显式清空：走 SetChannelExternalID(id, "")。
	changed, err := SetChannelExternalID(id, "")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("清空应当 changed=true")
	}
	cleared, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ExternalID != "" {
		t.Fatalf("显式清空后仍为 %q", cleared.ExternalID)
	}
	if changed, err = SetChannelExternalID(id, ""); err != nil || changed {
		t.Fatalf("重复清空应当 changed=false，得到 %v/%v", changed, err)
	}
}

// TestCopyChannelDoesNotCopyExternalID 复制不继承外部 id（唯一索引 + 寻址歧义）。
func TestCopyChannelDoesNotCopyExternalID(t *testing.T) {
	openTemp(t)
	src := newTestChannel("extid-src", "m1", "default")
	src.ExternalID = "ch-original"
	id, err := CreateChannel(src)
	if err != nil {
		t.Fatal(err)
	}
	newID, err := CopyChannel(id, "_复制", true)
	if err != nil {
		t.Fatalf("复制带外部 id 的渠道不该撞唯一键: %v", err)
	}
	clone, err := GetChannel(newID)
	if err != nil {
		t.Fatal(err)
	}
	if clone.ExternalID != "" {
		t.Fatalf("复制出来的渠道不该继承 external_id，得到 %q", clone.ExternalID)
	}
}

// TestSetChannelStatusAllowsAutoDisabled 状态 3（自动禁用）也允许**人工**置入。
//
// 放开理由见 SetChannelStatus 的注释：admin 端点与 channelstore 的冻结签名都允许 3，
// 存储层单独拒绝会把"上层合法、下层报错"变成只在运行时才暴露的分裂。
func TestSetChannelStatusAllowsAutoDisabled(t *testing.T) {
	openTemp(t)
	id, err := CreateChannel(newTestChannel("auto-off", "m1", "default"))
	if err != nil {
		t.Fatal(err)
	}
	if !IsValidChannelStatus(ChannelStatusAutoDisabled) {
		t.Fatal("IsValidChannelStatus(3) 必须为 true（3 是合法状态）")
	}
	changed, err := SetChannelStatus(id, ChannelStatusAutoDisabled, "人工标记自动停用（上游被封禁）")
	if err != nil {
		t.Fatalf("人工置入状态 3 应当成功: %v", err)
	}
	if !changed {
		t.Fatal("首次置入 3 应当 changed=true")
	}
	got, err := GetChannel(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ChannelStatusAutoDisabled {
		t.Fatalf("读回状态 = %d，期望 3", got.Status)
	}
	rows, err := AbilityRowsFor(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("状态 3 不该删能力行（保留绑定便于恢复），得到 %d 行", len(rows))
	}
	if rows[0].Enabled {
		t.Fatal("status != 1 时能力行必须 enabled=false（沿用既有状态机语义）")
	}
	if cands, _ := AbilityCandidates(AbilityDefaultGroup, "m1"); len(cands) != 0 {
		t.Fatalf("自动禁用（人工置入）的渠道不该被选到: %+v", cands)
	}
	// 3 -> 1 要能恢复（自动停用是可逆的运维姿态，不是终点）。
	changed, err = SetChannelStatus(id, ChannelStatusEnabled, "上游恢复")
	if err != nil || !changed {
		t.Fatalf("3 → 1 恢复失败: changed=%v err=%v", changed, err)
	}
	if rows, _ := AbilityRowsFor(id); len(rows) != 1 || !rows[0].Enabled {
		t.Fatalf("恢复后能力行应 enabled=true: %+v", rows)
	}
	if cands, _ := AbilityCandidates(AbilityDefaultGroup, "m1"); len(cands) != 1 {
		t.Fatalf("恢复后应能被选到: %+v", cands)
	}
	// 非法状态仍然要被挡住（放开 3 不等于放弃校验）。
	if _, err := SetChannelStatus(id, 99, "非法"); err == nil {
		t.Fatal("status=99 应当被拒")
	}
}
