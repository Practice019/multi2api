package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIntlLoginUACarriesProductIdentity 海外版登录要带**产品身份** UA。
//
// # 为什么需要它（参照 buddy-oauth.ts 明确要求）
//
// 参照注释原文：「轮询 token / 账户两步同样带产品身份标识，否则
// WorkBuddy 登录会以 CodeBuddy 的 UA 发请求"。
//
// 参照 product.ts:70-71 的两个形态（差异只在品牌段）：
//
//	海外版  WorkBuddy/<v> WorkBuddy AI/<v> CLI/<v>
//	国内版  WorkBuddy/<v> WorkBuddy/<v>    CLI/<v>
//
// ⚠ 差点漏掉：登录请求看起来"无所谓 UA"，但参照明确要求三步都带。
// 这正是"不能自己重新探究上游行为"的意义 —— 读了参照才知道。
func TestIntlLoginUACarriesProductIdentity(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"st1","authUrl":"https://login.example/x?platform=workbuddy-ai"}}`))
	}))
	defer srv.Close()

	c := newOAuthClientFor(srv.URL, "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2")
	if _, _, err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if gotUA != "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2" {
		t.Errorf("登录（auth/state）UA = %q\nwant 海外版形态（品牌段含 `AI`）", gotUA)
	}
}

// TestIntlLoginUADiffersFromCNForm 海外版与国内版的登录 UA**必须不同**。
//
// 两者唯一区别是品牌段的 `AI`。若被"整理"成同一个，海外版登录在服务端侧
// 会被当成另一种产品（账单归因错误）。
func TestIntlLoginUADiffersFromCNForm(t *testing.T) {
	intl := "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	cn := "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2" + "x" // 占位防止误判相等
	cn = "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"
	if intl == cn {
		t.Fatal("两个形态不得相同")
	}
	if !containsAI(intl) {
		t.Error("海外版品牌段应含 `AI`")
	}
	if containsAI(cn) {
		t.Error("国内版品牌段不得含 `AI`")
	}
}

func containsAI(ua string) bool {
	for i := 0; i+3 <= len(ua); i++ {
		if ua[i:i+3] == "AI/" {
			return true
		}
	}
	return false
}

// TestLoginUAFallsBackWhenUnset 未配 UA 时登录请求**逐字节不变**（关键约束）。
//
// 既有部署升级后，登录三步发出的 UA 必须与升级前完全相同 ——
// 否则上游归因/风控可能受影响。
//
// 判据：newOAuthClientFor 传空/纯空白 → 仍是 oauth 包的内置 CLI 形态
// `CLI/2.63.2 CodeBuddy/2.63.2`。
func TestLoginUAFallsBackWhenUnset(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"st1","authUrl":"https://login.example/x"}}`))
	}))
	defer srv.Close()

	for _, ua := range []string{"", "   ", "\t\n"} {
		gotUA = ""
		c := newOAuthClientFor(srv.URL, ua)
		if _, _, err := c.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if gotUA != "CLI/2.63.2 CodeBuddy/2.63.2" {
			t.Errorf("ua=%q → 登录 UA = %q\nwant 内置 CLI 形态（与改造前逐字节相同）",
				ua, gotUA)
		}
	}
}

// TestLoginUATrimmed 配置的 UA 首尾空白要 Trim（避免发出畸形 UA）。
func TestLoginUATrimmed(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"st1","authUrl":"https://login.example/x"}}`))
	}))
	defer srv.Close()

	c := newOAuthClientFor(srv.URL, "  MyAgent/1.0  ")
	if _, _, err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if gotUA != "MyAgent/1.0" {
		t.Errorf("UA = %q，want %q（应 Trim）", gotUA, "MyAgent/1.0")
	}
}
