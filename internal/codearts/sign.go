// Package codearts 提供华为云 CodeArts Agent 的请求签名（SDK-HMAC-SHA256）。
//
// 与 workbuddy2api 的差异：CodeBuddy 用 `Authorization: Bearer <token>`，
// 而 CodeArts 用华为云 SDK 的 AK/SK 签名（`Authorization: SDK-HMAC-SHA256 ...`
// + `x-security-token`）。
//
// 关键坑（已实测踩过）：CanonicalURI 必须**补尾斜杠**。
// 上游回显的 canonical_request 是 `GET|/v5/caller-identity/||...`，
// 注意 `/v5/caller-identity/` 带斜杠；不补则一律 401。
// 原始实现见 extension.js 的 Signer.getCanonicalURI()。
package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Credential 是一次签名所需的凭证三元组。
type Credential struct {
	AccessKey     string
	SecretKey     string
	SecurityToken string // STS 临时令牌；放进 x-security-token 头
}

const (
	algorithm   = "SDK-HMAC-SHA256"
	dateHeader  = "x-sdk-date"
	contentHash = "x-sdk-content-sha256"
)

// escape 按华为云 SDK 的规则做 URI 编码。
// Go 的 url.QueryEscape 会把空格编成 '+'，这里必须用 %20，
// 且需额外转义 !'()* —— 与 JS 的 encodeURIComponent 对齐。
func escape(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	replacer := strings.NewReplacer(
		"!", "%21", "'", "%27", "(", "%28", ")", "%29", "*", "%2A",
	)
	return replacer.Replace(e)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hmacSHA256Hex(key, data string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(data))
	return hex.EncodeToString(m.Sum(nil))
}

// SDKDate 生成 X-Sdk-Date（形如 20260911T111926Z）。
func SDKDate(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// canonicalURI 按华为云 SDK 的规则构造 CanonicalURI：
// 逐段 escape，**且末尾补 '/'**。
//
// 这是本包最容易踩的坑。依据：上游 401 响应会回显它自己算的
// canonical_request，其中 URI 段是 `/v5/caller-identity/`（带尾斜杠），
// 而请求路径是 `/v5/caller-identity`。不补斜杠则签名恒不匹配。
// 对应原始实现 extension.js: Signer.getCanonicalURI()。
func canonicalURI(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = escape(s)
	}
	uri := strings.Join(segs, "/")
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	return uri
}

// Sign 对一个请求签名，返回应附加到请求上的头。
//
// method  GET/POST/...
// rawURL  完整 URL（含 query）
// body    请求体（无则为空串）
// extra   业务头（会被纳入签名）
func Sign(cred Credential, method, rawURL, body string, extra map[string]string) (map[string]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	method = strings.ToUpper(method)

	headers := map[string]string{}
	for k, v := range extra {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		headers[k] = v
	}
	if _, ok := headers["X-Sdk-Date"]; !ok {
		headers["X-Sdk-Date"] = SDKDate(time.Now())
	}
	// 临时凭证必须携带 securityToken，否则 403。
	if cred.SecurityToken != "" {
		if _, ok := headers["X-Security-Token"]; !ok {
			headers["X-Security-Token"] = cred.SecurityToken
		}
	}
	hasBody := method == "PUT" || method == "PATCH" || method == "POST"
	if hasBody {
		if _, ok := headers["Content-Type"]; !ok {
			headers["Content-Type"] = "application/json"
		}
	}

	// ---- SignedHeaders：全部小写、排序 ----
	lower := map[string]string{}
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		lower[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	signedKeys := make([]string, 0, len(lower))
	for k := range lower {
		signedKeys = append(signedKeys, k)
	}
	sort.Strings(signedKeys)
	signedHeaders := strings.Join(signedKeys, ";")

	var canonicalHeaders strings.Builder
	for _, k := range signedKeys {
		canonicalHeaders.WriteString(k)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(lower[k])
		canonicalHeaders.WriteString("\n")
	}

	// ---- CanonicalURI：逐段 escape，且末尾补 '/'（关键！）----
	uri := canonicalURI(u.Path)

	// ---- CanonicalQueryString：key 排序，重复 key 再按值排序 ----
	type kv struct{ k, v string }
	var qs []kv
	for k, vs := range u.Query() {
		for _, v := range vs {
			qs = append(qs, kv{escape(k), escape(v)})
		}
	}
	sort.Slice(qs, func(i, j int) bool {
		if qs[i].k != qs[j].k {
			return qs[i].k < qs[j].k
		}
		return qs[i].v < qs[j].v
	})
	parts := make([]string, len(qs))
	for i, p := range qs {
		parts[i] = p.k + "=" + p.v
	}
	canonicalQuery := strings.Join(parts, "&")

	canonicalRequest := strings.Join([]string{
		method,
		uri,
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders,
		sha256Hex(body),
	}, "\n")

	stringToSign := strings.Join([]string{
		algorithm,
		lower[dateHeader],
		sha256Hex(canonicalRequest),
	}, "\n")

	signature := hmacSHA256Hex(cred.SecretKey, stringToSign)

	out := map[string]string{}
	for k, v := range headers {
		out[k] = v
	}
	out["Authorization"] = fmt.Sprintf(
		"%s Access=%s, SignedHeaders=%s, Signature=%s",
		algorithm, cred.AccessKey, signedHeaders, signature,
	)
	return out, nil
}

// CanonicalRequest 暴露完整的中间量，便于与上游回显的 canonical_request 对照。
//
// 用途：签名 401 时，上游会在 error_msg 里回显它自己算的 canonical_request，
// 用本函数算出本地版本直接 diff，能立刻定位是 URI / query / header 哪一段不一致。
func CanonicalRequest(cred Credential, method, rawURL, body string, extra map[string]string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	method = strings.ToUpper(method)

	headers := map[string]string{}
	for k, v := range extra {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		headers[k] = v
	}
	if _, ok := headers["X-Sdk-Date"]; !ok {
		headers["X-Sdk-Date"] = SDKDate(time.Now())
	}
	if cred.SecurityToken != "" {
		if _, ok := headers["X-Security-Token"]; !ok {
			headers["X-Security-Token"] = cred.SecurityToken
		}
	}
	if method == "PUT" || method == "PATCH" || method == "POST" {
		if _, ok := headers["Content-Type"]; !ok {
			headers["Content-Type"] = "application/json"
		}
	}

	lower := map[string]string{}
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		lower[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	signedKeys := make([]string, 0, len(lower))
	for k := range lower {
		signedKeys = append(signedKeys, k)
	}
	sort.Strings(signedKeys)

	var ch strings.Builder
	for _, k := range signedKeys {
		ch.WriteString(k + ":" + lower[k] + "\n")
	}

	type kv struct{ k, v string }
	var qs []kv
	for k, vs := range u.Query() {
		for _, v := range vs {
			qs = append(qs, kv{escape(k), escape(v)})
		}
	}
	sort.Slice(qs, func(i, j int) bool {
		if qs[i].k != qs[j].k {
			return qs[i].k < qs[j].k
		}
		return qs[i].v < qs[j].v
	})
	parts := make([]string, len(qs))
	for i, p := range qs {
		parts[i] = p.k + "=" + p.v
	}

	return strings.Join([]string{
		method,
		canonicalURI(u.Path),
		strings.Join(parts, "&"),
		ch.String(),
		strings.Join(signedKeys, ";"),
		sha256Hex(body),
	}, "\n"), nil
}
