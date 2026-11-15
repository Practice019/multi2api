// compat.go 本地数据结构的**版本兼容判据**（U-10）。
//
// # 它防的是什么
//
// 计划 §5 U-10 的验收写着：「旧版本不误读新组；**混合版本部署默认阻止**，
// 不能带病兼容」。
//
// 改造前没有任何本地 schema 版本标记 —— 渠道表、分组表、令牌表都只是"字段"。
// 于是**回退二进制**或**滚动发布中途**会出现：
//
//	新二进制写了一行 kind=builtin + provider_id=xxx 的渠道
//	旧二进制读到它，不认识这俩字段 ⇒ 当成 Kind="" 的普通渠道
//	  ⇒ 它会拿一个空凭据去转发，或者干脆跳过这条渠道
//	⇒ 表现是"额度没变但就是不走那条"，日志里毫无异常
//
// 这类问题**没有运行期信号**，所以只能在启动时**拒绝**。
//
// # 判据形态
//
// 盘上带一个整数版本，二进制带一个它支持的版本。三者只可能是：
//
//	盘上 = 0            从未打过标（引入本机制之前的库）⇒ 按 v1 处理并打标
//	盘上 = 二进制版本     ok
//	盘上 < 二进制版本    数据太旧 ⇒ 需要迁移（只读可，写入要显式确认）
//	盘上 > 二进制版本    ★ 数据太新 ⇒ **拒绝启动**（旧二进制不能读新数据）
//
// 最后一条是这套东西的**全部意义**：宁可起不来，也不要带病跑。
package channelstore

import "fmt"

// LocalSchemaVersion 本二进制支持的本地数据结构版本。
//
// ⚠ 改它必须同时改**读写两侧**的兼容逻辑，不是"加个数字"：
//
//	v1  引入 kind / provider_id / ModelMap 的稳定形态
//	v2  引入 policy_version=2 的分组与令牌解释（组候选 ∩ 白名单）
const LocalSchemaVersion = 2

// 兼容判定的三种结论。
const (
	// CompatOK 版本一致，正常读写。
	CompatOK = "ok"
	// CompatNeedMigration 盘上比二进制旧 —— 数据要迁移才能用新语义写。
	CompatNeedMigration = "need_migration"
	// CompatBinaryTooOld ★ 盘上比二进制新 —— **必须拒绝启动**。
	CompatBinaryTooOld = "binary_too_old"
	// CompatUnmarked 盘上没有版本标记（引入本机制之前的库）。
	CompatUnmarked = "unmarked"
)

// DataCompatibility 一次兼容判定的结论。
type DataCompatibility struct {
	OnDiskVersion int    `json:"on_disk_version"`
	BinaryVersion int    `json:"binary_version"`
	Verdict       string `json:"verdict"`
	Reason        string `json:"reason,omitempty"`
}

// OK 是否可以正常读写。
func (c DataCompatibility) OK() bool { return c.Verdict == CompatOK || c.Verdict == CompatUnmarked }

// MustBlockStartup ★ 是否**必须拒绝启动**。
//
// 只有一种情况：盘上的数据比本二进制新。
// 别的结论都能继续（只读或迁移），只有它不能 —— 因为旧代码不认识新字段，
// 而"不认识"会被静默当成"没设置"，那是数据损坏级的错。
func (c DataCompatibility) MustBlockStartup() bool { return c.Verdict == CompatBinaryTooOld }

// Error 把"必须拒绝启动"变成一个可返回的错误（其余情况返回 nil）。
//
// 用 error 而不是 bool：调用方在 main 里 `if err := ...; err != nil { log.Fatal }`
// 是最难被顺手绕过的形态。
func (c DataCompatibility) Error() error {
	if !c.MustBlockStartup() {
		return nil
	}
	return fmt.Errorf("本地数据版本 %d 比本二进制支持的 %d 新：拒绝启动。"+
		"低版本二进制不认识新字段，会把它们静默当成未设置 —— "+
		"那是数据损坏级的错，不能用'先起来再说'绕过", c.OnDiskVersion, c.BinaryVersion)
}

// CheckCompatibility 判定盘上版本与二进制版本的兼容性。
//
// `unknown` 参数为 true 时当作"没有标记"（例如存储里根本没有版本字段）。
func CheckCompatibility(onDisk int, unknown bool) DataCompatibility {
	out := DataCompatibility{OnDiskVersion: onDisk, BinaryVersion: LocalSchemaVersion}
	switch {
	case unknown || onDisk == 0:
		out.Verdict = CompatUnmarked
		out.Reason = "存储里没有版本标记（引入本机制之前的库），按 v1 处理并需要打标"
	case onDisk == LocalSchemaVersion:
		out.Verdict = CompatOK
	case onDisk < LocalSchemaVersion:
		out.Verdict = CompatNeedMigration
		out.Reason = fmt.Sprintf("盘上是 v%d，本二进制是 v%d：新语义可以读旧数据，但不能直接写 —— 先跑迁移",
			onDisk, LocalSchemaVersion)
	default:
		out.Verdict = CompatBinaryTooOld
		out.Reason = fmt.Sprintf("盘上是 v%d，本二进制只支持到 v%d：低版本二进制会静默忽略新字段", onDisk, LocalSchemaVersion)
	}
	return out
}

// NextVersionOnWrite 写入时该把版本标成多少。
//
// ⚠ 只在**迁移完成**之后才该升到 LocalSchemaVersion：在迁移中间态标成新版本，
// 会让"迁移跑了一半"这件事变得不可见（下次启动看到版本对得上，就以为干净）。
func NextVersionOnWrite() int { return LocalSchemaVersion }
