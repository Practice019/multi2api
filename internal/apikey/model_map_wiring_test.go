// model_map_wiring_test.go 两个「存了但读不出来 / 改了但这个入口没生效」的守卫
//
// # 用户报的两个现象（2026-10-21）
//
//	① 「设置了之后在编辑打开没有了 不会持久化？？」
//	② 「我明明设置了对外名称为什么 csg 还是原来的模型名称？」
//
// 两个都不是"没保存"，数据一直在盘上：
//
//	apikeys.json: "model_map":{"001/DeepSeek-V4.1-Flash":"loomy/DeepSeek-V4.1-Flash",
//	                            "002/deepseek-v4.1-flash-1017":"csg/deepseek-v4.1-flash-1017"}
//
// ① 是 `Store.List()` 构造 View 时**漏了 ModelMap 字段** ⇒ 前端拿到 undefined ⇒ 回填成 0 行。
// ② 是 `/v1/models` 的组装顺序：filter 改名的只覆盖了**上游目录**，
//
//	而后 append 进来的**渠道模型**用的是内部名，绕过了改名那一步。
//
// 两条都属于「四处要同时改，漏了一处」这一类 —— 所以用测试把每一处都钉住。
package apikey

import (
	"path/filepath"
	"testing"
)

// TestListFillsModelMap ★ 守卫 ①：List 必须把 ModelMap 带进 View。
//
// ⚠ 这条如果红了，界面上表现为「设了映射、重开编辑框是空的」，
//
//	而数据其实在盘上 —— 用户会以为"没保存"，很难往 List 上想。
func TestListFillsModelMap(t *testing.T) {
	s := newTestStore(t)
	k, err := s.CreateOwned("", "k1", 0, 0, []string{"csg/x"},
		map[string]string{"pro": "csg/x"})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	views := s.List()
	if len(views) != 1 {
		t.Fatalf("应有 1 条 View，实际 %d", len(views))
	}
	v := views[0]
	if v.ModelMap == nil {
		t.Fatalf("★ View.ModelMap 是 nil —— List() 没把 Key.ModelMap 带出来")
	}
	if v.ModelMap["pro"] != "csg/x" {
		t.Fatalf("★ View.ModelMap 内容不对：%v（期望 pro → csg/x）", v.ModelMap)
	}
	_ = k
}

// TestListModelMapIsCopy List 返回的 ModelMap 必须是副本。
//
// 前端把它 JSON 序列化出去没问题，但同一个进程里别的地方拿到 View 后
// 改一个键，就污染了 store —— 而下一次 saveLocked 会把它写进盘。
func TestListModelMapIsCopy(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateOwned("", "k1", 0, 0, nil, map[string]string{"pro": "csg/x"}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	v := s.List()[0]
	v.ModelMap["pro"] = "被改坏了"
	v.ModelMap["注入"] = "csg/evil"

	v2 := s.List()[0]
	if v2.ModelMap["pro"] != "csg/x" {
		t.Fatalf("★ View.ModelMap 不是副本：改了它污染了 store（%v）", v2.ModelMap)
	}
	if _, ok := v2.ModelMap["注入"]; ok {
		t.Fatalf("★ 往 View.ModelMap 加键污染了 store（%v）", v2.ModelMap)
	}
}

// TestModelMapSurvivesReload 落盘后重开 store 仍然有映射（真正的"持久化"）。
//
// ⚠ 这条看起来显然，但用户报的正是"不持久化" —— 而实际原因是 List 漏字段。
//
//	把它写成测试，是为了让"到底是没存还是没读"这个区分永远可查：
//	这条绿 = 存没问题；上面那条红 = 读有问题。
func TestModelMapSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apikeys.json")
	s, err := New(path)
	if err != nil {
		t.Fatalf("建 store 失败: %v", err)
	}
	if _, err := s.CreateOwned("", "k1", 0, 0, []string{"csg/x"},
		map[string]string{"pro": "csg/x"}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	s2, err := New(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	m, ok := s2.ModelMapOf(s.List()[0].ID)
	if !ok || m["pro"] != "csg/x" {
		t.Fatalf("★ 重开后映射丢了：%v ok=%v", m, ok)
	}
}
