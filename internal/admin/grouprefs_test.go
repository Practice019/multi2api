// grouprefs_test.go 守「写路径把分组引用归一成 ID」这条纪律。
//
// # 为什么必须有它（本仓一处真实的断裂）
//
// 分组引用在库里曾经有**两种**形态，来源不同：
//
//	P3.5 迁移（apikey/migrate.go:178）写的是 createGroup 返回的 **ID**
//	/admin/apikeys 的弹窗（webui.html）提交的也是 **ID**
//	/admin/tokens 的抽屉（webui-tokens.html）提交的却是 **分组名**
//
// 而判定链只按 ID 查 ⇒ 用令牌抽屉存下来的那把凭据「受限且并集为空」：
// 实测（demo 7860，令牌 id=3）GET /v1/models 回 {"data":[],"object":"list"}，
// 而它指向的分组里明明有 7 个模型。
//
// 本文件钉住两件事：
//  1. 写路径归一 —— 名字进来、ID 落库（此后改名不再断链）
//  2. 查不到的引用**拒**而不是**丢** —— 丢掉等于把用户配的授权悄悄放大
//     （受限 → 不限制），而界面上完全看不出来。
package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"workbuddy2api/internal/store"
)

// newGroupRefsFixture 一套带分组存储的管理端（复用反查测试的夹具）。
func newGroupRefsFixture(t *testing.T) (*groupsUsageFixture, string) {
	t.Helper()
	f := newGroupsUsageFixture(t)
	g, err := f.groups.Create("666 的模型", "夹具", []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("建分组: %v", err)
	}
	return f, g.ID
}

// TestCanonicalGroupRefsUnit 归一函数本身的三态与两种形态（ID / 名字）。
func TestCanonicalGroupRefsUnit(t *testing.T) {
	f, gid := newGroupRefsFixture(t)

	// ID 与名字都归一成 ID
	for _, ref := range []string{gid, "666 的模型", " 666 的模型 "} {
		got, msg := f.h.canonicalGroupRefs([]string{ref})
		if msg != "" {
			t.Fatalf("canonicalGroupRefs(%q) 报错: %s", ref, msg)
		}
		if len(got) != 1 || got[0] != gid {
			t.Errorf("canonicalGroupRefs(%q) = %v，期望 [%s]", ref, got, gid)
		}
	}

	// 同一个分组的两种形态同时出现 ⇒ 归一后去重
	got, msg := f.h.canonicalGroupRefs([]string{gid, "666 的模型"})
	if msg != "" || len(got) != 1 {
		t.Errorf("ID 与名字同时出现应去重成一条，实际 %v（msg=%q）", got, msg)
	}

	// 未知引用：**拒**（不是丢掉）
	if _, msg := f.h.canonicalGroupRefs([]string{"没有这个分组"}); msg == "" {
		t.Error("★ 未知引用必须报错 —— 丢掉它等于把作用域从「受限」放大成「不限制」")
	}

	// 三态：nil ⇒ nil（不改）；[] ⇒ 空切片（显式清空）；全空白 ⇒ 空切片
	if got, msg := f.h.canonicalGroupRefs(nil); msg != "" || got != nil {
		t.Errorf("nil 必须原样传下去（否则一次「只改额度」的更新会把分组清空），实际 %v", got)
	}
	empty := []string{}
	if got, msg := f.h.canonicalGroupRefs(empty); msg != "" || got == nil || len(got) != 0 {
		t.Errorf("显式空切片应回显式空切片，实际 %v", got)
	}
	if got, _ := f.h.canonicalGroupRefs([]string{"  "}); got == nil {
		t.Error("全是空白应回空切片（= 清空），不是 nil（= 不改）")
	}
}

// TestTokenCreateCanonicalizesGroupNameToID 端到端：抽屉发名字，库里存 ID。
func TestTokenCreateCanonicalizesGroupNameToID(t *testing.T) {
	f, gid := newGroupRefsFixture(t)

	code, body := doJSON(t, f.h, http.MethodPost, "/admin/tokens",
		`{"name":"抽屉建的有序分组令牌","automatic":false,"groups":["666 的模型"]}`)
	if code != http.StatusOK {
		t.Fatalf("创建令牌应 200，实际 %d: %s", code, body)
	}
	var created struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("解析创建结果: %v", err)
	}
	if len(created.Items) != 1 {
		t.Fatalf("应建出 1 条，实际 %d（%s）", len(created.Items), body)
	}
	id := int(created.Items[0]["id"].(float64))

	row, err := store.GetTokenByID(id)
	if err != nil {
		t.Fatalf("回读令牌: %v", err)
	}
	got := row.GetGroups()
	if len(got) != 1 || got[0] != gid {
		t.Fatalf("★ 库里应存分组 ID %s，实际 %v —— 存名字会让判定链查不到（受限且并集为空）", gid, got)
	}
}

// TestTokenCreateRejectsDanglingGroupRef 未知引用 ⇒ 400，且**什么都不落库**。
func TestTokenCreateRejectsDanglingGroupRef(t *testing.T) {
	f, _ := newGroupRefsFixture(t)

	code, body := doJSON(t, f.h, http.MethodPost, "/admin/tokens",
		`{"name":"悬空引用令牌","automatic":false,"groups":["这个分组不存在"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未知分组引用应 400，实际 %d: %s", code, body)
	}
	list, _, err := listTokenPage("悬空引用令牌", 0, 10)
	if err != nil {
		t.Fatalf("列表: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("被拒的创建不该留下任何行，实际 %d 条", len(list))
	}
}

// TestTokenCreateCanonicalizesAutoGroups 自动分组·自定义的候选集同样归一成 ID。
//
// 这一条同时是"自动分组真的进了判定链"的前置条件：快照里若是名字，
// 判定链（按 ID 查）仍然取不到模型，用户看到的还是"所有全局模型"。
func TestTokenCreateCanonicalizesAutoGroups(t *testing.T) {
	f, gid := newGroupRefsFixture(t)

	code, body := doJSON(t, f.h, http.MethodPost, "/admin/tokens",
		`{"name":"自动自定义组","automatic":true,"auto_group_mode":"custom","auto_groups":["666 的模型"]}`)
	if code != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d: %s", code, body)
	}
	var created struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal([]byte(body), &created)
	if len(created.Items) != 1 {
		t.Fatalf("应建出 1 条：%s", body)
	}
	id := int(created.Items[0]["id"].(float64))

	row, err := store.GetTokenByID(id)
	if err != nil {
		t.Fatalf("回读: %v", err)
	}
	auto, err := row.GetAutoGroups()
	if err != nil {
		t.Fatalf("解析快照: %v", err)
	}
	if len(auto) != 1 || auto[0] != gid {
		t.Fatalf("★ 快照里应存分组 ID %s，实际 %v", gid, auto)
	}
	if len(row.GetGroups()) != 0 {
		t.Errorf("自动分组模式下 Group 应为显式空列表，实际 %v", row.GetGroups())
	}
}
