// login_options.go 上游自报「添加账号时要用户先选什么」。
//
// # 为什么需要它（用户报的）
//
// 用户原话：「添加账号需要用户选择国内或者国际啊」。
//
// MonkeyCode 有国内站与国际站，**两站的账号体系不同、模型目录不同**
// （国内 45 个 / 国际 11 个），登录也必须去对的站点。而 `LoginFlow.Start()`
// 没有参数 —— 接口刻意只留三个方法。
//
// # 三个可选做法，只有第三个符合本仓判据
//
//	① 前端写 `if (provider === 'monkeycode') 显示区域选择器`
//	   → 前端知道了上游名，加第 14 个上游又要改前端（判据 1 失效）
//	② 核心写一张「哪个上游需要选区域」的表
//	   → 核心知道了上游的事实，同样违反「加新上游核心零改动」
//	③ 上游自己报「我要这些选项」 ← 本扩展点
//	   → 前端只读数据、核心只搬运
//
// 与 DailyActionExt / NoticeExt 完全同构：那些解决"上游有哪些动作"与
// "上游要说什么"，这个解决"上游要问用户什么"。
package gateway

// LoginOptionsExt 上游自报登录前要用户选择的选项。
//
// # 不实现 = 不需要选择（默认，也是多数上游的形态）
//
// 纯可选扩展点：不实现零行为变化，既有上游的添加账号流程逐字节不变。
type LoginOptionsExt interface {
	// LoginOptions 返回本次登录要用户先选的选项。
	//
	// 返回空切片 = 不需要选择（等价于不实现本扩展点）。
	//
	// 约束：每个选项的 Key 必须匹配 `^[a-z][a-z0-9_]*$` —— 它会成为
	// 表单字段名与 URL 查询参数，含引号/空格会破坏渲染
	//（与 ValidDailyActionID 同一套理由）。
	LoginOptions() []LoginOption
}

// LoginOption 一个登录选项（下拉框 / 单选组）。
//
// 它的字段刻意与 HTML 表单同构：前端渲染一个 `<select>`，
// 提交时把 value 装进 `{options:{<key>:<value>}}` 传给 /admin/login/start。
type LoginOption struct {
	// Key 选项标识（成为表单字段名）。
	Key string `json:"key"`
	// Label 显示给用户的中文名（如「站点」）。
	//
	// 由**上游**给而不是前端按 Key 翻译：同一个 Key 在不同上游可以叫不同名字。
	Label string `json:"label"`
	// Values 可选取值（顺序即选项顺序）。
	Values []LoginOptionValue `json:"values"`
	// Default 默认值（必须是 Values 里某一项的 Value）。空 = 用第一项。
	Default string `json:"default,omitempty"`
	// Hint 一句说明（可选）。
	Hint string `json:"hint,omitempty"`
}

// LoginOptionValue 选项的一个取值。
type LoginOptionValue struct {
	// Value 提交给上游的值（稳定标识，不随文案变）。
	Value string `json:"value"`
	// Label 显示给用户的文案（如「国内站」）。
	Label string `json:"label"`
}

// LoginOptionSpecs 把 LoginOptionsExt 的声明归一成便于前端消费的形状。
//
// 与 SanitizeDailyActions 同一职责：在**核心侧**就把畸形数据挡掉，
// 让坏数据变成"少一个控件"而不是"页面某处静默坏掉"。
func LoginOptionSpecs(list []LoginOption) []LoginOption {
	if len(list) == 0 {
		return nil
	}
	out := make([]LoginOption, 0, len(list))
	seenKey := map[string]bool{}
	for _, o := range list {
		if !validLoginOptionKey(o.Key) || seenKey[o.Key] {
			continue
		}
		vals := make([]LoginOptionValue, 0, len(o.Values))
		seenVal := map[string]bool{}
		for _, v := range o.Values {
			if v.Value == "" || seenVal[v.Value] {
				continue
			}
			label := v.Label
			if label == "" {
				label = v.Value
			}
			seenVal[v.Value] = true
			vals = append(vals, LoginOptionValue{Value: v.Value, Label: label})
		}
		if len(vals) == 0 {
			continue
		}
		seenKey[o.Key] = true
		label := o.Label
		if label == "" {
			label = o.Key
		}
		def := o.Default
		if def == "" || !seenVal[def] {
			def = vals[0].Value
		}
		out = append(out, LoginOption{
			Key:     o.Key,
			Label:   label,
			Values:  vals,
			Default: def,
			Hint:    o.Hint,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validLoginOptionKey Key 的字符集：小写字母开头，后接小写字母/数字/下划线。
func validLoginOptionKey(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i, r := range s {
		okLetter := r >= 'a' && r <= 'z'
		okDigit := r >= '0' && r <= '9'
		okUnderscore := r == '_'
		if !okLetter && !okDigit && !okUnderscore {
			return false
		}
		if i == 0 && !okLetter {
			return false
		}
	}
	return true
}

// LoginFlowWithOptions 需要用户先选选项的登录流程。
//
// # 为什么是**独立接口**而不是给 LoginFlow.Start 加参数
//
// 12 个上游实现的是 LoginFlow，而只有本家需要"先选站点"。
// 把参数加到 Start 上等于让 12 个上游都跟着改签名 ——
// 一次纯粹的连带改动，且它们都用不上那个参数。
//
// 所以：需要选项的上游**额外**实现本接口；核心先探测它，
// 探测不到就回落到 Start()（老行为逐字节不变）。
type LoginFlowWithOptions interface {
	LoginFlow
	// StartWith 用调用方给定的选项申请一次登录。
	//
	// opts 的键来自 LoginOptionsExt 自报的 Key；缺失的键由上游按
	// Default 补齐（核心不填 —— 默认值也是上游的事实）。
	StartWith(opts map[string]string) (state, authURL string, err error)
}
