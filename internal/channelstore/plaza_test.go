// plaza_test.go 模型广场的判据（U-11 多提供者详情 / 用户要求「分组里的模型全部写进去」）。
//
// # 广场与目录快照是两个集合
//
//	目录快照  按**渠道**算：这条渠道能服务什么
//	广场      按**分组**算：我们**对外承诺**了什么
//
// 差集才是最有用的东西：
//
//	分组里配了、渠道没供给 ⇒ **承诺了一个没人能服务的模型**（客户端会失败）
//
// 所以每行都带 `in_catalog`，且整份结果带 `orphaned`。
package channelstore

import (
	"reflect"
	"strings"
	"testing"

	"workbuddy2api/internal/groupstore"
)

func pgrp(id, name string, models []string, mm map[string][]string) *groupstore.Group {
	return &groupstore.Group{
		ID: id, Name: name, PolicyVersion: 2,
		Models: models, ModelMap: mm,
	}
}

// ★ ① 分组 ModelMap 的键 = 对外模型（有渠道能服务 ⇒ in_catalog）。
func TestPlazaIncludesMappedPublicNames(t *testing.T) {
	groups := []*groupstore.Group{
		pgrp("g-a", "主线", []string{"real-x"}, map[string][]string{"public-x": {"real-x"}}),
	}
	channels := []*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled,
			Models: []string{"real-x"}, ModelMap: map[string]string{"public-x": "real-x"}},
	}
	plaza := BuildModelPlaza(groups, channels)

	if plaza.Total != 1 {
		t.Fatalf("应有 1 个对外模型，实际 %d（%+v）", plaza.Total, plaza.Models)
	}
	m := plaza.Models[0]
	if m.Public != "public-x" {
		t.Fatalf("对外名 = %q，want public-x", m.Public)
	}
	if !m.InCatalog {
		t.Fatalf("★ 有渠道能服务它，in_catalog 应为 true：%+v", m)
	}
	if !reflect.DeepEqual(m.Reals, []string{"real-x"}) {
		t.Fatalf("Reals = %v，want [real-x]", m.Reals)
	}
	if !reflect.DeepEqual(m.Channels, []string{"ch-a"}) {
		t.Fatalf("Channels = %v，want [ch-a]", m.Channels)
	}
	// 内部名 real-x 已被映射覆盖 ⇒ **不该**再单独出现一行。
	for _, row := range plaza.Models {
		if row.Public == "real-x" {
			t.Fatalf("★ real-x 已被 ModelMap 覆盖，不该再作为独立对外模型出现：%+v", plaza.Models)
		}
	}
}

// ★ ② 没有映射的 Models ⇒ 内部名就是对外名。
func TestPlazaIncludesUnmappedModels(t *testing.T) {
	groups := []*groupstore.Group{pgrp("g-a", "主线", []string{"bare-model"}, nil)}
	channels := []*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled, Models: []string{"bare-model"}},
	}
	plaza := BuildModelPlaza(groups, channels)
	if plaza.Total != 1 || plaza.Models[0].Public != "bare-model" {
		t.Fatalf("未映射的模型应直接作为对外名出现，实际 %+v", plaza.Models)
	}
	if !plaza.Models[0].InCatalog {
		t.Fatalf("渠道有它，应为 in_catalog")
	}
}

// ★ ③ 分组里配了、渠道没供给 ⇒ orphaned（这是广场最该报的东西）。
func TestPlazaFlagsOrphans(t *testing.T) {
	groups := []*groupstore.Group{
		pgrp("g-a", "主线", []string{"ghost"}, map[string][]string{"ghost-pub": {"ghost"}}),
	}
	channels := []*Channel{
		{ID: "ch-a", Name: "a", Kind: "standard", Status: StatusEnabled, Models: []string{"other"}},
	}
	plaza := BuildModelPlaza(groups, channels)

	if plaza.Orphaned != 1 {
		t.Fatalf("★ 应有 1 个孤儿模型，实际 %d（%+v）", plaza.Orphaned, plaza.Models)
	}
	if !reflect.DeepEqual(plaza.OrphanedList, []string{"ghost-pub"}) {
		t.Fatalf("孤儿名单 = %v，want [ghost-pub]", plaza.OrphanedList)
	}
	if !strings.Contains(plaza.Note, "没有渠道能服务") {
		t.Fatalf("说明应警示没人能服务，实际 %q", plaza.Note)
	}
}

// ★ ④ 停用渠道的供给**不算数** ⇒ 该模型仍然是孤儿。
//
// 与删除影响分析同一条纪律：停用渠道不服务流量，把它算成供给等于骗人。
func TestPlazaIgnoresDisabledChannelSupply(t *testing.T) {
	groups := []*groupstore.Group{pgrp("g-a", "主线", []string{"m"}, nil)}
	channels := []*Channel{
		{ID: "ch-off", Name: "off", Kind: "standard", Status: 0, Models: []string{"m"}},
	}
	plaza := BuildModelPlaza(groups, channels)

	if plaza.Models[0].InCatalog {
		t.Fatalf("★ 停用渠道不该算成供给：%+v", plaza.Models[0])
	}
	if plaza.Orphaned != 1 {
		t.Fatalf("应判孤儿，实际 orphaned=%d", plaza.Orphaned)
	}
}

// ⑤ 同一个对外名出现在多个分组 ⇒ 合并成一行，Groups 全列出。
func TestPlazaMergesAcrossGroups(t *testing.T) {
	groups := []*groupstore.Group{
		pgrp("g-a", "主线", nil, map[string][]string{"shared": {"real-1"}}),
		pgrp("g-b", "备用", nil, map[string][]string{"shared": {"real-2"}}),
	}
	plaza := BuildModelPlaza(groups, nil)

	if plaza.Total != 1 {
		t.Fatalf("同名应合并成 1 行，实际 %d", plaza.Total)
	}
	m := plaza.Models[0]
	if !reflect.DeepEqual(m.Groups, []string{"g-a", "g-b"}) {
		t.Fatalf("Groups = %v，want [g-a g-b]", m.Groups)
	}
	if !reflect.DeepEqual(m.GroupNames, []string{"主线", "备用"}) {
		t.Fatalf("GroupNames = %v（应与 Groups 同序），want [主线 备用]", m.GroupNames)
	}
	if !reflect.DeepEqual(m.Reals, []string{"real-1", "real-2"}) {
		t.Fatalf("Reals = %v，want [real-1 real-2]", m.Reals)
	}
}

// ⑥ 空分组 ⇒ 空广场 + 一句说明（不是静默空白）。
func TestPlazaEmptyExplains(t *testing.T) {
	plaza := BuildModelPlaza(nil, nil)
	if plaza.Total != 0 {
		t.Fatalf("空输入应得空广场，实际 %d", plaza.Total)
	}
	if plaza.Note == "" {
		t.Fatal("空广场必须给一句说明，否则界面是一片空白")
	}
}

// ⑦ 排序稳定（同一份输入两次结果必须一模一样）。
func TestPlazaStable(t *testing.T) {
	groups := []*groupstore.Group{
		pgrp("g-b", "乙", []string{"z-model", "a-model"}, map[string][]string{"m-pub": {"r"}}),
		pgrp("g-a", "甲", []string{"b-model"}, nil),
	}
	a := BuildModelPlaza(groups, nil)
	b := BuildModelPlaza(groups, nil)
	if !reflect.DeepEqual(a.Models, b.Models) {
		t.Fatal("★ 两次构建结果不同 —— 排序不稳定，界面会随机跳")
	}
}

// ⑧ 空白项不制造空名字行。
func TestPlazaSkipsBlank(t *testing.T) {
	groups := []*groupstore.Group{
		pgrp("g-a", "甲", []string{"", "  ", "real"}, map[string][]string{"": {"x"}, "  ": {"y"}}),
	}
	plaza := BuildModelPlaza(groups, nil)
	for _, m := range plaza.Models {
		if strings.TrimSpace(m.Public) == "" {
			t.Fatalf("★ 出现了空名字的行：%+v", plaza.Models)
		}
	}
}
