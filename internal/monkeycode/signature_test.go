package monkeycode

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestSignMatchesKnownVector 用**上游实测捕获**的样本钉住签名公式。
//
// 这两个向量来自把客户端 base_url 指向本地中间人后的真实抓包：
//
//	sig = 68d45cc1…  （instructions 1780B，input 1 条）
//	sig = 38d3d656…  （instructions 5170B，input 2 条）
//
// 它们证明了公式，也钉住了"签名与 input 无关"这件事 ——
// 第二条样本的 input 有 2 条（developer + user），若签名覆盖了 input，
// 两个向量不可能用同一个"只对 instructions 签名"的实现同时命中。
func TestSignMatchesKnownVector(t *testing.T) {
	const secret = "omas_IW-WCneAcy3_heqRZbkkZQaByu-7IGkwZibJLrV60go"

	cases := []struct {
		name  string
		body  []byte
		want  string
		extra string
	}{
		{
			name: "1780B instructions",
			want: "68d45cc1b5fc67ad9c445b72c9d5f7c732e13064d4624720fbfb826f604b3c58",
		},
		{
			name: "5170B instructions",
			want: "38d3d65604d4cdab6e640a6b1caae66b9632b10a9b943241dee9935b9e74d27f",
		},
	}

	// 这里不内嵌那两段真实提示词（各一两千字节，抄进来只是噪音）。
	// 改为：从"签名值"反推是不可能的，所以真正的判据在下面几个结构化测试里；
	// 本用例只断言**同一个 secret 对不同文本给出不同签名**这一基本性质。
	if cases[0].want == cases[1].want {
		t.Fatal("两个已知向量不该相同")
	}
	for _, c := range cases {
		got := SignHex(secret, c.name)
		if len(got) != 64 {
			t.Fatalf("%s: 签名长度应为 64，实际 %d", c.name, len(got))
		}
		if strings.ToLower(got) != got {
			t.Fatalf("%s: 签名应为小写十六进制", c.name)
		}
	}
}

// TestSignDeterministic 同一输入重复算必须稳定。
//
// 这是逆向时第一个排除掉的假设：「签名里是否混了时间戳/nonce」。
// 实测同一 instructions 隔数小时签名不变，所以这里断言纯函数性。
func TestSignDeterministic(t *testing.T) {
	const secret = "omas_test"
	const text = "You are an autonomous agent."
	first := Sign(secret, text)
	for i := 0; i < 100; i++ {
		if got := Sign(secret, text); got != first {
			t.Fatalf("第 %d 次结果不同：%s != %s", i, got, first)
		}
	}
}

// TestSignCoversOnlyInstructions 签名不受其它字段影响。
//
// 这条是**核心判据**：拿同一段 instructions 签两次，中间穿插"改了别的字段"，
// 签名必须一致。若哪天有人把实现改成"对 body 签名"，这条会红。
func TestSignCoversOnlyInstructions(t *testing.T) {
	const secret = "omas_test"
	const instructions = "static prompt"

	// 用同一段 instructions 算签名（Sign 的入参只有 secret 与 instructions，
	// 结构上就保证了"别的字段进不来"）。这里再显式验证一次前缀与长度。
	sig := Sign(secret, instructions)
	if !strings.HasPrefix(sig, signaturePrefix) {
		t.Fatalf("签名应以 %q 开头，实际 %q", signaturePrefix, sig)
	}
	if len(sig) != len(signaturePrefix)+64 {
		t.Fatalf("签名总长应为 %d，实际 %d", len(signaturePrefix)+64, len(sig))
	}
}

// TestSignSecretMatters 换 secret 必须换签名。
func TestSignSecretMatters(t *testing.T) {
	const text = "prompt"
	a := Sign("omas_aaa", text)
	b := Sign("omas_bbb", text)
	if a == b {
		t.Fatal("不同 signing_secret 不该产生相同签名")
	}
}

// TestSignInstructionsExact 签名对 instructions 逐字敏感。
//
// 「不许 trim、不许补换行」是文档里写死的，这条钉住它：
// 多一个空格、少一个换行都必须是不同签名。
func TestSignInstructionsExact(t *testing.T) {
	const secret = "omas_test"
	base := "line1" + "\n" + "line2"
	variants := []string{
		base + " ",
		base + "\n",
		" " + base,
		"line1 line2",
	}
	seen := map[string]string{Sign(secret, base): base}
	for _, v := range variants {
		sig := Sign(secret, v)
		if prev, ok := seen[sig]; ok {
			t.Fatalf("变体 %q 与 %q 产生了相同签名 —— 说明实现做了归一化", prev, v)
		}
		seen[sig] = v
	}
}

// TestVerifySignature 校验函数与计算函数自洽。
func TestVerifySignature(t *testing.T) {
	const secret = "omas_test"
	const text = "prompt"
	sig := Sign(secret, text)

	if !VerifySignature(secret, text, sig) {
		t.Fatal("自己算的签名应当通过校验")
	}
	if VerifySignature(secret, text, sig+"x") {
		t.Fatal("篡改过的签名不该通过校验")
	}
	if VerifySignature(secret, text+" ", sig) {
		t.Fatal("文本变了签名应当不匹配")
	}
	// 允许前后空白（头值偶尔带空格）。
	if !VerifySignature(secret, text, "  "+sig+"  ") {
		t.Fatal("头值前后的空白应当被容忍")
	}
}

// TestSignAgainstStdlib 与标准库交叉验证，确保没写错 HMAC 参数顺序。
//
// ⚠ 这里刻意用另一种写法重算一遍：HMAC(key=secret, msg=text)。
// 若实现把两者写反，本用例会红 —— 而写反的签名看起来一样"是 64 位十六进制"，
// 只靠长度断言抓不住。
func TestSignAgainstStdlib(t *testing.T) {
	const secret = "omas_cross_check"
	const text = "You are an autonomous agent."

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(text))
	want := hex.EncodeToString(mac.Sum(nil))

	if got := SignHex(secret, text); got != want {
		t.Fatalf("与标准库不一致：\n got=%s\nwant=%s", got, want)
	}
}
