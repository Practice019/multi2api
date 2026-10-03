// imagedisplay.go 把上游给的图片 URL 转成"下游真的能显示"的形式。
//
// # 为什么需要它（这是用户报的"图片无法预览"的根因）
//
// 上游返回的是**腾讯 COS 签名 URL**，形如：
//
//	https://bucket.cos.ap-guangzhou.myqcloud.com/…/x.png
//	  ?q-sign-algorithm=sha1&q-ak=AKID…&q-sign-time=1791011698;1791054908
//	  &q-key-time=…&q-header-list=host&q-url-param-list=&q-signature=907b5d…
//
// 实测（10/10 复现，不是偶发）：
//
//	带签名 URL  GET → 200 ✓     HEAD → **403 ✗**
//	裸 URL       GET → 200 ✓     HEAD → 200 ✓
//
// **HEAD 必然失败**。原因：COS 的签名把 HTTP method 也算进签名，
// 而这个 URL 是上游按 GET 签的。COS 的判定顺序是：
//
//	query 里带 q-signature → 校验它 → 不匹配就 403（不再看 bucket 权限）
//	query 里没有签名       → 走 bucket ACL → 本 bucket 是公开读 → 放行
//
// 而预览器/取图器**通常先发 HEAD** 探测 Content-Type 与大小。
// 一探测就 403 → 显示"无法预览 / 图片加载失败"。
// 走 GET 的客户端则一切正常 —— 所以现象看起来像"链接偶尔损坏"，
// 取决于那个客户端先发哪种请求。
//
// # 为什么"裸 URL"不是权宜之计
//
// 裸形式在这三条上**严格优于**签名形式：
//
//	HEAD 可用         预览器能探测
//	不会过期          实测：过期签名的 URL 403，裸 URL 仍 200
//	markdown 友好     没有 `&` 与 `;`（那两个字符在 markdown 链接与
//	                  shell 里都是麻烦）
//
// 唯一的依赖是"bucket 是公开读" —— 这是**上游的部署事实**，不是我们
// 能保证的。所以本文件不硬编码这个假设，而是**探测后决定**：
//
//	探测通过 → 用裸 URL（预览器可工作）
//	探测失败 → 退回签名 URL（至少 GET 还能用）
//
// 并把两者都交给调用方（结构化产物里 `url` = 展示用的那个，
// `url_signed` = 原始签名 URL 作兜底）。
package loomy

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// displayURLProbeTimeout 探测裸 URL 是否可用的超时。
//
// 只发一个 HEAD，正常在 100-300ms 内返回。设 5 秒是因为：
// 探测失败不是致命错误（退回签名 URL 即可），不值得为它拖长响应。
// 相比生图本身的 13-35 秒，这点开销可忽略。
const displayURLProbeTimeout = 5 * time.Second

// cosSignatureParams 腾讯 COS 签名用到的 query 参数名。
//
// 按**完整名单**剥除，而不是"删掉 q-signature 就完事"：
// 只删一个会让 q-sign-algorithm / q-ak / q-sign-time 等残留下来，
// COS 看到"有部分签名参数"仍可能尝试校验并 403。
var cosSignatureParams = map[string]bool{
	"q-sign-algorithm": true,
	"q-ak":             true,
	"q-sign-time":      true,
	"q-key-time":       true,
	"q-header-list":    true,
	"q-url-param-list": true,
	"q-signature":      true,
}

// stripCOSSignature 剥掉 COS 签名参数，返回裸 URL。
//
// 没有签名参数时原样返回（对非 COS 的 URL 是安全的 no-op）。
// 其它 query 参数（若有）保留 —— 只剥签名，不做"删掉整个 query"
// 那种过度动作（那可能删掉对象本身需要的参数）。
func stripCOSSignature(raw string) string {
	i := strings.Index(raw, "?")
	if i < 0 {
		return raw
	}
	base, query := raw[:i], raw[i+1:]

	var kept []string
	sawSignature := false
	for _, kv := range strings.Split(query, "&") {
		if kv == "" {
			continue
		}
		key := kv
		if j := strings.Index(kv, "="); j >= 0 {
			key = kv[:j]
		}
		if cosSignatureParams[key] {
			sawSignature = true
			continue
		}
		kept = append(kept, kv)
	}

	// ⚠ 只有**确实剥掉了签名**才丢 query。否则保留原样 ——
	// 一个带非签名参数的 URL（例如别的上游用 `?token=`）不该被我们改。
	if !sawSignature {
		return raw
	}
	if len(kept) == 0 {
		return base
	}
	return base + "?" + strings.Join(kept, "&")
}

// displayImageURL 决定给下游用哪个 URL。
//
// 返回 (展示用 URL, 是否用了裸形式)。
//
// 探测策略（见文件头）：先 HEAD 裸 URL，2xx 就采用；否则退回原 URL。
// 探测**绝不**让整个请求失败 —— 出错就退回签名 URL（它至少 GET 能用）。
func displayImageURL(ctx context.Context, hc *http.Client, raw string) (string, bool) {
	bare := stripCOSSignature(raw)
	if bare == raw {
		// 本来就没有签名参数 → 没什么可优化的。
		return raw, false
	}
	if hc == nil {
		return raw, false
	}

	probeCtx, cancel := context.WithTimeout(ctx, displayURLProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, bare, nil)
	if err != nil {
		return raw, false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return raw, false
	}
	defer resp.Body.Close()

	// 2xx 才认为裸形式可用。3xx 也接受（对象存储偶尔用重定向）——
	// 但更严一点：只认 200/206，因为预览器需要的是直接可取的资源。
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		return bare, true
	}
	return raw, false
}
