// prune_test.go 守「删渠道 ⇒ key 里的引用也清理」这条语义。
//
// 用户的原话：「删除之后有关这个渠道的都不要了，语义不就是这样么」。
//
// 不清理的后果（实测）：目录里已经看不到那个模型，但白名单里还留着，
// 于是「授权了 N 个模型」的计数比实际能用的多，管理界面里那条是死配置。
package apikey

import (
	"path/filepath"
	"testing"
)

func newPruneStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestPruneModelsRemovesWhitelistAndAlias 白名单与别名映射都要清。
func TestPruneModelsRemovesWhitelistAndAlias(t *testing.T) {
	s := newPruneStore(t)
	k, err := s.Create("app", 0, 0,
		[]string{"shusheng/intern-latest", "loomy/DeepSeek-V4.1-Flash"},
		map[string]string{
			"003/intern-latest": "shusheng/intern-latest",
			"001/deepseek":       "loomy/DeepSeek-V4.1-Flash",
		})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := s.PruneModels([]string{"shusheng/intern-latest"})
	if err != nil {
		t.Fatalf("PruneModels: %v", err)
	}
	if res.KeysTouched != 1 {
		t.Errorf("应影响 1 把 key，实际 %d", res.KeysTouched)
	}

	models, _ := s.ModelsOf(k.ID)
	for _, m := range models {
		if m == "shusheng/intern-latest" {
			t.Error("白名单里的失效模型没被清掉")
		}
	}
	if len(models) != 1 || models[0] != "loomy/DeepSeek-V4.1-Flash" {
		t.Errorf("不该动其它条目，实际 %v", models)
	}

	mp, _ := s.ModelMapOf(k.ID)
	if _, has := mp["003/intern-latest"]; has {
		t.Error("指向失效模型的别名必须整条删掉（留着必然调用失败）")
	}
	if _, has := mp["001/deepseek"]; !has {
		t.Error("不该动其它别名")
	}
}

// TestPruneModelsNoMatchIsNoop 没命中时不动任何东西。
func TestPruneModelsNoMatchIsNoop(t *testing.T) {
	s := newPruneStore(t)
	k, _ := s.Create("app", 0, 0, []string{"a/b"}, nil)
	res, err := s.PruneModels([]string{"x/y"})
	if err != nil {
		t.Fatalf("PruneModels: %v", err)
	}
	if res.KeysTouched != 0 {
		t.Errorf("没命中不该报告影响了 key，实际 %d", res.KeysTouched)
	}
	models, _ := s.ModelsOf(k.ID)
	if len(models) != 1 || models[0] != "a/b" {
		t.Errorf("不该动任何条目，实际 %v", models)
	}
}

// TestPruneModelsAffectsAllKeys 清理要覆盖所有 key，不只当前身份那把。
func TestPruneModelsAffectsAllKeys(t *testing.T) {
	s := newPruneStore(t)
	k1, _ := s.Create("a", 0, 0, []string{"gone/x"}, nil)
	k2, _ := s.Create("b", 0, 0, []string{"gone/x", "keep/y"}, nil)
	res, err := s.PruneModels([]string{"gone/x"})
	if err != nil {
		t.Fatalf("PruneModels: %v", err)
	}
	if res.KeysTouched != 2 {
		t.Errorf("应影响 2 把 key，实际 %d", res.KeysTouched)
	}
	for _, id := range []string{k1.ID, k2.ID} {
		models, _ := s.ModelsOf(id)
		for _, m := range models {
			if m == "gone/x" {
				t.Errorf("key %s 里的失效模型没被清掉", id)
			}
		}
	}
}

// TestPruneModelsEmptyInputNoop 空输入不能把白名单清空（最危险的形态）。
func TestPruneModelsEmptyInputNoop(t *testing.T) {
	s := newPruneStore(t)
	k, _ := s.Create("app", 0, 0, []string{"a/b"}, nil)
	if _, err := s.PruneModels(nil); err != nil {
		t.Fatalf("PruneModels: %v", err)
	}
	models, _ := s.ModelsOf(k.ID)
	if len(models) != 1 {
		t.Error("空输入把白名单清空了 —— 那会让这把 key 变成不限制（最危险的失效）")
	}
}
