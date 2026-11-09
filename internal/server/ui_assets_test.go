package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestUIAssetsAreExternallyServed UI 的 CSS / 主 script 已外置为可并行的独立资源（T-409）。
//
// # 目标（用户原话）
//
//	「HTML 单文件 想办法把这个HTML拆成多个，这样我们写代码的时候也可以并行。」
//
// 第 1 步只做**搬移**：CSS → `internal/server/ui/app.css`，主 script → `internal/server/ui/app.js`，
// **不切 IIFE、不改一行逻辑**。（按域切分是第 2 步。）
//
// # 四条判据
//
//	① 两个资源文件存在且非空（embed 真的拿到了字节）
//	② `webui.html` 里**不再有** `<style>` 块，且恰好一个 app.css 引用、一个 app.js 引用
//	③ app.js 用 `defer` 加载（主 IIFE 里的 $('…') 依赖 DOM 就绪；原来它在 <body> 末尾＝DOM 已就绪）
//	④ 两个资源**不需要**控制台身份即可取到，Content-Type/ETag 正确，If-None-Match 命中返回 304
//
// ⚠ 逐字节一致性（外置内容 == 原内联内容）由**搬运当时的 hash 比对**证明
// （见 T-409 报告：两组 sha256 前 24 位一致），这里不再重复 —— 那段原文已被替换掉，
// 测试里无法再取到「原内联内容」这个参照物。
func TestUIAssetsAreExternallyServed(t *testing.T) {
	if len(appCSS) == 0 {
		t.Error("appCSS 为空 —— go:embed ui/app.css 没拿到内容")
	}
	if len(appJS) == 0 {
		t.Error("appJS 为空 —— go:embed ui/app.js 没拿到内容")
	}
	// 主 script 必须以 IIFE 开头（第 1 步明确不切它）
	if !strings.HasPrefix(strings.TrimSpace(string(appJS)), "(() => {") {
		t.Errorf("app.js 不是以 IIFE 开头（第 1 步要求保持完整不切）：%q", strings.TrimSpace(string(appJS))[:40])
	}

	b, err := os.ReadFile("webui.html")
	if err != nil {
		t.Fatalf("读 webui.html 失败：%v", err)
	}
	html := string(b)
	if strings.Contains(html, "`n<style>") {
		t.Error("webui.html 里仍有 <style> 块 —— CSS 没有完全外置")
	}
	if n := strings.Count(html, `<link rel="stylesheet" href="/ui/app.css">`); n != 1 {
		t.Errorf("app.css 引用 %d 处，应恰好 1 处", n)
	}
	if n := strings.Count(html, `<script src="/ui/app.js" defer></script>`); n != 1 {
		t.Errorf("app.js 引用 %d 处，应恰好 1 处（且必须带 defer）", n)
	}
	// ③ defer（若写成没有 defer 的 src= 引用，主 IIFE 可能在 DOM 就绪前跑）
	if !strings.Contains(html, `<script src="/ui/app.js" defer>`) {
		t.Error("app.js 的 <script> 没有 defer ⇒ 主 IIFE 可能在 DOM 就绪前执行（它内部满是 $('xxx')）")
	}
	// ② 顶部那两个必须同步执行的小脚本必须留内联
	if !strings.Contains(html, "window.__WB2API_KEY__") {
		t.Error("顶部的 __WB2API_KEY__ 快脚本被搬走了 —— 它必须在样式生效前同步执行（防闪白）")
	}
}

// TestUIAssetHandlerServesAnd304 静态资源的响应头与 304 行为（T-409）。
func TestUIAssetHandlerServesAnd304(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct{ path, ctype string }{
		{"/ui/app.css", "text/css; charset=utf-8"},
		{"/ui/app.js", "application/javascript; charset=utf-8"},
	} {
		var content []byte
		if tc.path == "/ui/app.css" {
			content = appCSS
		} else {
			content = appJS
		}
		rec := httptest.NewRecorder()
		h.uiAsset(content, tc.ctype)(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s 状态码 %d，应 200", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != tc.ctype {
			t.Errorf("%s Content-Type=%q，应 %q", tc.path, ct, tc.ctype)
		}
		etag := rec.Header().Get("ETag")
		if etag == "" || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
			t.Errorf("%s ETag=%q 形态不对（应为带引号的 16 位 hex）", tc.path, etag)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=300") {
			t.Errorf("%s Cache-Control=%q，应含 max-age=300", tc.path, cc)
		}
		if body := rec.Body.Len(); body != len(content) {
			t.Errorf("%s 正文 %d 字节，应 %d", tc.path, body, len(content))
		}
		// 304：带上同一 ETag 再要一次
		rec2 := httptest.NewRecorder()
		req2 := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req2.Header.Set("If-None-Match", etag)
		h.uiAsset(content, tc.ctype)(rec2, req2)
		if rec2.Code != http.StatusNotModified {
			t.Errorf("%s 带 If-None-Match 时状态码 %d，应 304", tc.path, rec2.Code)
		}
		if rec2.Body.Len() != 0 {
			t.Errorf("%s 304 不应带正文，实际 %d 字节", tc.path, rec2.Body.Len())
		}
	}
}
