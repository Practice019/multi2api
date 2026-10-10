// uuid.go 追踪头用的 v4 UUID。
//
// # 为什么这个包里也要有一份（而不是复用 zcode 的）
//
// 它原来只在 `internal/zcode`（`headers.go` 的 `randomUUID`）—— 那是本仓
// **第一个** Anthropic 协议上游。抽 `anthroconv` 时转换层把它一起带了过来，
// 因为转换层里 4 处要给上游追踪头造 id（`x-request-id` 之类）。
//
// 上游包之间**不得互相依赖**（架构约束：每个上游只依赖 gateway），
// 所以不能 import zcode 拿这个函数。而这个包更不能依赖 gateway ——
// 它是纯协议转换，不认识任何上游。
//
// ⚠ 两份实现是**刻意**的，不是漏抽：真要把 zcode 那份也换成这里，
// 会牵动 `headers.go` 的头构造（那是 zcode 专属的指纹逻辑，
// 与协议转换无关）。判据：**纯函数重复可以接受，逻辑分叉不可以** ——
// 这里是 16 字节随机数 + 版本位，没有可分的叉。
package anthroconv

import (
	"crypto/rand"
	"fmt"
)

// randomUUID 生成一个 v4 UUID（追踪头用）。
//
// 用 crypto/rand 而不是 math/rand：追踪头会进上游日志，
// 可预测的值在风控上是个信号（也便于上游把我们的请求串起来）。
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退回一个固定串也比 panic 好 ——
		// 追踪头不是正确性所需，缺了不该让对话失败。
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
