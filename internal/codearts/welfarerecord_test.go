// welfarerecord_test.go —— 福利领取必须留下**本地**记录。
//
// # 为什么这条记录是必需的（用户要的「福利是否领取」靠它）
//
// 上游的 `/v1/ops/delivery` 只回 `claimable` 布尔，**没有独立的"已领"标志**；
// 而 `claimable=false` 既可能是"今日已领"，也可能是"资格不符"——
// 界面无法区分。所以唯一能如实回答"今天领过没有"的，是**我们自己**的领取动作。
//
// 这组测试钉住三件事：
//
//	领到东西   → 记 ok      （界面说「已领取」）
//	一项没领到 → 记 skip    （界面说「无可领」—— 不说"已领"，因为分不清）
//	请求报错   → 记 fail    （界面说「失败」，带原因）
//
// ⚠ 第三条最容易被漏：不记的话界面永远停在「—」，
// 用户分不清"没领过"与"领过但失败了"。
package codearts

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/checkinlog"
	"workbuddy2api/internal/gateway"
)

// welfareStub 一个假的福利引擎：只用两条真实端点。
//
//	GET  /v1/ops/delivery  → 活动列表（claimable 由用例决定）
//	POST /v1/ops/claim     → 领取（code=0 视为成功）
type welfareStub struct {
	srv       *httptest.Server
	claimable bool
	claimFail bool // true = claim 端点回 500
	listFail  bool // true = delivery 端点回 500
}

func newWelfareStub(t *testing.T, claimable bool) *welfareStub {
	t.Helper()
	st := &welfareStub{claimable: claimable}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case welfareDeliveryPath:
			if st.listFail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":1,"message":"boom"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[` +
				`{"campaignId":101,"title":"每日签到领1000积分","claimable":` +
				boolJSON(st.claimable) + `}]}}`))
		case welfareClaimPath:
			if st.claimFail {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":1,"message":"boom"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"message":""}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(st.srv.Close)
	return st
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// newWelfareProvider 建一个"能领福利"的 Provider：假引擎 + 一个账号 + 一份历史。
func newWelfareProvider(t *testing.T, st *welfareStub) (*Provider, *checkinlog.Log) {
	t.Helper()
	cl := New()
	cl.EngineBase = st.srv.URL
	hist := checkinlog.New(t.TempDir()+"/log.json", 30)

	p := NewWithConfig(Config{Client: cl})
	p.SetAdminEnv(AdminEnv{
		Resolve: func(uid string) (*Auth, error) {
			return &Auth{UID: "ca-1", Nickname: "测试号", AccessKey: "AK", SecretKey: "SK"}, nil
		},
		Log: hist,
	})
	return p, hist
}

// claimVia 直接调管理端点（与浏览器点「领取福利」走的是同一条路径）。
func claimVia(t *testing.T, p *Provider) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/welfare/claim",
		strings.NewReader(`{"uid":"ca-1"}`))
	rec := httptest.NewRecorder()
	p.handleWelfareClaim(rec, req)
	return rec
}

// lastWelfareRecord 取该 uid 今天最后一条 welfare 记录。
func lastWelfareRecord(t *testing.T, hist *checkinlog.Log) (checkinlog.Record, bool) {
	t.Helper()
	return hist.LastByUID("ca-1", checkinlog.KindWelfare)
}

// TestWelfareClaimRecordsOK 领到了东西 → 记 ok。
func TestWelfareClaimRecordsOK(t *testing.T) {
	st := newWelfareStub(t, true)
	p, hist := newWelfareProvider(t, st)

	if rec := claimVia(t, p); rec.Code != http.StatusOK {
		t.Fatalf("领取端点 HTTP %d，want 200（body=%s）", rec.Code, rec.Body.String())
	}

	got, ok := lastWelfareRecord(t, hist)
	if !ok {
		t.Fatal("★ 领取成功了却没有留下 welfare 记录 —— " +
			"账号池的「福利」列会永远显示「—」，用户分不清领没领过")
	}
	if got.Kind != checkinlog.KindWelfare {
		t.Errorf("Kind = %q，want %q（不能并进 checkin —— codearts 没有签到端点）",
			got.Kind, checkinlog.KindWelfare)
	}
	if got.Status != checkinlog.StatusOK {
		t.Errorf("Status = %q，want %q", got.Status, checkinlog.StatusOK)
	}
	if got.UID != "ca-1" {
		t.Errorf("UID = %q，want ca-1", got.UID)
	}
	if got.Trigger != "manual" {
		t.Errorf("Trigger = %q，want manual（是用户点的，不是定时任务）", got.Trigger)
	}
}

// TestWelfareClaimRecordsSkipWhenNothingClaimed 一项都没领到 → 记 skip。
//
// ⚠ 关键：**不能**记成 ok。上游在 `claimable=false` 时既可能是"今日已领"
// 也可能是"资格不符"，我们无法区分 —— 所以只能说"这次没领到"。
func TestWelfareClaimRecordsSkipWhenNothingClaimed(t *testing.T) {
	st := newWelfareStub(t, false) // 唯一的活动不可领
	p, hist := newWelfareProvider(t, st)

	if rec := claimVia(t, p); rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200（不可领不是错误）", rec.Code)
	}
	got, ok := lastWelfareRecord(t, hist)
	if !ok {
		t.Fatal("没领到也该留一条记录（否则界面停在「—」）")
	}
	if got.Status == checkinlog.StatusOK {
		t.Error("★ 一项都没领到却记成了 ok —— 界面会说「已领取」，那是假的")
	}
	if got.Status != checkinlog.StatusSkip {
		t.Errorf("Status = %q，want %q", got.Status, checkinlog.StatusSkip)
	}
}

// TestWelfareClaimRecordsFailOnError 请求报错 → 记 fail（带原因）。
func TestWelfareClaimRecordsFailOnError(t *testing.T) {
	st := newWelfareStub(t, true)
	st.listFail = true // 拉列表就 500
	p, hist := newWelfareProvider(t, st)

	if rec := claimVia(t, p); rec.Code == http.StatusOK {
		t.Fatal("上游 500 时不该返回 200")
	}
	got, ok := lastWelfareRecord(t, hist)
	if !ok {
		t.Fatal("★ 领取失败没有留痕 —— 界面会永远显示「—」，" +
			"用户分不清\"没领过\"与\"领过但失败了\"")
	}
	if got.Status != checkinlog.StatusFail {
		t.Errorf("Status = %q，want %q", got.Status, checkinlog.StatusFail)
	}
	if got.Detail == "" {
		t.Error("失败记录应当带原因（Detail 为空的话界面帮不上排障）")
	}
}

// TestWelfareClaimWithoutLogDoesNotPanic 没注入历史时**不能 panic**。
//
// `Log` 是可选依赖（测试、以及"不落盘历史"的部署都是 nil）。
// 不判空会让整条领取端点挂掉 —— 而那是用户点按钮的那条路径。
func TestWelfareClaimWithoutLogDoesNotPanic(t *testing.T) {
	st := newWelfareStub(t, true)
	cl := New()
	cl.EngineBase = st.srv.URL
	p := NewWithConfig(Config{Client: cl})
	p.SetAdminEnv(AdminEnv{
		Resolve: func(uid string) (*Auth, error) {
			return &Auth{UID: "ca-1", AccessKey: "AK", SecretKey: "SK"}, nil
		},
		// Log 故意不设（nil）
	})

	if rec := claimVia(t, p); rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d，want 200（body=%s）", rec.Code, rec.Body.String())
	}
}

// 编译期断言：Provider 仍然满足它该满足的接口（本次改动不该动接口）。
var _ gateway.AdminExt = (*Provider)(nil)
