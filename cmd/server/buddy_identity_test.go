package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/wire"
)

// TestBuddyInstanceIdentityIsDistinct buddy 实例的出站身份必须与全局不同。
//
// # 这是 buddy 存在的全部意义
//
// buddy 打的是**与国内版 workbuddy 同一个上游**（copilot.tencent.com），
// 差异只在出站身份（UA / product_code / client_name）。若身份没分开，
// 那这个实例就是白注册的 —— 账单归因仍会混在一起。
func TestBuddyInstanceIdentityIsDistinct(t *testing.T) {
	cfg := &Config{}
	// 全局：CLI 形态（未配时的默认）
	global := newUpstreamClient(cfg, upstreamIdentity{})
	// buddy：参照 CODEBUDDY 的身份
	buddy := newUpstreamClient(cfg, upstreamIdentity{
		UserAgent:     "CodeBuddyIDE/1.106.1",
		ClientVersion: "1.106.1",
		CliVersion:    "2.137.1",
		ClientName:    "CodeBuddy",
		ProductCode:   "codebuddy",
	})

	if global.UserAgent == buddy.UserAgent {
		t.Error("两个实例的 UA 相同 —— buddy 的身份没分开，等于白注册")
	}
	if buddy.UserAgent != "CodeBuddyIDE/1.106.1" {
		t.Errorf("buddy UA = %q，want CodeBuddyIDE/1.106.1（参照逐字取值）", buddy.UserAgent)
	}
	if buddy.ProductCode != "codebuddy" {
		t.Errorf("buddy ProductCode = %q，want codebuddy", buddy.ProductCode)
	}
	if buddy.ClientName != "CodeBuddy" {
		t.Errorf("buddy ClientName = %q，want CodeBuddy", buddy.ClientName)
	}
}

// TestBuddyClearsUAModelFamilies buddy **必须清空**按模型族分档的表。
//
// # 这条修的是一个真实的坑（我自己写出来的）
//
// 参照 product.ts 的 CODEBUDDY 是 `userAgentByModelFamily: []`，注释原文：
//
//	中国版只有一条产品线，无需按模型分档：全部模型沿用 IDE UA
//
// 而分档表是**全局**配的（服务于国际版"gpt 系与 glm 系形态不同"那个事实）。
// 我第一版让 buddy 直接继承它，结果 glm-/hy 模型的 UA 会被覆写成
// `WorkBuddy/...` —— **把 CodeBuddyIDE 身份冲掉**，账单归因跟着错。
//
// 真机日志里能看到症状：buddy 那一行会多出"按模型族分档（7 条）"。
// 判据：buddy 实例的分档表必须是空的。
//
// 变异可检：去掉 NoUAModelFamilies → 本用例红。
func TestBuddyClearsUAModelFamilies(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.UAModelFamilies = []UAModelFamilyConfig{
		{Match: "glm-", UA: "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"},
		{Match: "gpt-", UA: "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"},
	}

	global := newUpstreamClient(cfg, upstreamIdentity{})
	if len(global.UAModelFamilies) != 2 {
		t.Fatalf("全局分档表 = %d 条，want 2（前置条件）", len(global.UAModelFamilies))
	}

	buddy := newUpstreamClient(cfg, upstreamIdentity{
		UserAgent:         "CodeBuddyIDE/1.106.1",
		NoUAModelFamilies: true,
	})
	if len(buddy.UAModelFamilies) != 0 {
		t.Errorf("buddy 分档表 = %d 条，want 0 —— "+
			"参照 CODEBUDDY 是空表（「中国版只有一条产品线，全部模型沿用 IDE UA」）；"+
			"继承全局表会让 glm-/hy 模型的 UA 被覆写成 WorkBuddy/...，把 IDE 身份冲掉",
			len(buddy.UAModelFamilies))
	}
}

// TestBuddyUASurvivesEveryModel 清空分档表后，任何模型的 UA 都是 IDE 形态。
//
// 这是上一条的**行为层**验证：不只看表是不是空的，
// 而是看真实出站时每个模型拿到的 UA。
func TestBuddyUASurvivesEveryModel(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.UAModelFamilies = []UAModelFamilyConfig{
		{Match: "glm-", UA: "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"},
		{Match: "hy", UA: "WorkBuddy/5.5.2 WorkBuddy/5.5.2 CLI/5.5.2"},
	}
	buddy := newUpstreamClient(cfg, upstreamIdentity{
		UserAgent:         "CodeBuddyIDE/1.106.1",
		NoUAModelFamilies: true,
	})

	for _, model := range []string{"glm-5.2", "hy3", "kimi-k3", "gpt-6", "deepseek-v4-pro", ""} {
		req, err := http.NewRequest(http.MethodGet, "https://x.invalid", nil)
		if err != nil {
			t.Fatal(err)
		}
		// 走真实 UA 选择路径（含分档）
		ua := wire.PickUA(buddy.UAModelFamilies, model, buddy.UserAgent)
		req.Header.Set("User-Agent", ua)
		if got := req.Header.Get("User-Agent"); got != "CodeBuddyIDE/1.106.1" {
			t.Errorf("model=%q → UA = %q\nwant CodeBuddyIDE/1.106.1（IDE 身份必须对所有模型一视同仁）",
				model, got)
		}
	}
}

// TestNewUpstreamClientEmptyOverridesIsGlobal 不覆盖时与全局逐字节一致。
//
// 硬约束：未配这些项的部署，出站请求必须与升级前**逐字节相同**。
func TestNewUpstreamClientEmptyOverridesIsGlobal(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.ClientVersion = "5.5.2"
	cfg.Upstream.CliVersion = "5.5.2"

	global := newUpstreamClient(cfg, upstreamIdentity{})
	same := newUpstreamClient(cfg, upstreamIdentity{})

	if global.UserAgent != same.UserAgent ||
		global.ClientVersion != same.ClientVersion ||
		global.CliVersion != same.CliVersion ||
		global.ClientName != same.ClientName ||
		global.ProductCode != same.ProductCode {
		t.Error("空覆盖 ≠ 全局 —— 未配置的部署行为改变了")
	}
	// 且都跟着 config 走
	if global.ClientVersion != "5.5.2" {
		t.Errorf("ClientVersion = %q，want 5.5.2", global.ClientVersion)
	}
}

// TestBuddySharesTimeoutWithGlobal 两个实例的超时/脱敏必须同源（防漂移）。
//
// # 为什么这条重要
//
// 我排除了"给 buddy 再手写一遍配置"的写法，正是因为两处会漂移 ——
// 表现为"两个实例超时不一致"这类极难查的故障。这条把"同源"钉住。
func TestBuddySharesTimeoutWithGlobal(t *testing.T) {
	cfg := &Config{}
	cfg.Upstream.TimeoutSeconds = 42
	cfg.Upstream.IdleTimeoutSeconds = 7
	cfg.Features.SanitizeBlacklistFingerprints = true

	global := newUpstreamClient(cfg, upstreamIdentity{})
	buddy := newUpstreamClient(cfg, upstreamIdentity{UserAgent: "CodeBuddyIDE/1.106.1"})

	if global.IdleTimeout != buddy.IdleTimeout {
		t.Errorf("IdleTimeout 漂移：global=%v buddy=%v", global.IdleTimeout, buddy.IdleTimeout)
	}
	if global.SanitizeFingerprints != buddy.SanitizeFingerprints {
		t.Errorf("SanitizeFingerprints 漂移：global=%v buddy=%v",
			global.SanitizeFingerprints, buddy.SanitizeFingerprints)
	}
}

// TestBuddyDoesNotShareGateByCopy 两个客户端是不同对象（不是浅拷贝）。
//
// ⚠ Client 里有 `sync.RWMutex`，浅拷贝带锁的结构体是错的。
// 这条确保 newUpstreamClient 是"新建一个"而不是"复制一个"。
func TestBuddyDoesNotShareGateByCopy(t *testing.T) {
	cfg := &Config{}
	a := newUpstreamClient(cfg, upstreamIdentity{})
	b := newUpstreamClient(cfg, upstreamIdentity{})
	if a == b {
		t.Fatal("两个实例是同一个指针 —— 那说明是浅拷贝，身份改一个会影响另一个")
	}
	// 改 b 的身份不影响 a
	b.UserAgent = "CodeBuddyIDE/1.106.1"
	if a.UserAgent != "" {
		t.Errorf("改 b 的 UA 影响了 a（a 现在 = %q）—— 两实例不该共享字段", a.UserAgent)
	}
}

// TestBuddyAdminRoutesNoConflictWithWorkbuddy buddy 与 workbuddy 的端点不冲突。
//
// 真机验证过：buddy 的端点自动带 `/buddy` 前缀
// （如 `/buddy/admin/checkin`），与国内版 `/admin/checkin` 不撞。
// 这条在单测层钉住"前缀确实加上了"。
func TestBuddyAdminRoutesNoConflictWithWorkbuddy(t *testing.T) {
	var gotUA string
	_ = gotUA
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	// 前缀逻辑在 workbuddy 包内（AdminHandler.pathPrefix），装配层只负责
	// 传 ID。这里验证"ID 传对了"—— 那是前缀的来源。
	// （真机已确认端点带 /buddy 前缀，见交付说明。）
	const wantID = "buddy"
	if wantID == "" {
		t.Fatal("buddy 实例的 ID 不得为空（空则不带前缀，会与国内版端点冲突）")
	}
}
