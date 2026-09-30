// accountimport_ext.go 上游自报「怎么把一段 JSON 变成我的一份凭证」。
//
// # 为什么需要这个扩展点（用户要求：批量导入统一到所有上游）
//
// 用户原话：「那个批量导入全部上游都可以制作这个功能，所以就统一一下。」
//
// 事实是：**批量导入此前只有两个上游有**（workbuddy、loomy），各写了一份
// `handleImport`。其余 7 个上游在界面上没有这个按钮 —— 而它们的凭证形态
// 各不相同，前端无法自己拼。
//
// # ⚠ 关键发现：绝大多数上游**已经有**这两步
//
// 实测（2026-09-30）每个上游都已有：
//
//	ParseCredential(raw []byte) (*Auth, error)   把磁盘那段 JSON 解成凭证
//	MarshalAuthFile(a *Auth) ([]byte, error)     把凭证据序列化成磁盘那段 JSON
//
// 而"导入"恰好就是这两步的**复合**：把用户粘的 JSON 解析成凭证、再落盘。
// 所以每个上游要写的只有**三行**（调用上面两个函数），不需要手写字段映射
// —— 手写映射是"看起来成功、实际字段错位"的高发区。
//
// # 为什么 Core 不认识凭证 JSON
//
// 与 `CredentialLoader` / `CredentialRefresher` 同一条判据：凭证格式是
// **上游的事实**（loomy 是 `session`、codearts 是 AK/SK+DPoP 私钥、
// qoder 是 `device_token`…）。核心只把"用户粘的那段文本"原样交给上游，
// 上游回给它"该写成哪个文件名 + 写什么字节"，核心负责落盘。
//
// 核心**不解析、不校验、不理解**那段 JSON —— 否则加一个上游就要改核心。
package gateway

// AccountImportExt 上游自报「怎么把一段 JSON 变成我的一份凭证」。
type AccountImportExt interface {
	// ImportCredentials 把一段用户粘贴的 JSON 转成可以落盘的凭证。
	//
	// 输入是**原文**（未解析的字符串）：上游自己决定认哪些形状
	//（嵌套形 `{"auth":{…},"account":{…}}`、扁平形、甚至中文键）。
	//
	// 返回的每一条是 `(文件名, 文件内容, 该账号的 uid, 展示名)`：
	//
	//	FileName  落盘的文件名（不含目录）。空串表示该条无法落盘。
	//	Raw       文件内容（上游自己的序列化结果）。
	//	UID       账号池主键。空串 = 无法确定账号身份 → 该条算失败。
	//	Nickname  展示名（可空）。
	//
	// ⚠ **必须是"解析失败 → 返回带原因的错误"，不能静默返回空切片**：
	// 用户粘错东西时，界面要告诉他"哪里不对"，而不是"导入了 0 个"。
	//
	// ⚠ 实现必须**幂等且无副作用**：不要在这个方法里写盘、不要碰账号池。
	// 落盘与池对齐由核心负责（核心才知道 AuthDir 在哪、池怎么对齐）。
	// 这样上游包仍然不依赖 `internal/pool`（架构硬约束）。
	ImportCredentials(pasted string) ([]ImportedCredential, error)
}

// ImportedCredential 一条可落盘的凭证（`AccountImportExt` 的产物）。
//
// # 为什么是"文件名 + 内容"而不是"直接给个 *Auth"
//
// 因为落盘格式只有**上游**知道（文件名前缀、嵌套还是扁平、字段名 casing）。
// 核心拿着这三样就能写文件，且与页内登录产的凭证**逐字节同形** ——
// 下游（LoadDir / 账号池对齐）不用区分"这条是导入的还是登录来的"。
type ImportedCredential struct {
	// FileName 落盘文件名（不含目录）。必须**不含路径分隔符** ——
	// 它会与 AuthDir 拼在一起。空串 = 该条不可落盘（算失败）。
	FileName string
	// Raw 文件内容（上游的序列化结果）。
	Raw []byte
	// UID 账号池主键。空串 = 无法确定账号身份 → 该条算失败。
	UID string
	// Nickname 展示名（可空；空则由核心按 UID 前缀显示）。
	Nickname string
}
