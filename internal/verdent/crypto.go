// crypto.go Verdent 请求体的 AES-256-GCM 加密（逆向成果的 Go 移植）。
//
// # 协议（实测推导，来源 register-machine 的 sources/verdent/verdent-client.mjs）
//
//	key      = asciiBytes( base64( "codeck502deck_25_09_15v7" ) )[:32]
//	密文      = base64( nonce[12] ++ ciphertext ++ tag[16] )，AES-256-GCM
//
// 密钥不是随机的、也不是从 token 推的 —— 它是**硬编码在桌面客户端里的**一段
// 常量字符串，先 base64 再把那段 base64 文本当字节取前 32 位。实测推导出来的，
// 写死在客户端二进制里（与 `internal/monkeycode/signature.go` 的签名是同一种情况：
// 上游把"防裸调"做成了客户端内嵌常量，而不是真的服务端密钥）。
//
// # 加密的是哪两个字段
//
// 只有 `system` 与 `messages`。其余字段（model / session_id / env / ...）是明文。
// 两个字段各自**独立加密**（各自一个 nonce），不是一个整体密文。
//
// ⚠ 明文是 **JSON 字符串本身**（`[{...}]`），不是整个请求体。
//
//	把请求体整体加密会得到一个上游解不开的 body —— 表现是 400，而不是"没加密"。
package verdent

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// proxySign 客户端内嵌的加密常量（逆向所得，见文件头）。
const proxySign = "codeck502deck_25_09_15v7"

// proxyKey 由 proxySign 推导出的 32 字节 AES-256 密钥。
//
// ⚠ 推导是"base64 之后再当字节用"，不是"解码这段 base64"。
//
//	  两者只差一个 Encode/Decode —— 而写错的表现是**每一次请求都 400**，
//	  且错误信息里不会提到密钥。所以这里把中间态也写出来：
//
//		base64("codeck502deck_25_09_15v7") = "Y29kZWNrNTAyZGVja18yNV8wOV8xNXY3"（32 字符）
//		key = 这 32 个 ASCII 字节本身
var proxyKey = func() []byte {
	b := []byte(base64.StdEncoding.EncodeToString([]byte(proxySign)))
	if len(b) < 32 {
		// 到不了：常量固定，上面那条等式是编译期事实。留着是为了让"有人改了
		// proxySign 却不知道长度有约束"这件事**在启动时就炸**，而不是运行到
		// 第一次对话才以 400 的形式暴露。
		panic(fmt.Sprintf("verdent: proxySign 推导出的密钥只有 %d 字节，需要 32", len(b)))
	}
	return b[:32]
}()

// ProxyEncode 把一段值编码成上游要的密文串。
//
// data 会被 JSON 序列化后加密（对应 JS 的 `JSON.stringify(data)`）。
// 调用方传的应当是数组/对象这类**会被上游当 JSON 解析**的值 ——
// 传一个裸字符串进去会得到 `"abc"`（带引号的 JSON），上游解出来是个字符串，
// 于是它不是校验失败而是"模型不看你的话"这种静默错误。
func ProxyEncode(data any) (string, error) {
	pt, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("verdent: 序列化待加密字段失败: %w", err)
	}
	block, err := aes.NewCipher(proxyKey)
	if err != nil {
		return "", fmt.Errorf("verdent: 构造 AES: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("verdent: 构造 GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize()) // 12 字节
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("verdent: 取随机 nonce 失败: %w", err)
	}
	// Seal 的返回是 ciphertext++tag，正好符合 nonce++ciphertext++tag 的拼法。
	sealed := gcm.Seal(nil, nonce, pt, nil)
	out := make([]byte, 0, len(nonce)+len(sealed))
	out = append(out, nonce...)
	out = append(out, sealed...)
	return base64.StdEncoding.EncodeToString(out), nil
}

// ProxyDecode 解密（自验与排障用，生产路径不需要）。
func ProxyDecode(b64 string) (any, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("verdent: base64 解码失败: %w", err)
	}
	block, err := aes.NewCipher(proxyKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	n := gcm.NonceSize()
	if len(raw) < n+gcm.Overhead() {
		return nil, fmt.Errorf("verdent: 密文长度 %d 不足（至少要 %d）", len(raw), n+gcm.Overhead())
	}
	nonce, ct := raw[:n], raw[n:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("verdent: 解密失败（密钥或密文不对）: %w", err)
	}
	var v any
	if err := json.Unmarshal(pt, &v); err != nil {
		return nil, fmt.Errorf("verdent: 解密后不是 JSON: %w", err)
	}
	return v, nil
}
