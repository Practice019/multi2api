// role.go 角色档位与建库版本戳 —— 两个**跨切片共享**的包级符号。
//
// # 为什么单独一个文件，而不是塞进 models.go
//
// 它们不是某张表的结构，而是"谁在用"的约定：setup.go 建 root 用户时读
// RoleRoot，写 setups 行时读 Version。放在独立文件里，改动面一眼可辨，
// 也避免"整文件重写模型定义"时把它们顺手删掉 —— 那已经发生过一次
// （2026-10-26：重写 models.go 丢了这两个符号，store 包不编译，阻塞了 3 个切片）。
//
// # 为什么 RoleRoot 是新加的一档，而不是复用 RoleAdmin
//
// PLAN-sql-unify §0.1 要求角色表对齐参考项目的三档。本仓既有两档
// （internal/account/account.go:62-64 的 RoleMember=1 / RoleAdmin=10），
// **数值不许动**：它们已经写进了 data/accounts.json，改了会让旧数据
// 在迁移后被读成别的身份（RoleMember 是"认出来再拒掉"的判据，错位就变成放行）。
//
// 所以这里**只追加第三档**，且刻意不复用 10：root 与 admin 的区别是
// "能否管理其他管理员"，把它压进同一档会让那条权限再也无法表达。
package store

// RoleMember / RoleAdmin 的说明见上面包注释。这里不重新定义它们（那会与
// internal/account 形成两份定义）。store 层只新增自己需要的那一档。
const (
	// RoleRoot 超管。数值对齐参考项目（new-api 与 hyxapi 都是 100）。
	//
	// # 为什么必须是 "root" 这个具体档位，而不能"admin 就够用"
	//
	// setup.go 的自愈判据是"有 RoleRoot 用户但没有 setups 行 ⇒ 补行"。
	// 若用 RoleAdmin 当判据，那么任何一次"先建了个普通管理员、还没走完初始化"
	// 的中间状态都会被误判成"已初始化完成" —— 自愈就变成了掩盖故障。
	RoleRoot = 100
)

// Version 建库时的程序版本戳，写进 setups 行的 version 列。
//
// # 为什么是可变的 var 而不是 const
//
// 它要能被**构建期注入**（-ldflags "-X workbuddy2api/internal/store.Version=v1.2.3"）。
// const 做不到这一点，而"判断这次初始化是哪个版本做的"正是这一列存在的理由。
//
// 默认值 "dev"：本地构建与测试不该假装自己是某个正式版本 —— 看到一个
// 具体的版本号时，人才能相信它是真的。
var Version = "dev"
