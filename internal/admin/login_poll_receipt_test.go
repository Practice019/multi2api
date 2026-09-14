// login_poll_receipt_test.go —— `/admin/login/poll` 回执里**过期信息**的两条守卫。
//
// # 这两条对应用户实测报的"前端添加账号不对"
//
// 现象：loomy 的弹窗在「授权成功」下面写着 **`有效期: 已过期`** ——
// 而**同一份凭证**在账号表里写着「永久」。
//
// 根因是回执把 `Credential.ExpiresAt` 的**零值**（loomy 没有到期时刻）
// 也序列化了出去：`"0001-01-01T00:00:00Z"`。前端判据是
//
//	if (!p.expires_at) return '—';            // 非空字符串 → 不命中
//	const t = Date.parse(p.expires_at);       // ≈ -6.2e13（truthy）→ 不命中
//	const days = (t - Date.now()) / 86400000; // 远小于 0
//	if (days <= 0) return '已过期';            // ← 命中这句
//
// 与 T5 那次的 `token_expire_sec` 是**同一个形态**：用字段的存在性表达"未知"，
// 却把一个零值当成有效值发了出去。所以两条断言分别是：
//
//	零值 → 字段**不出现**（不是"出现但是零"）
//	上游说永久 → 出现 `token_never_expires`（弹窗才能说「永久」）
package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/gateway"
)

// lifetimeFlowProvider 在 flowProvider 之上再实现 `CredentialLifetimeExt`。
//
// ⚠ 为什么必须另开一个类型（而不是给 flowProvider 加一个字段）：
// `flowProvider` 被一批既有用例用来表示"**没有**过期扩展点"的上游，
// 给它加上方法会让那些用例的语义**静默变宽**（它们就再也测不出
// "未实现 → 不下发 token_never_expires"这一条了）。
// 与 account_columns_test.go 里"每组都配一个实现/未实现"的做法一致。
type lifetimeFlowProvider struct {
	flowProvider
	never bool
}

func (p *lifetimeFlowProvider) NeverExpires(cred gateway.Credential) bool { return p.never }

var _ gateway.CredentialLifetimeExt = (*lifetimeFlowProvider)(nil)

// pollOnce 打一次 /admin/login/poll 并返回 (状态码, 解码后的回执, 原始正文)。
func pollOnce(t *testing.T, h *Handler) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := localReq("POST", "/admin/login/poll")
	req.Body = jsonBody(`{"state":"ST-abc","provider":"flowup"}`)
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	var out map[string]any
	// 非 200 时也要能把错误信息打出来，所以解码失败不算致命。
	_ = json.Unmarshal([]byte(body), &out)
	return rec.Code, out, body
}

// TestLoginPollOmitsZeroExpiresAt 零值到期时刻**不下发**。
//
// # 反向验证
//
// 把 pollViaFlow 里那句 `if cred.ExpiresAt.Unix() > 0` 去掉、改回无条件赋值，
// 这条立刻变红（body 里会出现 `0001-01-01T00:00:00Z`）。
func TestLoginPollOmitsZeroExpiresAt(t *testing.T) {
	flow := &fakeFlow{
		configured: true,
		authDir:    t.TempDir(),
		pollFn: func(string) (gateway.Credential, error) {
			return gateway.Credential{
				Provider: "flowup",
				UID:      "u-zero",
				Nickname: "零值号",
				// 刻意留零值 —— 这正是 loomy（以及任何"凭证不过期"的上游）的形态。
				Secret: &authFileStub{name: "workbuddy-u-zero.json", raw: []byte(`{"a":1}`)},
			}, nil
		},
	}
	reg := gateway.NewRegistry()
	if err := reg.Register(&flowProvider{
		stubProvider: stubProvider{id: "flowup", caps: gateway.CapChat},
		flow:         flow,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg, DefaultProvider: "flowup"})
	// ⚠ AuthDir 与 flow.authDir 都要给：前者给 LoadCredentials 扫，
	// 后者是"上游自报的落盘目录"。只给一个是既有用例踩过的坑。
	h.cfg.AuthDir = flow.authDir

	code, out, body := pollOnce(t, h)
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", code, body)
	}
	if _, present := out["expires_at"]; present {
		t.Fatalf("★ 零值到期时刻被下发了（expires_at=%v）—— "+
			"前端会把它渲染成「已过期」，而这份凭证其实是「永久」：\n%s",
			out["expires_at"], body)
	}
	// 连带：正文里不许出现 Go time.Time 的零值字符串。
	if strings.Contains(body, "0001-01-01") {
		t.Errorf("回执正文里出现了 Go 零值时间：%s", body)
	}
}

// TestLoginPollReportsNeverExpires 上游说"永久" → 回执带 `token_never_expires`。
//
// # 为什么这条重要
//
// 后端**下发**了它，弹窗才可能显示「永久（无 TTL）」。没有它，
// 弹窗与账号表对同一份凭证给出两个答案（`—` vs「永久」）——
// 正是用户看到的那种矛盾。
func TestLoginPollReportsNeverExpires(t *testing.T) {
	flow := &fakeFlow{
		configured: true,
		authDir:    t.TempDir(),
		pollFn: func(string) (gateway.Credential, error) {
			return gateway.Credential{
				Provider: "flowup",
				UID:      "u-perm",
				Nickname: "永久号",
				Secret:   &authFileStub{name: "workbuddy-u-perm.json", raw: []byte(`{"a":1}`)},
			}, nil
		},
	}
	reg := gateway.NewRegistry()
	if err := reg.Register(&lifetimeFlowProvider{
		flowProvider: flowProvider{
			stubProvider: stubProvider{id: "flowup", caps: gateway.CapChat},
			flow:         flow,
		},
		never: true,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg, DefaultProvider: "flowup"})
	h.cfg.AuthDir = flow.authDir

	code, out, body := pollOnce(t, h)
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", code, body)
	}
	if out["token_never_expires"] != true {
		t.Fatalf("★ 上游说「永久」，回执却没带 token_never_expires —— "+
			"弹窗只能显示 `—`，与账号表里的「永久」矛盾：\n%s", body)
	}
	if _, present := out["expires_at"]; present {
		t.Errorf("「永久」与「有到期时刻」互斥，不该同时下发：%s", body)
	}
}

// TestLoginPollOmitsNeverExpiresWhenNotImplemented 未实现该扩展点 → 字段不出现。
//
// 这是**向后兼容**那一条：workbuddy / codearts 的弹窗必须逐字节不变
// （它们有时刻，走 `expires_at`；没有"永久"这回事）。
func TestLoginPollOmitsNeverExpiresWhenNotImplemented(t *testing.T) {
	at := time.Now().Add(60 * 24 * time.Hour)
	flow := &fakeFlow{
		configured: true,
		authDir:    t.TempDir(),
		pollFn: func(string) (gateway.Credential, error) {
			return gateway.Credential{
				Provider:  "flowup",
				UID:       "u-tok",
				Nickname:  "有token号",
				ExpiresAt: at,
				Secret:    &authFileStub{name: "workbuddy-u-tok.json", raw: []byte(`{"a":1}`)},
			}, nil
		},
	}
	reg := gateway.NewRegistry()
	// 注意：这个桩**不实现** CredentialLifetimeExt。
	if err := reg.Register(&flowProvider{
		stubProvider: stubProvider{id: "flowup", caps: gateway.CapChat},
		flow:         flow,
	}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Registry: reg, DefaultProvider: "flowup"})
	h.cfg.AuthDir = flow.authDir

	code, out, body := pollOnce(t, h)
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", code, body)
	}
	if _, present := out["token_never_expires"]; present {
		t.Errorf("未实现 CredentialLifetimeExt 时不该下发 token_never_expires：%s", body)
	}
	if _, present := out["expires_at"]; !present {
		t.Errorf("有到期时刻时必须照旧下发 expires_at（这是可见回归）：%s", body)
	}
}
