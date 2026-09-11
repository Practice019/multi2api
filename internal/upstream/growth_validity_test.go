package upstream

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 真实响应样本（实测自 /v2/activity/growth/tasks，已脱敏）。
// 关键：18 个任务里只有 4 个带 valid_end，其余为 null —— 两种形态都要能吃下。
const realTasksWithExpiry = `{"code":0,"msg":"OK","data":{"tasks":[
  {"task_code":"Expert_Philanthropy","title":"公益专家","description":"x","task_desc":"y",
   "task_type":"single","valid_start":null,"valid_end":"2026-09-30T23:59:00+08:00",
   "reward_credit":200,"accept_status":"accepted","progress":{"current":0,"target":1}},
  {"task_code":"Hp_Appearance","title":"外观","description":"x","task_desc":"y",
   "task_type":"single","valid_start":"2026-09-07T00:00:00+08:00","valid_end":"2026-11-02T23:59:00+08:00",
   "reward_credit":100,"accept_status":"accepted","progress":{"current":1,"target":1}},
  {"task_code":"create_canvas","title":"画布","description":"x","task_desc":"y",
   "task_type":"single","valid_start":null,"valid_end":null,
   "reward_credit":300,"accept_status":"accepted","progress":{"current":0,"target":1}}
]}}`

// valid_start / valid_end 必须被解析出来 —— 此前完全丢弃，
// 于是"哪些任务快到期了"在界面上无从体现（实测 18 个里有 4 个带期限）。
func TestGrowthTasksParsesValidityWindow(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, realTasksWithExpiry), nil
	})

	tasks, err := c.GrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("GrowthTasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("应有 3 个任务，得到 %d", len(tasks))
	}

	byCode := map[string]GrowthTask{}
	for _, tk := range tasks {
		byCode[tk.TaskCode] = tk
	}

	// 1. 只有 valid_end（无 valid_start）
	p := byCode["Expert_Philanthropy"]
	if p.ValidEnd == nil {
		t.Fatal("Expert_Philanthropy 应解析出 valid_end")
	}
	if got := p.ValidEnd.Format(time.RFC3339); got != "2026-09-30T23:59:00+08:00" {
		t.Errorf("valid_end=%s want 2026-09-30T23:59:00+08:00", got)
	}
	if p.ValidStart != nil {
		t.Errorf("valid_start 为 null 时应保持 nil，得到 %v", p.ValidStart)
	}

	// 2. 两者都有
	h := byCode["Hp_Appearance"]
	if h.ValidStart == nil || h.ValidEnd == nil {
		t.Fatal("Hp_Appearance 应同时解析出 start 与 end")
	}
	if got := h.ValidStart.Format(time.RFC3339); got != "2026-09-07T00:00:00+08:00" {
		t.Errorf("valid_start=%s", got)
	}

	// 3. 都为 null → 都保持 nil（不能用零值伪装成"1970 年到期"）
	cc := byCode["create_canvas"]
	if cc.ValidStart != nil || cc.ValidEnd != nil {
		t.Errorf("无期限的任务两个字段都应为 nil，得到 start=%v end=%v", cc.ValidStart, cc.ValidEnd)
	}
}

// 时间格式异常时不能让整个任务列表解析失败 —— 期限是附加信息，不是关键字段。
func TestGrowthTasksBadValidityDoesNotBreakList(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"tasks":[
		  {"task_code":"a","title":"A","valid_end":"not-a-time","accept_status":"accepted"},
		  {"task_code":"b","title":"B","valid_end":"","accept_status":"accepted"},
		  {"task_code":"c","title":"C","valid_end":"2026-10-10T23:59:00+08:00","accept_status":"accepted"}
		]}}`), nil
	})

	tasks, err := c.GrowthTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("坏时间戳不该让整个列表失败: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("3 个任务都应保留，得到 %d", len(tasks))
	}
	byCode := map[string]GrowthTask{}
	for _, tk := range tasks {
		byCode[tk.TaskCode] = tk
	}
	if byCode["a"].ValidEnd != nil {
		t.Errorf("无法解析的时间应为 nil，得到 %v", byCode["a"].ValidEnd)
	}
	if byCode["b"].ValidEnd != nil {
		t.Errorf("空字符串应为 nil，得到 %v", byCode["b"].ValidEnd)
	}
	if byCode["c"].ValidEnd == nil {
		t.Error("同一列表里正常的时间仍要解析出来")
	}
}

// 反序列化 null 不应 panic，也不应把零值 time.Time 当成有效时间。
func TestGrowthTaskValidityNullSafe(t *testing.T) {
	var tk GrowthTask
	if err := json.Unmarshal([]byte(`{"task_code":"x","valid_start":null,"valid_end":null}`), &tk); err != nil {
		t.Fatal(err)
	}
	if tk.ValidStart != nil || tk.ValidEnd != nil {
		t.Error("null 应解成 nil 指针，而不是零值时间")
	}
	// 零值 time.Time 的 IsZero 为真；用指针就是为了能区分"没有"与"1970"
	var zero time.Time
	if !zero.IsZero() {
		t.Fatal("前置：零值应为 IsZero")
	}
}
