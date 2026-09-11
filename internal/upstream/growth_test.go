package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestGrowthClaimRequestShape 断言领奖请求的路径/方法/空 body。
//
// 路径形状是最容易写错的地方：官方前端用的是不带 /v2 的
// /activity/growth/tasks/{code}/claim，而列任务用的是带 /v2 的
// /v2/activity/growth/tasks。两种领奖路径实测都能通，本包统一走 /v2，
// 这个测试把「路径必须以 /claim 结尾且带上 task_code」钉住。
func TestGrowthClaimRequestShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotBody, _ = io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer at" {
			return nil, errors.New("missing Authorization")
		}
		if r.Header.Get("X-User-Id") != "u1" {
			return nil, errors.New("missing X-User-Id")
		}
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5,"share_uuid":"su-1"}}`), nil
	})

	res, err := c.GrowthClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("GrowthClaim: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method=%s want POST", gotMethod)
	}
	if gotPath != "/v2/activity/growth/tasks/chat_5/claim" {
		t.Errorf("path=%s want /v2/activity/growth/tasks/chat_5/claim", gotPath)
	}
	// 请求体是空对象，不能把 task_code 塞进 body（那是 accept 的形状）。
	var body map[string]any
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("body 不是 JSON 对象: %q", gotBody)
	}
	if len(body) != 0 {
		t.Errorf("body 应为空对象，得到 %v", body)
	}
	if res.AlreadyClaimed || res.Credit != 100 || res.Energy != 5 || res.ShareUUID != "su-1" {
		t.Errorf("解析结果错误: %+v", res)
	}
}

// TestGrowthClaimAlreadyClaimed 已领过的任务是幂等成功，不能被当成失败，
// 否则自动领奖会每天报一堆假错误。
func TestGrowthClaimAlreadyClaimed(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"already_claimed":true,"credit":0,"energy":0}}`), nil
	})
	res, err := c.GrowthClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("已领取不应报错: %v", err)
	}
	if !res.AlreadyClaimed || res.Credit != 0 {
		t.Errorf("应该识别出 already_claimed: %+v", res)
	}
}

// TestGrowthClaimBadgeParsed 带徽章的返回要能解出徽章名。
func TestGrowthClaimBadgeParsed(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5,`+
			`"badge":{"id":9,"name":"尝鲜达人","url":"https://x/badge-04.png"}}}`), nil
	})
	res, err := c.GrowthClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, "skill_1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Badge == nil || res.Badge.Name != "尝鲜达人" || res.Badge.ID != 9 {
		t.Errorf("徽章解析错误: %+v", res.Badge)
	}
}

// TestGrowthClaimMissingData 上游只回 code/msg 不 datas 时按「无事发生」处理，
// 不能让解析失败把一次可能的成功变成错误。
func TestGrowthClaimMissingData(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"msg":"OK"}`), nil
	})
	res, err := c.GrowthClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, "x")
	if err != nil {
		t.Fatalf("缺 data 不应报错: %v", err)
	}
	if res.AlreadyClaimed || res.Credit != 0 {
		t.Errorf("缺 data 时应为零值: %+v", res)
	}
}

// TestGrowthClaimEmptyCode 空 task_code 必须本地就拒绝，别打一个 /tasks//claim 出去。
func TestGrowthClaimEmptyCode(t *testing.T) {
	called := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		called = true
		return jsonResp(200, `{"code":0}`), nil
	})
	if _, err := c.GrowthClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, ""); err == nil {
		t.Fatal("空 task_code 应报错")
	}
	if called {
		t.Error("空 task_code 不应发出请求")
	}
}

// TestGrowthClaimBusinessError 任务不存在时上游回 400，应归一为 *Error。
func TestGrowthClaimBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(400, `{"code":400,"msg":"invalid request","data":null}`), nil
	})
	_, err := c.GrowthClaim(&auth.Auth{AccessToken: "at", UID: "u1"}, "no_such_task")
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Status != 400 {
		t.Errorf("status=%d", ue.Status)
	}
}

// TestStatusHelpers 状态判定：Acceptable 只认 not_accepted，Claimable 只认 completed。
// in_progress 两边都不算——它是官方枚举里的第五个状态。
func TestStatusHelpers(t *testing.T) {
	cases := []struct {
		status         string
		locked         bool
		wantAcceptable bool
		wantClaimable  bool
	}{
		{GrowthStatusNotAccepted, false, true, false},
		{GrowthStatusNotAccepted, true, false, false},
		{GrowthStatusAccepted, false, false, false},
		{GrowthStatusInProgress, false, false, false},
		{GrowthStatusCompleted, false, false, true},
		{GrowthStatusClaimed, false, false, false},
		{GrowthStatusCompleted, true, false, true}, // 已解锁与否不影响领奖
	}
	for _, tc := range cases {
		task := &GrowthTask{AcceptStatus: tc.status, Locked: tc.locked}
		if got := task.Acceptable(); got != tc.wantAcceptable {
			t.Errorf("Acceptable(%s, locked=%v)=%v want %v", tc.status, tc.locked, got, tc.wantAcceptable)
		}
		if got := task.Claimable(); got != tc.wantClaimable {
			t.Errorf("Claimable(%s, locked=%v)=%v want %v", tc.status, tc.locked, got, tc.wantClaimable)
		}
	}
}
