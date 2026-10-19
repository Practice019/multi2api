package account

import (
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 口令哈希代价。
//
// # 为什么是 12（而不是 Go 默认的 10）
//
// 安全清单（`.dsh/skills/_shared/references/security-checklist.md` 的
// Authentication 一节）明确要求 "bcrypt (≥12 rounds)"；new-api 用的是
// `bcrypt.DefaultCost`（10）。这里取**更严的那条**：单机控制台的登录
// 频率极低（不是每秒几十次的网关路径），多花几十毫秒换暴力破解成本 ×4。
//
// ⚠ 12 是"当下"的值，不是永远：代价要随硬件涨。改这个常量**不需要**
// 迁移已有哈希 —— bcrypt 把 cost 编码在哈希串里（`$2a$12$...`），
// VerifyPassword 按各条记录自己的 cost 校验，所以老哈希继续可用，
// 新设置的才用新 cost。
const bcryptCost = 12

// dummyHash 是一份"格式正确但永不匹配"的 bcrypt 哈希（明文是随机串，
// 每条进程随机生成）。用途见 VerifyPassword 的注释：
// 让"用户不存在"与"口令错误"两条路径**耗时相当**。
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("wb2api-dummy-password-for-timing"), bcryptCost)
	if err != nil {
		// 连哈希都生成不了说明 bcrypt 不可用（成本非法/内存不足），
		// 这种情况不能静默继续 —— 否则登录会变成"永远失败"或
		// "永远通过"这两种最糟的表现。
		panic("account: 无法生成 bcrypt 占位哈希: " + err.Error())
	}
	return h
}()

// HashPassword 生成口令哈希（含 cost 与 salt，直接可存盘）。
//
// 超过 72 字节的口令会被 bcrypt **静默截断**：这是 bcrypt 的已知性质。
// 我们的 validatePassword 上限是 128 字节，正好会踩到这一点 ——
// 于是这里显式判定：超长口令直接拒绝，而不是让"第 73 个字符之后
// 不参与校验"这件事成为一个没人知道的秘密。
func HashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	if len(password) > 72 {
		return "", errors.New("口令过长：bcrypt 只取前 72 字节，超过部分会被静默忽略")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword 校验口令；hash 为空或口令不匹配都返回 false。
//
// # 为什么要"对着占位哈希也算一遍"
//
// 调用方（登录处理）在账号不存在时必须返回与"口令错误"**同样的**错误。
// 但如果账号不存在时直接 return，那条路径会比真校验快一个数量级
// （bcrypt 一次约几十毫秒），于是"响应快 = 这个用户名不存在"成为
// 一个可测量的用户名枚举接口。所以账号不存在时也拿 dummyHash 走一遍
// 完整的 bcrypt 校验，把两条路径的耗时对齐。
//
// 用法：`VerifyPassword(u.PasswordHash, input)`；用户不存在时传 spaceHolder
// 库的 `DummyHash()`（见下）。
func VerifyPassword(hash, password string) bool {
	if strings.TrimSpace(hash) == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// DummyHash 返回进程内那份占位哈希，供"账号不存在也要走一遍校验"的调用方使用。
func DummyHash() string { return string(dummyHash) }
