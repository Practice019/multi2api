package loomy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestEscapeMarkdownText 转义会破坏 markdown 结构的字符。
//
// # 这是用户报的"图片无法预览"的第二个根因
//
// 提示词是用户输入，被直接插进 `![alt](url)`。CommonMark 规定 alt 在
// **第一个未转义的 `]`** 处结束，于是：
//
//	提示词 "含]括号" → ![含]括号](url)
//	                    ↑ alt 到此为止 → 图不显示、URL 变纯文本
//
// 用户实测的提示词正是"…用 [主推款] 这样的标签"那类 ——
// 他看到的"图片无法预览 + 一段被截断的提示词"里的那段文本，
// 就是截断后的 alt。
func TestEscapeMarkdownText(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"方括号", "用 [主推款] 标签", `用 \[主推款\] 标签`},
		{"圆括号", "产品(新款)", `产品\(新款\)`},
		{"反斜杠", `路径\a`, `路径\\a`},
		{"星号与下划线", "a*b_c", `a\*b\_c`},
		{"反引号", "`code`", "\\`code\\`"},
		{"竖线与波浪", "a|b~c", `a\|b\~c`},
		{"尖括号", "<tag>", `\<tag\>`},
		{"井号", "#标题", `\#标题`},
		{"纯中文不转义", "一只猫在窗台上", "一只猫在窗台上"},
		{"空串", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeMarkdownText(tc.in); got != tc.want {
				t.Errorf("escapeMarkdownText(%q) = %q，want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEscapeMarkdownTextBackslashFirst 反斜杠必须**先**转义。
//
// 顺序错了会双转义：`]` → `\]` → `\\]`（渲染出多一个反斜杠）。
// 这条容易在"重构转义逻辑"时被破坏，所以单独钉住。
func TestEscapeMarkdownTextBackslashFirst(t *testing.T) {
	// 输入本身含反斜杠 + 方括号
	got := escapeMarkdownText(`a\]b`)
	want := `a\\\]b`
	if got != want {
		t.Errorf(`escapeMarkdownText("a\]b") = %q，want %q —— `+
			`反斜杠没先转义会让反斜杠数量翻倍`, got, want)
	}
}

// TestMarkdownAltTextTruncatesThenEscapes 先截断再转义。
//
// 反过来（先转义再截断）可能把 `\x` 从中间切断，露出一个悬空反斜杠 ——
// 它又会去转义后面那个字符，产出不可预期的结果。
func TestMarkdownAltTextTruncatesThenEscapes(t *testing.T) {
	// 41 个字符全是 `]` → 截断到 40 个 + "…"，全部转义
	long := strings.Repeat("]", 41)
	got := markdownAltText(long)
	// 40 个 `\]` + `…`
	want := strings.Repeat(`\]`, 40) + "…"
	if got != want {
		t.Errorf("截断+转义结果不对：\n got=%q\nwant=%q", got, want)
	}
	if strings.Contains(got, `\\`) {
		t.Error("出现了双反斜杠 —— 说明先转义再截断，把转义序列切断了")
	}
}

// TestMarkdownAltTextCollapsesWhitespace 换行/多空格要压平。
//
// alt 里的换行会破坏 markdown 结构（链接语法不允许跨行）。
func TestMarkdownAltTextCollapsesWhitespace(t *testing.T) {
	got := markdownAltText("第一行\n\n第二行   第三行")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("alt 里还留着换行，会破坏 markdown：%q", got)
	}
	if got != "第一行 第二行 第三行" {
		t.Errorf("空白没压平：%q", got)
	}
}

// TestStripCOSSignature 剥掉 COS 签名参数。
func TestStripCOSSignature(t *testing.T) {
	base := "https://b.cos.ap-guangzhou.myqcloud.com/x/y.png"
	sig := base + "?q-sign-algorithm=sha1&q-ak=AKIDx&q-sign-time=1;2" +
		"&q-key-time=1;2&q-header-list=host&q-url-param-list=&q-signature=deadbeef"

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"完整签名", sig, base},
		{"没有 query（no-op）", base, base},
		{"没有签名参数（no-op，别改别人的 URL）", base + "?token=abc", base + "?token=abc"},
		{"部分签名参数也要剥干净", base + "?q-signature=abc", base},
		{"签名 + 非签名参数（只剥签名的）", base + "?q-signature=abc&v=2", base + "?v=2"},
		{"只有 q-ak（部分签名，仍要剥）", base + "?q-ak=x", base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripCOSSignature(tc.in); got != tc.want {
				t.Errorf("stripCOSSignature 结果不对：\n got=%q\nwant=%q", got, tc.want)
			}
		})
	}
}

// TestStripCOSSignatureKeepsForeignQuery 非 COS 的 URL 不该被改动。
//
// 只有**确实剥掉了签名参数**才允许丢 query。否则一个别的上游用
// `?token=` 签权的 URL 会被我们剥成裸链接（那会直接取不到）。
func TestStripCOSSignatureKeepsForeignQuery(t *testing.T) {
	foreign := "https://example.com/img.png?token=secret&v=2"
	if got := stripCOSSignature(foreign); got != foreign {
		t.Errorf("改动了非 COS 的 URL：\n got=%q\nwant=%q", got, foreign)
	}
}

// TestDisplayImageURLUsesBareWhenHeadWorks 裸 URL HEAD 可用时采用它。
//
// # 为什么这条是"图片无法预览"的主修
//
// 实测（10/10 复现）：签名 URL 的 HEAD **必然 403**（COS 把 method
// 也算进签名，而 URL 是按 GET 签的），裸 URL 的 HEAD 是 200。
// 预览器/取图器通常先发 HEAD 探测 → 一探测就失败 → "无法预览"。
func TestDisplayImageURLUsesBareWhenHeadWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("探测应当用 HEAD（GET 会把整张图拉下来，太贵）")
		}
		if r.URL.Query().Get("q-signature") != "" {
			// 带签名 → 模拟 COS 行为：403
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	signed := srv.URL + "/a.png?q-sign-algorithm=sha1&q-ak=x&q-sign-time=1;2" +
		"&q-key-time=1;2&q-header-list=host&q-url-param-list=&q-signature=deadbeef"

	hc := &http.Client{Timeout: 3 * time.Second}
	got, bare := displayImageURL(context.Background(), hc, signed)
	if !bare {
		t.Fatalf("裸 URL 的 HEAD 返回 200，应当采用它；实际 bare=false，got=%q", got)
	}
	if got != srv.URL+"/a.png" {
		t.Errorf("got = %q，want 裸 URL", got)
	}
}

// TestDisplayImageURLFallsBackWhenBareFails 裸 URL 不可用时退回签名 URL。
//
// # 为什么必须有这条兜底
//
// 裸形式依赖"bucket 公开读"这个**上游部署事实**，不是我们能保证的。
// 若上游哪天改成私有，裸 URL 会 403 —— 那时必须退回签名 URL
// （它至少 GET 还能用），而不是把一张取不到的图给用户。
func TestDisplayImageURLFallsBackWhenBareFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 无论带不带签名都 403（模拟 bucket 改为私有）
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	signed := srv.URL + "/a.png?q-signature=deadbeef"
	hc := &http.Client{Timeout: 3 * time.Second}
	got, bare := displayImageURL(context.Background(), hc, signed)
	if bare {
		t.Error("裸 URL 403 时不该采用它")
	}
	if got != signed {
		t.Errorf("应当退回原始签名 URL，got = %q", got)
	}
}

// TestDisplayImageURLNoProbeForUnsigned 本来就没签名时不做探测。
//
// 没有签名参数 = 没什么可优化的。多发一个 HEAD 是白费，
// 而且对"上游本来就用裸 URL"的场景会增加一次无谓的网络往返。
func TestDisplayImageURLNoProbeForUnsigned(t *testing.T) {
	probed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	plain := srv.URL + "/a.png"
	hc := &http.Client{Timeout: 3 * time.Second}
	got, bare := displayImageURL(context.Background(), hc, plain)
	if probed {
		t.Error("没有签名参数时不该发探测请求")
	}
	if bare || got != plain {
		t.Errorf("应当原样返回，got=%q bare=%v", got, bare)
	}
}

// TestDisplayImageURLProbeFailureIsNotFatal 探测出错不能让请求失败。
//
// 网络抖动、超时、DNS 失败都可能发生。探测失败只意味着"不知道裸形式
// 行不行" —— 那就退回签名 URL（GET 仍可用），绝不把整次生图判为失败。
func TestDisplayImageURLProbeFailureIsNotFatal(t *testing.T) {
	signed := "https://127.0.0.1:1/a.png?q-signature=deadbeef" // 必然连不上
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	got, bare := displayImageURL(context.Background(), hc, signed)
	if got != signed {
		t.Errorf("探测失败时应当原样返回签名 URL，got = %q", got)
	}
	if bare {
		t.Error("探测失败时不该声称用了裸形式")
	}
}

// TestDisplayImageURLNilClientIsSafe 没有 HTTP 客户端时不 panic。
//
// 单测构造的 Provider 可能没设 client —— 那条路径必须安全退回。
func TestDisplayImageURLNilClientIsSafe(t *testing.T) {
	signed := "https://x/a.png?q-signature=deadbeef"
	got, bare := displayImageURL(context.Background(), nil, signed)
	if got != signed || bare {
		t.Errorf("nil client 时应当原样返回，got=%q bare=%v", got, bare)
	}
}

// TestImageMarkdownIsParseable 端到端：生成的 markdown 真的是合法图片语法。
//
// 这是用户视角的判据 —— 它会用 markdown 解析器渲染。带 `]` 的提示词
// 曾经产出 `![含]括号](url)`，解析器在第一个 `]` 就截断了。
func TestImageMarkdownIsParseable(t *testing.T) {
	url := "https://b.example.com/a.png"
	for _, prompt := range []string{
		"用 [主推款] 这样的标签",
		"产品(新款) 促销",
		"a\\b 反斜杠",
		"电商产品主图宣传海报，正方形1:1构图，现代科技风平面设计，深蓝色渐变背景带青蓝光效",
	} {
		md := "![" + markdownAltText(prompt) + "](" + url + ")"

		// 简单而准确的判据：alt 段里不该出现**未转义**的 `]`
		// （转义后的 `\]` 是合法的，也是我们要的）
		altStart := strings.Index(md, "![") + 2
		altEnd := strings.LastIndex(md, "](")
		if altEnd < altStart {
			t.Fatalf("构造出来的 markdown 结构就不对：%q", md)
		}
		alt := md[altStart:altEnd]
		if hasUnescaped(alt, ']') {
			t.Errorf("alt 里有未转义的 ]，解析器会在此截断：\n  prompt=%q\n  md=%q", prompt, md)
		}
		if !strings.HasSuffix(md, "]("+url+")") {
			t.Errorf("URL 部分不完整：%q", md)
		}
	}
}

// hasUnescaped 报告 s 里是否含未转义的字符 c。
func hasUnescaped(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++ // 跳过被转义的那个字符
			continue
		}
		if s[i] == c {
			return true
		}
	}
	return false
}
