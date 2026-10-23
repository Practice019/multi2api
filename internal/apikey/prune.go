// prune.go 清理 API key 里**指向已不存在模型**的引用。
//
// # 为什么需要它（用户实测的残留）
//
// 用户删掉一条渠道后，模型目录里立刻没了那个模型 —— 但 API key 的
// 白名单与别名映射里**还留着它**。表现：
//
//	· 目录列表看不到（被目录过滤掉），但白名单里还占着一格
//	· 「本 key 授权了 N 个模型」的计数比实际能用的多
//	· 管理界面里那条是死配置，勾着却不可用
//
// # 为什么按「渠道声明过的模型」精确清理，而不是按目录快照
//
// 按目录快照清理的风险是：目录拉取**失败**时（上游抖动、凭证过期），
// 目录会短暂为空 —— 那时跑一次清理会把用户精心挑的白名单全删了。
// 「一次抖动毁掉全部配置」的代价远大于留一条死配置。
//
// 按渠道声明精确清理是安全的：它不依赖目录快照，只依赖「这条渠道声明过
// 哪些模型」—— 那是删除那一刻的确切事实。
package apikey

import (
	"sort"
)

// PruneResult 一次清理的结果（供界面提示「删了几条」）。
type PruneResult struct {
	KeysTouched int      `json:"keys_touched"`
	ModelsGone  []string `json:"models_gone"`
}

// PruneModels 从**所有** key 的白名单与别名映射里移除指定模型名。
//
// models 是「已不存在的内部模型名」（渠道声明过的那些）。
// 比对口径：
//
//	· 白名单里存的是内部名 → 直接比
//	· 别名映射的**值**是内部名 → 比 value；命中的那条映射整条删掉
//	  （留着「公开名 → 已不存在的内部名」比没有更糟：调用必然失败）
//
// 返回被清掉的模型名（按字典序，便于日志与界面展示）。
func (s *Store) PruneModels(models []string) (PruneResult, error) {
	want := map[string]bool{}
	for _, m := range models {
		if m != "" {
			want[m] = true
		}
	}
	var res PruneResult
	if len(want) == 0 {
		return res, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := map[string]bool{}
	for _, k := range s.keys {
		if k == nil {
			continue
		}
		changed := false

		// ① 白名单：直接比内部名
		if len(k.Models) > 0 {
			kept := make([]string, 0, len(k.Models))
			for _, m := range k.Models {
				if want[m] {
					gone[m] = true
					changed = true
					continue
				}
				kept = append(kept, m)
			}
			if changed {
				k.Models = kept
			}
		}

		// ② 别名映射：值是内部名
		if len(k.ModelMap) > 0 {
			for pub, internal := range k.ModelMap {
				if want[internal] {
					delete(k.ModelMap, pub)
					gone[internal] = true
					changed = true
				}
			}
		}

		if changed {
			res.KeysTouched++
		}
	}
	if res.KeysTouched == 0 {
		return res, nil
	}
	for m := range gone {
		res.ModelsGone = append(res.ModelsGone, m)
	}
	sort.Strings(res.ModelsGone)
	return res, s.saveLocked()
}
