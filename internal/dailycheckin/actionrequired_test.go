package dailycheckin

import (
	"context"
	"errors"
	"testing"

	"workbuddy2api/internal/checkinlog"
)

// TestActionRequiredSurvivesError ★ actionRequired 必须**跟着错误一起**出去。
//
// # 这条守的是 qoder 的一个真实分支
//
// qoder 的领取有一条 inactive 是"账号尚未在官方客户端登录过"——
// 它**既不是"上游没开放"（等就好），也不是纯粹的失败**（用户去官方
// 客户端登录一次就能领）。
//
// 若驱动把 actionRequired 丢掉，前端只能显示「签到失败」——
// 用户会反复点按钮而永远不成功（他没有被告知要做什么）。
//
// 变异可检：把 Claim 失败分支里的 ActionRequired 去掉 → 本用例红。
func TestActionRequiredSurvivesError(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	s.actionRequired["u1"] = true
	s.claimErr["u1"] = errors.New("请先在官方客户端登录一次")
	d := New(s, testDesc(), Options{Log: lg})

	res := d.CheckinOne(context.Background(), Account{UID: "u1"}, "manual")
	if res.Status != checkinlog.StatusFail {
		t.Fatalf("status = %q，want fail", res.Status)
	}
	if !res.ActionRequired {
		t.Error("actionRequired 被丢掉了 —— 前端只能显示「签到失败」，" +
			"用户不知道要先去官方客户端登录一次，会反复点按钮而永远不成功")
	}

	// 端点上也要透出去（前端读的是 result.action_required）。
	_, out := doReq(t, d.HandlerOne, "/admin/probe/checkin", `{"uid":"u1"}`)
	resObj, _ := out["result"].(map[string]any)
	if resObj == nil {
		t.Fatalf("回执缺 result：%v", out)
	}
	if resObj["action_required"] != true {
		t.Errorf("result.action_required = %v，want true —— "+
			"前端按这个字段决定要不要醒目提示用户", resObj["action_required"])
	}
}

// TestActionRequiredFalseByDefault 没有该概念的上游恒 false（不误报）。
func TestActionRequiredFalseByDefault(t *testing.T) {
	lg := newTestLog(t)
	s := newStub()
	s.accounts = []Account{{UID: "u1"}}
	s.claimErr["u1"] = errors.New("boom")
	d := New(s, testDesc(), Options{Log: lg})

	res := d.CheckinOne(context.Background(), Account{UID: "u1"}, "manual")
	if res.ActionRequired {
		t.Error("上游没报 actionRequired 时不该置 true —— " +
			"误报会让前端提示用户去做一件没必要的事")
	}
}
