package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// X-Product-Code 是产品身份的一部分（参照项目从 CodeBuddy CN IDE 逆向）。
//
// # 为什么这条测试必要
//
// 参照项目的 buddy 与 workbuddy 两个产品配置里，productCode 分别是
// `codebuddy` / `workbuddy`，注释写明「对齐 IDE headers 设置」——
// 它的 header 常量整段标注「逆向自 IDE Jd/jM/qM 定义」。
//
// 而本网关原先**完全没有**这个头。补上之后必须钉住两件事：
//
//  1. 非空时真的发出去（否则等于没补）
//  2. 空时**不发**（改造前行为，既有部署逐字节不变）
//
// 第 2 条尤其重要：这是一个新 header，若"空值也发一个空串"，
// 所有既有部署的出站请求都会多一个头 —— 那是行为变更。
//
// 复用 headers_identity_test.go 里的 newHdrReq（同包，不重复定义）。
func TestProductCodeHeader(t *testing.T) {
	authOf := func() *auth.Auth {
		return &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	}

	t.Run("非空时注入", func(t *testing.T) {
		c := New()
		c.ProductCode = "codebuddy"
		req := newHdrReq(t)
		c.ChatHeaders(req, authOf(), "", "")
		if got := req.Header.Get("X-Product-Code"); got != "codebuddy" {
			t.Errorf("X-Product-Code = %q, want codebuddy", got)
		}
	})

	t.Run("为空时不注入", func(t *testing.T) {
		c := New()
		c.ProductCode = ""
		req := newHdrReq(t)
		c.ChatHeaders(req, authOf(), "", "")
		if _, present := req.Header["X-Product-Code"]; present {
			t.Error("ProductCode 为空时不该注入该头（改造前行为：既有部署不多任何头）")
		}
	})

	t.Run("与 ClientName 互补而非联动", func(t *testing.T) {
		// 归属名与产品代码是两个独立事实，可以各自设置。
		// 归属名写品牌、product code 用英文小写 —— 这种组合必须能表达。
		c := New()
		c.ClientName = "WorkBuddy"
		c.ProductCode = "workbuddy"
		req := newHdrReq(t)
		c.ChatHeaders(req, authOf(), "", "")
		if got := req.Header.Get("X-Product"); got != "WorkBuddy" {
			t.Errorf("X-Product = %q, want WorkBuddy", got)
		}
		if got := req.Header.Get("X-Product-Code"); got != "workbuddy" {
			t.Errorf("X-Product-Code = %q, want workbuddy", got)
		}
	})

	t.Run("只配 ProductCode 不配 ClientName", func(t *testing.T) {
		// ClientName 空 → X-Product 仍是 "SaaS"（旧行为），
		// 但 X-Product-Code 照发 —— 两者互不影响。
		c := New()
		c.ProductCode = "codebuddy"
		req := newHdrReq(t)
		c.ChatHeaders(req, authOf(), "", "")
		if got := req.Header.Get("X-Product"); got != "SaaS" {
			t.Errorf("ClientName 为空时 X-Product 应为 SaaS，实际 %q", got)
		}
		if got := req.Header.Get("X-Product-Code"); got != "codebuddy" {
			t.Errorf("X-Product-Code = %q, want codebuddy", got)
		}
	})

	t.Run("billing 路径也带", func(t *testing.T) {
		// BillingHeaders 走的是另一条分支（不调 CommonHeaders），
		// 它是否带该头需要单独确认。
		c := New()
		c.ProductCode = "codebuddy"
		req := newHdrReq(t)
		c.BillingHeaders(req, authOf())
		if got := req.Header.Get("X-Product-Code"); got != "codebuddy" {
			t.Errorf("billing 请求也应带 X-Product-Code，实际 %q", got)
		}
	})
}
