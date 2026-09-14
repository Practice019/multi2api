package workbuddy

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 源码级守卫：聚合行不得再讲槽听不懂的语言
// ---------------------------------------------------------------------------
//
// # 为什么上面那条测试不够
//
// TestTaskSlotSummaryCountsAggregateRows 喂的是**手写的行**，所以它只证明
// "槽能数出这种行"，证明不了 handler 真的这么填 —— 有人把 statusOK 改回
// 字符串 "done"，上面那条**依然全绿**，而线上摘要又会回到"成功 0"。
//
// 所以这里直接对源码立约束：判据是"没有槽听不懂的 status 字面量"，
// 且"必须有聚合行讲槽语言的证据"。与 webui_endpoint_guard_test.go 同款做法
// （源码级、与调用点数量无关、失败信息直接指向要改的地方）。
func TestAggregateRowsUseSlotVocabularySourceGuard(t *testing.T) {
	const srcFile = "autotask_admin.go"
	raw, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", srcFile, err)
	}
	src := string(raw)
	if len(strings.TrimSpace(src)) == 0 {
		t.Fatal("源码读到了空内容 —— 守卫失效（fail-open）")
	}

	// 这两个字面量正是曾经的缺陷形态："成功"和"失败"被写成了槽听不懂的字。
	// （"skipped" 不在其列：禁用账号确实什么都没发生，不计成功也不计失败是对的。）
	for _, bad := range []string{`"status": "done"`, `"status": "partial"`} {
		if i := strings.Index(src, bad); i >= 0 {
			t.Errorf("%s 里仍出现 %s（第 %d 行）：\n"+
				"taskslot.sumStatus 只认 ok / fail / already，写别的会让\n"+
				"/admin/task 的完成摘要**恒报成功 0**（实测：开学季真跑完 3 次抽奖，\n"+
				"摘要显示成功 0）。聚合行请用 statusOK / statusFail。",
				srcFile, bad, strings.Count(src[:i], "\n")+1)
		}
	}

	// `"status": "error"` **不能**一刀切禁掉 —— runSchoolFor 内层的步骤结果
	// 正是用 error 表示"这一步失败了"，而 schoolRowStatus 就是靠读它折算聚合状态。
	// 所以判据要精确到"**带 uid 的**聚合行"：
	//
	//	聚合行：map[string]any{"uid": …, "status": "error", …}    ← 必须改用 statusFail
	//	步骤行：map[string]any{"step": "share", "status": "error"} ← 合法，勿动
	//
	// 用 uid 区分是可靠的：内层步骤行从不带 uid（它属于某个已知账号，
	// 只有外层聚合行才标 uid）。
	aggregateErr := regexp.MustCompile(`"uid":\s*[^}]*?"status":\s*"error"`)
	if loc := aggregateErr.FindStringIndex(src); loc != nil {
		t.Errorf("%s 第 %d 行有**带 uid 的聚合行**仍在用 status \"error\"：\n    %s\n"+
			"槽只认 ok / fail / already → 该行不会被计入失败数，真实故障会被摘要吞掉。\n"+
			"（带 step 的内层步骤行用 \"error\" 是合法的，schoolRowStatus 靠它折算。）",
			srcFile, strings.Count(src[:loc[0]], "\n")+1,
			strings.TrimSpace(src[loc[0]:loc[1]]))
	}

	// 正向证据：聚合行都必须讲槽的语言。
	for _, want := range []string{
		`"status": statusOK`,               // 成长成功行 / 夜猫子成功行
		`"status": statusFail`,             // 成长失败行 / 夜猫子失败行
		`"status": schoolRowStatus(steps)`, // 开学季行（按步骤折算）
	} {
		if !strings.Contains(src, want) {
			t.Errorf("%s 里找不到 %s —— 聚合行没有讲任务槽的语言，摘要会数不出来",
				srcFile, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 守卫：后台任务的完成摘要必须真的能数出成长 / 开学季任务的成绩
// ---------------------------------------------------------------------------
//
// # 这条守卫守的是什么 bug（实测发现）
//
// `/admin/task` 的 ok / fail / already 由 taskslot.sumStatus 按**行**统计 status，
// 而它只认 ok / fail / already 三个字（见 taskslot.Snapshot）。
//
// 而三个新聚合行原来各讲各的语言：
//
//	AutoAll    单账号/整池成功 → "done"，账号出错 → "error"
//	SchoolRun  **什么都不填**
//	BlackcatRun → "ok" / "partial"（只有它是对的）
//
// 后果不是"显示难看"，而是**摘要恒报成功 0**：
// 2026-09-14 实测 school-run 13 秒跑完全部步骤（分享 + 3 次领奖 + 3 次抽奖
// 命中 +78c），前端进度条却显示「完成：成功 0 · 已签到 0 · 失败 0」——
// 用户会认为整轮什么都没做，甚至以为失败了。
//
// # 为什么判据钉在"槽能看见它"而不是钉具体字段名
//
// 槽的词汇表是唯一判据：聚合行必须讲槽的语言。所以这里喂进各种聚合行，
// 断言 Snapshot 数得出来 —— 不关心它叫 done 还是 ok，只关心它**没被静默吞掉**。
func TestTaskSlotSummaryCountsAggregateRows(t *testing.T) {
	void := []map[string]any{}
	cases := []struct {
		name        string
		rows        []map[string]any
		ok, fail    int
		wantAlready int
	}{
		{
			name: "成长一键完成（整账号成功）",
			rows: []map[string]any{{
				"uid": "u", "status": statusOK,
				"results": []map[string]any{{"task_code": "chat_5", "ok": true}},
			}},
			ok: 1,
		},
		{
			name: "成长一键完成（该账号失败）",
			rows: []map[string]any{{"uid": "u", "status": statusFail, "message": "上游错误"}},
			fail: 1,
		},
		{
			name: "成长一键完成（整池：一成一败）",
			rows: []map[string]any{
				{"uid": "a", "status": statusOK, "results": void},
				{"uid": "b", "status": statusFail, "message": "上游错误"},
			},
			ok: 1, fail: 1,
		},
		{
			name: "开学季成功",
			rows: []map[string]any{{
				"uid": "u", "status": statusOK,
				"result": []map[string]any{{"step": "share", "status": "ok"}},
			}},
			ok: 1,
		},
		{
			name: "开学季某步骤出错",
			rows: []map[string]any{{
				"uid": "u", "status": statusFail,
				"result": []map[string]any{
					{"step": "share", "status": "ok"},
					{"step": "expert_use", "status": "error", "message": "boom"},
				},
			}},
			fail: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runSlotAndSnapshot(t, c.rows)
			if got["ok"] != c.ok {
				t.Errorf("ok = %v，期望 %d（聚合行没讲槽的语言 → 摘要会恒报成功 0）", got["ok"], c.ok)
			}
			if got["fail"] != c.fail {
				t.Errorf("fail = %v，期望 %d（真实故障会被摘要吞掉）", got["fail"], c.fail)
			}
			if c.wantAlready != 0 && got["already"] != c.wantAlready {
				t.Errorf("already = %v，期望 %d", got["already"], c.wantAlready)
			}
		})
	}
}

// runSlotAndSnapshot 跑一次任务槽并等它落定，返回 Snapshot。
//
// 必须轮询而不是只等 fn 返回：results 是在 fn 返回**之后**由 Start 的
// goroutine 写回的（见 taskslot.Start），只等 fn 会读到 running=true 的空快照，
// 测试就会变成一条永远绿灯的空转断言。
func runSlotAndSnapshot(t *testing.T, rows []map[string]any) map[string]any {
	t.Helper()
	ts := newTaskSlot()
	fnDone := make(chan struct{})
	if !ts.Start("test-kind", func() []map[string]any {
		defer close(fnDone)
		return rows
	}) {
		t.Fatal("任务槽拒绝启动（本测试不该与其它任务并发）")
	}
	select {
	case <-fnDone:
	case <-time.After(5 * time.Second):
		t.Fatal("任务函数未在 5s 内返回")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap := ts.Snapshot()
		if running, _ := snap["running"].(bool); !running {
			return snap
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("任务槽 5s 内没有落回 running=false")
	return nil
}

// TestSchoolRowStatus 钉住开学季聚合行的判据。
//
// 判据取"任一步骤 error 即 fail"：开学季的步骤是独立可跳过的（已 claimed 的
// 直接不跑，所以没有 step 结果也是正常成功），但**有 error 必须计数**，
// 否则真实故障会被摘要吃掉 —— 那正是这条判据存在的理由。
func TestSchoolRowStatus(t *testing.T) {
	cases := []struct {
		name  string
		steps []map[string]any
		want  string
	}{
		{"全部成功", []map[string]any{{"step": "share", "status": "ok"}}, statusOK},
		{"空步骤（全部已 claimed，无事可做）", []map[string]any{}, statusOK},
		{"不在期（skip 不是错误）", []map[string]any{{"step": "list", "status": "skip"}}, statusOK},
		{"任一步骤 error", []map[string]any{
			{"step": "share", "status": "ok"},
			{"step": "chat_3_times", "status": "error"},
		}, statusFail},
		{"只有 error", []map[string]any{{"step": "list", "status": "error"}}, statusFail},
		{"status 类型不符（非字符串）不 panic", []map[string]any{{"status": 42}}, statusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := schoolRowStatus(c.steps); got != c.want {
				t.Errorf("schoolRowStatus = %q，期望 %q", got, c.want)
			}
		})
	}
}
