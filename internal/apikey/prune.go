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

	// GroupsGone 被移除的分组 ID（PruneGroups 用；按字典序）。
	GroupsGone []string `json:"groups_gone,omitempty"`

	// KeysUnrestricted 因这次清理而**变成「不限制」**的 key ID 列表。
	//
	// # ⚠ 这个字段是本文件最重要的输出，不是"附带的统计"
	//
	// 「Groups 清空」在语义上是**权限放大**：这把 key 从"只能用这几个分组的模型"
	// 变成"什么模型都能用"。而用户的操作是「删掉一个分组」—— 他完全不会预期
	// 某把 key 的权限被放开。
	//
	// 所以必须**如实回报**，让管理端在响应里明说"这几把 key 现在没有分组限制了"。
	// 静默处理等于把一次删配置变成一次安全事件，而且在界面上完全看不出来。
	//
	// ⚠ 与 Models 的空白语义一致：空 = 不限制（不是"一个都不许"）。
	// 那个决定是在 apikey.go 的 Key.Models 注释里做的，这里不能改口径。
	KeysUnrestricted []string `json:"keys_unrestricted,omitempty"`
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

// PruneGroups 从**所有** key 的 Groups 里移除指定分组 ID（删除分组时的级联清理）。
//
// # 为什么必须做（这是「删分组」最容易忽略的一半）
//
// key.Groups 存的是分组 **ID**。删掉分组之后，那些 ID 就指向了不存在的对象。
// P2/P3 的判定链对「受限且并集为空」的处理是**拒绝**（安全，见
// server.apikey_models.go 的拦截）—— 于是那把 key 会变成「什么模型都不能用」，
// 而用户看到的现象只是「调用全 403」，错误信息只会说「分组里没有任何模型」，
// 他得自己去猜是不是刚才删的那个分组。
//
// 所以删除动作要**连带清干净**：与删渠道时清 key 里的模型引用（PruneModels）
// 是同一套口径 —— 用户删了一个东西，指向它的残留就该一起消失。
//
// # ⚠ 清理会让某些 key 变成「不限制」（权限放大），必须回报
//
// 一把 key 如果只属于这一个分组，清理后 Groups 变空 ⇒ 语义从「受限」跳到
// 「不限制」。**这不是 bug，是既有语义决定的**（空 = 不限制，见 Key.Groups 的注释），
// 但不能悄悄发生：结果里逐把列出 KeysUnrestricted，让管理端明说。
//
// 另一种可选做法是「把这种 key 直接禁用」，但那更糟 —— 用户删一个分组
// 把几把 key 停掉，同样是没预期的事故，而且更难恢复（要回管理页逐把启用）。
func (s *Store) PruneGroups(groupIDs []string) (PruneResult, error) {
	var res PruneResult
	want := map[string]bool{}
	for _, g := range groupIDs {
		if g != "" {
			want[g] = true
		}
	}
	if len(want) == 0 {
		return res, nil // 空输入：什么都不动（与 PruneModels 的同一条守卫）
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	gone := map[string]bool{}
	for _, k := range s.keys {
		if k == nil || len(k.Groups) == 0 {
			continue
		}
		kept := make([]string, 0, len(k.Groups))
		changed := false
		for _, g := range k.Groups {
			if want[g] {
				gone[g] = true
				changed = true
				continue
			}
			kept = append(kept, g)
		}
		if !changed {
			continue
		}
		// ⚠ 变空要记下来：这是权限放大，调用方必须能告诉用户。
		if len(kept) == 0 {
			res.KeysUnrestricted = append(res.KeysUnrestricted, k.ID)
		}
		// normalizeGroups 在空输入时返回 nil（而不是空切片）—— 与「不限制」的
		// 规范表达一致，也让 omitempty 在落盘时不产生这个字段。
		k.Groups = normalizeGroups(kept)
		res.KeysTouched++
	}
	if res.KeysTouched == 0 {
		return res, nil
	}
	for g := range gone {
		res.GroupsGone = append(res.GroupsGone, g)
	}
	sort.Strings(res.GroupsGone)
	sort.Strings(res.KeysUnrestricted)
	return res, s.saveLocked()
}
