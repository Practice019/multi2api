package codearts

import (
	"strings"
	"testing"
)

// TestModelIDsMatchAuthoritativeList 锁定模型名与服务端注册名逐字符一致。
//
// 背景：早期把 `openpangu-2.0-pro` 写成 `OpenPangu-2.0-Pro`，
// 服务端返回 `InferHub.002002009 The model is not registered` ——
// 现象与"账号未开通该模型"完全相同，极难定位。
//
// 权威清单来自 AgentKernel 下发的 provider 列表（cmd/model-list 可导出）。
// 这里把 7 个名字钉住：改名/改大小写都会让测试失败。
func TestModelIDsMatchAuthoritativeList(t *testing.T) {
	// 与 AgentKernel cag-global-server 下发的清单逐字一致
	want := []string{
		"GLM-5.2",
		"glm-5.2-sft-harmony",
		"openpangu-2.0-pro",
		"openpangu-2.0-flash",
		"deepseek-v4-flash-0731",
		"deepseek-v4-pro-0813",
		"glm-5.3-flash",
	}

	got := AllModels()
	if len(got) != len(want) {
		t.Fatalf("模型数 = %d, 期望 %d", len(got), len(want))
	}
	seen := map[string]bool{}
	for _, m := range got {
		seen[m.ID] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("缺少模型 %q（大小写必须与服务端注册名一致）", w)
		}
	}

	// 反向：不应存在清单外的模型（例如曾经误加的 GLM-5.2-ArkTS-SPARK）
	allowed := map[string]bool{}
	for _, w := range want {
		allowed[w] = true
	}
	for _, m := range got {
		if !allowed[m.ID] {
			t.Errorf("多出清单外的模型 %q", m.ID)
		}
	}
}

// TestKnownModelsOnlyExposesVerified 确认 /v1/models 只暴露实测可用的模型。
//
// 未验证的模型若被暴露，客户端选中后会拿到 404，
// 而错误信息是 `InferHub.002002009 The model is not registered` ——
// 用户会以为是网关坏了，实际是账号侧没开通（或走错了通道）。
func TestKnownModelsOnlyExposesVerified(t *testing.T) {
	exposed := KnownModels()
	if len(exposed) == 0 {
		t.Fatal("KnownModels 返回空，客户端将看不到任何模型")
	}
	// 暴露的必须都是 Verified 的
	verified := map[string]bool{}
	for _, m := range AllModels() {
		if m.Verified {
			verified[m.ID] = true
		}
	}
	for _, m := range exposed {
		if m.ID == "" {
			t.Error("暴露了 ID 为空的模型")
		}
		if !verified[m.ID] {
			t.Errorf("暴露了未验证的模型 %q", m.ID)
		}
	}
	// 所有 Verified 的都必须暴露（不能有"验过但藏着"的）
	if len(exposed) != len(verified) {
		t.Errorf("暴露 %d 个但已验证 %d 个，两者应一致", len(exposed), len(verified))
	}
}

// TestSetModelVerifiedPromotesToExposed 确认运行时开通模型后能被暴露。
func TestSetModelVerifiedPromotesToExposed(t *testing.T) {
	const id = "deepseek-v4-pro-0813"
	// 先确保它是未验证状态
	for _, m := range AllModels() {
		if m.ID == id && m.Verified {
			t.Skipf("%s 已是 verified，跳过", id)
		}
	}

	SetModelVerified(id)
	defer func() {
		// 还原，避免影响其它测试
		for i := range knownModels {
			if knownModels[i].ID == id {
				knownModels[i].Verified = false
			}
		}
	}()

	found := false
	for _, m := range KnownModels() {
		if m.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("SetModelVerified(%q) 后仍未暴露", id)
	}
}

// TestNewSessionIDFormat 确认会话 id 形如 ses_<32hex>。
//
// 服务端按 user-session-id 计数并发会话；格式不对或重复会导致
// 会话槽无法释放，表现为「每账号只能成功 N 次」。
func TestNewSessionIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := NewSessionID()
		if !strings.HasPrefix(id, "ses_") {
			t.Fatalf("id 缺 ses_ 前缀: %q", id)
		}
		if len(id) != len("ses_")+32 {
			t.Fatalf("id 长度 = %d, 期望 %d: %q", len(id), len("ses_")+32, id)
		}
		if seen[id] {
			t.Fatalf("id 重复: %q", id)
		}
		seen[id] = true
	}
}

// TestHeartbeatValidatesArgs 确认参数校验拦住明显误用。
func TestHeartbeatValidatesArgs(t *testing.T) {
	c := New()
	a := &Auth{AccessKey: "AK", SecretKey: "SK"}

	if err := c.Heartbeat(a, "wat", "ses_x"); err == nil {
		t.Error("非法 status 应报错")
	}
	if err := c.Heartbeat(a, "idle", ""); err == nil {
		t.Error("缺 sessionID 应报错（服务端按会话计数，没 id 无法释放）")
	}
}
