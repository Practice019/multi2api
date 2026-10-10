// util.go 本包的小工具：哈希、日志。
//
// 单独一个文件是为了让 `credential.go` 只讲"凭证是什么"，
// 不与"怎么算哈希"混在一起。
package minimax

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
)

// logf 统一的日志出口。
//
// 走标准库 `log`（与其余上游一致）—— 不引第三方 logger：
// 本仓的直接依赖只有 redis 与 wazero，为几行日志加依赖不值。
func logf(format string, args ...any) {
	log.Printf(format, args...)
}

// sha256Sum 取 sha256 摘要。
func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// hex6 取摘要前 6 字节的 hex（12 个字符）。
//
// 6 字节 = 48 bit：账号池里几千个 token 的碰撞概率可忽略，
// 而 12 个字符在界面上足够短（uid 会被截短显示）。
func hex6(sum []byte) string {
	if len(sum) < 6 {
		return hex.EncodeToString(sum)
	}
	return hex.EncodeToString(sum[:6])
}
