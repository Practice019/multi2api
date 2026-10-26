package server

import (
	"strings"
	"testing"
)

// B2（task-59）追加的两条：allowedModelsForRequest 的**分组优先**与**白名单兜底**。
//
// 为什么放在 internal/server：allowedModelsForRequest 不可导出，测试必须同包。
// 本文件**只加测试**，一行都不改 apikey_models.go（task-59 的硬约束）。
//
// 复用的脚手架都在 apikey_models_test.go：newKeyStore(t) / reqAs(t, id)。

// TestB2AllowedModelsGroupPriorityWins 分组非空且解析器已接线 ⇒ 走分组路径，
// 且**白名单不参与判定**（PLAN-groups.md Q3：两者语义重复，分组优先）。
func TestB2AllowedModelsGroupPriorityWins(t *testing.T) {
	keys := newKeyStore(t)
	// 这把 key 同时有白名单与分组：白名单里放一个只有它才有的模型名，
	// 用来证明"分组非空时白名单被忽略"。
	k, err := keys.Create("b2-分组优先", 0, 0, []string{"只在白名单里的模型"})
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.UpdateWithGroups(k.ID, nil, nil, nil, []string{"g-1"}); err != nil {
		t.Fatal(err)
	}
	keys.SetGroupModels(func(groupID string) []string {
		if groupID == "g-1" {
			return []string{"m-a", "m-b"}
		}
		return nil
	})
	h := &Handler{cfg: Config{APIKeys: keys}}

	got, restricted := h.allowedModelsForRequest(reqAs(t, k.ID))
	if !restricted {
		t.Fatal("分组非空 ⇒ 必须受限（restricted=true）")
	}
	if strings.Join(got, ",") != "m-a,m-b" {
		t.Fatalf("分组优先：应当是分组并集 m-a,m-b，实际 %v", got)
	}
	for _, m := range got {
		if m == "只在白名单里的模型" {
			t.Fatal("分组非空时白名单不该参与判定（Q3）")
		}
	}
}

// TestB2AllowedModelsWhitelistFallback 没有分组（或解析器未接线）⇒ 落回白名单路径，
// 存量行为不变。
func TestB2AllowedModelsWhitelistFallback(t *testing.T) {
	keys := newKeyStore(t)
	k, err := keys.Create("b2-白名单兜底", 0, 0, []string{"m-only"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: Config{APIKeys: keys}}

	got, restricted := h.allowedModelsForRequest(reqAs(t, k.ID))
	if !restricted {
		t.Fatal("白名单非空 ⇒ 必须受限")
	}
	if len(got) != 1 || got[0] != "m-only" {
		t.Fatalf("白名单兜底应当只放行 m-only，实际 %v", got)
	}

	// 反向：既没有分组也没有白名单 ⇒ 不限制（不能被读成"一个都不许"）
	free, err := keys.Create("b2-不限制", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, restricted := h.allowedModelsForRequest(reqAs(t, free.ID)); restricted || got != nil {
		t.Fatalf("空分组 + 空白名单 ⇒ 不限制，实际 (%v, %v)", got, restricted)
	}
}
