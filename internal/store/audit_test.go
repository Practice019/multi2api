package store

import (
	"errors"
	"path/filepath"
	"testing"
)

// openAuditTestDB 本文件的测试库入口（独立文件名 / 独立符号）。
//
// ⚠ 与 openLogTestDB 同一条理由：**不**复用别的测试文件里的助手。
// 同一个包里多个切片并行写测试时，文件隔离只保证「文件不重叠」，
// 不保证「符号名不撞车」。audit.go 只依赖 db.go 的 Open/Migrate/Close
// 与 models.go 的 AuditLog —— 那是冻结契约。
func openAuditTestDB(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gw.db")
	if err := Open(path); err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	if err := Migrate(); err != nil {
		t.Fatalf("Migrate(): %v", err)
	}
	t.Cleanup(func() { _ = Close() })
}

// TestAuditAppendRoundTrip 写进去的每一个业务字段都要能**原样读回**。
//
// # 为什么这条是必须的（行数判据看不见它）
//
// 「行数 +1」这种二元判据对**字段错位**完全没有判别力：把 Success 存成
// Status 的切片、把 Route 写成 Action，行数照样 +1、审计页照样有那么多行，
// 只是内容全错。所以这里逐字段比对。
//
// ⚠ 同时钉住「不存请求体」：Params 由调用方给，本层不自动生成；
// 这条断言防止将来有人「顺手」把请求体塞进 Params。
func TestAuditAppendRoundTrip(t *testing.T) {
	openAuditTestDB(t)

	const eventID = "audit-round-trip-0001"
	e := AuditLog{
		EventID:    eventID,
		At:         1793000000,
		Actor:      "admin",
		AuthMethod: "token",
		Category:   AuditCategoryOperation,
		Action:     "channel.save",
		Params:     "",
		IP:         "127.0.0.1",
		UserAgent:  "go-test",
		Method:     "POST",
		Route:      "/admin/channels",
		Status:     200,
		Success:    true,
	}
	if err := AppendAudit(e); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}

	var got AuditLog
	if err := DB.First(&got, "event_id = ?", eventID).Error; err != nil {
		t.Fatalf("读回审计行失败: %v", err)
	}
	if got.At != e.At || got.Actor != e.Actor || got.AuthMethod != e.AuthMethod {
		t.Errorf("At/Actor/AuthMethod 应原样读回，期望 %+v，得到 %+v", e, got)
	}
	if got.Category != AuditCategoryOperation || got.Action != "channel.save" {
		t.Errorf("Category/Action 应原样读回，得到 category=%q action=%q", got.Category, got.Action)
	}
	if got.Method != "POST" || got.Route != "/admin/channels" {
		t.Errorf("Method/Route 应原样读回，得到 method=%q route=%q", got.Method, got.Route)
	}
	if got.Status != 200 || !got.Success {
		t.Errorf("Status/Success 应原样读回，得到 status=%d success=%v", got.Status, got.Success)
	}
	if got.Params != "" {
		t.Errorf("Params 必须由调用方显式给（本层不自动生成摘要），得到 %q", got.Params)
	}

	n, err := CountAudit()
	if err != nil {
		t.Fatalf("CountAudit: %v", err)
	}
	if n != 1 {
		t.Errorf("应 1 行，得到 %d", n)
	}
}

// TestAuditEventIDIsIdempotent EventID 是幂等键：同一个 ID 写两次仍只有一行。
//
// # 为什么必须有这条（它是 EventID 这个字段存在的全部理由）
//
// 审计写入会被重放：客户端重试、代理重发、运维手工重跑。若重放产生第二行，
// 「改一次 base_url 后行数 +1」这条判据就会在重放时变成 +2，而更糟的是
// 审计页会把同一次操作显示成两次 —— 那会让「谁在什么时候改了什么」
// 变成不可回答的问题。
func TestAuditEventIDIsIdempotent(t *testing.T) {
	openAuditTestDB(t)

	const eventID = "audit-idempotent-0001"
	first := AuditLog{EventID: eventID, At: 1793000001, Actor: "admin", Action: "channel.save", Method: "POST", Route: "/admin/channels", Status: 200, Success: true}
	if err := AppendAudit(first); err != nil {
		t.Fatalf("首次 AppendAudit: %v", err)
	}
	// 重放：同一 EventID、内容略有差异（模拟重试时的时间/状态不同）
	replay := first
	replay.At = 1793000999
	replay.Status = 500
	replay.Success = false
	if err := AppendAudit(replay); err != nil {
		t.Fatalf("重放 AppendAudit 不该报错（幂等忽略，不是失败）: %v", err)
	}

	n, err := CountAudit()
	if err != nil {
		t.Fatalf("CountAudit: %v", err)
	}
	if n != 1 {
		t.Fatalf("同一 EventID 写入两次应仍为 1 行，得到 %d", n)
	}
	// 先写的那条必须保留（幂等 = 第一次为准，而不是「后到的覆盖」）：
	// 覆盖语义会把「第一次真的发生了什么」抹掉。
	var got AuditLog
	if err := DB.First(&got, "event_id = ?", eventID).Error; err != nil {
		t.Fatalf("读回审计行失败: %v", err)
	}
	if got.Status != 200 || !got.Success || got.At != 1793000001 {
		t.Errorf("重放不该改写已有行，期望 Status=200/Success=true/At=1793000001，得到 %+v", got)
	}
}

// TestAuditClosedDatabase 未 Open（或已 Close）时必须**如实报错**。
//
// # 为什么不是静默成功
//
// 审计是「事后追责」的唯一依据：写失败却回 nil，会让调用方以为留下了痕迹，
// 而实际上什么都没记 —— 这类静默丢失要到真正要查的时候才发现。
func TestAuditClosedDatabase(t *testing.T) {
	if err := Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if err := AppendAudit(AuditLog{EventID: "audit-closed-0001"}); !errors.Is(err, ErrNotOpen) {
		t.Errorf("未打开时应回 ErrNotOpen，得到 %v", err)
	}
	if _, err := CountAudit(); !errors.Is(err, ErrNotOpen) {
		t.Errorf("未打开时 CountAudit 应回 ErrNotOpen，得到 %v", err)
	}
}

// TestAuditGeneratesMissingEventIDAndAt 空 EventID / 零 At 由本层补齐。
//
// 让调用方「忘了填」不产生不可幂等的行：没有 ID 的行既不能去重，
// 也会让「同一操作被记两次」永远查不出来。
func TestAuditGeneratesMissingEventIDAndAt(t *testing.T) {
	openAuditTestDB(t)

	if err := AppendAudit(AuditLog{Actor: "admin", Action: "generic", Method: "POST", Route: "/admin/x", Status: 200, Success: true}); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	var got AuditLog
	if err := DB.First(&got).Error; err != nil {
		t.Fatalf("读回审计行失败: %v", err)
	}
	if len(got.EventID) != 32 {
		t.Errorf("缺省 EventID 应补成 32 字符 hex，得到 %q（长度 %d）", got.EventID, len(got.EventID))
	}
	if got.At <= 0 {
		t.Errorf("缺省 At 应补成当前时间，得到 %d", got.At)
	}
}
