// interfaces_compile_test.go **编译期**自检：本包必须满足核心会断言的全部接口。
//
// # 为什么必须有这个文件
//
// 本仓的扩展点发现是**纯类型断言**：
//
//	if lf, ok := gateway.ExtOf[gateway.LoginFlow](p); ok { … }
//
// 所以"实现了方法"与"核心能发现它"是两件事 —— 方法名差一个字母、
// 返回值多一个、参数类型不同，都会让断言**静默失败**，而且**编译能过**。
//
// 症状全都极难定位：
//
//	LoginFlow 认不出 → 界面没有「＋ 添加账号」按钮，看起来像上游没这功能
//	CredentialLoader 认不出 → 登录后凭证落盘了但账号不入池
//	AccountColumnsExt 认不出 → 账号池少几列，界面不报任何错
//
// zcode 那一轮**连续撞了三次**同类问题（其中一次还是我已经在 raccoon 上
// 踩过并记录过的），三次都是用户点下去才暴露的。所以这里的判据是
// **把运行时的断言提前到编译期**：零运行成本，覆盖"忘写某个方法"这个
// 最常见的错误。
package minimax

import (
	"workbuddy2api/internal/gateway"
)

// 核心通过 ExtOf 发现本上游的全部接口。
var (
	// Provider 四方法（必需）。
	_ gateway.Provider = (*Provider)(nil)

	// 登录流程 —— 缺了会让「＋ 添加账号」按钮不出现。
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

	// 导入与错误分类。
	_ gateway.AccountImportExt = (*Provider)(nil)
	// SoftRateExt：本上游的限流信息不在 body 的结构化字段里，
	// 实现恒返回 ok=false（让核心走保守退避）—— 但**必须实现**，
	// 否则核心不知道"这个上游不提供具体重置时刻"。
	_ gateway.SoftRateExt = (*Provider)(nil)

	// 每日动作与签到。
	_ gateway.DailyActionExt  = (*Provider)(nil)
	_ gateway.DailyCheckinExt = (*Provider)(nil)

	// 界面提醒（可选，但本上游要提醒"未经真实对话验证"）。
	_ gateway.NoticeExt = (*Provider)(nil)
)

// authFile 必须能序列化成凭证文件（核心 pollViaFlow 的落盘判据）。
//
// ⚠ 这条断言是**两轮事故的直接补丁**：我最初在 raccoon 与 zcode 上
// 都把裸 `*Auth` 当 Secret 交出去，于是"浏览器授权成功、然后停在
// 501「该上游的凭证结构尚未接入落盘」"。把生产判据写成编译期断言，
// 就不可能再"守错契约"。
var _ interface {
	MarshalAuthFile() (name string, raw []byte, err error)
} = (*authFile)(nil)
