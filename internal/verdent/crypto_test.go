// crypto_test.go 加密层的判据 —— 与**参考实现（JS 客户端）逐字节对齐**。
//
// # 为什么要有"和 JS 对齐"这一条（而不是只测自往返）
//
// 自往返（Go 加密 → Go 解密）在密钥完全推错时**照样通过** —— 两边用的是同一把
// 错的钥匙。而真错的表现是"每一次对话都 400"，且上游的报错不会告诉你差的是哪个字节。
// 所以这里钉两件事：
//
//  1. 推导出来的密钥等于参考实现算出的那 32 字节（硬编码成十六进制常量）；
//  2. 参考实现（固定 nonce）产出的密文，本实现能解开并得到原值。
//
// 第 2 条是真正的**跨语言一致性**：它同时证明了 nonce 长度、GCM tag 位置、
// 拼接顺序（nonce++ciphertext++tag）与 base64 变体全都一致。
package verdent

import (
	"encoding/hex"
	"reflect"
	"testing"
)

// refKeyHex 参考实现（node: crypto.createCipheriv("aes-256-gcm", KEY, nonce)）算出的密钥。
//
// 来源：`Buffer.from(Buffer.from("codeck502deck_25_09_15v7","utf-8").toString("base64"),"utf-8").subarray(0,32)`
// —— 即"把这段常量的 base64 **文本本身**当字节用"，不是解码它。
const refKeyHex = "5932396b5a574e724e5441795a47566a613138794e5638774f5638784e585933"

// refCipher 参考实现用**固定 nonce（12 个 0x07）**对 `[{"type":"text","text":"hi"}]` 加密的结果。
const refCipher = "BwcHBwcHBwcHBwcH/DjZ8MiFwWShIlxXp5AtWD1EapKuREMvQNHc08JvywsfTZL6bx4ygdKPsBJc"

// TestProxyKeyMatchesReference 密钥必须与参考实现逐字节相同。
func TestProxyKeyMatchesReference(t *testing.T) {
	got := hex.EncodeToString(proxyKey)
	if got != refKeyHex {
		t.Errorf("proxyKey 与参考实现不一致：\n  实得 %s\n  期望 %s\n"+
			"（最常错的是把 base64 那一层写成了 Decode —— 要的是 Encode 后把文本当字节）",
			got, refKeyHex)
	}
	if len(proxyKey) != 32 {
		t.Errorf("密钥长度 %d，AES-256 需要 32", len(proxyKey))
	}
}

// TestProxyDecodeReferenceCiphertext 必须能解开参考实现产出的密文。
//
// ⚠ 这条是"跨语言一致"的证明，不是自往返：密文是 JS 那边用固定 nonce 生成的。
func TestProxyDecodeReferenceCiphertext(t *testing.T) {
	got, err := ProxyDecode(refCipher)
	if err != nil {
		t.Fatalf("解不开参考实现的密文（说明拼法/nonce/tag/base64 有一处与 JS 不一致）: %v", err)
	}
	want := []any{map[string]any{"type": "text", "text": "hi"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("解出的值与参考实现不同：\n  实得 %#v\n  期望 %#v", got, want)
	}
}

// TestProxyEncodeRoundTrip 自往返（次要判据，但能抓住"加密了整个 body"这类错）。
func TestProxyEncodeRoundTrip(t *testing.T) {
	for _, v := range []any{
		[]any{map[string]any{"type": "text", "text": "你好"}},
		[]any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "x"}}}},
	} {
		enc, err := ProxyEncode(v)
		if err != nil {
			t.Fatalf("加密失败: %v", err)
		}
		if enc == "" {
			t.Fatal("加密结果为空串")
		}
		got, err := ProxyDecode(enc)
		if err != nil {
			t.Fatalf("解密失败: %v", err)
		}
		if !reflect.DeepEqual(got, v) {
			t.Errorf("往返不一致：%#v → %#v", v, got)
		}
	}
}

// TestProxyEncodeUsesFreshNonce 同一个输入两次加密必须得到不同密文。
//
// ⚠ 这不是形式检查：nonce 复用会让 AES-GCM 的密钥流复用，是密码学上的硬伤
// （同一 nonce 下两段明文的异或可从密文异或推出）。上游不检查，但写错就是留了个洞。
func TestProxyEncodeUsesFreshNonce(t *testing.T) {
	v := []any{map[string]any{"type": "text", "text": "same"}}
	a, err := ProxyEncode(v)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ProxyEncode(v)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("同一明文两次加密得到相同密文 —— nonce 被固定了")
	}
}

// TestProxyDecodeRejectsGarbage 坏输入必须返回 error，不得 panic。
func TestProxyDecodeRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "!!!不是base64", "AAAA", "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXo="} {
		if _, err := ProxyDecode(s); err == nil {
			t.Errorf("ProxyDecode(%q) 没报错 —— 应当拒绝", s)
		}
	}
}
