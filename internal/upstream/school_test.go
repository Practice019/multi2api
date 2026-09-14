// school_test.go 开学季（小程序指纹）事件形状 + 夜猫子窗口判据。
//
// 与 desktop_test.go 同一目的：B 对这块零测试，而判据是逐字段匹配。
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestMPEventBaseFingerprint 小程序指纹必须是完整的一套。
//
// 缺 ideType/extName/platform 之类会被上游按"非 mp 来源"处理，
// expert_use 这类判据就永远不亮 —— 而这是**静默**的。
func TestMPEventBaseFingerprint(t *testing.T) {
	base := mpEventBase(&auth.Auth{UID: "u-mp", Nickname: "昵称"})

	required := map[string]string{
		"ideType":      "WorkBuddy_MP",
		"ideVersion":   "2.4.0",
		"extName":      "workbuddy-mp",
		"extVersion":   "2.4.0",
		"ideName":      "wx_app_cloud",
		"platform":     "mini_program",
		"product":      "SaaS",
		"os":           "windows",
		"osVersion":    "11",
		"arch":         "x64",
		"timezone":     "Asia/Shanghai",
		"userId":       "u-mp",
		"userNickname": "昵称",
	}
	for k, want := range required {
		if got, _ := base[k].(string); got != want {
			t.Errorf("mp 指纹 %s=%q，期望 %q", k, got, want)
		}
	}
	if _, ok := base["machineId"]; !ok {
		t.Error("mp 指纹缺 machineId")
	}
	if _, ok := base["timestamp"]; !ok {
		t.Error("mp 指纹缺 timestamp")
	}
}

// TestReportMPEventWire 小程序上报的 wire 形态：codebuddy.cn + 四个 mp 头。
func TestReportMPEventWire(t *testing.T) {
	var gotPath string
	gotHdr := map[string]string{}
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		for _, h := range []string{"X-Client-Product", "X-Client-Version", "X-Client-Platform", "X-Platform"} {
			gotHdr[h] = r.Header.Get(h)
		}
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BillingBaseCN = srv.URL
	a := &auth.Auth{UID: "u", AccessToken: "at"}
	if err := c.ReportMPEvent(a, SchoolChatTimesEvents("conv-1")); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if gotPath != "/v2/report" {
		t.Errorf("path=%q", gotPath)
	}
	wantHdr := map[string]string{
		"X-Client-Product":  "workbuddy-mp",
		"X-Client-Version":  "2.4.0",
		"X-Client-Platform": "mp-weixin",
		"X-Platform":        "wechatmp",
	}
	for k, want := range wantHdr {
		if gotHdr[k] != want {
			t.Errorf("头 %s=%q，期望 %q", k, gotHdr[k], want)
		}
	}
	// 指纹必须已注入每个事件。
	arr := decodeEventsAny(t, gotBody)
	if len(arr) != 1 {
		t.Fatalf("事件数=%d", len(arr))
	}
	if arr[0]["ideType"] != "WorkBuddy_MP" {
		t.Errorf("事件未注入 mp 指纹: %v", arr[0])
	}
	if arr[0]["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v", arr[0]["eventCode"])
	}
}

// TestSchoolChatTimesEventsShape chat_3_times 的判据事件形状。
func TestSchoolChatTimesEventsShape(t *testing.T) {
	ev := SchoolChatTimesEvents("conv-A")
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	// JOIN 字段必须齐：conversationId / parentConversationId / messageId / traceId。
	for _, k := range []string{"conversationId", "parentConversationId", "messageId", "traceId", "rootRequestId"} {
		if _, ok := ev[k]; !ok {
			t.Errorf("缺 %q", k)
		}
	}
	if ev["conversationId"] != "conv-A" {
		t.Errorf("conversationId=%v", ev["conversationId"])
	}
	if ev["agentName"] != "mp" {
		t.Errorf("agentName=%v，期望 mp（mp 域事件的标志）", ev["agentName"])
	}
	// 两次调用必须产生**不同**的 requestId（否则同一天多条上报会被去重成一条）。
	ev2 := SchoolChatTimesEvents("conv-B")
	if ev["traceId"] == ev2["traceId"] {
		t.Error("两次构造的 traceId 相同 —— 上游会按 requestId 去重，导致只计一条")
	}
}

// TestSchoolExpertUseEventsShape expert_use 的四事件链。
func TestSchoolExpertUseEventsShape(t *testing.T) {
	events := SchoolExpertUseEvents("ex_x", "论文写作导师", "conv-E")
	if len(events) != 4 {
		t.Fatalf("事件数=%d，期望 4", len(events))
	}
	want := []string{"expert_summon_click", "expert_summoned", "expert_actual_use", "chat_request_send"}
	for i, w := range want {
		if events[i]["eventCode"] != w {
			t.Errorf("第 %d 个=%v，期望 %v", i, events[i]["eventCode"], w)
		}
	}
	// 分类必须是开学季专属（16-BackToSchool）—— 换成别的分类不计分。
	//
	// ⚠ 只有第 0 与第 2 条带 type：第 1 条（expert_summoned）在**原样本里
	// 本来就没有这个字段**。以为"每条都该有"是很容易犯的错 ——
	// 真按那个假想补上去，就等于改动了 1:1 移植的载荷。
	if events[0]["type"] != "16-BackToSchool" {
		t.Errorf("expert_summon_click.type=%v，期望 16-BackToSchool", events[0]["type"])
	}
	if events[2]["type"] != "16-BackToSchool" {
		t.Errorf("expert_actual_use.type=%v，期望 16-BackToSchool", events[2]["type"])
	}
	if _, ok := events[1]["type"]; ok {
		t.Errorf("expert_summoned 不该带 type（原样本无该字段，多一个字段即偏离载荷）: %v", events[1])
	}
	if _, ok := events[0]["position"]; !ok {
		t.Error("expert_summon_click 缺 position（判据样本里有）")
	}
	// 第四条（chat）必须带 expertId/expertName 才能 JOIN 上专家使用。
	if events[3]["expertId"] != "ex_x" {
		t.Errorf("chat 事件缺 expertId: %v", events[3]["expertId"])
	}
}

// ── 夜猫子窗口 ──────────────────────────────────────────────────────────

// TestInNightWindow 23:00–08:00 的边界。
//
// 用本地时区构造（与实现同口径）。边界值 23 与 8 是**互斥**的两端：
// 23 点在内、8 点不在内 —— 写反一位就会让"早上 8 点补足"静默不计分。
func TestInNightWindow(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{0, true}, {1, true}, {5, true}, {7, true},
		{8, false}, // 8 点不在窗口内（窗口是 23:00–08:00 左闭右开）
		{12, false}, {22, false},
		{23, true}, // 23 点在窗口内
	}
	for _, c := range cases {
		at := time.Date(2026, 9, 14, c.hour, 30, 0, 0, time.Local)
		if got := InNightWindow(at); got != c.want {
			t.Errorf("%02d:30 InNightWindow=%v，期望 %v", c.hour, got, c.want)
		}
	}
}

// TestBlackcatNeedNoTask 任务不存在时返回 0（不是错误）。
//
// 返回错误会让守卫轮把"这个号没有夜猫子任务"记成失败，
// 在界面上表现为持续告警 —— 而它其实只是没这个任务。
func TestBlackcatNeedNoTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.ChatBaseCN = srv.URL
	need, err := c.BlackcatNeed(&auth.Auth{UID: "u", AccessToken: "at"})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if need != 0 {
		t.Errorf("need=%d，期望 0", need)
	}
}

// TestBlackcatNeedGap 有进度缺口时返回差额；已达标/已领取返回 0。
func TestBlackcatNeedGap(t *testing.T) {
	cases := []struct {
		name  string
		tasks string
		wantN int64
	}{
		{"缺口 2", `[{"task_code":"black_cat","accept_status":"accepted","progress":{"current":1,"target":3}}]`, 2},
		{"已达标", `[{"task_code":"black_cat","accept_status":"completed","progress":{"current":3,"target":3}}]`, 0},
		{"已领取", `[{"task_code":"black_cat","accept_status":"claimed","progress":{"current":3,"target":3}}]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":` + tc.tasks + `}}`))
			}))
			defer srv.Close()
			c := New()
			c.ChatBaseCN = srv.URL
			need, err := c.BlackcatNeed(&auth.Auth{UID: "u", AccessToken: "at"})
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if need != tc.wantN {
				t.Errorf("need=%d，期望 %d", need, tc.wantN)
			}
		})
	}
}

// TestSchoolJSONUnwrapsEnvelope 学院 API 的信封解包与业务错误。
func TestSchoolJSONUnwrapsEnvelope(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[{"task_code":"share_invite","status":"pending"}],"in_period":true}}`))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BillingBaseCN = srv.URL
	tasks, inPeriod, err := c.SchoolTasks(&auth.Auth{UID: "u", AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if gotPath != "/portal/activity/school/tasks" {
		t.Errorf("path=%q", gotPath)
	}
	if !inPeriod {
		t.Error("in_period 应为 true")
	}
	if len(tasks) != 1 || tasks[0].TaskCode != "share_invite" {
		t.Errorf("解析结果=%+v", tasks)
	}
}

// TestSchoolJSONBusinessError 业务 code≠0 必须变成 error（不能当成成功）。
func TestSchoolJSONBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10001,"msg":"活动已结束"}`))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BillingBaseCN = srv.URL
	if _, _, err := c.SchoolTasks(&auth.Auth{UID: "u", AccessToken: "at"}); err == nil {
		t.Fatal("code≠0 必须返回 error —— 否则活动结束后会被当成「0 个待办」静默通过")
	} else if !strings.Contains(err.Error(), "活动已结束") {
		t.Errorf("错误信息应带上游原文，得到 %v", err)
	}
}

// TestSchoolDrawUsesUUID 抽奖必须带 draw_uuid（缺它上游拒绝消耗次数）。
func TestSchoolDrawUsesUUID(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"prize_code":"credit_100","credit_amount":100}}`))
	}))
	t.Cleanup(srv.Close)

	c := New()
	c.BillingBaseCN = srv.URL
	prize, err := c.SchoolDraw(&auth.Auth{UID: "u", AccessToken: "at"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatal(err)
	}
	if s, _ := body["draw_uuid"].(string); s == "" {
		t.Error("draw_uuid 为空 —— 上游会拒绝这次抽奖")
	}
	if !strings.Contains(prize, "credit_100") || !strings.Contains(prize, "+100c") {
		t.Errorf("奖品描述=%q，期望含 prize_code 与积分数", prize)
	}
}
