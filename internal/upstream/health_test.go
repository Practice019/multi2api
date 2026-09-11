package upstream

import (
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// 真实响应样本（实测自 /v2/plugin/accounts，已脱敏 uid/phone）。
const realAccountsBody = `{"code":0,"msg":"OK","requestId":"x","data":{"accounts":[
  {"uid":"u-1","nickname":"测试号","uin":"330118282272","type":"personal","lastLogin":true,
   "pluginEnabled":true,"deployStatus":{"statusCode":0,"statusMsg":"","detailMsg":""},
   "phoneNumber":"13800000000"}
]}}`

// 正常路径：解析出存活、账号数、插件开关与联系方式。
func TestCheckAccountAliveHappyPath(t *testing.T) {
	var gotPath, gotMethod, gotUA, gotAuth string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotUA, gotAuth = r.Header.Get("User-Agent"), r.Header.Get("Authorization")
		return jsonResp(200, realAccountsBody), nil
	})

	got, err := c.CheckAccountAlive(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("CheckAccountAlive: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method=%s want GET", gotMethod)
	}
	if gotPath != "/v2/plugin/accounts" {
		t.Errorf("path=%s want /v2/plugin/accounts", gotPath)
	}
	// UA 是实测必需头：缺了上游按未知客户端拒绝。
	if gotUA != clientUA {
		t.Errorf("User-Agent=%q want %q", gotUA, clientUA)
	}
	if gotAuth != "Bearer at" {
		t.Errorf("Authorization=%q", gotAuth)
	}
	if !got.Alive {
		t.Error("应判为存活")
	}
	if got.Accounts != 1 || !got.PluginEnabled {
		t.Errorf("accounts=%d pluginEnabled=%v", got.Accounts, got.PluginEnabled)
	}
	if got.Nickname != "测试号" || got.Phone != "13800000000" {
		t.Errorf("nickname=%q phone=%q", got.Nickname, got.Phone)
	}
}

// 401（失效 token）必须被判定为"不可用"，而不是"解析失败"。
//
// 实测：上游对失效 token 返回 **HTML** 错误页而非 JSON，
// 所以这里同时守住"HTML 响应不会让我们 panic 或误判为存活"。
func TestCheckAccountAliveUnauthorizedIsError(t *testing.T) {
	const html401 = `<html><head><title>401 Authorization Required</title></head><body>...</body></html>`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, html401), nil
	})

	got, err := c.CheckAccountAlive(&auth.Auth{AccessToken: "bad"})
	if err == nil {
		t.Fatalf("401 应返回 error，得到 %+v", got)
	}
	if got != nil {
		t.Errorf("出错时不应返回结果: %+v", got)
	}
	// 错误里要能看出是鉴权问题（调用方据此把账号标为不健康）
	if !strings.Contains(strings.ToLower(err.Error()), "401") {
		t.Errorf("错误信息应含 401: %q", err.Error())
	}
}

// code != 0（200 但业务失败）也要报错 —— 只判状态码会把这种情况当健康。
func TestCheckAccountAliveBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":1001,"msg":"token expired"}`), nil
	})
	if _, err := c.CheckAccountAlive(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("业务 code != 0 应报错")
	}
}

// 200 但 accounts 为空 → **不可用**（不是错误）。
//
// 语义区分：没有账号条目说明这个 token 不代表任何可用账号，
// 这是"探测成功且结论是不健康"，与"探测失败"是两回事。
func TestCheckAccountAliveEmptyAccounts(t *testing.T) {
	for name, body := range map[string]string{
		"空数组":    `{"code":0,"msg":"OK","data":{"accounts":[]}}`,
		"缺 data": `{"code":0,"msg":"OK"}`,
	} {
		c := testClient(func(r *http.Request) (*http.Response, error) {
			return jsonResp(200, body), nil
		})
		got, err := c.CheckAccountAlive(&auth.Auth{AccessToken: "at"})
		if err != nil {
			t.Errorf("%s: 不该报错: %v", name, err)
			continue
		}
		if got == nil || got.Alive {
			t.Errorf("%s: 应判为不可用，得到 %+v", name, got)
		}
	}
}

// deployStatus 的说明文案要能被取出来（非空时它常是"为什么不能用"的答案）。
func TestCheckAccountAliveDeployMessage(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accounts":[{
			"uid":"u","pluginEnabled":false,
			"deployStatus":{"statusCode":3,"statusMsg":"generic","detailMsg":"具体原因"}
		}]}}`), nil
	})
	got, err := c.CheckAccountAlive(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if got.DeployMsg != "具体原因" {
		t.Errorf("detailMsg 应优先: %q", got.DeployMsg)
	}
}

// ---------------------------------------------------------------------------
// DosageNotify
// ---------------------------------------------------------------------------

// 正常（无告警）→ 返回 nil，调用方用一个 `if w != nil` 就能判断。
func TestDosageNotifySilentWhenNormal(t *testing.T) {
	var gotPath, gotMethod string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.Path, r.Method
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"dosageNotifyCode":0,"dosageNotifyZh":"","dosageNotifyEn":""}}`), nil
	})

	got, err := c.DosageNotify(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("正常情况不该报错: %v", err)
	}
	if got != nil {
		t.Errorf("无告警应返回 nil，得到 %+v", got)
	}
	if gotMethod != http.MethodPost || gotPath != "/v2/billing/meter/get-dosage-notify" {
		t.Errorf("请求形态错误: %s %s", gotMethod, gotPath)
	}
}

// 有告警 → 返回中文原因。
func TestDosageNotifyWithWarning(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"dosageNotifyCode":2,"dosageNotifyZh":"额度已用尽，请充值","dosageNotifyEn":"quota exhausted"}}`), nil
	})
	got, err := c.DosageNotify(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("有告警时不该返回 nil")
	}
	if got.Code != 2 {
		t.Errorf("code=%d want 2", got.Code)
	}
	if got.Message() != "额度已用尽，请充值" {
		t.Errorf("Message()=%q", got.Message())
	}
}

// 只有英文时回落英文；都没有时按 code 给兜底文案（不能返回空串）。
func TestDosageWarningMessageFallback(t *testing.T) {
	cases := []struct {
		w    *DosageWarning
		want string
	}{
		{&DosageWarning{Code: 1, Zh: "中文", En: "en"}, "中文"},
		{&DosageWarning{Code: 1, En: "en"}, "en"},
		{&DosageWarning{Code: 7}, "上游额度告警（code=7）"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := c.w.Message(); got != c.want {
			t.Errorf("Message()=%q want %q", got, c.want)
		}
	}
}

// 401 应报错（与存活探测一致）。
func TestDosageNotifyUnauthorized(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(401, "<html>401</html>"), nil
	})
	if _, err := c.DosageNotify(&auth.Auth{AccessToken: "bad"}); err == nil {
		t.Fatal("401 应报错")
	}
}

// code!=0 且无文案（例如上游只在 code 上表达）→ 仍然给出告警而不是丢弃。
func TestDosageNotifyCodeOnly(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"dosageNotifyCode":5}}`), nil
	})
	got, err := c.DosageNotify(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("code!=0 时不该返回 nil（那会丢掉告警信号）")
	}
	if got.Message() == "" {
		t.Error("必须给出非空文案")
	}
}
