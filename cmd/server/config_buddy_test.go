package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuddySectionParsed buddy 配置段能被解析（防"静默丢弃"）。
//
// # 为什么这条必须存在
//
// 本项目踩过同型的坑：`Qoder` 配置段连 struct 都没有，json.Unmarshal
// 把整个对象当未知字段静默丢弃 —— 配置写了 enabled:true，程序照旧打印
// 「未启用」且无任何报错。所以每个新增段都要有一条"JSON → 解析后字段"的断言。
func TestBuddySectionParsed(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{"buddy":{
		"enabled": true,
		"oauth_platform": "ide",
		"user_agent": "CodeBuddyIDE/1.106.1",
		"product_code": "codebuddy",
		"client_name": "CodeBuddy",
		"client_version": "1.106.1",
		"cli_version": "2.137.1"
	}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.BuddyEnabled {
		t.Fatal("buddy.enabled=true 未生效 —— 配置被静默丢弃（本项目踩过同型的坑）")
	}
	if c.BuddyUserAgent != "CodeBuddyIDE/1.106.1" {
		t.Errorf("BuddyUserAgent = %q，want CodeBuddyIDE/1.106.1", c.BuddyUserAgent)
	}
	if c.BuddyProductCode != "codebuddy" {
		t.Errorf("BuddyProductCode = %q，want codebuddy", c.BuddyProductCode)
	}
	if c.BuddyClientName != "CodeBuddy" {
		t.Errorf("BuddyClientName = %q，want CodeBuddy", c.BuddyClientName)
	}
	if c.BuddyOAuthPlatform != "ide" {
		t.Errorf("BuddyOAuthPlatform = %q，want ide（参照 CODEBUDDY 的取值）", c.BuddyOAuthPlatform)
	}
	if c.BuddyClientVersion != "1.106.1" || c.BuddyCliVersion != "2.137.1" {
		t.Errorf("版本段 = %q / %q，want 1.106.1 / 2.137.1",
			c.BuddyClientVersion, c.BuddyCliVersion)
	}
	// 并池默认开（与其余上游同规则）。
	if !c.BuddyPoolAccounts {
		t.Error("BuddyPoolAccounts 默认应为 true")
	}
}

// TestBuddyAbsentIsDisabledAndUnchanged 段缺席时**完全不生效**（向后兼容硬要求）。
//
// 既有部署的 config.json 里没有 buddy 段，解析后必须是：不注册、不并池，
// 且全局出站身份不受影响。
func TestBuddyAbsentIsDisabledAndUnchanged(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"listen":":7863"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.BuddyEnabled {
		t.Error("段缺席时 buddy 必须是未启用")
	}
	if c.BuddyPoolAccounts {
		t.Error("未启用时 BuddyPoolAccounts 必须恒 false")
	}
	// 身份字段必须是空串 —— 装配层据此"不覆盖"，
	// 于是既有部署的出站请求与升级前逐字节相同。
	for name, v := range map[string]string{
		"BuddyUserAgent":     c.BuddyUserAgent,
		"BuddyProductCode":   c.BuddyProductCode,
		"BuddyClientName":    c.BuddyClientName,
		"BuddyClientVersion": c.BuddyClientVersion,
		"BuddyCliVersion":    c.BuddyCliVersion,
	} {
		if v != "" {
			t.Errorf("%s = %q，want 空串（不覆盖全局值）", name, v)
		}
	}
}

// TestBuddyDefaultsAuthDirAndSite 缺省的凭证目录与授权站点。
func TestBuddyDefaultsAuthDirAndSite(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"buddy":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.BuddyAuthDir == "" {
		t.Error("BuddyAuthDir 应填默认值")
	}
	if c.BuddyOAuthBaseURL != "https://copilot.tencent.com" {
		t.Errorf("BuddyOAuthBaseURL = %q，want copilot.tencent.com"+
			"（与国内版同一站点 —— buddy 打的就是同一个上游）", c.BuddyOAuthBaseURL)
	}
	// platform 默认 ide：buddy 整段是显式启用才生效的新实例，
	// 不存在"既有部署行为改变"的问题，所以给参照验证过的取值。
	if c.BuddyOAuthPlatform != "ide" {
		t.Errorf("BuddyOAuthPlatform = %q，want ide", c.BuddyOAuthPlatform)
	}
}

// TestBuddyPlatformExplicitCLIWins 显式写 CLI 时不被默认值覆盖。
//
// 想保留 CLI 登录流程的运维能在配置里写 `oauth_platform: "CLI"` ——
// 若默认值无脑覆盖，这条退路就没了。
func TestBuddyPlatformExplicitCLIWins(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp,
		[]byte(`{"buddy":{"enabled":true,"oauth_platform":"CLI"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.BuddyOAuthPlatform != "CLI" {
		t.Errorf("BuddyOAuthPlatform = %q，want CLI（显式值不该被默认覆盖）", c.BuddyOAuthPlatform)
	}
}
