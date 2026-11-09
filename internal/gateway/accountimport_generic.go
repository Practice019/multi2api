package gateway

// accountimport_generic.go —— 批量粘贴导入的**共享骨架**（T-330）。
//
// # 为什么要抽（实测依据，不是"看起来像"）
//
// T-329 的调查把 10 个上游的 ImportCredentials 逐行比过：其中 5 个
// （loomy / raccoon / lobsterai / cline / trae）除了包名、Parse 函数名、
// UID 取值方式（`a.UID` vs `a.UID()`）与错误文案之外，**控制流逐字相同**：
//
//	items, err := SplitAccountImportItems(pasted)
//	if err != nil { return nil, err }
//	out := make([]ImportedCredential, 0, len(items))
//	for _, raw := range items {
//	    a, perr := <各家解析>([]byte(raw))
//	    if perr != nil { return nil, perr }
//	    if <uid 为空> { return nil, errNoImportUID }
//	    b, merr := MarshalAuthFile(a)
//	    if merr != nil { return nil, merr }
//	    out = append(out, ImportedCredential{FileName: ..., Raw: b, UID: uid, Nickname: a.Nickname})
//	}
//	return out, nil
//
// 差异**只在**中间那三行（解析 + 判 uid + 组装）⇒ 抽成回调参数，其余归共享。
//
// # 收益（不只是少几行）
//
//   - `out := make([]ImportedCredential, 0, len(items))` 这种"容易抄漏一行就静默变成 nil 切片"
//     的形态，在结构上不可能再写错
//   - 空输入 / 非法 JSON 的报错文案统一（都来自 SplitAccountImportItems），
//     前端不必为每个上游各写一种提示
//   - 逐条失败语义只有一处定义（见下）
//
// # 逐条失败语义：**任一条坏 ⇒ 整批失败**（保持现状，不引入 skipBad）
//
// 这一点是核对出来的，不是猜的：上述 5 个实现遇到坏条目时**一律** `return nil, perr`，
// 没有任何一家"跳过坏的继续导"。所以这里**刻意不做** `skipBad bool` 参数 ——
// 加一个当前无人使用、且会改变语义的开关属于投机式泛化；
// 将来真有上游需要"部分成功"，它应当带着自己的用例来加这个参数。

// ImportParser 把一条粘贴内容解析成可落盘的凭证（各上游唯一需要提供的东西）。
//
// ⚠ 契约：返回的 cred 必须自带 FileName 与 UID —— 两者为空都视为该条失败。
// 本函数不再替调用方补默认值（补了就会掩盖上游的解析缺陷：一个 uid 为空的
// 凭证进池子只会变成幽灵号，见 cmd/server 里 "没有 uid 的号进池子只会变成幽灵号"）。
type ImportParser func(item string) (ImportedCredential, error)

// ImportCredentialsGeneric 执行「分割 → 逐条解析 → 汇总」的共享流程。
//
// 每条失败即整体失败（见文件头的语义说明）。
func ImportCredentialsGeneric(pasted string, parse ImportParser) ([]ImportedCredential, error) {
	items, err := SplitAccountImportItems(pasted)
	if err != nil {
		return nil, err
	}
	return MapImportItems(items, parse)
}

// MapImportItems 是 ImportCredentialsGeneric 的后半段：把**已经切好的**条目逐个解析。
//
// # 为什么单独暴露
//
// 有上游在分割之前要做一次"整体优先"的判断（例如先按自己的格式整段解析、
// 失败才退回通用分割 —— 裸 JWT 那种不是 JSON 的输入就会走这条）。
// 那种上游需要的是"给我一段条目列表，我来逐条解析"，而不是"帮我分割"。
// 拆成两个函数，两种形态都能复用同一份逐条循环（含逐条失败语义）。
func MapImportItems(items []string, parse ImportParser) ([]ImportedCredential, error) {
	out := make([]ImportedCredential, 0, len(items))
	for _, item := range items {
		cred, perr := parse(item)
		if perr != nil {
			return nil, perr
		}
		if cred.FileName == "" || cred.UID == "" {
			return nil, errImportUnusable
		}
		out = append(out, cred)
	}
	return out, nil
}

// errImportUnusable 上游解析返回了"不可落盘"的凭证（缺 FileName 或 UID）。
//
// 这条兜底与各家自己的 errNoImportUID 并存：各家给出**具体**原因
// （"uid 为空" / "access_token 与 account_id 都取不到"），这里只兜住
// 将来新增上游忘了判的那种情况。文案刻意写成"上游：..."的形态，
// 让用户在界面上能看出是哪一家的解析器不合契约。
var errImportUnusable = errorString("导入失败：该条解析出的凭证缺少文件名或账号标识（上游解析器不符合契约）")

// errorString 极小的 error 实现（避免为一行文案引入 fmt/errors 依赖差异）。
type errorString string

func (e errorString) Error() string { return string(e) }
