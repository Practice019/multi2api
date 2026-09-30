// pkce.go Qoder 设备码登录的 **PKCE**（RFC 7636）。
//
// # 为什么需要它（本轮修的缺陷）
//
// 本包的登录流程此前**完全没有 PKCE** —— 授权 URL 只有
// `client_id` / `response_type=code` / `state`，轮询也只有一个 `state`。
// 而参照实现（dsh-codearts-auth 的 qoder-oauth.ts + qoder.ts，已实测跑通）
// 是标准的 PKCE 设备码流程：
//
//	授权 URL  {authBase}/device/selectAccounts
//	          ?challenge=<base64url(sha256(verifier))>
//	          &challenge_method=S256
//	          &nonce=<uuid>
//	          &machine_id=<uuid>
//	          &client_id=<product.clientId>
//
//	轮询      GET {openApiBase}/api/v1/deviceToken/poll
//	          ?nonce=<同一个>
//	          &verifier=<明文 verifier>
//	          &challenge_method=S256
//
// 服务端靠 `verifier` 校验授权时提交的 `challenge`。我们不发 PKCE，
// 服务端就没有可校验的东西 —— **授权页能打开、点了授权却永远拿不到 token**，
// 表现为"添加账号"卡住直到 5 分钟超时。这正是用户报的现象。
//
// # 三个必须逐字照抄的细节
//
//  1. verifier 长度 `43 + floor(86*random)`（43..128）—— 源码 `Y_a()`
//  2. challenge = `base64url(sha256(verifier))` 且**去掉 padding**
//     —— 带 `=` 会让服务端校验失败
//  3. 轮询用 **GET + query 参数**，不是 POST + body
//
// 有测试逐条钉住这三点（pkce_test.go）。
package qoder

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/big"
)

// pkceAlphabet PKCE verifier 的字符集。
//
// RFC 7636 的 unreserved 集合（ALPHA / DIGIT / "-" / "." / "_" / "~"，共 66 个），
// 与参照实现的 `PKCE_ALPHABET` 逐字一致。
const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// Pkce 一次 PKCE 生成的结果。
type Pkce struct {
	// Verifier 明文 verifier（轮询时提交给服务端）。
	Verifier string
	// Challenge base64url(sha256(verifier))，**无 padding**。
	Challenge string
}

// newPkce 生成一对 PKCE verifier / challenge。
//
// # 长度为什么是随机的（43..128）
//
// 源码 `Y_a()` 取 `43 + floor(86 * random)`。虽然 RFC 只要求 43..128，
// 但**照抄源码的分布**更安全：服务端若对长度做了统计性检查
// （例如拒绝固定长度的 verifier），随机长度才不会踩雷。
//
// 熵来源是 crypto/rand（不是 math/rand）—— verifier 是一次性密钥，
// 可预测的 verifier 等于把授权码交给攻击者。
func newPkce() (Pkce, error) {
	// 43 + floor(86*random) —— 用 crypto/rand 取 [0,86)
	n, err := rand.Int(rand.Reader, big.NewInt(86))
	if err != nil {
		return Pkce{}, err
	}
	length := 43 + int(n.Int64())

	// 逐字节取模选字符（与参照的 `bytes[i] % ALPHABET.length` 同法）。
	//
	// ⚠ 模偏差在这里是可接受的：参照实现就是这么做，而我们**必须与它
	// 行为一致**（这是协议实现，不是密码学库）。用 rejection sampling
	// 会让 verifier 的字符分布与参照不同 —— 服务端不该能区分两者。
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return Pkce{}, err
	}
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = pkceAlphabet[int(b)%len(pkceAlphabet)]
	}
	verifier := string(out)
	return Pkce{Verifier: verifier, Challenge: pkceChallenge(verifier)}, nil
}

// pkceChallenge 算 `base64url(sha256(verifier))`，**去掉 padding**。
//
// ⚠ padding 必须去掉：`base64.URLEncoding` 会补 `=`，
// 而服务端比对的是无 padding 的形态。带 `=` 的表现是
// "授权页正常、点击授权后报参数无效" —— 极难从现象定位到这一行。
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomNonce 生成一次性 nonce（16 字节 hex，与既有 randomState 同形）。
func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
