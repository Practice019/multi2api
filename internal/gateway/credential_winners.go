package gateway

import "sort"

// PickWinnersByUID 按 uid 去重：同一 uid 有多份凭证时留下 better 判定为"更好"的那一份，
// 输出按 uid 升序（稳定排序）。
//
// # 实测依据（T-339）
//
// 有 5 家的 credential.go 里各有一份 pickWinners：
//
//	cline ≡ raccoon   函数体**逐字相同**（22 行代码，diff=0）
//	lobsterai / qoder 只差**一个取值器名**（a.UIDValue() vs a.UID()）—— 完全可参数化
//	loomy             ⚠ **语义不同**，因此**没有**纳入本函数（见下）
//
// ⇒ 4 家共用这一份骨架；各家只提供"uid 怎么取"与"哪份更好"。
//
// # ⚠ 为什么 loomy 不进来
//
// 它的 pickWinners 是另一套语义：
//
//	· 选的是 `a.LoggedInAt` 最大的那份（不是 betterAuth 的判据）
//	· **不排序**（输出保持首次出现顺序）
//	· 还额外跳过 `a == nil || a.UID == ""`
//
// 三者任一不同都会改变可观察行为 ⇒ 强行归一等于偷偷改 loomy 的选号策略。
//
// # ⚠ 刻意**不**加 nil 过滤
//
// 那 4 家的原实现遇到 `nil` 元素会 panic（`a.UID()`）。保持 panic 才是"行为不变"；
// 顺手加一层跳过会把一个会崩的路径变成一个静默丢号的路径 —— 那是行为变更，
// 而不是重构。真要加固应当单独一轮、带自己的用例来。
//
// # 为什么是泛型 + 两个回调，而不是"统一凭证结构"
//
// 那 4 家的凭证类型各不相同（各自私有）。把它们的字段强行塞进一个公共结构，
// 就会让"读错字段"变成编译期发现不了的事故（本仓真踩过：ExpiresAt 秒/毫秒、
// access_token 下划线/驼峰）⇒ 断言与取值留在各家，共享的只是骨架。
func PickWinnersByUID[T any](list []T, uidOf func(T) string, better func(a, b T) bool) []T {
	byUID := make(map[string]T, len(list))
	order := make([]string, 0, len(list))
	for _, a := range list {
		uid := uidOf(a)
		cur, ok := byUID[uid]
		if !ok {
			byUID[uid] = a
			order = append(order, uid)
			continue
		}
		if better(a, cur) {
			byUID[uid] = a
		}
	}
	out := make([]T, 0, len(order))
	for _, uid := range order {
		out = append(out, byUID[uid])
	}
	sort.SliceStable(out, func(i, j int) bool { return uidOf(out[i]) < uidOf(out[j]) })
	return out
}
