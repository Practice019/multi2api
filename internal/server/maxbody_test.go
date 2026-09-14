// maxbody_test.go 请求体上限：超限必须回 413，而不是静默截断。
//
// # 为什么单独为这一条写测试（借鉴 workbuddy2api-panel）
//
// 改造前出口层是 `io.ReadAll(io.LimitReader(r.Body, 8<<20))`。它有三个后果，
// 而三个都是**观察不到**的：
//
//	① 超过 8 MiB 的请求体被无声切掉尾巴；
//	② 剩下的半截 JSON 解析失败 → 网关把半截 body 原样转发；
//	③ 上游回 code 11101 "Unmarshal chat params failed"。
//
// 客户端看到的是③ —— 一条**指向自己 JSON 的错误**，而真因（请求太大）
// 在整条链路上从未被说出口。同时出站错误分类把 11101 当业务错误处理，
// 会让一次"太大"演变成多次换号重试，白烧账号健康度。
//
// 因此本测试的核心断言不是"返回了非 200"，而是**错误码必须是 413**：
// LimitReader 的实现同样会返回非 200（它更可能返回上游的 400/500），
// 只断言"非 200"的测试**无法区分这两种实现** —— 那就是一条没有判别力的测试。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// decodeOpenAIErrCode 取出 writeOpenAIError 写下的 error.code。
func decodeOpenAIErrCode(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v; body=%.200s", err, body)
	}
	return resp.Error.Code
}

// TestChatBodyOverLimitReturns413 超过上限 → 413 + request_too_large。
//
// 反向判别力：若实现退回 LimitReader（静默截断），本用例**必然失败** ——
// 截断后的 body 是一个语义完整的 JSON（我们在末尾填充空格，见下），
// 网关会把它转发出去，最终拿到上游的 200/4xx 而不是 413。
func TestChatBodyOverLimitReturns413(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		// 上游被调用本身就说明网关**没有**在入口拦住超限请求。
		t.Errorf("超限请求不该被转发到上游（authz=%q）", authz)
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		MaxBodyMB: 1, // 1 MiB，便于用 ~2 MiB 的 body 触发
	})

	// 刻意构造一个**截断后仍然合法**的 JSON：
	// {"model":"glm-5.2","messages":[...],"pad":"<大量空白>"}
	// 前 1 MiB 截出来依然是完整 JSON（pad 是最后一个字段），
	// 于是"静默截断"实现在这里会一路走到上游 —— 这正是我们要区分的。
	pad := strings.Repeat(" ", 2<<20)
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"pad":"` + pad + `"}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 413 {
		t.Fatalf("超限应回 413，得到 %d；body=%.200s\n"+
			"  ← 返回非 413 说明请求体被静默截断后继续转发了", rec.Code, rec.Body.String())
	}
	if code := decodeOpenAIErrCode(t, rec.Body.String()); code != "request_too_large" {
		t.Errorf("error.code=%q，期望 request_too_large", code)
	}
	// 文案必须给出可执行出路（改哪个配置键），否则用户只能去猜。
	if !strings.Contains(rec.Body.String(), "max_body_mb") {
		t.Errorf("413 文案应指出 server.max_body_mb，实际：%s", rec.Body.String())
	}
}

// TestChatBodyUnderLimitPasses 未超限的请求不受影响（默认值与改造前同为 8 MiB）。
func TestChatBodyUnderLimitPasses(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:      testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:  up,
		MaxBodyMB: 1,
	})
	// 约 256 KiB，远小于 1 MiB。
	pad := strings.Repeat(" ", 256<<10)
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"pad":"` + pad + `"}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("未超限应正常处理，得到 %d body=%.300s", rec.Code, rec.Body.String())
	}
}

// TestMaxBodyMBDefaultsTo8 未配置（<=0）时回落 8 MiB —— 与改造前的硬编码一致。
//
// 这条保证"改动的意图是改报错方式，不是改接受能力"：
// 既有部署不配该项时，能收的请求大小逐字节不变。
func TestMaxBodyMBDefaultsTo8(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: newFakeUpstream(t, nil)})
	if h.cfg.MaxBodyMB != 8 {
		t.Errorf("MaxBodyMB 缺省应为 8，得到 %d", h.cfg.MaxBodyMB)
	}
	if defaultMaxBodyMB != 8 {
		t.Errorf("defaultMaxBodyMB=%d，必须与改造前硬编码的 8<<20 一致", defaultMaxBodyMB)
	}
}
