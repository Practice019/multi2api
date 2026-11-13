package phanthywork

// banned_account_test.go 封禁账号的分类护栏（2026-11-13 真机取证）。
//
// # 为什么单独一个文件
//
// 这个错误码是**改造协议之后才拿到的**：
//
//	改造前（/v1/messages + 签名）：403 request_not_authorized（含糊，可自愈档）
//	改造后（/v1/chat/completions） ：403 disabled_account  ← 真实原因：账号被封
//
// 也就是说：**旧的错误信息把"封号"伪装成了"客户端需要更新"**。
// 这条链路正是本文件要钉死的东西 —— 谁把 bannedMarkers 删掉、或把它挪到
// 状态码分支之后，测试立刻红。

import (
	"testing"

	"workbuddy2api/internal/gateway"
)

// TestBannedAccountIsSessionDead 封禁 → ErrKindSessionDead（终态，不重试）。
func TestBannedAccountIsSessionDead(t *testing.T) {
	// 真机原文（逐字，勿改）
	const live = `{"error":{"code":"disabled_account","message":"当前账户已被封禁，请加微信群联系客服处理"}}`

	if k := Classify(403, live); k != gateway.ErrKindSessionDead {
		t.Fatalf("封禁账号被归成 %v，want %v\n"+
			"后果：归成可自愈档（Auth/Client）会让核心把这个死号反复放回池子轮，"+
			"而服务端已经明确说「账户已被封禁」——重试一万次也不会好。", k, gateway.ErrKindSessionDead)
	}
}

// TestBannedBeatsStatusCodeBranch 封禁判定必须**先于**状态码分支。
//
// 这条是最容易被无意破坏的：bannedMarkers 与 authExpiredMarkers 都是 403，
// 一旦顺序反了，封号会被当成"刷新一下就好"，于是死号被反复刷新。
func TestBannedBeatsStatusCodeBranch(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"403", 403, `{"error":{"code":"disabled_account"}}`},
		{"401", 401, `{"error":{"code":"disabled_account"}}`},
		{"200(异常但正文说了)", 200, `{"error":{"code":"disabled_account"}}`},
	}
	for _, c := range cases {
		if k := Classify(c.status, c.body); k != gateway.ErrKindSessionDead {
			t.Errorf("[%s] 封禁未优先命中，得到 %v\n"+
				"正文里已经写了 disabled_account，状态码分支不该抢先", c.name, k)
		}
	}
}

// TestBannedMarkersVariants 各种写法的封禁都要能认出来（含中英文）。
func TestBannedMarkersVariants(t *testing.T) {
	bodies := []string{
		`{"error":{"code":"disabled_account"}}`,
		`{"error":{"code":"account_disabled"}}`,
		`{"error":{"code":"account_banned"}}`,
		`{"error":{"message":"当前账户已被封禁，请加微信群联系客服处理"}}`,
		`{"error":{"message":"账户被封禁"}}`,
		`{"error":{"message":"账号已被封禁"}}`,
	}
	for _, b := range bodies {
		if k := Classify(403, b); k != gateway.ErrKindSessionDead {
			t.Errorf("body=%s → %v，want %v", b, k, gateway.ErrKindSessionDead)
		}
	}
}

// TestRequestNotAuthorizedStaysRecoverable request_not_authorized **不能**被当成封禁。
//
// 这是反向断言，同样重要：文件头的原因链里明确写着
// 「request_not_authorized 要么是客户端拼装问题、要么是推理令牌过期，可自愈」。
// 如果为了让封禁"更好判"而把它也归成终态，会误杀健康账号
// （这是本仓明确禁止过的形态：workbuddy 的裸数字判据曾把健康号永久禁用）。
func TestRequestNotAuthorizedStaysRecoverable(t *testing.T) {
	const live = `{"error":{"code":"request_not_authorized","message":"当前请求不可用，请更新或重新打开客户端后重试。"}}`
	k := Classify(403, live)
	if k == gateway.ErrKindSessionDead || k == gateway.ErrKindAuth {
		t.Fatalf("request_not_authorized 被归成 %v（终态/需刷新档）\n"+
			"它必须保持「换号但不罚号」—— errors.go 文件头：「把它判成 SessionDead ="+
			" 永久禁用这个账号——一个账号会因为一个 3 分钟的令牌过期被人工重登。」", k)
	}
}

// TestQuotaExhaustedIsNotBan 额度用尽**不是**封禁（对照，防止判据串味）。
func TestQuotaExhaustedIsNotBan(t *testing.T) {
	// 真机原文：`current_plan.remaining_credits=0` 时同样是 403，但码不同
	bodies := []string{
		`{"error":{"code":"insufficient_credits"}}`,
		`{"error":{"message":"额度不足"}}`,
	}
	for _, b := range bodies {
		k := Classify(403, b)
		if k == gateway.ErrKindSessionDead {
			t.Errorf("额度不足被归成封禁（%s）—— 额度会重置，账号没废", b)
		}
		if k != gateway.ErrKindHardCredit {
			t.Errorf("额度不足应归 HardCredit（吃长冷却等重置），得到 %v（%s）", k, b)
		}
	}
}
