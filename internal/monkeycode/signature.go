// signature.go `X-OhMyAgent-Signature` —— MonkeyCode 模型网关的请求签名。
//
// # 公式（逆向结论，实测验证）
//
//	sig = "v1=" + lowerHex(HMAC-SHA256(key=signing_secret, msg=instructions))
//
// `instructions` 就是请求里的**静态 system prompt**：
//
//	POST /v1/responses  → 顶层 `instructions` 字段（字符串）
//	POST /v1/messages   → 顶层 `system` 字段（字符串形态）
//
// # 为什么不是"对 body 签名"
//
// 逆向时我在这上面绕了很久，留档避免重走：签名**只覆盖那段静态提示词**，
// 与 `input` / `tools` / `model` / `stream` / `api_key` **全部无关**。
//
// 三个决定性证据：
//
//  1. 同一 instructions + 不同 input → 签名**完全相同**
//  2. 同一 instructions 隔数小时再发 → 签名**完全相同**（⇒ 无时间戳/nonce）
//  3. 改 signing_secret → 签名变；改 api_key → 签名不变
//
// 客户端二进制内嵌文档的原话（`docs` 段即可搜到）：
//
//	「它从已有提示词中拆出不依赖会话运行态的通用指令作为第一条，使用
//	 signing_secret 对其计算 HMAC-SHA256，并通过 X-OhMyAgent-Signature 发送；
//	 Task/Todo 模式、工具、技能、AGENTS.md、项目、环境、Git 和模型信息等
//	 动态内容放在后续 system/developer 条目中，不参与签名。」
//
// ⇒ 所以**不需要**模仿官方的 5170 字节提示词：自己写一段，对自己那段签名即可。
//
// # 唯一会踩的坑
//
// 签名对象必须与**实际发出去的那段字符串逐字相同**。三个不许：
// 不许 trim、不许补换行、不许在两个地方各写一份常量。
// 所以本包把提示词收敛成 `defaultInstructions` 常量，签名与请求都引用它 ——
// 漂移一次就是 403 `invalid ohmyagent request`，而那个报错不会告诉你差的是哪个字节。
package monkeycode

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// signaturePrefix 签名头的值前缀。
const signaturePrefix = "v1="

// signatureHeader 签名头的名字（大小写不敏感，这里按官方写法）。
const signatureHeader = "X-OhMyAgent-Signature"

// Sign 计算签名头的值（含 `v1=` 前缀）。
//
// instructions 必须与请求体里那段字符串**逐字一致**。
func Sign(signingSecret, instructions string) string {
	return signaturePrefix + SignHex(signingSecret, instructions)
}

// SignHex 计算不带前缀的 64 位小写十六进制。
func SignHex(signingSecret, instructions string) string {
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte(instructions))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature 校验一个签名头的值是否匹配（测试与自检用）。
func VerifySignature(signingSecret, instructions, header string) bool {
	want := Sign(signingSecret, instructions)
	// 定长比较走 hmac.Equal，避免时序泄漏（虽然这里是本地自检，仍照规范写）。
	return hmac.Equal([]byte(want), []byte(strings.TrimSpace(header)))
}
