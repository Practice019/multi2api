// access_template_test.go 接入模板的判据（U-11「复制不含秘密」）。
//
// # 为什么要单独一个文件
//
// 这条功能的**唯一价值**是"复制粘贴不会带出密钥"。
// 模板好看不好看没人会为此写判据，但**泄漏一次就是一个真事故** ——
// 而模板是最容易顺手把真值塞进去的地方（前端渲染、示例值、默认值）。
//
// 所以这里钉三件事：
//
//	① 响应里**一个字节的密钥都没有**（用真实 seed 的 key 去搜）
//	② 占位符必须在（否则"没泄漏"可能只是"什么都没给"）
//	③ 模板用的密钥入口是环境变量，不是字面量
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/apikey"
	"workbuddy2api/internal/groupstore"
	"workbuddy2api/internal/store"
)

// newAccessFixture 造一个带 apikey 的部署（用来 seed 一个**真实密钥**去搜）。
//
// ⚠ store.DB 是包级单例：每个用例自己开一个临时库并 Cleanup，
// 否则下一个用例会在同一个库上跑，"空库"这个前提会静默失效。
func newAccessFixture(t *testing.T) (*Handler, string) {
	t.Helper()
	if err := store.Open(filepath.Join(t.TempDir(), "gw.db")); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ks, err := apikey.New(filepath.Join(t.TempDir(), "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	k, err := ks.Create("app", 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	gs, err := groupstore.New(filepath.Join(t.TempDir(), "groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{AdminToken: testAdminToken, ListensLoopback: true, APIKeys: ks, Groups: gs})
	return h, k.ID
}

// callAccessTemplate 发一次模板请求。
func callAccessTemplate(t *testing.T, h *Handler, admin bool, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/access-template"+query, nil)
	req.Host = "gw.example.test"
	if admin {
		req = withTestAuth(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ★ ① 响应里一个字节的密钥都没有。
func TestAccessTemplateNeverLeaksKey(t *testing.T) {
	h, key := newAccessFixture(t)
	rec := callAccessTemplate(t, h, true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if key != "" && strings.Contains(body, key) {
		t.Fatalf("★ 模板里出现了真实密钥 %q —— 这份文本是要被复制粘贴、发给同事的", key)
	}
	// 也不能出现任何 sk- 形态的真值（占位符不以 sk- 开头）。
	if strings.Contains(body, "sk-") {
		t.Fatalf("★ 模板里出现了 sk- 形态的字符串 —— 那看起来就是一把真密钥\nbody=%s", body)
	}
}

// ★ ② 占位符必须在（否则"没泄漏"可能只是"什么都没给"）。
//
// ⚠ 要断言**解析后**的值，不是原始字节：`encoding/json` 默认把 `<` `>` `&`
// 转义成 `\u003c` 之类，于是原始 body 里搜不到 `<YOUR_API_KEY>` 字面量，
// 而它在语义上完全存在（前端 JSON.parse 出来就是它）。
// 第一版就是在原始 body 上搜的，红了一次 —— 那是判据错，不是实现错。
func TestAccessTemplateHasPlaceholder(t *testing.T) {
	h, _ := newAccessFixture(t)
	rec := callAccessTemplate(t, h, true, "")
	var out accessTemplate
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v\nbody=%s", err, rec.Body.String())
	}
	if !strings.Contains(out.Templates["env"], accessTemplateKeyPlaceholder) {
		t.Fatalf("★ env 模板必须含占位符 %q —— 否则用户不知道该往哪填密钥\nenv=%s",
			accessTemplateKeyPlaceholder, out.Templates["env"])
	}
	if !strings.Contains(out.Note, accessTemplateKeyPlaceholder) {
		t.Fatalf("说明里应点出占位符是什么，实际 %q", out.Note)
	}
	if len(out.Templates) < 4 {
		t.Fatalf("模板种类太少（至少 curl/python/node/env），实际 %d 种", len(out.Templates))
	}
	// ★ ③ 每个模板都用环境变量取密钥，没有一处写死值。
	for name, tpl := range out.Templates {
		if !strings.Contains(tpl, "WORKBUDDY_API_KEY") && !strings.Contains(tpl, accessTemplateKeyPlaceholder) {
			t.Fatalf("★ 模板 %q 既没有环境变量名也没有占位符 —— 它要么写死了值要么没法用：\n%s",
				name, tpl)
		}
	}
}

// 只有管理员能看（模板暴露的是网关的接入面）。
func TestAccessTemplateRequiresAdmin(t *testing.T) {
	h, _ := newAccessFixture(t)
	rec := callAccessTemplate(t, h, false, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("★ 非管理员拿到了接入模板：%s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("应 401/403，实际 %d", rec.Code)
	}
}

// 禁缓存。
func TestAccessTemplateIsNoStore(t *testing.T) {
	h, _ := newAccessFixture(t)
	rec := callAccessTemplate(t, h, true, "")
	if got := rec.Header().Get("Cache-Control"); got != credentialCacheControl {
		t.Fatalf("必须带 no-store（%q），实际 %q", credentialCacheControl, got)
	}
}

// base_url 用请求的 Host 拼（反代后才是用户手上那个地址）。
func TestAccessTemplateBaseURLFromHost(t *testing.T) {
	h, _ := newAccessFixture(t)
	rec := callAccessTemplate(t, h, true, "")
	var out accessTemplate
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.BaseURL, "https://gw.example.test/v1") {
		t.Fatalf("base_url 应据请求 Host 拼成 https://gw.example.test/v1，实际 %q", out.BaseURL)
	}
}

// 回环地址按 http（本机部署的常态）。
func TestAccessBaseURLSchemeForLoopback(t *testing.T) {
	cases := map[string]string{
		"localhost:8080":  "http://localhost:8080",
		"127.0.0.1:7863":  "http://127.0.0.1:7863",
		"192.168.1.9:80":  "http://192.168.1.9:80",
		"gw.example.test": "https://gw.example.test",
		"10.0.0.5:9000":   "http://10.0.0.5:9000",
	}
	for host, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/admin/access-template", nil)
		req.Host = host
		if got := accessBaseURL(req); got != want {
			t.Fatalf("Host=%q ⇒ %q，want %q", host, got, want)
		}
	}
}

// ?model= 可指定示例模型；不指定则用占位符。
func TestAccessTemplateModelHint(t *testing.T) {
	h, _ := newAccessFixture(t)

	rec := callAccessTemplate(t, h, true, "?model=my-model")
	var out accessTemplate
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ModelHint != "my-model" || !strings.Contains(out.Templates["curl"], "my-model") {
		t.Fatalf("指定模型应出现在模板里：hint=%q", out.ModelHint)
	}

	rec = callAccessTemplate(t, h, true, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ModelHint != "<YOUR_MODEL>" {
		t.Fatalf("未指定时应回占位符，实际 %q", out.ModelHint)
	}
}
