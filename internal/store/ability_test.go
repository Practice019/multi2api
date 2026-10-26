// ability_test.go abilities 维护与选路查询的门禁。
//
// # 这组为什么是门禁而不是覆盖率
//
// abilities 是**路由索引**：它错了的表现不是报错，而是「请求走到了不该走的渠道」或
// 「明明有渠道却报 no_channel_for_model」。两类都难查，所以每条不变量都写成
// 「坏掉就一定红」的形式，其中最关键的是反例守卫
// TestAbilityRoutingIgnoresChannelsWithoutAbilityRows：
// 它手工往 channels 表插一行而**不**建能力行，然后断言选路查询返回空 ——
// 谁哪天「顺手」让写 channels 自动补能力行，这条会红（那时必须同时删掉这条测试，
// 而不是把断言改宽）。
//
// ⚠ 全局约束：store.DB 是包级句柄，本文件所有用例都不许 t.Parallel()。
package store

import (
	"strconv"
	"testing"
)

// abilityTestChannel 直接插一行渠道（绕开 seed），返回带着自增 ID 的行。
//
// 注意字段形状：新版 Channel 的 BaseURL/Priority/Weight 都是**指针**（可空语义，
// 对齐参考项目），Models/Group 是逗号分隔的字符串列。
func abilityTestChannel(t *testing.T, name, models, group string, priority int64, weight uint) *Channel {
	t.Helper()
	base := "https://" + name + ".example"
	prio := priority
	we := weight
	ch := &Channel{
		Name:     name,
		BaseURL:  &base,
		Models:   models,
		Group:    group,
		Status:   ChannelStatusEnabled,
		Priority: &prio,
		Weight:   &we,
	}
	if err := DB.Create(ch).Error; err != nil {
		t.Fatalf("插渠道 %s 失败: %v", name, err)
	}
	if ch.ID == 0 {
		t.Fatalf("插渠道 %s 后没有拿到自增 ID", name)
	}
	return ch
}

// TestAbilityAddIsCartesianProductDedupAndIdempotent
//
// 守三条：① 行数 = len(models) × len(groups)；② 重复 Add 不增行（OnConflict 幂等）；
// ③ 分组缺省三级回落（入参 → ch.Group → default）。
func TestAbilityAddIsCartesianProductDedupAndIdempotent(t *testing.T) {
	openTemp(t)
	// 模型与分组两列都刻意混入空白项与重复项：m1 出现两次、末尾一个空项。
	ch := abilityTestChannel(t, "cart", "m1, m2, m1, ,", "default, vip, default", 0, 0)

	if err := AddAbilities(nil, ch); err != nil {
		t.Fatalf("AddAbilities 失败: %v", err)
	}
	rows, err := AbilityRowsFor(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 2 个模型 × 2 个分组 = 4 行（重复项与空项在展开阶段就被去掉）
	if len(rows) != 4 {
		t.Fatalf("笛卡尔积行数 = %d，期望 4（models 去重后 2 × groups 去重后 2）", len(rows))
	}

	// 幂等：再 Add 一次不增行
	if err := AddAbilities(nil, ch); err != nil {
		t.Fatalf("第二次 AddAbilities 失败: %v", err)
	}
	rows2, err := AbilityRowsFor(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != len(rows) {
		t.Fatalf("重复 Add 之后行数从 %d 变成 %d —— OnConflict DoNothing 没生效", len(rows), len(rows2))
	}

	// 渠道没写分组 ⇒ 落到唯一默认分组
	ch2 := abilityTestChannel(t, "cart-default", "m9", "", 0, 0)
	if err := AddAbilities(nil, ch2); err != nil {
		t.Fatal(err)
	}
	r2, err := AbilityRowsFor(ch2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2) != 1 || r2[0].Group != AbilityDefaultGroup {
		t.Fatalf("默认分组应为 %q，得到 %+v", AbilityDefaultGroup, r2)
	}

	// 显式入参覆盖渠道行上的分组
	if err := AddAbilities(nil, ch2, "vip"); err != nil {
		t.Fatal(err)
	}
	r3, err := AbilityRowsFor(ch2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r3) != 2 {
		t.Fatalf("默认分组 + 覆盖的 vip 应共 2 行，得到 %d", len(r3))
	}
}

// TestAbilityAddBatchesOver50 一次展开超过 50 行（证明分批插入真的能跑通）。
func TestAbilityAddBatchesOver50(t *testing.T) {
	openTemp(t)
	models := ""
	for i := 0; i < 60; i++ {
		if i > 0 {
			models += ","
		}
		models += "bm" + strconv.Itoa(i)
	}
	ch := abilityTestChannel(t, "batch", models, "default", 0, 0)
	if err := AddAbilities(nil, ch); err != nil {
		t.Fatalf("分批插入失败: %v", err)
	}
	rows, err := AbilityRowsFor(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 60 {
		t.Fatalf("60 个模型应产生 60 行（跨 2 批），得到 %d", len(rows))
	}
}

// TestAbilityUpdateIsDeleteThenInsert 先删后插：模型变少时旧行必须消失。
func TestAbilityUpdateIsDeleteThenInsert(t *testing.T) {
	openTemp(t)
	ch := abilityTestChannel(t, "upd", "m1, m2", "default", 0, 0)
	if err := UpdateAbilities(nil, ch); err != nil {
		t.Fatalf("首次 UpdateAbilities 失败: %v", err)
	}
	if rows, _ := AbilityRowsFor(ch.ID); len(rows) != 2 {
		t.Fatalf("首次应有 2 行，得到 %d", len(rows))
	}

	ch.Models = "m3"
	if err := UpdateAbilities(nil, ch); err != nil {
		t.Fatalf("第二次 UpdateAbilities 失败: %v", err)
	}
	rows, err := AbilityRowsFor(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("改为 1 个模型后应剩 1 行（先删后插），得到 %d", len(rows))
	}
	if rows[0].Model != "m3" {
		t.Fatalf("剩下的行应该是 m3，得到 %q", rows[0].Model)
	}
	// 旧模型必须彻底消失：路由查询不能还能找到 m1
	cands, err := AbilityCandidates(AbilityDefaultGroup, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("先删后插没删干净：m1 仍有 %d 条候选", len(cands))
	}
}

// TestAbilityStatusSyncOnlyFlipsEnabled 状态同步只改 enabled，不删行。
func TestAbilityStatusSyncOnlyFlipsEnabled(t *testing.T) {
	openTemp(t)
	ch := abilityTestChannel(t, "stat", "sm1", "default", 0, 0)
	if err := AddAbilities(nil, ch); err != nil {
		t.Fatal(err)
	}

	if err := UpdateAbilityStatus(ch.ID, false); err != nil {
		t.Fatalf("停用同步失败: %v", err)
	}
	rows, err := AbilityRowsFor(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("状态同步不该删行，现在剩 %d 行", len(rows))
	}
	if rows[0].Enabled {
		t.Fatal("停用后 enabled 仍为 true")
	}
	cands, err := AbilityCandidates(AbilityDefaultGroup, "sm1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("停用渠道不该出现在候选里，得到 %d 条", len(cands))
	}

	if err := UpdateAbilityStatus(ch.ID, true); err != nil {
		t.Fatalf("恢复同步失败: %v", err)
	}
	cands2, err := AbilityCandidates(AbilityDefaultGroup, "sm1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands2) != 1 {
		t.Fatalf("恢复后应重新出现在候选里，得到 %d 条", len(cands2))
	}
}

// TestAbilityRoutingOrdersByPriorityThenWeight 选路顺序 + 返回值。
//
// 三个渠道都服务 (default, rm1)，优先级/权重各不相同；期望顺序在代码里写死。
func TestAbilityRoutingOrdersByPriorityThenWeight(t *testing.T) {
	openTemp(t)
	low := abilityTestChannel(t, "r-low", "rm1", "default", 1, 100) // 优先级最低，权重最大
	mid := abilityTestChannel(t, "r-mid", "rm1", "default", 5, 1)   // 同优先级里权重较小
	high := abilityTestChannel(t, "r-high", "rm1", "default", 5, 9) // 同优先级里权重较大 ⇒ 排第一
	for _, ch := range []*Channel{low, mid, high} {
		if err := AddAbilities(nil, ch); err != nil {
			t.Fatal(err)
		}
	}

	cands, err := AbilityCandidates("default", "rm1")
	if err != nil {
		t.Fatalf("选路查询失败: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("应有 3 条候选，得到 %d", len(cands))
	}
	wantOrder := []int64{high.ID, mid.ID, low.ID}
	for i, want := range wantOrder {
		if cands[i].ChannelID != want {
			t.Fatalf("候选顺序第 %d 位 = %d，期望 %d（priority DESC, weight DESC）", i, cands[i].ChannelID, want)
		}
	}
	// 返回值带着 priority/weight（JOIN channels 现读），供上层做分桶 + 加权随机
	if cands[0].Priority != 5 || cands[0].Weight != 9 {
		t.Fatalf("首条候选应带 priority=5/weight=9，得到 %d/%d", cands[0].Priority, cands[0].Weight)
	}
	if cands[2].Priority != 1 || cands[2].Weight != 100 {
		t.Fatalf("末条候选应带 priority=1/weight=100，得到 %d/%d", cands[2].Priority, cands[2].Weight)
	}
}

// TestAbilityRoutingIgnoresChannelsWithoutAbilityRows ★反例守卫
//
// 手工往 channels 表插一行、**不**调 AddAbilities ⇒ 选路查询必须返回空。
// 这条守的不变量是「渠道行本身不是路由索引」（见 ability.go 文件头第 3 条）。
// 谁把「写 channels 自动补 abilities」加进来，这条就会红 —— 那时正确的做法是
// 删掉这条测试并同步改文件头的不变量，而不是把断言改宽。
func TestAbilityRoutingIgnoresChannelsWithoutAbilityRows(t *testing.T) {
	openTemp(t)
	// ① 只插渠道，不建能力行
	ch := abilityTestChannel(t, "no-idx", "nm1", "default", 9, 9)
	cands, err := AbilityCandidates(AbilityDefaultGroup, "nm1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("★ 反例失败：只有 channels 行、没有能力行时选路查询应返回空，实际 %d 条", len(cands))
	}
	idx, err := AbilityRouteIndex(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if idx {
		t.Fatal("AbilityRouteIndex 应为 false（这条渠道还没有路由索引）")
	}

	// ② 补建能力行之后必须立刻可见 ⇒ 证明①的空结果来自「没维护」，不是查询坏了
	if err := AddAbilities(nil, ch); err != nil {
		t.Fatal(err)
	}
	cands2, err := AbilityCandidates(AbilityDefaultGroup, "nm1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands2) != 1 || cands2[0].ChannelID != ch.ID {
		t.Fatalf("补建后应命中 1 条且 channel_id=%d，得到 %+v", ch.ID, cands2)
	}
	idx2, err := AbilityRouteIndex(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !idx2 {
		t.Fatal("补建后 AbilityRouteIndex 应为 true")
	}
}

// TestAbilityFixAllAbilitiesRebuildsAndIsIdempotent 全表重建：清空后能重建，跑两次结果一致。
func TestAbilityFixAllAbilitiesRebuildsAndIsIdempotent(t *testing.T) {
	openTemp(t)
	a := abilityTestChannel(t, "fix-a", "fm1, fm2", "default,vip", 3, 2)
	b := abilityTestChannel(t, "fix-b", "fm1", "default,vip", 1, 1)
	for _, ch := range []*Channel{a, b} {
		if err := AddAbilities(nil, ch); err != nil {
			t.Fatal(err)
		}
	}
	want := 2*2 + 1*2 // a: 2 模型 × 2 分组；b: 1 模型 × 2 分组
	if got := abilityRowTotal(t); got != want {
		t.Fatalf("重建前总行数 = %d，期望 %d", got, want)
	}

	if _, _, err := FixAllAbilities(); err != nil {
		t.Fatalf("FixAllAbilities 失败: %v", err)
	}
	if got := abilityRowTotal(t); got != want {
		t.Fatalf("重建后总行数 = %d，期望 %d（全表重建应当等价）", got, want)
	}
	// 幂等：再跑一次还是同样的行数
	if _, _, err := FixAllAbilities(); err != nil {
		t.Fatalf("第二次 FixAllAbilities 失败: %v", err)
	}
	if got := abilityRowTotal(t); got != want {
		t.Fatalf("第二次重建后总行数 = %d，期望 %d", got, want)
	}

	// 清空后必须能重建回来（FixAll 是「修复」而不是「补齐」）
	if err := DeleteAllAbilities(); err != nil {
		t.Fatal(err)
	}
	if got := abilityRowTotal(t); got != 0 {
		t.Fatalf("清空后总行数 = %d，期望 0", got)
	}
	if _, _, err := FixAllAbilities(); err != nil {
		t.Fatal(err)
	}
	if got := abilityRowTotal(t); got != want {
		t.Fatalf("从空表重建后总行数 = %d，期望 %d", got, want)
	}
}

// TestAbilityGuardsRejectZeroChannelID channel_id 为 0 时全部拒绝（不许变成「操作全表」）。
//
// 这是本文件里最危险的一类失败：结构体条件会跳过零值字段，条件为空时
// DELETE / UPDATE 会命中所有行 —— 一次静默清空路由索引。
func TestAbilityGuardsRejectZeroChannelID(t *testing.T) {
	openTemp(t)
	keep := abilityTestChannel(t, "guard", "gm1", "default", 0, 0)
	if err := AddAbilities(nil, keep); err != nil {
		t.Fatal(err)
	}

	if err := DeleteAbilities(nil, 0); err == nil {
		t.Fatal("DeleteAbilities(0) 应当报错（否则会删全表）")
	}
	if err := UpdateAbilityStatus(0, false); err == nil {
		t.Fatal("UpdateAbilityStatus(0) 应当报错（否则会改全表）")
	}
	if _, err := AbilityRowsFor(0); err == nil {
		t.Fatal("AbilityRowsFor(0) 应当报错")
	}

	// 关键：上面三次拒绝之后，别的渠道的行必须**原封不动**
	if got := abilityRowTotal(t); got != 1 {
		t.Fatalf("拒绝无效操作后总行数 = %d，期望 1（有行被误删/误改）", got)
	}
	rows, err := AbilityRowsFor(keep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Enabled {
		t.Fatalf("guard 渠道的能力行被改动了: %+v", rows)
	}
}

// abilityRowTotal 数一遍 abilities 全表（跨渠道）。
func abilityRowTotal(t *testing.T) int {
	t.Helper()
	byChannel, err := AbilityCountByChannel()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range byChannel {
		n += c
	}
	return n
}
