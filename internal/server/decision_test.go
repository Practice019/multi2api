package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/logbuf"
)

// ⚠ 本文件钉住"判决留痕"这件事（2026-10-18 加）。
//
// # 它解决什么
//
// 用户看到 503 时，日志里此前只有 `status=503` 这一个事实 ⇒ 读者只能猜。
// 2026-10-18 排那个 503 时我**猜了四轮**（额度 / 容量 / 重试策略全错），
// 最后是靠"日志里有 uid 但 mode=sync"这种侧面证据才定位。
// ⇒ 现在出口层在判决点写一个 `reason:source`（形如 `pool_unavailable:global`），
//   直接照着 new-api 的 `PolicyDecision{Action, Reason, Source}`
//   （service/request_policy.go:19 · 前端类型 web/src/features/system-settings/request-policies/api.ts:7）。
//
// # 为什么只留 reason:source、不留 action
//
//	`Action`（retry / stop）**已经由 `Status` 体现**；而"为什么"是行为里看不出来的。
//	详见 internal/logbuf/logbuf.go 的 `Entry.Decision` 注释。

// TestLogRowCarriesDecision decision 必须落进 ring（那是 /admin/logs 与 jsonl 的来源）。
func TestLogRowCarriesDecision(t *testing.T) {
	withChatLog(t)
	const uid = "decision-test-uid-1"
	captureStdout(t, func() {
		logChatRowProvider(0, time.Second, "m", "sync", uid, "loomy",
			http.StatusServiceUnavailable, -1, nil, "pool_unavailable:global", 137, "10.0.0.9", "probe/1.0")
	})
	got := findEntryByUID(t, uid)
	if got.Decision != "pool_unavailable:global" {
		t.Errorf("Decision = %q，想要 pool_unavailable:global", got.Decision)
	}
	// ⚠ pre_ms（发上游之前的耗时）也必须落盘：它是判断"慢该怪谁"的唯一直接证据，
	//   用户 2026-10-18 的原话是"根本就不是号级速度的问题，而是后端代码调用有 bug"——
	//   而那件事只能靠这一列回答。
	if got.PreMs != 137 {
		t.Errorf("PreMs = %d，想要 137（发上游前的耗时没落盘 ⇒ 没法回答「慢在哪一段」）", got.PreMs)
	}
	// ⚠ 请求来源也要落盘：它用来指认"是谁在直连网关发非流式"（实测有 6256 条，
	// 且不经过 new-api）—— 没有这两列就只能靠模型/token 规律猜。
	if got.ClientIP != "10.0.0.9" || got.ClientUA != "probe/1.0" {
		t.Errorf("客户端来源没落盘: ip=%q ua=%q", got.ClientIP, got.ClientUA)
	}
	// ⚠ 序列化必须带上它（jsonl 落盘与 /admin/logs 都走这条）：
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"decision":"pool_unavailable:global"`) {
		t.Errorf("序列化后没有 decision 字段: %s", string(b))
	}
}

// TestLogRowDecisionOmittedWhenEmpty ⚠ 成功请求**不该**多出一个空字段
// （omitempty 的语义：缺字段与空串等价，但日志体积不该被它撑大 —— 每天几万行）。
func TestLogRowDecisionOmittedWhenEmpty(t *testing.T) {
	withChatLog(t)
	const uid = "decision-test-uid-2"
	captureStdout(t, func() {
		// ⚠ 老调用方（不传 decision）：兼容性也在这里被钉住
		logChatRowProvider(0, time.Second, "m", "sync", uid, "loomy", http.StatusOK, 1, nil, "", 0, "", "")
	})
	got := findEntryByUID(t, uid)
	if got.Decision != "" {
		t.Errorf("不传 decision 时应为空串，得到 %q", got.Decision)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), `"decision"`) {
		t.Errorf("空 decision 不该出现在序列化结果里: %s", string(b))
	}
}

// findEntryByUID 从 ring 的最近快照里按 uid 找一条。
//
// ⚠ 必须按 uid 过滤而不是"取最后一条"：`chatLogRing` 是**包级共享**的，
// 同包其它测试也会往里 Push ⇒ 直接取末尾会串味（这类"测试互相污染"在本仓踩过）。
func findEntryByUID(t *testing.T, uid string) logbuf.Entry {
	t.Helper()
	ents, _ := chatLogRing.Snapshot(0)
	for i := len(ents) - 1; i >= 0; i-- {
		if ents[i].UID == uid {
			return ents[i]
		}
	}
	t.Fatalf("ring 里找不到 uid=%s 的那条（Snapshot 拿到 %d 条）", uid, len(ents))
	return logbuf.Entry{}
}
