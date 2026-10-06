// interfaces_compile_test.go **编译期**自检：本包必须满足核心会断言的全部接口。
//
// # 为什么需要它（这一轮连撞三个 501 的根因）
//
// 本仓的扩展点发现是**纯类型断言**：
//
//	if lf, ok := gateway.ExtOf[gateway.LoginFlow](p); ok { ... }
//
// 所以"实现了方法"与"核心能发现它"是两件事 —— 方法名差一个字母、
// 返回值多一个、参数类型不同，都会让断言**静默失败**，而且**编译能过**。
//
// 这一轮我在 zcode 上连续撞了三次同一类问题：
//
//	① Secret 没实现 MarshalAuthFile   → 501「凭证结构尚未接入落盘」
//	② 没实现 CredentialLoader          → 501「无法重扫凭证」
//	③ 账号列回中文标题而非规范 id       → 前端静默跳过那两列
//
// 三次都是**用户点下去才暴露**的（①②在授权成功之后、③只在运行时日志里）。
//
// # 这个文件怎么守住它们
//
// `var _ Interface = (*Provider)(nil)` 是**编译期**断言：
// 方法名/签名不匹配时**直接编译失败**，而不是等到运行时静默跳过。
// 换句话说：把"运行时才发现的接口不匹配"提前成"编译不过"。
//
// ⚠ 它不能替代行为测试（接口满足 ≠ 行为正确），但它是**最便宜的第一道门**：
// 零运行成本，且覆盖"忘写某个方法"这个最常见的错误。
package zcode

import (
	"workbuddy2api/internal/gateway"
)

// 核心通过 ExtOf 发现本上游的全部接口。
//
// 加新扩展点时必须往这里加一行 —— 忘加不会导致失败，但会让本文件
// 失去对那个接口的保护（所以它与 provider.go 的方法列表要一起看）。
var (
	// Provider 四方法（必需）。
	_ gateway.Provider = (*Provider)(nil)

	// 登录流程 —— 缺了会让「＋ 添加账号」按钮不出现（用户报过的障）。
	_ gateway.LoginFlow = (*Provider)(nil)

	// 落盘链路的两个必需点 —— 缺任一都会让登录以 501 失败。
	_ gateway.CredentialLoader       = (*Provider)(nil)
	_ gateway.CredentialSecretLoader = (*Provider)(nil)

	// 账号池呈现。
	_ gateway.AuthDirExt        = (*Provider)(nil)
	_ gateway.DisplayNameExt    = (*Provider)(nil)
	_ gateway.AccountColumnsExt = (*Provider)(nil)

	// 凭证生命周期（决定"该不该续期"，判错会让每个请求都续期）。
	_ gateway.CredentialTokenExt    = (*Provider)(nil)
	_ gateway.CredentialExpiryExt   = (*Provider)(nil)
	_ gateway.CredentialLifetimeExt = (*Provider)(nil)
	_ gateway.RefreshSkewExt        = (*Provider)(nil)

	// 运维端点与健康。
	_ gateway.AdminExt       = (*Provider)(nil)
	_ gateway.QuotaExt       = (*Provider)(nil)
	_ gateway.HealthProbeExt = (*Provider)(nil)

	// 界面提醒（用户要求"Zcode 那个地方用小字提醒一下，这个是测试"）。
	//
	// 缺了它 → manifest 里没有 notice 字段 → 前端不显示提醒。
	// ⚠ 而前端**不会报错**（读不到字段就不渲染），所以这类缺失
	// 只能靠编译期断言拦 —— 这正是本文件存在的理由。
	_ gateway.NoticeExt = (*Provider)(nil)

	// 导入与错误分类。
	_ gateway.AccountImportExt = (*Provider)(nil)
	// SoftRateExt 是**本上游最要紧的一个** —— 它的失败可能是
	// HTTP 200 + 业务码，不看业务码会把风控记成成功（本仓在 TRAE 上踩过）。
	_ gateway.SoftRateExt = (*Provider)(nil)
)

// authFile 必须能序列化成凭证文件（核心 pollViaFlow 的落盘判据）。
//
// ⚠ 这条断言是**这一轮事故的直接补丁**：我最初的测试断言的是
// `Secret.(*Auth)`，而生产的判据是 `Secret.(authFileWriter)` ——
// 两者不一致，于是测试全绿、登录卡在 501。
// 把生产判据写成编译期断言，就不可能再"守错契约"。
var _ interface {
	MarshalAuthFile() (name string, raw []byte, err error)
} = (*authFile)(nil)
