package trae

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── 用户报障「签到不了」的两个根因，各一组回归测试 ──
//
//	根因 1  X-Device-Id 填了登录用的 32hex，而签到接口只认**账号 uid**
//	根因 2  claim 的业务失败是 HTTP 200 + code!=0，旧代码只看 HTTP 状态
//
// 两组的判据都做成"喂真实上游回执"，而不是自己编一个理想响应 ——
// 回执是从真实上游抄下来的原文。

// newCheckinStub 起一个假上游，记录收到的 X-Device-Id 与 claim 次数。
func newCheckinStub(t *testing.T, statusBody, claimBody string) (*httptest.Server, *[]string, *int) {
	t.Helper()
	var devs []string
	claims := 0
	mux := http.NewServeMux()
	mux.HandleFunc(EpCheckinStatus, func(w http.ResponseWriter, r *http.Request) {
		devs = append(devs, r.Header.Get("X-Device-Id"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, statusBody)
	})
	mux.HandleFunc(EpCheckinClaim, func(w http.ResponseWriter, r *http.Request) {
		claims++
		devs = append(devs, r.Header.Get("X-Device-Id"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claimBody)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &devs, &claims
}

// TestCheckinSendsUIDAsDeviceID 签到请求的 X-Device-Id 必须是**账号 uid**。
//
// # 这是用户报障「签到不了」的主因（实测）
//
// 真实上游对同一份凭证的三个对照：
//
//	X-Device-Id=uid      → {"code":0}    到手
//	X-Device-Id=32hex    → {"code":9074} 拒绝
//	不带该头              → {"code":9004} 拒绝
//
// 设备维度回执（status 的 did_checked_in）也印证：uid → true，
// 32hex/随机 → false。即上游只承认 uid 作这台"设备"。
//
// ⚠ 本包最初直接填 credential.DeviceID（登录 URL 用的 32hex 指纹），
// 于是每次都拿 9074。端点/头名/body 全对，唯独这个**值**错了。
func TestCheckinSendsUIDAsDeviceID(t *testing.T) {
	srv, devs, _ := newCheckinStub(t,
		`{"code":0,"checked_in":false,"credits":100,"extra_credits":100,"enable":true}`,
		`{"code":0,"credits":100,"message":"success"}`)

	c := NewWithBase(srv.URL)
	a := &Auth{
		AccessToken: "t",
		UID:         "3929003642848586",                 // 账号主键（18 位数字）
		DeviceID:    "ca207992892093703231e01aabd848d8", // 登录指纹（32hex）
	}
	if _, err := c.CheckinClaim(context.Background(), a); err != nil {
		t.Fatalf("claim 失败: %v", err)
	}
	if len(*devs) == 0 {
		t.Fatal("没收到任何请求")
	}
	for i, d := range *devs {
		if d != a.UID {
			t.Errorf("第 %d 个请求的 X-Device-Id = %q，want %q（账号 uid）\n"+
				"填 DeviceID 会让上游回 9074（实测），签到永远失败",
				i+1, d, a.UID)
		}
	}
}

// TestCheckinDeviceIDFallback uid 缺失时退回 DeviceID（不发空串）。
func TestCheckinDeviceIDFallback(t *testing.T) {
	if got := checkinDeviceID(&Auth{UID: "u1", DeviceID: "dev"}); got != "u1" {
		t.Errorf("有 uid 时 = %q，want u1（uid 优先）", got)
	}
	if got := checkinDeviceID(&Auth{DeviceID: "dev"}); got != "dev" {
		t.Errorf("无 uid 时 = %q，want dev（退回 DeviceID）", got)
	}
	if got := checkinDeviceID(nil); got != "" {
		t.Errorf("nil 时 = %q，want 空", got)
	}
}

// TestCheckinClaimRejectsBusinessError claim 必须把 code!=0 当**失败**。
//
// # 这是用户报障「签到不了」的第二层（实测）
//
// 上游的业务拒绝是 **HTTP 200 + code != 0**。旧代码 `doJSON` 只在
// `StatusCode >= 400` 时报错，于是：
//
//	{"code":9074,…} → err=nil → 上层记 StatusOK
//	→ 界面"签到成功"、历史 `ok`，而**积分一分没到账**
//
// 实测 9074 会持续数分钟（8/8 次，间隔 30 秒），不是瞬时抖动，
// 所以"重试一下就好"不成立。
func TestCheckinClaimRejectsBusinessError(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		msg   string
		match func(error) bool
	}{
		{"9074 设备限流", 9074, "当前参与用户太多，请稍后再试", IsCheckinBusy},
		{"9095 设备今日已签", 9095, "当前设备今日已经签到，请明日再来", IsCheckinDeviceDone},
		{"9004 缺设备", 9004, "The submitted order parameters are incorrect.", nil},
		{"1005 权益不足", 1005, "insufficient", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"code": tc.code, "message": tc.msg})
			stub, _, _ := newCheckinStub(t,
				`{"code":0,"checked_in":false,"enable":true}`,
				string(body))

			c := NewWithBase(stub.URL)
			a := &Auth{AccessToken: "t", UID: "u1"}
			_, err := c.CheckinClaim(context.Background(), a)
			if err == nil {
				t.Fatalf("code=%d 被判成功 —— 这正是「签到不了」：界面显示成功、"+
					"历史记 ok，而实际没到账", tc.code)
			}
			if !strings.Contains(err.Error(), "9074") && !strings.Contains(err.Error(), itoa(tc.code)) {
				t.Errorf("错误里应带 code %d（便于分类与排障）：%v", tc.code, err)
			}
			if tc.match != nil && !tc.match(err) {
				t.Errorf("分类判据没认出 code %d：%v", tc.code, err)
			}
		})
	}
}

// TestCheckinClaimRequiresConfirmation code=0 但复查未到账 → 仍算失败。
//
// 参照实现 caigee-cmd/cli2api 的结论：
//
//	"The claim endpoint answers code 0 even when the device identity is
//	 refused (9074) or the daily grant was already taken, so success is
//	 decided by re-probing: a real claim flips checked_in to true."
//
// 即存在「code=0 但没真到账」。所以判据必须是复查后的 Confirmed。
func TestCheckinClaimRequiresConfirmation(t *testing.T) {
	stub, _, _ := newCheckinStub(t,
		// 复查始终说"没签"——模拟"code=0 但没到账"
		`{"code":0,"checked_in":false,"credits":100,"enable":true}`,
		`{"code":0,"credits":100,"message":"success"}`)

	c := NewWithBase(stub.URL)
	a := &Auth{AccessToken: "t", UID: "u1"}
	res, err := c.CheckinClaim(context.Background(), a)
	if err != nil {
		t.Fatalf("code=0 不该报错（该由 Confirmed 表达）：%v", err)
	}
	if res.Confirmed {
		t.Error("复查 checked_in=false 却被判 Confirmed=true —— " +
			"存在「code=0 但没到账」的情形，只信 code 会谎报成功")
	}
}

// TestCheckinClaimConfirmsWhenStatusFlips 复查到 checked_in=true 才算成功。
func TestCheckinClaimConfirmsWhenStatusFlips(t *testing.T) {
	stub, _, _ := newCheckinStub(t,
		`{"code":0,"checked_in":true,"credits":100,"extra_credits":50,"enable":true}`,
		`{"code":0,"message":"success"}`)

	c := NewWithBase(stub.URL)
	a := &Auth{AccessToken: "t", UID: "u1"}
	res, err := c.CheckinClaim(context.Background(), a)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if !res.Confirmed {
		t.Error("复查 checked_in=true 应当 Confirmed")
	}
	// claim 回执没带 credits 时应取复查的 credits + extra_credits。
	if res.Credits != 150 {
		t.Errorf("credits = %d，want 150（100 基础 + 50 额外）—— "+
			"额外奖励此前完全没统计", res.Credits)
	}
}

// TestCheckinStatusRejectsBusinessError status 也必须校验 code。
//
// 旧版不读 code：上游返回错误信封时全部字段为零值 →
// checked_in=false, enable=false → 被当成"活动未开启"静默跳过。
func TestCheckinStatusRejectsBusinessError(t *testing.T) {
	stub, _, _ := newCheckinStub(t,
		`{"code":1001,"message":"authentication failed"}`,
		`{"code":0}`)

	c := NewWithBase(stub.URL)
	a := &Auth{AccessToken: "t", UID: "u1"}
	_, _, _, _, err := c.CheckinStatus(context.Background(), a)
	if err == nil {
		t.Error("code=1001 的 status 应报错，而不是被当成「活动未开启」静默跳过")
	}
}

// TestCheckinDeviceIDUsedByBothEndpoints status 与 claim 用同一个设备值。
//
// 两者必须一致 —— 上游按「账号 × 设备」记账，若 status 用 uid、
// claim 用别的，会签到一个"幽灵设备"上。
func TestCheckinDeviceIDUsedByBothEndpoints(t *testing.T) {
	stub, devs, _ := newCheckinStub(t,
		`{"code":0,"checked_in":false,"credits":1,"enable":true}`,
		`{"code":0,"credits":1}`)

	c := NewWithBase(stub.URL)
	a := &Auth{AccessToken: "t", UID: "u9", DeviceID: "hexdev"}
	ctx := context.Background()
	_, _, _, _, _ = c.CheckinStatus(ctx, a)
	_, _ = c.CheckinClaim(ctx, a)

	if len(*devs) < 2 {
		t.Fatalf("应有两个请求（status + claim），实际 %d", len(*devs))
	}
	if (*devs)[0] != (*devs)[1] {
		t.Errorf("status 用 %q 而 claim 用 %q —— 两者必须一致",
			(*devs)[0], (*devs)[1])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
