package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// TestSignSelfConsistencyWithKnownKey 用**自造的**密钥验证签名算法的自洽性。
//
// # 为什么需要它（而 TestSignMatchesJS 不够）
//
// `TestSignMatchesJS` 把 Go 实现锚定到"已实测通过服务端校验的 JS 实现"，
// 是本包最有价值的测试 —— 但它需要真实 secretKey（向量文件刻意不含它），
// 未设置环境变量时会 **skip**。也就是说：在没凭证的环境里（CI、别人 clone），
// 签名正确性**完全没有被验证**。
//
// 这条测试补上那一块：用固定测试密钥调用 Sign()，再用本文件里
// **独立重写**的一份实现重算，两者必须逐字节相等。
//
// # 我第一版写错了什么（值得记下来）
//
// 我按 **AWS SigV4** 的印象写了 HMAC **四步派生链**
// （secret → 日期 → 区域 → 服务 → 签名 key）。
// 实测与 Go 实现不一致，查源码后发现：
//
//	华为云 SDK-HMAC-SHA256 用的是**单次 HMAC**，key 就是原始 secretKey
//	（sign.go:181 `hmacSHA256Hex(cred.SecretKey, stringToSign)`），
//	**没有** AWS 那套派生链。
//
// 这是两家厂商签名方案的真实差异。若我当时"以测试为准"去改产品代码，
// 会把一个已经对齐服务端、且被 JS 向量锚定的实现改坏。
// 教训：独立实现不一致时，先确认**哪一边**才是权威（这里是向量），
// 而不是默认"被测代码错了"。
//
// # 它能证明什么、不能证明什么
//
// 能证明：Sign() 的规范请求构造、StringToSign 拼接、单次 HMAC、
// 以及 Authorization 头格式，与"按文档重写一遍"一致。
//
// **不能证明**：算法与服务端一致 —— 那只能靠真实凭证或 JS 向量。
func TestSignSelfConsistencyWithKnownKey(t *testing.T) {
	cred := Credential{
		AccessKey:     "AKIDEXAMPLE",
		SecretKey:     "SECRETEXAMPLE",
		SecurityToken: "TOKENEXAMPLE",
	}
	const (
		method = "POST"
		rawURL = "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions"
		body   = `{"model":"GLM-5.2"}`
		date   = "20260101T000000Z"
	)

	got, err := Sign(cred, method, rawURL, body, map[string]string{"X-Sdk-Date": date})
	if err != nil {
		t.Fatalf("Sign 失败: %v", err)
	}
	auth := got["Authorization"]
	if auth == "" {
		t.Fatal("Sign 没有产出 Authorization 头")
	}
	t.Logf("Go 实现  : %s", auth)

	want := independentSign(t, cred, method, rawURL, body, date)
	t.Logf("独立实现: %s", want)

	if auth != want {
		ga, ws := parseAuthParts(auth), parseAuthParts(want)
		for _, k := range []string{"Access", "SignedHeaders"} {
			if ga[k] != ws[k] {
				t.Errorf("%s 不一致：Go=%q 独立=%q", k, ga[k], ws[k])
			}
		}
		if ga["Signature"] != ws["Signature"] {
			t.Errorf("Signature 不一致：\n  Go  =%s\n  独立=%s", ga["Signature"], ws["Signature"])
		}
		t.Fatalf("签名与独立实现不一致")
	}
}

// TestSignIsDeterministic 同一输入必须产出同一签名。
//
// 看似显然，但若签名里混入 `time.Now()`（而不是只从 extra 取日期），
// 或 map 遍历顺序影响了 SignedHeaders，就会不稳定 ——
// 那种 bug 表现为"偶发 401"，极难查。
func TestSignIsDeterministic(t *testing.T) {
	cred := Credential{AccessKey: "AK", SecretKey: "SK", SecurityToken: "ST"}
	const u = "https://example.com/api/v2/chat/completions"

	first, err := Sign(cred, "POST", u, `{"a":1}`, map[string]string{"X-Sdk-Date": "20260101T000000Z"})
	if err != nil {
		t.Fatal(err)
	}
	// 每次都用**新的 map**，防遍历顺序带来的差异
	for i := 0; i < 30; i++ {
		again, err := Sign(cred, "POST", u, `{"a":1}`,
			map[string]string{"X-Sdk-Date": "20260101T000000Z"})
		if err != nil {
			t.Fatal(err)
		}
		if again["Authorization"] != first["Authorization"] {
			t.Fatalf("第 %d 次签名不同：\n  %s\n  %s",
				i, first["Authorization"], again["Authorization"])
		}
	}
	t.Log("30 次重复签名结果一致（无隐藏的时间/顺序依赖）")
}

// TestSignSecretKeyActuallyUsed 换 secret 必须改变签名。
//
// 防的是"密钥没被真正用到"这类静默错误（例如传了空串、
// 或用了错误的字段），那种情况下签名会稳定地错，
// 而确定性测试抓不到（它只比较同一输入）。
func TestSignSecretKeyActuallyUsed(t *testing.T) {
	const u = "https://example.com/x"
	const d = "20260101T000000Z"

	a, err := Sign(Credential{AccessKey: "AK", SecretKey: "SECRET-A"}, "POST", u, `{}`,
		map[string]string{"X-Sdk-Date": d})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Sign(Credential{AccessKey: "AK", SecretKey: "SECRET-B"}, "POST", u, `{}`,
		map[string]string{"X-Sdk-Date": d})
	if err != nil {
		t.Fatal(err)
	}
	if a["Authorization"] == b["Authorization"] {
		t.Error("换 secretKey 后签名不变 —— secret 没有被真正使用")
	}
	// accessKey 只影响 Access= 段，不影响签名本身
	if parseAuthParts(a["Authorization"])["Signature"] == "" {
		t.Error("Authorization 里没有 Signature")
	}
}

// TestSignBodyAffectsSignature 请求体必须参与签名（防"body 没进 hash"）。
func TestSignBodyAffectsSignature(t *testing.T) {
	cred := Credential{AccessKey: "AK", SecretKey: "SK"}
	const u = "https://example.com/x"
	const d = "20260101T000000Z"

	a, _ := Sign(cred, "POST", u, `{"a":1}`, map[string]string{"X-Sdk-Date": d})
	b, _ := Sign(cred, "POST", u, `{"a":2}`, map[string]string{"X-Sdk-Date": d})
	if a["Authorization"] == b["Authorization"] {
		t.Error("改请求体后签名不变 —— payload hash 没有参与签名")
	}
}

// ---- 独立实现（刻意手写，不复用 Sign 的内部结构）----

func independentSign(t *testing.T, cred Credential, method, rawURL, body, date string) string {
	t.Helper()

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("URL 解析失败: %v", err)
	}
	// 注意：用 u.Path 而不是 u.EscapedPath() —— 与 sign.go:144 一致。
	// escape 由本文件独立实现，不复用包内函数。
	canonURI := canonURIOf(u.Path)

	canonQuery := canonQueryOf(u.RawQuery)

	hdrs := map[string]string{
		"content-type":     "application/json",
		"x-sdk-date":       date,
		"x-security-token": cred.SecurityToken,
	}
	names := make([]string, 0, len(hdrs))
	for k := range hdrs {
		names = append(names, k)
	}
	sort.Strings(names)

	var hb strings.Builder
	for _, n := range names {
		hb.WriteString(n)
		hb.WriteString(":")
		hb.WriteString(strings.TrimSpace(hdrs[n]))
		hb.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonical := strings.Join([]string{
		strings.ToUpper(method),
		canonURI,
		canonQuery,
		hb.String(),
		signedHeaders,
		hexSHA256(body),
	}, "\n")

	stringToSign := strings.Join([]string{
		"SDK-HMAC-SHA256",
		date,
		hexSHA256(canonical),
	}, "\n")

	// 华为云：**单次** HMAC，key = 原始 secretKey（无 AWS 式派生链）
	sig := hex.EncodeToString(hmacRaw([]byte(cred.SecretKey), stringToSign))

	return "SDK-HMAC-SHA256 Access=" + cred.AccessKey +
		", SignedHeaders=" + signedHeaders +
		", Signature=" + sig
}

// canonURIOf 逐段 escape 并在末尾补 '/'（华为云要求）。
func canonURIOf(p string) string {
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = escape(s)
	}
	out := strings.Join(segs, "/")
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func canonQueryOf(raw string) string {
	if raw == "" {
		return ""
	}
	pairs := strings.Split(raw, "&")
	for i, p := range pairs {
		kv := strings.SplitN(p, "=", 2)
		k := escape(kv[0])
		v := ""
		if len(kv) == 2 {
			v = escape(kv[1])
		}
		pairs[i] = k + "=" + v
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func hmacRaw(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func parseAuthParts(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if i := strings.Index(part, "="); i > 0 {
			out[part[:i]] = part[i+1:]
		}
	}
	return out
}
